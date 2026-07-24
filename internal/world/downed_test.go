package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"

	"github.com/stretchr/testify/require"
)

// downed_test.go covers the downed/dying state (#535): a content affect carrying suspends_death holds
// its bearer alive at 0 HP instead of dying — the "unconscious and dying, not dead" state. The engine
// provides only the hold-at-0 + no-self-revive; content owns the death-save resolution.
//
// The premise was confirmed by a verification panel: 0-HP-alive is ALREADY reachable, and the ONLY
// engine blocker was onPoolDepleted's binary disposition (deplete → die). This adds the third
// disposition INSIDE onPoolDepleted, never a second die() call site.
//
// REALIZABLE CONTENT SHAPE (combat/security panels). The vital on_depleted hook that applies the dying
// affect MUST gate its own re-apply, or the bearer is unkillable (every lethal blow re-downs it). The
// faithful, no-new-engine-feature pattern models the death-save budget as a resource (`death_saves`,
// max 3) and gates the hook `if death_saves >= 1 then [apply dying]`. The dying affect's on_tick
// decrements death_saves on a failed save and, at 0, removes itself + deals lethal — so the finishing
// depletion falls through the now-no-op hook to die(). downedZone builds exactly that shape, and the
// tests exercise finishing WITHOUT reloading the resourceDef mid-fight (which live content cannot do).

const deathSaveSlots = 3

// downedZone registers hp (vital), a `death_saves` counter, a `dying` affect that suspends death + locks
// the bearer, and an hp on_depleted hook that applies the dying affect to self ONLY while death saves
// remain — the whole realizable content shape.
func downedZone(t *testing.T) (*Zone, *session, *Entity) {
	t.Helper()
	z, caster := abilityTestZone(t)
	z.defs.affect.register("dying", &affectDef{
		ref: "dying", name: "Dying", stacking: stackRefresh, maxStacks: 1, duration: 30,
		suspendsDeath: true,
		prevents:      []string{"act", "move", "cast"}, // downed: cannot act
	})
	// death_saves is the content death-save budget (D&D-faithful: 3 failure slots). The hp on_depleted
	// hook re-downs ONLY while a slot remains — once content's death-save loop exhausts them, the hook is
	// a no-op and the next depletion falls through to die().
	z.defs.attr.register("max_death_saves", &attributeDef{ref: "max_death_saves", base: litNode{v: deathSaveSlots}})
	z.defs.res.register("death_saves", &resourceDef{ref: "death_saves", maxAttr: "max_death_saves"})
	// hp's on_depleted applies the dying affect to self, GATED on death_saves remaining. The onPoolDepleted
	// re-check then sees the suspension and holds at 0 instead of dying.
	z.defs.res.reload("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true, primary: true,
		onDepleted: []effectOp{{
			kind: "if", ifResource: "death_saves", ifResourceMin: 1,
			then: []effectOp{{kind: "apply_affect", affect: "dying", tgt: "self"}},
		}},
	}, false)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(mob, "death_saves", deathSaveSlots)
	return z, caster, mob
}

// removeDying strips the dying affect off e (as content's resolution would), so deathSuspended lifts.
func removeDying(e *Entity) {
	a, ok := Get[*Affected](e)
	if !ok {
		return
	}
	for _, inst := range append([]*affectInstance(nil), a.list...) {
		if inst.def != nil && inst.def.ref == "dying" {
			a.expire(e, inst, nil)
		}
	}
}

// TestKillingBlowDownsInsteadOfKilling is the headline: a blow that empties hp applies the dying affect
// and the victim is HELD at 0 — alive, not dead, no corpse, posDead unset.
func TestKillingBlowDownsInsteadOfKilling(t *testing.T) {
	z, caster, mob := downedZone(t)
	gen := deathGen(mob)

	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "") // massively lethal

	require.Equal(t, 0, resourceCurrent(mob, "hp"), "the pool is at 0")
	require.NotEqual(t, posDead, position(mob), "the victim is NOT dead")
	require.True(t, deathSuspended(mob), "the dying affect holds it")
	require.Equal(t, gen, deathGen(mob), "no death occurred (deathGen unchanged)")
	require.NotNil(t, mob.location, "still in the world, not extracted to a corpse")
}

// TestDownedEntityDoesNotRegen pins the no-self-revive: a downed entity must not passively heal out of
// the downed state.
func TestDownedEntityDoesNotRegen(t *testing.T) {
	z, caster, mob := downedZone(t)
	z.defs.res.reload("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true, primary: true, regen: 5,
		onDepleted: []effectOp{{
			kind: "if", ifResource: "death_saves", ifResourceMin: 1,
			then: []effectOp{{kind: "apply_affect", affect: "dying", tgt: "self"}},
		}},
	}, false)

	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))

	for i := 0; i < 10; i++ {
		runRegen(mob)
	}
	require.Equal(t, 0, resourceCurrent(mob, "hp"), "a downed entity must NOT regen out of downed")
}

// TestDownedEntityCannotSwing pins that the downed bearer does not keep attacking even if dragged into a
// fighting link — via the STRUCTURAL canAct gate, not the content prevents tag.
func TestDownedEntityCannotSwing(t *testing.T) {
	z, caster, mob := downedZone(t)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))

	// Force a fighting link (as a mob's startFight retaliation would).
	mutableLiving(mob).fighting = caster.entity
	require.False(t, z.swingGatesPass(mob, caster.entity),
		"a downed entity must not swing")
}

// TestSuspensionResolvedThenDies pins the FINISH: content's death saves run out, the dying affect is
// removed and the hook stops re-downing (death_saves == 0), and the next blow kills normally — WITHOUT
// reloading the resourceDef mid-fight (the realizable content path, not a test-only shortcut).
func TestSuspensionResolvedThenDies(t *testing.T) {
	z, caster, mob := downedZone(t)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))
	require.NotEqual(t, posDead, position(mob))

	// Content's death-save loop exhausted the budget (3 failed saves) and resolved to death: drain the
	// death_saves counter and lift the suspension. The hp hook is UNCHANGED — its `if death_saves >= 1`
	// gate now takes the empty else path, so it no longer re-downs.
	setResourceCurrent(mob, "death_saves", 0)
	removeDying(mob)
	require.False(t, deathSuspended(mob), "suspension lifted")

	dealDamage(c, mob, 10, "slash", "") // 10 slash * 0.5 resist = 5 > 0
	require.Equal(t, posDead, position(mob), "with the saves spent, the finishing blow kills normally")
}

// TestUnkillableIfHookNotGated is the mutation guard for the CONTENT CONTRACT: an UNGATED hook (applies
// dying every depletion) makes the bearer unkillable — every lethal blow re-downs it. This pins WHY the
// gate is required, and fails RED if a future refactor lets the hook re-apply while already suspended.
func TestUnkillableIfHookNotGated(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defs.affect.register("dying", &affectDef{
		ref: "dying", name: "Dying", stacking: stackRefresh, maxStacks: 1, duration: 30, suspendsDeath: true,
	})
	z.defs.res.reload("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true, primary: true,
		onDepleted: []effectOp{{kind: "apply_affect", affect: "dying", tgt: "self"}}, // UNGATED (bad)
	}, false)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	c := seededCtx(z, caster.entity, mob, dispHarmful)

	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob), "first blow downs")

	// Even after removing dying, an UNGATED hook re-applies it on the next blow (re-down) — you cannot
	// finish it without a content gate. This is the trap the death_saves gate avoids.
	removeDying(mob)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob), "an ungated hook re-downs — the bearer is unkillable")
	require.NotEqual(t, posDead, position(mob))
}

// TestDownedVitalHookNotFarmed is the SECURITY guard (auditor F2): the vital on_depleted hook must NOT
// re-fire on repeated blows onto an already-downed victim. A rewarding op in the hook (here a counter
// bump standing in for produce_item/grant) would otherwise be farmed once per swing — a dupe the #406
// lint's vital-pool exemption cannot catch, because a downed victim never latches posDead.
func TestDownedVitalHookNotFarmed(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defs.affect.register("dying", &affectDef{
		ref: "dying", name: "Dying", stacking: stackRefresh, maxStacks: 1, duration: 30,
		suspendsDeath: true, prevents: []string{"act"},
	})
	z.defs.attr.register("loot_count", &attributeDef{ref: "loot_count"})
	// A vital hp hook that BOTH downs the victim AND hands out a reward (the farmable shape). Ungated on
	// purpose: the ENGINE (not the content) must stop the re-run while downed.
	z.defs.res.reload("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true, primary: true,
		onDepleted: []effectOp{
			{kind: "apply_affect", affect: "dying", tgt: "self"},
			{kind: "modify_attribute_base", attr: "loot_count", amount: 1, tgt: "self"},
		},
	}, false)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	c := seededCtx(z, caster.entity, mob, dispHarmful)

	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))
	require.Equal(t, 1.0, attr(mob, "loot_count"), "the hook ran ONCE on the downing blow")

	for i := 0; i < 5; i++ {
		dealDamage(c, mob, 500, "slash", "") // further blows onto the downed victim
	}
	require.Equal(t, 1.0, attr(mob, "loot_count"),
		"the vital hook must NOT re-fire while downed (no farmed reward)")
}

// TestDownedClockNotRefreshed is the other half of the re-fire guard (combat panel): repeated blows onto
// a downed victim must NOT reset the dying affect's finite duration. If they did, a sustained attack
// refreshes the death clock faster than it ticks — an eternal, unresolvable hold.
func TestDownedClockNotRefreshed(t *testing.T) {
	z, caster, mob := downedZone(t)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))

	// Wind the dying affect's remaining down toward expiry, then keep hitting the downed victim.
	a, _ := Get[*Affected](mob)
	var dying *affectInstance
	for _, inst := range a.list {
		if inst.def.ref == "dying" {
			dying = inst
		}
	}
	require.NotNil(t, dying)
	dying.remaining = 3

	for i := 0; i < 5; i++ {
		dealDamage(c, mob, 500, "slash", "")
	}
	require.Equal(t, 3, dying.remaining,
		"a blow onto a downed victim must not refresh the death-save clock")
}

// TestSuspensionExpiresAndRecovers pins the other resolution: the dying affect expires (survived the
// death saves), the suspension lifts, and regen resumes so the entity recovers from 0.
func TestSuspensionExpiresAndRecovers(t *testing.T) {
	z, caster, mob := downedZone(t)
	z.defs.res.reload("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true, primary: true, regen: 5,
		onDepleted: []effectOp{{
			kind: "if", ifResource: "death_saves", ifResourceMin: 1,
			then: []effectOp{{kind: "apply_affect", affect: "dying", tgt: "self"}},
		}},
	}, false)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))
	require.Equal(t, 0, resourceCurrent(mob, "hp"))

	// Expire the dying affect (stabilized): suspension lifts.
	removeDying(mob)
	require.False(t, deathSuspended(mob))
	runRegen(mob)
	require.Positive(t, resourceCurrent(mob, "hp"), "with the suspension gone, regen resumes and it recovers")
	require.NotEqual(t, posDead, position(mob))
}

// TestNonSuspendedDepletionStillKills is the non-regression: an entity with NO dying affect dies the
// engine-default way. The third disposition must be inert unless content opts in.
func TestNonSuspendedDepletionStillKills(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	require.False(t, deathSuspended(mob), "no dying affect")

	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.Equal(t, posDead, position(mob), "without a suspension, a vital depletion kills as before")
}

// TestDeathSuspendedReader pins the reader directly across the affect states.
func TestDeathSuspendedReader(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	require.False(t, deathSuspended(e), "no affects")

	z.defs.affect.register("dying", &affectDef{ref: "dying", duration: 30, suspendsDeath: true})
	z.defs.affect.register("poison", &affectDef{ref: "poison", duration: 30})
	applyAffect(e, "poison", attachOpts{}, nil)
	require.False(t, deathSuspended(e), "an ordinary affect does not suspend death")
	applyAffect(e, "dying", attachOpts{}, nil)
	require.True(t, deathSuspended(e), "a suspends_death affect does")
}

// TestSuspendsDeathWiredFromContent pins the content_map wiring: buildAffectDef must carry SuspendsDeath
// from the DTO onto the runtime affectDef. Every other test registers the affectDef directly, so the
// DTO->def mapping is otherwise unpinned.
func TestSuspendsDeathWiredFromContent(t *testing.T) {
	def := buildAffectDef(content.AffectDTO{
		Ref: "dying", Body: content.AffectBodyDTO{Duration: 30, SuspendsDeath: true},
	})
	require.True(t, def.suspendsDeath, "buildAffectDef must carry suspends_death from the DTO")

	plain := buildAffectDef(content.AffectDTO{Ref: "poison", Body: content.AffectBodyDTO{Duration: 30}})
	require.False(t, plain.suspendsDeath, "an ordinary affect does not suspend death")
}

// TestSuspendsDeathIsDetrimental pins the harm-gate derivation (security F1a): a suspends_death affect
// with NO modifiers and NO prevents tags is still DETRIMENTAL, so a cross-player apply routes through
// the PvP gate. This is the sixth harm-gate-blindness class of the round — the sign heuristic cannot
// see suspends_death.
func TestSuspendsDeathIsDetrimental(t *testing.T) {
	bare := &affectDef{ref: "undying", suspendsDeath: true} // no modifiers, no prevents
	require.True(t, affectIsDetrimental(bare, harmPolarity{}),
		"suspends_death must be treated as harm even with no modifiers/prevents")

	plain := &affectDef{ref: "bless"}
	require.False(t, affectIsDetrimental(plain, harmPolarity{}), "a plain affect is not harm")
}

// TestSuspendsDeathApplyGatedCrossPlayer is the integration half of F1a: a bare suspends_death affect
// applied (via the derived-harm path, NEUTRAL disposition) to a non-consenting player is BLOCKED by the
// PvP gate. Before the derivation fix it landed ungated.
func TestSuspendsDeathApplyGatedCrossPlayer(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defs.affect.register("undying", &affectDef{ref: "undying", duration: 30, suspendsDeath: true})
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")

	// NEUTRAL ctx, op.harmful unset: the ONLY thing that can route this through the gate is
	// affectIsDetrimental deriving harm from suspends_death.
	c := seededCtx(z, caster.entity, victim.entity, dispNeutral)
	c.source = caster.entity
	require.NoError(t, opApplyAffect(c, &effectOp{kind: "apply_affect", affect: "undying"}))

	if a, ok := Get[*Affected](victim.entity); ok {
		for _, inst := range a.list {
			require.NotEqual(t, "undying", inst.def.ref,
				"a suspends_death affect must not land on a non-consenting player")
		}
	}
}

// TestSuspendedHealthyEntityRegens pins the SCOPED regen suppression (security F1b): a suspends_death
// affect on an otherwise-healthy (not-at-0) entity must NOT freeze its regen — the suppression is
// scoped to the DEPLETED VITAL pool. A blanket skip would make any suspends_death affect a total
// regen-denial debuff.
func TestSuspendedHealthyEntityRegens(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defs.res.reload("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true, primary: true, regen: 5}, false)
	z.defs.affect.register("undying", &affectDef{ref: "undying", duration: 30, suspendsDeath: true})
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 50) // healthy, not at 0
	applyAffect(mob, "undying", attachOpts{}, nil)
	require.True(t, deathSuspended(mob))

	runRegen(mob)
	require.Greater(t, resourceCurrent(mob, "hp"), 50,
		"a suspends_death affect must not deny regen to a healthy (not-at-0) bearer")
}

// TestDownedNoPreventsCannotAct is the structural incapacitation guard (security F3): a suspends_death
// affect with NO prevents tags still leaves its downed bearer unable to act — canAct/canReact/swing all
// refuse it via the deathSuspended predicate, not via a content `prevents: [act]` tag. Without this a
// downed entity is unkillable AND still swinging.
func TestDownedNoPreventsCannotAct(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defs.affect.register("dying", &affectDef{
		ref: "dying", stacking: stackRefresh, maxStacks: 1, duration: 30, suspendsDeath: true,
	}) // NOTE: no prevents tags
	z.defs.res.reload("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true, primary: true,
		onDepleted: []effectOp{{kind: "apply_affect", affect: "dying", tgt: "self"}},
	}, false)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))

	require.False(t, canAct(mob), "a downed entity cannot act, even without a prevents tag")
	require.False(t, canReact(mob), "a downed entity cannot react")
	mutableLiving(mob).fighting = caster.entity
	require.False(t, z.swingGatesPass(mob, caster.entity), "a downed entity cannot swing")
}

// TestDownedPlayerCannotCast pins the cast backstop (security F3): a downed PLAYER session cannot cast
// even a fully-open ability — the checkRequires downed gate refuses it.
func TestDownedPlayerCannotCast(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defs.affect.register("dying", &affectDef{ref: "dying", duration: 30, suspendsDeath: true})
	applyAffect(caster.entity, "dying", attachOpts{}, nil)
	require.True(t, deathSuspended(caster.entity))

	open := &abilityDef{ref: "shout", name: "shout", invocation: "command", words: []string{"shout"}}
	require.False(t, z.checkRequires(caster, open), "a downed player cannot cast even an open ability")
}
