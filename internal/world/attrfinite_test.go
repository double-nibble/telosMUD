package world

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// attrfinite_test.go covers the magnitude bound + degraded-marker on the attribute modifier fold.
//
// # What broke, and the two opposed constraints the fix has to satisfy
//
// resolveAttr computed `(base + flat) * mul` across every modifier source with no bound. Two ordinary
// content affects with a large modifier drive an attribute to ±Inf (1e308 x 1e308), and even one
// finite 1e300 modifier leaves int() to wrap. Two independent reviews established that neither obvious
// remedy is safe ALONE:
//
//   - Failing an overflow CLOSED (resolve to 0, the cycle path's behaviour) is unsafe for a DIRECT
//     reader: 0 on max_hp means resourceMax <= 0, which is the natural-immunity discard AND makes the
//     entity undying. So a direct reader wants a bounded NUMBER.
//   - Saturating to a finite number SILENTLY is unsafe for a FORMULA reader: before any screen, an
//     overflow was +Inf and evalFinite rejected it, so a deal_damage bonus reading a poisoned
//     attribute contributed 0 — the formula path failed closed for free. A legitimate-looking 1e12
//     hands every formula a usable one-shot value. So a formula reader wants the value REFUSED.
//
// The fix does both: attrScreen returns a bounded value AND flags the attribute degraded; direct
// readers get the number, formula readers (evalCheckFormulaErr) refuse the degraded marker.

// foldZone registers an attribute plus affects that drive it out of range. `mulValue`/`addValue`
// select the poisoning shape without a helper explosion.
func foldZone(t *testing.T, attrName string, mods ...affectModifier) (*Zone, *Entity) {
	t.Helper()
	z, caster := abilityTestZone(t)
	if z.defs.attr.get(attrName) == nil {
		z.defs.attr.register(attrName, &attributeDef{ref: attrName, base: litNode{v: 2}})
	}
	for i, m := range mods {
		ref := "poison_" + string(rune('a'+i))
		z.defs.affect.register(ref, &affectDef{
			ref: ref, name: ref, stacking: stackRefresh, maxStacks: 1, duration: 100,
			modifiers: []affectModifier{m},
		})
		applyAffect(caster.entity, ref, attachOpts{}, nil)
	}
	return z, caster.entity
}

// TestAttrScreen is the table test for the screen primitive itself, across the whole input space the
// panel called out: ±Inf, NaN, above/below the ceiling, exactly the ceiling, a non-finite FALLBACK
// (the hole the first attempt had — it returned an infinite fallback verbatim), denormals, and -0.
func TestAttrScreen(t *testing.T) {
	c := attrFoldCeiling
	for _, tc := range []struct {
		name       string
		v, fb      float64
		want       float64
		wantScreen bool
	}{
		{"an ordinary value passes untouched", 42, 0, 42, false},
		{"exactly the ceiling is not screened", c, 0, c, false},
		{"just over the ceiling saturates", c * 1.0001, 0, c, true},
		{"a huge finite value saturates (the case a finiteness-only screen missed)", 1e300, 0, c, true},
		{"a negative over the ceiling saturates toward the floor", -1e300, 0, -c, true},
		{"+Inf saturates at the ceiling", math.Inf(1), 0, c, true},
		{"-Inf saturates at the floor", math.Inf(-1), 0, -c, true},
		{"NaN falls back to a finite fallback", math.NaN(), 7, 7, true},
		{"NaN with a NaN fallback resolves to 0", math.NaN(), math.NaN(), 0, true},
		{"NaN with an INFINITE fallback resolves to 0 (the self-hole)", math.NaN(), math.Inf(1), 0, true},
		{"NaN with an over-ceiling fallback resolves to 0", math.NaN(), 1e300, 0, true},
		{"a denormal is an ordinary value", 5e-324, 0, 5e-324, false},
		{"negative zero passes untouched", math.Copysign(0, -1), 0, math.Copysign(0, -1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, screened := attrScreen(tc.v, tc.fb)
			require.Equal(t, tc.wantScreen, screened, "screened flag")
			if math.IsNaN(tc.want) {
				require.True(t, math.IsNaN(got))
			} else {
				require.Equal(t, tc.want, got)
			}
			require.False(t, math.IsInf(got, 0), "the screen must never RETURN a non-finite value")
			require.False(t, math.IsNaN(got), "the screen must never return NaN")
		})
	}
}

// TestOverflowIsBoundedThroughResolveAttr drives the screen through the real derivation, so the wiring
// (resolveAttr calling attrScreen and threading the flag out) is pinned — not just the helper.
func TestOverflowIsBoundedThroughResolveAttr(t *testing.T) {
	t.Run("multiplicative overflow to Inf", func(t *testing.T) {
		z, e := foldZone(t, "power",
			affectModifier{attr: "power", add: false, value: 1e308},
			affectModifier{attr: "power", add: false, value: 1e308})
		require.Equal(t, attrFoldCeiling, attr(e, "power"))
		require.True(t, attrIsDegraded(e, "power"))
		_ = z
	})

	t.Run("ONE finite large modifier is bounded too (the F3 case)", func(t *testing.T) {
		// This is the case a finiteness-only screen let through: 1e300 is finite, so a screen that only
		// checked IsInf/IsNaN would pass it and int() would wrap.
		z, e := foldZone(t, "might", affectModifier{attr: "might", add: true, value: 1e300})
		require.Equal(t, attrFoldCeiling, attr(e, "might"))
		require.True(t, attrIsDegraded(e, "might"))
		_ = z
	})

	t.Run("an ordinary finite fold is untouched and not degraded", func(t *testing.T) {
		z, caster := abilityTestZone(t)
		e := caster.entity
		z.defs.attr.register("might", &attributeDef{ref: "might", base: litNode{v: 10}})
		z.defs.affect.register("plus3", &affectDef{
			ref: "plus3", duration: 100, maxStacks: 1,
			modifiers: []affectModifier{{attr: "might", add: true, value: 3}},
		})
		z.defs.affect.register("double", &affectDef{
			ref: "double", duration: 100, maxStacks: 1,
			modifiers: []affectModifier{{attr: "might", add: false, value: 2}},
		})
		applyAffect(e, "plus3", attachOpts{}, nil)
		applyAffect(e, "double", attachOpts{}, nil)
		require.Equal(t, 26.0, attr(e, "might"), "(10+3)*2 — an ordinary fold is untouched")
		require.False(t, attrIsDegraded(e, "might"), "and not flagged degraded")
		_ = z
	})
}

// TestNaNFoldFallsBackToBaseNotZero pins the fallback SEMANTIC — the guard the first attempt left
// unmutated. A `mul: 0` nullify over an overflowed additive is `(Inf + flat) * 0 = NaN`, and the
// screen must fall back to the pre-modifier base rather than to 0 (0 would drop the attribute to
// nothing, silently defeating any downstream reader). Base itself is finite here.
func TestNaNFoldFallsBackToBase(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	z.defs.attr.register("might", &attributeDef{ref: "might", base: litNode{v: 40}})
	z.defs.affect.register("bloat", &affectDef{
		ref: "bloat", duration: 100, maxStacks: 1,
		modifiers: []affectModifier{{attr: "might", add: true, value: math.MaxFloat64}},
	})
	z.defs.affect.register("bloat2", &affectDef{
		ref: "bloat2", duration: 100, maxStacks: 1,
		modifiers: []affectModifier{{attr: "might", add: true, value: math.MaxFloat64}},
	})
	z.defs.affect.register("nullify", &affectDef{
		ref: "nullify", duration: 100, maxStacks: 1,
		modifiers: []affectModifier{{attr: "might", add: false, value: 0}},
	})
	applyAffect(e, "bloat", attachOpts{}, nil)
	applyAffect(e, "bloat2", attachOpts{}, nil) // additive part overflows to +Inf
	applyAffect(e, "nullify", attachOpts{}, nil)

	got := attr(e, "might")
	require.False(t, math.IsNaN(got), "the fold went NaN via (Inf + flat) * 0; must not surface")
	require.Equal(t, 40.0, got, "NaN falls back to the pre-modifier base, not to 0")
	require.True(t, attrIsDegraded(e, "might"))
}

// TestScreenBeforeClampThenBackstopAfter pins the ORDERING the panel proved was untested. The NaN/Inf
// screen runs BEFORE the declared clamp (so the fallback is re-clamped into the author's range), and a
// magnitude backstop runs AFTER it (so a declared max larger than the ceiling cannot reintroduce an
// unbounded value). Both directions are asserted, through resolveAttr, not a mirrored copy of it.
func TestScreenBeforeClampThenBackstopAfter(t *testing.T) {
	t.Run("a NaN fold is re-clamped into a declared range", func(t *testing.T) {
		z, caster := abilityTestZone(t)
		e := caster.entity
		lo, hi := 1.0, 5.0
		z.defs.attr.register("skill", &attributeDef{ref: "skill", base: litNode{v: 3}, min: &lo, max: &hi})
		z.defs.affect.register("inf", &affectDef{
			ref: "inf", duration: 100, maxStacks: 1,
			modifiers: []affectModifier{{attr: "skill", add: true, value: math.MaxFloat64}},
		})
		z.defs.affect.register("inf2", &affectDef{
			ref: "inf2", duration: 100, maxStacks: 1,
			modifiers: []affectModifier{{attr: "skill", add: true, value: math.MaxFloat64}},
		})
		z.defs.affect.register("null", &affectDef{
			ref: "null", duration: 100, maxStacks: 1,
			modifiers: []affectModifier{{attr: "skill", add: false, value: 0}},
		})
		applyAffect(e, "inf", attachOpts{}, nil)
		applyAffect(e, "inf2", attachOpts{}, nil)
		applyAffect(e, "null", attachOpts{}, nil)
		// NaN -> fallback base 3 -> clamped into [1,5] -> 3. If the screen ran AFTER the clamp, the NaN
		// would slip through the clamp untouched (every NaN comparison is false) and surface.
		require.Equal(t, 3.0, attr(e, "skill"))
		require.False(t, math.IsNaN(attr(e, "skill")))
	})

	t.Run("backstop catches a value a declared MIN raised above the ceiling", func(t *testing.T) {
		// The case ONLY the post-clamp backstop catches: the fold is small and in range, so the pre-clamp
		// screen passes it untouched, but a declared `min` larger than the ceiling then RAISES it past the
		// bound. Without the backstop after the clamp, this attribute escapes at 1e13.
		z, caster := abilityTestZone(t)
		e := caster.entity
		bigMin := 1e13
		z.defs.attr.register("floored", &attributeDef{ref: "floored", base: litNode{v: 5}, min: &bigMin})
		require.Equal(t, attrFoldCeiling, attr(e, "floored"),
			"a declared min above the ceiling is itself clamped by the backstop")
		require.True(t, attrIsDegraded(e, "floored"))
	})
}

// TestDegradedFlagIsClearedWithTheCache pins that the degraded marker does not outlive the derivation
// that produced it: once the poisoning affect expires and the cache is dirtied, the attribute recomputes
// clean and is no longer degraded. A stale marker would keep a formula refusing a now-healthy
// attribute forever — the mirror hazard of never setting it.
func TestDegradedFlagIsClearedWithTheCache(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	z.defs.attr.register("power", &attributeDef{ref: "power", base: litNode{v: 2}})
	z.defs.affect.register("poison", &affectDef{
		ref: "poison", duration: 100, maxStacks: 1,
		modifiers: []affectModifier{{attr: "power", add: true, value: 1e300}},
	})
	applyAffect(e, "poison", attachOpts{}, nil)
	require.True(t, attrIsDegraded(e, "power"), "poisoned -> degraded")

	// Expire the affect; the tick/expiry dirties the cache.
	a, _ := Get[*Affected](e)
	for _, inst := range append([]*affectInstance(nil), a.list...) {
		a.expire(e, inst, nil)
	}
	require.Equal(t, 2.0, attr(e, "power"), "the attribute recomputes to its clean base")
	require.False(t, attrIsDegraded(e, "power"),
		"the degraded marker must be cleared with the cache, not linger after the poison is gone")
}

// TestDerivedOfDegradedIsAlsoDegraded pins the propagation: an attribute DERIVED from a screened one is
// itself untrustworthy, so the marker rides through derivation. Without it, `armour = soak` would
// launder a screened soak into a clean attribute that a formula would then trust.
func TestDerivedOfDegradedIsAlsoDegraded(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	z.defs.attr.register("soak_raw", &attributeDef{ref: "soak_raw", base: litNode{v: 1}})
	z.defs.attr.register("armour", &attributeDef{ref: "armour", base: opNode{op: "*", args: []formulaNode{attrNode{ref: "soak_raw"}, litNode{v: 1}}}})
	z.defs.affect.register("corrupt", &affectDef{
		ref: "corrupt", duration: 100, maxStacks: 1,
		modifiers: []affectModifier{{attr: "soak_raw", add: true, value: 1e300}},
	})
	applyAffect(e, "corrupt", attachOpts{}, nil)

	require.True(t, attrIsDegraded(e, "soak_raw"), "the leaf is degraded")
	require.Equal(t, attrFoldCeiling, attr(e, "armour"), "the derived value is bounded and finite")
	require.True(t, attrIsDegraded(e, "armour"),
		"a value DERIVED from a degraded attribute is itself degraded — else the marker laundered away")
}

// TestInductiveBoundHoldsAcrossDerivation is the argument for the ceiling being below sqrt(2^63): even
// B = A*A, with A screened at the ceiling, stays finite and re-bounded rather than blowing int64.
func TestInductiveBoundHoldsAcrossDerivation(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	z.defs.attr.register("a", &attributeDef{ref: "a", base: litNode{v: 1}})
	z.defs.attr.register("b", &attributeDef{ref: "b", base: opNode{op: "*", args: []formulaNode{attrNode{ref: "a"}, attrNode{ref: "a"}}}})
	z.defs.affect.register("poison", &affectDef{
		ref: "poison", duration: 100, maxStacks: 1,
		modifiers: []affectModifier{{attr: "a", add: true, value: 1e300}},
	})
	applyAffect(e, "poison", attachOpts{}, nil)

	require.Equal(t, attrFoldCeiling, attr(e, "a"))
	// b's formula evaluates a=1e12, so a*a=1e24 — finite, so evalFinite passes it — and b's own screen
	// pulls it back to the ceiling. Every level re-bounds.
	require.Equal(t, attrFoldCeiling, attr(e, "b"))
	require.Less(t, attr(e, "b"), float64(1)*(1<<62), "must stay well inside int64")
}

// --- Consequence tests: the two opposed constraints, each measured at a real consumer -------------

// TestDirectReaderGetsABoundedNumber is the OWNING-ENGINEER half: resourceMax reads attr() directly
// via int(), and a poisoned max_hp must yield a large bounded pool — NOT MaxInt64 (arm64) or MinInt64
// (amd64, which is <= 0 and hits the natural-immunity discard). Erroring to 0 would be the immunity
// bug; a bounded number is the fix.
func TestDirectReaderGetsABoundedNumber(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	z.defs.affect.register("bloat", &affectDef{
		ref: "bloat", duration: 100, maxStacks: 1,
		modifiers: []affectModifier{{attr: "max_hp", add: true, value: 1e300}},
	})
	applyAffect(e, "bloat", attachOpts{}, nil)

	got := resourceMax(e, "hp")
	require.Positive(t, got, "must be a positive pool, not MinInt64 (which would read as immune/undying)")
	require.Less(t, got, 1<<41, "and bounded well short of an int64 wrap on any architecture")
	require.Equal(t, int(attrFoldCeiling), got, "int(ceiling) exactly")
}

// TestFormulaReaderRefusesADegradedAttribute is the SECURITY half: a deal_damage bonus reading a
// poisoned attribute must contribute 0, not a bounded-but-usable one-shot. This is the property the
// silent-saturation attempt DESTROYED — it dealt 1e12 damage where this asserts the blow lands for its
// base amount.
func TestFormulaReaderRefusesADegradedAttribute(t *testing.T) {
	z, e := foldZone(t, "power", affectModifier{attr: "power", add: false, value: 1e308},
		affectModifier{attr: "power", add: false, value: 1e308})
	require.True(t, attrIsDegraded(e, "power"))

	mob := makeMobTarget(z, e, "goblin")
	setResourceCurrent(mob, "hp", 100)
	c := seededCtx(z, e, mob, dispHarmful)
	// The documented "flat amount + scoped attribute bonus" damage shape.
	op := &effectOp{kind: "deal_damage", dmgType: "fire", amount: 5, bonus: attrNode{ref: "$actor.power"}}
	require.NoError(t, opDealDamage(c, op))

	require.Equal(t, 5, c.lastDamage,
		"the degraded bonus must contribute 0 — the blow lands for its base amount, NOT a one-shot")
	require.Equal(t, 95, resourceCurrent(mob, "hp"))
}

// TestBaseFormulaOverflowIsScreenedNotZeroed pins the seam both #557 reviews flagged: a base FORMULA
// that overflows must be BOUNDED and marked degraded, not errored to 0. Zero on a direct-read attr
// like max_hp is the undying/immunity path this whole change exists to prevent, so the base seam must
// behave like the fold seam.
func TestBaseFormulaOverflowIsScreenedNotZeroed(t *testing.T) {
	t.Run("a base formula overflowing to +Inf is bounded", func(t *testing.T) {
		z, caster := abilityTestZone(t)
		e := caster.entity
		// max_hp base = 1e300 * 1e300 -> +Inf. Must screen to the ceiling, not error to 0.
		z.defs.attr.register("max_hp", &attributeDef{
			ref:  "max_hp",
			base: opNode{op: "*", args: []formulaNode{litNode{v: 1e300}, litNode{v: 1e300}}},
		})
		require.Equal(t, attrFoldCeiling, attr(e, "max_hp"), "an overflowing base is bounded, not 0")
		require.True(t, attrIsDegraded(e, "max_hp"))
		require.Positive(t, resourceMax(e, "hp"), "resourceMax must be positive, not 0 (undying)")
	})

	t.Run("a division taming to Inf is bounded", func(t *testing.T) {
		z, caster := abilityTestZone(t)
		e := caster.entity
		z.defs.attr.register("tiny", &attributeDef{ref: "tiny", base: litNode{v: 1e-300}})
		z.defs.attr.register("huge", &attributeDef{
			ref:  "huge",
			base: opNode{op: "/", args: []formulaNode{litNode{v: 1e300}, attrNode{ref: "tiny"}}},
		})
		require.Equal(t, attrFoldCeiling, attr(e, "huge"), "1e300/1e-300 overflows and is bounded")
		require.True(t, attrIsDegraded(e, "huge"))
	})

	t.Run("a genuine base error (div by zero) still resolves to 0", func(t *testing.T) {
		z, caster := abilityTestZone(t)
		e := caster.entity
		z.defs.attr.register("zero", &attributeDef{ref: "zero", base: litNode{v: 0}})
		z.defs.attr.register("bad", &attributeDef{
			ref:  "bad",
			base: opNode{op: "/", args: []formulaNode{litNode{v: 5}, attrNode{ref: "zero"}}},
		})
		require.Equal(t, 0.0, attr(e, "bad"), "a div-by-zero is a real error and resolves to 0")
		require.False(t, attrIsDegraded(e, "bad"), "an error is not a screen — not degraded")
	})
}

// TestGrantRefusesADegradedSeed pins Finding 1: modify_attribute_base must not SNAPSHOT a degraded
// value into a permanent base, which would launder the poison into a clean, formula-trusted number.
func TestGrantRefusesADegradedSeed(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	// power is degraded by an overflowing fold; scaled_power's base reads it.
	z.defs.attr.register("power", &attributeDef{ref: "power", base: litNode{v: 2}})
	z.defs.attr.register("scaled_power", &attributeDef{ref: "scaled_power", base: attrNode{ref: "power"}})
	for _, ref := range []string{"h1", "h2"} {
		z.defs.affect.register(ref, &affectDef{
			ref: ref, duration: 100, maxStacks: 1,
			modifiers: []affectModifier{{attr: "power", add: false, value: 1e308}},
		})
	}
	applyAffect(e, "h1", attachOpts{}, nil)
	applyAffect(e, "h2", attachOpts{}, nil)
	require.True(t, attrIsDegraded(e, "scaled_power"), "scaled_power derives from the degraded power")

	c := seededCtx(z, e, e, dispNeutral)
	require.NoError(t, opModifyAttributeBase(c, &effectOp{kind: "modify_attribute_base", attr: "scaled_power", amount: 0}))

	// The grant must have been REFUSED: no explicit base override was written, so the attribute still
	// derives (degraded) rather than snapshotting a clean 1e12 the next formula would trust.
	require.True(t, attrIsDegraded(e, "scaled_power"),
		"a refused grant leaves the attribute degraded; a laundered snapshot would read clean")
	_, hasOverride := e.living.attrBase["scaled_power"]
	require.False(t, hasOverride, "no permanent base override may be written from a degraded seed")
}
