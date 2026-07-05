package world

import "testing"

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
