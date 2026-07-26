package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// worn_mods_test.go — #35 worn-affix stat effect. Done-when: wearing an item with a rolled affix raises the
// wearer's derived attribute; removing it drops the bonus; multiple sources stack additively; and the
// register-once seam never double-counts across repeated equips.

// wornStrength is the derived strength of the actor (a demo `stat` attribute, base 10) — the quantity the
// gear bonus should move.
func wornStrength(actor *Entity) float64 { return attr(actor, "strength") }

func TestWornAffixModifiesAttribute(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	addTestItem(e.z, actor, "an iron helmet", []string{"helmet"},
		wearableFor(WearLocHead), &Quality{Level: 1, Affixes: map[string]float64{"strength": 3}})

	e.run("wear helmet")
	if got := wornStrength(actor); got != base+3 {
		t.Fatalf("strength after wearing a +3-str helmet = %v, want %v", got, base+3)
	}

	e.run("remove helmet")
	if got := wornStrength(actor); got != base {
		t.Fatalf("strength after removing the helmet = %v, want %v (back to base)", got, base)
	}
}

// TestWornAffixStacksAdditively: two worn items each granting +str sum on the wearer.
func TestWornAffixStacksAdditively(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	addTestItem(e.z, actor, "an iron helmet", []string{"helmet"},
		wearableFor(WearLocHead), &Quality{Affixes: map[string]float64{"strength": 2}})
	addTestItem(e.z, actor, "iron boots", []string{"boots"},
		wearableFor(WearLocFeet), &Quality{Affixes: map[string]float64{"strength": 4}})

	e.run("wear helmet")
	e.run("wear boots")
	if got := wornStrength(actor); got != base+6 {
		t.Fatalf("strength with +2 helm and +4 boots = %v, want %v", got, base+6)
	}

	// Removing one drops only its contribution.
	e.run("remove boots")
	if got := wornStrength(actor); got != base+2 {
		t.Fatalf("strength after removing the boots = %v, want %v", got, base+2)
	}
}

// TestWornAffixNoDoubleCountOnRewear: the modSource is registered ONCE — wear/remove/wear cycles must land on
// the correct single-counted value, never a doubled bonus (the register-once seam under test).
func TestWornAffixNoDoubleCountOnRewear(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	addTestItem(e.z, actor, "an iron helmet", []string{"helmet"},
		wearableFor(WearLocHead), &Quality{Affixes: map[string]float64{"strength": 5}})

	for i := 0; i < 3; i++ {
		e.run("wear helmet")
		if got := wornStrength(actor); got != base+5 {
			t.Fatalf("iteration %d: strength worn = %v, want %v (no double-count)", i, got, base+5)
		}
		e.run("remove helmet")
		if got := wornStrength(actor); got != base {
			t.Fatalf("iteration %d: strength removed = %v, want %v", i, got, base)
		}
	}
}

// TestWieldedAffixModifiesAttribute: a wielded weapon's affix applies through the same seam.
func TestWieldedAffixModifiesAttribute(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	addTestItem(e.z, actor, "a steel sword", []string{"sword"},
		wearableFor(WearLocWield), &Weapon{diceNum: 2, diceSize: 6, damageType: "slash"},
		&Quality{Affixes: map[string]float64{"strength": 7}})

	e.run("wield sword")
	if got := wornStrength(actor); got != base+7 {
		t.Fatalf("strength wielding a +7-str sword = %v, want %v", got, base+7)
	}
	e.run("remove sword")
	if got := wornStrength(actor); got != base {
		t.Fatalf("strength after removing the sword = %v, want %v", got, base)
	}
}

// TestDestroyingWornItemDropsItsBonus: destroying a worn item (Move to nil — the salvage/consume path) must
// clear the slot AND drop its affix bonus, not leave a phantom permanent stat (the review-#1 exploit).
func TestDestroyingWornItemDropsItsBonus(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	helm := addTestItem(e.z, actor, "an iron helmet", []string{"helmet"},
		wearableFor(WearLocHead), &Quality{Affixes: map[string]float64{"strength": 10}})
	e.run("wear helmet")
	if got := wornStrength(actor); got != base+10 {
		t.Fatalf("precondition: strength worn = %v, want %v", got, base+10)
	}

	// Destroy the still-worn item the way salvage_item/consume_item do.
	Move(helm, nil)

	if got := wornStrength(actor); got != base {
		t.Fatalf("strength after destroying the worn item = %v, want %v (no phantom bonus)", got, base)
	}
	if wr, _ := Get[*Wearer](actor); wr.slotOf(helm) != WearLocNone || wr.worn[WearLocHead] != nil {
		t.Fatal("the worn slot must be cleared when the item is destroyed")
	}
}

// TestAugmentWornItemAppliesLive: augmenting an item while it is worn takes effect immediately (review-#2).
func TestAugmentWornItemAppliesLive(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	helm := addTestItem(e.z, actor, "an iron helmet", []string{"helmet"},
		wearableFor(WearLocHead), &Quality{Affixes: map[string]float64{"strength": 1}})
	e.run("wear helmet")

	// Augment the worn item's strength affix by +4 through the op (as a content enchant verb would).
	c := &effectCtx{z: e.z, actor: actor, source: actor}
	if err := opAugmentItem(c, &effectOp{item: string(helm.proto), attr: "strength", amount: 4}); err != nil {
		t.Fatalf("augment_item: %v", err)
	}
	if got := wornStrength(actor); got != base+5 {
		t.Fatalf("strength after augmenting a worn item by +4 (from +1) = %v, want %v (live)", got, base+5)
	}
}

// TestWornStaticModifierFlat (#514): an item's PROTOTYPE static `add` modifier raises the wearer's attribute
// while worn and drops on remove — no rolled Quality needed (every instance grants it).
func TestWornStaticModifierFlat(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	addTestItem(e.z, actor, "plate armor", []string{"plate"},
		&Wearable{locs: []WearLoc{WearLocBody}, add: map[string]float64{"strength": 2}})

	e.run("wear plate")
	if got := wornStrength(actor); got != base+2 {
		t.Fatalf("strength after wearing +2-str plate = %v, want %v", got, base+2)
	}
	e.run("remove plate")
	if got := wornStrength(actor); got != base {
		t.Fatalf("strength after removing the plate = %v, want %v (back to base)", got, base)
	}
}

// TestWornStaticModifierMul (#514): a static `mul` modifier scales the attribute multiplicatively (and
// mulMod returns to the identity when the item is removed).
func TestWornStaticModifierMul(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	addTestItem(e.z, actor, "a giant's crown", []string{"crown"},
		&Wearable{locs: []WearLoc{WearLocHead}, mul: map[string]float64{"strength": 1.5}})

	e.run("wear crown")
	if got := wornStrength(actor); got != base*1.5 {
		t.Fatalf("strength wearing a ×1.5-str crown = %v, want %v", got, base*1.5)
	}
	e.run("remove crown")
	if got := wornStrength(actor); got != base {
		t.Fatalf("strength after removing the crown = %v, want %v (mul back to identity)", got, base)
	}
}

// TestWornStaticAndRolledStack (#514): a prototype static `add` and a per-instance rolled affix on the SAME
// item both apply — the static modifier and the rolled Quality sum.
func TestWornStaticAndRolledStack(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	addTestItem(e.z, actor, "an enchanted helm", []string{"helm"},
		&Wearable{locs: []WearLoc{WearLocHead}, add: map[string]float64{"strength": 2}},
		&Quality{Affixes: map[string]float64{"strength": 1}})

	e.run("wear helm")
	if got := wornStrength(actor); got != base+3 {
		t.Fatalf("strength with +2 static and +1 rolled = %v, want %v", got, base+3)
	}
}

// TestWornStaticModifiersStackAcrossItems (#514): static adds SUM and static muls MULTIPLY across worn items.
func TestWornStaticModifiersStackAcrossItems(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	addTestItem(e.z, actor, "iron gauntlets", []string{"gauntlets"},
		&Wearable{locs: []WearLoc{WearLocHands}, add: map[string]float64{"strength": 3}})
	addTestItem(e.z, actor, "mighty boots", []string{"boots"},
		&Wearable{locs: []WearLoc{WearLocFeet}, mul: map[string]float64{"strength": 2}})

	e.run("wear gauntlets")
	e.run("wear boots")
	// Derivation is (base + flat) * mul: (base + 3) * 2.
	if got := wornStrength(actor); got != (base+3)*2 {
		t.Fatalf("strength with +3 flat and ×2 = %v, want %v", got, (base+3)*2)
	}
	e.run("remove boots")
	if got := wornStrength(actor); got != base+3 {
		t.Fatalf("strength after removing the ×2 boots = %v, want %v", got, base+3)
	}
}

// TestWearableFromDTO (#514): the content DTO's Modifiers parse into the Wearable's add/mul maps — "add"
// (and an unknown op) accumulate into add; "mul" into mul; repeated attrs combine.
func TestWearableFromDTO(t *testing.T) {
	d := &content.WearableDTO{
		Locations: []string{"body"},
		Modifiers: []content.AffectModifierDTO{
			{Attr: "ac", Op: "add", Value: 2},
			{Attr: "ac", Op: "add", Value: 1},         // sums -> ac add 3
			{Attr: "strength", Op: "mul", Value: 1.5}, // -> mul
			{Attr: "dex", Op: "", Value: 4},           // empty op defaults to add
			{Attr: "", Op: "add", Value: 9},           // no attr -> skipped
		},
	}
	w := wearableFromDTO(d)
	if w.add["ac"] != 3 {
		t.Fatalf("ac add = %v, want 3 (summed)", w.add["ac"])
	}
	if w.add["dex"] != 4 {
		t.Fatalf("dex add = %v, want 4 (empty op defaults to add)", w.add["dex"])
	}
	if w.mul["strength"] != 1.5 {
		t.Fatalf("strength mul = %v, want 1.5", w.mul["strength"])
	}
	if _, ok := w.add[""]; ok {
		t.Fatal("an attr-less modifier must be skipped")
	}
}

// TestDemoHelmetStaticModifierLiveEndToEnd (#514) is the WIRING guard: it spawns the REAL demo iron helmet
// (which declares a static +1 strength modifier in its YAML) through the content pipeline, wears it, and
// asserts the wearer's derived strength rises by 1. This pins the whole chain — YAML -> WearableDTO ->
// wearableFromDTO -> Wearable.add -> recomputeWornMods -> attr — that the hand-built &Wearable{} unit tests
// bypass. Reverting content_map.go from wearableFromDTO to wearableFromNames (dropping modifiers at spawn)
// fails HERE.
func TestDemoHelmetStaticModifierLiveEndToEnd(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := newTestPlayerEntity(z, "Helm")
	Move(s.entity, z.rooms["midgaard:room:temple"])
	base := attr(s.entity, "strength")

	helm := z.spawn(ProtoRef("midgaard:obj:helmet"))
	if helm == nil {
		t.Fatal("could not spawn the demo helmet")
	}
	Move(helm, s.entity) // into the player's inventory

	z.dispatch(s, "wear helmet")
	if got := attr(s.entity, "strength"); got != base+1 {
		t.Fatalf("strength after wearing the demo helmet = %v, want %v (+1 from its static modifier)", got, base+1)
	}
	z.dispatch(s, "remove helmet")
	if got := attr(s.entity, "strength"); got != base {
		t.Fatalf("strength after removing the demo helmet = %v, want %v (bonus dropped)", got, base)
	}
}

// TestWornStaticModifierNoDoubleCountOnRewear (#514): like the rolled-affix rewear guard, a static modifier
// must land on the single-counted value across wear/remove/wear cycles (the recompute-from-scratch seam).
func TestWornStaticModifierNoDoubleCountOnRewear(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	addTestItem(e.z, actor, "a heavy crown", []string{"crown"},
		&Wearable{locs: []WearLoc{WearLocHead}, add: map[string]float64{"strength": 5}})

	for i := 0; i < 3; i++ {
		e.run("wear crown")
		if got := wornStrength(actor); got != base+5 {
			t.Fatalf("iteration %d: strength worn = %v, want %v (no double-count)", i, got, base+5)
		}
		e.run("remove crown")
		if got := wornStrength(actor); got != base {
			t.Fatalf("iteration %d: strength removed = %v, want %v", i, got, base)
		}
	}
}

// TestWornItemNoQualityNoBonus: a worn item with no rolled Quality contributes nothing (an un-rolled
// prototype piece is inert, not a crash).
func TestWornItemNoQualityNoBonus(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	base := wornStrength(actor)

	addTestItem(e.z, actor, "a plain cap", []string{"cap"}, wearableFor(WearLocHead))
	e.run("wear cap")
	if got := wornStrength(actor); got != base {
		t.Fatalf("a no-quality worn item changed strength: %v, want %v", got, base)
	}
}
