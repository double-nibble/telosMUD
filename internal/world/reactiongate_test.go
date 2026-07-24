package world

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

// reactiongate_test.go covers the incapacitation gate on reactions (#540): an incapacitated reactor
// takes no reaction — no Shield, no Counterspell, no OnDamageTaken reaction, no opportunity attack —
// while the plain event bus (DoT ticks, level-ups, affect expiry, a PASSIVE thorns shield) is
// untouched. "Incapacitated" is content-defined: an affect carrying `prevents: [react]`, or a
// sleeping/dead position. The premise was confirmed empirically by a verification panel: before this,
// every reaction path ran with no reactor gate at all.

// incapacitate gives entity e an affect that prevents the `react` tag (a stun/paralyze).
func incapacitate(z *Zone, e *Entity) {
	if z.defs.affect.get("stunned") == nil {
		z.defs.affect.register("stunned", &affectDef{
			ref: "stunned", name: "Stunned", stacking: stackRefresh, maxStacks: 1, duration: 100,
			prevents: []string{reactPreventTag},
		})
	}
	applyAffect(e, "stunned", attachOpts{}, nil)
}

// TestCanReact is the unit table for the predicate.
func TestCanReact(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	require.True(t, canReact(e), "a standing, unaffected entity can react")

	incapacitate(z, e)
	require.False(t, canReact(e), "an affect preventing `react` blocks reactions")

	// Position also gates: strip the affect, then sleep.
	a, _ := Get[*Affected](e)
	for _, inst := range append([]*affectInstance(nil), a.list...) {
		a.expire(e, inst, nil)
	}
	require.True(t, canReact(e), "affect gone -> can react again")
	setPosition(e, posSleeping)
	require.False(t, canReact(e), "a sleeping entity cannot react (canAct is false)")
	setPosition(e, posDead)
	require.False(t, canReact(e), "a dead entity cannot react")
}

// TestStunnedDefenderDoesNotCounterspell drives the BeforeCastCommit reaction: a stunned observer must
// NOT counter a cast. The control (an able observer) does counter, so the gate is what makes the
// difference, not a broken fixture.
func TestStunnedDefenderDoesNotCounterspell(t *testing.T) {
	run := func(t *testing.T, stun bool) int {
		z, caster := abilityTestZone(t)
		mob := makeMobTarget(z, caster.entity, "goblin")
		setResourceCurrent(mob, "hp", 100)
		setResourceCurrent(caster.entity, "mana", 100)
		registerRoom(z, caster.entity.location)
		wizard := makeMobTarget(z, caster.entity, "wizard")
		reactScriptedDefender(z, wizard, `on("BeforeCastCommit", function(ev, rx) rx:cancel() end)`)
		if stun {
			incapacitate(z, wizard)
		}
		def := &abilityDef{
			ref: "luabolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
			costs:        []resourceCost{{resource: "mana", amount: 10}},
			onResolveLua: `ctx.target:damage{amount=30, type="fire"}`,
		}
		z.defs.ability.register("luabolt", def)
		z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))
		return resourceCurrent(mob, "hp")
	}

	require.Equal(t, 100, run(t, false), "control: an able observer counters, no damage")
	require.Equal(t, 70, run(t, true), "a STUNNED observer does not counter — the cast lands for 30")
}

// TestStunnedDefenderDoesNotShield drives the ToHit reaction: a stunned defender must not raise its AC
// via a Shield reaction. Verified through fireToHitReaction directly (the swing pipeline reads its
// returned AC delta).
func TestStunnedDefenderDoesNotShield(t *testing.T) {
	run := func(t *testing.T, stun bool) float64 {
		z, caster := abilityTestZone(t)
		defender := makeMobTarget(z, caster.entity, "knight")
		reactScriptedDefender(z, defender, `on("ToHit", function(ev, rx) rx:modify("ac", 5) end)`)
		if stun {
			incapacitate(z, defender)
		}
		c := &effectCtx{z: z, actor: caster.entity, source: caster.entity, target: defender, mag: 1, rng: rand.New(rand.NewSource(1))}
		return z.fireToHitReaction(c, caster.entity, defender)
	}

	require.Equal(t, 5.0, run(t, false), "control: an able defender Shields for +5 AC")
	require.Equal(t, 0.0, run(t, true), "a STUNNED defender does not Shield")
}

// TestStunnedTargetRunsNoDamageReaction drives the OnDamageTaken reaction: a stunned target must not
// run a reaction that cancels/reduces the blow.
func TestStunnedTargetRunsNoDamageReaction(t *testing.T) {
	run := func(t *testing.T, stun bool) int {
		z, caster := abilityTestZone(t)
		target := makeMobTarget(z, caster.entity, "monk")
		reactScriptedDefender(z, target, `on("OnDamageTaken", function(ev, rx) rx:cancel() end)`)
		if stun {
			incapacitate(z, target)
		}
		c := &effectCtx{z: z, actor: caster.entity, source: caster.entity, target: target, mag: 1, rng: rand.New(rand.NewSource(1))}
		return z.applyDamageReaction(c, target, 30, 30, "fire", "")
	}

	require.Equal(t, 0, run(t, false), "control: an able target cancels the blow")
	require.Equal(t, 30, run(t, true), "a STUNNED target runs no reaction — the blow lands in full")
}

// TestStunnedReactorMakesNoOpportunityAttack drives the OA path (fireLeaveRoom → the OnLeaveRoom bus).
// An engaged foe that is stunned must not opportunity-attack a fleeing leaver.
func TestStunnedReactorMakesNoOpportunityAttack(t *testing.T) {
	run := func(t *testing.T, stun bool) int {
		z, caster := abilityTestZone(t)
		leaver := caster.entity
		setResourceCurrent(leaver, "hp", 100)
		// A resource whose OnLeaveRoom handler deals an opportunity attack (the demo's shape).
		z.defs.res.register("reactions", &resourceDef{
			ref: "reactions", maxAttr: "max_reactions",
			onEvent: map[eventKind][]effectOp{
				evOnLeaveRoom: {{kind: "deal_damage", dmgType: "slash", amount: 7, tgt: "other"}},
			},
		})
		z.defs.attr.register("max_reactions", &attributeDef{ref: "max_reactions", base: litNode{v: 1}})
		foe := makeMobTarget(z, leaver, "guard")
		setResourceCurrent(foe, "reactions", 1)
		z.startFight(foe, leaver) // engage: foe.fighting = leaver
		if stun {
			incapacitate(z, foe)
		}
		z.fireLeaveRoom(nil, leaver)
		return resourceCurrent(leaver, "hp")
	}

	require.Less(t, run(t, false), 100, "control: an engaged foe opportunity-attacks the leaver")
	require.Equal(t, 100, run(t, true), "a STUNNED foe makes no opportunity attack — the leaver is untouched")
}

// TestPassiveBusHandlerStillFiresWhileStunned is the other half of the contract: the gate touches ONLY
// the reaction checkpoints, never the plain event bus. A PASSIVE handler on the same OnDamageTaken
// event (an on_event thorns shield, not an on_reaction reaction) must still fire while stunned — this
// is how content draws the passive/reactive line, and gating it would be a regression.
func TestPassiveBusHandlerStillFiresWhileStunned(t *testing.T) {
	z, caster := abilityTestZone(t)
	attacker := caster.entity
	setResourceCurrent(attacker, "hp", 100)
	victim := makeMobTarget(z, attacker, "cactus")
	setResourceCurrent(victim, "hp", 100)
	// A PASSIVE thorns: an affect on_event[OnDamageTaken] that reflects damage to the attacker. This
	// rides the declarative bus (gatherEventHandlers), NOT the reaction pass.
	z.defs.affect.register("thorns", &affectDef{
		ref: "thorns", name: "Thorns", stacking: stackRefresh, maxStacks: 1, duration: 100,
		onEvent: map[eventKind][]effectOp{
			evOnDamageTaken: {{kind: "deal_damage", dmgType: "slash", amount: 5, tgt: "other"}},
		},
	})
	applyAffect(victim, "thorns", attachOpts{}, nil)
	incapacitate(z, victim) // stunned AND thorned

	c := &effectCtx{z: z, actor: attacker, source: attacker, target: victim, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	dealDamage(c, victim, 20, "fire", "")

	require.Less(t, resourceCurrent(attacker, "hp"), 100,
		"a PASSIVE (on_event) thorns shield must still reflect while the bearer is stunned — the gate is reaction-only")
}
