package world

import (
	"reflect"
	"sort"
	"testing"
)

// Parity guard for the content loader (docs/PHASE4-PLAN.md §7 risk 1 & 2). It asserts the
// prototypes the EMBEDDED demo pack (packs/demo.yaml) produces are byte-identical to what the
// old hand-authored newDemoZone/defineTorch/defineHelmet/defineSword/defineChest produced:
// same keywords, same short/long, same Room exits map, same component types and field values,
// and the same market-floor instance placements. The literals below ARE the old Go authoring,
// transcribed verbatim; if the YAML drifts, this fails before any look/move test does.
//
// This is the keystone-risk mitigation: the loader must replace newDemoZone without shifting
// one byte of the demo world. Kept permanently as a regression guard.

// wantProto is the expected immutable template for one prototype.
type wantProto struct {
	keywords []string
	short    string
	long     string
	exits    map[string]ProtoRef // nil for non-room prototypes
	comps    []reflect.Type      // component types present, sorted by name
}

func TestDemoPackPrototypeParity(t *testing.T) {
	// Build the full demo world (both zones) into one shared cache, exactly as a real shard.
	protos := newProtoCache()
	_ = newDemoZone("midgaard", protos)
	_ = newDemoZone("darkwood", protos)

	roomT := reflect.TypeFor[*Room]()
	physT := reflect.TypeFor[*Physical]()
	wearT := reflect.TypeFor[*Wearable]()
	wpnT := reflect.TypeFor[*Weapon]()
	contT := reflect.TypeFor[*Container]()

	want := map[ProtoRef]wantProto{
		"midgaard:room:temple": {
			short: "The Temple Square",
			long: "A broad plaza of worn flagstones stretches before the great temple. " +
				"Pilgrims murmur in the shade of its columns.",
			exits: map[string]ProtoRef{"north": "midgaard:room:market"},
			comps: []reflect.Type{roomT},
		},
		"midgaard:room:market": {
			short: "Market Square",
			long:  "Stalls crowd the square and merchants cry their wares over the din of haggling.",
			exits: map[string]ProtoRef{"south": "midgaard:room:temple", "north": "darkwood:room:grove"},
			comps: []reflect.Type{roomT},
		},
		"darkwood:room:grove": {
			short: "A Moonlit Grove",
			long:  "Silver birches ring a still clearing; the air hums with quiet magic.",
			exits: map[string]ProtoRef{"south": "midgaard:room:market", "north": "darkwood:room:hollow"},
			comps: []reflect.Type{roomT},
		},
		"darkwood:room:hollow": {
			short: "A Dark Hollow",
			long:  "The trees crowd close and the moonlight fails. Something rustles, unseen.",
			exits: map[string]ProtoRef{"south": "darkwood:room:grove"},
			comps: []reflect.Type{roomT},
		},
		"midgaard:obj:torch": {
			keywords: []string{"torch", "wooden"},
			short:    "a wooden torch",
			long:     "A wooden torch lies here, its pitch cold.",
			comps:    []reflect.Type{physT},
		},
		"midgaard:obj:helmet": {
			keywords: []string{"helmet", "iron"},
			short:    "an iron helmet",
			long:     "An iron helmet rests here.",
			comps:    []reflect.Type{physT, wearT},
		},
		"midgaard:obj:sword": {
			keywords: []string{"sword", "steel", "long"},
			short:    "a steel longsword",
			long:     "A steel longsword lies here.",
			comps:    []reflect.Type{physT, wearT, wpnT},
		},
		"midgaard:obj:chest": {
			keywords: []string{"chest", "oak", "wooden"},
			short:    "a wooden chest",
			long:     "A heavy wooden chest sits here.",
			comps:    []reflect.Type{physT, contT},
		},
	}

	for ref, w := range want {
		p := protos.get(ref)
		if p == nil {
			t.Errorf("%s: prototype missing from loaded cache", ref)
			continue
		}
		if p.short != w.short {
			t.Errorf("%s: short = %q, want %q", ref, p.short, w.short)
		}
		if p.long != w.long {
			t.Errorf("%s: long = %q, want %q", ref, p.long, w.long)
		}
		if !reflect.DeepEqual(p.keywords, w.keywords) {
			t.Errorf("%s: keywords = %v, want %v", ref, p.keywords, w.keywords)
		}
		// Component type set.
		var gotComps []reflect.Type
		for ct := range p.comps {
			gotComps = append(gotComps, ct)
		}
		sortTypes(gotComps)
		sortTypes(w.comps)
		if !reflect.DeepEqual(gotComps, w.comps) {
			t.Errorf("%s: component types = %v, want %v", ref, gotComps, w.comps)
		}
		// Room exits map.
		if w.exits != nil {
			rc, ok := p.comps[roomT].(*Room)
			if !ok {
				t.Errorf("%s: expected a *Room component", ref)
				continue
			}
			if !reflect.DeepEqual(rc.exits, w.exits) {
				t.Errorf("%s: exits = %v, want %v", ref, rc.exits, w.exits)
			}
		}
	}

	// Component FIELD-VALUE parity for the items (mirrors the old define* field values).
	assertPhysical(t, protos, "midgaard:obj:torch", Physical{weight: 2, material: "wood"})
	assertPhysical(t, protos, "midgaard:obj:helmet", Physical{weight: 3, material: "iron"})
	assertPhysical(t, protos, "midgaard:obj:sword", Physical{weight: 5, material: "steel"})
	assertPhysical(t, protos, "midgaard:obj:chest", Physical{weight: 40, material: "oak"})

	if w := protos.get("midgaard:obj:helmet").comps[wearT].(*Wearable); !w.canWear(WearLocHead) {
		t.Errorf("helmet wearable does not advertise head: %v", w)
	}
	sw := protos.get("midgaard:obj:sword")
	if w := sw.comps[wearT].(*Wearable); !w.canWear(WearLocWield) {
		t.Errorf("sword wearable does not advertise wield: %v", w)
	}
	if wp := sw.comps[wpnT].(*Weapon); *wp != (Weapon{diceNum: 2, diceSize: 6, damageType: "slash", class: "sword", attackVerb: "slash"}) {
		t.Errorf("sword weapon = %+v", wp)
	}
	if c := protos.get("midgaard:obj:chest").comps[contT].(*Container); c.capacity != 10 || !c.closed {
		t.Errorf("chest container = %+v, want capacity 10 closed", c)
	}
}

// TestDemoPackInstancePlacementParity proves the reset script places the same instances on
// the market floor as the old newDemoZone Move(z.spawn(...), marketEntity) calls: 5 torches +
// one each of helmet/sword/chest, and NOTHING on the temple/start room (so targeting-count
// and look-text tests are unchanged).
func TestDemoPackInstancePlacementParity(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	temple := z.rooms["midgaard:room:temple"]
	if len(temple.contents) != 0 {
		t.Fatalf("temple has %d ground items, want 0 (start room stays clean)", len(temple.contents))
	}

	market := z.rooms["midgaard:room:market"]
	counts := map[ProtoRef]int{}
	for _, e := range market.contents {
		counts[e.proto]++
	}
	wantCounts := map[ProtoRef]int{
		"midgaard:obj:torch":  5,
		"midgaard:obj:helmet": 1,
		"midgaard:obj:sword":  1,
		"midgaard:obj:chest":  1,
	}
	if !reflect.DeepEqual(counts, wantCounts) {
		t.Fatalf("market floor instances = %v, want %v", counts, wantCounts)
	}
}

func assertPhysical(t *testing.T, c *protoCache, ref ProtoRef, want Physical) {
	t.Helper()
	p := c.get(ref)
	phys, ok := Get[*Physical](&Entity{comps: p.comps})
	if !ok {
		t.Errorf("%s: missing Physical", ref)
		return
	}
	if *phys != want {
		t.Errorf("%s: physical = %+v, want %+v", ref, *phys, want)
	}
}

func sortTypes(ts []reflect.Type) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].String() < ts[j].String() })
}
