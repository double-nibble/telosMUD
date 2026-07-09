package world

import (
	"context"
	"testing"
	"time"
)

// fakeOccPublisher records the per-zone occupancy the shard publishes.
type fakeOccPublisher struct {
	got map[string]int
}

func (f *fakeOccPublisher) SetZoneOccupancy(_ context.Context, zoneID string, players int, _ time.Duration) error {
	f.got[zoneID] = players
	return nil
}

// TestPublishOccupancy pins the #42 signal source: publishOccupancy heartbeats a confirmed-owned zone's live
// player count (its pop mirror) to the wired publisher, and is a safe no-op without one.
func TestPublishOccupancy(t *testing.T) {
	sh := NewDemoShard()
	z := sh.zoneByID("midgaard")
	z.pop.Store(7)

	f := &fakeOccPublisher{got: map[string]int{}}
	sh.WithOccupancyPublisher(f)
	sh.publishOccupancy(context.Background(), "midgaard", time.Second)
	if f.got["midgaard"] != 7 {
		t.Fatalf("published occupancy = %d, want 7", f.got["midgaard"])
	}

	// No publisher wired: a clean no-op, no panic.
	NewDemoShard().publishOccupancy(context.Background(), "midgaard", time.Second)

	// An unknown zone: no-op (nothing to resolve).
	f.got = map[string]int{}
	sh.publishOccupancy(context.Background(), "nonexistent", time.Second)
	if len(f.got) != 0 {
		t.Fatalf("published for an unknown zone: %v", f.got)
	}
}
