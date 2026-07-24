package world

import (
	"math"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
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

	// The symmetric sentinel an author reaches for to mean "fire only while helpless": the band edge
	// `max: 1e9*(helpless-1)` sits below the rolled total at one stack (so the band correctly does not
	// match) and leaps above it at two, which is the overshoot.
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
		// A NEGATIVE value is FALSE. This is the load-bearing half of choosing "> 0" over "!= 0": the
		// formula vocabulary has no negation, so "A unless B" is written `A - B`, and under a non-zero
		// rule that expression fires again once B exceeds A — the same affine overshoot that makes
		// sentinel edges unusable. See TestNegatedPredicateIsStackSafe.
		{"negative is FALSE, not true-because-non-zero", -3, false},
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
	forced := resolveCheck(c, spec)
	require.Equal(t, "fail", forced.bandLabel,
		"a forced band outranks a boon: the roll was still 20, and it still fails")
	// Assert the ROLL too. Without this, a regression that stopped selecting the boon die would leave
	// the assertion above green for entirely the wrong reason (a neutral 1 also fails).
	require.Equal(t, 20, forced.roll, "the boon die must still have been rolled")
}

// TestNegatedPredicateIsStackSafe is the concrete reason truthiness is "> 0" rather than "!= 0". The
// formula vocabulary has no `not`, so "fumble on a nat-1 UNLESS you are Lucky" is naturally written
// `1 - lucky`. Under a non-zero rule that predicate is true at -1 as well as at 1, so a SECOND source
// of Lucky turns fumbling back on — the identical affine-overshoot defect that makes sentinel band
// edges unusable, reintroduced through the primitive meant to replace them.
func TestNegatedPredicateIsStackSafe(t *testing.T) {
	z, caster, mob := whenZone(t)
	z.defs.attr.register("lucky", &attributeDef{ref: "lucky"})
	z.defs.affect.register("blessed", &affectDef{
		ref: "blessed", name: "Blessed", stacking: stackCount, maxStacks: 5, duration: 100,
		modifiers: []affectModifier{{attr: "lucky", add: true, value: 1}},
	})
	one := 1.0
	spec := &checkSpec{
		dice: mustDice(t, "1d1"), // always a natural 1
		bands: []checkBand{
			{faceEq: &one, when: opFormula("-", litNode{v: 1}, attrNode{ref: "lucky"}), label: "fumble"},
			{label: "ok"},
		},
	}
	c := checkCtx(z, caster.entity, caster.entity, mob)

	require.Equal(t, "fumble", resolveCheck(c, spec).bandLabel, "no Lucky: a nat-1 fumbles")
	for stacks := 1; stacks <= 4; stacks++ {
		applyAffect(caster.entity, "blessed", attachOpts{}, nil)
		require.Equal(t, "ok", resolveCheck(c, spec).bandLabel,
			"Lucky x%d must keep suppressing the fumble — under a non-zero rule stack 2 would re-enable it", stacks)
	}
}

// TestWhenOnTheLastBandIsRejected pins the parse guard for the nil-band hazard. `when` makes "no band
// matched" reachable for the first time, and an unmatched check is NOT a failure: classifyToHit reads
// nil as a HIT. So a false predicate on the final band would silently turn a colossal miss into a hit.
func TestWhenOnTheLastBandIsRejected(t *testing.T) {
	_, err := parseCheckSpec(map[string]any{
		"dice": "1d20",
		"bands": []any{
			map[string]any{"margin_min": float64(0), "label": "hit"},
			map[string]any{"when": []any{"attr", "$target.helpless"}, "label": "miss"},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "LAST band")

	// The same predicate is fine with a fall-through band beneath it.
	_, err = parseCheckSpec(map[string]any{
		"dice": "1d20",
		"bands": []any{
			map[string]any{"margin_min": float64(0), "label": "hit"},
			map[string]any{"when": []any{"attr", "$target.helpless"}, "label": "forced"},
			map[string]any{"label": "miss"},
		},
	})
	require.NoError(t, err)
}

// TestUnmatchedCheckReadsAsAHit documents WHY the guard above exists, by pinning the engine behaviour
// it protects against. This is a property of classifyToHit, not of #513 — but #513 is what made it
// reachable from content, so it belongs in the record here.
func TestUnmatchedCheckReadsAsAHit(t *testing.T) {
	hit, crit := classifyToHit(checkResult{band: nil})
	require.True(t, hit, "an unmatched check reads as a HIT — which is why a nil band must be unreachable")
	require.False(t, crit)
	require.False(t, avoidanceSucceeded(checkResult{band: nil}), "...and as 'did not avoid' on the ladder")
}

// TestWhenOnAContestedSubSpecIsRejected closes the sibling silent no-op. resolveCheck uses ONLY a
// contested sub-spec's dice and bonus — its bands are never consulted — so a `when` there is authored,
// parsed, and permanently dead. That is the same class as boon_dice-without-boon, and gets the same
// treatment: reject at parse rather than let content believe a rule is live.
func TestWhenOnAContestedSubSpecIsRejected(t *testing.T) {
	_, err := parseCheckSpec(map[string]any{
		"dice": "1d20",
		"vs": map[string]any{"contested": map[string]any{
			"dice": "1d20",
			"bands": []any{
				map[string]any{"when": []any{"attr", "helpless"}, "label": "auto-lose"},
				map[string]any{"label": "normal"},
			},
		}},
		"bands": []any{map[string]any{"label": "win"}},
	})
	require.Error(t, err, "a contested sub-spec's bands are ignored, so a `when` there can never fire")
	require.Contains(t, err.Error(), "contested")
}

// TestConditionFlagsAreGatedUnconditionally is the SECURITY test for #513. A `when` predicate names a
// content-declared condition flag, and which direction of that flag helps is written in the band's
// LABEL — which the engine never reads. So the engine does not guess: any modifier touching such an
// attribute is gate-worthy, in either direction.
//
// The alternative (mirror the boon/bane ROLE rule: $target-scoped is inverted) was prototyped and
// measured to close only HALF the hole — it catches the auto-crit flag on the victim and leaves the
// roller-scoped auto-fail-save wide open, which is the more dangerous of the two. The precedent for
// gating instead of inferring is opModifyAttributeBase, which already gates every cross-player
// attribute-base write regardless of sign for exactly this reason.
func TestConditionFlagsAreGatedUnconditionally(t *testing.T) {
	mk := func(when formulaNode) *defRegistries {
		d := &defRegistries{
			ability: newDefRegistry[*abilityDef](), bundle: newDefRegistry[*bundleDef](),
			track: newDefRegistry[*trackDef](), affect: newDefRegistry[*affectDef](),
			res: newDefRegistry[*resourceDef](), combat: newDefRegistry[*combatProfile](),
		}
		d.combat.register("melee", &combatProfile{toHit: &checkSpec{
			dice:  mustDice(t, "1d20"),
			bands: []checkBand{{when: when, label: "crit"}, {label: "hit"}},
		}})
		return d
	}

	for _, tc := range []struct {
		name string
		ref  string
		flag string
	}{
		{"a $target-scoped auto-crit flag", "$target.helpless", "helpless"},
		{"a ROLLER-scoped auto-fail flag", "autofail_save", "autofail_save"},
		{"a $source-scoped flag", "$source.marked", "marked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := harmPolarity{conditionFlags: conditionFlagAttrs(mk(attrNode{ref: tc.ref}))}
			require.True(t, h.conditionFlags[tc.flag], "the flag must be derived from the when predicate")

			// Both directions are gate-worthy: the engine cannot tell which one helps.
			up := &affectDef{ref: "hex", modifiers: []affectModifier{{attr: tc.flag, add: true, value: 1}}}
			down := &affectDef{ref: "ward", modifiers: []affectModifier{{attr: tc.flag, add: true, value: -1}}}
			for _, def := range []*affectDef{up, down} {
				require.True(t, affectIsDetrimental(def, h),
					"%s: a modifier on a condition flag must route through the harm gate", def.ref)
				require.False(t, affectSurvivesRespawn(def, h),
					"%s: ...and must not survive the respawn purge", def.ref)
			}

			// Without the derivation this is the hole: a severe debuff reads as a buff.
			require.False(t, affectIsDetrimental(up, harmPolarity{}),
				"this is what the derivation exists to close")
			require.True(t, affectSurvivesRespawn(up, harmPolarity{}),
				"...and it would have followed the victim through death")
		})
	}

	t.Run("an attribute no when predicate names is untouched", func(t *testing.T) {
		h := harmPolarity{conditionFlags: conditionFlagAttrs(mk(attrNode{ref: "$target.helpless"}))}
		buff := &affectDef{ref: "might", modifiers: []affectModifier{{attr: "strength", add: true, value: 3}}}
		require.False(t, affectIsDetrimental(buff, h), "an ordinary stat buff stays ungated")
		require.True(t, affectSurvivesRespawn(buff, h))
	})

	t.Run("content with no when predicate derives nothing", func(t *testing.T) {
		d := &defRegistries{
			ability: newDefRegistry[*abilityDef](), bundle: newDefRegistry[*bundleDef](),
			track: newDefRegistry[*trackDef](), affect: newDefRegistry[*affectDef](),
			res: newDefRegistry[*resourceDef](), combat: newDefRegistry[*combatProfile](),
		}
		require.Nil(t, conditionFlagAttrs(d), "the pre-#513 behaviour, byte for byte")
	})
}

// TestConditionFlagGateIsWiredIntoApplyAffect is the WIRING half: the derivation existing is not the
// same as the apply path consulting it. This drives a real apply_affect at a non-consenting player.
func TestConditionFlagGateIsWiredIntoApplyAffect(t *testing.T) {
	z, caster, _ := whenZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	z.defs.affect.register("hex", &affectDef{
		ref: "hex", name: "Hexed", stacking: stackRefresh, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "autofail_save", add: true, value: 1}},
	})
	// Declare autofail_save as a condition flag the way content does: by naming it in a when predicate.
	z.defs.combat.register("melee", &combatProfile{toHit: &checkSpec{
		dice:  mustDice(t, "1d20"),
		bands: []checkBand{{when: attrNode{ref: "autofail_save"}, label: "fail"}, {label: "hit"}},
	}})
	z.defs.harm = harmPolarity{conditionFlags: conditionFlagAttrs(z.defs)}
	require.True(t, z.defs.harm.conditionFlags["autofail_save"])

	// A NEUTRAL-disposition apply at a non-consenting player: only the DERIVED harm can gate it.
	c := &effectCtx{z: z, actor: caster.entity, source: caster.entity, target: victim.entity, mag: 1, disp: dispNeutral}
	require.NoError(t, opApplyAffect(c, &effectOp{kind: "apply_affect", affect: "hex"}))

	require.False(t, hasAffect(victim.entity, "hex"),
		"a condition-flag debuff at a non-consenting player must be refused by the PvP gate")
	require.Equal(t, 0.0, attr(victim.entity, "autofail_save"))
}

// TestWhenScreensNonFiniteAttributes drives a `when`-referenced attribute non-finite through
// resolveCheck, so the finiteness screen is pinned by BEHAVIOUR rather than only by a direct unit test
// on the helper. The bare-ref case is fully closed here; the LAUNDERED case is not, and is asserted as
// a known limit so it cannot quietly regress in either direction.
func TestWhenScreensNonFiniteAttributes(t *testing.T) {
	z, caster, mob := whenZone(t)
	z.defs.attr.register("corrupt", &attributeDef{ref: "corrupt", base: litNode{v: 1}})
	for _, ref := range []string{"huge_a", "huge_b"} {
		z.defs.affect.register(ref, &affectDef{
			ref: ref, name: ref, stacking: stackRefresh, maxStacks: 1, duration: 100,
			modifiers: []affectModifier{{attr: "corrupt", add: false, value: 1e308}},
		})
	}
	applyAffect(mob, "huge_a", attachOpts{}, nil)
	applyAffect(mob, "huge_b", attachOpts{}, nil)

	c := checkCtx(z, caster.entity, caster.entity, mob)
	band := func(when formulaNode) string {
		return resolveCheck(c, &checkSpec{
			dice:  mustDice(t, "1d1"),
			bands: []checkBand{{when: when, label: "forced"}, {label: "normal"}},
		}).bandLabel
	}

	poisoned := attr(mob, "corrupt")
	require.True(t, math.IsInf(poisoned, 1) || poisoned == attrFoldCeilingSentinel(),
		"the fixture must actually poison the attribute; got %v", poisoned)

	require.Equal(t, "normal", band(attrNode{ref: "$target.corrupt"}),
		"a bare reference to a non-finite attribute must NOT satisfy a forcing predicate")
}

// attrFoldCeilingSentinel lets the test above keep asserting the right thing once attr() itself is
// bounded: today the fold overflows to +Inf, and a later fix will saturate it instead. Either way the
// fixture is poisoned, which is the precondition the test needs.
func attrFoldCeilingSentinel() float64 { return math.MaxFloat64 }

// TestWhenLaunderedNonFiniteIsAKnownLimit records the half that screening at the band CANNOT close:
// evalFinite checks only a formula's final value, so a wrapper turns an infinite attribute into a
// clean finite one before the predicate ever sees it. This is asserted as the CURRENT behaviour, not
// as desirable behaviour — it is closed properly by bounding attr(), and this test is what will go red
// (and want updating) when that lands.
func TestWhenLaunderedNonFiniteIsAKnownLimit(t *testing.T) {
	z, caster, mob := whenZone(t)
	z.defs.attr.register("corrupt", &attributeDef{ref: "corrupt", base: litNode{v: 1}})
	for _, ref := range []string{"huge_a", "huge_b"} {
		z.defs.affect.register(ref, &affectDef{
			ref: ref, name: ref, stacking: stackRefresh, maxStacks: 1, duration: 100,
			modifiers: []affectModifier{{attr: "corrupt", add: false, value: 1e308}},
		})
	}
	applyAffect(mob, "huge_a", attachOpts{}, nil)
	applyAffect(mob, "huge_b", attachOpts{}, nil)

	c := checkCtx(z, caster.entity, caster.entity, mob)
	laundered := opFormula("min", attrNode{ref: "$target.corrupt"}, litNode{v: 1})
	got := resolveCheck(c, &checkSpec{
		dice:  mustDice(t, "1d1"),
		bands: []checkBand{{when: laundered, label: "forced"}, {label: "normal"}},
	}).bandLabel

	require.Equal(t, "forced", got,
		"KNOWN LIMIT: min(+Inf, 1) evaluates to a finite 1, so the band screen cannot see the poison. "+
			"Bounding attr() is the real fix; when that lands this expectation should become \"normal\"")
}

// TestEmptyFormulaFieldIsRejected pins the present-but-null gate. `parseFormula(nil)` returns
// (nil, nil), so a bare `when:` in YAML — an author mid-edit, a template that interpolated empty, a
// merge that dropped a value — would leave the field nil and the axis SKIPPED. For a numeric bound
// that silently widens a band; for `when` it fails OPEN, turning a when-only forcing band placed
// first into an unconditional one, so every swing on that profile crits with no error at all.
func TestEmptyFormulaFieldIsRejected(t *testing.T) {
	for _, key := range []string{"when", "min", "max", "margin_min", "margin_max"} {
		t.Run("band "+key, func(t *testing.T) {
			_, err := parseCheckSpec(map[string]any{
				"dice": "1d20",
				"bands": []any{
					map[string]any{"label": "forced", key: nil},
					map[string]any{"label": "normal"},
				},
			})
			require.Error(t, err, "a present-but-empty %q must not be read as absent", key)
			require.Contains(t, err.Error(), key)
			require.Contains(t, err.Error(), "present but empty")
		})
	}
	for _, key := range []string{"boon", "bane"} {
		t.Run("spec "+key, func(t *testing.T) {
			_, err := parseCheckSpec(map[string]any{
				"dice": "1d20", key: nil,
				"bands": []any{map[string]any{"label": "hit"}},
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), "present but empty")
		})
	}

	t.Run("a genuinely absent key is still fine", func(t *testing.T) {
		_, err := parseCheckSpec(map[string]any{
			"dice":  "1d20",
			"bands": []any{map[string]any{"label": "hit"}},
		})
		require.NoError(t, err)
	})
}

// TestHarmPolarityIsPopulatedAtBuild is the BUILD WIRING test. Every other polarity test constructs a
// harmPolarity by hand, so nothing pinned that defineGlobals actually derives and stores one —
// clearing the field at the build site left the whole suite green. This drives real content through
// the real build path and then through the real harm derivation.
func TestHarmPolarityIsPopulatedAtBuild(t *testing.T) {
	lc := &content.LoadedContent{
		Attributes: []content.AttributeDTO{{Ref: "helpless"}, {Ref: "atk_bane"}},
		CombatProfiles: []content.CombatProfileDTO{{
			Ref: "melee",
			ToHit: map[string]any{
				"dice": "1d20",
				"bane": []any{"attr", "atk_bane"},
				"bands": []any{
					map[string]any{"label": "crit", "when": []any{"attr", "$target.helpless"}},
					map[string]any{"label": "hit"},
				},
			},
		}},
	}
	d := newDefRegistries()
	defineGlobals(d, lc)

	require.True(t, d.harm.conditionFlags["helpless"],
		"defineGlobals must derive the condition-flag set from the shipped check formulas")
	require.True(t, d.harm.inverted["atk_bane"],
		"...and the inverted-polarity set alongside it")

	// And the derived sets must actually reach the harm decision.
	hex := &affectDef{ref: "hex", modifiers: []affectModifier{{attr: "helpless", add: true, value: 1}}}
	require.True(t, affectIsDetrimental(hex, d.harm))
	require.False(t, affectSurvivesRespawn(hex, d.harm))
}
