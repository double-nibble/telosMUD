package world

import (
	"testing"
)

// attributes_test.go exercises attr() through the full modifier stack, the memoized cache + dirty
// invalidation, and the resource-current-clamped-to-derived-max model on a real entity.

// fixedMods is a test modSource: a fixed flat/mul contribution per attribute. It stands in for the
// 5.2 Affected source so the modifier-stack PLUMBING is exercised before affects exist.
type fixedMods struct {
	flat map[string]float64
	mul  map[string]float64
}

func (m fixedMods) flatMod(ref string) float64 {
	if m.flat == nil {
		return 0
	}
	return m.flat[ref]
}

func (m fixedMods) mulMod(ref string) float64 {
	if m.mul == nil {
		return 1
	}
	if v, ok := m.mul[ref]; ok {
		return v
	}
	return 1
}

// testEntityWithDefs builds a bare zone, registers the given attribute/resource defs into its
// private bundle, and returns a living entity in it. modSources is overridden via the package hook.
func testEntityWithDefs(t *testing.T, attrs []*attributeDef, res []*resourceDef) (*Zone, *Entity) {
	t.Helper()
	z := newZone("test")
	for _, a := range attrs {
		z.defs.attr.register(a.ref, a)
	}
	for _, r := range res {
		z.defs.res.register(r.ref, r)
	}
	e := z.newEntity("tester")
	Add(e, &Living{})
	return z, e
}

func fptr(f float64) *float64 { return &f }

// installTestMods replaces an entity's modifier sources with a single fixed source (test helper).
func installTestMods(e *Entity, src fixedMods) {
	e.living.modSrcs = []modSource{src}
}

func TestAttrFullStack(t *testing.T) {
	// strength: literal base 10, clamped [0,20].
	str := &attributeDef{ref: "strength", base: litNode{v: 10}, min: fptr(0), max: fptr(20)}
	_, e := testEntityWithDefs(t, []*attributeDef{str}, nil)

	// Base only.
	if got := attr(e, "strength"); got != 10 {
		t.Fatalf("base strength = %v, want 10", got)
	}

	// Inject a +5 flat and a x2 multiplier via a test mod source. (10+5)*2 = 30, clamped to 20.
	installTestMods(e, fixedMods{flat: map[string]float64{"strength": 5}, mul: map[string]float64{"strength": 2}})
	markAttrsDirty(e)
	if got := attr(e, "strength"); got != 20 {
		t.Fatalf("modded+clamped strength = %v, want 20 (clamp)", got)
	}

	// Drop the multiplier: (10+5)*1 = 15, within clamp.
	installTestMods(e, fixedMods{flat: map[string]float64{"strength": 5}})
	markAttrsDirty(e)
	if got := attr(e, "strength"); got != 15 {
		t.Fatalf("flat-only strength = %v, want 15", got)
	}
}

// TestAddModSourcePlumbing exercises the 5.2 seam: addModSource registers a source AND dirties the
// cache, so its contribution lands on the next attr() with no manual markAttrsDirty.
func TestAddModSourcePlumbing(t *testing.T) {
	str := &attributeDef{ref: "strength", base: litNode{v: 10}}
	_, e := testEntityWithDefs(t, []*attributeDef{str}, nil)
	if got := attr(e, "strength"); got != 10 {
		t.Fatalf("base strength = %v, want 10", got)
	}
	// Two sources sum their flat mods and multiply their mul mods: (10 + 3 + 2) * 2 = 30.
	addModSource(e, fixedMods{flat: map[string]float64{"strength": 3}})
	addModSource(e, fixedMods{flat: map[string]float64{"strength": 2}, mul: map[string]float64{"strength": 2}})
	if got := attr(e, "strength"); got != 30 {
		t.Fatalf("two-source strength = %v, want 30", got)
	}
}

func TestAttrCacheAndDirtyInvalidation(t *testing.T) {
	str := &attributeDef{ref: "strength", base: litNode{v: 10}}
	_, e := testEntityWithDefs(t, []*attributeDef{str}, nil)

	// Prime the cache.
	if got := attr(e, "strength"); got != 10 {
		t.Fatalf("strength = %v, want 10", got)
	}
	// Change the base override WITHOUT dirtying: a correct setAttrBase dirties for us, so this
	// asserts the cache is actually consulted (a stale read would still be 10 if we bypass dirty).
	e.living.attrBase = map[string]float64{"strength": 99}
	if got := attr(e, "strength"); got != 10 {
		t.Fatalf("cached strength = %v, want stale 10 (cache not invalidated yet)", got)
	}
	// Now mark dirty: the next read recomputes from the new base.
	markAttrsDirty(e)
	if got := attr(e, "strength"); got != 99 {
		t.Fatalf("post-dirty strength = %v, want 99", got)
	}
	// setAttrBase dirties on its own.
	setAttrBase(e, "strength", 7)
	if got := attr(e, "strength"); got != 7 {
		t.Fatalf("setAttrBase strength = %v, want 7", got)
	}
}

func TestDerivedOfDerived(t *testing.T) {
	// con=12, level=3, max_hp = con*10 + level*5 = 135.
	con := &attributeDef{ref: "constitution", base: litNode{v: 12}}
	lvl := &attributeDef{ref: "level", base: litNode{v: 3}}
	maxHP := &attributeDef{ref: "max_hp", valueKind: "derived", base: mustParse(t, []any{
		"+",
		[]any{"*", []any{"attr", "constitution"}, 10.0},
		[]any{"*", []any{"attr", "level"}, 5.0},
	})}
	_, e := testEntityWithDefs(t, []*attributeDef{con, lvl, maxHP}, nil)

	if got := attr(e, "max_hp"); got != 135 {
		t.Fatalf("max_hp = %v, want 135", got)
	}
	// Raising the constitution base flows through to max_hp (derived re-resolves).
	setAttrBase(e, "constitution", 20)
	if got := attr(e, "max_hp"); got != 215 { // 20*10 + 3*5
		t.Fatalf("max_hp after con raise = %v, want 215", got)
	}
}

func TestResourceClampedToDerivedMax(t *testing.T) {
	con := &attributeDef{ref: "constitution", base: litNode{v: 10}}
	maxHP := &attributeDef{
		ref: "max_hp", valueKind: "derived",
		base: mustParse(t, []any{"*", []any{"attr", "constitution"}, 10.0}),
	} // 100
	hp := &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true}
	_, e := testEntityWithDefs(t, []*attributeDef{con, maxHP}, []*resourceDef{hp})

	// No current stored yet: reads as full (the derived max).
	if got := resourceCurrent(e, "hp"); got != 100 {
		t.Fatalf("fresh hp = %v, want 100 (full)", got)
	}
	if got := resourceMax(e, "hp"); got != 100 {
		t.Fatalf("max hp = %v, want 100", got)
	}
	// Set current above max: clamps to max on store.
	setResourceCurrent(e, "hp", 250)
	if got := resourceCurrent(e, "hp"); got != 100 {
		t.Fatalf("over-cap hp = %v, want clamped 100", got)
	}
	// Set a sane current, then LOWER the max (con base drop): the read re-clamps to the new max.
	setResourceCurrent(e, "hp", 80)
	setAttrBase(e, "constitution", 5) // max_hp now 50
	if got := resourceCurrent(e, "hp"); got != 50 {
		t.Fatalf("hp after max lowered = %v, want re-clamped 50", got)
	}
	// RAISE the max back: the stored value (clamped to 50 on the read above is NOT re-stored), so
	// the current stays where it was stored (80) and now reads under the higher cap.
	setAttrBase(e, "constitution", 10) // max_hp 100 again
	if got := resourceCurrent(e, "hp"); got != 80 {
		t.Fatalf("hp after max restored = %v, want 80 (stored value, under cap)", got)
	}
}

func mustParse(t *testing.T, v any) formulaNode {
	t.Helper()
	n, err := parseFormula(v)
	if err != nil {
		t.Fatalf("parseFormula: %v", err)
	}
	return n
}
