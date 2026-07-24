package world

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
)

// boonbane_test.go exercises the boon/bane channel (#511) — the transient advantage/disadvantage
// mechanism — and the kept-face band fix it depends on.
//
// The channel's whole engine rule is: net two scoped formulas by SIGN and SELECT one of three
// content-authored dice expressions. So the properties worth pinning are (a) the selection matrix,
// (b) that the engine never derives an expression content did not write, (c) that a spec with no
// boon/bane is byte-identically unaffected, and (d) that the direction of "better" stays content's —
// which is what the roll-under and pool cases below prove. Determinism comes from a seeded ctx rng.

// boonZone builds a zone with the attributes and affects the channel tests drive: an attacker-side
// boon/bane pair and a defender-side "grants a boon to my attackers" attribute (the prone/helpless
// shape), each fed by an ordinary additive affect modifier — deliberately NOT a new affect field,
// since the point of the design is that no affect-runtime change was needed.
func boonZone(t *testing.T) (*Zone, *session, *Entity) {
	t.Helper()
	z, caster := abilityTestZone(t)
	for _, a := range []string{"atk_boon", "atk_bane", "grants_boon_to_attackers", "save_boon"} {
		z.defs.attr.register(a, &attributeDef{ref: a})
	}
	z.defs.affect.register("bless", &affectDef{
		ref: "bless", name: "Blessed", stacking: stackCount, maxStacks: 5, duration: 100,
		modifiers: []affectModifier{{attr: "atk_boon", add: true, value: 1}},
	})
	z.defs.affect.register("curse", &affectDef{
		ref: "curse", name: "Cursed", stacking: stackCount, maxStacks: 5, duration: 100,
		modifiers: []affectModifier{{attr: "atk_bane", add: true, value: 1}},
	})
	z.defs.affect.register("prone", &affectDef{
		ref: "prone", name: "Prone", stacking: stackRefresh, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "grants_boon_to_attackers", add: true, value: 1}},
	})
	return z, caster, makeMobTarget(z, caster.entity, "goblin")
}

// fiveESpec is the canonical 5e-shaped to-hit: a neutral d20, with content naming BOTH alternatives
// itself. The boon formula reads the attacker's own boon PLUS one the defender grants — the whole
// "attacks against a prone target have advantage" case, expressed with the scoping the engine already
// had, on the attacker's spec.
func fiveESpec(t *testing.T) *checkSpec {
	t.Helper()
	boonD, baneD := mustDice(t, "2d20kh1"), mustDice(t, "2d20kl1")
	return &checkSpec{
		dice:     mustDice(t, "1d20"),
		boonDice: &boonD,
		baneDice: &baneD,
		boon: opFormula("+", attrNode{ref: "$actor.atk_boon"},
			attrNode{ref: "$target.grants_boon_to_attackers"}),
		bane: attrNode{ref: "$actor.atk_bane"},
	}
}

// TestEffectiveDiceSelectionMatrix is the core table: every combination of net sign and authored
// alternative, asserted against the RAW NOTATION of the returned spec (a frozen literal the
// implementation cannot move — not a re-derivation of the selection rule).
func TestEffectiveDiceSelectionMatrix(t *testing.T) {
	z, caster, mob := boonZone(t)
	c := checkCtx(z, caster.entity, caster.entity, mob)

	boonD, baneD := mustDice(t, "2d20kh1"), mustDice(t, "2d20kl1")
	base := mustDice(t, "1d20")

	tests := []struct {
		name     string
		boon     formulaNode
		bane     formulaNode
		boonDice *diceSpec
		baneDice *diceSpec
		want     string
	}{
		{"no channel authored at all", nil, nil, &boonD, &baneD, "1d20"},
		{"net positive picks the boon die", litNode{v: 1}, nil, &boonD, &baneD, "2d20kh1"},
		{"net negative picks the bane die", nil, litNode{v: 1}, &boonD, &baneD, "2d20kl1"},
		{"equal boon and bane cancel to the neutral die", litNode{v: 3}, litNode{v: 3}, &boonD, &baneD, "1d20"},
		{"many boons and one bane still cancel (sign, not magnitude)", litNode{v: 9}, litNode{v: 1}, &boonD, &baneD, "2d20kh1"},
		{"net positive with no boon die authored falls back", litNode{v: 1}, nil, nil, &baneD, "1d20"},
		{"net negative with no bane die authored falls back", nil, litNode{v: 1}, &boonD, nil, "1d20"},
		{"a zero-valued boon formula is not a boon", litNode{v: 0}, nil, &boonD, &baneD, "1d20"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := &checkSpec{dice: base, boon: tc.boon, bane: tc.bane, boonDice: tc.boonDice, baneDice: tc.baneDice}
			require.Equal(t, tc.want, effectiveDice(c, spec, c.actor).raw)
		})
	}
}

// TestBoonChannelReadsLiveAffects walks the channel end to end through the ORDINARY affect runtime:
// no affect field was added for #511, so a boon must arrive purely as an additive modifier on a
// content-named attribute. It also pins the two-sided case (the defender granting the attacker a
// boon) and 5e's cancellation rule.
func TestBoonChannelReadsLiveAffects(t *testing.T) {
	z, caster, mob := boonZone(t)
	c := checkCtx(z, caster.entity, caster.entity, mob)
	spec := fiveESpec(t)

	require.Equal(t, "1d20", effectiveDice(c, spec, c.actor).raw, "an unaffected attacker rolls the neutral die")

	applyAffect(caster.entity, "bless", attachOpts{}, nil)
	require.Equal(t, "2d20kh1", effectiveDice(c, spec, c.actor).raw, "a blessed attacker takes the boon die")

	applyAffect(caster.entity, "curse", attachOpts{}, nil)
	require.Equal(t, "1d20", effectiveDice(c, spec, c.actor).raw,
		"boon + bane cancel to the neutral die (5e's rule falls out of sign(net))")

	// A SECOND bless stacks the attribute to 2 vs 1 bane — but cancellation is by SIGN, so this must
	// flip back to the boon die rather than 'out-weighing' anything by magnitude.
	applyAffect(caster.entity, "bless", attachOpts{}, nil)
	require.Equal(t, "2d20kh1", effectiveDice(c, spec, c.actor).raw)

	// Reset to cancelled, then let the DEFENDER supply the deciding boon: the prone/helpless case,
	// keyed off $target on the attacker's own spec.
	applyAffect(caster.entity, "curse", attachOpts{}, nil) // 2 bless vs 2 curse -> net 0
	require.Equal(t, "1d20", effectiveDice(c, spec, c.actor).raw)
	applyAffect(mob, "prone", attachOpts{}, nil)
	require.Equal(t, "2d20kh1", effectiveDice(c, spec, c.actor).raw,
		"a prone DEFENDER grants the attacker the boon through $target scoping")
}

// TestChannelLearnsNoDirection is the anti-5e-vocabulary property, and the reason the engine selects
// rather than synthesizes. The demo pack's avoidance ladder is ROLL-UNDER (1d100, lower is better);
// a boon there must keep the LOW die. An engine that "knew" advantage meant keep-highest would invert
// this ladder silently. Here content names the direction and the engine simply obeys.
func TestChannelLearnsNoDirection(t *testing.T) {
	z, caster, mob := boonZone(t)
	c := checkCtx(z, caster.entity, caster.entity, mob)
	applyAffect(caster.entity, "bless", attachOpts{}, nil)

	rollUnderBoon, rollUnderBane := mustDice(t, "2d100kl1"), mustDice(t, "2d100kh1")
	ladder := &checkSpec{
		dice: mustDice(t, "1d100"), boonDice: &rollUnderBoon, baneDice: &rollUnderBane,
		boon: attrNode{ref: "$actor.atk_boon"},
	}
	require.Equal(t, "2d100kl1", effectiveDice(c, ladder, c.actor).raw,
		"a roll-under boon keeps the LOW die — the engine must not assume higher is better")

	// A dice-POOL system's boon is not a keep at all: it is an extra die in the pool. Synthesizing
	// keep-highest could never express this; selecting a content-authored expression does it for free.
	poolBoon := mustDice(t, "3d6>=4")
	pool := &checkSpec{
		dice: mustDice(t, "2d6>=4"), boonDice: &poolBoon,
		boon: attrNode{ref: "$actor.atk_boon"},
	}
	got := effectiveDice(c, pool, c.actor)
	require.Equal(t, "3d6>=4", got.raw)
	require.Equal(t, dicePool, got.kind, "the boon alternative may be a different KIND than the neutral die")
	_ = mob
}

// TestUnauthoredSpecIsUntouched is the non-regression guarantee: every check authored before #511 has
// nil boon and nil bane, and must come back byte-identical without evaluating anything. The 4d6kh3
// case is the one that matters most — a stat-roll spec is keep-high as a MECHANIC, not as advantage,
// and an earlier design that inferred "kh means advantage" mangled it into 6d6kh3 / 3d6.
func TestUnauthoredSpecIsUntouched(t *testing.T) {
	z, caster, mob := boonZone(t)
	c := checkCtx(z, caster.entity, caster.entity, mob)
	// Live boon/bane attributes on both entities, to prove the fast path ignores them entirely.
	applyAffect(caster.entity, "bless", attachOpts{}, nil)
	applyAffect(mob, "prone", attachOpts{}, nil)

	for _, notation := range []string{"1d20", "4d6kh3", "3d20kl1", "4dF", "5d6>4", "1d100"} {
		t.Run(notation, func(t *testing.T) {
			spec := &checkSpec{dice: mustDice(t, notation)}
			require.Equal(t, spec.dice, effectiveDice(c, spec, c.actor),
				"a spec with no boon/bane formula must be returned unchanged, field for field")
		})
	}
}

// TestKeptFacesDriveNatFaceBands pins the face_eq fix. Under a boon-selected 2d20kh1 the natural-face
// bands must read the die the check ACTUALLY USED, not the discarded one. The oracle is exhaustive
// over all 400 (a,b) outcomes and computed from first principles (max/min of the pair), so it cannot
// drift with the implementation.
func TestKeptFacesDriveNatFaceBands(t *testing.T) {
	one := 1.0
	twenty := 20.0
	missBand := checkBand{faceEq: &one, label: "miss"}
	critBand := checkBand{faceEq: &twenty, label: "crit"}
	noEval := func(formulaNode) float64 { return 0 }

	for _, tc := range []struct {
		name string
		kind diceKind
		pick func(a, b int) int
	}{
		{"boon (keep highest)", diceKeepHigh, func(a, b int) int { return max(a, b) }},
		{"bane (keep lowest)", diceKeepLow, func(a, b int) int { return min(a, b) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var missHits, critHits int
			for a := 1; a <= 20; a++ {
				for b := 1; b <= 20; b++ {
					_, kept := sumKept([]int{a, b}, 1, tc.kind == diceKeepHigh)
					used := tc.pick(a, b)
					require.Equal(t, []int{used}, kept, "kept face for (%d,%d)", a, b)
					if missBand.matches(0, 0, kept, noEval) {
						missHits++
						require.Equal(t, 1, used, "a nat-1 band fired when the USED die was %d", used)
					}
					if critBand.matches(0, 0, kept, noEval) {
						critHits++
						require.Equal(t, 20, used, "a nat-20 band fired when the USED die was %d", used)
					}
				}
			}
			// Exactly one of the 400 pairs yields the extreme in the unfavourable direction, and 39 in
			// the favourable one. Frozen literals: reading ALL faces gave 39 in BOTH directions, which
			// is the ~37x auto-miss regression this fix removes.
			if tc.kind == diceKeepHigh {
				require.Equal(t, 1, missHits, "keep-high: only (1,1) may auto-miss")
				require.Equal(t, 39, critHits, "keep-high: any 20 crits")
			} else {
				require.Equal(t, 39, missHits, "keep-low: any 1 auto-misses")
				require.Equal(t, 1, critHits, "keep-low: only (20,20) may crit")
			}
		})
	}
}

// TestResolveCheckClassifiesOnKeptFaces is the WIRING test for the face_eq fix — deliberately separate
// from the matches()/sumKept() unit tests above, which both passed while resolveCheck still handed the
// classifier every rolled face. (Mutation-testing caught exactly that: reverting the one argument at
// the call site left the whole unit suite green.)
//
// It is a property test over seeds rather than a fixed roll, because a deterministic d1 shows the same
// face on both dice and so cannot distinguish kept from discarded. The oracle — "the nat-1 band fires
// iff the die the check USED was a 1" — is computed from the faces the roll reports, not from the
// classifier under test.
func TestResolveCheckClassifiesOnKeptFaces(t *testing.T) {
	z, caster, mob := boonZone(t)
	one := 1.0
	boonD := mustDice(t, "2d20kh1")
	spec := &checkSpec{
		dice: mustDice(t, "1d20"), boonDice: &boonD,
		boon:  attrNode{ref: "$actor.atk_boon"},
		bands: []checkBand{{faceEq: &one, label: "miss"}, {label: "hit"}},
	}
	applyAffect(caster.entity, "bless", attachOpts{}, nil) // select the keep-high die

	var discriminating int // rolls where SOME face is 1 but the USED face is not
	for seed := int64(0); seed < 400; seed++ {
		c := &effectCtx{
			z: z, actor: caster.entity, source: caster.entity, target: mob,
			mag: 1, rng: rand.New(rand.NewSource(seed)),
		}
		res := resolveCheck(c, spec)
		require.Len(t, res.faces, 2, "the boon die must have been selected")

		used := max(res.faces[0], res.faces[1])
		anyOne := res.faces[0] == 1 || res.faces[1] == 1
		if anyOne && used != 1 {
			discriminating++
			require.Equal(t, "hit", res.bandLabel,
				"seed %d rolled %v: the discarded die showed a 1 but the check USED a %d", seed, res.faces, used)
		}
		require.Equal(t, used == 1, res.bandLabel == "miss",
			"seed %d rolled %v (used %d)", seed, res.faces, used)
	}
	// The precondition: assert the discriminating case actually occurred, so this test cannot pass by
	// never exercising the difference it exists to pin.
	require.Greater(t, discriminating, 20,
		"the sample must contain rolls where a DISCARDED die showed 1, or the test proves nothing")
}

// TestKeptEqualsAllFacesForNonKeepKinds is the zero-change half of the face_eq fix: for every dice
// kind that discards nothing, the kept set IS every rolled face, so no existing content shifts.
func TestKeptEqualsAllFacesForNonKeepKinds(t *testing.T) {
	for _, notation := range []string{"1d20", "3d6", "4dF", "5d6>4", "5d6>=5"} {
		t.Run(notation, func(t *testing.T) {
			c := &effectCtx{rng: rand.New(rand.NewSource(7))}
			_, faces, kept := rollDiceSpec(c, mustDice(t, notation))
			require.Equal(t, faces, kept, "a kind that discards nothing must report every face as kept")
		})
	}
}

// TestResolveCheckUsesTheSelectedDie drives the channel through resolveCheck itself (not just the
// selection helper), pinning that the roll, the reported faces and the band classification all come
// from the selected expression. "2d1" / "1d1" are deterministic anchors: a d1 always shows 1.
func TestResolveCheckUsesTheSelectedDie(t *testing.T) {
	z, caster, mob := boonZone(t)
	c := checkCtx(z, caster.entity, caster.entity, mob)

	boonD := mustDice(t, "2d1") // deterministic: totals 2, two faces
	spec := &checkSpec{
		dice: mustDice(t, "1d1"), boonDice: &boonD,
		boon:  attrNode{ref: "$actor.atk_boon"},
		bands: []checkBand{{min: bn(2), label: "boosted"}, {label: "plain"}},
	}

	res := resolveCheck(c, spec)
	require.Equal(t, 1, res.roll)
	require.Len(t, res.faces, 1)
	require.Equal(t, "plain", res.bandLabel)
	require.Equal(t, "1d1", res.dice.raw)

	applyAffect(caster.entity, "bless", attachOpts{}, nil)
	res = resolveCheck(c, spec)
	require.Equal(t, 2, res.roll, "the boon die was rolled")
	require.Len(t, res.faces, 2)
	require.Equal(t, "boosted", res.bandLabel, "the selected die re-classified the band")
	require.Equal(t, "2d1", res.dice.raw)
}

// TestContestedSubSpecGetsItsOwnSelection pins that a contested defender's conditions matter too — a
// prone defender must contest a grapple with its own bane die, not at full strength. The defender's
// bare refs scope to $target, matching how its `bonus` already resolves.
func TestContestedSubSpecGetsItsOwnSelection(t *testing.T) {
	z, caster, mob := boonZone(t)
	c := checkCtx(z, caster.entity, caster.entity, mob)

	defBane := mustDice(t, "1d1") // deterministic 1
	defender := &checkSpec{
		dice: mustDice(t, "5d1"), baneDice: &defBane, // neutral totals 5, baned totals 1
		bane: attrNode{ref: "save_boon"}, // BARE ref: must resolve against the defender ($target)
	}
	spec := &checkSpec{
		dice:  mustDice(t, "3d1"), // attacker deterministically totals 3
		vs:    checkVs{contested: defender},
		bands: []checkBand{{marginMin: bn(0), label: "win"}, {label: "lose"}},
	}

	require.Equal(t, "lose", resolveCheck(c, spec).bandLabel, "3 vs the defender's neutral 5")

	// Give the DEFENDER a bane via its own attribute; the sub-spec must pick its bane die.
	z.defs.affect.register("hobbled", &affectDef{
		ref: "hobbled", name: "Hobbled", stacking: stackRefresh, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "save_boon", add: true, value: 1}},
	})
	applyAffect(mob, "hobbled", attachOpts{}, nil)
	require.Equal(t, "win", resolveCheck(c, spec).bandLabel,
		"the defender's own bane die (1) dropped the contested DC below the attacker's 3")
}

// TestParseCheckSpecRejectsUnknownKeys pins the typo gate. Every field of a check is optional, so
// without this an unrecognised key is indistinguishable from an absent one and a mistyped `boon_dice`
// is a silent, permanent no-op.
func TestParseCheckSpecRejectsUnknownKeys(t *testing.T) {
	valid := map[string]any{
		"dice":      "1d20",
		"boon":      float64(1),
		"boon_dice": "2d20kh1",
		"bands":     []any{map[string]any{"label": "hit"}},
	}
	spec, err := parseCheckSpec(valid)
	require.NoError(t, err)
	require.NotNil(t, spec.boonDice)
	require.Equal(t, "2d20kh1", spec.boonDice.raw)

	for _, tc := range []struct {
		name   string
		mutate func(m map[string]any)
		want   string
	}{
		{"misspelled boon_dice", func(m map[string]any) { m["dice_boon"] = "2d20kh1" }, "dice_boon"},
		{"misspelled bane", func(m map[string]any) { m["banes"] = float64(1) }, "banes"},
		{"a plausible-but-absent field", func(m map[string]any) { m["roller"] = "target" }, "roller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]any{}
			for k, v := range valid {
				m[k] = v
			}
			tc.mutate(m)
			_, err := parseCheckSpec(m)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
			require.Contains(t, err.Error(), "legal keys:", "the error must tell a builder what IS legal")
		})
	}

	t.Run("band keys are gated too", func(t *testing.T) {
		_, err := parseCheckSpec(map[string]any{
			"dice":  "1d20",
			"bands": []any{map[string]any{"label": "hit", "margin_minimum": float64(0)}},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "margin_minimum")
	})
}

// TestParseCheckSpecAcceptsEveryDocumentedKey is the other half of the gate: the legal set must not
// be so tight that it rejects a check the docs tell builders to write. This is what would fail if
// someone added a checkSpec field and forgot to register its key.
func TestParseCheckSpecAcceptsEveryDocumentedKey(t *testing.T) {
	spec, err := parseCheckSpec(map[string]any{
		"label": "Dexterity save", "dice": "1d20", "visibility": "show",
		"bonus": []any{"attr", "$target.dex_save"}, "vs": []any{"attr", "$source.spell_dc"},
		"boon": []any{"attr", "$target.save_boon"}, "bane": []any{"attr", "$target.save_bane"},
		"boon_dice": "2d20kh1", "bane_dice": "2d20kl1",
		"bands": []any{
			map[string]any{"face_eq": float64(20), "face_count": float64(1), "label": "crit"},
			map[string]any{"min": float64(10), "max": float64(19), "label": "ok"},
			map[string]any{"margin_min": float64(0), "margin_max": float64(5), "label": "narrow", "ops": []any{}},
			map[string]any{"label": "fail"},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, spec.boon)
	require.NotNil(t, spec.bane)
	require.NotNil(t, spec.boonDice)
	require.NotNil(t, spec.baneDice)
	require.Len(t, spec.bands, 4)
}
