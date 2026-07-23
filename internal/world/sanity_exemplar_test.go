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
