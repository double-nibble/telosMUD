package world

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// overflow_test.go exercises #407: OVERFLOW MAGNITUDE AT THE DEPLETION CHECKPOINT. setResourceCurrent
// clamps the stored current at 0, so before #407 the excess of a blow that drove a pool past 0 was simply
// gone by the time the on_depleted hook ran — a hook could tell THAT the pool emptied but not by HOW MUCH.
// That is the whole difficulty of a two-track system (a stun/stagger track carrying its excess into a
// lethal pool, fatigue below 0 biting into health, an HP-buffer spilling into a stat). #407 captures the
// pre-clamp excess at the write and exposes it to the hook's formulas as `$depletion.overflow` (plus
// `.applied` and `.amount`), in the same `$` ctx-scalar family as `$swing.index`.

// spillHook is the canonical carry-over op-list: deal the OVERFLOW of this pool's depletion into `into`.
// `amount: 0` + a bonus formula is the standard formula-damage idiom (rollOpAmount sums them).
func spillHook(into string) []effectOp {
	return []effectOp{{
		kind: "deal_damage", tgt: "self", resource: into,
		amount: 0, bonus: attrNode{ref: "$depletion.overflow"},
	}}
}

// --- The primitive: a hook can read how far past 0 the blow went ---------------------------------

// TestDepletionOverflowIsReadableByTheHook is the core #407 case. The pool holds 10 and takes 30, so 20
// overflowed. The hook deals exactly that into a second pool — a number that did not exist anywhere after
// the write before this change.
func TestDepletionOverflowIsReadableByTheHook(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "spilled", "max_spilled", 500, nil, false)
	registerPool(z, "stagger", "max_stagger", 100, spillHook("spilled"), false)

	mob := combatMob(z, s.entity, "brawler", "", 100)
	setResourceCurrent(mob, "stagger", 10)
	setResourceCurrent(mob, "spilled", 500) // full; the spill DAMAGES it, so read the drop

	dealTo(z, s.entity, mob, 30, "slash", "stagger")

	require.Equal(t, 0, resourceCurrent(mob, "stagger"), "the struck pool is emptied and clamped")
	require.Equal(t, 480, resourceCurrent(mob, "spilled"),
		"the hook must have carried exactly the 20 of overflow (30 dealt onto a pool holding 10)")
}

// TestDepletionFieldsSplitTheBlow pins all three fields together and the identity that ties them:
// applied + overflow == amount. A hook that narrates "you lost N" and carries the rest needs both halves,
// and the identity is what lets it split a blow without doing arithmetic in content.
func TestDepletionFieldsSplitTheBlow(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "absorbed", "max_absorbed", 500, nil, false)
	registerPool(z, "spilled", "max_spilled", 500, nil, false)
	registerPool(z, "total", "max_total", 500, nil, false)
	registerPool(z, "stagger", "max_stagger", 100, []effectOp{
		{kind: "deal_damage", tgt: "self", resource: "absorbed", amount: 0, bonus: attrNode{ref: "$depletion.applied"}},
		{kind: "deal_damage", tgt: "self", resource: "spilled", amount: 0, bonus: attrNode{ref: "$depletion.overflow"}},
		{kind: "deal_damage", tgt: "self", resource: "total", amount: 0, bonus: attrNode{ref: "$depletion.amount"}},
	}, false)

	mob := combatMob(z, s.entity, "brawler", "", 100)
	setResourceCurrent(mob, "stagger", 10)
	for _, p := range []string{"absorbed", "spilled", "total"} {
		setResourceCurrent(mob, p, 500)
	}

	dealTo(z, s.entity, mob, 30, "slash", "stagger")

	applied := 500 - resourceCurrent(mob, "absorbed")
	overflow := 500 - resourceCurrent(mob, "spilled")
	amount := 500 - resourceCurrent(mob, "total")

	require.Equal(t, 10, applied, "applied = what the pool could absorb (it held 10)")
	require.Equal(t, 20, overflow, "overflow = what it could not (30 - 10)")
	require.Equal(t, 30, amount, "amount = the whole mitigated blow")
	require.Equal(t, amount, applied+overflow, "applied + overflow must equal amount, always")
}

// --- The edge cases that define the invariant ----------------------------------------------------

// TestOverflowEdgeCases walks the boundary values. The exact-to-zero blow is the one that bites authors:
// it still FIRES the hook (the checkpoint keys on current <= 0, not on overflow > 0) but carries 0, so a
// hook must read correctly at 0. The already-empty pool is the carry-over case: it absorbs nothing, so the
// whole blow overflows — which is exactly what makes a saturated stun track keep spilling.
func TestOverflowEdgeCases(t *testing.T) {
	tests := []struct {
		name         string
		start, blow  int
		wantOverflow int
		why          string
	}{
		{"exact to zero", 10, 10, 0, "the pool absorbed the whole blow — fires the hook, carries nothing"},
		{"one past zero", 10, 11, 1, "the smallest real overflow"},
		{"already empty", 0, 25, 25, "an empty pool absorbs nothing: the WHOLE blow carries over"},
		{"deep overkill", 5, 500, 495, "no cap — content bounds it with a min() if it wants one"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z, s := combatZone(t)
			registerPool(z, "spilled", "max_spilled", 1000, nil, false)
			registerPool(z, "stagger", "max_stagger", 100, spillHook("spilled"), false)

			mob := combatMob(z, s.entity, "subject", "", 100)
			setResourceCurrent(mob, "stagger", tc.start)
			setResourceCurrent(mob, "spilled", 1000)

			dealTo(z, s.entity, mob, float64(tc.blow), "slash", "stagger")

			require.Equal(t, tc.wantOverflow, 1000-resourceCurrent(mob, "spilled"), tc.why)
		})
	}
}

// TestNonDepletingBlowCarriesNoOverflow is the negative control: a blow the pool survives fires no hook at
// all, so nothing is carried. Without it, a hook that always spilled its full damage would pass every
// positive assertion above.
func TestNonDepletingBlowCarriesNoOverflow(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "spilled", "max_spilled", 500, nil, false)
	registerPool(z, "stagger", "max_stagger", 100, spillHook("spilled"), false)

	mob := combatMob(z, s.entity, "subject", "", 100)
	setResourceCurrent(mob, "stagger", 50)
	setResourceCurrent(mob, "spilled", 500)

	dealTo(z, s.entity, mob, 10, "slash", "stagger")

	require.Equal(t, 40, resourceCurrent(mob, "stagger"), "precondition: the pool survived the blow")
	require.Equal(t, 500, resourceCurrent(mob, "spilled"), "a non-depleting blow must carry nothing")
}

// --- The scalar is SCOPED to the hook ------------------------------------------------------------

// TestDepletionScalarsAreZeroOutsideAHook pins the containment property. The depletion arithmetic lives on
// the depletion ctx only — never on the swing/cast ctx, which outlives the checkpoint and would otherwise
// leak a stale overflow into later ops in the same op-list or later swings in the same round. A reference
// outside a depletion must read a clean 0, and — the trap the $swing.* resolver already guards — a TYPO'd
// field must read 0 too, never fall through to an entity attribute of that name.
func TestDepletionScalarsAreZeroOutsideAHook(t *testing.T) {
	z, s := combatZone(t)
	// An entity attribute named exactly like the typo'd field. If the resolver fell through to the attr
	// lookup, this 999 would be read as the amount.
	z.defs.attr.register("overflw", &attributeDef{ref: "overflw", base: litNode{v: 999}})

	c := &effectCtx{z: z, actor: s.entity, source: s.entity, target: s.entity, mag: 1}

	for _, ref := range []string{"$depletion.overflow", "$depletion.applied", "$depletion.amount"} {
		require.Zero(t, evalCheckFormula(c, attrNode{ref: ref}, s.entity),
			"%s must read 0 outside a depletion ctx — a stale value here would leak across ops", ref)
	}
	require.Zero(t, evalCheckFormula(c, attrNode{ref: "$depletion.overflw"}, s.entity),
		"a typo'd $depletion field must read a clean 0, NOT fall through to the entity attribute of that name")
}

// TestOverflowDoesNotLeakIntoLaterOps is the containment property asserted end-to-end rather than through
// the resolver: after a hook has run with an overflow, an op in the ORIGINAL op-list must still see 0.
func TestOverflowDoesNotLeakIntoLaterOps(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "spilled", "max_spilled", 500, nil, false)
	registerPool(z, "leaked", "max_leaked", 500, nil, false)
	registerPool(z, "stagger", "max_stagger", 100, spillHook("spilled"), false)

	mob := combatMob(z, s.entity, "subject", "", 100)
	setResourceCurrent(mob, "stagger", 10)
	setResourceCurrent(mob, "spilled", 500)
	setResourceCurrent(mob, "leaked", 500)

	// One op-list: empty the pool (whose hook spills 20), THEN read $depletion.overflow from the outer ctx.
	c := &effectCtx{
		z: z, actor: s.entity, source: s.entity, target: mob,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	runOps(c, []effectOp{
		{kind: "deal_damage", resource: "stagger", amount: 30},
		{kind: "deal_damage", resource: "leaked", amount: 0, bonus: attrNode{ref: "$depletion.overflow"}},
	})

	require.Equal(t, 480, resourceCurrent(mob, "spilled"), "precondition: the hook did carry 20")
	require.Equal(t, 500, resourceCurrent(mob, "leaked"),
		"the overflow leaked onto the OUTER ctx — a later op in the same list saw a stale depletion")
}

// --- The two-track system, end to end ------------------------------------------------------------

// TestStunTrackOverflowKillsThroughTheVitalPool is the acceptance case #407 exists for, and the reason
// #406 had to be level-triggered. A non-vital `stagger` track soaks blows; its excess carries into hp,
// which is vital — so a big enough blow onto an ALREADY-empty track kills through the ordinary death seam.
// Both halves matter: the first blow is absorbed and non-lethal, and the pool keeps carrying afterwards
// rather than swallowing damage forever.
func TestStunTrackOverflowKillsThroughTheVitalPool(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "stagger", "max_stagger", 100, spillHook("hp"), false)

	mob := combatMob(z, s.entity, "brute", "", 100)
	room := mob.location
	setResourceCurrent(mob, "stagger", 40)

	// Blow 1: 40 stagger absorbs 40 of a 50 blow; 10 carries into hp. Non-lethal.
	dealTo(z, s.entity, mob, 50, "slash", "stagger")
	require.Equal(t, 0, resourceCurrent(mob, "stagger"), "the track is emptied")
	require.Equal(t, 90, resourceCurrent(mob, "hp"), "only the 10 of overflow reached hp")
	require.NotEqual(t, posDead, position(mob), "a blow the track mostly absorbed must not kill")

	// Blow 2 onto the now-empty track: the WHOLE blow carries into hp and kills through the normal seam.
	dealTo(z, s.entity, mob, 200, "slash", "stagger")
	require.Equal(t, posDead, position(mob),
		"an overflow big enough to empty the vital pool must kill through the ordinary death seam")
	require.Equal(t, 1, countRoomCorpses(room), "exactly one corpse")
}

// TestOverflowCascadeIsBounded is the safety case. Overflow is content-controllable magnitude at a seam
// that can re-enter itself: two pools whose hooks spill into each other are a mutual cascade. Overflow
// never AMPLIFIES on its own (overflow <= the blow), but a content formula could multiply it, so the
// depth/budget bound at the depletion seam has to hold with the new amount source in play.
func TestOverflowCascadeIsBounded(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "alpha", "max_alpha", 100, []effectOp{
		{
			kind: "deal_damage", tgt: "self", resource: "beta", amount: 0,
			bonus: opNode{op: "*", args: []formulaNode{attrNode{ref: "$depletion.overflow"}, litNode{v: 3}}},
		}, // deliberately amplifying
	}, false)
	registerPool(z, "beta", "max_beta", 100, []effectOp{
		{
			kind: "deal_damage", tgt: "self", resource: "alpha", amount: 0,
			bonus: opNode{op: "*", args: []formulaNode{attrNode{ref: "$depletion.overflow"}, litNode{v: 3}}},
		},
	}, false)

	mob := combatMob(z, s.entity, "ouroboros", "", 100)
	setResourceCurrent(mob, "alpha", 5)
	setResourceCurrent(mob, "beta", 5)

	c := &effectCtx{
		z: z, actor: s.entity, source: s.entity, target: mob,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	require.Nil(t, c.eventBudget, "precondition: a nil budget — the seam must bound this on its own")

	done := make(chan struct{})
	go func() {
		defer close(done)
		dealDamage(c, mob, 50, "slash", "alpha")
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("an amplifying overflow cascade did not terminate — the depletion seam's bound does not hold")
	}
	require.NotEqual(t, posDead, position(mob), "no vital pool was touched, so nothing may have died")
}
