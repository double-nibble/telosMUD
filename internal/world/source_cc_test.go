package world

// source_cc_test.go exercises source-relative crowd control (#546): a `prevents_source` tag blocks an
// action ONLY when its target is the affect's source (the charmer). Charmed can't attack the charmer, but
// attacks everyone else; the same shape backs Frightened's "can't move closer to the source".

import (
	"math/rand"
	"testing"
)

// charmZone sets up a charmed hero (an auto-hit attacker) plus the charmer and a bystander in the room.
func charmZone(t *testing.T) (*Zone, *session, *Entity, *Entity) {
	z, s := combatZone(t)
	z.defs.affect.register("charm", &affectDef{
		ref: "charm", name: "Charmed", stacking: stackRefresh, maxStacks: 1, duration: 100,
		preventsSource: []string{"attack"}, // can't attack the SOURCE specifically
	})
	z.defs.combat.register("attacker", autoHitProfile(nil))
	s.entity.living.combatRef = "attacker"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	charmer := combatMob(z, s.entity, "charmer", "", 100)
	bystander := combatMob(z, s.entity, "bystander", "", 100)
	return z, s, charmer, bystander
}

// TestSwingCharmedCannotHitCharmer proves the swing gate: a charmed attacker's swing at the CHARMER is
// blocked (no damage), while a swing at a bystander lands — source-relative, not a global disarm.
func TestSwingCharmedCannotHitCharmer(t *testing.T) {
	z, s, charmer, bystander := charmZone(t)
	hero := s.entity
	applyAffect(hero, "charm", attachOpts{source: charmer}, nil) // charmed BY the charmer

	// Swing at the charmer: blocked -> no damage.
	z.resolveSwing(hero, charmer, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(charmer, "hp"); got != 100 {
		t.Fatalf("charmer hp = %d, want 100 (a charmed attacker cannot hit the charmer)", got)
	}
	// Swing at a bystander: lands normally.
	z.resolveSwing(hero, bystander, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(bystander, "hp"); got == 100 {
		t.Fatalf("bystander hp = %d, want < 100 (charm is source-relative, not a global disarm)", got)
	}
}

// TestSourceCCDoesNotBlockDirectDamage pins the INTENTIONAL scope (#546 review): source-relative CC is a
// TARGETING gate on cast + swing only — it is NOT a hard harm firewall. A direct deal_damage op aimed at
// the charmer (the AoE / Lua-damage / reaction path) still lands (subject only to the independent PvP
// gate, which is ungated here because the target is a mob). This test exists so the scope stays a
// conscious decision, not an assumed protection.
func TestSourceCCDoesNotBlockDirectDamage(t *testing.T) {
	z, s, charmer, _ := charmZone(t)
	hero := s.entity
	applyAffect(hero, "charm", attachOpts{source: charmer}, nil)

	// A direct deal_damage at the charmer (the shape an AoE op / a Lua h:damage uses) is NOT gated by
	// prevents_source — only the swing and cast gates are.
	c := &effectCtx{z: z, actor: hero, source: hero, target: charmer, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	dealDamage(c, charmer, 10, "slash", "")
	if got := resourceCurrent(charmer, "hp"); got != 90 {
		t.Fatalf("charmer hp = %d, want 90 (source-relative CC does NOT gate a direct deal_damage — it is not a harm firewall)", got)
	}
}

// TestPreventsTagFromSourceScoping unit-tests the query: true for the affect's source, false for another
// entity, and a GLOBAL prevents does not leak into it (source-relative and global are separate sets).
func TestPreventsTagFromSourceScoping(t *testing.T) {
	z, e := affectTestZone(t)
	z.defs.affect.register("charm", &affectDef{
		ref: "charm", stacking: stackRefresh, maxStacks: 1, duration: 100, preventsSource: []string{"attack"},
	})
	z.defs.affect.register("disarm_global", &affectDef{
		ref: "disarm_global", stacking: stackRefresh, maxStacks: 1, duration: 100, prevents: []string{"attack"},
	})
	charmer := makeMobTarget(z, e, "charmer")
	other := makeMobTarget(z, e, "other")

	applyAffect(e, "charm", attachOpts{source: charmer}, nil)
	if !preventsTagFromSource(e, "attack", charmer) {
		t.Fatal("must be blocked from attacking the charmer")
	}
	if preventsTagFromSource(e, "attack", other) {
		t.Fatal("must NOT be blocked from attacking a non-charmer")
	}
	// A GLOBAL prevents:[attack] must not register as source-relative (the sets are distinct).
	applyAffect(e, "disarm_global", attachOpts{source: nil}, nil)
	if preventsTagFromSource(e, "attack", other) {
		t.Fatal("a global prevents:[attack] must not leak into the source-relative query")
	}
}

// TestCastCharmedCannotTargetCharmer proves the cast gate (step 3): a charmed caster is blocked from a
// harmful "attack"-tagged ability aimed at the charmer, but may aim it at anyone else.
func TestCastCharmedCannotTargetCharmer(t *testing.T) {
	z, s, charmer, bystander := charmZone(t)
	hero := s.entity
	applyAffect(hero, "charm", attachOpts{source: charmer}, nil)

	def := &abilityDef{ref: "strike", name: "Strike", tags: []string{"attack"}}
	if z.checkRequires(s, def, charmer) {
		t.Fatal("checkRequires should BLOCK an attack-tagged cast at the charmer")
	}
	if !z.checkRequires(s, def, bystander) {
		t.Fatal("checkRequires should ALLOW the same cast at a bystander")
	}
}

// TestNilSourceCharmIsNotGlobalBlock proves a source-relative affect with a NIL source blocks nothing (it
// never silently becomes a global block) — a nil-source charm is a content error, not a lockout.
func TestNilSourceCharmIsNotGlobalBlock(t *testing.T) {
	z, e := affectTestZone(t)
	z.defs.affect.register("charm", &affectDef{
		ref: "charm", stacking: stackRefresh, maxStacks: 1, duration: 100, preventsSource: []string{"attack"},
	})
	other := makeMobTarget(z, e, "other")
	applyAffect(e, "charm", attachOpts{source: nil}, nil) // no source
	if preventsTagFromSource(e, "attack", other) {
		t.Fatal("a nil-source source-relative affect must block nothing")
	}
	a, _ := Get[*Affected](e)
	if len(a.preventsSrc) > 0 {
		t.Fatalf("a nil-source affect must not populate preventsSrc, got %v", a.preventsSrc)
	}
}
