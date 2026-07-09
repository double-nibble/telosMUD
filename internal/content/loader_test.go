package content

import (
	"context"
	"strings"
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
	// midgaard expanded to 4 rooms (temple/market/guildhall/smithy) and 14 item prototypes
	// (the original torch/helmet/sword/chest + the smithy gear: warhammer/frostbrand/vest/
	// gloves/boots/shield + the #35 leather belt in the content-defined "waist" slot + the #148
	// guaranteed bind-on-pickup hex-charm loot fixture). The crypt zone proves multi-zone-per-shard.
	if len(mid.Rooms) != 4 || len(mid.Items) != 14 {
		t.Fatalf("midgaard rooms=%d items=%d, want 4/14", len(mid.Rooms), len(mid.Items))
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
	if len(lc.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2 (hero_advancement + leatherworking_skill)", len(lc.Tracks))
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

	// Bundles (Phase 11.4b): classes fighter+mage, races elf+dwarf, the leatherworking profession, and the
	// uncapped foraging gathering profession (#55) — each a kind + a grant op-list. Pack globals, loaded onto
	// lc.Bundles. (mage+dwarf were added in 14.8 to give the chargen picker a real choice.)
	if len(lc.Bundles) != 6 {
		t.Fatalf("bundles = %d, want 6 (fighter + mage + elf + dwarf + leatherworking + foraging)", len(lc.Bundles))
	}
	var fighter, foraging *BundleDTO
	for i := range lc.Bundles {
		if lc.Bundles[i].Ref == "fighter" {
			fighter = &lc.Bundles[i]
		}
		if lc.Bundles[i].Ref == "foraging" {
			foraging = &lc.Bundles[i]
		}
	}
	// #55: foraging is the uncapped gathering profession — pin the uncapped flag so a false default regression
	// (or a store round-trip that drops it) is caught here, not only in the gated Postgres test.
	if foraging == nil || foraging.Kind != "profession" || !foraging.Uncapped {
		t.Fatalf("foraging must be an uncapped profession bundle, got %+v", foraging)
	}
	if fighter == nil || fighter.Kind != "class" || fighter.Grants == nil {
		t.Fatalf("fighter bundle = %+v, want kind=class with grants", fighter)
	}

	// Chargen (Phase 14.8): the demo ships one flow (race pick → class pick → 27-pt point-buy).
	if len(lc.Chargens) != 1 {
		t.Fatalf("chargens = %d, want 1 (demo:chargen)", len(lc.Chargens))
	}
	cg := lc.Chargens[0]
	if cg.Ref != "demo:chargen" || len(cg.Steps) != 3 {
		t.Fatalf("demo chargen = %+v, want ref demo:chargen with 3 steps", cg)
	}
	if cg.Steps[0].Kind != "bundle_choice" || cg.Steps[0].BundleKind != "race" {
		t.Fatalf("chargen step 0 = %+v, want bundle_choice/race", cg.Steps[0])
	}
	if pb := cg.Steps[2]; pb.Kind != "point_buy" || pb.Points != 27 || len(pb.Attributes) != 3 {
		t.Fatalf("chargen step 2 = %+v, want point_buy 27pts over 3 attrs", pb)
	}

	// Loot (Phase 12.1): the demo ships 3 rarity tiers + a goblin_loot table (a guaranteed torch, a #148
	// guaranteed bind-on-pickup hex-charm, and a 25% chance sword). Pack globals, loaded onto
	// lc.RarityTiers / lc.LootTables.
	if len(lc.RarityTiers) != 4 {
		t.Fatalf("rarity_tiers = %d, want 4 (common/uncommon/rare/epic)", len(lc.RarityTiers))
	}
	if len(lc.LootTables) != 2 {
		t.Fatalf("loot_tables = %d, want 2 (goblin_loot + disenchant_arms)", len(lc.LootTables))
	}
	gl := lc.LootTables[0]
	if gl.Ref != "goblin_loot" || len(gl.Rolls) != 3 {
		t.Fatalf("goblin_loot = %+v, want ref goblin_loot with 3 rolls", gl)
	}

	// Spawn schedules (Phase 12.4): the demo ships one weekly boss schedule.
	if len(lc.SpawnSchedules) != 1 {
		t.Fatalf("spawn_schedules = %d, want 1 (boss:warden)", len(lc.SpawnSchedules))
	}
	ws := lc.SpawnSchedules[0]
	if ws.Ref != "boss:warden" || ws.Zone != "darkwood" || ws.IntervalAfterDeathSec != 604800 {
		t.Fatalf("boss:warden schedule = %+v, want darkwood weekly", ws)
	}

	// Recipes (Phase 13.5): the demo ships one leatherworking recipe crafted at the forge station.
	if len(lc.Recipes) != 1 {
		t.Fatalf("recipes = %d, want 1 (craft:leather_vest)", len(lc.Recipes))
	}
	rc := lc.Recipes[0]
	if rc.Ref != "craft:leather_vest" || rc.Profession != "leatherworking" || rc.Station != "forge" {
		t.Fatalf("recipe[0] = %+v, want craft:leather_vest/leatherworking/forge", rc)
	}
	if len(rc.Inputs) != 2 || rc.Output.Item != "midgaard:obj:leather-vest" || rc.Output.Bind != "bound" {
		t.Fatalf("recipe inputs/output = %+v / %+v, want 2 inputs -> bound leather-vest", rc.Inputs, rc.Output)
	}

	// Display templates: the demo ships a `score` sheet template (the display-templating feature's first consumer).
	var score *DisplayDefDTO
	for i := range lc.DisplayDefs {
		if lc.DisplayDefs[i].Surface == "score" {
			score = &lc.DisplayDefs[i]
		}
	}
	if score == nil {
		t.Fatalf("display_defs = %+v, want a score template", lc.DisplayDefs)
	}
	if !strings.Contains(score.Render, "ui.sheet()") || !strings.Contains(score.Render, "self:name()") {
		t.Fatalf("score template body does not build a ui.sheet with self: %q", score.Render)
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
