package directory

import (
	"context"
	"testing"
	"time"
)

// TestZoneOccupancyRoundTrip pins the #42 occupancy signal: a shard publishes each hosted zone's player
// count, and the coordinator reads the whole map back for weighted placement.
func TestZoneOccupancyRoundTrip(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if occ, err := d.ZoneOccupancies(ctx); err != nil || len(occ) != 0 {
		t.Fatalf("ZoneOccupancies on empty = %v, %v; want {}", occ, err)
	}
	if err := d.SetZoneOccupancy(ctx, "midgaard", 42, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := d.SetZoneOccupancy(ctx, "darkwood", 3, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	occ, err := d.ZoneOccupancies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if occ["midgaard"] != 42 || occ["darkwood"] != 3 || len(occ) != 2 {
		t.Fatalf("ZoneOccupancies = %v; want {midgaard:42, darkwood:3}", occ)
	}

	// A re-publish overwrites (not accumulates).
	if err := d.SetZoneOccupancy(ctx, "midgaard", 40, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	occ, _ = d.ZoneOccupancies(ctx)
	if occ["midgaard"] != 40 {
		t.Fatalf("midgaard occupancy = %d after re-publish; want 40", occ["midgaard"])
	}
}
