package world

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/content"
)

// depletion_test.go exercises #406: NON-VITAL DEPLETION HOOKS. Before #406, `on_depleted` fired only for
// a VITAL pool (it was the death hook and nothing else), so a secondary pool damaged to 0 ran nothing —
// which forced any pool that wanted a consequence to be `vital`, i.e. lethal (#71's any-vital-lethal
// policy). #406 decouples the two: ANY pool bottoming out runs its own op-list, and `vital` means only
// "the default consequence is DEATH". These tests drive the shared dealDamage funnel directly (white-box),
// like multivital_test.go, and they are written to fail if either half of the property breaks — the hook
// not running for a non-vital pool, OR a non-vital depletion reaching die().

// registerPool registers a pool `ref` capped by attribute `maxAttr` (base `maxVal`) with an on_depleted
// op-list. It is registerVital's non-vital-first sibling: the vital flag is the LAST argument so a test
// reads "register a pool ... which is not vital". Call BEFORE creating the mob so its first attr() read
// resolves the cap.
func registerPool(z *Zone, ref, maxAttr string, maxVal float64, onDepleted []effectOp, vital bool) {
	z.defs.attr.register(maxAttr, &attributeDef{ref: maxAttr, base: litNode{v: maxVal}})
	z.defs.res.register(ref, &resourceDef{
		ref: ref, maxAttr: maxAttr, vital: vital, onDepleted: onDepleted,
	})
}

// countingHook is an on_depleted op-list that raises a `fired` counter pool by 1 every time it runs. The
// counter is a plain non-vital pool with no hook of its own, so reading it is a direct count of hook
// RUNS — cardinality, not mere occurrence (the #25 lesson).
func countingHook(extra ...effectOp) []effectOp {
	ops := []effectOp{{kind: "modify_resource", tgt: "self", resource: "fired", amount: 1}}
	return append(ops, extra...)
}

// firedCount reads the counting-hook counter. The pool starts at 0 (the test sets it), so the current IS
// the number of times the hook ran.
func firedCount(e *Entity) int { return resourceCurrent(e, "fired") }

// countRoomCorpses counts the corpse containers in room. CARDINALITY, not occurrence: "a corpse exists"
// passes when TWO exist, and two corpses is a double die() — a second OnKill and a second resolveLoot,
// i.e. an item dupe (#69).
func countRoomCorpses(room *Entity) int {
	n := 0
	for _, e := range room.contents {
		if _, ok := Get[*Container](e); ok {
			n++
		}
	}
	return n
}

// depletionZone is combatZone plus the `fired` counter pool, ready for countingHook.
func depletionZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	z, s := combatZone(t)
	registerPool(z, "fired", "max_fired", 1000, nil, false)
	return z, s
}

// --- The headline: a NON-VITAL pool runs its own on_depleted, and does not kill ------------------

// TestNonVitalDepletionRunsItsHook is the core #406 case — the Call of Cthulhu model. A `sanity` pool
// that is explicitly NOT vital bottoms out: its on_depleted must run (applying 'insane'), and the victim
// must survive. Before #406 the hook simply never ran.
func TestNonVitalDepletionRunsItsHook(t *testing.T) {
	z, s := depletionZone(t)
	z.defs.affect.register("insane", &affectDef{ref: "insane", name: "Insane", stacking: stackRefresh, maxStacks: 1, duration: 20})
	registerPool(z, "sanity", "max_sanity", 100, countingHook(
		effectOp{kind: "apply_affect", affect: "insane", tgt: "self"},
	), false) // vital:false — a Sanity track, not a life pool

	mob := combatMob(z, s.entity, "investigator", "", 100)
	room := mob.location
	setResourceCurrent(mob, "sanity", 10)
	setResourceCurrent(mob, "fired", 0)

	dealt := dealTo(z, s.entity, mob, 50, "slash", "sanity")

	require.Positive(t, dealt, "a non-vital pool still takes damage")
	require.Equal(t, 0, resourceCurrent(mob, "sanity"), "sanity should be drained and clamped at 0")
	require.Equal(t, 1, firedCount(mob), "the NON-VITAL pool's on_depleted must run exactly once")
	require.True(t, hasAffect(mob, "insane"), "the hook's apply_affect must have landed")
	// The security half of the property, asserted on the same blow.
	require.NotEqual(t, posDead, position(mob), "a NON-VITAL depletion must never kill (security-critical)")
	require.Nil(t, roomCorpse(room), "a non-vital depletion must not drop a corpse")
	require.Equal(t, 100, resourceCurrent(mob, "hp"), "hp must be untouched by a sanity blow")
}

// TestNonVitalDepletionNeverReachesDieEvenWithoutARevive is the security case stated adversarially. The
// vital path's cancel re-check makes death conditional on the hook NOT reviving the pool; a reader could
// wrongly conclude a non-vital pool is safe only because its hook happens to revive something. Here the
// hook does nothing but count — the pool is left at 0 — and death must still not happen. The ONE gate is
// def.vital (onPoolDepleted's vitalDepleted re-check), not anything the hook did.
func TestNonVitalDepletionNeverReachesDieEvenWithoutARevive(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "stress", "max_stress", 100, countingHook(), false)

	mob := combatMob(z, s.entity, "rookie", "", 100)
	room := mob.location
	setResourceCurrent(mob, "stress", 1)
	setResourceCurrent(mob, "fired", 0)

	dealTo(z, s.entity, mob, 99, "slash", "stress")

	require.Equal(t, 1, firedCount(mob), "hook must have run")
	require.Equal(t, 0, resourceCurrent(mob, "stress"), "the pool is left EMPTY — nothing revived it")
	require.NotEqual(t, posDead, position(mob), "a non-vital pool left at 0 must not kill")
	require.Nil(t, roomCorpse(room))
	require.Equal(t, uint64(0), deathGen(mob), "no death generation may be consumed by a non-vital depletion")
}

// --- LEVEL-triggering: the hook fires on every blow that leaves the pool empty ------------------

// TestNonVitalHookIsLevelTriggered pins the trigger rule DELIBERATELY, because the alternative (fire only
// on the 0-crossing) is superficially attractive — it would stop a hook repeating while the pool sits
// empty — and is wrong. Under an edge rule an already-empty track SWALLOWS every subsequent blow with no
// consequence at all, so a two-track system (a stun/stagger pool whose excess carries into a lethal one)
// would make its bearer permanently immune to that damage kind after the first crossing. The carry-over
// is precisely what must keep happening. So: every blow that leaves the pool at 0 fires the hook.
func TestNonVitalHookIsLevelTriggered(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "stagger", "max_stagger", 100, countingHook(), false)

	mob := combatMob(z, s.entity, "brawler", "", 100)
	setResourceCurrent(mob, "stagger", 10)
	setResourceCurrent(mob, "fired", 0)

	// Blow 1: 10 -> 0, the crossing.
	dealTo(z, s.entity, mob, 50, "slash", "stagger")
	require.Equal(t, 1, firedCount(mob), "the crossing blow must fire the hook")

	// Blows 2 and 3 land on an ALREADY-empty pool. Each still deals damage — so under an edge rule that
	// damage would vanish with no consequence whatsoever.
	for i := 2; i <= 3; i++ {
		dealt := dealTo(z, s.entity, mob, 50, "slash", "stagger")
		require.Positive(t, dealt, "blow %d must still land (the precondition for this test to mean anything)", i)
		require.Equal(t, i, firedCount(mob),
			"blow %d onto an already-empty pool did NOT fire the hook: an empty track would absorb damage forever", i)
	}
}

// TestNonVitalHookFiresAgainAfterThePoolIsRestored covers the other direction: restore the pool above 0
// and the next depleting blow fires again. A Sanity track that could only ever break once per character
// would be a latch, not a pool.
func TestNonVitalHookFiresAgainAfterThePoolIsRestored(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "sanity", "max_sanity", 100, countingHook(), false)

	mob := combatMob(z, s.entity, "victim", "", 100)
	setResourceCurrent(mob, "sanity", 10)
	setResourceCurrent(mob, "fired", 0)

	dealTo(z, s.entity, mob, 50, "slash", "sanity")
	require.Equal(t, 1, firedCount(mob))

	setResourceCurrent(mob, "sanity", 10) // a heal / regen / respawn restores the pool
	dealTo(z, s.entity, mob, 50, "slash", "sanity")

	require.Equal(t, 2, firedCount(mob), "a SECOND depletion after a restore must fire the hook again")
}

// TestNonDepletingBlowFiresNothing is the negative control the two tests above need: a blow that leaves
// the pool ABOVE 0 must fire nothing. Without it, a hook that ran unconditionally on every blow would
// satisfy every "the hook fired" assertion in this file.
func TestNonDepletingBlowFiresNothing(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "sanity", "max_sanity", 100, countingHook(), false)

	mob := combatMob(z, s.entity, "victim", "", 100)
	setResourceCurrent(mob, "sanity", 40)
	setResourceCurrent(mob, "fired", 0)

	dealTo(z, s.entity, mob, 10, "slash", "sanity")

	require.Equal(t, 30, resourceCurrent(mob, "sanity"), "precondition: the pool is damaged but NOT empty")
	require.Zero(t, firedCount(mob), "a blow that did not empty the pool must fire no hook")
}

// --- The VITAL path keeps its trigger unchanged -------------------------------------------------

// TestVitalDepletionStillKillsOnANonCrossingBlow is the regression the level trigger exists to prevent on
// the death side. A vital pool can reach 0 through a path that runs NO checkpoint — modify_resource, an
// ability cost, a max drop. If death were gated on the crossing edge, the sword that lands on that
// already-0-hp victim would REFUSE TO KILL, leaving an unkillable 0-hp entity.
func TestVitalDepletionStillKillsOnANonCrossingBlow(t *testing.T) {
	z, s := depletionZone(t)
	mob := combatMob(z, s.entity, "goblin", "", 100)
	room := mob.location

	// Empty hp OFF the damage path (no checkpoint runs, so no death yet) — the setup that makes the next
	// blow a non-crossing one.
	c := &effectCtx{z: z, actor: s.entity, source: s.entity, target: mob, mag: 1, disp: dispHarmful}
	require.NoError(t, opModifyResource(c, &effectOp{kind: "modify_resource", resource: "hp", amount: -100}))
	require.Equal(t, 0, resourceCurrent(mob, "hp"), "precondition: hp emptied off the damage path")
	require.NotEqual(t, posDead, position(mob), "precondition: modify_resource must not itself kill")

	// The sword now lands on an already-empty vital: cur == 0, so crossed == false.
	dealTo(z, s.entity, mob, 5, "slash", "hp")

	require.Equal(t, posDead, position(mob),
		"a blow onto an already-empty VITAL pool must still kill — death must NOT be gated on the crossing edge")
	require.NotNil(t, roomCorpse(room), "the kill must produce a corpse")
}

// TestVitalHookStillRunsAndCancelsDeath re-pins the #71/6.5 vital contract under the #406 refactor: the
// vital pool's own hook still runs, and reviving that pool inside it still CANCELS the death.
func TestVitalHookStillRunsAndCancelsDeath(t *testing.T) {
	z, s := depletionZone(t)
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onDepleted: countingHook(effectOp{kind: "modify_resource", tgt: "self", resource: "hp", amount: 7}),
	})

	mob := combatMob(z, s.entity, "warden", "", 10)
	room := mob.location
	setResourceCurrent(mob, "fired", 0)

	dealTo(z, s.entity, mob, 50, "slash", "")

	require.Equal(t, 1, firedCount(mob), "the vital pool's hook must still run")
	require.Equal(t, 7, resourceCurrent(mob, "hp"), "the hook's revive must stand")
	require.NotEqual(t, posDead, position(mob), "reviving the depleted vital must still CANCEL the death")
	require.Nil(t, roomCorpse(room))
}

// --- Capacity: a pool the target has no capacity for fires nothing ------------------------------

// TestZeroMaxNonVitalPoolFiresNoHook is the #71 immunity discard viewed through #406. A mindless construct
// with max_sanity 0 must not "go insane": the routed blow is discarded before any write, and poolDepleted
// re-asserts max > 0 as defense in depth. Without the max>0 clause a capacity-less pool reads current 0 =
// depleted and would fire its hook on every psychic hit forever.
func TestZeroMaxNonVitalPoolFiresNoHook(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "sanity", "max_sanity", 0, countingHook(), false) // max 0 = no capacity

	mob := combatMob(z, s.entity, "construct", "", 100)
	setResourceCurrent(mob, "fired", 0)

	dealt := dealTo(z, s.entity, mob, 50, "slash", "sanity")

	require.Zero(t, dealt, "a blow to a 0-max pool is discarded as natural immunity")
	require.Zero(t, firedCount(mob), "a capacity-less pool must never fire its on_depleted")
	require.False(t, poolDepleted(mob, "sanity"), "poolDepleted must be false for a 0-max pool")
}

// --- A non-vital hook that WANTS to be lethal says so, and kills exactly once -------------------

// TestNonVitalHookCanCascadeIntoDeathExactlyOnce covers the deliberate escape hatch: a non-vital break
// that should be fatal (a Sanity break that stops the heart) is authored as damage to a VITAL pool from
// inside the non-vital hook. That must kill through the ordinary funnel — and EXACTLY once.
//
// CARDINALITY, not occurrence: the outer (sanity) frame is still holding a victim whose hp reads 0 when
// the nested death unwinds, and a second die() there would mean a second corpse, a second OnKill and a
// second resolveLoot — an item dupe (#69). Mutation-testing says the guard that actually stops it on THIS
// path is the pool-local vital gate (the outer frame re-checks `sanity`, which is not vital, and returns);
// deleting the #69 deathGen guard alone leaves this test green, because that guard covers the
// vital-hook-kills-through-another-vital path instead (deathgen_test.go). Both are asserted here because
// the property the world cares about is the count, not which guard delivered it.
func TestNonVitalHookCanCascadeIntoDeathExactlyOnce(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "sanity", "max_sanity", 100, countingHook(
		effectOp{kind: "deal_damage", tgt: "self", resource: "hp", amount: 500},
	), false)

	mob := combatMob(z, s.entity, "cultist", "", 30)
	room := mob.location
	setResourceCurrent(mob, "sanity", 5)
	setResourceCurrent(mob, "fired", 0)

	dealTo(z, s.entity, mob, 40, "slash", "sanity")

	require.Equal(t, 1, firedCount(mob), "the sanity hook ran once")
	require.Equal(t, posDead, position(mob), "the hook's lethal damage to a VITAL pool must kill")
	require.Equal(t, 1, countRoomCorpses(room), "exactly ONE corpse — a second die() is an item dupe (#69)")
	require.Equal(t, uint64(1), deathGen(mob), "exactly ONE death generation consumed")
}

// --- Recursion is bounded exactly as the death hook is ------------------------------------------

// TestNonVitalHookRecursionIsBounded proves the depth cap covers the new non-vital path too. Two non-vital
// pools whose hooks empty each other are an infinite mutual cascade; unbounded that is a process-fatal
// stack overflow taking down every zone on the shard, not just the fight. runDepletionHook's depth cap
// must terminate it — and with a nil eventBudget (a command-issued cast), depth ALONE must do it.
func TestNonVitalHookRecursionIsBounded(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "alpha", "max_alpha", 100, countingHook(
		effectOp{kind: "deal_damage", tgt: "self", resource: "beta", amount: 500},
	), false)
	registerPool(z, "beta", "max_beta", 100, countingHook(
		effectOp{kind: "deal_damage", tgt: "self", resource: "alpha", amount: 500},
	), false)

	mob := combatMob(z, s.entity, "ouroboros", "", 100)
	setResourceCurrent(mob, "alpha", 5)
	setResourceCurrent(mob, "beta", 5)
	setResourceCurrent(mob, "fired", 0)

	c := &effectCtx{
		z: z, actor: s.entity, source: s.entity, target: mob,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	require.Nil(t, c.eventBudget, "precondition: a nil budget, so the DEPTH cap alone must bound this")
	dealDamage(c, mob, 50, "slash", "alpha")

	fired := firedCount(mob)
	require.Positive(t, fired, "the cascade must actually have run (else this test proves nothing)")
	require.LessOrEqual(t, fired, maxEventDepth,
		"the non-vital hook cascade exceeded the depth cap (%d firings) — the recursion bound does not cover it", fired)
	require.NotEqual(t, posDead, position(mob), "no vital pool was ever touched, so nothing may have died")
}

// --- The CONTENT wiring: a non-vital on_depleted survives the YAML -> DTO -> runtime trip ---------

// TestNonVitalOnDepletedSurvivesTheContentPipeline pins the WIRING, not the unit. Every test above
// registers a resourceDef struct directly, so all of them would still pass if defineGlobals silently
// dropped `on_depleted` for a non-vital resource (it parses the list without consulting `vital` — but
// nothing pinned that, and a one-line `if r.Vital` there would revert the whole feature with the package
// green). This drives the real content path: a ResourceDTO with vital:false + an on_depleted op-list,
// through defineGlobals, into a live depletion.
func TestNonVitalOnDepletedSurvivesTheContentPipeline(t *testing.T) {
	z, s := depletionZone(t)
	z.defs.affect.register("insane", &affectDef{ref: "insane", name: "Insane", stacking: stackIgnore, maxStacks: 1, duration: 20})

	lc := &content.LoadedContent{
		Attributes: []content.AttributeDTO{
			{Ref: "max_sanity", DefaultBase: content.BaseSpecDTO{Lit: floatPtr(60)}},
		},
		Resources: []content.ResourceDTO{{
			Ref:         "sanity",
			DisplayName: "Sanity",
			MaxAttr:     "max_sanity",
			Vital:       false, // THE point: a non-vital pool carrying a hook
			OnDepleted: []any{
				map[string]any{"op": "apply_affect", "affect": "insane", "target": "self"},
			},
		}},
	}
	defineGlobals(z.defs, lc)

	def := z.defs.res.get("sanity")
	require.NotNil(t, def, "the content resource must be registered")
	require.False(t, def.vital, "fixture check: the pool under test is NOT vital")
	require.NotEmpty(t, def.onDepleted, "on_depleted was DROPPED at build for a non-vital resource")

	mob := combatMob(z, s.entity, "scholar", "", 100)
	room := mob.location
	setResourceCurrent(mob, "sanity", 5)

	dealTo(z, s.entity, mob, 50, "slash", "sanity")

	require.True(t, hasAffect(mob, "insane"), "the content-authored non-vital hook did not run")
	require.NotEqual(t, posDead, position(mob), "a content-authored non-vital depletion must not kill")
	require.Nil(t, roomCorpse(room))
}

func floatPtr(v float64) *float64 { return &v }

// --- TOTAL WORK is bounded, not just chain length (the security-review blocker) ------------------

// TestAreaDepletionHookIsWorkBounded is the regression test for the amplification the security review
// measured. A non-vital hook containing an AREA op — "a Sanity break panics the room", the headline use
// case — re-empties every entity in the room, each of which fires its own hook, at every level down to the
// depth cap: (N^maxEventDepth) work from ONE blow. Before the fix this seam only ever DECREMENTED a work
// budget somebody else had allocated; with a nil budget (a command-issued cast, an affect on_tick, a Lua
// trigger) nothing bounded the TOTAL, and 6 entities in a room measured 335,923 hook runs / 7.5s ON THE
// ZONE GOROUTINE — the heartbeat for every player in the zone. The depth cap alone does NOT catch this:
// each level is short, there are just exponentially many of them.
//
// The fix roots a budget at the seam exactly as fireEvent does at the root of an event cascade. The
// assertion is deliberately on TOTAL RUNS, not on wall-clock (which would be a flaky oracle).
func TestAreaDepletionHookIsWorkBounded(t *testing.T) {
	z, s := depletionZone(t)
	// The hook re-empties EVERY living entity in the room, including the one that just fired it.
	registerPool(z, "panic", "max_panic", 100, countingHook(
		effectOp{kind: "deal_damage", area: "room", resource: "panic", amount: 500},
	), false)

	victims := make([]*Entity, 0, 5)
	for i, name := range []string{"one", "two", "three", "four", "five"} {
		m := combatMob(z, s.entity, name, "", 100)
		setResourceCurrent(m, "panic", 5)
		setResourceCurrent(m, "fired", 0)
		victims = append(victims, m)
		require.Equal(t, i+1, len(victims))
	}

	c := &effectCtx{
		z: z, actor: s.entity, source: s.entity, target: victims[0],
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	require.Nil(t, c.eventBudget,
		"precondition: a NIL budget — this is the entry shape (cast / on_tick / Lua trigger) that was unbounded")

	dealDamage(c, victims[0], 50, "slash", "panic")

	total := 0
	for _, v := range victims {
		total += firedCount(v)
	}
	require.Positive(t, total, "the cascade must actually have run (else this test proves nothing)")
	require.LessOrEqual(t, total, maxEventHandlers,
		"an area on_depleted ran %d hooks off ONE blow with a nil budget — total work is unbounded at this seam", total)
}

// --- A blow whose victim died mid-flight does not fire that blow's depletion ---------------------

// TestDepletionIsVoidedWhenTheVictimDiesMidBlow is the second security-review blocker. The checkpoint runs
// AFTER OnDamageTaken/OnHit, and a handler there can kill the victim (an execute rider, a thorns reflect).
// respawnPlayer then puts a PLAYER back at the start room. Firing this blow's depletion hook afterwards
// aims a consequence at someone who has left the fight — in another room, with $other bound to a killer who
// is elsewhere: the "your harm follows you to the temple" class (#318).
//
// Before #406 this was fail-safe only by ACCIDENT (respawn refilled the vital, so vitalDepleted went false).
// It is closed by TWO independent mechanisms, and this test asserts the OBSERVABLE property both serve:
//  1. respawn now refills every pool, so the outer checkpoint finds the pool full — this is the load-bearing
//     one, and mutation-reverting it turns TestRespawnRestoresNonVitalPoolsToo red;
//  2. the deathGen re-read at the checkpoint (effect_op.go), deliberate defense-in-depth. Every path I can
//     enumerate is covered by (1) or by onPoolDepleted's posDead entry check for a mob, so reverting (2)
//     alone leaves this test green — it is belt-and-braces at a security seam, in the same spirit as
//     vitalDepleted re-asserting max > 0, NOT the primary guard. Said plainly here so nobody reads this test
//     as evidence for it.
func TestDepletionIsVoidedWhenTheVictimDiesMidBlow(t *testing.T) {
	z, s := depletionZone(t)
	// The tripwire is an AFFECT, not the usual counter pool: respawn now restores every pool, so a resource
	// counter reads its max after a respawn whether the hook ran or not. A benign affect applied by the hook
	// lands AFTER stripHostileAffects, so its presence means — unambiguously — the hook ran post-respawn.
	z.defs.affect.register("marked", &affectDef{ref: "marked", name: "Marked", stacking: stackIgnore, maxStacks: 1, duration: 50})
	registerPool(z, "sanity", "max_sanity", 100, []effectOp{
		{kind: "apply_affect", affect: "marked", tgt: "self"},
	}, false)
	// hp subscribes an execute: any damage taken finishes the victim off. It fires from OnDamageTaken — i.e.
	// INSIDE this blow, after the pool write and before the depletion checkpoint. For a PLAYER that means
	// die() -> respawnPlayer, which relocates them to the start room.
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onEvent: map[eventKind][]effectOp{
			evOnDamageTaken: {{kind: "deal_damage", tgt: "self", resource: "hp", amount: 9999}},
		},
	})

	victim := s.entity // a PLAYER: death respawns rather than corpses, which is the dangerous shape
	setResourceCurrent(victim, "sanity", 5)
	attacker := combatMob(z, victim, "horror", "", 100)

	c := &effectCtx{
		z: z, actor: attacker, source: attacker, target: victim,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	dealDamage(c, victim, 50, "slash", "sanity")

	require.Positive(t, deathGen(victim), "precondition: the OnDamageTaken execute must actually have killed the victim")
	require.False(t, hasAffect(victim, "marked"),
		"the depletion hook fired on a victim who had already died and respawned during this blow")
}

// --- Respawn restores every pool, not just the vitals -------------------------------------------

// TestRespawnRestoresNonVitalPoolsToo pins the third security-review finding. Once a non-vital pool
// bottoming out has a CONSEQUENCE, leaving it at 0 through respawn makes that consequence inescapable:
// stripHostileAffects clears the 'insane' the hook applied, then the next single point of damage to the
// still-empty pool re-fires the hook and re-applies it — so the #318 strip is a no-op and the player is
// stuck across logout/login/handoff (pool currents are persisted). Respawn therefore restores EVERY pool.
func TestRespawnRestoresNonVitalPoolsToo(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "sanity", "max_sanity", 60, nil, false)

	setResourceCurrent(s.entity, "hp", 0)
	setResourceCurrent(s.entity, "sanity", 0)
	setResourceCurrent(s.entity, "fired", 3)

	z.respawnPlayer(s.entity)

	require.Equal(t, resourceMax(s.entity, "hp"), resourceCurrent(s.entity, "hp"), "vitals restore as before")
	require.Equal(t, 60, resourceCurrent(s.entity, "sanity"),
		"a NON-VITAL pool left at 0 through respawn is an inescapable condition — respawn must restore it")
}

// --- The rewarding-op lint ----------------------------------------------------------------------

// TestLintDepletionHookGrants pins the engine-side guard behind the "never put a rewarding op in a
// non-vital on_depleted" authoring rule. Prose in a Go doc comment is not a control; the build-time lint
// is. The vital/death hook is exempt (latched to a real kill), and a narration/affect/damage hook — the
// normal case — must stay lint-clean or the warning becomes noise everyone ignores.
func TestLintDepletionHookGrants(t *testing.T) {
	z, _ := combatZone(t)
	reward := effectOp{kind: "produce_item", tgt: "self"}

	// A reward buried inside a flow branch must still be found.
	registerPool(z, "greed", "max_greed", 10, []effectOp{
		{kind: "if", then: []effectOp{reward}},
	}, false)
	// A vital pool's hook is exempt.
	registerPool(z, "blood", "max_blood", 10, []effectOp{reward}, true)
	// The normal case: narrate + affect + damage. Must NOT be flagged.
	registerPool(z, "sanity", "max_sanity", 10, []effectOp{
		{kind: "act", tgt: "self"},
		{kind: "apply_affect", affect: "insane", tgt: "self"},
		{kind: "deal_damage", tgt: "self", resource: "hp", amount: 5},
	}, false)

	got := lintDepletionHookGrants(z.defs)

	require.Len(t, got, 1, "expected exactly one finding: %+v", got)
	require.Equal(t, "greed", got[0].resource, "the NON-VITAL pool with the buried reward is the finding")
	require.Equal(t, "produce_item", got[0].op)
}

// --- The hook's ctx bindings ---------------------------------------------------------------------

// TestNonVitalHookBindsSourceAsOther pins the ctx binding the DTO now documents ($actor = the depleted
// entity, $other = the damage source). Nothing else in this file exercises `target: other` from a
// non-vital hook, so the documented contract was unpinned — a builder writing "lash back at whatever broke
// you" would be the one to discover it.
func TestNonVitalHookBindsSourceAsOther(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "sanity", "max_sanity", 100, []effectOp{
		{kind: "deal_damage", tgt: "other", resource: "hp", amount: 25},
	}, false)

	attacker := combatMob(z, s.entity, "horror", "", 100)
	victim := combatMob(z, s.entity, "seer", "", 100)
	setResourceCurrent(victim, "sanity", 5)

	c := &effectCtx{
		z: z, actor: attacker, source: attacker, target: victim,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	dealDamage(c, victim, 50, "slash", "sanity")

	require.Equal(t, 75, resourceCurrent(attacker, "hp"),
		"$other in a non-vital hook must bind the DAMAGE SOURCE — the hook's lash-back missed its attacker")
	require.Equal(t, 100, resourceCurrent(victim, "hp"), "the depleted entity is $actor, not $other")
}

// TestNonVitalHookRefusalIsFailOpen pins the refusal semantics documented on runDepletionHook: when the
// budget/depth is exhausted, a VITAL pool still dies the engine-default way (fail-CLOSED — a death-ward
// that could not run does not save you) while a NON-VITAL pool's depletion simply has no consequence
// (fail-OPEN). Those are opposite directions on purpose, and neither was pinned.
func TestNonVitalHookRefusalIsFailOpen(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "sanity", "max_sanity", 100, countingHook(), false)
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onDepleted: countingHook(effectOp{kind: "modify_resource", tgt: "self", resource: "hp", amount: 99}),
	})

	spent := 0 // an already-exhausted shared budget: every hook must be refused
	mob := combatMob(z, s.entity, "subject", "", 10)
	room := mob.location
	setResourceCurrent(mob, "sanity", 5)
	setResourceCurrent(mob, "fired", 0)

	nonVital := &effectCtx{
		z: z, actor: s.entity, source: s.entity, target: mob,
		mag: 1, disp: dispHarmful, eventBudget: &spent, rng: rand.New(rand.NewSource(1)),
	}
	dealDamage(nonVital, mob, 50, "slash", "sanity")

	require.Zero(t, firedCount(mob), "precondition: the exhausted budget must have refused the hook")
	require.NotEqual(t, posDead, position(mob), "FAIL-OPEN: a refused NON-vital hook is simply no consequence")

	vital := &effectCtx{
		z: z, actor: s.entity, source: s.entity, target: mob,
		mag: 1, disp: dispHarmful, eventBudget: &spent, rng: rand.New(rand.NewSource(1)),
	}
	dealDamage(vital, mob, 50, "slash", "hp")

	require.Zero(t, firedCount(mob), "precondition: the vital hook must ALSO have been refused")
	require.Equal(t, posDead, position(mob),
		"FAIL-CLOSED: a refused VITAL hook must still die the engine-default way — its revive never ran")
	require.Equal(t, 1, countRoomCorpses(room))
}

// --- The two triggers cannot be confused --------------------------------------------------------

// TestPoolDepletedAndVitalDepletedAgree pins the predicate pair directly: vitalDepleted must be exactly
// poolDepleted AND def.vital, for every combination. If they ever drift, the checkpoint and the pre-die
// cancel re-check can disagree about "is this death real?" — the failure mode #71 built the shared
// predicate to prevent.
func TestPoolDepletedAndVitalDepletedAgree(t *testing.T) {
	z, s := depletionZone(t)
	registerPool(z, "vitalfull", "max_vitalfull", 100, nil, true)
	registerPool(z, "vitalempty", "max_vitalempty", 100, nil, true)
	registerPool(z, "plainfull", "max_plainfull", 100, nil, false)
	registerPool(z, "plainempty", "max_plainempty", 100, nil, false)
	registerPool(z, "zeromax", "max_zeromax", 0, nil, true)

	mob := combatMob(z, s.entity, "subject", "", 100)
	setResourceCurrent(mob, "vitalempty", 0)
	setResourceCurrent(mob, "plainempty", 0)

	tests := []struct {
		pool       string
		wantPool   bool
		wantVital  bool
		whyItMatte string
	}{
		{"vitalfull", false, false, "a full vital is neither depleted nor lethal"},
		{"vitalempty", true, true, "an empty vital is both"},
		{"plainfull", false, false, "a full non-vital is neither"},
		{"plainempty", true, false, "an empty NON-vital is depleted but NEVER lethal"},
		{"zeromax", false, false, "a 0-max pool is capacity-less: neither, whatever its vital flag"},
		{"unknown", false, false, "an unregistered pool is neither"},
		{"", false, false, "the empty ref is neither"},
	}
	for _, tc := range tests {
		t.Run(tc.pool, func(t *testing.T) {
			require.Equal(t, tc.wantPool, poolDepleted(mob, tc.pool), "poolDepleted: %s", tc.whyItMatte)
			require.Equal(t, tc.wantVital, vitalDepleted(mob, tc.pool), "vitalDepleted: %s", tc.whyItMatte)
			if vitalDepleted(mob, tc.pool) {
				require.True(t, poolDepleted(mob, tc.pool), "vitalDepleted must IMPLY poolDepleted")
			}
		})
	}
}
