package directory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *Redis {
	t.Helper()
	d, _ := newTestRedisWithClock(t)
	return d
}

// newTestRedisWithClock also returns the miniredis handle, whose SetTime is the seam for anything that reads
// the SERVER clock inside a Lua script (the drain-target reservation expiries, #284).
func newTestRedisWithClock(t *testing.T) (*Redis, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedis(rdb, "test"), mr
}

func TestZoneRegistration(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.ShardForZone(ctx, "midgaard"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unregistered zone: want ErrNotFound, got %v", err)
	}
	if err := d.RegisterZone(ctx, "midgaard", "world-a:9090"); err != nil {
		t.Fatal(err)
	}
	addr, err := d.ShardForZone(ctx, "midgaard")
	if err != nil || addr != "world-a:9090" {
		t.Fatalf("ShardForZone = %q, %v; want world-a:9090", addr, err)
	}
}

func TestZoneClaimExclusiveAndRelease(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	// A claims the zone.
	if ok, err := d.ClaimZone(ctx, "midgaard", "shard-a", DefaultZoneLease); err != nil || !ok {
		t.Fatalf("A claim: ok=%v err=%v", ok, err)
	}
	// A DIFFERENT shard cannot claim a live-leased zone — the two-writer guard.
	if ok, err := d.ClaimZone(ctx, "midgaard", "shard-b", DefaultZoneLease); err != nil || ok {
		t.Fatalf("B claim should fail while A holds the lease: ok=%v err=%v", ok, err)
	}
	if addr, _ := d.ShardForZone(ctx, "midgaard"); addr != "shard-a" {
		t.Fatalf("ShardForZone = %q, want shard-a", addr)
	}
	// The owner may renew its own lease.
	if ok, err := d.ClaimZone(ctx, "midgaard", "shard-a", DefaultZoneLease); err != nil || !ok {
		t.Fatalf("A renew: ok=%v err=%v", ok, err)
	}
	// After release, another shard can take over.
	if err := d.ReleaseZone(ctx, "midgaard", "shard-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ShardForZone(ctx, "midgaard"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after release: want ErrNotFound, got %v", err)
	}
	if ok, err := d.ClaimZone(ctx, "midgaard", "shard-b", DefaultZoneLease); err != nil || !ok {
		t.Fatalf("B claim after release: ok=%v err=%v", ok, err)
	}
	if addr, _ := d.ShardForZone(ctx, "midgaard"); addr != "shard-b" {
		t.Fatalf("ShardForZone = %q, want shard-b", addr)
	}
}

func TestZoneLeaseExpiry(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()
	// A short-lived claim: once it lapses the zone reads as unhosted and is reclaimable.
	if ok, err := d.ClaimZone(ctx, "midgaard", "shard-a", 50*time.Millisecond); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if addr, _ := d.ShardForZone(ctx, "midgaard"); addr != "shard-a" {
		t.Fatalf("ShardForZone = %q, want shard-a", addr)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := d.ShardForZone(ctx, "midgaard"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after lease expiry: want ErrNotFound, got %v", err)
	}
	if ok, err := d.ClaimZone(ctx, "midgaard", "shard-b", DefaultZoneLease); err != nil || !ok {
		t.Fatalf("reclaim after expiry: ok=%v err=%v", ok, err)
	}
	// The old owner's late release must NOT yank the new owner's claim.
	if err := d.ReleaseZone(ctx, "midgaard", "shard-a"); err != nil {
		t.Fatal(err)
	}
	if addr, _ := d.ShardForZone(ctx, "midgaard"); addr != "shard-b" {
		t.Fatalf("after stale release: ShardForZone = %q, want shard-b", addr)
	}
}

func TestShardRegistryAndEndpoint(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	// Unregistered shard: not found.
	if _, err := d.EndpointForShard(ctx, "shard-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unregistered shard: want ErrNotFound, got %v", err)
	}
	// Register and resolve.
	if err := d.RegisterShard(ctx, "shard-a", "world-a:9090", DefaultShardLease); err != nil {
		t.Fatal(err)
	}
	if ep, err := d.EndpointForShard(ctx, "shard-a"); err != nil || ep != "world-a:9090" {
		t.Fatalf("EndpointForShard = %q, %v; want world-a:9090", ep, err)
	}
	// Deregister: resolves to not-found again.
	if err := d.DeregisterShard(ctx, "shard-a", "world-a:9090"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EndpointForShard(ctx, "shard-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after deregister: want ErrNotFound, got %v", err)
	}
}

// TestListShards covers the live-fleet view the placement coordinator watches: only registered,
// unexpired shards are listed; a deregistered (or lapsed) one drops out.
func TestListShards(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if got, err := d.ListShards(ctx); err != nil || len(got) != 0 {
		t.Fatalf("ListShards on empty = %v, %v; want []", got, err)
	}
	for _, s := range []struct{ id, ep string }{{"shard-a", "world-a:9090"}, {"shard-b", "world-b:9090"}} {
		if err := d.RegisterShard(ctx, s.id, s.ep, DefaultShardLease); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.ListShards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListShards = %v, want 2 live shards", got)
	}
	// Deregister one: it leaves the live view.
	if err := d.DeregisterShard(ctx, "shard-a", "world-a:9090"); err != nil {
		t.Fatal(err)
	}
	got, err = d.ListShards(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "shard-b" {
		t.Fatalf("after deregister: ListShards = %v, want [shard-b]", got)
	}
}

// TestShardIdConflict covers the two-writer guard: a second process booting with the
// same shard id but a different endpoint is refused, while the legitimate owner keeps
// renewing and a same-endpoint restart is allowed.
func TestShardIdConflict(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if err := d.RegisterShard(ctx, "shard-a", "world-a:9090", DefaultShardLease); err != nil {
		t.Fatal(err)
	}
	// A different process claiming the same id is refused.
	if err := d.RegisterShard(ctx, "shard-a", "world-rogue:9090", DefaultShardLease); !errors.Is(err, ErrShardConflict) {
		t.Fatalf("duplicate shard id: want ErrShardConflict, got %v", err)
	}
	// The legitimate owner still renews (same endpoint).
	if err := d.RegisterShard(ctx, "shard-a", "world-a:9090", DefaultShardLease); err != nil {
		t.Fatalf("owner renewal should succeed: %v", err)
	}
	// The rogue's late deregister must NOT remove the real owner's registration.
	if err := d.DeregisterShard(ctx, "shard-a", "world-rogue:9090"); err != nil {
		t.Fatal(err)
	}
	if ep, _ := d.EndpointForShard(ctx, "shard-a"); ep != "world-a:9090" {
		t.Fatalf("owner registration yanked by rogue deregister: ep=%q", ep)
	}
	// Once the registration lapses, the id is reusable by anyone (old holder is gone).
	if err := d.RegisterShard(ctx, "shard-b", "world-b:9090", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := d.RegisterShard(ctx, "shard-b", "world-b2:9090", DefaultShardLease); err != nil {
		t.Fatalf("lapsed id should be reusable: %v", err)
	}
}

func TestShardRegistrationExpiry(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()
	if err := d.RegisterShard(ctx, "shard-a", "world-a:9090", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if ep, _ := d.EndpointForShard(ctx, "shard-a"); ep != "world-a:9090" {
		t.Fatalf("EndpointForShard = %q, want world-a:9090", ep)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := d.EndpointForShard(ctx, "shard-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after registration lapse: want ErrNotFound, got %v", err)
	}
}

// TestZoneToEndpointTwoHop exercises the full routing the world uses: zone -> shard id
// (lease) -> endpoint (registration).
func TestZoneToEndpointTwoHop(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()
	if err := d.RegisterShard(ctx, "shard-b", "world-b:9090", DefaultShardLease); err != nil {
		t.Fatal(err)
	}
	if err := d.RegisterZone(ctx, "darkwood", "shard-b"); err != nil {
		t.Fatal(err)
	}
	shardID, err := d.ShardForZone(ctx, "darkwood")
	if err != nil || shardID != "shard-b" {
		t.Fatalf("ShardForZone = %q, %v; want shard-b", shardID, err)
	}
	ep, err := d.EndpointForShard(ctx, shardID)
	if err != nil || ep != "world-b:9090" {
		t.Fatalf("EndpointForShard = %q, %v; want world-b:9090", ep, err)
	}
}

func TestPlayerPlacementEpochMonotonic(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.PlayerPlacement(ctx, "Bilbo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown player: want ErrNotFound, got %v", err)
	}

	// First placement applies.
	ok, err := d.SetPlayerShard(ctx, "Bilbo", "world-a:9090", "midgaard", 1)
	if err != nil || !ok {
		t.Fatalf("first placement: ok=%v err=%v", ok, err)
	}

	// An equal or older epoch must be rejected (stale/duplicate handoff).
	ok, err = d.SetPlayerShard(ctx, "Bilbo", "world-b:9090", "darkwood", 1)
	if err != nil || ok {
		t.Fatalf("equal epoch should be rejected: ok=%v err=%v", ok, err)
	}
	if p, _ := d.PlayerPlacement(ctx, "Bilbo"); p.ShardID != "world-a:9090" || p.Epoch != 1 {
		t.Fatalf("placement rolled back by stale write: %+v", p)
	}

	// A strictly newer epoch wins.
	ok, err = d.SetPlayerShard(ctx, "Bilbo", "world-b:9090", "darkwood", 2)
	if err != nil || !ok {
		t.Fatalf("newer epoch: ok=%v err=%v", ok, err)
	}
	if p, _ := d.PlayerPlacement(ctx, "Bilbo"); p.ShardID != "world-b:9090" || p.Epoch != 2 {
		t.Fatalf("newer epoch should win: %+v", p)
	}

	// Clearing removes the placement.
	if err := d.ClearPlayer(ctx, "Bilbo"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.PlayerPlacement(ctx, "Bilbo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after clear: want ErrNotFound, got %v", err)
	}
}
