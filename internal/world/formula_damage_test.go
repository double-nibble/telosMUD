package world

import (
	"math/rand"
	"testing"
)

// formula_damage_test.go exercises [G-A]: deal_damage with a SCOPED ATTRIBUTE formula bonus and a
// formula DICE COUNT — what lets a sword add STR, a crit scale, and a level-scaled rider express as
// content (the acceptance-gate requirement for ROM/5e/WoW). Deterministic: a size-1 die always rolls
// 1, so a dice-count formula's value equals the rolled total.

func damageZone(t *testing.T) (*Zone, *session, *Entity) {
	t.Helper()
	z, caster := abilityTestZone(t)
	z.defs.attr.register("str_bonus", &attributeDef{ref: "str_bonus"})
	z.defs.attr.register("damroll", &attributeDef{ref: "damroll"})
	z.defs.attr.register("level", &attributeDef{ref: "level"})
	z.defs.attr.register("exposed", &attributeDef{ref: "exposed"})
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	return z, caster, mob
}

func dmgCtx(z *Zone, actor, target *Entity) *effectCtx {
	return &effectCtx{
		z: z, actor: actor, source: actor, target: target, mag: 1,
		rng: rand.New(rand.NewSource(1)),
	}
}

// TestDealDamageScopedBonus: a weapon's "+ $actor.damroll + $actor.str_bonus" adds the ATTACKER's
// derived attributes to a flat/dice base — the canonical ROM/5e "STR-bonus sword".
func TestDealDamageScopedBonus(t *testing.T) {
	z, caster, mob := damageZone(t)
	setAttrBase(caster.entity, "str_bonus", 4)
	setAttrBase(caster.entity, "damroll", 2)

	op := &effectOp{
		kind: "deal_damage", dmgType: "fire", amount: 0,
		bonus: opNode{op: "+", args: []formulaNode{
			attrNode{ref: "$actor.damroll"}, attrNode{ref: "$actor.str_bonus"},
		}},
	}
	c := dmgCtx(z, caster.entity, mob)
	if err := opDealDamage(c, op); err != nil {
		t.Fatalf("opDealDamage: %v", err)
	}
	// fire is neutral (×1.0), soak 0: 0 base + (2 + 4) bonus = 6 damage -> hp 94.
	if hp := resourceCurrent(mob, "hp"); hp != 94 {
		t.Fatalf("scoped-bonus damage: hp = %d, want 94 (6 from damroll+str_bonus)", hp)
	}
}

// TestDealDamageDiceCountFormula: a level-scaled rider — ceil(level/2) d1 — scales the dice COUNT with
// a derived attribute. A size-1 die makes the total equal the count, so the formula is asserted exactly.
func TestDealDamageDiceCountFormula(t *testing.T) {
	z, caster, mob := damageZone(t)
	setAttrBase(caster.entity, "level", 5) // ceil(5/2) = 3

	op := &effectOp{
		kind: "deal_damage", dmgType: "fire", diceSize: 1,
		diceCount: opNode{op: "ceil", args: []formulaNode{
			opNode{op: "/", args: []formulaNode{attrNode{ref: "$actor.level"}, litNode{v: 2}}},
		}},
	}
	c := dmgCtx(z, caster.entity, mob)
	if err := opDealDamage(c, op); err != nil {
		t.Fatalf("opDealDamage: %v", err)
	}
	// ceil(5/2)=3 dice of size 1 -> 3 damage -> hp 97.
	if hp := resourceCurrent(mob, "hp"); hp != 97 {
		t.Fatalf("dice-count formula: hp = %d, want 97 (ceil(level/2)=3 d1)", hp)
	}
}

// TestDealDamageTargetScoping: the bonus can read the TARGET's attributes (armor-piercing / exposed).
func TestDealDamageTargetScoping(t *testing.T) {
	z, caster, mob := damageZone(t)
	setAttrBase(mob, "exposed", 10) // a defender-side weakness the attacker exploits

	op := &effectOp{kind: "deal_damage", dmgType: "fire", bonus: attrNode{ref: "$target.exposed"}}
	c := dmgCtx(z, caster.entity, mob)
	if err := opDealDamage(c, op); err != nil {
		t.Fatalf("opDealDamage: %v", err)
	}
	if hp := resourceCurrent(mob, "hp"); hp != 90 {
		t.Fatalf("target-scoped bonus: hp = %d, want 90 (10 from $target.exposed)", hp)
	}
}

// TestDealDamageFlatPathUnchanged: with no bonus/dice_count, the original flat-amount + literal-dice
// behavior is untouched (no regression).
func TestDealDamageFlatPathUnchanged(t *testing.T) {
	z, caster, mob := damageZone(t)
	op := &effectOp{kind: "deal_damage", dmgType: "fire", amount: 8}
	c := dmgCtx(z, caster.entity, mob)
	if err := opDealDamage(c, op); err != nil {
		t.Fatalf("opDealDamage: %v", err)
	}
	if hp := resourceCurrent(mob, "hp"); hp != 92 {
		t.Fatalf("flat-amount path: hp = %d, want 92 (8 flat)", hp)
	}
}

// TestDealDamageFormulaParse: the bonus + dice_count parse end-to-end from a content op map (a
// 1d8 + STR longsword authored as data).
func TestDealDamageFormulaParse(t *testing.T) {
	z, caster, mob := damageZone(t)
	setAttrBase(caster.entity, "str_bonus", 3)

	raw := map[string]any{
		"op":    "deal_damage",
		"type":  "fire",
		"dice":  "1d1", // deterministic: 1
		"bonus": []any{"attr", "$actor.str_bonus"},
	}
	op, err := parseOp(raw)
	if err != nil {
		t.Fatalf("parseOp: %v", err)
	}
	if op.bonus == nil {
		t.Fatal("parseOp did not parse the bonus formula")
	}
	c := dmgCtx(z, caster.entity, mob)
	if err := opDealDamage(c, &op); err != nil {
		t.Fatalf("opDealDamage: %v", err)
	}
	// 1 (1d1) + 3 (str_bonus) = 4 -> hp 96.
	if hp := resourceCurrent(mob, "hp"); hp != 96 {
		t.Fatalf("parsed formula-damage: hp = %d, want 96 (1d1 + str_bonus 3)", hp)
	}
}
