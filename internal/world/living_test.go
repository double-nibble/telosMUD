package world

import (
	"testing"
)

// living_test.go asserts the Living accessors return sane values WITH and WITHOUT content, and that
// a demo character resolves the demo pack's content-defined attributes/resources on construction.

// TestLivingAccessorsNoContent: a Living entity in a bare zone (no defs) reports 0/absent for every
// vital — never a panic — the bare-engine invariant behind the accessors.
func TestLivingAccessorsNoContent(t *testing.T) {
	z := newZone("bare") // no global defs registered
	e := z.newEntity("ghost")
	Add(e, &Living{})

	if got := e.HP(); got != 0 {
		t.Fatalf("contentless HP() = %d, want 0", got)
	}
	if got := e.MaxHP(); got != 0 {
		t.Fatalf("contentless MaxHP() = %d, want 0", got)
	}
	if got := e.Attr("strength"); got != 0 {
		t.Fatalf("contentless Attr(strength) = %v, want 0", got)
	}
	if hasResource(e, "hp") {
		t.Fatal("bare zone should not know the hp resource")
	}
}

// TestLivingAccessorsWithDemoContent builds a demo zone (which registers the demo pack's globals)
// and asserts a fresh player resolves max_hp from the formula and reads full hp/mana.
func TestLivingAccessorsWithDemoContent(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	e := z.newEntity("Hero")
	Add(e, &Living{})

	// Demo: strength=10, constitution=10, level=1, intellect=10.
	if got := e.Attr("strength"); got != 10 {
		t.Fatalf("strength = %v, want 10", got)
	}
	if got := e.Attr("level"); got != 1 {
		t.Fatalf("level = %v, want 1", got)
	}
	// max_hp = con*10 + level*5 = 105; max_mana = int*8 + level*3 = 83.
	if got := e.MaxHP(); got != 105 {
		t.Fatalf("MaxHP() = %d, want 105", got)
	}
	if got := e.MaxMana(); got != 83 {
		t.Fatalf("MaxMana() = %d, want 83", got)
	}
	// A fresh entity reads full (no current stored => max).
	if got := e.HP(); got != 105 {
		t.Fatalf("fresh HP() = %d, want 105 (full)", got)
	}
	// Raising constitution (a base override) raises the derived max_hp through the stack.
	setAttrBase(e, "constitution", 20)
	if got := e.MaxHP(); got != 205 { // 20*10 + 1*5
		t.Fatalf("MaxHP() after con raise = %d, want 205", got)
	}

	// hp is the only VITAL resource in the demo.
	def := z.resourceDefs().get("hp")
	if def == nil || !def.vital {
		t.Fatal("demo hp resource should be vital")
	}
	// fire/slash damage types loaded with their resist matrix.
	if dt := z.damageTypeDefs().get("slash"); dt == nil || dt.resist["slash"] != 0.9 {
		t.Fatalf("slash damage type / resist not loaded: %+v", dt)
	}
}

// TestDemoPlayerEntityResolves drives newPlayerEntity (the real login construction path) in a demo
// zone and asserts the player resolves the content stats — "the demo character resolves on load".
func TestDemoPlayerEntityResolves(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := &session{character: "Pat"}
	e := z.newPlayerEntity(s, "Pat")
	if got := e.MaxHP(); got != 105 {
		t.Fatalf("demo player MaxHP() = %d, want 105", got)
	}
	if got := e.HP(); got != 105 {
		t.Fatalf("demo player HP() = %d, want full 105", got)
	}
}
