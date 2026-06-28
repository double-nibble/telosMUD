package world

import (
	"math/rand"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"

	"github.com/double-nibble/telosmud/internal/content"
)

// cow_repop_cycle_test.go is the BLACK-BOX (through the real demo content + the real repop
// reset path) regression for the copy-on-write (COW) prototype-corruption bug — Wave 1.
//
// THE BUG (latent core-model corruption): a spawned mob's combat/death wrote straight through
// its SHARED prototype's *Living pointer (component.go's mutableLiving choke-point was missing),
// so killing a mob mutated the template every future spawn reads. It was LATENT until a repop
// (reset.go runResets -> z.spawn re-reads the prototype) produced the NEXT goblin from the now-
// corrupted template: it came back posDead / 0-hp / with a stale `fighting` pointer, so it was
// un-attackable (startFight rejects a posDead target). A single kill looked fine; the SECOND
// run — kill, repop, re-kill — is what surfaced it.
//
// cow_living_test.go pins the model-level invariant via raw z.spawn calls. THIS test pins the
// SAME invariant through the path that actually bit us in the world: the real demo goblin, the
// real swing/death/corpse pipeline, and the real REPOP RESET (the reset op the repop timer fires).
// It is the kill -> repop -> re-kill CYCLE the owner specified: both kills land clean, and the
// repop is alive / full-HP / standing at its spawn position — not a posDead zombie.
//
// Determinism: a kill-shot combat profile (auto-hit + a flat 100 damage bonus) fells the 15-hp
// goblin in ONE swing, and a seeded RNG makes the (already non-random) swing reproducible. No
// pulse pacing, no wall clock — the death and the repop are driven explicitly.
func TestCOWKillRepopRekillCycle(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	z.testCombatRng = rand.New(rand.NewSource(1))

	// The hollow is where the demo's reset op spawns the goblin (demo.yaml darkwood resets:
	// {op: spawn_mob, proto: darkwood:mob:goblin, room: darkwood:room:hollow, count: 1}).
	hollow := z.rooms["darkwood:room:hollow"]
	if hollow == nil {
		t.Fatal("darkwood:room:hollow missing from the demo zone")
	}

	// The repop reset op the timer fires (reset.go startRepop -> runResets). We drive it
	// EXPLICITLY rather than ticking the 90s demo cadence, so the test is fast and deterministic
	// while exercising the IDENTICAL code path (runResets -> applyReset -> z.spawn(proto)) that
	// re-reads the shared prototype and produced the corrupted repop pre-fix.
	goblinRepop := []content.ResetDTO{
		{Op: "spawn_mob", Proto: "darkwood:mob:goblin", Room: "darkwood:room:hollow", Count: 1},
		// Re-arm the repopped goblin with its rusty knife so the SECOND kill's corpse-loot transfer
		// is exercised too (the into-mob reset, demo.yaml). Order matters: spawn the mob, then arm it.
		{Op: "spawn_item", Proto: "darkwood:obj:rusty-knife", Into: "darkwood:mob:goblin", Room: "darkwood:room:hollow", Count: 1},
	}

	// A hero set up to one-shot the goblin through the REAL death pipeline (resolveSwing -> die ->
	// corpse). The kill-shot profile removes to-hit/avoidance randomness so the killing blow is exact.
	s := &session{character: "Slayer", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Slayer")
	Move(s.entity, hollow)
	z.players["Slayer"] = s
	z.defs.combat.register("killshot", killShotProfile(100))
	s.entity.living.combatRef = "killshot"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"}) // 6 + 100 >> 15 hp

	// findGoblin returns the live (non-corpse) goblin instance the reset placed in the hollow, or nil.
	// A corpse shares no proto ref with the mob (death.go mints a corpse entity), so this never matches it.
	findGoblin := func() *Entity {
		for _, e := range hollow.contents {
			if e.proto == "darkwood:mob:goblin" {
				return e
			}
		}
		return nil
	}

	// killGoblin drives one full kill through the swing pipeline and asserts a CLEAN death: the
	// killing blow lands, the goblin entity is removed from the room, and a corpse appears. It
	// returns nothing — a failed kill is a fatal here so the cycle stops at the first regression.
	killGoblin := func(t *testing.T, phase string) {
		t.Helper()
		goblin := findGoblin()
		if goblin == nil {
			t.Fatalf("%s: no live goblin in the hollow to kill", phase)
		}

		// PRECONDITION — the repop invariant the bug violated: every goblin we set out to kill must be
		// born ALIVE, FULL-HP, and STANDING. Pre-fix the repopped goblin failed this (posDead / 0 hp),
		// so this is the load-bearing assertion that distinguishes a clean repop from a corrupted one.
		if got := position(goblin); got != posStanding {
			t.Fatalf("%s: goblin born in position %d, want posStanding (%d) — prototype corrupted by a prior death (COW regression)",
				phase, got, posStanding)
		}
		if goblin.living.fighting != nil {
			t.Fatalf("%s: goblin born with a stale `fighting` pointer — prototype corrupted by a prior death (COW regression)", phase)
		}
		hp, maxHP := resourceCurrent(goblin, "hp"), resourceMax(goblin, "hp")
		if hp != maxHP || hp <= 0 {
			t.Fatalf("%s: goblin born at hp %d/%d, want full and > 0 — prototype's resCur corrupted by a prior death (COW regression)",
				phase, hp, maxHP)
		}

		// startFight must ACCEPT the target. Pre-fix a corrupted (posDead) repop was rejected here —
		// the player literally could not attack the goblin that came back. This is the user-visible
		// symptom the regression locks out.
		if !z.startFight(s.entity, goblin) {
			t.Fatalf("%s: startFight rejected the goblin (a posDead/corrupted repop is un-attackable — COW regression)", phase)
		}

		// One swing fells it (kill-shot profile). The real death path runs: die() -> posDead latch,
		// corpse container with the mob's loot, mob removed from the room.
		z.resolveSwing(s.entity, goblin, 0, rand.New(rand.NewSource(1)), newBudget())

		if findGoblin() != nil {
			t.Fatalf("%s: the killed goblin was not removed from the room (death path regression)", phase)
		}
		// A corpse container appeared holding the goblin's knife (the death-sequence loot transfer).
		var corpse *Entity
		for _, e := range hollow.contents {
			if _, ok := Get[*Container](e); ok {
				corpse = e
			}
		}
		if corpse == nil {
			t.Fatalf("%s: no corpse appeared after the kill (death path regression)", phase)
		}
		if len(corpse.contents) != 1 {
			t.Fatalf("%s: corpse holds %d items, want 1 (the rusty knife) — loot transfer regression", phase, len(corpse.contents))
		}
		// Clear the corpse so the next repop's countProto sees an empty hollow and re-spawns the goblin
		// (a corpse is not a goblin instance, so this is only to keep the room tidy for the assertion).
		Move(corpse, z.rooms["darkwood:room:lair"]) // shove it elsewhere; lair exists in the demo zone.
		s.entity.living.fighting = nil              // the hero disengages between kills (the victim is gone).
		setPosition(s.entity, posStanding)
	}

	// --- KILL 1: the boot-spawned goblin. Pre-fix this kill CORRUPTED the shared prototype. ---
	killGoblin(t, "kill-1 (boot goblin)")

	// --- REPOP: the real reset re-reads the prototype and spawns the next goblin. ---
	// Pre-fix, z.spawn here cloned the corrupted template -> a posDead, 0-hp, un-attackable goblin.
	z.runResets(goblinRepop)
	if findGoblin() == nil {
		t.Fatal("repop reset did not spawn a replacement goblin (reset/spawn regression)")
	}

	// --- KILL 2: the REPOPPED goblin. Its born-clean preconditions (asserted inside killGoblin)
	// are the COW regression catch — a corrupted repop fails them; a correct one is re-killed clean. ---
	killGoblin(t, "kill-2 (repopped goblin)")
}
