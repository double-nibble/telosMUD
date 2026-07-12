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

	if _, err := d.RegisterPlacement(ctx, "Frodo", "world-a", "darkwood", 3, 0); err != nil {
		t.Fatal(err)
	}
	ok, err := d.ClearPlayerShard(ctx, "Frodo", "world-a", "", 3, 0)
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

	if _, err := d.RegisterPlacement(ctx, "Sam", "world-a", "midgaard", 1, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.ClearPlayerShard(ctx, "Sam", "world-a", "", 1, 0); err != nil || !ok {
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
	if ok, err := d.ClearPlayerShard(ctx, "Merry", "world-b", "", 5, 0); err != nil || !ok {
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

	if _, err := d.RegisterPlacement(ctx, "Pippin", "world-a", "midgaard", 2, 0); err != nil {
		t.Fatal(err)
	}
	// The relog lands first: the player is now live on world-b.
	if ok, err := d.RegisterPlacement(ctx, "Pippin", "world-b", "crypt", 2, 0); err != nil || !ok {
		t.Fatalf("relog: ok=%v err=%v", ok, err)
	}

	// world-a's queued logout tombstone drains late. It names a shard that no longer owns the record.
	ok, err := d.ClearPlayerShard(ctx, "Pippin", "world-a", "", 2, 0)
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

	if _, err := d.RegisterPlacement(ctx, "Gimli", "world-a", "midgaard", 1, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.SetPlayerShard(ctx, "Gimli", "world-b", "darkwood", 2); err != nil || !ok {
		t.Fatalf("handoff: ok=%v err=%v", ok, err)
	}

	// The source shard's logout tombstone, carrying the OLD epoch, must be rejected.
	if ok, _ := d.ClearPlayerShard(ctx, "Gimli", "world-a", "", 1, 0); ok {
		t.Fatal("a tombstone at a stale epoch applied — it would have orphaned a handed-off player")
	}
	if p, _ := d.PlayerPlacement(ctx, "Gimli"); p.ShardID != "world-b" {
		t.Fatalf("the handed-off placement was cleared: %+v", p)
	}
}

// TestTombstoneOnAnUnknownPlayerIsANoOp: no record, nothing to clear, no error.
func TestTombstoneOnAnUnknownPlayerIsANoOp(t *testing.T) {
	d := newTestRedis(t)
	ok, err := d.ClearPlayerShard(context.Background(), "Ghost", "world-a", "", 1, 0)
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

	if _, err := d.RegisterPlacement(ctx, "Boromir", "world-a", "midgaard", 1, 0); err != nil {
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

// TestTombstoneFenceDiscriminatesASameShardSameEpochRelog is the #329 fix: the NONCE axis closes the one
// gap shard+epoch could not see. A same-shard relog resumes the SAME epoch (registerPlacement accepts an
// equal epoch by design; requiring `>` would make every login a no-op), so shard and epoch both match a
// late clear. The relog's fresh session nonce is what discriminates: the record now carries the new
// session's nonce, and the quitting session's late clear carries the OLD one, so the fence rejects it and
// the connected player's shard field survives.
//
// This used to be the "blind spot" test that pinned the OPPOSITE behavior (the clear applied, relying on
// world-side single-writer ordering the directory could not see). With #329 the directory enforces it
// unaided.
func TestTombstoneFenceDiscriminatesASameShardSameEpochRelog(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	// Session 1 registers with nonce 100.
	if _, err := d.RegisterPlacement(ctx, "Echo", "world-a", "midgaard", 4, 100); err != nil {
		t.Fatal(err)
	}
	// The relog: same shard, same resumed epoch, but a NEW session nonce (202). It rewrites the record's nonce.
	if ok, err := d.RegisterPlacement(ctx, "Echo", "world-a", "midgaard", 4, 202); err != nil || !ok {
		t.Fatalf("a same-shard relog must re-register: ok=%v err=%v", ok, err)
	}

	// Session 1's late quit tombstone, carrying its OLD nonce (100). Same shard, same epoch — only the nonce
	// discriminates, and it must fence this out now.
	ok, err := d.ClearPlayerShard(ctx, "Echo", "world-a", "", 4, 100)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a stale same-shard/same-epoch clear evicted a live placement — the nonce fence did not hold (#329)")
	}
	// The live player's shard field SURVIVES: the relog is still hosted, addressable, and routable.
	p, _ := d.PlayerPlacement(ctx, "Echo")
	if p.ShardID != "world-a" {
		t.Fatalf("the live relog's shard field was cleared by a stale tombstone: %+v (#329)", p)
	}
	if p.Epoch != 4 || p.ZoneID != "midgaard" || p.Nonce != 202 {
		t.Fatalf("the live record was corrupted: %+v — want {world-a midgaard 4 nonce=202}", p)
	}

	// And the SAME session's clear (matching nonce 202) still works: the fence rejects only a STALE session,
	// not the owner. This proves the nonce is a discriminator, not a blanket block.
	if ok, err := d.ClearPlayerShard(ctx, "Echo", "world-a", "", 4, 202); err != nil || !ok {
		t.Fatalf("the live session's own clear (matching nonce) must apply: ok=%v err=%v", ok, err)
	}
	if p, _ := d.PlayerPlacement(ctx, "Echo"); p.ShardID != "" {
		t.Fatalf("the owning session's clear did not tombstone: %+v", p)
	}
}

// TestHandoffClearsTheStaleNonceSoAnArrivalCanTombstone is the regression the distsys review caught (#329):
// a cross-shard handoff CAS writes {shard:B, epoch:E+1} but must NOT leave the SOURCE session's stale nonce
// in the record. If it did, an arrived player who quits before their destination register drains (the
// coalescing placement writer collapses register+clear into just the clear) would carry the DESTINATION
// nonce, mismatch the stale source nonce, and be wrongly fenced — leaving a permanent stale `shard` field
// (the tell/mail oracle would keep reporting them hosted on B). Pre-#329 this interleaving tombstoned
// correctly, so the nonce fence must not regress it.
func TestHandoffClearsTheStaleNonceSoAnArrivalCanTombstone(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	// Source session on world-a registered nonce 100.
	if _, err := d.RegisterPlacement(ctx, "Mover", "world-a", "midgaard", 5, 100); err != nil {
		t.Fatal(err)
	}
	// Handoff to world-b (epoch bumps). The CAS must clear the stale source nonce.
	if ok, err := d.SetPlayerShard(ctx, "Mover", "world-b", "darkwood", 6); err != nil || !ok {
		t.Fatalf("handoff CAS: ok=%v err=%v", ok, err)
	}
	if p, _ := d.PlayerPlacement(ctx, "Mover"); p.Nonce != 0 {
		t.Fatalf("the handoff CAS left a stale nonce (%d) — an arrival's own clear would be wrongly fenced (#329)", p.Nonce)
	}

	// The destination session (nonce 200) quits before its register drained: only the clear reaches the
	// directory, carrying 200. Against a nonce-less record it must tombstone on shard+epoch.
	ok, err := d.ClearPlayerShard(ctx, "Mover", "world-b", "", 6, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("an arrived-then-quit player was wrongly fenced by a stale CAS-era nonce — permanent stale shard field (#329)")
	}
	if p, _ := d.PlayerPlacement(ctx, "Mover"); p.ShardID != "" {
		t.Fatalf("the arrival's clean quit did not tombstone: %+v", p)
	}
}

// TestTombstoneNonceFenceIsPresentOnly pins the backward-compat / handoff-CAS carve-out: a record with NO
// nonce (a legacy pre-#329 record, or a handoff-CAS-only record whose destination has not registered yet) is
// still clearable on shard+epoch alone. The nonce check only bites when a nonce is present. This is what
// keeps SetPlayerShard (which writes no nonce) working and lets a rolling deploy self-heal.
func TestTombstoneNonceFenceIsPresentOnly(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	// A handoff-CAS write: shard+zone+epoch, no nonce field.
	if ok, err := d.SetPlayerShard(ctx, "Legacy", "world-a", "midgaard", 3); err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}
	// A clear carrying an arbitrary nonce must still apply — the record has no nonce to discriminate on.
	if ok, err := d.ClearPlayerShard(ctx, "Legacy", "world-a", "", 3, 999); err != nil || !ok {
		t.Fatalf("a nonce-less record must be clearable on shard+epoch: ok=%v err=%v", ok, err)
	}
	if p, _ := d.PlayerPlacement(ctx, "Legacy"); p.ShardID != "" {
		t.Fatalf("the nonce-less record was not tombstoned: %+v", p)
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
	if _, err := d.RegisterPlacement(ctx, "Rover", "world-a", "midgaard", 1, 0); err != nil {
		t.Fatal(err)
	}
	// They quit in darkwood. The tombstone carries it.
	if ok, err := d.ClearPlayerShard(ctx, "Rover", "world-a", "darkwood", 1, 0); err != nil || !ok {
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

	if _, err := d.RegisterPlacement(ctx, "Quiet", "world-a", "crypt", 2, 0); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.ClearPlayerShard(ctx, "Quiet", "world-a", "", 2, 0); err != nil || !ok {
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

	if _, err := d.RegisterPlacement(ctx, "Racer", "world-a", "midgaard", 1, 0); err != nil {
		t.Fatal(err)
	}
	// The player relogged on world-b before world-a's logout drained.
	if ok, err := d.RegisterPlacement(ctx, "Racer", "world-b", "crypt", 1, 0); err != nil || !ok {
		t.Fatalf("relog: ok=%v err=%v", ok, err)
	}
	// world-a's late tombstone claims they quit in darkwood. The fence must reject it whole.
	if ok, _ := d.ClearPlayerShard(ctx, "Racer", "world-a", "darkwood", 1, 0); ok {
		t.Fatal("a stale tombstone applied")
	}
	if p, _ := d.PlayerPlacement(ctx, "Racer"); p.ShardID != "world-b" || p.ZoneID != "crypt" {
		t.Fatalf("a fenced-out tombstone partially applied: %+v — it must not rewrite the zone either", p)
	}
}
