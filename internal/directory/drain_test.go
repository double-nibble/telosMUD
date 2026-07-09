package directory

import (
	"context"
	"testing"
	"time"
)

// TestReserveDrainTargetCountingAndCeiling pins the #41 counting reservation: reservations SUM per target,
// a reserve that would exceed the caller's headroom is refused, and a release frees that drainer's hold.
func TestReserveDrainTargetCountingAndCeiling(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()
	const target, ttl = "shard-b", 30 * time.Second

	// First drainer reserves 60 of a 100 headroom: admitted.
	ok, err := d.ReserveDrainTarget(ctx, target, "shard-a", 100, 60, ttl)
	if err != nil || !ok {
		t.Fatalf("first reserve = %v, %v; want true", ok, err)
	}
	// Second drainer wants 60 more, but 60+60 > 100: refused (would overload the target).
	ok, err = d.ReserveDrainTarget(ctx, target, "shard-c", 100, 60, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second reserve admitted past the ceiling; want refused (60+60 > 100)")
	}
	// A smaller reserve that fits the remaining headroom is admitted (40 fits under 100).
	ok, err = d.ReserveDrainTarget(ctx, target, "shard-c", 100, 40, ttl)
	if err != nil || !ok {
		t.Fatalf("fitting reserve = %v, %v; want true (60+40 == 100)", ok, err)
	}
	// Release the first drainer's 60; now a fresh 60 fits again (40 + 60 == 100).
	if err := d.ReleaseDrainTarget(ctx, target, "shard-a"); err != nil {
		t.Fatal(err)
	}
	ok, err = d.ReserveDrainTarget(ctx, target, "shard-d", 100, 60, ttl)
	if err != nil || !ok {
		t.Fatalf("post-release reserve = %v, %v; want true (40 + 60 == 100)", ok, err)
	}
}

// TestReserveDrainTargetNonPositiveHeadroom: a target already at/over its ceiling (headroom <= 0) refuses
// any reservation — the caller then re-selects or proceeds over the soft ceiling.
func TestReserveDrainTargetNonPositiveHeadroom(t *testing.T) {
	d := newTestRedis(t)
	ok, err := d.ReserveDrainTarget(context.Background(), "shard-b", "shard-a", 0, 1, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("reserve admitted with zero headroom; want refused")
	}
}

// TestDrainingMarker pins SetDraining/ListDraining/ClearDraining — the drain-target selector uses it to
// exclude a peer that is itself draining.
func TestDrainingMarker(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if set, err := d.ListDraining(ctx); err != nil || len(set) != 0 {
		t.Fatalf("ListDraining on empty = %v, %v; want {}", set, err)
	}
	if err := d.SetDraining(ctx, "shard-a", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.SetDraining(ctx, "shard-b", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	set, err := d.ListDraining(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !set["shard-a"] || !set["shard-b"] || len(set) != 2 {
		t.Fatalf("ListDraining = %v; want {shard-a, shard-b}", set)
	}
	if err := d.ClearDraining(ctx, "shard-a"); err != nil {
		t.Fatal(err)
	}
	set, _ = d.ListDraining(ctx)
	if set["shard-a"] || !set["shard-b"] || len(set) != 1 {
		t.Fatalf("after clear: ListDraining = %v; want {shard-b}", set)
	}
}
