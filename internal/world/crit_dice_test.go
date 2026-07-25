package world

// crit_dice_test.go exercises the crit dice-doubling mode (#544): a crit that doubles the DICE term
// only (1d8+3 -> 2d8+3), the 5e rule and the correct extra-dice variance — selectable alongside the
// existing whole-roll crit_mult, and reachable from both the swing crit and a check-band (spell) crit.
// Determinism: dice of size 1 always roll their count (2d1 = 2), so a doubled count is exactly observable.

import (
	"math/rand"
	"testing"
)

// critHitProfile is a to-hit that ALWAYS crits (a single "crit" band, no dice dependence).
func critHitProfile(damageBonus formulaNode) *combatProfile {
	return &combatProfile{
		toHit:       &checkSpec{label: "Attack", dice: mustDiceT("1d1"), bands: []checkBand{{label: "crit"}}},
		damageBonus: damageBonus,
	}
}

// regCritDice registers the crit_dice attribute (combatZone does not) with the given base.
func regCritDice(z *Zone, base float64) {
	z.defs.attr.register("crit_dice", &attributeDef{ref: "crit_dice", base: litNode{v: base}})
}

// TestSwingCritDoublesDiceNotModifier proves the 5e crit: crit_dice:2 doubles the weapon DICE only, and
// the flat bonus is added ONCE. Weapon 2d1 (=2) + bonus 5 = 7 on a normal hit; a crit_dice:2 crit rolls
// 4d1 (=4) + 5 = 9 — NOT 2*(2+5)=14. crit_mult is pinned to 1 so only the dice-doubling is in play.
func TestSwingCritDoublesDiceNotModifier(t *testing.T) {
	z, s := combatZone(t)
	regCritDice(z, 2)
	setAttrBase(s.entity, "crit_mult", 1) // isolate: no whole-roll multiply
	z.defs.combat.register("critter", critHitProfile(litNode{v: 5}))
	s.entity.living.combatRef = "critter"
	equipWeapon(s.entity, &Weapon{diceNum: 2, diceSize: 1, damageType: "slash"}) // 2d1 = 2
	mob := combatMob(z, s.entity, "dummy", "", 100)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())

	// 4d1 (dice doubled 2->4) + 5 bonus = 9. If the modifier were also doubled it would be 4+10=14; if
	// the whole roll were multiplied it would be 2*(2+5)=14. 9 proves DICE-ONLY doubling.
	if got := resourceCurrent(mob, "hp"); got != 91 {
		t.Fatalf("hp after crit_dice:2 crit = %d, want 91 (4d1 + 5 = 9; dice doubled, modifier once)", got)
	}
}

// TestSwingCritMultStillMultipliesWholeRoll proves the existing whole-roll crit_mult path is unchanged
// by #544: with crit_dice unset (1) and crit_mult:2, a crit is 2*(2+5)=14, modifier included.
func TestSwingCritMultStillMultipliesWholeRoll(t *testing.T) {
	z, s := combatZone(t)
	regCritDice(z, 1) // no dice-doubling
	// combatZone leaves crit_mult at base 2.
	z.defs.combat.register("critter", critHitProfile(litNode{v: 5}))
	s.entity.living.combatRef = "critter"
	equipWeapon(s.entity, &Weapon{diceNum: 2, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "dummy", "", 100)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())

	// 2*(2 dice + 5 bonus) = 14. Whole-roll multiply, modifier included.
	if got := resourceCurrent(mob, "hp"); got != 86 {
		t.Fatalf("hp after crit_mult:2 crit = %d, want 86 (2*(2+5)=14)", got)
	}
}

// TestSwingCritModesCompound proves the two knobs are independent and compound when BOTH are set:
// crit_dice:2 (2d1->4d1=4, +5 = 9) then crit_mult:2 (whole roll) = 18.
func TestSwingCritModesCompound(t *testing.T) {
	z, s := combatZone(t)
	regCritDice(z, 2)
	// crit_mult stays 2 (combatZone default).
	z.defs.combat.register("critter", critHitProfile(litNode{v: 5}))
	s.entity.living.combatRef = "critter"
	equipWeapon(s.entity, &Weapon{diceNum: 2, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "dummy", "", 100)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())

	// crit_dice: 4d1 + 5 = 9; crit_mult: *2 = 18.
	if got := resourceCurrent(mob, "hp"); got != 82 {
		t.Fatalf("hp after both crit knobs = %d, want 82 (crit_dice 4d1+5=9, x crit_mult 2 = 18)", got)
	}
}

// TestNonCritSwingIgnoresCritDice proves crit_dice is inert on a NORMAL (non-crit) hit: 2d1 + 5 = 7.
func TestNonCritSwingIgnoresCritDice(t *testing.T) {
	z, s := combatZone(t)
	regCritDice(z, 2)
	setAttrBase(s.entity, "crit_mult", 1)
	z.defs.combat.register("attacker", autoHitProfile(litNode{v: 5})) // always a plain hit, never crit
	s.entity.living.combatRef = "attacker"
	equipWeapon(s.entity, &Weapon{diceNum: 2, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "dummy", "", 100)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())

	if got := resourceCurrent(mob, "hp"); got != 93 {
		t.Fatalf("hp after non-crit hit = %d, want 93 (2d1 + 5 = 7, no crit doubling)", got)
	}
}

// TestCheckBandCritDoublesDice proves spell crits share the mechanism (#544): a `check` whose CRIT band
// contains a deal_damage doubles that op's dice by crit_dice. A 3d1 (=3) blow becomes 6d1 (=6) on the
// crit band with crit_dice:2 — the seam that was previously unwired for spells.
func TestCheckBandCritDoublesDice(t *testing.T) {
	z, s := combatZone(t)
	regCritDice(z, 2)
	mob := combatMob(z, s.entity, "dummy", "", 100)

	// A check that always lands in the "crit" band; the band deals 3d1 to the target.
	critOp := &effectOp{kind: "check", check: &checkSpec{
		dice: mustDiceT("1d1"),
		bands: []checkBand{{label: "crit", ops: []effectOp{
			{kind: "deal_damage", diceNum: 3, diceSize: 1, dmgType: "slash"},
		}}},
	}}
	c := &effectCtx{
		z: z, actor: s.entity, source: s.entity, target: mob, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(1)),
	}
	if err := opCheck(c, critOp); err != nil {
		t.Fatalf("opCheck: %v", err)
	}
	// 3d1 dice doubled to 6d1 = 6 applied on the crit band.
	if got := resourceCurrent(mob, "hp"); got != 94 {
		t.Fatalf("hp after spell crit-band = %d, want 94 (3d1 doubled to 6d1 = 6)", got)
	}
}

// TestCheckBandCritReadsRollerNotActor proves crit_dice is read from the ROLLER of the check, not the
// ctx actor (#544 review). Under subject: target (the saving-throw idiom) the roller is the ctx target;
// with crit_dice set ONLY on that target, the crit band's damage still doubles — so the wiring reads the
// entity that actually rolled the crit, matching the check's own scoping. Reverting to attr(c.actor)
// would read the caster (crit_dice unset -> 1) and fail to double.
func TestCheckBandCritReadsRollerNotActor(t *testing.T) {
	z, s := combatZone(t)
	regCritDice(z, 1) // default 1: nobody doubles unless overridden per-entity
	hero := s.entity  // the ctx actor (the caster); crit_dice stays 1
	mob := combatMob(z, hero, "dummy", "", 100)
	setAttrBase(mob, "crit_dice", 2) // ONLY the roller (the saver/target) has crit_dice

	// subject: target -> the mob rolls; its crit band deals 3d1 to itself (the saver is the default op
	// target under subject: target).
	saveOp := &effectOp{kind: "check", check: &checkSpec{
		subject: subjTarget,
		dice:    mustDiceT("1d1"),
		bands: []checkBand{{label: "crit", ops: []effectOp{
			{kind: "deal_damage", diceNum: 3, diceSize: 1, dmgType: "slash"},
		}}},
	}}
	c := &effectCtx{
		z: z, actor: hero, source: hero, target: mob, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(1)),
	}
	if err := opCheck(c, saveOp); err != nil {
		t.Fatalf("opCheck: %v", err)
	}
	// The roller (mob) has crit_dice 2 -> 3d1 doubled to 6d1 = 6. Reading the actor (hero, crit_dice 1)
	// would leave it at 3 (hp 97).
	if got := resourceCurrent(mob, "hp"); got != 94 {
		t.Fatalf("hp after roller-crit = %d, want 94 (roller crit_dice:2 doubles 3d1->6d1); reads the roller, not the actor", got)
	}
}

// TestCheckBandNonCritDoesNotDoubleDice proves the isCritBandLabel gate: a non-"crit" band (label "hit")
// does NOT double, even with crit_dice:2 set. 3d1 = 3 applied.
func TestCheckBandNonCritDoesNotDoubleDice(t *testing.T) {
	z, s := combatZone(t)
	regCritDice(z, 2)
	mob := combatMob(z, s.entity, "dummy", "", 100)

	hitOp := &effectOp{kind: "check", check: &checkSpec{
		dice: mustDiceT("1d1"),
		bands: []checkBand{{label: "hit", ops: []effectOp{
			{kind: "deal_damage", diceNum: 3, diceSize: 1, dmgType: "slash"},
		}}},
	}}
	c := &effectCtx{
		z: z, actor: s.entity, source: s.entity, target: mob, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(1)),
	}
	if err := opCheck(c, hitOp); err != nil {
		t.Fatalf("opCheck: %v", err)
	}
	if got := resourceCurrent(mob, "hp"); got != 97 {
		t.Fatalf("hp after non-crit band = %d, want 97 (3d1 = 3, no doubling)", got)
	}
	// The ctx crit multiplier must be fully restored after the band ops (no leak).
	if c.critDiceMult != 0 {
		t.Fatalf("critDiceMult leaked = %d, want 0 after opCheck", c.critDiceMult)
	}
}
