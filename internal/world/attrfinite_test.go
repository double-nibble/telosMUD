package world

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// attrfinite_test.go covers the finiteness guard on the ATTRIBUTE MODIFIER FOLD.
//
// resolveAttr guards the attribute's own base FORMULA with evalFinite, but the fold that follows it —
// `(base + flat) * mul` summed and multiplied across every modifier source — had no guard at all. Two
// ordinary content affects (a `mul` modifier is a documented affect shape) therefore drive any
// attribute to ±Inf, and NaN additionally escapes the attribute_def's declared min/max clamp, because
// every comparison against NaN is false.
//
// WHAT IS ACTUALLY REACHABLE, measured rather than assumed: op formulas are already safe, since they
// resolve through evalFinite and a non-finite result collapses to 0. The exposed consumers are the
// ones that read attr() DIRECTLY — resourceMax (`int(attr(...))`, which yields MaxInt64 and an
// unkillable pool) and soak — plus any predicate comparing against a NaN, which silently takes the
// unexpected branch.

// infZone registers an attribute plus two multiplicative affects large enough to overflow float64 when
// composed. Nothing here is exotic content — a `mul` modifier is an ordinary, documented affect shape.
func infZone(t *testing.T) (*Zone, *Entity) {
	t.Helper()
	z, caster := abilityTestZone(t)
	z.defs.attr.register("power", &attributeDef{ref: "power", base: litNode{v: 2}})
	z.defs.affect.register("huge_a", &affectDef{
		ref: "huge_a", name: "Huge A", stacking: stackRefresh, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "power", add: false, value: 1e308}},
	})
	z.defs.affect.register("huge_b", &affectDef{
		ref: "huge_b", name: "Huge B", stacking: stackRefresh, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "power", add: false, value: 1e308}},
	})
	return z, caster.entity
}

// TestModFoldOverflowIsClampedNotInfinite is the headline guard: composing two large multipliers must
// not yield a non-finite attribute.
func TestModFoldOverflowIsClampedNotInfinite(t *testing.T) {
	z, e := infZone(t)
	require.Equal(t, 2.0, attr(e, "power"))

	applyAffect(e, "huge_a", attachOpts{}, nil)
	applyAffect(e, "huge_b", attachOpts{}, nil)

	got := attr(e, "power")
	require.False(t, math.IsInf(got, 0), "the fold overflowed to %v — an infinite attribute", got)
	require.False(t, math.IsNaN(got), "the fold produced NaN")
	require.Equal(t, attrFoldCeiling, got, "an overflowing fold saturates at the engine ceiling")
	_ = z
}

// TestResourceMaxCannotReachMaxInt64 is the CONSEQUENCE test, and it pins the harm that is actually
// reachable rather than the one that is easiest to assume.
//
// The tempting story is "a poisoned attribute in a damage formula becomes int(+Inf) = MaxInt64, an
// instant kill". MEASURED, THAT IS FALSE: every op formula resolves through evalFinite, which rejects
// a non-finite result and yields 0, so a deal_damage bonus reading an infinite attribute contributes
// nothing and the blow lands for its base amount. The formula path was already guarded.
//
// The reachable path is the one that does NOT go through a formula. resourceMax is
// `int(attr(e, def.maxAttr))` — a direct read and an unguarded conversion — so an infinite max_hp
// yields a pool whose maximum is MaxInt64. The harm is the OPPOSITE of a one-shot: an entity with an
// effectively bottomless vital pool is unkillable, and since resourceCurrent treats an absent pool as
// full, it starts there. `soak_<type>` (effect_op.go) is the same shape and grants total immunity.
func TestResourceMaxCannotReachMaxInt64(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	for _, ref := range []string{"bloat_a", "bloat_b"} {
		z.defs.affect.register(ref, &affectDef{
			ref: ref, name: ref, stacking: stackRefresh, maxStacks: 1, duration: 100,
			modifiers: []affectModifier{{attr: "max_hp", add: false, value: 1e308}},
		})
	}
	applyAffect(e, "bloat_a", attachOpts{}, nil)
	applyAffect(e, "bloat_b", attachOpts{}, nil)

	require.False(t, math.IsInf(attr(e, "max_hp"), 0), "the fold must not hand a pool an infinite max")
	got := resourceMax(e, "hp")
	require.NotEqual(t, math.MaxInt64, got,
		"int(+Inf) is MaxInt64 on this platform — an unkillable entity with a bottomless vital pool")
	// A FROZEN bound, tied to the ceiling but not re-derived from it: whatever attrFoldCeiling is, the
	// int it produces must stay far short of wrapping. Raising the ceiling past this is the regression.
	require.Less(t, got, 1<<40)
	require.Positive(t, got, "...while still being a large, visibly-wrong number an operator can spot")
}

// TestSaturatedSoakDoesNotGrantTotalImmunity covers the second direct-attr consumer. soak reads
// `attr(target, "soak_<type>")` without a formula in between, so an infinite value would subtract
// infinity from every blow.
func TestSaturatedSoakDoesNotGrantTotalImmunity(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	z.defs.attr.register("soak_fire", &attributeDef{ref: "soak_fire", base: litNode{v: 1}})
	for _, ref := range []string{"ward_a", "ward_b"} {
		z.defs.affect.register(ref, &affectDef{
			ref: ref, name: ref, stacking: stackRefresh, maxStacks: 1, duration: 100,
			modifiers: []affectModifier{{attr: "soak_fire", add: false, value: 1e308}},
		})
	}
	mob := makeMobTarget(z, e, "goblin")
	applyAffect(mob, "ward_a", attachOpts{}, nil)
	applyAffect(mob, "ward_b", attachOpts{}, nil)

	require.False(t, math.IsInf(soak(mob, "fire"), 0), "soak must stay finite")
	// The saturated soak is still enormous, so the blow is fully absorbed — that is CONTENT's problem
	// and is legitimate behaviour for a huge soak. What must not happen is a non-finite intermediate.
	require.False(t, math.IsNaN(soak(mob, "fire")))
}

// TestClampDoesNotContainNaN documents WHY the guard sits in the fold rather than relying on the
// attribute_def's declared range: a NaN passes straight through a min/max clamp untouched, because
// every comparison against NaN is false. An author who bounded their attribute defensively is not
// protected by that bound.
func TestClampDoesNotContainNaN(t *testing.T) {
	lo, hi := 1.0, 5.0
	nan := math.NaN()

	// The clamp as resolveAttr applies it, reproduced in isolation on a NaN.
	//
	// staticcheck flags these comparisons as always-false, which is EXACTLY the point being
	// demonstrated: that is why a declared range cannot contain a NaN, and why the finiteness screen
	// has to run before the clamp rather than relying on it.
	v := nan
	//nolint:staticcheck // SA4012: the always-false NaN comparison IS the trap under demonstration
	if v < lo {
		v = lo
	}
	//nolint:staticcheck // SA4012: as above — a NaN survives both bounds untouched
	if v > hi {
		v = hi
	}
	require.True(t, math.IsNaN(v), "a declared [1,5] range does NOT contain a NaN — this is the trap")

	// And the guard's own behaviour on the same input.
	require.Equal(t, 0.0, finiteOrFallback(nan, 0), "NaN resolves to the documented fallback")
	require.Equal(t, attrFoldCeiling, finiteOrFallback(math.Inf(1), 0), "+Inf saturates at the ceiling")
	require.Equal(t, -attrFoldCeiling, finiteOrFallback(math.Inf(-1), 0), "-Inf saturates at the floor")
	require.Equal(t, 3.25, finiteOrFallback(3.25, 0), "an ordinary value passes through untouched")
}

// TestFiniteFoldIsUnchangedForOrdinaryContent is the non-regression half: the guard must be invisible
// to every attribute whose modifiers compose to a finite number, which is all existing content.
func TestFiniteFoldIsUnchangedForOrdinaryContent(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	z.defs.attr.register("might", &attributeDef{ref: "might", base: litNode{v: 10}})
	z.defs.affect.register("plus3", &affectDef{
		ref: "plus3", name: "Plus3", stacking: stackRefresh, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "might", add: true, value: 3}},
	})
	z.defs.affect.register("double", &affectDef{
		ref: "double", name: "Double", stacking: stackRefresh, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "might", add: false, value: 2}},
	})

	require.Equal(t, 10.0, attr(e, "might"))
	applyAffect(e, "plus3", attachOpts{}, nil)
	require.Equal(t, 13.0, attr(e, "might"))
	applyAffect(e, "double", attachOpts{}, nil)
	require.Equal(t, 26.0, attr(e, "might"), "(10+3)*2 — the ordinary fold is untouched")
}
