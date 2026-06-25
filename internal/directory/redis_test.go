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
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedis(rdb, "test")
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

func TestPlayerPlacementEpochMonotonic(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, err := d.PlayerPlacement(ctx, "Bilbo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown player: want ErrNotFound, got %v", err)
	}

	// First placement applies.
	ok, err := d.SetPlayerShard(ctx, "Bilbo", "world-a:9090", 1)
	if err != nil || !ok {
		t.Fatalf("first placement: ok=%v err=%v", ok, err)
	}

	// An equal or older epoch must be rejected (stale/duplicate handoff).
	ok, err = d.SetPlayerShard(ctx, "Bilbo", "world-b:9090", 1)
	if err != nil || ok {
		t.Fatalf("equal epoch should be rejected: ok=%v err=%v", ok, err)
	}
	if p, _ := d.PlayerPlacement(ctx, "Bilbo"); p.ShardAddr != "world-a:9090" || p.Epoch != 1 {
		t.Fatalf("placement rolled back by stale write: %+v", p)
	}

	// A strictly newer epoch wins.
	ok, err = d.SetPlayerShard(ctx, "Bilbo", "world-b:9090", 2)
	if err != nil || !ok {
		t.Fatalf("newer epoch: ok=%v err=%v", ok, err)
	}
	if p, _ := d.PlayerPlacement(ctx, "Bilbo"); p.ShardAddr != "world-b:9090" || p.Epoch != 2 {
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
