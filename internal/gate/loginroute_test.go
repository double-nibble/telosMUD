package gate

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/double-nibble/telosmud/internal/directory"
)

// loginroute_test.go covers the gate's login routing policy (#320): resolve a returning player by the ZONE
// their placement records, not by the shard it records.
//
// A shard id says where the player WAS. It goes stale the instant that zone is rebalanced onto another
// shard, or that shard exits — and the old code then fell through to the fixed home zone, silently losing
// the player's location. A zone id is stable; ShardForZone names its current owner.

// resolver binds ResolveLoginShard to a fresh miniredis directory, matching how cmd/telos-gate calls it.
type resolver struct {
	dir      *directory.Redis
	homeZone string
	fallback string
}

func (r resolver) ShardForCharacter(characterID string) (string, bool) {
	return ResolveLoginShard(context.Background(), r.dir, characterID, r.homeZone, r.fallback)
}

func newLoginDir(t *testing.T, homeZone string) (resolver, *directory.Redis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")
	return resolver{dir: dir, homeZone: homeZone, fallback: "fallback:9090"}, dir
}

// TestLoginDirectoryRoutesByPlacementZone is the core of slice 2: the placement names a zone, and routing
// resolves that zone's CURRENT owner.
//
// The recorded SHARD is deliberately different from the zone's owner, so this test DISCRIMINATES: if the
// resolver consulted place.ShardID it would return addr-stale, and if it ignored the placement entirely it
// would return the home zone's addr-a. Only zone-routing yields addr-b.
func TestLoginDirectoryRoutesByPlacementZone(t *testing.T) {
	d, dir := newLoginDir(t, "midgaard")
	ctx := context.Background()

	for _, s := range []struct{ id, addr string }{{"shard-a", "addr-a"}, {"shard-b", "addr-b"}, {"shard-stale", "addr-stale"}} {
		if err := dir.RegisterShard(ctx, s.id, s.addr, directory.DefaultShardLease); err != nil {
			t.Fatal(err)
		}
	}
	if err := dir.RegisterZone(ctx, "midgaard", "shard-a"); err != nil {
		t.Fatal(err)
	}
	if err := dir.RegisterZone(ctx, "darkwood", "shard-b"); err != nil {
		t.Fatal(err)
	}
	// The record names shard-stale, but darkwood is owned by shard-b.
	if _, err := dir.RegisterPlacement(ctx, "Wanderer", "shard-stale", "darkwood", 1); err != nil {
		t.Fatal(err)
	}

	got, ok := d.ShardForCharacter("Wanderer")
	if !ok || got != "addr-b" {
		t.Fatalf("ShardForCharacter = %q, %v; want addr-b (the CURRENT owner of the placement's zone, not its recorded shard)", got, ok)
	}
}

// TestLoginDirectoryFollowsARebalancedZone is the failure the old shard-keyed routing could not survive:
// the player logs out in darkwood on shard-b, darkwood is rebalanced onto shard-c while they are offline,
// and they reconnect. The placement is NOT rewritten — routing must still find them.
func TestLoginDirectoryFollowsARebalancedZone(t *testing.T) {
	d, dir := newLoginDir(t, "midgaard")
	ctx := context.Background()

	for _, s := range []struct{ id, addr string }{{"shard-b", "addr-b"}, {"shard-c", "addr-c"}} {
		if err := dir.RegisterShard(ctx, s.id, s.addr, directory.DefaultShardLease); err != nil {
			t.Fatal(err)
		}
	}
	if err := dir.RegisterZone(ctx, "darkwood", "shard-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := dir.RegisterPlacement(ctx, "Wanderer", "shard-b", "darkwood", 1); err != nil {
		t.Fatal(err)
	}

	// The rebalance moves darkwood b -> c. The placement still says shard-b.
	if ok, err := dir.HandoverZone(ctx, "darkwood", "shard-b", "shard-c", directory.DefaultZoneLease); err != nil || !ok {
		t.Fatalf("HandoverZone: ok=%v err=%v", ok, err)
	}

	got, ok := d.ShardForCharacter("Wanderer")
	if !ok || got != "addr-c" {
		t.Fatalf("ShardForCharacter = %q, %v; want addr-c — routing must follow the ZONE, not the stale shard id", got, ok)
	}
}

// TestLoginDirectoryFallsBackToTheRecordedShardForALegacyRecord: a placement written before #320 has no
// zone field. Routing must still use its shard id rather than dropping to the home zone, so an upgrade
// needs no backfill.
func TestLoginDirectoryFallsBackToTheRecordedShardForALegacyRecord(t *testing.T) {
	d, dir := newLoginDir(t, "midgaard")
	ctx := context.Background()

	if err := dir.RegisterShard(ctx, "shard-b", "addr-b", directory.DefaultShardLease); err != nil {
		t.Fatal(err)
	}
	// Pre-#320 shape: shard + epoch, no zone. SetPlayerShard with an empty zone is the honest simulation
	// (the field is written but empty), and PlayerPlacement reads it as ZoneID == "".
	if ok, err := dir.SetPlayerShard(ctx, "Oldtimer", "shard-b", "", 1); err != nil || !ok {
		t.Fatalf("seed: ok=%v err=%v", ok, err)
	}

	got, ok := d.ShardForCharacter("Oldtimer")
	if !ok || got != "addr-b" {
		t.Fatalf("ShardForCharacter = %q, %v; want addr-b (the legacy shard-keyed fallback)", got, ok)
	}
}

// TestLoginDirectoryFallsBackToTheHomeZone: a brand-new character has no placement at all and must resolve
// through the home zone, exactly as before.
func TestLoginDirectoryFallsBackToTheHomeZone(t *testing.T) {
	d, dir := newLoginDir(t, "midgaard")
	ctx := context.Background()

	if err := dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease); err != nil {
		t.Fatal(err)
	}
	if err := dir.RegisterZone(ctx, "midgaard", "shard-a"); err != nil {
		t.Fatal(err)
	}

	got, ok := d.ShardForCharacter("Newbie")
	if !ok || got != "addr-a" {
		t.Fatalf("ShardForCharacter = %q, %v; want addr-a (the home zone's owner)", got, ok)
	}
}

// TestLoginDirectoryFallsBackToTheConfiguredTarget: with no directory records at all (a single-shard dev
// stack whose Redis is empty), routing degrades to the configured world target rather than refusing the
// login.
func TestLoginDirectoryFallsBackToTheConfiguredTarget(t *testing.T) {
	d, _ := newLoginDir(t, "midgaard")

	got, ok := d.ShardForCharacter("Nobody")
	if !ok || got != "fallback:9090" {
		t.Fatalf("ShardForCharacter = %q, %v; want the configured fallback", got, ok)
	}
}

// TestLoginDirectoryFallsBackWhenThePlacementZoneIsUnowned: the recorded zone exists but no shard currently
// leases it (mid-rebalance, or its owner died). Routing must not dead-end — it falls through to the
// recorded shard, then the home zone.
func TestLoginDirectoryFallsBackWhenThePlacementZoneIsUnowned(t *testing.T) {
	d, dir := newLoginDir(t, "midgaard")
	ctx := context.Background()

	if err := dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease); err != nil {
		t.Fatal(err)
	}
	if err := dir.RegisterZone(ctx, "midgaard", "shard-a"); err != nil {
		t.Fatal(err)
	}
	// "crypt" is recorded on the placement but leased by nobody, and shard-z is not registered either.
	if _, err := dir.RegisterPlacement(ctx, "Lost", "shard-z", "crypt", 1); err != nil {
		t.Fatal(err)
	}

	got, ok := d.ShardForCharacter("Lost")
	if !ok || got != "addr-a" {
		t.Fatalf("ShardForCharacter = %q, %v; want addr-a (home-zone fallback when the placement zone has no owner)", got, ok)
	}
}
