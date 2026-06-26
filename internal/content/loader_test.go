package content

import (
	"context"
	"testing"
)

// TestEmbeddedDemoPackLoads proves the embedded demo YAML parses into the expected zones,
// rooms, prototypes, and resets — the source the unit tests and a bare dev run rely on (no
// Postgres). The byte-for-byte prototype parity lives in the world package
// (content_parity_test.go); this asserts the structural shape and the folded-scalar long
// descriptions the YAML must preserve.
func TestEmbeddedDemoPackLoads(t *testing.T) {
	lc, err := LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	if lc.Empty() {
		t.Fatal("demo pack loaded empty")
	}
	if len(lc.Zones) != 2 {
		t.Fatalf("zones = %d, want 2 (midgaard + darkwood)", len(lc.Zones))
	}

	mid := lc.Zone("midgaard")
	if mid == nil {
		t.Fatal("midgaard zone missing")
	}
	if mid.StartRoom != "midgaard:room:temple" {
		t.Fatalf("midgaard start_room = %q", mid.StartRoom)
	}
	if len(mid.Rooms) != 2 || len(mid.Items) != 4 {
		t.Fatalf("midgaard rooms=%d items=%d, want 2/4", len(mid.Rooms), len(mid.Items))
	}

	// The folded YAML scalar for the temple long desc must be a single joined line, no
	// trailing newline (byte-identical to the old Go string concat).
	var temple *RoomDTO
	for i := range mid.Rooms {
		if mid.Rooms[i].Ref == "midgaard:room:temple" {
			temple = &mid.Rooms[i]
		}
	}
	if temple == nil {
		t.Fatal("temple room missing")
	}
	wantLong := "A broad plaza of worn flagstones stretches before the great temple. " +
		"Pilgrims murmur in the shade of its columns."
	if temple.Long != wantLong {
		t.Fatalf("temple long = %q\nwant %q", temple.Long, wantLong)
	}
	if temple.Exits["north"] != "midgaard:room:market" {
		t.Fatalf("temple north exit = %q", temple.Exits["north"])
	}

	// The cross-zone exit market -> darkwood:room:grove is present (both zones ship together).
	var market *RoomDTO
	for i := range mid.Rooms {
		if mid.Rooms[i].Ref == "midgaard:room:market" {
			market = &mid.Rooms[i]
		}
	}
	if market.Exits["north"] != "darkwood:room:grove" {
		t.Fatalf("market cross-zone north exit = %q", market.Exits["north"])
	}

	// Resets: 4 ops (torches count=5, helmet, sword, chest).
	if len(mid.Resets) != 4 {
		t.Fatalf("midgaard resets = %d, want 4", len(mid.Resets))
	}
	if mid.Resets[0].Proto != "midgaard:obj:torch" || mid.Resets[0].Count != 5 {
		t.Fatalf("first reset = %+v, want torch count 5", mid.Resets[0])
	}
}

// TestEmptyLoad proves the bare-engine boot: no source / no enabled packs yields empty,
// error-free content.
func TestEmptyLoad(t *testing.T) {
	lc, err := Load(context.Background(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !lc.Empty() {
		t.Fatal("nil source must load empty content")
	}
	if lc.Zone("midgaard") != nil {
		t.Fatal("empty content must not resolve any zone")
	}

	// An enabled name with no embedded file contributes nothing (still empty, no error).
	lc2, err := Load(context.Background(), EmbeddedSource{}, []string{"nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if !lc2.Empty() {
		t.Fatal("unknown pack name must load empty content")
	}
}
