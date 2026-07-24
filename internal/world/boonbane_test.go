package world

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"

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

// divByZero builds a formula that FAILS to evaluate (formula.go rejects division by zero), so a test
// can distinguish "this side counted zero" from "this side could not be evaluated" — which
// effectiveDice must treat differently.
func divByZero() formulaNode { return opFormula("/", litNode{v: 1}, litNode{v: 0}) }

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
		{"a boon alone picks the boon die", litNode{v: 1}, nil, &boonD, &baneD, "2d20kh1"},
		{"a bane alone picks the bane die", nil, litNode{v: 1}, &boonD, &baneD, "2d20kl1"},
		{"one of each cancels to the neutral die", litNode{v: 1}, litNode{v: 1}, &boonD, &baneD, "1d20"},
		// The rule is ANY-versus-ANY, which is 5e's. A net-difference rule would resolve these two rows
		// as advantage/disadvantage respectively; both must be the straight roll.
		{"nine boons against one bane still cancel", litNode{v: 9}, litNode{v: 1}, &boonD, &baneD, "1d20"},
		{"one boon against nine banes still cancel", litNode{v: 1}, litNode{v: 9}, &boonD, &baneD, "1d20"},
		{"a boon with no boon die authored falls back", litNode{v: 1}, nil, nil, &baneD, "1d20"},
		{"a bane with no bane die authored falls back", nil, litNode{v: 1}, &boonD, nil, "1d20"},
		{"a zero-valued boon formula is not a boon", litNode{v: 0}, nil, &boonD, &baneD, "1d20"},
		{"a negative boon count is not a boon", litNode{v: -4}, nil, &boonD, &baneD, "1d20"},
		// A BROKEN channel is no channel: a side that fails to evaluate must not leave the other side
		// unopposed, which would turn an authoring bug in the boon half into a bane.
		{"an errored boon fails the whole channel neutral", divByZero(), litNode{v: 1}, &boonD, &baneD, "1d20"},
		{"an errored bane fails the whole channel neutral", litNode{v: 1}, divByZero(), &boonD, &baneD, "1d20"},
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
		"one boon and one bane cancel to the neutral die")

	// A SECOND bless makes it 2 boons against 1 bane. 5e's rule is ANY-versus-ANY, so this must STAY
	// cancelled — a net-difference rule would wrongly resolve it as advantage.
	applyAffect(caster.entity, "bless", attachOpts{}, nil)
	require.Equal(t, "1d20", effectiveDice(c, spec, c.actor).raw,
		"two boons against one bane still cancel: presence, not magnitude")

	// The DEFENDER can supply a boon too (the prone/helpless case, keyed off $target on the attacker's
	// own spec) — but while a bane is live it must still cancel.
	applyAffect(mob, "prone", attachOpts{}, nil)
	require.Equal(t, "1d20", effectiveDice(c, spec, c.actor).raw)

	// Drop the bane by expiring the curse, and the accumulated boons finally take the boon die. This
	// also proves the channel tracks affect REMOVAL, not just application.
	a, ok := Get[*Affected](caster.entity)
	require.True(t, ok)
	for _, inst := range append([]*affectInstance(nil), a.list...) {
		if inst.def.ref == "curse" {
			a.expire(caster.entity, inst, nil)
		}
	}
	require.Equal(t, "2d20kh1", effectiveDice(c, spec, c.actor).raw,
		"with the bane gone the boon side selects, including the $target-granted one")
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
			// NOTE the claim being made: the returned spec is field-for-field identical. The fast path in
			// effectiveDice is an OPTIMISATION, not the source of that identity — with it deleted, nil
			// formulas evaluate to 0, neither side is present, and the default branch returns spec.dice
			// anyway. So this pins the identity, which is what matters; it does not pin "nothing was
			// evaluated", which is unobservable from behaviour.
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
			d := mustDice(t, notation)
			for seed := int64(0); seed < 25; seed++ {
				_, faces, kept := rollDiceSpec(&effectCtx{rng: rand.New(rand.NewSource(seed))}, d)
				// CARDINALITY FIRST: without this the equality below is vacuously satisfiable by
				// returning nil for both slices, which is exactly what a mutation that drops the faces
				// entirely would do.
				require.Len(t, faces, d.num, "seed %d: every declared die must be rolled", seed)
				require.Equal(t, faces, kept, "a kind that discards nothing must report every face as kept")
			}
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

// TestParsedBoonKeyActuallyBoons is the PARSER->SELECTION wiring test. Every other behavioural test in
// this file hand-builds a checkSpec in Go, so swapping the parser's two destinations (`boon` writing
// spec.bane and vice versa) left the entire package green — a pack authoring `boon:` would have got
// disadvantage. This drives a spec that came out of parseCheckSpec through effectiveDice, which is the
// only shape that pins the two together.
func TestParsedBoonKeyActuallyBoons(t *testing.T) {
	z, caster, mob := boonZone(t)
	c := checkCtx(z, caster.entity, caster.entity, mob)

	spec, err := parseCheckSpec(map[string]any{
		"dice":      "1d20",
		"boon":      []any{"attr", "$actor.atk_boon"},
		"bane":      []any{"attr", "$actor.atk_bane"},
		"boon_dice": "2d20kh1",
		"bane_dice": "2d20kl1",
		"bands":     []any{map[string]any{"label": "hit"}},
	})
	require.NoError(t, err)

	applyAffect(caster.entity, "bless", attachOpts{}, nil)
	require.Equal(t, "2d20kh1", effectiveDice(c, spec, c.actor).raw,
		"the `boon` KEY must feed the boon side")

	// Remove the boon, add a bane: the other direction, so a swap cannot pass by symmetry.
	a, _ := Get[*Affected](caster.entity)
	for _, inst := range append([]*affectInstance(nil), a.list...) {
		a.expire(caster.entity, inst, nil)
	}
	applyAffect(caster.entity, "curse", attachOpts{}, nil)
	require.Equal(t, "2d20kl1", effectiveDice(c, spec, c.actor).raw,
		"the `bane` KEY must feed the bane side")
}

// TestEmitCheckDescribesTheSelectedDie pins that emission reads the EFFECTIVE die's kind, not the
// spec's. A boon alternative may be a different KIND than the neutral expression (a pool system's boon
// is an extra pool die), and reverting this to spec.dice left the suite green in both directions.
func TestEmitCheckDescribesTheSelectedDie(t *testing.T) {
	z, caster, mob := boonZone(t)

	drain := func() []string {
		var out []string
		for {
			select {
			case f := <-caster.out:
				if o := f.GetOutput(); o != nil {
					out = append(out, o.GetMarkup())
				}
			default:
				return out
			}
		}
	}

	t.Run("neutral sum, boon selects a pool", func(t *testing.T) {
		boonD := mustDice(t, "3d1>=1") // deterministic: 3 faces of 1, all succeed
		spec := &checkSpec{
			label: "Probe", dice: mustDice(t, "1d1"), boonDice: &boonD,
			boon: attrNode{ref: "$actor.atk_boon"}, visibility: visShow,
			bands: []checkBand{{label: "ok"}},
		}
		c := checkCtx(z, caster.entity, caster.entity, mob)
		resolveCheck(c, spec)
		require.Equal(t, []string{"[Probe] 1+0 = 1 — ok"}, drain(), "the neutral sum renders as a total")

		applyAffect(caster.entity, "bless", attachOpts{}, nil)
		resolveCheck(c, spec)
		require.Equal(t, []string{"[Probe] 3 successes — ok"}, drain(),
			"once the POOL alternative is selected, emission must describe a pool")
	})

	t.Run("neutral pool, bane selects a sum", func(t *testing.T) {
		a, _ := Get[*Affected](caster.entity)
		for _, inst := range append([]*affectInstance(nil), a.list...) {
			a.expire(caster.entity, inst, nil)
		}
		baneD := mustDice(t, "1d1")
		spec := &checkSpec{
			label: "Probe", dice: mustDice(t, "2d1>=1"), baneDice: &baneD,
			bane: attrNode{ref: "$actor.atk_bane"}, visibility: visShow,
			bands: []checkBand{{label: "ok"}},
		}
		c := checkCtx(z, caster.entity, caster.entity, mob)
		resolveCheck(c, spec)
		require.Equal(t, []string{"[Probe] 2 successes — ok"}, drain())

		applyAffect(caster.entity, "curse", attachOpts{}, nil)
		resolveCheck(c, spec)
		require.Equal(t, []string{"[Probe] 1+0 = 1 — ok"}, drain(),
			"the SUM alternative must render as a total, not as successes")
	})
}

// TestBoonDiceRejectsNonStringValues closes the VALUE axis of the typo gate. mapStr yields "" for any
// non-string, so a present-but-wrongly-typed alternative would silently leave the channel inert — the
// same permanent no-op the unknown-KEY gate exists to prevent, reached one axis over.
func TestBoonDiceRejectsNonStringValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  any
	}{
		{"a bare number", float64(20)},
		{"an int", 20},
		{"a sequence", []any{"2d20kh1"}},
		{"a bool", true},
		{"a nested map", map[string]any{"dice": "2d20kh1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCheckSpec(map[string]any{
				"dice": "1d20", "boon_dice": tc.val,
				"bands": []any{map[string]any{"label": "hit"}},
			})
			require.Error(t, err, "a present boon_dice of the wrong type must not be read as absent")
			// Assert the TYPE-specific message, not merely that some error mentions the key: without the
			// type check the value still fails, but as a confusing "empty expression" from parseDiceSpec,
			// so a laxer assertion would pass with the guard deleted.
			require.Contains(t, err.Error(), "boon_dice: must be a dice-notation string")
		})
	}
}

// TestInvertedPolarityGatesABaneDebuff is the SECURITY property. The PvP apply-gate derives harm from
// the SIGN of a modifier, which assumes higher-is-better. A bane counter inverts exactly that, so
// `{attr: atk_bane, op: add, value: 3}` — a real debuff that forces the bearer onto the worse die on
// every roll — would read as a buff, skip applyDebuff/guardHarmful entirely, and land on a
// non-consenting player in a no-PvP room. attributeInvertedPolarity is what closes it.
func TestInvertedPolarityGatesABaneDebuff(t *testing.T) {
	inverted := map[string]bool{"atk_bane": true, "grants_boon_to_attackers": true}

	hex := &affectDef{ref: "hex", modifiers: []affectModifier{{attr: "atk_bane", add: true, value: 3}}}
	marked := &affectDef{ref: "marked", modifiers: []affectModifier{{attr: "grants_boon_to_attackers", add: true, value: 1}}}
	bless := &affectDef{ref: "bless", modifiers: []affectModifier{{attr: "atk_boon", add: true, value: 1}}}
	cleanse := &affectDef{ref: "cleanse", modifiers: []affectModifier{{attr: "atk_bane", add: true, value: -2}}}

	t.Run("without the inverted set the debuffs read as benign", func(t *testing.T) {
		require.False(t, affectIsDetrimental(hex, nil), "this is the hole the derivation closes")
		require.True(t, affectSurvivesRespawn(hex, nil), "...and it would survive the respawn purge too")
	})

	t.Run("with it they are gated and purged", func(t *testing.T) {
		require.True(t, affectIsDetrimental(hex, inverted))
		require.False(t, affectSurvivesRespawn(hex, inverted))
		require.True(t, affectIsDetrimental(marked, inverted))
		require.False(t, affectSurvivesRespawn(marked, inverted))
	})

	t.Run("genuine buffs stay ungated so blessing an ally still lands", func(t *testing.T) {
		require.False(t, affectIsDetrimental(bless, inverted), "a boon counter is higher-is-better")
		require.True(t, affectSurvivesRespawn(bless, inverted))
		require.False(t, affectIsDetrimental(cleanse, inverted), "LOWERING a bane counter is a buff")
		require.True(t, affectSurvivesRespawn(cleanse, inverted))
	})
}

// TestAttributeInvertedPolarityDerivation pins the derivation rule itself: which refs land in the set
// and, just as importantly, which do NOT. The rule is by ROLE — a bane on the roller, or a boon on a
// counterpart — so a bare ref works identically in a top-level spec and a contested sub-spec.
func TestAttributeInvertedPolarityDerivation(t *testing.T) {
	spec := &checkSpec{
		dice: mustDice(t, "1d20"),
		boon: opFormula("+", attrNode{ref: "$target.victim_marked"}, attrNode{ref: "$actor.my_boon"}),
		bane: opFormula("+", attrNode{ref: "my_bane"}, attrNode{ref: "$target.their_defence"}),
		vs: checkVs{contested: &checkSpec{
			dice: mustDice(t, "1d20"),
			bane: attrNode{ref: "defender_bane"}, // bare == the roller == the defender
		}},
		bands: []checkBand{{label: "hit", ops: []effectOp{{
			kind:  "check",
			check: &checkSpec{dice: mustDice(t, "1d6"), boon: attrNode{ref: "$source.nested_marked"}},
		}}}},
	}
	d := &defRegistries{
		ability: newDefRegistry[*abilityDef](),
		bundle:  newDefRegistry[*bundleDef](),
		track:   newDefRegistry[*trackDef](),
		affect:  newDefRegistry[*affectDef](),
		res:     newDefRegistry[*resourceDef](),
		combat:  newDefRegistry[*combatProfile](),
	}
	d.combat.register("melee", &combatProfile{toHit: spec})

	got := attributeInvertedPolarity(d)

	// Inverted: a bane on the ROLLER, and a boon on a COUNTERPART.
	require.True(t, got["my_bane"], "a bare bane ref is the roller's own bane counter")
	require.True(t, got["victim_marked"], "a $target-scoped boon is a counter on the victim")
	require.True(t, got["defender_bane"], "a bare bane inside a CONTESTED sub-spec is the defender's")
	require.True(t, got["nested_marked"], "a check nested in a band's op-list is still walked")

	// NOT inverted: the ordinary higher-is-better shapes.
	require.False(t, got["my_boon"], "the roller's own boon counter is higher-is-better")
	require.False(t, got["their_defence"], "a $target-scoped BANE is the target's defence, a good stat")

	require.Nil(t, attributeInvertedPolarity(&defRegistries{
		ability: newDefRegistry[*abilityDef](), bundle: newDefRegistry[*bundleDef](),
		track: newDefRegistry[*trackDef](), affect: newDefRegistry[*affectDef](),
		res: newDefRegistry[*resourceDef](), combat: newDefRegistry[*combatProfile](),
	}), "content with no boon/bane derives an empty set, restoring pre-#511 behaviour exactly")
}

// TestBrokenCombatProfileRefusesTheSwing pins the fail-CLOSED direction. A nil toHit is how the engine
// spells the legitimate "no classifier authored, the swing auto-lands" case, so folding a PARSE FAILURE
// into it would turn one typo into "nothing can ever miss" for every entity using the profile.
func TestBrokenCombatProfileRefusesTheSwing(t *testing.T) {
	good := buildCombatProfile(content.CombatProfileDTO{
		Ref: "ok", ToHit: map[string]any{"dice": "1d20", "bands": []any{map[string]any{"label": "hit"}}},
	})
	require.False(t, good.broken)
	require.NotNil(t, good.toHit)

	for _, tc := range []struct {
		name string
		dto  content.CombatProfileDTO
	}{
		{"an unknown key in to_hit", content.CombatProfileDTO{Ref: "bad", ToHit: map[string]any{
			"dice": "1d20", "dice_boon": "2d20kh1", "bands": []any{map[string]any{"label": "hit"}},
		}}},
		{"an unknown key in an avoidance rung", content.CombatProfileDTO{Ref: "bad", Avoidance: []any{
			map[string]any{"dice": "1d100", "margin_minimum": float64(0), "bands": []any{map[string]any{"label": "dodge"}}},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := buildCombatProfile(tc.dto)
			require.True(t, p.broken,
				"a sub-spec that failed to parse must mark the profile broken, not silently degrade it")
		})
	}
}

// TestBrokenProfileRefusesTheSwing is the WIRING half of the fail-closed fix — separate from the
// buildCombatProfile test above, which pins only that the flag gets SET. Mutation-testing showed the
// flag being set and the swing consulting it were independently unpinned: deleting the check in
// resolveSwing left the suite green, which is precisely the auto-hit regression the flag exists to
// prevent. Both directions are asserted (attacker-broken and defender-broken).
func TestBrokenProfileRefusesTheSwing(t *testing.T) {
	for _, tc := range []struct{ name, whose string }{
		{"the attacker's profile is broken", "attacker"},
		{"the defender's profile is broken", "defender"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z, s := combatZone(t)
			// A profile with NO to_hit at all: the legitimate degenerate case that auto-hits. The broken
			// profile below must NOT behave like this one, which is the whole point.
			z.defs.combat.register("healthy", &combatProfile{})
			z.defs.combat.register("busted", &combatProfile{broken: true})

			mob := combatMob(z, s.entity, "dummy", "", 100)
			if tc.whose == "attacker" {
				s.entity.living.combatRef, mob.living.combatRef = "busted", "healthy"
			} else {
				s.entity.living.combatRef, mob.living.combatRef = "healthy", "busted"
			}
			equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})

			rng := rand.New(rand.NewSource(1))
			z.resolveSwing(s.entity, mob, 0, rng, newBudget())
			require.Equal(t, 100, resourceCurrent(mob, "hp"),
				"a broken profile must resolve NO swing — falling through would auto-hit for full damage")
		})
	}

	// The control: the same setup with two healthy (empty) profiles DOES land a blow, so the assertion
	// above cannot pass merely because the harness never swings.
	t.Run("control: healthy profiles still swing", func(t *testing.T) {
		z, s := combatZone(t)
		z.defs.combat.register("healthy", &combatProfile{})
		mob := combatMob(z, s.entity, "dummy", "", 100)
		s.entity.living.combatRef, mob.living.combatRef = "healthy", "healthy"
		equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
		z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
		require.Less(t, resourceCurrent(mob, "hp"), 100, "the control swing must actually land")
	})
}

// TestBoonChannelThroughTheSwingPipeline drives the channel through resolveSwing rather than
// resolveCheck directly — 5e advantage-on-attacks is the motivating case, and the swing path is where
// a real pack meets it. The to-hit is rigged so the neutral die MISSES and the boon die HITS, making
// the selection observable as damage rather than as an internal return value.
func TestBoonChannelThroughTheSwingPipeline(t *testing.T) {
	z, s := combatZone(t)
	z.defs.attr.register("atk_boon", &attributeDef{ref: "atk_boon"})
	z.defs.affect.register("bless", &affectDef{
		ref: "bless", name: "Blessed", stacking: stackRefresh, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "atk_boon", add: true, value: 1}},
	})

	// Neutral 1d1 totals 1 (below the DC of 2 -> miss); the boon alternative 3d1 totals 3 (-> hit).
	boonD := mustDice(t, "3d1")
	z.defs.combat.register("attacker", &combatProfile{
		toHit: &checkSpec{
			label: "Attack", dice: mustDice(t, "1d1"), boonDice: &boonD,
			boon: attrNode{ref: "$actor.atk_boon"},
			vs:   checkVs{dc: litNode{v: 2}},
			bands: []checkBand{
				{marginMin: bn(0), label: "hit"},
				{label: "miss"},
			},
		},
	})
	s.entity.living.combatRef = "attacker"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "dummy", "", 100)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	require.Equal(t, 100, resourceCurrent(mob, "hp"), "unblessed, the neutral die misses")

	applyAffect(s.entity, "bless", attachOpts{}, nil)
	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	require.Less(t, resourceCurrent(mob, "hp"), 100,
		"blessed, the boon die is selected and the same swing lands")
}

// TestRollDiceSpecHonoursKeepDirection pins the keep DIRECTION at rollDiceSpec, which is the joint the
// unit tests around it left open: dice_test.go only PARSES "2d20kl1" (asserting the kind), and the
// kept-face tests call sumKept directly. So hardcoding sumKept's `high` argument at the rollDiceSpec
// call site — every disadvantage roll in the engine silently becoming advantage — survived the entire
// package. That is the same wiring-untested-while-both-ends-are shape as the face_eq fix, one layer
// down, and it is why this test asserts magnitude AND kept together, for BOTH directions.
func TestRollDiceSpecHonoursKeepDirection(t *testing.T) {
	for _, tc := range []struct {
		notation string
		want     func(a, b int) int
	}{
		{"2d20kh1", func(a, b int) int { return max(a, b) }},
		{"2d20kl1", func(a, b int) int { return min(a, b) }},
	} {
		t.Run(tc.notation, func(t *testing.T) {
			d := mustDice(t, tc.notation)
			differed := 0
			for seed := int64(0); seed < 300; seed++ {
				mag, faces, kept := rollDiceSpec(&effectCtx{rng: rand.New(rand.NewSource(seed))}, d)
				require.Len(t, faces, 2, "seed %d", seed)
				want := tc.want(faces[0], faces[1])
				require.Equal(t, want, mag, "seed %d: faces %v", seed, faces)
				require.Equal(t, []int{want}, kept, "seed %d: faces %v", seed, faces)
				if faces[0] != faces[1] {
					differed++
				}
			}
			// The precondition: if the two dice never differed, keep-high and keep-low are
			// indistinguishable and this test would prove nothing.
			require.Greater(t, differed, 200, "the sample must contain rolls where the two dice DIFFER")
		})
	}
}

// TestResolveCheckClassifiesOnKeptFacesUnderABane is the BANE mirror of the keep-high wiring test. The
// PR's own claim has two halves — a nat-1 miss band under a boon die, and a nat-20 crit band under a
// bane die (which all-faces fired at 9.75% where 5e wants 0.25%) — and only the first had coverage.
func TestResolveCheckClassifiesOnKeptFacesUnderABane(t *testing.T) {
	z, caster, mob := boonZone(t)
	twenty := 20.0
	baneD := mustDice(t, "2d20kl1")
	spec := &checkSpec{
		dice: mustDice(t, "1d20"), baneDice: &baneD,
		bane:  attrNode{ref: "$actor.atk_bane"},
		bands: []checkBand{{faceEq: &twenty, label: "crit"}, {label: "plain"}},
	}
	applyAffect(caster.entity, "curse", attachOpts{}, nil) // select the keep-low die

	discriminating := 0
	for seed := int64(0); seed < 400; seed++ {
		c := &effectCtx{
			z: z, actor: caster.entity, source: caster.entity, target: mob,
			mag: 1, rng: rand.New(rand.NewSource(seed)),
		}
		res := resolveCheck(c, spec)
		require.Len(t, res.faces, 2, "the bane die must have been selected")

		used := min(res.faces[0], res.faces[1])
		if (res.faces[0] == 20 || res.faces[1] == 20) && used != 20 {
			discriminating++
			require.Equal(t, "plain", res.bandLabel,
				"seed %d rolled %v: a DISCARDED 20 must not crit when the check used a %d", seed, res.faces, used)
		}
		require.Equal(t, used == 20, res.bandLabel == "crit", "seed %d rolled %v", seed, res.faces)
	}
	require.Greater(t, discriminating, 20,
		"the sample must contain rolls where a DISCARDED die showed 20, or the test proves nothing")
}

// TestCheckKeySetsCannotDriftFromTheStructs makes rejectUnknownKeys' documented promise — "adding a
// field to checkSpec/checkBand means adding its key here, so the two cannot drift apart silently" —
// actually enforceable. It guards BOTH directions: a struct field with no legal key would be
// unauthorable, and a legal key with no field is a silently accepted no-op, which is precisely the
// shape the gate exists to prevent.
func TestCheckKeySetsCannotDriftFromTheStructs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		keys   map[string]bool
		fields map[string]string // struct field -> the content key that feeds it
	}{
		{
			name: "checkSpec", keys: checkSpecKeys,
			fields: map[string]string{
				"dice": "dice", "bonus": "bonus", "vs": "vs", "bands": "bands",
				"visibility": "visibility", "label": "label",
				"boon": "boon", "bane": "bane", "boonDice": "boon_dice", "baneDice": "bane_dice",
			},
		},
		{
			name: "checkBand", keys: checkBandKeys,
			fields: map[string]string{
				"min": "min", "max": "max", "marginMin": "margin_min", "marginMax": "margin_max",
				"faceEq": "face_eq", "faceCount": "face_count", "label": "label", "ops": "ops",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var typ reflect.Type
			if tc.name == "checkSpec" {
				typ = reflect.TypeOf(checkSpec{})
			} else {
				typ = reflect.TypeOf(checkBand{})
			}
			for i := 0; i < typ.NumField(); i++ {
				name := typ.Field(i).Name
				key, mapped := tc.fields[name]
				require.True(t, mapped,
					"%s.%s is a new field with no entry in this test's map — decide whether it is authorable "+
						"and either register its key in %sKeys or record it here as engine-internal", tc.name, name, tc.name)
				require.True(t, tc.keys[key],
					"%s.%s maps to content key %q, which %sKeys does not accept — the field is unauthorable",
					tc.name, name, key, tc.name)
			}
			for key := range tc.keys {
				found := false
				for _, k := range tc.fields {
					if k == key {
						found = true
						break
					}
				}
				require.True(t, found,
					"legal key %q is accepted by the parser but feeds no %s field — a silently accepted no-op",
					key, tc.name)
			}
		})
	}
}

// TestUnreachableAlternativeIsRejected closes the last silent-no-op in the channel's authoring surface.
// A boon_dice with no boon formula parses cleanly and can NEVER be selected (effectiveDice returns the
// neutral die when both formulas are nil), so the author has written a rule that looks live and is
// permanently dead — the same class the unknown-key and wrong-type gates exist to prevent, reached by
// authoring a valid key with nothing to drive it.
func TestUnreachableAlternativeIsRejected(t *testing.T) {
	bands := []any{map[string]any{"label": "hit"}}

	for _, tc := range []struct {
		name, missing string
		m             map[string]any
	}{
		{"boon_dice with no boon formula", "boon", map[string]any{
			"dice": "1d20", "boon_dice": "2d20kh1", "bands": bands,
		}},
		{"bane_dice with no bane formula", "bane", map[string]any{
			"dice": "1d20", "bane_dice": "2d20kl1", "bands": bands,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseCheckSpec(tc.m)
			require.Error(t, err, "an alternative die that can never be selected must not parse silently")
			require.Contains(t, err.Error(), tc.missing)
		})
	}

	t.Run("a formula with no alternative die is legal", func(t *testing.T) {
		// The mirror case is NOT an error: authoring only `boon` with no `boon_dice` is the documented
		// "this direction has no alternative expression" case, and a shared bane_dice may be the point.
		_, err := parseCheckSpec(map[string]any{
			"dice": "1d20", "boon": []any{"attr", "x"}, "bands": bands,
		})
		require.NoError(t, err)
	})
}

// TestRollUnderBoonMakesTheActorBetter is the DELIVERABLE behind "the engine learns no direction".
// Every other test in this file asserts which NOTATION comes back, which is the seam; the claim the
// design actually rests on is an outcome — that a content-authored boon on a ROLL-UNDER ladder makes
// the actor better at dodging. An engine that synthesized keep-highest for "advantage" would return a
// plausible-looking 2d100kh1 and pass every notation assertion while measurably inverting the ladder
// (measured on the rejected design: success 40% -> 16%). Deterministic: fixed seeds, no wall clock.
func TestRollUnderBoonMakesTheActorBetter(t *testing.T) {
	z, caster, mob := boonZone(t)
	// resolveAttr returns 0 for an UNREGISTERED attribute, so the def must exist for the band edge to
	// read anything — a base override alone would leave the threshold at 0 and nothing would ever
	// succeed, which is a silently-passing-nothing shape worth being explicit about.
	z.defs.attr.register("dodge", &attributeDef{ref: "dodge", base: litNode{v: 40}}) // roll 40 or under

	boonD := mustDice(t, "2d100kl1") // roll-under: a BOON keeps the LOW die
	ladder := &checkSpec{
		label: "Dodge", dice: mustDice(t, "1d100"), boonDice: &boonD,
		boon: attrNode{ref: "$actor.atk_boon"},
		bands: []checkBand{
			{max: attrNode{ref: "$actor.dodge"}, label: "dodge"},
			{label: "fail"},
		},
	}

	rate := func() float64 {
		hits := 0
		const n = 4000
		for seed := int64(0); seed < n; seed++ {
			c := &effectCtx{
				z: z, actor: caster.entity, source: caster.entity, target: mob,
				mag: 1, rng: rand.New(rand.NewSource(seed)),
			}
			if resolveCheck(c, ladder).bandLabel == "dodge" {
				hits++
			}
		}
		return float64(hits) / n
	}

	neutral := rate()
	applyAffect(caster.entity, "bless", attachOpts{}, nil)
	boosted := rate()
	t.Logf("roll-under dodge rate: neutral %.3f -> boon %.3f", neutral, boosted)

	// Oracles from first principles, NOT from the implementation: a single d100 succeeds at p = 0.40;
	// keeping the lower of two succeeds at 1-(1-p)^2 = 0.64.
	require.InDelta(t, 0.40, neutral, 0.03, "the neutral 1d100 ladder succeeds at ~the skill value")
	require.InDelta(t, 0.64, boosted, 0.03, "keep-LOW of two d100 succeeds at 1-(1-p)^2")
	require.Greater(t, boosted, neutral+0.15,
		"a content-authored roll-under boon must make the actor BETTER at dodging — an engine that "+
			"assumed higher-is-better would have made this WORSE")
}
