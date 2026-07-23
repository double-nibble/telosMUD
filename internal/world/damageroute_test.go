package world

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/content"
)

// damageroute_test.go exercises #405: DAMAGE-TYPE -> POOL ROUTING. Before it, the ONLY way to aim a blow at
// a pool was an explicit per-op `resource`, so a damage KIND could not name its own track: every psychic
// spell, every mind-flayer tentacle, every third-party source and every melee swing landed on the primary
// vital no matter what the type meant. A pack could not express "psychic damage hits sanity" at all — it
// could only ask every author of every op to remember to repeat `resource: sanity`.
//
// Precedence under test: op.resource ?? damage_type.target_resource ?? the primary vital.

// registerRoutedType registers a damage type that routes to `pool` (#405).
func registerRoutedType(z *Zone, ref, pool string) {
	z.defs.dmg.register(ref, &damageTypeDef{ref: ref, resist: map[string]float64{}, targetResource: pool})
}

// --- The three tiers of precedence ---------------------------------------------------------------

// TestDamageTypeRoutesToItsPool is the core #405 case: a blow with NO explicit resource lands on the pool
// its damage TYPE names, not on the primary vital.
func TestDamageTypeRoutesToItsPool(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "sanity", "max_sanity", 100, nil, false)
	registerRoutedType(z, "psychic", "sanity")

	mob := combatMob(z, s.entity, "investigator", "", 100)
	setResourceCurrent(mob, "sanity", 100)

	dealTo(z, s.entity, mob, 30, "psychic", "") // no explicit resource — the TYPE routes it

	require.Equal(t, 70, resourceCurrent(mob, "sanity"), "the psychic blow must land on the routed pool")
	require.Equal(t, 100, resourceCurrent(mob, "hp"), "hp must be untouched — routing failed if it moved")
}

// TestOpResourceOverridesTheTypeRoute pins the top tier. An op that names its own `resource` wins over the
// type's route — which is what lets a single ability aim a psychic blow at hp deliberately, and (the case
// that matters for #407) lets a carry-over op escape the type that routed the blow into the pool it is
// spilling OUT of, instead of looping straight back in.
func TestOpResourceOverridesTheTypeRoute(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "sanity", "max_sanity", 100, nil, false)
	registerRoutedType(z, "psychic", "sanity")

	mob := combatMob(z, s.entity, "investigator", "", 100)
	setResourceCurrent(mob, "sanity", 100)

	dealTo(z, s.entity, mob, 30, "psychic", "hp") // explicit resource beats the type route

	require.Equal(t, 70, resourceCurrent(mob, "hp"), "an explicit op resource must win over the type's route")
	require.Equal(t, 100, resourceCurrent(mob, "sanity"), "the routed pool must be untouched")
}

// TestUnroutedDamageStillHitsThePrimaryVital pins the bottom tier — the legacy path, unchanged. A type with
// no route, and an untyped blow, both land on the primary vital exactly as before #405.
func TestUnroutedDamageStillHitsThePrimaryVital(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "sanity", "max_sanity", 100, nil, false)
	registerRoutedType(z, "psychic", "sanity")

	for _, dmgType := range []string{"slash", ""} {
		t.Run("type="+dmgType, func(t *testing.T) {
			mob := combatMob(z, s.entity, "guard", "", 100)
			setResourceCurrent(mob, "sanity", 100)

			dealTo(z, s.entity, mob, 25, dmgType, "")

			require.Equal(t, 75, resourceCurrent(mob, "hp"), "unrouted damage must still hit the primary vital")
			require.Equal(t, 100, resourceCurrent(mob, "sanity"), "an unrelated routed pool must not be touched")
		})
	}
}

// --- Natural immunity: the sharp edge of the feature ---------------------------------------------

// TestTypeRoutedBlowOnACapacitylessPoolIsDiscarded is the security-shaped case, and the reason the immunity
// discard keys on ROUTED rather than on `resource != ""`. A mindless construct with max_sanity 0 takes a
// psychic hit: the blow must be discarded ENTIRELY — before mitigation, threat and the checkpoint — and must
// never write a current on that pool.
//
// Leaving the type-routed path unguarded (the naive implementation) writes a phantom 0 there, and that
// phantom is a stored ONE-WAY DOOR: resourceCurrent's "absent reads as full" stops applying, so if the pool
// ever gains capacity the entity reads permanently empty on it — which for a vital pool means the next
// point of damage is an instant kill. It also makes entityHasResource true forever (silently subscribing
// the entity to that resource's event handlers) and persists through the character save.
func TestTypeRoutedBlowOnACapacitylessPoolIsDiscarded(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "sanity", "max_sanity", 0, nil, false) // NO capacity: a mind-less construct
	registerRoutedType(z, "psychic", "sanity")

	mob := combatMob(z, s.entity, "construct", "", 100)

	dealt := dealTo(z, s.entity, mob, 30, "psychic", "")

	require.Zero(t, dealt, "a type-routed blow at a pool the target has no capacity for must be discarded")
	require.Equal(t, 100, resourceCurrent(mob, "hp"), "the discard must NOT fall back onto hp")
	require.Zero(t, mob.living.threat[s.entity], "an immune blow must build no threat (the discard precedes it)")
	// The phantom-write assertion: nothing may be stored for that pool at all.
	_, stored := mob.living.resCur["sanity"]
	require.False(t, stored,
		"a phantom current was written on a capacity-less routed pool — a stored one-way door that reads permanently empty if the pool ever gains capacity")
	require.False(t, entityHasResource(mob, "sanity"),
		"the phantom write silently subscribed the entity to that resource's handlers")
}

// TestRoutedTypeIsSafeForExistingContent is the "can I ship this in a shared pack" property, asserted as a
// behaviour rather than a promise. Content that predates the routed type — an ordinary mob with no sanity
// capacity — is completely unaffected by a psychic source, with no engine predicate and no content check.
func TestRoutedTypeIsSafeForExistingContent(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "sanity", "max_sanity", 0, nil, false)
	registerRoutedType(z, "psychic", "sanity")

	rat := combatMob(z, s.entity, "rat", "", 20)
	before := resourceCurrent(rat, "hp")

	for i := 0; i < 5; i++ {
		// Assert the RETURN, not just the aftermath. Under the naive discard (`resource != ""`) the blow is
		// not discarded at all: it reports its full damage and writes a phantom current — the hp and
		// not-dead assertions below both still hold, so without this line the test is false-green and passes
		// the very implementation it exists to reject.
		require.Zero(t, dealTo(z, s.entity, rat, 100, "psychic", ""),
			"blow %d must be DISCARDED, not merely harmless", i)
	}

	require.Equal(t, before, resourceCurrent(rat, "hp"), "a pre-existing mob must be untouched by a routed damage kind")
	require.NotEqual(t, posDead, position(rat), "and certainly must not die of it")
	_, stored := rat.living.resCur["sanity"]
	require.False(t, stored, "a phantom current was written on a mob that has no such track")
	require.False(t, entityHasResource(rat, "sanity"), "the mob was silently subscribed to that resource's handlers")
}

// TestCapacityLostMidBlowIsStillDiscarded closes the TOCTOU the security review found. The discard reads
// the pool's max BEFORE applyDamageReaction and the write happens AFTER, so a reaction that drops the routed
// pool's derived max to 0 mid-blow ("shatter its mind, then the psychic hit has nothing to land on" — a
// perfectly natural thing to author) leaves the earlier check stale.
//
// Without the re-check the blow stores a current on a pool with NO capacity, and that is worse than it
// sounds: setResourceCurrent only clamps the TOP end when max > 0, so the pool stores and reads back a
// POSITIVE value at max 0 — and once anything is stored, resourceCurrent's "absent reads as full" is gone
// for good. When capacity returns the entity reads permanently empty, and on a VITAL pool the next point of
// damage kills. The TOCTOU predates #405 (an op-level route reaches it identically), but #405 takes the
// exposed traffic from "ops an author deliberately wrote" to every blow carrying a routed type.
func TestCapacityLostMidBlowIsStillDiscarded(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "sanity", "max_sanity", 100, nil, false)
	registerRoutedType(z, "psychic", "sanity")
	// An affect that zeroes the pool's derived max, applied by an OnDamageTaken reaction — i.e. AFTER the
	// discard check and BEFORE the write.
	z.defs.affect.register("mindless", &affectDef{
		ref: "mindless", stacking: stackIgnore, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "max_sanity", add: true, value: -100}},
	})
	z.defs.res.register("shell", &resourceDef{
		ref: "shell", maxAttr: "max_hp",
		onEvent: map[eventKind][]effectOp{
			evOnDamageTaken: {{kind: "apply_affect", affect: "mindless", tgt: "self"}},
		},
	})

	mob := combatMob(z, s.entity, "brittle", "", 100)
	setResourceCurrent(mob, "sanity", 100)

	dealTo(z, s.entity, mob, 30, "psychic", "")

	require.Zero(t, resourceMax(mob, "sanity"), "precondition: the handler must actually have zeroed the max")
	// The property asserted is the READ, not the store, and that is deliberate: the handler can zero the max
	// EITHER side of the write (a reaction runs before it, an event handler after), so no pre-write check
	// can cover both. What must hold either way is that a pool with no capacity reads as holding nothing.
	require.Zero(t, resourceCurrent(mob, "sanity"),
		"a pool with NO capacity read back a positive current — the invariant the routing/immunity story rests on")
	require.False(t, poolDepleted(mob, "sanity"), "and it is not 'depleted' either — it simply has no track")
}

// TestCapacityRestoredAfterAMaxDropKeepsWhatItHeld is the other half of the accessor rule, and the reason
// the fix is a read-time floor rather than deleting the stored value. A TEMPORARY max-0 debuff must not be a
// free refill: when capacity comes back the pool holds what it held before, not full. (Deleting on write
// would have made every such debuff a full restore, which is a neat exploit: drop your own max to 0, let it
// lapse, come back topped up.)
func TestCapacityRestoredAfterAMaxDropKeepsWhatItHeld(t *testing.T) {
	z, s := combatZone(t)
	z.defs.attr.register("max_sanity", &attributeDef{ref: "max_sanity", base: litNode{v: 100}})
	z.defs.res.register("sanity", &resourceDef{ref: "sanity", maxAttr: "max_sanity"})
	z.defs.affect.register("mindless", &affectDef{
		ref: "mindless", stacking: stackIgnore, maxStacks: 1, duration: 100,
		modifiers: []affectModifier{{attr: "max_sanity", add: true, value: -100}},
	})

	mob := combatMob(z, s.entity, "scholar", "", 100)
	setResourceCurrent(mob, "sanity", 60)

	inst := applyAffect(mob, "mindless", attachOpts{}, nil)
	require.NotNil(t, inst, "precondition: the debuff attached")
	require.Zero(t, resourceCurrent(mob, "sanity"), "while capacity is 0 the pool reads as holding nothing")

	affectedComponent(mob).expire(mob, inst, nil)
	require.Equal(t, 100, resourceMax(mob, "sanity"), "precondition: capacity is back")
	require.Equal(t, 60, resourceCurrent(mob, "sanity"),
		"the pool must hold what it held — a temporary max-0 debuff is not a free refill")
}

// --- The routing reaches damage the PACK DID NOT AUTHOR ------------------------------------------

// TestSwingRoutesByWeaponDamageType is the case that motivates putting the resolution in dealDamage rather
// than in the op handler. A melee swing carries the WEAPON's damage type but never a resource
// (buildSwingDamageOp sets none), so before #405 a psychic natural weapon — a mind-flayer's tentacles — could
// not reach sanity at all. Routing in the shared funnel makes the swing path work with no swing-path change.
func TestSwingRoutesByWeaponDamageType(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "sanity", "max_sanity", 100, nil, false)
	registerRoutedType(z, "psychic", "sanity")
	z.defs.combat.register("psi", autoHitProfile(nil))
	s.entity.living.combatRef = "psi"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "psychic"})

	mob := combatMob(z, s.entity, "victim", "", 100)
	setResourceCurrent(mob, "sanity", 100)
	z.startFight(s.entity, mob)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())

	require.Less(t, resourceCurrent(mob, "sanity"), 100, "a swing with a psychic weapon must route to sanity")
	require.Equal(t, 100, resourceCurrent(mob, "hp"), "the swing must NOT have landed on hp")
}

// TestLuaDamageRoutesByType covers the other unauthorable source: a Lua h:damage{type=...} with no resource.
// The load-time lints cannot see a runtime-supplied type, so the routing has to hold at the funnel — which
// it does precisely because the resolution lives in dealDamage and not in the data-op handler.
func TestLuaDamageRoutesByType(t *testing.T) {
	z, s := combatZone(t)
	registerPool(z, "sanity", "max_sanity", 100, nil, false)
	registerRoutedType(z, "psychic", "sanity")

	mob := combatMob(z, s.entity, "victim", "", 100)
	setResourceCurrent(mob, "sanity", 100)

	// h:damage's shape: an amount + a type, no resource (luaharm.go passes "" through to dealDamage).
	c := &effectCtx{
		z: z, actor: s.entity, source: s.entity, target: mob,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	dealDamage(c, mob, 40, "psychic", "")

	require.Equal(t, 60, resourceCurrent(mob, "sanity"), "a Lua-issued typed blow must route by type too")
	require.Equal(t, 100, resourceCurrent(mob, "hp"))
}

// --- Routing composes with #406 and #407 ---------------------------------------------------------

// TestRoutedDamageFiresTheRoutedPoolsHook is the whole Round-42 stack in one blow: a psychic source routes
// to sanity by TYPE (#405), sanity bottoms out and runs its own NON-LETHAL hook (#406), and the hook reads
// how far past 0 the blow went (#407). This is the Call of Cthulhu shape, authored without a single
// per-op `resource`.
func TestRoutedDamageFiresTheRoutedPoolsHook(t *testing.T) {
	z, s := depletionZone(t)
	z.defs.affect.register("insane", &affectDef{ref: "insane", name: "Insane", stacking: stackIgnore, maxStacks: 1, duration: 50})
	registerPool(z, "spilled", "max_spilled", 500, nil, false)
	registerPool(z, "sanity", "max_sanity", 100, append(
		countingHook(),
		effectOp{kind: "apply_affect", affect: "insane", tgt: "self"},
		effectOp{kind: "deal_damage", tgt: "self", resource: "spilled", amount: 0, bonus: attrNode{ref: "$depletion.overflow"}},
	), false)
	registerRoutedType(z, "psychic", "sanity")

	mob := combatMob(z, s.entity, "investigator", "", 100)
	room := mob.location
	setResourceCurrent(mob, "sanity", 10)
	setResourceCurrent(mob, "spilled", 500)
	setResourceCurrent(mob, "fired", 0)

	dealTo(z, s.entity, mob, 30, "psychic", "") // TYPE-routed, no explicit resource anywhere

	require.Equal(t, 1, firedCount(mob), "the routed pool's own hook must run")
	require.True(t, hasAffect(mob, "insane"), "the non-lethal consequence landed")
	require.Equal(t, 480, resourceCurrent(mob, "spilled"), "the hook read the overflow (30 onto a pool holding 10)")
	require.NotEqual(t, posDead, position(mob), "a Sanity break is NOT a death")
	require.Nil(t, roomCorpse(room))
	require.Equal(t, 100, resourceCurrent(mob, "hp"), "hp untouched throughout")
}

// --- The lints -----------------------------------------------------------------------------------

// TestLintDamageTypeResources pins the wide-blast-radius lint. A typo'd target_resource does not break one
// op — it silently routes EVERY blow of that damage kind into the immunity discard, so the whole kind does
// nothing everywhere. A correctly-spelled route, and a type with no route at all, must stay silent.
func TestLintDamageTypeResources(t *testing.T) {
	z, _ := combatZone(t)
	registerPool(z, "sanity", "max_sanity", 100, nil, false)
	registerRoutedType(z, "psychic", "sanity")               // fine
	registerRoutedType(z, "eldritch", "santiy")              // typo
	z.defs.dmg.register("fire", &damageTypeDef{ref: "fire"}) // no route

	got := lintDamageTypeResources(z.defs)

	require.Len(t, got, 1, "expected exactly the typo'd type: %+v", got)
	require.Equal(t, "eldritch", got[0].dmgType)
	require.Equal(t, "santiy", got[0].resource)
}

// TestLintDealDamageTypes pins the companion lint. Nothing validated `type` before, because an unknown type
// was merely inert; once a type can carry a ROUTE, a typo silently loses the routing too and the blow falls
// back to the primary vital. It must walk into flow branches, and must not flag untyped damage.
func TestLintDealDamageTypes(t *testing.T) {
	z, _ := combatZone(t) // registers "slash"
	registerPool(z, "sanity", "max_sanity", 100, []effectOp{
		{kind: "if", then: []effectOp{{kind: "deal_damage", dmgType: "psychick"}}}, // typo, nested
		{kind: "deal_damage", dmgType: "slash"},                                    // registered
		{kind: "deal_damage"},                                                      // untyped: never flagged
	}, false)

	got := lintDealDamageTypes(z.defs)

	require.Len(t, got, 1, "expected exactly the typo'd type: %+v", got)
	require.Equal(t, "psychick", got[0].dmgType)
}

// --- A broken route is REJECTED by reload, not merely logged -------------------------------------

// TestReloadRejectsAnUnusableRoute pins the finding as a CONTROL rather than a log line. The route lint is
// ERROR severity because of blast radius — a target_resource naming a pool that cannot receive damage sends
// every blow of that kind into the immunity discard, so the whole damage kind silently does nothing against
// every target, including damage authored by other packs. A boot-time slog.Error does not stop anything: a
// staff `reload` would happily install it. Both ways to be unusable are covered, because they have the
// identical effect at runtime.
func TestReloadRejectsAnUnusableRoute(t *testing.T) {
	tests := []struct {
		name     string
		resource content.ResourceDTO
		route    string
		reject   bool
		why      string
	}{
		{
			"unregistered pool",
			content.ResourceDTO{Ref: "sanity", MaxAttr: "max_sanity"},
			"santiy", true,
			"the obvious typo: the ref resolves to nothing",
		},
		{
			"pool with no max_attr",
			content.ResourceDTO{Ref: "sanity"},
			"sanity", true,
			"the silent twin: a registered pool with no cap has max 0 forever, so every blow is discarded",
		},
		{
			"usable pool",
			content.ResourceDTO{Ref: "sanity", MaxAttr: "max_sanity"},
			"sanity", false,
			"a correct route must not be rejected — a lint that cries wolf is worse than none",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pk := content.Pack{
				Pack:        "horror",
				Resources:   []content.ResourceDTO{tc.resource},
				DamageTypes: []content.DamageTypeDTO{{Ref: "psychic", TargetResource: tc.route}},
			}
			problems := validatePacks([]content.Pack{pk}, map[string]bool{"horror": true})
			if tc.reject {
				require.NotEmpty(t, problems, "reload must REJECT this pack: %s", tc.why)
				require.Contains(t, problems[0], "psychic", "the problem must name the offending damage type")
			} else {
				require.Empty(t, problems, "%s", tc.why)
			}
		})
	}
}

// --- The content pipeline: target_resource survives the DTO -> runtime trip -----------------------

// TestTargetResourceSurvivesTheContentPipeline pins the WIRING. Every test above registers a damageTypeDef
// struct directly, so all of them would pass if defineGlobals dropped the field on the floor — one missing
// word in the registration reverts the whole feature with the package green.
func TestTargetResourceSurvivesTheContentPipeline(t *testing.T) {
	z, s := combatZone(t)
	lc := &content.LoadedContent{
		Attributes: []content.AttributeDTO{{Ref: "max_sanity", DefaultBase: content.BaseSpecDTO{Lit: floatPtr(80)}}},
		Resources:  []content.ResourceDTO{{Ref: "sanity", MaxAttr: "max_sanity"}},
		DamageTypes: []content.DamageTypeDTO{
			{Ref: "psychic", DisplayName: "Psychic", TargetResource: "sanity"},
		},
	}
	defineGlobals(z.defs, lc)

	def := z.defs.dmg.get("psychic")
	require.NotNil(t, def, "the content damage type must be registered")
	require.Equal(t, "sanity", def.targetResource, "target_resource was DROPPED at build")

	mob := combatMob(z, s.entity, "scholar", "", 100)
	dealTo(z, s.entity, mob, 30, "psychic", "")

	require.Equal(t, 50, resourceCurrent(mob, "sanity"), "the content-authored route did not take effect")
	require.Equal(t, 100, resourceCurrent(mob, "hp"))
}
