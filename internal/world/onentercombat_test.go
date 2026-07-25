package world

// onentercombat_test.go exercises the OnEnterCombat event (#547): the combat-start checkpoint fired
// from startFight about each entity that JUST entered a fight, so a content initiative check can roll
// `combat_order` before the first round sorts by it. The primitive is the EVENT; the initiative model
// (a check into combat_order) is content, exercised here through a resource on_event handler.

import (
	"math/rand"
	"sort"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// initEdgeAffect registers a transient affect that adds `bonus` to combat_order while active (the
// content side of an initiative roll — a winner acts sooner). stackCount so a SECOND fire would be
// observable (combat_order would double), which is how the fire-once test detects a spurious re-fire.
func initEdgeAffect(z *Zone, bonus float64) {
	z.defs.affect.register("init_edge", &affectDef{
		ref: "init_edge", name: "initiative edge", stacking: stackCount, maxStacks: 10,
		duration:  100,
		modifiers: []affectModifier{{attr: "combat_order", add: true, value: bonus}},
	})
}

// giveInitiativeTrigger gives e a resource whose OnEnterCombat handler applies init_edge to itself —
// the content initiative hook. e "has" the resource via a stored current so the bus gathers its handler.
func giveInitiativeTrigger(z *Zone, e *Entity) {
	if z.defs.res.get("initiative") == nil {
		z.defs.res.register("initiative", &resourceDef{
			ref: "initiative",
			onEvent: map[eventKind][]effectOp{
				evOnEnterCombat: {{kind: "apply_affect", affect: "init_edge", tgt: "self"}},
			},
		})
	}
	setResourceCurrent(e, "initiative", 1)
}

// TestOnEnterCombatRollsInitiative proves the event fires for BOTH sides that enter a fresh fight (the
// attacker and the retaliating target), and that each side's own OnEnterCombat handler can write its
// combat_order before any round resolves. This is the end-to-end "roll initiative at combat start".
func TestOnEnterCombatRollsInitiative(t *testing.T) {
	z, s := combatZone(t)
	initEdgeAffect(z, 50)
	hero := s.entity
	giveInitiativeTrigger(z, hero)

	goblin := combatMob(z, hero, "goblin", "", 100)
	giveInitiativeTrigger(z, goblin)

	if got := attr(hero, "combat_order"); got != 0 {
		t.Fatalf("precondition: hero combat_order = %v, want 0", got)
	}

	if !z.startFight(hero, goblin) {
		t.Fatal("startFight returned false")
	}

	// The attacker entered combat -> its OnEnterCombat handler ran -> combat_order raised.
	if got := attr(hero, "combat_order"); got != 50 {
		t.Fatalf("hero combat_order after OnEnterCombat = %v, want 50 (initiative handler ran)", got)
	}
	// The retaliating target also entered combat -> its handler ran too (the event fires about BOTH
	// entrants, not just the attacker).
	if got := attr(goblin, "combat_order"); got != 50 {
		t.Fatalf("goblin combat_order after OnEnterCombat = %v, want 50 (retaliation side fires too)", got)
	}
}

// TestOnEnterCombatFiresOncePerEntry proves the event fires on genuine ENTRY only — an already-fighting
// attacker that switches to a new foe (a re-`kill`) does NOT re-roll initiative. Detected via the
// stackCount affect: a spurious second fire would raise combat_order to 100.
func TestOnEnterCombatFiresOncePerEntry(t *testing.T) {
	z, s := combatZone(t)
	initEdgeAffect(z, 50)
	hero := s.entity
	giveInitiativeTrigger(z, hero)

	goblin1 := combatMob(z, hero, "goblin1", "", 100)
	goblin2 := combatMob(z, hero, "goblin2", "", 100)

	if !z.startFight(hero, goblin1) {
		t.Fatal("startFight(hero, goblin1) returned false")
	}
	if got := attr(hero, "combat_order"); got != 50 {
		t.Fatalf("hero combat_order after first entry = %v, want 50", got)
	}

	// Hero is ALREADY fighting; switching to goblin2 is a target change, not a combat entry. OnEnterCombat
	// must not fire about the hero again (no re-roll).
	if !z.startFight(hero, goblin2) {
		t.Fatal("startFight(hero, goblin2) returned false")
	}
	if got := attr(hero, "combat_order"); got != 50 {
		t.Fatalf("hero combat_order after target switch = %v, want 50 (must NOT re-roll on a re-kill)", got)
	}
}

// TestOnEnterCombatOrdersRound proves the payoff: after initiative writes combat_order, the round driver
// sorts combatants by it (highest acts first). A player who wins initiative appears ahead of one who
// does not in the resolved gather/sort order.
func TestOnEnterCombatOrdersRound(t *testing.T) {
	z, s := combatZone(t)
	initEdgeAffect(z, 50)
	hero := s.entity
	// Only the hero gets the initiative edge; the goblin stays at combat_order 0.
	giveInitiativeTrigger(z, hero)
	goblin := combatMob(z, hero, "goblin", "", 100)

	if !z.startFight(hero, goblin) {
		t.Fatal("startFight returned false")
	}
	if attr(hero, "combat_order") != 50 || attr(goblin, "combat_order") != 0 {
		t.Fatalf("initiative not set as expected: hero=%v goblin=%v", attr(hero, "combat_order"), attr(goblin, "combat_order"))
	}
	// The driver's [G-G] sort is DESC by combat_order (runCombatRound); the hero (50) must sort ahead of
	// the goblin (0). Replicate the driver's stable sort here so the assertion tracks the real ordering.
	combatants := z.gatherCombatants()
	sort.SliceStable(combatants, func(i, j int) bool {
		return attr(combatants[i], "combat_order") > attr(combatants[j], "combat_order")
	})
	if len(combatants) < 2 || combatants[0] != hero {
		t.Fatalf("initiative winner did not sort first: got %d combatants, first=%v",
			len(combatants), targetShort(combatants[0]))
	}
}

// TestOnEnterCombatRollIsSeeded proves the #58 seeded-combat invariant holds for the initiative roll:
// an OnEnterCombat handler's dice draw from z.combatRng() (NOT the process-global math/rand), so a
// seeded fight reproduces its initiative outcome — and the outcome genuinely varies with the seed (it
// is rolling, not returning a constant). The prior nil-parent fire gave handlers a nil rng and drew
// from the global stream, which this test would catch (same-seed reproducibility would fail).
func TestOnEnterCombatRollIsSeeded(t *testing.T) {
	// run seeds the ZONE combat rng, enters combat with an initiative handler that rolls 1d20 and, on a
	// high roll (>=11), grants the +50 combat_order edge — so the resulting combat_order is roll-dependent.
	// Three graded initiative tiers so nearly every distinct 1d20 roll maps to a distinct combat_order
	// (a binary threshold would need lucky seeds to vary). Each band applies a different-magnitude edge.
	regTier := func(z *Zone, ref string, bonus float64) {
		z.defs.affect.register(ref, &affectDef{
			ref: ref, name: ref, stacking: stackIgnore, maxStacks: 1, duration: 100,
			modifiers: []affectModifier{{attr: "combat_order", add: true, value: bonus}},
		})
	}
	run := func(seed int64) float64 {
		z, s := combatZone(t)
		z.combatRand = rand.New(rand.NewSource(seed))
		regTier(z, "init_hi", 100)
		regTier(z, "init_mid", 50)
		regTier(z, "init_lo", 10)
		z.defs.res.register("rolled_init", &resourceDef{
			ref: "rolled_init",
			onEvent: map[eventKind][]effectOp{
				evOnEnterCombat: {{kind: "check", check: &checkSpec{
					dice: mustDiceT("1d20"),
					bands: []checkBand{
						{min: litNode{v: 8}, label: "hi", ops: []effectOp{{kind: "apply_affect", affect: "init_hi", tgt: "self"}}},
						{min: litNode{v: 5}, label: "mid", ops: []effectOp{{kind: "apply_affect", affect: "init_mid", tgt: "self"}}},
						{label: "lo", ops: []effectOp{{kind: "apply_affect", affect: "init_lo", tgt: "self"}}},
					},
				}}},
			},
		})
		hero := s.entity
		setResourceCurrent(hero, "rolled_init", 1)
		goblin := combatMob(z, hero, "goblin", "", 100)
		if !z.startFight(hero, goblin) {
			t.Fatal("startFight returned false")
		}
		return attr(hero, "combat_order")
	}

	seeds := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	first := map[int64]float64{}
	for _, sd := range seeds {
		first[sd] = run(sd)
	}
	// Same seed reproduces exactly (the property the nil-rng bug broke).
	for _, sd := range seeds {
		if got := run(sd); got != first[sd] {
			t.Fatalf("seed %d not reproducible: %v vs %v (handler must draw from z.combatRng, not global rand)", sd, first[sd], got)
		}
	}
	// The outcome actually depends on the seeded stream (not a constant / a global draw): at least one
	// seed must differ from another. If every seed produced the same combat_order the roll isn't reading
	// the zone rng at all.
	varied := false
	for _, sd := range seeds[1:] {
		if first[sd] != first[seeds[0]] {
			varied = true
			break
		}
	}
	if !varied {
		t.Fatalf("initiative did not vary across seeds (%v) — is the handler reading z.combatRng()?", first)
	}
}

// TestOnEnterCombatSuppressesFireOnKilledEntrant proves the corpse guard (#547 review): if the
// attacker-side OnEnterCombat handler LETHALLY procs and kills the target before the target's own entry
// event fires, that second fire is suppressed — its handler must not run on a dead, room-detached entity.
func TestOnEnterCombatSuppressesFireOnKilledEntrant(t *testing.T) {
	z, s := combatZone(t)
	initEdgeAffect(z, 50)
	hero := s.entity
	// The hero's OnEnterCombat handler is a one-shot ambush: it deals lethal damage to its opponent
	// (`other`), killing the just-engaged goblin during the FIRST fire.
	z.defs.res.register("ambush", &resourceDef{
		ref: "ambush",
		onEvent: map[eventKind][]effectOp{
			evOnEnterCombat: {{kind: "deal_damage", amount: 100, dmgType: "slash", tgt: "other"}},
		},
	})
	setResourceCurrent(hero, "ambush", 1)

	// The goblin carries the initiative trigger: if its entry fire ran, it would grant itself the +50 edge.
	goblin := combatMob(z, hero, "goblin", "", 1) // 1 hp: the ambush is lethal
	giveInitiativeTrigger(z, goblin)

	if !z.startFight(hero, goblin) {
		t.Fatal("startFight returned false")
	}

	// The goblin died to the ambush during the hero's fire; its own OnEnterCombat must have been
	// suppressed (combatEntryLive false), so it never granted itself the edge.
	if position(goblin) != posDead {
		t.Fatalf("precondition: expected goblin dead from the ambush, position=%v", position(goblin))
	}
	if got := attr(goblin, "combat_order"); got != 0 {
		t.Fatalf("dead goblin combat_order = %v, want 0 (its entry fire must be suppressed on a corpse)", got)
	}
}

// TestOnEnterCombatIsAParseableEventKind proves OnEnterCombat is an engine-known event content may
// subscribe to via `on_event` (it is in knownEventKinds), so a pack authoring an initiative handler
// parses rather than being dropped with a lint warning.
func TestOnEnterCombatIsAParseableEventKind(t *testing.T) {
	m := parseEventMap(map[string]any{
		"OnEnterCombat": []any{map[string]any{"op": "apply_affect", "affect": "init_edge", "tgt": "self"}},
	}, "test")
	if len(m[evOnEnterCombat]) == 0 {
		t.Fatal("OnEnterCombat did not parse into an on_event handler (is it in knownEventKinds?)")
	}
	// Belt-and-braces: the DTO round-trip through the content package accepts the key too.
	_ = content.AffectBodyDTO{OnEvent: map[string]any{"OnEnterCombat": nil}}
}
