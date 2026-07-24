package world

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// bandwhen_test.go covers the band STATE predicate (#513) — the `when` axis that lets an outcome be
// selected by an entity's condition rather than by the roll, which is what makes auto-crit and
// auto-fail-save authorable.
//
// The issue claimed both were IMPOSSIBLE today. A premise-verification panel disproved that: a
// sentinel band edge (`margin_min: 0 + 1e9*$target.autofail`) already produces a guaranteed auto-fail,
// and `min: 1e6*(1 - $target.helpless)` a guaranteed auto-crit. What it also proved is WHY those are
// not good enough — every sentinel encoding is AFFINE in the flag attribute, so it mis-fires the
// moment two sources set the flag. TestSentinelEncodingBreaksOnStacking below reproduces that, and is
// the actual justification for this primitive.

func whenZone(t *testing.T) (*Zone, *session, *Entity) {
	t.Helper()
	z, caster := abilityTestZone(t)
	for _, a := range []string{"helpless", "autofail_save"} {
		z.defs.attr.register(a, &attributeDef{ref: a})
	}
	// A condition that STACKS — the case the sentinel hack cannot survive.
	z.defs.affect.register("paralyzed", &affectDef{
		ref: "paralyzed", name: "Paralyzed", stacking: stackCount, maxStacks: 5, duration: 100,
		modifiers: []affectModifier{
			{attr: "helpless", add: true, value: 1},
			{attr: "autofail_save", add: true, value: 1},
		},
	})
	return z, caster, makeMobTarget(z, caster.entity, "goblin")
}

// TestSentinelEncodingBreaksOnStacking is the DISPROOF-OF-THE-ALTERNATIVE test: it pins the defect in
// the workaround this primitive replaces, so the primitive's existence stays justified in the record.
// A sentinel band edge is affine in the flag, so a second stack overshoots and the band mis-fires.
func TestSentinelEncodingBreaksOnStacking(t *testing.T) {
	z, caster, mob := whenZone(t)

	// The symmetric sentinel: fire only when helpless == 1. `max: 1e9*(helpless-1)` is 0 at one stack
	// (so a roll of 1 is <= 0? no — it is the shape an author reaches for, and it is wrong at 2).
	sentinel := &checkSpec{
		dice: mustDice(t, "1d1"), // deterministic total of 1
		bands: []checkBand{
			{max: opFormula("*", litNode{v: 1e9},
				opFormula("-", attrNode{ref: "$target.helpless"}, litNode{v: 1})), label: "forced"},
			{label: "normal"},
		},
	}
	c := checkCtx(z, caster.entity, caster.entity, mob)

	require.Equal(t, "normal", resolveCheck(c, sentinel).bandLabel, "no stacks: not forced")

	applyAffect(mob, "paralyzed", attachOpts{}, nil) // helpless == 1 -> edge is 0, total 1 > 0
	require.Equal(t, "normal", resolveCheck(c, sentinel).bandLabel)

	applyAffect(mob, "paralyzed", attachOpts{}, nil) // helpless == 2 -> edge is 1e9, total 1 <= it
	require.Equal(t, "forced", resolveCheck(c, sentinel).bandLabel,
		"THE DEFECT: a second stack of the same condition flips a sentinel band that must not fire")
}

// TestWhenPredicateIsStackSafe is the same scenario expressed with the primitive: truthiness does not
// care how many sources set the flag, so every stack count behaves identically.
func TestWhenPredicateIsStackSafe(t *testing.T) {
	z, caster, mob := whenZone(t)
	spec := &checkSpec{
		dice: mustDice(t, "1d1"),
		bands: []checkBand{
			{when: attrNode{ref: "$target.helpless"}, label: "forced"},
			{label: "normal"},
		},
	}
	c := checkCtx(z, caster.entity, caster.entity, mob)

	require.Equal(t, "normal", resolveCheck(c, spec).bandLabel)
	for stacks := 1; stacks <= 5; stacks++ {
		applyAffect(mob, "paralyzed", attachOpts{}, nil)
		require.Equal(t, "forced", resolveCheck(c, spec).bandLabel,
			"stack %d must behave exactly like stack 1", stacks)
	}
}

// TestWhenIsAndedNotOverriding pins the semantic that keeps this a band predicate rather than a new
// engine concept: `when` is a FIFTH AXIS, ANDed with the others exactly as they are ANDed with each
// other. It does not force, override, or reorder anything — content gets its forcing behaviour purely
// from where it places a when-only band.
func TestWhenIsAndedNotOverriding(t *testing.T) {
	z, caster, mob := whenZone(t)
	c := checkCtx(z, caster.entity, caster.entity, mob)

	twenty := 20.0
	// A band with BOTH a face test and a state predicate: it must require both.
	spec := &checkSpec{
		dice: mustDice(t, "1d1"), // always rolls 1, so face_eq:20 never holds
		bands: []checkBand{
			{faceEq: &twenty, when: attrNode{ref: "$target.helpless"}, label: "both"},
			{label: "neither"},
		},
	}
	require.Equal(t, "neither", resolveCheck(c, spec).bandLabel, "no state, no face")
	applyAffect(mob, "paralyzed", attachOpts{}, nil)
	require.Equal(t, "neither", resolveCheck(c, spec).bandLabel,
		"the state predicate alone must NOT satisfy a band that also carries a face test")
}

// TestAutoCritAndAutoFailAreAuthorable is the acceptance test for the two behaviours #513 exists to
// serve, each expressed as ordinary content: a when-only band placed above the natural ones.
func TestAutoCritAndAutoFailAreAuthorable(t *testing.T) {
	twenty := 20.0

	t.Run("auto-crit on a landing hit against a helpless target", func(t *testing.T) {
		// A FRESH fixture per subtest: these apply affects, and sharing one entity across subtests
		// silently carries a condition into the next case (which is how this test first went red).
		z, caster, mob := whenZone(t)
		toHit := &checkSpec{
			dice: mustDice(t, "10d1"), // total 10: a comfortable hit, never a natural 20
			vs:   checkVs{dc: litNode{v: 5}},
			bands: []checkBand{
				{when: attrNode{ref: "$target.helpless"}, marginMin: bn(0), label: "crit"},
				{faceEq: &twenty, label: "crit"},
				{marginMin: bn(0), label: "hit"},
				{label: "miss"},
			},
		}
		c := checkCtx(z, caster.entity, caster.entity, mob)
		require.Equal(t, "hit", resolveCheck(c, toHit).bandLabel, "an ordinary target takes an ordinary hit")

		applyAffect(mob, "paralyzed", attachOpts{}, nil)
		require.Equal(t, "crit", resolveCheck(c, toHit).bandLabel,
			"every landing hit on a helpless target crits, with no natural 20 involved")

		// The marginMin on the forced band is load-bearing: a MISS must still miss.
		missing := &checkSpec{
			dice: mustDice(t, "1d1"), vs: checkVs{dc: litNode{v: 50}},
			bands: toHit.bands,
		}
		require.Equal(t, "miss", resolveCheck(c, missing).bandLabel,
			"auto-crit applies to a hit that LANDS — it must not turn a miss into a crit")
	})

	t.Run("auto-fail a save", func(t *testing.T) {
		z, caster, mob := whenZone(t)
		save := &checkSpec{
			dice: mustDice(t, "20d1"), // total 20: would comfortably clear the DC
			vs:   checkVs{dc: litNode{v: 10}},
			bands: []checkBand{
				{when: attrNode{ref: "autofail_save"}, label: "fail"},
				{marginMin: bn(0), label: "success"},
				{label: "fail"},
			},
		}
		// The SAVER rolls, so bare refs must resolve against them: bind the mob as the actor.
		c := checkCtx(z, mob, caster.entity, caster.entity)
		require.Equal(t, "success", resolveCheck(c, save).bandLabel)

		applyAffect(mob, "paralyzed", attachOpts{}, nil)
		require.Equal(t, "fail", resolveCheck(c, save).bandLabel,
			"a guaranteed auto-fail, regardless of how good the roll was")
	})
}

// TestWhenPredicateTruthiness pins the truthiness rule directly, including the non-finite cases. NaN
// is the one that matters: `NaN != 0` is TRUE in Go, so a naive predicate would FIRE a forced band on
// an attribute poisoned by a modifier fold.
func TestWhenPredicateTruthiness(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    float64
		want bool
	}{
		{"zero is false", 0, false},
		{"one is true", 1, true},
		{"a fraction is true", 0.5, true},
		{"negative is true (non-zero)", -3, true},
		{"NaN is FALSE despite NaN != 0", math.NaN(), false},
		{"+Inf is false", math.Inf(1), false},
		{"-Inf is false", math.Inf(-1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, truthyPredicate(tc.v))
		})
	}
}

// TestWhenErrorFailsSafe pins the direction a broken predicate degrades in. A formula error evaluates
// to 0 through evalCheckFormula, so the band is SKIPPED and the roll decides — an authoring bug loses
// the forced outcome rather than applying it unconditionally, which is the safe direction for a
// primitive whose whole purpose is to bypass the dice.
func TestWhenErrorFailsSafe(t *testing.T) {
	z, caster, mob := whenZone(t)
	spec := &checkSpec{
		dice: mustDice(t, "1d1"),
		bands: []checkBand{
			{when: opFormula("/", litNode{v: 1}, litNode{v: 0}), label: "forced"},
			{label: "normal"},
		},
	}
	c := checkCtx(z, caster.entity, caster.entity, mob)
	require.Equal(t, "normal", resolveCheck(c, spec).bandLabel,
		"a predicate that cannot be evaluated must not select its band")
}

// TestWhenParsesFromContent is the parser->behaviour wiring test. Every other test here hand-builds a
// checkBand, which cannot catch the `when` key landing on the wrong field — the exact mutation that
// survived a full package run on the sibling boon/bane work.
func TestWhenParsesFromContent(t *testing.T) {
	z, caster, mob := whenZone(t)
	spec, err := parseCheckSpec(map[string]any{
		"dice": "1d1",
		"bands": []any{
			map[string]any{"label": "forced", "when": []any{"attr", "$target.helpless"}},
			map[string]any{"label": "normal"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, spec.bands[0].when)
	require.Nil(t, spec.bands[1].when)

	c := checkCtx(z, caster.entity, caster.entity, mob)
	require.Equal(t, "normal", resolveCheck(c, spec).bandLabel)
	applyAffect(mob, "paralyzed", attachOpts{}, nil)
	require.Equal(t, "forced", resolveCheck(c, spec).bandLabel,
		"the parsed `when` key must drive band selection")
}

// TestWhenComposesWithTheBoonChannel pins that the two #511/#513 primitives are independent: a forced
// band is selected regardless of which die the boon/bane channel picked, and a boon does not rescue a
// roller whose state has forced a failure.
func TestWhenComposesWithTheBoonChannel(t *testing.T) {
	z, caster, mob := whenZone(t)
	z.defs.attr.register("atk_boon", &attributeDef{ref: "atk_boon"})
	z.defs.affect.register("bless", &affectDef{
		ref: "bless", name: "Blessed", stacking: stackRefresh, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "atk_boon", add: true, value: 1}},
	})
	boonD := mustDice(t, "20d1") // the boon die totals 20 vs the neutral 1
	spec := &checkSpec{
		dice: mustDice(t, "1d1"), boonDice: &boonD,
		boon: attrNode{ref: "atk_boon"},
		vs:   checkVs{dc: litNode{v: 10}},
		bands: []checkBand{
			{when: attrNode{ref: "autofail_save"}, label: "fail"},
			{marginMin: bn(0), label: "success"},
			{label: "fail"},
		},
	}
	c := checkCtx(z, mob, caster.entity, caster.entity) // the mob rolls

	require.Equal(t, "fail", resolveCheck(c, spec).bandLabel, "neutral die of 1 vs DC 10")
	applyAffect(mob, "bless", attachOpts{}, nil)
	require.Equal(t, "success", resolveCheck(c, spec).bandLabel, "the boon die of 20 clears the DC")

	applyAffect(mob, "paralyzed", attachOpts{}, nil)
	require.Equal(t, "fail", resolveCheck(c, spec).bandLabel,
		"a forced band outranks a boon: the roll was still 20, and it still fails")
}
