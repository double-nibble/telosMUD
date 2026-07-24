package world

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// damagemult_test.go covers the per-target damage resistance/vulnerability/immunity multiplier (#537).
//
// The premise was confirmed by a panel: `mitigate` reads only the damage TYPE's GLOBAL resist scalar
// (one value per type, for everyone), and `soak` is a flat subtraction that cannot express "half of a
// variable roll", cannot guarantee 0 on a big hit, and cannot express vulnerability at all. The panel
// also REJECTED the obvious attribute-namespace design (a `damage_taken_mult_<type>` attribute): an
// absent attribute reads 0 not 1, and gear can only feed FLAT modifiers, so two flat resistance rings
// would sum to permanent immunity. This is the map-on-Affected design it recommended instead: absence
// is identity-1 STRUCTURALLY (a map miss), and the engine owns the composition rule.

// multZone registers hp/fire/cold + a set of resistance/vulnerability affects.
func multZone(t *testing.T) (*Zone, *Entity, *Entity) {
	t.Helper()
	z, caster := abilityTestZone(t)
	z.defs.dmg.register("cold", &damageTypeDef{ref: "cold"})
	reg := func(ref string, mult map[string]float64) {
		z.defs.affect.register(ref, &affectDef{
			ref: ref, name: ref, stacking: stackRefresh, maxStacks: 1, duration: 100,
			damageTakenMult: mult,
		})
	}
	reg("resist_fire", map[string]float64{"fire": 0.5})
	reg("resist_fire2", map[string]float64{"fire": 0.5})
	reg("vuln_fire", map[string]float64{"fire": 2})
	reg("immune_fire", map[string]float64{"fire": 0})
	reg("mixed", map[string]float64{"fire": 0.5, "cold": 2})
	mob := makeMobTarget(z, caster.entity, "goblin")
	return z, caster.entity, mob
}

// TestMitigateAppliesPerTargetMultiplier is the core behaviour: half / double / zero, per target,
// against a variable raw amount — the three things soak and the global matrix could not express.
func TestMitigateAppliesPerTargetMultiplier(t *testing.T) {
	z, _, mob := multZone(t)

	// No affect: the multiplier is identity, so mitigate is byte-for-byte unchanged.
	require.Equal(t, 40, mitigate(mob, 40, "fire"), "no multiplier -> unchanged")

	applyAffect(mob, "resist_fire", attachOpts{}, nil)
	require.Equal(t, 20, mitigate(mob, 40, "fire"), "resistance halves a variable amount")
	require.Equal(t, 2, mitigate(mob, 5, "fire"), "half of 5 = 2.5 -> floor 2 (5e rounds down)")

	// Immunity guarantees zero on an arbitrarily large hit — soak (a flat subtraction) never could.
	other := makeMobTarget(z, mob, "wraith")
	applyAffect(other, "immune_fire", attachOpts{}, nil)
	require.Equal(t, 0, mitigate(other, 100000, "fire"), "immunity zeroes any hit")

	// Vulnerability doubles — unexpressible before this change by ANY path (Lua reactions are reduce-only).
	vuln := makeMobTarget(z, mob, "iceling")
	applyAffect(vuln, "vuln_fire", attachOpts{}, nil)
	require.Equal(t, 80, mitigate(vuln, 40, "fire"), "vulnerability doubles")
}

// TestMultiplierIsPerTargetNotGlobal is the property the issue is named for: two entities in the same
// zone take different damage from the same blow, which the global type matrix cannot do.
func TestMultiplierIsPerTargetNotGlobal(t *testing.T) {
	z, _, resistant := multZone(t)
	normal := makeMobTarget(z, resistant, "human")
	applyAffect(resistant, "resist_fire", attachOpts{}, nil)

	require.Equal(t, 20, mitigate(resistant, 40, "fire"), "the resistant one takes half")
	require.Equal(t, 40, mitigate(normal, 40, "fire"), "the normal one, same blow, takes full")
}

// TestMultiplierComposition pins the engine-owned combination rule: product across active affects,
// with immunity dominating and resist+vuln cancelling.
func TestMultiplierComposition(t *testing.T) {
	z, _, mob := multZone(t)

	applyAffect(mob, "resist_fire", attachOpts{}, nil)
	applyAffect(mob, "resist_fire2", attachOpts{}, nil)
	require.Equal(t, 10, mitigate(mob, 40, "fire"), "two 0.5 resistances compose by product to 0.25")

	applyAffect(mob, "vuln_fire", attachOpts{}, nil) // 0.25 * 2 = 0.5
	require.Equal(t, 20, mitigate(mob, 40, "fire"), "adding a vulnerability multiplies in")

	applyAffect(mob, "immune_fire", attachOpts{}, nil) // * 0
	require.Equal(t, 0, mitigate(mob, 40, "fire"), "immunity (×0) dominates the product")
	_ = z
}

// TestMultiplierIsPerType pins that a multiplier applies only to its declared type, not to others.
func TestMultiplierIsPerType(t *testing.T) {
	_, _, mob := multZone(t)
	applyAffect(mob, "mixed", attachOpts{}, nil) // fire 0.5, cold 2
	require.Equal(t, 20, mitigate(mob, 40, "fire"), "fire halved")
	require.Equal(t, 80, mitigate(mob, 40, "cold"), "cold doubled")
	require.Equal(t, 40, mitigate(mob, 40, "poison"), "an unlisted type is unaffected (×1)")
}

// TestMultiplierOrdersAfterSoak pins the documented order: (raw × globalMatrix − soak) × mult. A
// vulnerability amplifies the POST-soak number, and resistance halves after soak.
func TestMultiplierOrdersAfterSoak(t *testing.T) {
	z, _, mob := multZone(t)
	z.defs.attr.register("soak_fire", &attributeDef{ref: "soak_fire", base: litNode{v: 10}})
	applyAffect(mob, "vuln_fire", attachOpts{}, nil)
	// raw 40, soak 10 -> 30, then ×2 = 60. If the mult ran BEFORE soak it would be (40×2)−10 = 70.
	require.Equal(t, 60, mitigate(mob, 40, "fire"), "(40 - 10 soak) * 2 = 60, not (40*2) - 10")
}

// TestMultiplierCeiling caps a stacked vulnerability so a malicious pack cannot amplify a blow without
// bound.
func TestMultiplierCeiling(t *testing.T) {
	z, _, mob := multZone(t)
	// Register several ×10 vulnerabilities; their product would be enormous.
	for i := 0; i < 5; i++ {
		ref := "big" + string(rune('a'+i))
		z.defs.affect.register(ref, &affectDef{
			ref: ref, duration: 100, maxStacks: 1,
			damageTakenMult: map[string]float64{"fire": 10},
		})
		applyAffect(mob, ref, attachOpts{}, nil)
	}
	require.Equal(t, damageTakenMultCeiling, damageTakenMult(mob, "fire"),
		"the composed multiplier is capped at the engine ceiling")
	require.Equal(t, int(40*damageTakenMultCeiling), mitigate(mob, 40, "fire"))
}

// TestNegativeMultiplierClampsToImmunity pins that a negative composed multiplier cannot turn damage
// into healing — it clamps to 0.
func TestNegativeMultiplierClampsToImmunity(t *testing.T) {
	z, _, mob := multZone(t)
	z.defs.affect.register("neg", &affectDef{
		ref: "neg", duration: 100, maxStacks: 1,
		damageTakenMult: map[string]float64{"fire": -3},
	})
	applyAffect(mob, "neg", attachOpts{}, nil)
	require.Equal(t, 0.0, damageTakenMult(mob, "fire"), "a negative multiplier clamps to 0, never heals")
	require.Equal(t, 0, mitigate(mob, 40, "fire"))
}

// TestMultiplierRecomputesOnExpire pins that removing the affect restores identity — the map is
// recomputed on expire, not left stale.
func TestMultiplierRecomputesOnExpire(t *testing.T) {
	_, _, mob := multZone(t)
	applyAffect(mob, "resist_fire", attachOpts{}, nil)
	require.Equal(t, 20, mitigate(mob, 40, "fire"))

	a, _ := Get[*Affected](mob)
	for _, inst := range append([]*affectInstance(nil), a.list...) {
		a.expire(mob, inst, nil)
	}
	require.Equal(t, 40, mitigate(mob, 40, "fire"), "with the affect gone, the multiplier is identity again")
}

// TestVulnerabilityIsGatedAsHarm is the SECURITY property. A vulnerability affect is a debuff authored
// with no modifier and no prevents tag, so the sign-based harm heuristic would miss it — exactly the
// class #511/#513 had to close. Here the polarity is KNOWN (>1 is harm), so it is a direct check.
func TestVulnerabilityIsGatedAsHarm(t *testing.T) {
	vuln := &affectDef{ref: "vuln", damageTakenMult: map[string]float64{"fire": 2}}
	resist := &affectDef{ref: "resist", damageTakenMult: map[string]float64{"fire": 0.5}}
	immune := &affectDef{ref: "immune", damageTakenMult: map[string]float64{"fire": 0}}

	require.True(t, affectIsDetrimental(vuln, harmPolarity{}), "a vulnerability is a debuff")
	require.False(t, affectSurvivesRespawn(vuln, harmPolarity{}), "...and must not survive respawn")

	require.False(t, affectIsDetrimental(resist, harmPolarity{}), "a resistance is a BUFF — ward an ally ungated")
	require.True(t, affectSurvivesRespawn(resist, harmPolarity{}), "...and a blessing survives respawn")
	require.False(t, affectIsDetrimental(immune, harmPolarity{}), "immunity is a buff too")
	require.True(t, affectSurvivesRespawn(immune, harmPolarity{}))
}

// TestVulnerabilityGateIsWiredIntoApplyAffect drives a real apply_affect: a vulnerability at a
// non-consenting player must be refused by the PvP gate, which only happens if the derived-harm path
// classifies it.
func TestVulnerabilityGateIsWiredIntoApplyAffect(t *testing.T) {
	z, caster, _ := multZone(t)
	victim := makePlayerTargetInRoom(z, caster, "Victim")
	z.defs.affect.register("hex", &affectDef{
		ref: "hex", name: "Hexed", stacking: stackRefresh, maxStacks: 1, duration: 100,
		damageTakenMult: map[string]float64{"fire": 3},
	})
	c := &effectCtx{z: z, actor: caster, source: caster, target: victim.entity, mag: 1, disp: dispNeutral}
	require.NoError(t, opApplyAffect(c, &effectOp{kind: "apply_affect", affect: "hex"}))

	require.False(t, hasAffect(victim.entity, "hex"),
		"a vulnerability debuff at a non-consenting player must be refused by the PvP gate")
	require.Equal(t, 1.0, damageTakenMult(victim.entity, "fire"), "and no multiplier landed")
}

// TestLintDamageTakenMultTypes pins that a multiplier on an unregistered damage type is flagged — a
// silent no-op an author would otherwise never notice (the multiplier just never applies).
func TestLintDamageTakenMultTypes(t *testing.T) {
	z, _ := abilityTestZone(t)
	z.defs.dmg.register("cold", &damageTypeDef{ref: "cold"})
	z.defs.affect.register("good", &affectDef{ref: "good", damageTakenMult: map[string]float64{"fire": 0.5, "cold": 2}})
	z.defs.affect.register("typo", &affectDef{ref: "typo", damageTakenMult: map[string]float64{"frost": 0.5, "clod": 2}})

	misses := lintDamageTakenMultTypes(z.defs)
	require.Len(t, misses, 2, "both typo'd types flagged, neither real one: %+v", misses)
	// Deterministic order: sorted by affect then type.
	require.Equal(t, "typo", misses[0].affect)
	require.Equal(t, "clod", misses[0].dmgType)
	require.Equal(t, "frost", misses[1].dmgType)
}

// TestVulnerabilityThresholdBoundary pins that the harm test is strictly > 1: a multiplier of exactly
// 1.0 is a no-op (not harm), and a resistance (< 1) is a buff. Only > 1 is a vulnerability. Without
// this, a `>= 1` threshold would wrongly gate a no-op or (with a looser bug) a resistance.
func TestVulnerabilityThresholdBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		mult float64
		harm bool
	}{
		{"exactly 1.0 is a no-op, not harm", 1.0, false},
		{"just above 1 is a vulnerability", 1.0000001, true},
		{"just below 1 is a resistance (buff)", 0.9999999, false},
		{"0.5 resistance is a buff", 0.5, false},
		{"0 immunity is a buff", 0, false},
		{"2 vulnerability is harm", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := &affectDef{ref: "x", damageTakenMult: map[string]float64{"fire": tc.mult}}
			require.Equal(t, tc.harm, affectIsDetrimental(def, harmPolarity{}))
			require.Equal(t, !tc.harm, affectSurvivesRespawn(def, harmPolarity{}),
				"a non-harmful multiplier affect survives respawn; a harmful one does not")
		})
	}
}

// TestTwoNegativesDoNotComposeToAmplification is the P1 SECURITY test. Two `{fire:-3}` affects — each
// classified a benign buff by the harm gate — would, under a read-time-only clamp, compose to +9 and
// amplify incoming damage 9× on a non-consenting target. Per-factor normalization at COMPOSITION makes
// each factor 0 (immunity), so the product is 0, not 9.
func TestTwoNegativesDoNotComposeToAmplification(t *testing.T) {
	z, _, mob := multZone(t)
	for _, ref := range []string{"neg1", "neg2"} {
		z.defs.affect.register(ref, &affectDef{
			ref: ref, duration: 100, maxStacks: 1,
			damageTakenMult: map[string]float64{"fire": -3},
		})
		applyAffect(mob, ref, attachOpts{}, nil)
	}
	require.Equal(t, 0.0, damageTakenMult(mob, "fire"),
		"two negatives must compose to immunity (0), NOT amplification (+9)")
	require.Equal(t, 0, mitigate(mob, 40, "fire"))

	// And each negative affect on its own is classified a BUFF (immunity), not harm — so this is not a
	// gate bypass: neither affect is a vulnerability, and composition cannot manufacture one.
	neg := &affectDef{ref: "neg", damageTakenMult: map[string]float64{"fire": -3}}
	require.False(t, affectIsDetrimental(neg, harmPolarity{}), "a negative is immunity-like, a buff")
}

// TestNaNMultiplierIsNeutralized pins that a NaN factor (authorable as `.nan` in YAML, or composable
// as 0×Inf) reads as identity, never reaching an int() conversion where int(NaN) is
// implementation-defined — a determinism hazard.
func TestNaNMultiplierIsNeutralized(t *testing.T) {
	z, _, mob := multZone(t)
	z.defs.affect.register("nan", &affectDef{
		ref: "nan", duration: 100, maxStacks: 1,
		damageTakenMult: map[string]float64{"fire": math.NaN()},
	})
	applyAffect(mob, "nan", attachOpts{}, nil)
	require.Equal(t, 1.0, damageTakenMult(mob, "fire"), "a NaN factor is ignored (identity)")
	require.Equal(t, 40, mitigate(mob, 40, "fire"), "so the blow is unscaled, not int(NaN)")

	// Pin the COMPOSITION layer independently of the reader's defensive clamp: the composed map itself
	// must never contain a non-finite value, so any future consumer that bypasses the reader is safe.
	a, _ := Get[*Affected](mob)
	require.False(t, math.IsNaN(a.damageMult["fire"]), "the composed map must never store NaN")
	require.False(t, math.IsInf(a.damageMult["fire"], 0), "...nor Inf")

	// The 0 × Inf composition case: an immunity plus an Inf-vulnerability. Inf clamps to the ceiling
	// per-factor, so 0 × ceiling = 0, not NaN.
	z.defs.affect.register("imm", &affectDef{ref: "imm", duration: 100, maxStacks: 1, damageTakenMult: map[string]float64{"cold": 0}})
	z.defs.affect.register("inf", &affectDef{ref: "inf", duration: 100, maxStacks: 1, damageTakenMult: map[string]float64{"cold": math.Inf(1)}})
	applyAffect(mob, "imm", attachOpts{}, nil)
	applyAffect(mob, "inf", attachOpts{}, nil)
	require.False(t, math.IsNaN(damageTakenMult(mob, "cold")), "0 × Inf must not surface as NaN")
	require.Equal(t, 0.0, damageTakenMult(mob, "cold"), "immunity dominates: 0 × ceiling = 0")
}

// TestMixedAffectThroughApplyGate pins the boundary case: an affect that both resists and makes
// vulnerable is a debuff overall and must be refused at a non-consenting player.
func TestMixedAffectThroughApplyGate(t *testing.T) {
	z, caster, _ := multZone(t)
	victim := makePlayerTargetInRoom(z, caster, "Victim")
	z.defs.affect.register("mixedhex", &affectDef{
		ref: "mixedhex", name: "Mixed Hex", stacking: stackRefresh, maxStacks: 1, duration: 100,
		damageTakenMult: map[string]float64{"fire": 0.5, "cold": 2},
	})
	c := &effectCtx{z: z, actor: caster, source: caster, target: victim.entity, mag: 1, disp: dispNeutral}
	require.NoError(t, opApplyAffect(c, &effectOp{kind: "apply_affect", affect: "mixedhex"}))
	require.False(t, hasAffect(victim.entity, "mixedhex"),
		"an affect with ANY vulnerability is a debuff and must be gated, even if it also resists")
}

// TestLintDamageTakenMultValues pins the value lint: non-finite and negative values are flagged.
func TestLintDamageTakenMultValues(t *testing.T) {
	z, _ := abilityTestZone(t)
	z.defs.dmg.register("cold", &damageTypeDef{ref: "cold"})
	z.defs.affect.register("ok", &affectDef{ref: "ok", damageTakenMult: map[string]float64{"fire": 0.5, "cold": 2}})
	z.defs.affect.register("bad", &affectDef{ref: "bad", damageTakenMult: map[string]float64{
		"fire": math.NaN(), "cold": -3,
	}})
	misses := lintDamageTakenMultValues(z.defs)
	require.Len(t, misses, 2, "%+v", misses)
	require.Equal(t, "bad", misses[0].affect)
	require.Equal(t, "cold", misses[0].dmgType)
	require.Equal(t, "negative", misses[0].reason)
	require.Equal(t, "fire", misses[1].dmgType)
	require.Equal(t, "non-finite", misses[1].reason)
}
