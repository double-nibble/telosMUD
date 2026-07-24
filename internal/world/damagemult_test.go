package world

import (
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
