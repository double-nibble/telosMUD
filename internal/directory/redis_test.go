package directory

import (
	"context"
	"errors"
	"testing"

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
