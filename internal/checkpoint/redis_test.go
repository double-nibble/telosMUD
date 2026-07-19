package checkpoint

import (
	"context"
	"reflect"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/double-nibble/telosmud/internal/world"
)

// redis_test.go guards the checkpoint tier's hand-mapped `value` struct against a FIELD DROP.
//
// The Redis leg is the one place in the durability ladder where a CharSnapshot is copied field-by-field
// into a private struct and back (Checkpoint / LoadCheckpoint). Every field must survive the round trip.
// A dropped field here is invisible in the Postgres path — the caller (loadCharacterSnapshot) prefers the
// FRESHER of the two tiers, so on a crash-rehydrate the truncated checkpoint silently wins and the field
// is gone. This project has shipped that exact bug twice, in the store's def-table round trips.
//
// #320 raised the stakes: ZoneRef is now the LOGIN ROUTING KEY. A checkpoint that dropped it would route
// every crash-rehydrated player back to the shard's home zone — reintroducing the very data loss #320 fixes,
// on the one path a Postgres-only test can never see.

func newTestRedis(t *testing.T) *Redis {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedis(rdb, "telos")
}

// fullSnapshot is a CharSnapshot with every persisted field set to a DISTINCT non-zero value, so a dropped
// field shows up as a zero rather than coincidentally matching its neighbour.
func fullSnapshot() world.CharSnapshot {
	return world.CharSnapshot{
		PID:          world.PersistID("11111111-2222-3333-4444-555555555555"),
		Name:         "Wanderer",
		ZoneRef:      "darkwood",
		RoomRef:      "darkwood:room:grove",
		StateVersion: 42,
		// The ownership epoch (#432) must be non-zero here or the exhaustive check below cannot tell a
		// dropped field from an unset one. This tier is the one that was completely unfenced before #432:
		// a dropped OwnerEpoch reads back as 0, which the login comparator ranks lowest and the guard
		// script compares against a value it never stored — an unfenced checkpoint next to a fenced row
		// is exactly where a bypass lives.
		OwnerEpoch: 9,
		State:      world.StateJSON{AppliedSeq: 7, Attributes: map[string]float64{"strength": 14}},
	}
}

// TestCheckpointRoundTripPreservesEveryField is the field-drop guard. It compares the loaded snapshot to
// the written one field by field, so ADDING a field to CharSnapshot without threading it through `value`
// fails here rather than in production.
func TestCheckpointRoundTripPreservesEveryField(t *testing.T) {
	r := newTestRedis(t)
	ctx := context.Background()
	want := fullSnapshot()

	if err := r.Checkpoint(ctx, want); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	got, found, err := r.LoadCheckpoint(ctx, want.Name)
	if err != nil || !found {
		t.Fatalf("LoadCheckpoint: found=%v err=%v", found, err)
	}

	// ZoneRef called out explicitly: since #320 it is the login routing key, and losing it silently sends
	// every crash-rehydrated player back to the home zone.
	if got.ZoneRef != want.ZoneRef {
		t.Errorf("ZoneRef dropped by the checkpoint round trip: got %q, want %q — every crash-rehydrated player would route to the home zone (#320)", got.ZoneRef, want.ZoneRef)
	}
	if got.RoomRef != want.RoomRef {
		t.Errorf("RoomRef = %q, want %q", got.RoomRef, want.RoomRef)
	}
	if got.PID != want.PID {
		t.Errorf("PID = %q, want %q", got.PID, want.PID)
	}
	if got.Name != want.Name {
		t.Errorf("Name = %q, want %q", got.Name, want.Name)
	}
	if got.StateVersion != want.StateVersion {
		t.Errorf("StateVersion = %d, want %d", got.StateVersion, want.StateVersion)
	}
	if !reflect.DeepEqual(got.State, want.State) {
		t.Errorf("State = %v, want %v", got.State, want.State)
	}
}

// TestCheckpointRoundTripIsExhaustive is the DRIFT GUARD. reflect over CharSnapshot's exported fields and
// require each one to be either round-tripped or explicitly listed as intentionally not persisted. Adding a
// field to CharSnapshot and forgetting `value` is the field-drop bug; this fails the moment it happens,
// without anyone having to remember to extend the test above.
func TestCheckpointRoundTripIsExhaustive(t *testing.T) {
	// PendingChargen is deliberately NOT checkpointed: it is an account-owned, apply-once column that the
	// world reads from Postgres on first spawn and clears. It never rides the Redis mirror.
	notPersisted := map[string]string{
		"PendingChargen": "account-owned, apply-once; read from Postgres on first spawn, never mirrored",
	}

	r := newTestRedis(t)
	ctx := context.Background()
	want := fullSnapshot()
	if err := r.Checkpoint(ctx, want); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	got, found, err := r.LoadCheckpoint(ctx, want.Name)
	if err != nil || !found {
		t.Fatalf("LoadCheckpoint: found=%v err=%v", found, err)
	}

	tv := reflect.TypeOf(want)
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	for i := range tv.NumField() {
		name := tv.Field(i).Name
		if reason, ok := notPersisted[name]; ok {
			if !gv.Field(i).IsZero() {
				t.Errorf("%s survived the checkpoint but is documented as not persisted (%s)", name, reason)
			}
			continue
		}
		if wv.Field(i).IsZero() {
			t.Fatalf("test bug: fullSnapshot() leaves %s at its zero value, so a drop of it is undetectable", name)
		}
		if gv.Field(i).IsZero() {
			t.Errorf("FIELD DROP: CharSnapshot.%s is not threaded through checkpoint's `value` struct — it is silently lost on every crash-rehydrate", name)
		}
	}
}

// TestLoadCheckpointMissingIsNotAnError pins the not-found contract loadCharacterSnapshot leans on: a
// character with no checkpoint (never saved, or the TTL lapsed) reports found=false with a nil error, so
// the Postgres row wins rather than the login failing.
func TestLoadCheckpointMissingIsNotAnError(t *testing.T) {
	r := newTestRedis(t)
	got, found, err := r.LoadCheckpoint(context.Background(), "NoSuchCharacter")
	if err != nil {
		t.Fatalf("a missing checkpoint must not be an error, got %v", err)
	}
	if found {
		t.Fatalf("found=true for a character that was never checkpointed (snapshot %+v)", got)
	}
}

// TestCheckpointOverwritesThePriorValue: the checkpoint is a MIRROR, not a log. A second write for the same
// character must replace the first — otherwise a rehydrate could read a stale position.
func TestCheckpointOverwritesThePriorValue(t *testing.T) {
	r := newTestRedis(t)
	ctx := context.Background()

	first := fullSnapshot()
	if err := r.Checkpoint(ctx, first); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	second := fullSnapshot()
	second.ZoneRef, second.RoomRef, second.StateVersion = "crypt", "crypt:room:entrance", 43
	if err := r.Checkpoint(ctx, second); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	got, found, err := r.LoadCheckpoint(ctx, first.Name)
	if err != nil || !found {
		t.Fatalf("LoadCheckpoint: found=%v err=%v", found, err)
	}
	if got.ZoneRef != "crypt" || got.StateVersion != 43 {
		t.Fatalf("checkpoint did not overwrite: got zone %q version %d, want crypt/43", got.ZoneRef, got.StateVersion)
	}
}

// TestCheckpointEmptyZoneRefPreservesTheStoredZone pins the "" contract (#411): an empty ZoneRef means
// "leave the stored location alone", NOT "clear it".
//
// The world's only producer of ZoneRef (world.dumpCharacter) returns "" for a player who is inside a
// runtime-minted zone INSTANCE, because the instance's id is ephemeral and must never be persisted — a
// durable row naming a reaped instance is dangling by construction, and it is also a poisoned record aimed
// at the login path's instance guard. But RoomRef is NOT empty on that path: an instance hosts its
// TEMPLATE's authored rooms, so the snapshot carries a real room ref like "darkwood:room:lair".
//
// So a blanking write produces an internally inconsistent mirror — a real room with no zone — and this is
// the tier the login path PREFERS whenever it is the fresher of the two. The reconnect falls back to the
// home zone, cannot resolve that room there, and start-rooms the player. The save cadence alone (~10s) would
// do it to every dungeon occupant.
func TestCheckpointEmptyZoneRefPreservesTheStoredZone(t *testing.T) {
	r := newTestRedis(t)
	ctx := context.Background()

	anchored := fullSnapshot() // zone darkwood: where the player was before they stepped into the instance
	if err := r.Checkpoint(ctx, anchored); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// The instance-occupant checkpoint: no zone, but a real (template-authored) room.
	inside := fullSnapshot()
	inside.ZoneRef, inside.RoomRef, inside.StateVersion = "", "darkwood:room:lair", 43
	if err := r.Checkpoint(ctx, inside); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	got, found, err := r.LoadCheckpoint(ctx, anchored.Name)
	if err != nil || !found {
		t.Fatalf("LoadCheckpoint: found=%v err=%v", found, err)
	}
	if got.ZoneRef != anchored.ZoneRef {
		t.Fatalf("an empty ZoneRef CLEARED the checkpoint's zone (now %q, was %q) while room_ref kept %q. The "+
			"mirror now holds a real room with no zone: the login path prefers this tier when it is fresher, "+
			"falls back to the home zone, cannot resolve that room there, and start-rooms the player — durable "+
			"location loss on every save tick for anyone inside a zone instance (#411)",
			got.ZoneRef, anchored.ZoneRef, got.RoomRef)
	}
	// Everything else must still be overwritten: this is a targeted preserve, not a frozen record.
	if got.StateVersion != 43 || got.RoomRef != "darkwood:room:lair" {
		t.Fatalf("the rest of the checkpoint stopped overwriting: version %d room %q, want 43/darkwood:room:lair",
			got.StateVersion, got.RoomRef)
	}
	// The CONTROL: a non-empty ZoneRef still overwrites, so "" is the only preserving value.
	moved := fullSnapshot()
	moved.ZoneRef, moved.StateVersion = "crypt", 44
	if err := r.Checkpoint(ctx, moved); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got, _, _ := r.LoadCheckpoint(ctx, anchored.Name); got.ZoneRef != "crypt" {
		t.Fatalf("a real zone change was not written: zone %q, want crypt", got.ZoneRef)
	}
}
