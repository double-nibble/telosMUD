package directory

import (
	"context"
	"errors"
	"testing"
)

// tombstone_test.go covers #70: a clean logout TOMBSTONES the placement (drops the `shard` field) instead of
// deleting the record.
//
// Deleting it outright — which is what the issue's title literally asked for — breaks three things:
//   - the tell/mail existence oracle would answer "there is no player by that name" for an offline
//     character, so tells to them would be refused rather than queued;
//   - the monotonic `epoch` is the fence the handoff CAS compares against, and a deleted key lets a delayed
//     or retried SetPlayerShard from an earlier handoff find `cur == nil` and APPLY;
//   - the `zone` is the reconnect routing key (#320), so a returning player would lose their location.
//
// The tombstone keeps epoch and zone, drops only shard, and is FENCED: it applies only when the record still
// names the clearing shard at the clearing epoch.

func TestClearPlayerShardKeepsEpochAndZone(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.RegisterPlacement(ctx, "Frodo", "world-a", "darkwood", 3); err != nil {
		t.Fatal(err)
	}
	ok, err := d.ClearPlayerShard(ctx, "Frodo", "world-a", "", 3)
	if err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}

	p, err := d.PlayerPlacement(ctx, "Frodo")
	if err != nil {
		t.Fatalf("the record must SURVIVE a clean logout, got %v", err)
	}
	if p.ShardID != "" {
		t.Fatalf("shard = %q, want \"\" (the tombstone must drop it)", p.ShardID)
	}
	if p.ZoneID != "darkwood" {
		t.Fatalf("zone = %q, want darkwood — a returning player routes by it (#320)", p.ZoneID)
	}
	if p.Epoch != 3 {
		t.Fatalf("epoch = %d, want 3 — it is the handoff CAS fence", p.Epoch)
	}
}

// TestTombstonedPlayerStaysTellAddressable is the regression for the tell/mail oracle. `found` reports
// EXISTENCE, not presence: an offline character must still resolve, or tells to them are refused outright.
func TestTombstonedPlayerStaysTellAddressable(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.RegisterPlacement(ctx, "Sam", "world-a", "midgaard", 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.ClearPlayerShard(ctx, "Sam", "world-a", "", 1); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}

	shardID, found, err := d.PlayerShard(ctx, "Sam")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("a cleanly-logged-out character must still EXIST to the tell oracle, or their tells are refused with \"no player by that name\"")
	}
	if shardID != "" {
		t.Fatalf("shardID = %q, want \"\" — the caller distinguishes 'exists' from 'is hosted'", shardID)
	}

	// A genuinely unknown name still does not resolve.
	if _, found, err := d.PlayerShard(ctx, "Nobody"); err != nil || found {
		t.Fatalf("an unknown name must not resolve: found=%v err=%v", found, err)
	}
}

// TestTombstonePreservesTheHandoffFence: the epoch survives, so a REPLAYED handoff CAS at the old epoch is
// still rejected after logout. A blind DEL would have let it apply (cur == nil), resurrecting a stale
// placement pointing at a shard the player left.
func TestTombstonePreservesTheHandoffFence(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if ok, err := d.SetPlayerShard(ctx, "Merry", "world-b", "darkwood", 5); err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}
	if ok, err := d.ClearPlayerShard(ctx, "Merry", "world-b", "", 5); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}

	// A delayed/retried handoff CAS from before the logout must NOT apply.
	if ok, _ := d.SetPlayerShard(ctx, "Merry", "world-b", "darkwood", 5); ok {
		t.Fatal("a replayed handoff CAS applied after logout — the tombstone dropped the monotonic fence")
	}
	if ok, _ := d.SetPlayerShard(ctx, "Merry", "world-c", "crypt", 4); ok {
		t.Fatal("an OLDER handoff CAS applied after logout — the fence is gone")
	}
	// A genuinely newer one still wins (the fence is a floor, not a wall).
	if ok, err := d.SetPlayerShard(ctx, "Merry", "world-c", "crypt", 6); err != nil || !ok {
		t.Fatalf("a newer handoff must still apply: ok=%v err=%v", ok, err)
	}
}

// TestTombstoneIsFencedAgainstAFastRelog is the race the fence exists for. The player quits on world-a; the
// logout write is still queued; they reconnect and world-b registers them. The late tombstone must NOT evict
// the live placement.
func TestTombstoneIsFencedAgainstAFastRelog(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.RegisterPlacement(ctx, "Pippin", "world-a", "midgaard", 2); err != nil {
		t.Fatal(err)
	}
	// The relog lands first: the player is now live on world-b.
	if ok, err := d.RegisterPlacement(ctx, "Pippin", "world-b", "crypt", 2); err != nil || !ok {
		t.Fatalf("relog: ok=%v err=%v", ok, err)
	}

	// world-a's queued logout tombstone drains late. It names a shard that no longer owns the record.
	ok, err := d.ClearPlayerShard(ctx, "Pippin", "world-a", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a stale tombstone evicted a live placement — the shard fence did not hold")
	}
	if p, _ := d.PlayerPlacement(ctx, "Pippin"); p.ShardID != "world-b" || p.ZoneID != "crypt" {
		t.Fatalf("the live placement was clobbered by a late logout: %+v", p)
	}
}

// TestTombstoneIsFencedAgainstAHandoff: the same fence, on the epoch axis. The player was handed off to
// another shard (epoch bumped) before the source's logout write drained.
func TestTombstoneIsFencedAgainstAHandoff(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.RegisterPlacement(ctx, "Gimli", "world-a", "midgaard", 1); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.SetPlayerShard(ctx, "Gimli", "world-b", "darkwood", 2); err != nil || !ok {
		t.Fatalf("handoff: ok=%v err=%v", ok, err)
	}

	// The source shard's logout tombstone, carrying the OLD epoch, must be rejected.
	if ok, _ := d.ClearPlayerShard(ctx, "Gimli", "world-a", "", 1); ok {
		t.Fatal("a tombstone at a stale epoch applied — it would have orphaned a handed-off player")
	}
	if p, _ := d.PlayerPlacement(ctx, "Gimli"); p.ShardID != "world-b" {
		t.Fatalf("the handed-off placement was cleared: %+v", p)
	}
}

// TestTombstoneOnAnUnknownPlayerIsANoOp: no record, nothing to clear, no error.
func TestTombstoneOnAnUnknownPlayerIsANoOp(t *testing.T) {
	d := newTestRedis(t)
	ok, err := d.ClearPlayerShard(context.Background(), "Ghost", "world-a", "", 1)
	if err != nil {
		t.Fatalf("clearing an unknown player must not error: %v", err)
	}
	if ok {
		t.Fatal("clearing an unknown player reported a write")
	}
}

// TestClearPlayerStillDeletesTheWholeRecord: ClearPlayer is now CHARACTER DELETION. It must remove the
// record entirely, so the character stops existing to the oracle.
func TestClearPlayerStillDeletesTheWholeRecord(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.RegisterPlacement(ctx, "Boromir", "world-a", "midgaard", 1); err != nil {
		t.Fatal(err)
	}
	if err := d.ClearPlayer(ctx, "Boromir"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PlayerPlacement(ctx, "Boromir"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after ClearPlayer: want ErrNotFound, got %v", err)
	}
	if _, found, _ := d.PlayerShard(ctx, "Boromir"); found {
		t.Fatal("a deleted character must not resolve to the tell oracle")
	}
}

// TestPlacementExistenceIsKeyedOnEpochNotShard pins the re-keying #70 required. Before it, PlayerPlacement
// reported ErrNotFound when the `shard` field was missing — which is exactly the state a tombstone leaves.
func TestPlacementExistenceIsKeyedOnEpochNotShard(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	// A hand-built record with an epoch and a zone but NO shard: the tombstoned shape.
	if err := d.rdb.HSet(ctx, d.playerKey("Faramir"), "epoch", "9", "zone", "crypt").Err(); err != nil {
		t.Fatal(err)
	}
	p, err := d.PlayerPlacement(ctx, "Faramir")
	if err != nil {
		t.Fatalf("a shard-less record must still be found (existence is keyed on epoch), got %v", err)
	}
	if p.Epoch != 9 || p.ZoneID != "crypt" || p.ShardID != "" {
		t.Fatalf("placement = %+v, want {\"\" crypt 9}", p)
	}

	// And a record with a shard but no epoch — which no writer produces — is treated as absent.
	if err := d.rdb.HSet(ctx, d.playerKey("Denethor"), "shard", "world-a").Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PlayerPlacement(ctx, "Denethor"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an epoch-less record must read as absent, got %v", err)
	}
}

// TestTombstoneFenceIsBlindToASameShardSameEpochRelog documents, deliberately, the ONE axis the fence cannot
// see — so that nobody reads `clearPlayerShard` and assumes it protects a live placement unconditionally.
//
// A same-shard relog resumes the SAME epoch (registerPlacement accepts an equal epoch by design; requiring
// `>` would make every login a no-op). So a late clear carrying that shard and that epoch matches the live
// record on both axes, and applies.
//
// This is safe today only because of ORDERING outside the directory: detach offers the clear before leave()
// removes the session, and one serial writer drains a per-player coalescing map, so a relog's registration is
// always written after the clear. The directory cannot see any of that. Tracked in #329.
//
// The assertion below pins the ACTUAL behavior. If someone strengthens the fence, this test should be
// inverted — deliberately, with #329 closed.
func TestTombstoneFenceIsBlindToASameShardSameEpochRelog(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.RegisterPlacement(ctx, "Echo", "world-a", "midgaard", 4); err != nil {
		t.Fatal(err)
	}
	// The relog: same shard, same resumed epoch.
	if ok, err := d.RegisterPlacement(ctx, "Echo", "world-a", "midgaard", 4); err != nil || !ok {
		t.Fatalf("a same-shard relog must re-register: ok=%v err=%v", ok, err)
	}

	// The quit's late clear, carrying the same pair. The fence has nothing to discriminate on.
	ok, err := d.ClearPlayerShard(ctx, "Echo", "world-a", "", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the fence rejected a same-shard/same-epoch clear — if you strengthened it, invert this test and close #329")
	}
	// And this is what "blind" costs: a live player's shard field is gone. Harmless today (nothing routes on
	// it, and existence is epoch-keyed), but it is not the fence that saves us.
	p, _ := d.PlayerPlacement(ctx, "Echo")
	if p.ShardID != "" {
		t.Fatalf("expected the shard field to have been cleared: %+v", p)
	}
	if p.Epoch != 4 || p.ZoneID != "midgaard" {
		t.Fatalf("the clear must still be a tombstone, not a delete: %+v", p)
	}
	if _, found, _ := d.PlayerShard(ctx, "Echo"); !found {
		t.Fatal("even a spurious tombstone must leave the player addressable")
	}
}

// TestTombstoneRecordsTheQuittingZone is the fix for a bug the world-level test caught: the placement writer
// coalesces per player, so a logout offered while a zone-change registration is still pending REPLACES it.
// Without the tombstone carrying the zone, the record would keep naming the zone the player walked out of —
// and a reconnect would route by that stale zone, straight to the class of misroute #320 exists to prevent.
func TestTombstoneRecordsTheQuittingZone(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	// The player logged in in midgaard; their walk to darkwood was never written.
	if _, err := d.RegisterPlacement(ctx, "Rover", "world-a", "midgaard", 1); err != nil {
		t.Fatal(err)
	}
	// They quit in darkwood. The tombstone carries it.
	if ok, err := d.ClearPlayerShard(ctx, "Rover", "world-a", "darkwood", 1); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}

	p, _ := d.PlayerPlacement(ctx, "Rover")
	if p.ZoneID != "darkwood" {
		t.Fatalf("zone = %q, want darkwood — the tombstone must record where the player actually quit", p.ZoneID)
	}
	if p.ShardID != "" || p.Epoch != 1 {
		t.Fatalf("placement = %+v, want a tombstone that keeps the epoch", p)
	}
}

// TestTombstoneWithNoZoneLeavesTheStoredZone: an empty zoneID means "I have nothing better to say", so the
// existing routing key survives.
func TestTombstoneWithNoZoneLeavesTheStoredZone(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.RegisterPlacement(ctx, "Quiet", "world-a", "crypt", 2); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.ClearPlayerShard(ctx, "Quiet", "world-a", "", 2); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}
	if p, _ := d.PlayerPlacement(ctx, "Quiet"); p.ZoneID != "crypt" {
		t.Fatalf("zone = %q, want crypt (unchanged)", p.ZoneID)
	}
}

// TestAFencedOutTombstoneDoesNotRewriteTheZone: if the fence rejects the clear, it must not have written the
// zone either — a stale logout must be a total no-op, not a partial one.
func TestAFencedOutTombstoneDoesNotRewriteTheZone(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.RegisterPlacement(ctx, "Racer", "world-a", "midgaard", 1); err != nil {
		t.Fatal(err)
	}
	// The player relogged on world-b before world-a's logout drained.
	if ok, err := d.RegisterPlacement(ctx, "Racer", "world-b", "crypt", 1); err != nil || !ok {
		t.Fatalf("relog: ok=%v err=%v", ok, err)
	}
	// world-a's late tombstone claims they quit in darkwood. The fence must reject it whole.
	if ok, _ := d.ClearPlayerShard(ctx, "Racer", "world-a", "darkwood", 1); ok {
		t.Fatal("a stale tombstone applied")
	}
	if p, _ := d.PlayerPlacement(ctx, "Racer"); p.ShardID != "world-b" || p.ZoneID != "crypt" {
		t.Fatalf("a fenced-out tombstone partially applied: %+v — it must not rewrite the zone either", p)
	}
}
