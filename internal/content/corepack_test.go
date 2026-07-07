package content

import (
	"context"
	"testing"
)

// memSource is a trivial in-test Source: it returns the packs it holds for the enabled names.
type memSource map[string]Pack

func (m memSource) LoadPacks(_ context.Context, enabled []string) ([]Pack, error) {
	var out []Pack
	for _, n := range enabled {
		if p, ok := m[n]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// TestLoadWithCore_AlwaysPresent: even a nil delegate (no Postgres) yields the bootstrap zone.
func TestLoadWithCore_AlwaysPresent(t *testing.T) {
	lc, err := LoadWithCore(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("LoadWithCore(nil): %v", err)
	}
	if lc.Empty() {
		t.Fatal("core-only boot must not be empty")
	}
	if z := lc.Zone(CoreZone); z == nil {
		t.Fatalf("core zone %q absent; zones=%d", CoreZone, len(lc.Zones))
	} else if z.StartRoom != CoreStartRoom {
		t.Fatalf("core start room = %q, want %q", z.StartRoom, CoreStartRoom)
	}
	// The scaffold attributes/resources the lobby needs to be coherent.
	if !hasAttr(lc, "max_hp") || !hasAttr(lc, "level") {
		t.Fatal("core pack missing its attribute scaffold")
	}
	if !hasResource(lc, "hp") {
		t.Fatal("core pack missing its hp vital")
	}
}

// TestLoadWithCore_RealOverridesCore: a real pack shipping the same ref wins the merge, and the
// core zone stays present alongside real zones.
func TestLoadWithCore_RealOverridesCore(t *testing.T) {
	realSrc := memSource{"demo": {
		Pack: "demo",
		Zones: []ZoneDTO{{
			Ref: "midgaard", Name: "Midgaard", StartRoom: "midgaard:room:market",
			Rooms: []RoomDTO{{Ref: "midgaard:room:market", Name: "The Market"}},
		}},
		// Override the core-shipped max_hp with a different definition to prove last-write-wins.
		Attributes: []AttributeDTO{{
			Ref: "max_hp", DisplayName: "Overridden Max HP", ValueKind: "int",
			DefaultBase: BaseSpecDTO{Lit: floatptr(999)},
		}},
	}}
	lc, err := LoadWithCore(context.Background(), realSrc, []string{"demo"})
	if err != nil {
		t.Fatalf("LoadWithCore: %v", err)
	}
	// Both the bootstrap zone and the real zone are present.
	if lc.Zone(CoreZone) == nil {
		t.Fatal("core zone dropped when real content present")
	}
	if lc.Zone("midgaard") == nil {
		t.Fatal("real zone absent")
	}
	// The real pack's max_hp (loaded AFTER core) won the merge.
	for _, a := range lc.Attributes {
		if a.Ref == "max_hp" {
			if a.DisplayName != "Overridden Max HP" {
				t.Fatalf("max_hp not overridden by real pack: got %q", a.DisplayName)
			}
			return
		}
	}
	t.Fatal("max_hp absent after merge")
}

// TestLoadWithCore_DelegateNotAskedForCore: core is embedded-only; the delegate must not be
// asked for it (else a delegate that happened to hold a "core" row would double-load).
func TestLoadWithCore_DelegateNotAskedForCore(t *testing.T) {
	spy := &spySource{}
	if _, err := LoadWithCore(context.Background(), spy, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range spy.asked {
		if n == CorePack {
			t.Fatalf("delegate was asked for %q; core must be embedded-only. asked=%v", CorePack, spy.asked)
		}
	}
}

type spySource struct{ asked []string }

func (s *spySource) LoadPacks(_ context.Context, enabled []string) ([]Pack, error) {
	s.asked = append(s.asked, enabled...)
	return nil, nil
}

// TestLintReservedCoreRefs: a real pack shipping a core: ref is flagged; the core pack's own
// refs and non-prefixed def overrides are not.
func TestLintReservedCoreRefs(t *testing.T) {
	packs := []Pack{
		// The embedded core pack: its own core: refs are exempt.
		{Pack: CorePack, Zones: []ZoneDTO{{Ref: CoreZone, Rooms: []RoomDTO{{Ref: CoreStartRoom}}}}},
		// A real pack: a core: zone and a core: room are violations; a normal ref and a max_hp
		// override (not core-prefixed) are fine.
		{Pack: "demo", Zones: []ZoneDTO{
			{Ref: "core", Rooms: []RoomDTO{{Ref: "core:room"}, {Ref: "midgaard:room:ok"}}},
		}, Attributes: []AttributeDTO{{Ref: "max_hp"}}},
	}
	got := LintReservedCoreRefs(packs)
	if len(got) != 2 {
		t.Fatalf("want 2 violations, got %d: %+v", len(got), got)
	}
	for _, v := range got {
		if v.Pack != "demo" {
			t.Errorf("violation attributed to wrong pack: %+v", v)
		}
		if v.Ref != CoreZone && !hasPrefix(v.Ref, CoreRefPrefix) {
			t.Errorf("non-core ref flagged: %+v", v)
		}
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func hasAttr(lc *LoadedContent, ref string) bool {
	for _, a := range lc.Attributes {
		if a.Ref == ref {
			return true
		}
	}
	return false
}

func hasResource(lc *LoadedContent, ref string) bool {
	for _, r := range lc.Resources {
		if r.Ref == ref {
			return true
		}
	}
	return false
}

func floatptr(f float64) *float64 { return &f }
