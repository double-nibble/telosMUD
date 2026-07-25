package world

// ladder_test.go exercises the #541 stacking primitives: a graded LADDER affect (exhaustion) whose each
// rung carries its OWN, non-linear modifier set, moved by increment_rung/decrement_rung; and a
// HIGHEST-WINS stacking mode where duplicate instances across sources take the strongest, not the sum.

import (
	"math/rand"
	"testing"
)

// exhaustionZone registers a 4-rung ladder (each rung a DISTINCT, non-linear effect) on the affectTestZone.
func exhaustionZone(t *testing.T) (*Zone, *Entity) {
	z, e := affectTestZone(t)
	z.defs.affect.register("exhaustion", &affectDef{
		ref: "exhaustion", name: "Exhausted", stacking: stackIgnore, maxStacks: 1, scopeTarget: true, duration: 1000,
		rungs: []affectRung{
			{modifiers: []affectModifier{{attr: "strength", add: true, value: -1}}},                          // rung 1: -1 str
			{modifiers: []affectModifier{{attr: "strength", add: true, value: -4}}},                          // rung 2: -4 (non-linear, not -2)
			{modifiers: []affectModifier{{attr: "max_hp", add: false, value: 0.5}}},                          // rung 3: HALF max hp
			{modifiers: []affectModifier{{attr: "max_hp", add: false, value: 0}}, prevents: []string{"act"}}, // rung 4: 0 hp + can't act
		},
	})
	return z, e
}

// TestLadderRungDistinctEffects proves increment_rung walks the rungs and each applies its OWN set (not a
// scaled single debuff): -1 str, then -4 str, then half max_hp, then 0 max_hp + prevents act.
func TestLadderRungDistinctEffects(t *testing.T) {
	z, e := exhaustionZone(t)
	c := seededCtx(z, e, e, dispHarmful)
	op := &effectOp{kind: "increment_rung", affect: "exhaustion"}

	opIncrementRung(c, op) // rung 1
	if got := attr(e, "strength"); got != 9 {
		t.Fatalf("rung 1 strength = %v, want 9 (base 10 - 1)", got)
	}
	opIncrementRung(c, op) // rung 2
	if got := attr(e, "strength"); got != 6 {
		t.Fatalf("rung 2 strength = %v, want 6 (base 10 - 4, NOT cumulative -1-2)", got)
	}
	opIncrementRung(c, op) // rung 3: half max hp, strength back to base
	if got := attr(e, "max_hp"); got != 50 {
		t.Fatalf("rung 3 max_hp = %v, want 50 (halved)", got)
	}
	if got := attr(e, "strength"); got != 10 {
		t.Fatalf("rung 3 strength = %v, want 10 (rung 3 has no str modifier)", got)
	}
	opIncrementRung(c, op) // rung 4: 0 max hp + prevents act
	if got := attr(e, "max_hp"); got != 0 {
		t.Fatalf("rung 4 max_hp = %v, want 0", got)
	}
	if !preventsTag(e, "act") {
		t.Fatal("rung 4 should prevent act")
	}
	// Incrementing past the top clamps (no overflow).
	opIncrementRung(c, op)
	if got := attr(e, "max_hp"); got != 0 {
		t.Fatalf("past-top max_hp = %v, want 0 (clamped at rung 4)", got)
	}
}

// TestLadderDecrementRecovers proves decrement_rung walks back down and removes the affect below rung 1.
func TestLadderDecrementRecovers(t *testing.T) {
	z, e := exhaustionZone(t)
	c := seededCtx(z, e, e, dispHarmful)
	inc := &effectOp{kind: "increment_rung", affect: "exhaustion"}
	dec := &effectOp{kind: "decrement_rung", affect: "exhaustion"}
	opIncrementRung(c, inc) // rung 1
	opIncrementRung(c, inc) // rung 2 (-4 str)
	if attr(e, "strength") != 6 {
		t.Fatalf("precondition rung 2 strength = %v, want 6", attr(e, "strength"))
	}
	opDecrementRung(c, dec) // rung 1 (-1 str)
	if got := attr(e, "strength"); got != 9 {
		t.Fatalf("after decrement strength = %v, want 9 (rung 1)", got)
	}
	opDecrementRung(c, dec) // below rung 1 -> fully recovered (removed)
	if hasAffect(e, "exhaustion") {
		t.Fatal("decrementing below rung 1 must remove the affect (fully recovered)")
	}
	if got := attr(e, "strength"); got != 10 {
		t.Fatalf("recovered strength = %v, want base 10", got)
	}
}

// TestLadderIncrementByAmount proves the amount arg jumps multiple rungs, applying if absent.
func TestLadderIncrementByAmount(t *testing.T) {
	z, e := exhaustionZone(t)
	c := seededCtx(z, e, e, dispHarmful)
	opIncrementRung(c, &effectOp{kind: "increment_rung", affect: "exhaustion", amount: 2}) // straight to rung 2
	if got := attr(e, "strength"); got != 6 {
		t.Fatalf("increment by 2 strength = %v, want 6 (rung 2)", got)
	}
}

// TestLadderRungPersists proves the rung round-trips through save/load.
func TestLadderRungPersists(t *testing.T) {
	z, e := exhaustionZone(t)
	c := seededCtx(z, e, e, dispHarmful)
	opIncrementRung(c, &effectOp{kind: "increment_rung", affect: "exhaustion", amount: 3}) // rung 3
	dump := dumpAffects(e)
	if len(dump) != 1 || dump[0].Rung != 3 {
		t.Fatalf("dumpAffects rung = %+v, want rung 3", dump)
	}
	// Reattach onto a fresh entity in the same zone.
	e2 := newTestPlayerEntity(z, "Hero2").entity
	applyAffect(e2, "exhaustion", attachOpts{rung: dump[0].Rung, duration: dump[0].Remaining, reattach: true}, nil)
	if got := attr(e2, "max_hp"); got != 50 {
		t.Fatalf("reattached rung-3 max_hp = %v, want 50 (rung restored)", got)
	}
}

// --- Highest-wins ------------------------------------------------------------------------------

// TestHighestWinsNonSumming proves two instances of a highest-wins affect (different sources) apply only
// the STRONGEST, not the sum.
func TestHighestWinsNonSumming(t *testing.T) {
	z, e := affectTestZone(t)
	z.defs.affect.register("bless", &affectDef{
		ref: "bless", name: "Blessed", stacking: stackHighest, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "strength", add: true, value: 1}}, // +1 x magnitude
	})
	a := makeMobTarget(z, e, "casterA")
	b := makeMobTarget(z, e, "casterB")
	applyAffect(e, "bless", attachOpts{source: a, magnitude: 2}, nil) // +2
	applyAffect(e, "bless", attachOpts{source: b, magnitude: 3}, nil) // +3 (stronger)
	if got := attr(e, "strength"); got != 13 {
		t.Fatalf("highest-wins strength = %v, want 13 (base 10 + strongest 3, NOT 10+2+3)", got)
	}
	// Remove the stronger -> the weaker takes over.
	af, _ := Get[*Affected](e)
	def := z.defs.affect.get("bless")
	af.expire(e, af.byKey[keyFor(def, b)], nil)
	if got := attr(e, "strength"); got != 12 {
		t.Fatalf("after removing the stronger, strength = %v, want 12 (base 10 + weaker 2)", got)
	}
}

// TestLadderRungHarmClassified proves the HIGH review fix: a RUNG-ONLY ladder (harm lives in the rungs,
// the top-level modifiers/prevents are empty) is still classified DETRIMENTAL — so a bare apply_affect
// with neutral disposition is harm-gated on a non-consenting player, and the ladder is purged on respawn.
// Without folding rungs into the harm classifier, the ladder would land ungated (the 6th blind spot).
func TestLadderRungHarmClassified(t *testing.T) {
	z, s := combatZone(t)
	z.defs.affect.register("exhaustion", &affectDef{
		ref: "exhaustion", name: "Exhausted", stacking: stackIgnore, maxStacks: 1, scopeTarget: true, duration: 100,
		rungs: []affectRung{{modifiers: []affectModifier{{attr: "strength", add: true, value: -4}}}}, // harm in the RUNG
	})
	def := z.defs.affect.get("exhaustion")
	if !affectIsDetrimental(def, z.harmPolarity()) {
		t.Fatal("a rung-only harmful ladder must be classified detrimental (harm gate)")
	}
	if affectSurvivesRespawn(def, z.harmPolarity()) {
		t.Fatal("a rung-only harmful ladder must NOT survive the respawn purge")
	}
	// End-to-end: a bare apply_affect (neutral disposition) at a non-consenting player is GATED.
	attacker := s.entity
	victim := makePlayerTargetInRoom(z, attacker, "Victim").entity
	c := &effectCtx{z: z, actor: attacker, source: attacker, target: victim, mag: 1, disp: dispNeutral, rng: rand.New(rand.NewSource(1))}
	opApplyAffect(c, &effectOp{kind: "apply_affect", affect: "exhaustion"})
	if hasAffect(victim, "exhaustion") {
		t.Fatal("a rung-only ladder must be harm-gated on apply_affect to a non-consenting player")
	}
}

// TestLadderRungCrossPlayerGated proves BOTH rung ops are gated cross-player (#541 review F1/F3): an
// increment (harm) and a decrement (a cross-player affect strip) on a non-consenting player are no-ops,
// and land only with mutual PvP consent.
func TestLadderRungCrossPlayerGated(t *testing.T) {
	z, s := combatZone(t)
	z.defs.affect.register("exhaustion", &affectDef{
		ref: "exhaustion", name: "Exhausted", stacking: stackIgnore, maxStacks: 1, scopeTarget: true, duration: 1000,
		rungs: []affectRung{{modifiers: []affectModifier{{attr: "strength", add: true, value: -1}}}},
	})
	attacker := s.entity
	victim := makePlayerTargetInRoom(z, attacker, "Victim").entity
	inc := func() {
		c := &effectCtx{z: z, actor: attacker, source: attacker, target: victim, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
		opIncrementRung(c, &effectOp{kind: "increment_rung", affect: "exhaustion"})
	}
	// No consent: increment is a no-op (cross-player harm gated).
	inc()
	if hasAffect(victim, "exhaustion") {
		t.Fatal("cross-player increment_rung must be gated without PvP consent")
	}
	// Mutual consent: it lands.
	setFlag(attacker, flagPvP, true)
	setFlag(victim, flagPvP, true)
	inc()
	if !hasAffect(victim, "exhaustion") {
		t.Fatal("with mutual PvP consent, increment_rung should land")
	}
	// Now revoke consent and prove DECREMENT is also gated (it is a cross-player affect strip): the affect
	// must NOT be removed/reduced without consent.
	setFlag(attacker, flagPvP, false)
	setFlag(victim, flagPvP, false)
	c := &effectCtx{z: z, actor: attacker, source: attacker, target: victim, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	opDecrementRung(c, &effectOp{kind: "decrement_rung", affect: "exhaustion"})
	if !hasAffect(victim, "exhaustion") {
		t.Fatal("cross-player decrement_rung must be gated without consent (it strips a ladder affect)")
	}
}

// TestLadderHighestWinsByRung proves highest-wins over a LADDER selects by RUNG, not magnitude*stacks
// (#541 review F2): a rung-2 (-9) instance wins over a rung-1 (-1) one applied first.
func TestLadderHighestWinsByRung(t *testing.T) {
	z, e := affectTestZone(t)
	z.defs.affect.register("madness", &affectDef{
		ref: "madness", name: "Madness", stacking: stackHighest, maxStacks: 1, duration: 100,
		rungs: []affectRung{
			{modifiers: []affectModifier{{attr: "strength", add: true, value: -1}}},
			{modifiers: []affectModifier{{attr: "strength", add: true, value: -9}}},
		},
	})
	a := makeMobTarget(z, e, "a")
	b := makeMobTarget(z, e, "b")
	applyAffect(e, "madness", attachOpts{source: a}, nil) // rung 1 (applied FIRST)
	ib := applyAffect(e, "madness", attachOpts{source: b}, nil)
	ib.rung = 2 // b's instance is the SEVERE rung
	af, _ := Get[*Affected](e)
	af.recomputeMods()
	markAttrsDirty(e)
	if got := attr(e, "strength"); got != 1 {
		t.Fatalf("highest-wins over a ladder = %v, want 1 (rung 2 = -9 wins over rung 1 = -1, NOT first-applied)", got)
	}
}

// TestHighestRefreshesOnRecast proves a same-(ref,source) re-apply of a highest-wins affect refreshes its
// duration (#541 review F4 — the missing stackHighest case).
func TestHighestRefreshesOnRecast(t *testing.T) {
	z, e := affectTestZone(t)
	z.defs.affect.register("bless", &affectDef{
		ref: "bless", name: "Blessed", stacking: stackHighest, maxStacks: 1, duration: 20,
		modifiers: []affectModifier{{attr: "strength", add: true, value: 1}},
	})
	a := makeMobTarget(z, e, "a")
	inst := applyAffect(e, "bless", attachOpts{source: a}, nil)
	inst.remaining = 5                                  // simulate decay
	applyAffect(e, "bless", attachOpts{source: a}, nil) // re-cast same (ref, source)
	if inst.remaining != 20 {
		t.Fatalf("highest-wins re-cast remaining = %d, want 20 (must refresh like refresh)", inst.remaining)
	}
}

// TestNonHighestStillSums proves the non-summing rule is OPT-IN: an ordinary (refresh/stack) affect from
// two sources still sums, unchanged.
func TestNonHighestStillSums(t *testing.T) {
	z, e := affectTestZone(t)
	a := makeMobTarget(z, e, "a")
	b := makeMobTarget(z, e, "b")
	applyAffect(e, "weaken", attachOpts{source: a}, nil) // -2 str (stackRefresh)
	applyAffect(e, "weaken", attachOpts{source: b}, nil) // -2 str
	if got := attr(e, "strength"); got != 6 {
		t.Fatalf("two-source weaken strength = %v, want 6 (base 10 - 2 - 2; non-highest affects sum)", got)
	}
}
