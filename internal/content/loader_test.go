package content

import (
	"context"
	"testing"
)

// staticSource is a test Source that returns a fixed list of packs in order, so a merge test can feed
// two packs and assert the last-write-wins / accumulate semantics without touching the embedded YAML.
type staticSource []Pack

func (s staticSource) LoadPacks(_ context.Context, _ []string) ([]Pack, error) {
	return []Pack(s), nil
}

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
	// The richer demo ships THREE zones: midgaard + darkwood (the original pair) + the crypt
	// (the multi-zone-per-shard expansion). The empty-boot invariant is asserted separately
	// (TestEmptyLoad): an empty pack still yields an empty world.
	if len(lc.Zones) != 3 {
		t.Fatalf("zones = %d, want 3 (midgaard + darkwood + crypt)", len(lc.Zones))
	}

	mid := lc.Zone("midgaard")
	if mid == nil {
		t.Fatal("midgaard zone missing")
	}
	if mid.StartRoom != "midgaard:room:temple" {
		t.Fatalf("midgaard start_room = %q", mid.StartRoom)
	}
	// midgaard expanded to 4 rooms (temple/market/guildhall/smithy) and 10 item prototypes
	// (the original torch/helmet/sword/chest + the smithy gear: warhammer/frostbrand/vest/
	// gloves/boots/shield). The crypt zone proves multi-zone-per-shard.
	if len(mid.Rooms) != 4 || len(mid.Items) != 10 {
		t.Fatalf("midgaard rooms=%d items=%d, want 4/10", len(mid.Rooms), len(mid.Items))
	}
	if crypt := lc.Zone("crypt"); crypt == nil {
		t.Fatal("crypt zone missing (multi-zone-per-shard expansion)")
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

	// Resets: the original 4 market/temple ops (torches count=5, helmet, sword, chest) are kept
	// FIRST and byte-identical (the instance-placement parity test depends on them), followed by
	// the new smithy gear placements + the guild quartermaster — 11 ops total.
	if len(mid.Resets) != 11 {
		t.Fatalf("midgaard resets = %d, want 11", len(mid.Resets))
	}
	if mid.Resets[0].Proto != "midgaard:obj:torch" || mid.Resets[0].Count != 5 {
		t.Fatalf("first reset = %+v, want torch count 5", mid.Resets[0])
	}

	// Regions (Phase 10.3): the demo defines one region "heartlands" grouping midgaard + darkwood; the
	// crypt is region-less. The region is a pack global (not under a zone), so it loads onto lc.Regions.
	if len(lc.Regions) != 1 {
		t.Fatalf("regions = %d, want 1 (heartlands)", len(lc.Regions))
	}
	hl := lc.Regions[0]
	if hl.Ref != "heartlands" || hl.Name != "The Heartlands" {
		t.Fatalf("region[0] = %+v, want heartlands/The Heartlands", hl)
	}
	if len(hl.Zones) != 2 || hl.Zones[0] != "midgaard" || hl.Zones[1] != "darkwood" {
		t.Fatalf("heartlands zones = %v, want [midgaard darkwood]", hl.Zones)
	}

	// Tracks (Phase 11.2): the demo defines one XP→level track "hero_advancement" with 3 thresholds + a
	// grant op-list per step. The track is a pack global (not under a zone), so it loads onto lc.Tracks.
	if len(lc.Tracks) != 1 {
		t.Fatalf("tracks = %d, want 1 (hero_advancement)", len(lc.Tracks))
	}
	tr := lc.Tracks[0]
	if tr.Ref != "hero_advancement" || tr.ProgressAttr != "xp" || tr.LevelAttr != "level" {
		t.Fatalf("track[0] = %+v, want hero_advancement/xp/level", tr)
	}
	if len(tr.Thresholds) != 3 || tr.Thresholds[0] != 100 {
		t.Fatalf("hero_advancement thresholds = %v, want [100 250 500]", tr.Thresholds)
	}
	if len(tr.Steps) != 3 {
		t.Fatalf("hero_advancement steps = %d, want 3 (one grant op-list per threshold)", len(tr.Steps))
	}

	// Bundles (Phase 11.4b): the demo defines a "fighter" class + an "elf" race, each a kind + a grant
	// op-list. Pack globals, loaded onto lc.Bundles.
	if len(lc.Bundles) != 2 {
		t.Fatalf("bundles = %d, want 2 (fighter + elf)", len(lc.Bundles))
	}
	var fighter *BundleDTO
	for i := range lc.Bundles {
		if lc.Bundles[i].Ref == "fighter" {
			fighter = &lc.Bundles[i]
		}
	}
	if fighter == nil || fighter.Kind != "class" || fighter.Grants == nil {
		t.Fatalf("fighter bundle = %+v, want kind=class with grants", fighter)
	}

	// Loot (Phase 12.1): the demo ships 3 rarity tiers + a goblin_loot table (a guaranteed + a chance
	// roll). Pack globals, loaded onto lc.RarityTiers / lc.LootTables.
	if len(lc.RarityTiers) != 3 {
		t.Fatalf("rarity_tiers = %d, want 3 (common/uncommon/rare)", len(lc.RarityTiers))
	}
	if len(lc.LootTables) != 1 {
		t.Fatalf("loot_tables = %d, want 1 (goblin_loot)", len(lc.LootTables))
	}
	gl := lc.LootTables[0]
	if gl.Ref != "goblin_loot" || len(gl.Rolls) != 2 {
		t.Fatalf("goblin_loot = %+v, want ref goblin_loot with 2 rolls", gl)
	}
}

// TestRegionMergeLastWriteWins proves a later pack overrides an earlier region by ref (the same
// last-write-wins rule as the other pack globals) while a distinct ref accumulates — the merge
// semantics the director relies on to resolve a region's final member set.
func TestRegionMergeLastWriteWins(t *testing.T) {
	base := Pack{Pack: "base", Regions: []RegionDTO{
		{Ref: "heartlands", Name: "Heartlands", Zones: []string{"midgaard"}},
		{Ref: "frontier", Name: "Frontier", Zones: []string{"darkwood"}},
	}}
	override := Pack{Pack: "override", Regions: []RegionDTO{
		{Ref: "heartlands", Name: "Greater Heartlands", Zones: []string{"midgaard", "darkwood"}},
	}}
	lc, err := Load(context.Background(), staticSource{base, override}, []string{"base", "override"})
	if err != nil {
		t.Fatal(err)
	}
	if len(lc.Regions) != 2 {
		t.Fatalf("regions = %d, want 2 (heartlands overridden in place + frontier)", len(lc.Regions))
	}
	var hl, fr *RegionDTO
	for i := range lc.Regions {
		switch lc.Regions[i].Ref {
		case "heartlands":
			hl = &lc.Regions[i]
		case "frontier":
			fr = &lc.Regions[i]
		}
	}
	if hl == nil || hl.Name != "Greater Heartlands" || len(hl.Zones) != 2 {
		t.Fatalf("heartlands not overridden by the later pack: %+v", hl)
	}
	if fr == nil || fr.Name != "Frontier" {
		t.Fatalf("frontier (distinct ref) should accumulate: %+v", fr)
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
