package directory

import (
	"context"
	"errors"
	"testing"
)

// placement_test.go covers the #320 zone-bearing placement record and RegisterPlacement, the login /
// zone-change writer that sits beside the handoff CAS.
//
// The two writers have deliberately different epoch rules, and getting that wrong breaks something
// important in each direction:
//   - SetPlayerShard (handoff) demands a STRICTLY GREATER epoch. It is the monotonic fence: a stale or
//     duplicated handoff must never roll a player back to a shard they already left.
//   - RegisterPlacement (login / zone change) accepts an EQUAL epoch, because a login re-registers at the
//     epoch it just resumed from this very directory. Demanding `>` would make every login a silent no-op.
//
// What RegisterPlacement must never do is clobber a NEWER placement — an in-flight handoff writing
// epoch+1 has to win against a concurrent login still holding the old epoch.

func TestRegisterPlacementRecordsZone(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.PlayerPlacement(ctx, "Frodo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown player: want ErrNotFound, got %v", err)
	}

	ok, err := d.RegisterPlacement(ctx, "Frodo", "world-a", "midgaard", 1)
	if err != nil || !ok {
		t.Fatalf("first registration: ok=%v err=%v", ok, err)
	}
	p, err := d.PlayerPlacement(ctx, "Frodo")
	if err != nil {
		t.Fatal(err)
	}
	if p.ShardID != "world-a" || p.ZoneID != "midgaard" || p.Epoch != 1 {
		t.Fatalf("placement = %+v, want {world-a midgaard 1}", p)
	}
}

// TestRegisterPlacementAcceptsAnEqualEpoch is the whole reason RegisterPlacement is not just casPlacement.
// A login reads its epoch from this directory and re-registers AT that epoch; a strictly-greater rule would
// silently drop every login write, leaving the placement pointing at wherever the player last handed off.
func TestRegisterPlacementAcceptsAnEqualEpoch(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.RegisterPlacement(ctx, "Sam", "world-a", "midgaard", 5); err != nil {
		t.Fatal(err)
	}
	// Same epoch, different shard + zone: this is a relog onto a different shard after a rebalance.
	ok, err := d.RegisterPlacement(ctx, "Sam", "world-b", "darkwood", 5)
	if err != nil || !ok {
		t.Fatalf("equal epoch must be accepted by RegisterPlacement: ok=%v err=%v", ok, err)
	}
	if p, _ := d.PlayerPlacement(ctx, "Sam"); p.ShardID != "world-b" || p.ZoneID != "darkwood" || p.Epoch != 5 {
		t.Fatalf("placement = %+v, want {world-b darkwood 5}", p)
	}
}

// TestRegisterPlacementCannotClobberANewerEpoch is the fence. A handoff CAS moves the player to epoch+1;
// a login still holding the old epoch must NOT overwrite it, or the directory would point at a shard the
// player has already left — the exact rollback the monotonic epoch exists to prevent.
func TestRegisterPlacementCannotClobberANewerEpoch(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	// The player is on world-a at epoch 1, then hands off to world-b at epoch 2.
	if _, err := d.RegisterPlacement(ctx, "Merry", "world-a", "midgaard", 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.SetPlayerShard(ctx, "Merry", "world-b", "darkwood", 2); err != nil || !ok {
		t.Fatalf("handoff CAS: ok=%v err=%v", ok, err)
	}

	// A racing login registration, still carrying epoch 1, must be refused outright.
	ok, err := d.RegisterPlacement(ctx, "Merry", "world-a", "midgaard", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a stale-epoch registration must not apply — it would roll the player back to the shard they left")
	}
	if p, _ := d.PlayerPlacement(ctx, "Merry"); p.ShardID != "world-b" || p.ZoneID != "darkwood" || p.Epoch != 2 {
		t.Fatalf("placement was rolled back by a stale registration: %+v", p)
	}
}

// TestRegisterPlacementNeverLowersTheEpoch: even an ACCEPTED registration must keep the stored epoch at the
// high-water mark, because the handoff CAS's `>` rule is measured against it. Lowering it would let a
// replayed handoff at an old epoch apply again.
func TestRegisterPlacementNeverLowersTheEpoch(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if ok, err := d.SetPlayerShard(ctx, "Pippin", "world-a", "midgaard", 7); err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}
	// Equal epoch -> accepted, and the stored epoch stays 7.
	if ok, err := d.RegisterPlacement(ctx, "Pippin", "world-a", "crypt", 7); err != nil || !ok {
		t.Fatalf("equal epoch: ok=%v err=%v", ok, err)
	}
	p, _ := d.PlayerPlacement(ctx, "Pippin")
	if p.Epoch != 7 {
		t.Fatalf("epoch = %d, want 7 (the fence must not move)", p.Epoch)
	}
	if p.ZoneID != "crypt" {
		t.Fatalf("zone = %q, want crypt (the zone change must have applied)", p.ZoneID)
	}
	// And a replayed handoff at the old epoch is still rejected.
	if ok, _ := d.SetPlayerShard(ctx, "Pippin", "world-b", "darkwood", 7); ok {
		t.Fatal("a replayed handoff at the stored epoch must still be rejected")
	}
}

// TestSetPlayerShardRecordsZone pins that the handoff CAS carries the destination zone too — otherwise a
// player who hands off cross-shard and logs out there has a shard-only record, and the gate cannot route
// them after that zone is later rebalanced.
func TestSetPlayerShardRecordsZone(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if ok, err := d.SetPlayerShard(ctx, "Gimli", "world-b", "darkwood", 3); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if p, _ := d.PlayerPlacement(ctx, "Gimli"); p.ZoneID != "darkwood" {
		t.Fatalf("handoff CAS did not record the destination zone: %+v", p)
	}
}

// TestPlacementZoneIsEmptyForALegacyRecord: a hash written before #320 has no `zone` field. Reading it must
// yield ZoneID == "" and a valid shard, so the gate's fallback (route by shard id) still works and an
// upgrade needs no backfill.
func TestPlacementZoneIsEmptyForALegacyRecord(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	// Simulate the pre-#320 shape: shard + epoch, no zone.
	if err := d.rdb.HSet(ctx, d.playerKey("Legolas"), "shard", "world-a", "epoch", "4").Err(); err != nil {
		t.Fatal(err)
	}
	p, err := d.PlayerPlacement(ctx, "Legolas")
	if err != nil {
		t.Fatalf("a legacy record must still read cleanly, got %v", err)
	}
	if p.ShardID != "world-a" || p.Epoch != 4 {
		t.Fatalf("legacy record misread: %+v", p)
	}
	if p.ZoneID != "" {
		t.Fatalf("ZoneID = %q, want \"\" for a legacy record", p.ZoneID)
	}
}
