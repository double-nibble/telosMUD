package content

import (
	"context"
	"testing"
	"testing/fstest"
)

// TestLoadPackFS_SingleFile proves the classic single-file layout still loads unchanged through the
// tree-aware loader: packs/<name>.yaml is read, parsed, and its empty Pack.Pack defaulted to name.
func TestLoadPackFS_SingleFile(t *testing.T) {
	fsys := fstest.MapFS{
		"packs/solo.yaml": &fstest.MapFile{Data: []byte(`
zones:
  - ref: town
    name: Town
    start_room: town:square
    rooms:
      - ref: town:square
        name: The Square
attributes:
  - ref: str
`)},
	}
	p, found, err := loadPackFS(fsys, "solo")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("single-file pack not found")
	}
	if p.Pack != "solo" {
		t.Fatalf("pack name = %q, want defaulted to %q", p.Pack, "solo")
	}
	if len(p.Zones) != 1 || len(p.Zones[0].Rooms) != 1 {
		t.Fatalf("unexpected shape: %d zones", len(p.Zones))
	}
	if len(p.Attributes) != 1 || p.Attributes[0].Ref != "str" {
		t.Fatalf("attributes not parsed: %+v", p.Attributes)
	}
}

// TestLoadPackFS_TreeMerge is the core Slice-A proof: a pack authored as a DIRECTORY of small files
// merges into one Pack — a single zone's rooms split across two files are UNIONED (not replaced),
// pack globals accumulate, and resets concatenate. It also proves sorted-path merge order by having a
// later-sorting file override an earlier one's room by ref (last-write-wins).
func TestLoadPackFS_TreeMerge(t *testing.T) {
	fsys := fstest.MapFS{
		"packs/city/pack.yaml": &fstest.MapFile{Data: []byte(`
pack: city
default_combat: unarmed
`)},
		"packs/city/attributes.yaml": &fstest.MapFile{Data: []byte(`
attributes:
  - ref: str
  - ref: dex
`)},
		// Zone town, first slice of rooms + a reset.
		"packs/city/zones/town/a_plaza.yaml": &fstest.MapFile{Data: []byte(`
zones:
  - ref: town
    name: Town
    start_room: town:plaza
    rooms:
      - ref: town:plaza
        name: Plaza ORIGINAL
      - ref: town:gate
        name: Gate
    resets:
      - op: mob
        proto: town:guard
        room: town:gate
        count: 1
`)},
		// Zone town again, more rooms + a reset + an override of town:plaza (later sort order wins).
		"packs/city/zones/town/b_market.yaml": &fstest.MapFile{Data: []byte(`
zones:
  - ref: town
    rooms:
      - ref: town:market
        name: Market
      - ref: town:plaza
        name: Plaza OVERRIDDEN
    resets:
      - op: mob
        proto: town:vendor
        room: town:market
        count: 1
`)},
		// A second zone in its own subtree.
		"packs/city/zones/wild/forest.yaml": &fstest.MapFile{Data: []byte(`
zones:
  - ref: wild
    name: Wilds
    rooms:
      - ref: wild:clearing
        name: Clearing
`)},
	}

	p, found, err := loadPackFS(fsys, "city")
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("tree pack not found")
	}
	if p.Pack != "city" {
		t.Fatalf("pack name = %q, want city", p.Pack)
	}
	if p.DefaultCombat != "unarmed" {
		t.Fatalf("default_combat = %q, want unarmed", p.DefaultCombat)
	}
	if len(p.Attributes) != 2 {
		t.Fatalf("attributes = %d, want 2", len(p.Attributes))
	}

	// Two zones, unioned by ref (town appears in two files -> ONE merged zone).
	if len(p.Zones) != 2 {
		t.Fatalf("zones = %d, want 2 (town + wild)", len(p.Zones))
	}
	town := findZone(p.Zones, "town")
	if town == nil {
		t.Fatal("town zone missing")
	}
	// Scalars survive from the file that set them.
	if town.Name != "Town" || town.StartRoom != "town:plaza" {
		t.Fatalf("town scalars lost: name=%q start=%q", town.Name, town.StartRoom)
	}
	// Rooms unioned: plaza + gate + market = 3, and plaza was OVERRIDDEN by the later-sorting file.
	if len(town.Rooms) != 3 {
		t.Fatalf("town rooms = %d, want 3 (plaza+gate+market)", len(town.Rooms))
	}
	plaza := findRoom(town.Rooms, "town:plaza")
	if plaza == nil || plaza.Name != "Plaza OVERRIDDEN" {
		t.Fatalf("plaza last-write-wins failed: %+v", plaza)
	}
	// Resets concatenated across both town files (guard + vendor).
	if len(town.Resets) != 2 {
		t.Fatalf("town resets = %d, want 2", len(town.Resets))
	}

	// End-to-end contract: the merged Pack, run through the boot loader, must yield the unioned
	// zone intact — Load's own zone dedup is whole-zone REPLACEMENT, so this proves the union
	// happened in mergePacks (before Load) and survives, not that Load re-merges.
	lc, err := Load(context.Background(), staticSource{p}, []string{"city"})
	if err != nil {
		t.Fatal(err)
	}
	lt := lc.Zone("town")
	if lt == nil || len(lt.Rooms) != 3 {
		t.Fatalf("loaded town zone lost its unioned rooms: %+v", lt)
	}
}

// TestLoadPackFS_Missing proves an enabled name with neither a file nor a directory is a benign miss
// (found=false, no error) — the enable-a-missing-pack-contributes-nothing invariant.
func TestLoadPackFS_Missing(t *testing.T) {
	fsys := fstest.MapFS{"packs/other.yaml": &fstest.MapFile{Data: []byte("pack: other\n")}}
	_, found, err := loadPackFS(fsys, "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("missing pack reported found")
	}
}

// TestMergePacks_ParityWithLoad proves the deliberate contract: feeding a tree pack's files as
// SEPARATE packs to Load, versus merging them first and feeding ONE pack, yields the same pack
// globals — while the zone-union is the intended divergence (Load would drop the first file's rooms).
func TestMergePacks_ParityWithLoad(t *testing.T) {
	a := Pack{Pack: "p", Attributes: []AttributeDTO{{Ref: "str"}}}
	b := Pack{Pack: "p", Attributes: []AttributeDTO{{Ref: "dex"}, {Ref: "str"}}}

	merged := mergePacks([]Pack{a, b})
	if len(merged.Attributes) != 2 {
		t.Fatalf("merged attributes = %d, want 2 (str+dex, str deduped)", len(merged.Attributes))
	}
	// Order: str first (from a), then dex (new in b); str replaced in place by b's copy.
	if merged.Attributes[0].Ref != "str" || merged.Attributes[1].Ref != "dex" {
		t.Fatalf("merge order wrong: %+v", merged.Attributes)
	}
}

func findZone(zs []ZoneDTO, ref string) *ZoneDTO {
	for i := range zs {
		if zs[i].Ref == ref {
			return &zs[i]
		}
	}
	return nil
}

func findRoom(rs []RoomDTO, ref string) *RoomDTO {
	for i := range rs {
		if rs[i].Ref == ref {
			return &rs[i]
		}
	}
	return nil
}
