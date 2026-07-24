package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"

	"github.com/stretchr/testify/require"
)

// downed_test.go covers the downed/dying state (#535): a content affect carrying suspends_death holds
// its bearer alive at 0 HP instead of dying — the "unconscious and dying, not dead" state. The engine
// provides only the hold-at-0 + no-auto-regen; content owns the death-save resolution.
//
// The premise was confirmed by a verification panel: 0-HP-alive is ALREADY reachable, and the ONLY
// engine blocker was onPoolDepleted's binary disposition (deplete → die). This adds the third
// disposition INSIDE onPoolDepleted, never a second die() call site.

// downedZone registers hp (vital), a `dying` affect that suspends death + locks the bearer, and an
// hp on_depleted hook that applies the dying affect to self — the whole content shape.
func downedZone(t *testing.T) (*Zone, *session, *Entity) {
	t.Helper()
	z, caster := abilityTestZone(t)
	z.defs.affect.register("dying", &affectDef{
		ref: "dying", name: "Dying", stacking: stackRefresh, maxStacks: 1, duration: 30,
		suspendsDeath: true,
		prevents:      []string{"act", "move", "cast"}, // downed: cannot act
	})
	// hp's on_depleted applies the dying affect to self, then the onPoolDepleted re-check sees the
	// suspension and holds at 0 instead of dying.
	z.defs.res.reload("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true, primary: true,
		onDepleted: []effectOp{{kind: "apply_affect", affect: "dying", tgt: "self"}},
	}, false)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	return z, caster, mob
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

// TestDownedEntityDoesNotRegen pins the no-auto-revive: a downed entity must not passively heal out of
// the downed state.
func TestDownedEntityDoesNotRegen(t *testing.T) {
	z, caster, mob := downedZone(t)
	z.defs.res.reload("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true, primary: true, regen: 5,
		onDepleted: []effectOp{{kind: "apply_affect", affect: "dying", tgt: "self"}},
	}, false)

	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))

	for i := 0; i < 10; i++ {
		runRegen(mob)
	}
	require.Equal(t, 0, resourceCurrent(mob, "hp"), "a downed entity must NOT regen out of downed")
}

// TestDownedEntityCannotSwing pins that the downed bearer (prevents act) does not keep attacking even
// if dragged into a fighting link.
func TestDownedEntityCannotSwing(t *testing.T) {
	z, caster, mob := downedZone(t)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))

	// Force a fighting link (as a mob's startFight retaliation would).
	mutableLiving(mob).fighting = caster.entity
	require.False(t, z.swingGatesPass(mob, caster.entity),
		"a downed entity (prevents act) must not swing")
}

// TestSuspensionLiftedThenDies pins the resolution: when the dying affect is removed (content's death-
// save loop failed), a further blow — or re-running the checkpoint — kills normally.
func TestSuspensionLiftedThenDies(t *testing.T) {
	z, caster, mob := downedZone(t)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))
	require.NotEqual(t, posDead, position(mob))

	// Content resolves the death (death saves failed): remove the dying affect AND stop the hp hook from
	// re-downing (in real content the dying affect's on_tick gates the re-apply once resolved). Then the
	// next lethal blow kills normally.
	z.defs.res.reload("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true, primary: true, // no on_depleted: won't re-down
	}, false)
	a, _ := Get[*Affected](mob)
	for _, inst := range append([]*affectInstance(nil), a.list...) {
		if inst.def.ref == "dying" {
			a.expire(mob, inst, nil)
		}
	}
	require.False(t, deathSuspended(mob), "suspension lifted")

	dealDamage(c, mob, 10, "slash", "") // 10 slash * 0.5 resist = 5 > 0
	require.Equal(t, posDead, position(mob), "with the suspension gone, the blow kills normally")
}

// TestSuspensionExpiresAndRecovers pins the other resolution: the dying affect expires (survived the
// death saves), the suspension lifts, and regen resumes so the entity recovers from 0.
func TestSuspensionExpiresAndRecovers(t *testing.T) {
	z, caster, mob := downedZone(t)
	z.defs.res.reload("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true, primary: true, regen: 5,
		onDepleted: []effectOp{{kind: "apply_affect", affect: "dying", tgt: "self"}},
	}, false)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	dealDamage(c, mob, 500, "slash", "")
	require.True(t, deathSuspended(mob))
	require.Equal(t, 0, resourceCurrent(mob, "hp"))

	// Expire the dying affect (stabilized): suspension lifts.
	a, _ := Get[*Affected](mob)
	for _, inst := range append([]*affectInstance(nil), a.list...) {
		if inst.def.ref == "dying" {
			a.expire(mob, inst, nil)
		}
	}
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

// TestSuspendsDeathWiredFromContent pins the content_map wiring: buildAffectDef must carry
// SuspendsDeath from the DTO onto the runtime affectDef. Every other test registers the affectDef
// directly, so the DTO->def mapping is otherwise unpinned.
func TestSuspendsDeathWiredFromContent(t *testing.T) {
	def := buildAffectDef(content.AffectDTO{
		Ref: "dying", Body: content.AffectBodyDTO{Duration: 30, SuspendsDeath: true},
	})
	require.True(t, def.suspendsDeath, "buildAffectDef must carry suspends_death from the DTO")

	plain := buildAffectDef(content.AffectDTO{Ref: "poison", Body: content.AffectBodyDTO{Duration: 30}})
	require.False(t, plain.suspendsDeath, "an ordinary affect does not suspend death")
}
