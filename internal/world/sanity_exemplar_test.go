package world

import (
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/content"
)

// sanity_exemplar_test.go is the #408 acceptance artifact: it drives the SHIPPED demo pack (not
// hand-registered structs) to prove the whole Round 42 family composes into a working secondary-vital
// system — the Call of Cthulhu / Delta Green shape.
//
//	#405  a `horror` damage type routes to `sanity` with no per-op `resource` anywhere
//	#406  `sanity` is NON-vital, so bottoming it out applies `insane` instead of killing
//	#407  the hook can read how far past 0 the blow went
//
// It also pins the property that makes the pool safe to ship in a pack everything else already loads:
// `pow` defaults to 0, so every pre-existing player and mob has NO capacity and is immune by construction.

// demoZoneForSanity builds a zone from the real embedded demo pack.
func demoZoneForSanity(t *testing.T) *Zone {
	t.Helper()
	lc, err := content.LoadDemoPack()
	require.NoError(t, err)
	z := newZone("midgaard")
	defineContent(z.protos, lc)
	defineGlobals(z.defs, lc)
	z.buildZone(lc)
	return z
}

// sanityMob puts a living mob in the demo start room with `pow` raised, i.e. a creature that HAS a mind.
func sanityMob(t *testing.T, z *Zone, name string, pow float64) *Entity {
	t.Helper()
	room := z.resolveRoom(z.startRoom)
	require.NotNil(t, room, "the demo start room must resolve")
	e := z.newEntity(ProtoRef("test:mob"))
	e.short = name
	e.setKeywords([]string{name})
	Add(e, &Living{})
	Move(e, room)
	if pow > 0 {
		setAttrBase(e, "pow", pow)
	}
	return e
}

// TestDemoPackSanityExemplar is the end-to-end run: a horror blow routes to sanity by TYPE, empties it,
// runs the non-vital hook, applies `insane`, and leaves the victim ALIVE with hp untouched.
func TestDemoPackSanityExemplar(t *testing.T) {
	z := demoZoneForSanity(t)
	attacker := sanityMob(t, z, "horror", 0)
	victim := sanityMob(t, z, "investigator", 4) // pow 4 => max_sanity 20

	require.Equal(t, 20, resourceMax(victim, "sanity"), "max_sanity must derive from pow (pow*5)")
	require.Equal(t, 20, resourceCurrent(victim, "sanity"), "a fresh pool reads full")
	hpBefore := resourceCurrent(victim, "hp")

	c := &effectCtx{
		z: z, actor: attacker, source: attacker, target: victim,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	// NOTE: no `resource` anywhere — the damage TYPE carries the route (#405).
	dealDamage(c, victim, 25, "horror", "")

	require.Equal(t, 0, resourceCurrent(victim, "sanity"), "the horror blow must have emptied sanity")
	require.Equal(t, hpBefore, resourceCurrent(victim, "hp"), "hp must be untouched — the type routed away from it")
	require.True(t, hasAffect(victim, "insane"), "the non-vital depletion hook must have applied `insane`")
	require.NotEqual(t, posDead, position(victim), "a Sanity break is NOT a death (#406)")
	require.Nil(t, roomCorpse(victim.location), "and drops no corpse")
}

// TestDemoPackSanityIsInertForExistingContent is the property that lets this ship in the pack every zone
// already loads. An ordinary creature — anything that does not raise `pow` — has max_sanity 0, so the
// engine's own capacity discard makes it immune to horror damage with no content check anywhere.
func TestDemoPackSanityIsInertForExistingContent(t *testing.T) {
	z := demoZoneForSanity(t)
	attacker := sanityMob(t, z, "horror", 0)
	rat := sanityMob(t, z, "rat", 0) // an ordinary creature: pow defaults to 0

	require.Zero(t, resourceMax(rat, "sanity"), "an ordinary creature has NO capacity in the new pool")
	hpBefore := resourceCurrent(rat, "hp")

	c := &effectCtx{
		z: z, actor: attacker, source: attacker, target: rat,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	for i := 0; i < 3; i++ {
		require.Zero(t, dealDamage(c, rat, 100, "horror", ""),
			"blow %d must be DISCARDED against a creature with no mind, not merely harmless", i)
	}

	require.Equal(t, hpBefore, resourceCurrent(rat, "hp"), "horror damage must never fall back onto hp")
	require.NotEqual(t, posDead, position(rat))
	require.False(t, hasAffect(rat, "insane"), "a mindless creature must not go insane")
	_, stored := rat.living.resCur["sanity"]
	require.False(t, stored, "no phantom current may be stored on a pool it has no capacity in")
}

// TestDemoPackSanityHookIsIdempotent pins the authoring guard the shipped hook uses. The depletion hook is
// LEVEL-triggered, so every further blow onto a pool already at 0 re-enters it. The content answers with
// `if has_affect: insane` plus `stacking: ignore`, and this asserts both actually hold — without them the
// room narration would re-print on every blow for the rest of the fight.
func TestDemoPackSanityHookIsIdempotent(t *testing.T) {
	z := demoZoneForSanity(t)
	attacker := sanityMob(t, z, "horror", 0)
	victim := sanityMob(t, z, "investigator", 4)

	c := &effectCtx{
		z: z, actor: attacker, source: attacker, target: victim,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	for i := 0; i < 4; i++ {
		dealDamage(c, victim, 25, "horror", "")
	}

	require.True(t, hasAffect(victim, "insane"))
	require.Equal(t, 1, affectStacks(victim, "insane"),
		"`stacking: ignore` must make re-application a no-op — a level-triggered hook re-enters on every blow")
	require.NotEqual(t, posDead, position(victim), "no amount of horror damage may kill through a non-vital pool")
}

// affectStacks returns the stack count of affect `ref` on e, or 0 if absent.
func affectStacks(e *Entity, ref string) int {
	a, ok := Get[*Affected](e)
	if !ok {
		return 0
	}
	for _, inst := range a.list {
		if inst != nil && inst.def != nil && inst.def.ref == ref {
			return inst.stacks
		}
	}
	return 0
}

// TestShadeSwingRoutesToSanityInWorld is the in-world half of the exemplar, and the author's real question
// about #405: does routing hold for a SWING? A swing carries the weapon's damage type and never a resource
// (buildSwingDamageOp sets none), so before #405 a horror-clawed mob was unbuildable — its blows would have
// landed on hp whatever its weapon said. The shipped `crypt:mob:shade` has a natural weapon of type horror
// and nothing anywhere names a pool.
func TestShadeSwingRoutesToSanityInWorld(t *testing.T) {
	z := demoZoneForSanity(t)
	shade := z.protos.get("crypt:mob:shade")
	require.NotNil(t, shade, "the shipped pack must define the shade")

	victim := sanityMob(t, z, "investigator", 4) // pow 4 => max_sanity 20
	attacker := z.spawn("crypt:mob:shade")
	require.NotNil(t, attacker, "the shade must spawn")
	Move(attacker, victim.location)
	require.Zero(t, resourceMax(attacker, "sanity"), "the shade has no mind of its own to lose")

	hpBefore := resourceCurrent(victim, "hp")
	z.startFight(attacker, victim)
	for i := 0; i < 6; i++ {
		z.resolveSwing(attacker, victim, 0, rand.New(rand.NewSource(int64(i+1))), newBudget())
	}

	require.Less(t, resourceCurrent(victim, "sanity"), 20, "the shade's SWINGS must land on sanity")
	require.Equal(t, hpBefore, resourceCurrent(victim, "hp"), "and must never touch hp")
}

// TestSteadyRecoversSanityAndLiftsTheCondition closes the loop the RPG review asked for. `sanity` has
// regen 0 — recovery is an authored act, which is the d100-family rule — so without a shipped cure the pack
// would teach that the only way out of madness is to die. It also proves the `stack_scope: target` fix:
// keyed by source, this remove_affect would silently miss an instance the depletion hook applied.
func TestSteadyRecoversSanityAndLiftsTheCondition(t *testing.T) {
	z := demoZoneForSanity(t)
	attacker := sanityMob(t, z, "horror", 0)
	victim := sanityMob(t, z, "investigator", 4)
	setResourceCurrent(victim, "mana", 100)

	c := &effectCtx{
		z: z, actor: attacker, source: attacker, target: victim,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	dealDamage(c, victim, 25, "horror", "")
	require.True(t, hasAffect(victim, "insane"), "precondition: the victim is broken")
	require.Zero(t, resourceCurrent(victim, "sanity"))

	// The cure is cast by the VICTIM here, but the point of stack_scope: target is that it need not be —
	// the affect was applied with the hook's ctx, not this one.
	// THE DISCRIMINATING CASE: the cure is cast by a THIRD PARTY. An affect instance is keyed by
	// (ref, source) unless the def sets stack_scope: target, and the depletion hook applied this one with
	// the victim as its source — so a healer's remove_affect looks up (insane, healer) and, without the
	// target scope, silently misses. Casting it AS the victim passes either way and proves nothing; I wrote
	// it that way first and the mutation test caught it.
	def := z.defs.ability.get("steady")
	require.NotNil(t, def, "the pack must ship a recovery ability")
	healer := sanityMob(t, z, "counsellor", 0)
	rc := &effectCtx{
		z: z, actor: healer, source: healer, target: victim, mag: 1, disp: dispHelpful,
		rng: rand.New(rand.NewSource(1)),
	}
	runOps(rc, def.ops)

	require.Positive(t, resourceCurrent(victim, "sanity"), "steady must restore the pool")
	require.False(t, hasAffect(victim, "insane"),
		"a THIRD PARTY's remove_affect missed the condition — an affect keyed by its CAUSE rather than its sufferer is uncurable by anyone but that cause, and by nobody at all after a relog (a persisted affect re-attaches with no source)")
}

// TestRespawnWritesNoPhantomForACapacitylessPool is the durable half of the inertness property, and the
// one the damage-path test cannot see. respawnPlayer restores every pool (#406, so a non-vital condition
// is not inescapable), and without a capacity guard that writes a stored 0 for a pool the character has no
// capacity in — on every death, persisted.
//
// A stored 0 is NOT the same as an absent key: resourceCurrent reads an absent pool as FULL and a stored 0
// as empty. So a character who died BEFORE their content granted them capacity would come back holding a
// durable 0, and the instant capacity arrived — exactly the path this exemplar advertises, a bundle grant
// raising `pow` — they would read permanently empty and break on the first point of horror damage instead
// of starting whole.
func TestRespawnWritesNoPhantomForACapacitylessPool(t *testing.T) {
	z := demoZoneForSanity(t)
	s := makeRoomPlayer(z, "Commoner") // an ordinary player: pow 0, so no capacity in sanity
	require.Zero(t, resourceMax(s.entity, "sanity"), "precondition: no capacity")

	z.respawnPlayer(s.entity)

	_, stored := s.entity.living.resCur["sanity"]
	require.False(t, stored, "respawn wrote a durable phantom 0 for a pool the character has no capacity in")

	// The consequence, asserted end-to-end rather than trusting the absence: once content grants capacity,
	// the character must read FULL, not empty.
	setAttrBase(s.entity, "pow", 4)
	require.Equal(t, 20, resourceMax(s.entity, "sanity"), "precondition: the grant lifted the cap")
	require.Equal(t, 20, resourceCurrent(s.entity, "sanity"),
		"a character who died before gaining the pool must start it FULL, not permanently empty")
}

// TestRespawnStillRestoresPoolsTheCharacterHas guards the other direction: the capacity guard must not undo
// #406's reason for restoring every pool. A character who HAS the pool and died with it empty must come
// back whole, or the condition it carries is inescapable.
func TestRespawnStillRestoresPoolsTheCharacterHas(t *testing.T) {
	z := demoZoneForSanity(t)
	s := makeRoomPlayer(z, "Investigator")
	setAttrBase(s.entity, "pow", 4)
	setResourceCurrent(s.entity, "sanity", 0)
	setResourceCurrent(s.entity, "hp", 0)

	z.respawnPlayer(s.entity)

	require.Equal(t, 20, resourceCurrent(s.entity, "sanity"), "a pool the character HAS must still be restored")
	require.Equal(t, resourceMax(s.entity, "hp"), resourceCurrent(s.entity, "hp"), "vitals restore as always")
}

// --- The HUD must not leak a pool the character does not have ------------------------------------

// TestVitalsSurfacesAgreeOnACapacitylessPool pins the fix for the inconsistency #408 would otherwise
// expose to every rich client. The GMCP Char.Vitals payload and the text prompt are the same question
// asked twice — "what is player-visible" — and they disagreed: the prompt filtered pools the character has
// no capacity in, Char.Vitals did not. Invisible while every gauged pool had a positive cap for everyone;
// very visible the moment a pack ships an OPT-IN pool, which is exactly what the sanity exemplar is.
func TestVitalsSurfacesAgreeOnACapacitylessPool(t *testing.T) {
	z := demoZoneForSanity(t)

	t.Run("no capacity: absent from BOTH surfaces", func(t *testing.T) {
		e := sanityMob(t, z, "commoner", 0)
		require.NotContains(t, string(z.charVitalsJSON(e)), "sanity",
			"Char.Vitals leaked a pool this character has no capacity in")
		require.NotContains(t, z.vitalsPrompt(e), "sanity", "the prompt correctly omits it — the two must agree")
	})

	t.Run("with capacity: present in BOTH", func(t *testing.T) {
		e := sanityMob(t, z, "investigator", 4)
		require.Contains(t, string(z.charVitalsJSON(e)), "sanity", "a character who HAS the pool must see it")
		require.Contains(t, z.vitalsPrompt(e), "sanity")
	})

	t.Run("hp is unaffected either way", func(t *testing.T) {
		e := sanityMob(t, z, "commoner", 0)
		require.Contains(t, string(z.charVitalsJSON(e)), "hp", "the filter must not drop ordinary pools")
	})
}
