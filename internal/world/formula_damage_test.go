package world

import (
	"math/rand"
	"strings"
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

// TestDealDamageDiceNumFormula (#517): the natural `dice_num` key accepts a FORMULA, not just an int
// literal — the canonical 5e cantrip scaler (fire-bolt: 1 die at L1..4, ceil(level/2) here as a size-1
// proxy). Authored end-to-end from a content op map through parseOp; a size-1 die makes the total equal
// the count so the formula is asserted exactly. Pins that dice_num-as-formula routes to the dice-count
// slot instead of the silent-0 that int(mapFloat(...)) would produce for a non-numeric value.
func TestDealDamageDiceNumFormula(t *testing.T) {
	z, caster, mob := damageZone(t)
	setAttrBase(caster.entity, "level", 7) // ceil(7/2) = 4

	raw := map[string]any{
		"op":        "deal_damage",
		"type":      "fire",
		"dice_num":  []any{"ceil", []any{"/", []any{"attr", "$actor.level"}, 2}},
		"dice_size": 1,
	}
	op, err := parseOp(raw)
	if err != nil {
		t.Fatalf("parseOp: %v", err)
	}
	if op.diceCount == nil {
		t.Fatal("dice_num formula did not populate op.diceCount")
	}
	if op.diceNum != 0 {
		t.Fatalf("dice_num formula should not set the literal diceNum, got %d", op.diceNum)
	}
	c := dmgCtx(z, caster.entity, mob)
	if err := opDealDamage(c, &op); err != nil {
		t.Fatalf("opDealDamage: %v", err)
	}
	// ceil(7/2)=4 dice of size 1 -> 4 damage -> hp 96.
	if hp := resourceCurrent(mob, "hp"); hp != 96 {
		t.Fatalf("dice_num-formula damage: hp = %d, want 96 (ceil(level/2)=4 d1)", hp)
	}
}

// TestDiceNumLiteralStillWorks (#517): a numeric dice_num remains the literal count (no regression from
// the polymorphic key).
func TestDiceNumLiteralStillWorks(t *testing.T) {
	z, caster, mob := damageZone(t)
	raw := map[string]any{"op": "deal_damage", "type": "fire", "dice_num": 3, "dice_size": 1}
	op, err := parseOp(raw)
	if err != nil {
		t.Fatalf("parseOp: %v", err)
	}
	if op.diceNum != 3 || op.diceCount != nil {
		t.Fatalf("literal dice_num: diceNum=%d diceCount=%v, want 3/nil", op.diceNum, op.diceCount)
	}
	c := dmgCtx(z, caster.entity, mob)
	if err := opDealDamage(c, &op); err != nil {
		t.Fatalf("opDealDamage: %v", err)
	}
	if hp := resourceCurrent(mob, "hp"); hp != 97 { // 3 d1
		t.Fatalf("literal dice_num damage: hp = %d, want 97 (3 d1)", hp)
	}
}

// TestDiceCountSourceConflicts (#517): the die count may be set EXACTLY ONE way — `dice` / `dice_num` /
// `dice_count`. Every pairing is contradictory content and must be REJECTED at parse, not silently
// resolved by dropping one source (the silent-precedence footgun). Covers the whole matrix, including
// the natural cantrip mistake `dice: "1d10"` + a scaling `dice_num`, which the old code silently ignored.
func TestDiceCountSourceConflicts(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
	}{
		{"formula dice_num + dice_count", map[string]any{
			"dice_num": []any{"attr", "$actor.level"}, "dice_count": []any{"attr", "$actor.level"}, "dice_size": 6,
		}},
		{"literal dice_num + dice_count", map[string]any{ // the silent-precedence case (dice_count would win)
			"dice_num": 3, "dice_count": []any{"attr", "$actor.level"}, "dice_size": 6}},
		{"dice shorthand + formula dice_num", map[string]any{ // the natural cantrip mistake
			"dice": "1d10", "dice_num": []any{"attr", "$actor.level"}}},
		{"dice shorthand + literal dice_num", map[string]any{
			"dice": "2d6", "dice_num": 3,
		}},
		{"dice shorthand + dice_count", map[string]any{
			"dice": "1d10", "dice_count": []any{"attr", "$actor.level"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.raw["op"] = "deal_damage"
			tc.raw["type"] = "fire"
			_, err := parseOp(tc.raw)
			if err == nil {
				t.Fatalf("expected parseOp to reject a doubly-set die count (%s)", tc.name)
			}
			if !strings.Contains(err.Error(), "die count") {
				t.Fatalf("error should name the die-count conflict, got: %v", err)
			}
		})
	}
}

// TestDiceNumUnparseableFormula (#517): a present-but-unparseable dice_num now surfaces a parse error
// (fail-at-boot), not the pre-change loud-log silent-0 — the footgun the polymorphic key closes.
func TestDiceNumUnparseableFormula(t *testing.T) {
	raw := map[string]any{"op": "deal_damage", "type": "fire", "dice_num": []any{"bogus_head", 1}, "dice_size": 6}
	_, err := parseOp(raw)
	if err == nil {
		t.Fatal("expected parseOp to reject an unparseable dice_num formula")
	}
	if !strings.Contains(err.Error(), "dice_num") {
		t.Fatalf("error should be attributed to dice_num, got: %v", err)
	}
}

// TestHealDiceCountFormula (#517): the dice-count formula also drives the HEAL/restore path (rollOpAmount
// is shared) — the upcast case: a cure spell rolling `slot_level` d1 heals more per slot spent.
func TestHealDiceCountFormula(t *testing.T) {
	z, caster, mob := damageZone(t)
	z.defs.attr.register("slot_level", &attributeDef{ref: "slot_level"})
	setAttrBase(caster.entity, "slot_level", 5) // a 5th-level slot
	setResourceCurrent(mob, "hp", 10)

	raw := map[string]any{
		"op":        "heal",
		"resource":  "hp",
		"dice_num":  []any{"attr", "$actor.slot_level"},
		"dice_size": 1,
	}
	op, err := parseOp(raw)
	if err != nil {
		t.Fatalf("parseOp: %v", err)
	}
	c := dmgCtx(z, caster.entity, mob)
	if err := opHeal(c, &op); err != nil {
		t.Fatalf("opHeal: %v", err)
	}
	// 5 (slot_level) d1 = 5 healing -> hp 15.
	if hp := resourceCurrent(mob, "hp"); hp != 15 {
		t.Fatalf("heal dice-count formula: hp = %d, want 15 (slot_level=5 d1)", hp)
	}
}

// TestRestoreDiceCountFormula (#517): `restore` shares rollOpAmount with heal/deal_damage, so a formula
// dice count drives it too. Pins the `restore` op directly (the issue names deal_damage/heal/restore).
func TestRestoreDiceCountFormula(t *testing.T) {
	z, caster, mob := damageZone(t)
	z.defs.attr.register("slot_level", &attributeDef{ref: "slot_level"})
	setAttrBase(caster.entity, "slot_level", 4)
	setResourceCurrent(mob, "hp", 10)

	raw := map[string]any{
		"op":        "restore",
		"resource":  "hp",
		"dice_num":  []any{"attr", "$actor.slot_level"},
		"dice_size": 1,
	}
	op, err := parseOp(raw)
	if err != nil {
		t.Fatalf("parseOp: %v", err)
	}
	c := dmgCtx(z, caster.entity, mob)
	if err := opRestore(c, &op); err != nil {
		t.Fatalf("opRestore: %v", err)
	}
	// 4 (slot_level) d1 = 4 restored -> hp 14.
	if hp := resourceCurrent(mob, "hp"); hp != 14 {
		t.Fatalf("restore dice-count formula: hp = %d, want 14 (slot_level=4 d1)", hp)
	}
}
