package checkpoint

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/world"
)

// ownerepoch_test.go pins the OWNERSHIP guard on the checkpoint tier (#432).
//
// This tier was the complete bypass. The Postgres fence can be perfectly intact and the rollback
// simply happens one layer up: the checkpoint is a SINGLE key per character name, written every ~10s
// by every live copy of that character, with no CAS at all — so a stale copy was the last writer
// roughly half the time. And because state_version only advances on a durable Postgres CAS, the stale
// and live copies sit at the SAME version for the whole window between a login and its first flush,
// where the login read's `>=` tie-break (#322) then preferred the checkpoint. Every guarantee the
// row-level fence provides was reachable around, through Redis.
//
// The guard is a Lua script evaluated SERVER-SIDE (the compare and the write must be one atomic step,
// or two ~10s pulses interleave between the HGET and the HSET), so these tests exercise it through a
// live Redis that actually runs the script — the package's established idiom, miniredis, which
// executes EVAL rather than emulating it. A pure-Go double would prove nothing about the script.

// guardedRedis returns a checkpoint tier over a live miniredis, plus the server handle so a test can
// inspect TTLs and plant raw at-rest values.
func guardedRedis(t *testing.T) (*Redis, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedis(rdb, "telos"), mr
}

func snapAt(name string, epoch, version uint64, room string) world.CharSnapshot {
	return world.CharSnapshot{
		PID: world.PersistID("11111111-2222-3333-4444-555555555555"), Name: name,
		ZoneRef: "midgaard", RoomRef: room, StateVersion: version, OwnerEpoch: epoch,
		State: world.StateJSON{Inventory: []world.ItemJSON{{ProtoRef: room + ":marker"}}},
	}
}

// TestCheckpointCannotRollBackAcrossAnEpoch is the guard's contract, table-driven over the three
// orderings a writer's epoch can have against the stored one.
func TestCheckpointCannotRollBackAcrossAnEpoch(t *testing.T) {
	cases := []struct {
		name        string
		storedEpoch uint64
		writerEpoch uint64
		wantApplied bool
		why         string
	}{
		{
			name: "a ZOMBIE's pulse is refused", storedEpoch: 4, writerEpoch: 3, wantApplied: false,
			why: "a superseded copy pulses this same key every ~10s; unguarded it is last-writer-wins roughly " +
				"half the time, and the next login rehydrates from it",
		},
		{
			name: "the OWNER's own later pulse applies", storedEpoch: 4, writerEpoch: 4, wantApplied: true,
			why: "the ordinary case — the guard must not break the tier it protects",
		},
		{
			name: "a NEWER owner takes the slot", storedEpoch: 4, writerEpoch: 5, wantApplied: true,
			why: "a fresh claim owns the character and therefore owns its checkpoint slot",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := guardedRedis(t)
			ctx := context.Background()

			require.NoError(t, r.Checkpoint(ctx, snapAt("Slot", tc.storedEpoch, 7, "darkwood:room:grove")))
			err := r.Checkpoint(ctx, snapAt("Slot", tc.writerEpoch, 99, "midgaard:room:temple"))

			if tc.wantApplied {
				require.NoError(t, err, "the guard must not break the tier it protects")
			} else {
				// A refusal is REPORTED, not swallowed. This tier pulses every ~10s against the Postgres
				// fence's ~60s, so it is the earliest detector of a double-own; a nil return here would
				// throw that signal away six times per Postgres cycle and let a zombie keep generating
				// unsaveable play (and externalizing wealth) for a full flush cadence. It must also be a
				// DISTINCT sentinel, not a generic error — the saver logs generic write failures at Debug
				// as "the crash window widened by a tick", which would misdescribe this entirely.
				require.ErrorIs(t, err, world.ErrCheckpointNotOwner,
					"an ownership refusal must surface as ErrCheckpointNotOwner so the saver can route it to the "+
						"same eviction the Postgres verdict drives (#432); got %v", err)
			}

			got, found, err := r.LoadCheckpoint(ctx, "Slot")
			require.NoError(t, err)
			require.True(t, found)

			if tc.wantApplied {
				require.Equal(t, "midgaard:room:temple", got.RoomRef, "the write must have applied: %s", tc.why)
				require.Equal(t, tc.writerEpoch, got.OwnerEpoch, "the slot's epoch must advance with the writer")
			} else {
				require.Equal(t, "darkwood:room:grove", got.RoomRef,
					"the checkpoint slot was rolled back by a superseded owner (#432): %s", tc.why)
				require.Equal(t, tc.storedEpoch, got.OwnerEpoch,
					"a refused write must not lower the stored epoch either, or the next zombie pulse succeeds")
			}
		})
	}
}

// TestCheckpointOwnerEpochRoundTripsThroughTheHash is the FIELD-DROP guard for the new axis.
//
// The checkpoint's `value` struct is a hand-written serialization, separate from CharSnapshot and
// from the `state` JSONB — the one place in the ladder where a snapshot is copied field-by-field into
// a private struct and back. This repo has shipped a silently-dropped field through a store round
// trip three times (Rounds 11, 35, 36). A dropped OwnerEpoch here would leave this tier unfenced
// (every stored epoch reading back as 0) while the others were fenced, which is exactly where a
// bypass lives — and it would be invisible to the Postgres tests.
func TestCheckpointOwnerEpochRoundTripsThroughTheHash(t *testing.T) {
	r, mr := guardedRedis(t)
	ctx := context.Background()

	require.NoError(t, r.Checkpoint(ctx, snapAt("Trip", 42, 7, "midgaard:room:market")))
	got, found, err := r.LoadCheckpoint(ctx, "Trip")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 42, got.OwnerEpoch,
		"OwnerEpoch was DROPPED by the checkpoint round trip (#432). Every stored epoch would read back as 0, "+
			"the login read's comparator would rank every checkpoint lowest, and the guard would compare against "+
			"a value it never actually stored")

	// The epoch also lives in its own HASH FIELD, not only inside the blob: the guard compares a number
	// server-side rather than decoding JSON, so a value present in `data` but missing from `epoch` is
	// unfenced no matter what the round trip says.
	epochField := mr.HGet("telos:ckpt:char:v2:Trip", "epoch")
	require.Equal(t, "42", epochField,
		"the guard reads this field, not the blob; a blob-only epoch leaves the compare reading nil forever")
}

// TestCheckpointGuardRefreshesTheTTLOnlyWhenItApplies: the TTL is the tier's self-cleaning property,
// and a zombie that keeps refreshing it would keep its own stale value alive indefinitely — extending
// the window in which a crash-rehydrate can pick it up.
func TestCheckpointGuardRefreshesTheTTLOnlyWhenItApplies(t *testing.T) {
	r, mr := guardedRedis(t)
	ctx := context.Background()
	key := "telos:ckpt:char:v2:Aging"

	require.NoError(t, r.Checkpoint(ctx, snapAt("Aging", 4, 7, "darkwood:room:grove")))
	full := mr.TTL(key)
	require.Equal(t, DefaultTTL, full, "premise: an applied write sets the full TTL")

	// Age the key, then let a zombie pulse at it.
	mr.FastForward(DefaultTTL / 2)
	aged := mr.TTL(key)
	require.Less(t, aged, full, "premise: the key aged")

	require.ErrorIs(t, r.Checkpoint(ctx, snapAt("Aging", 3, 99, "midgaard:room:temple")), world.ErrCheckpointNotOwner)
	require.Equal(t, aged, mr.TTL(key),
		"a REFUSED write refreshed the TTL (#432): a zombie pulsing every ~10s would keep its stale value alive "+
			"forever, and the slot would never self-heal by expiring")

	// The legitimate owner's write does refresh it.
	require.NoError(t, r.Checkpoint(ctx, snapAt("Aging", 4, 8, "darkwood:room:grove")))
	require.Equal(t, full, mr.TTL(key), "an APPLIED write must restore the full TTL")
}

// TestCheckpointGuardTreatsAMissingEpochAsUnfenced covers the rolling-deploy tail: a value written
// before the epoch field existed (or by any writer that omits it) must compare as 0 — losing to any
// real claim — and the slot must self-heal on the first guarded write rather than erroring forever.
func TestCheckpointGuardTreatsAMissingEpochAsUnfenced(t *testing.T) {
	r, mr := guardedRedis(t)
	ctx := context.Background()
	key := "telos:ckpt:char:v2:Legacy"

	// Plant a hash with `data` but no `epoch`, as a writer that predates the field would leave it.
	require.NoError(t, r.Checkpoint(ctx, snapAt("Legacy", 0, 3, "midgaard:room:temple")))
	mr.HDel(key, "epoch")

	require.NoError(t, r.Checkpoint(ctx, snapAt("Legacy", 1, 3, "darkwood:room:grove")),
		"an absent epoch field must compare as 0, not raise")
	got, found, err := r.LoadCheckpoint(ctx, "Legacy")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "darkwood:room:grove", got.RoomRef,
		"a real claim must take an unfenced slot; the fence self-heals on the first guarded write")
	require.EqualValues(t, 1, got.OwnerEpoch)
}

// TestCheckpointKeyIsNamespacedAwayFromThePreV2Format pins the deliberate at-rest format break. The
// value went from a bare string (SET/GET) to a HASH, and HGET against a pre-#432 string key raises
// WRONGTYPE — a Redis Lua error that propagates, so reusing the old key would make EVERY checkpoint
// write fail for the length of a rolling deploy. Splitting the namespace trades one TTL window of
// merely-cold crash recovery for that, which is the right trade for a tier that sits over an
// authoritative Postgres.
func TestCheckpointKeyIsNamespacedAwayFromThePreV2Format(t *testing.T) {
	r, mr := guardedRedis(t)
	ctx := context.Background()

	// An old-format value left behind by a pre-#432 shard.
	require.NoError(t, mr.Set("telos:ckpt:char:Rolling", `{"name":"Rolling","state_version":5}`))

	require.NoError(t, r.Checkpoint(ctx, snapAt("Rolling", 2, 6, "midgaard:room:market")),
		"a stale pre-v2 string key must not make the guarded write fail with WRONGTYPE — that would break "+
			"checkpointing for every character for the whole deploy")
	got, found, err := r.LoadCheckpoint(ctx, "Rolling")
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 2, got.OwnerEpoch)
	require.Equal(t, "midgaard:room:market", got.RoomRef)

	// And an old shard's SET over the old key cannot corrupt the new slot.
	require.NoError(t, mr.Set("telos:ckpt:char:Rolling", `{"name":"Rolling","state_version":99}`))
	got, _, err = r.LoadCheckpoint(ctx, "Rolling")
	require.NoError(t, err)
	require.EqualValues(t, 6, got.StateVersion, "the v2 slot must be unaffected by writes to the legacy key")
}
