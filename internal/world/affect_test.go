package world

import (
	"context"
	"testing"
	"time"
)

// affect_test.go exercises the Affected runtime (affected.go / affect_runtime.go): attach + modifiers
// feeding derivation, the four stacking modes, the per-pulse tick (decrement / expire / regen), the
// tag-CC prevents query, the resolve-by-id/skip-frozen tick contract, persistence round-trip, and the
// COW reset. Ticks are driven by z.pulses.tick() DIRECTLY (no real timers) for determinism — except
// the explicit contract test which runs the zone loop to prove the by-id re-resolution.

// affectTestZone builds a bare zone with a small set of affect/attribute/resource defs registered and
// a living entity in it, plus a session in z.players so the tick's resolve-by-id path has a row.
func affectTestZone(t *testing.T) (*Zone, *Entity) {
	t.Helper()
	z := newZone("test")
	// strength: literal base 10 (so a -2 affect modifier is observable).
	z.defs.attr.register("strength", &attributeDef{ref: "strength", base: litNode{v: 10}})
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true, regen: 5})

	z.defs.affect.register("weaken", &affectDef{
		ref: "weaken", name: "Weakened", stacking: stackRefresh, maxStacks: 1, duration: 20,
		modifiers: []affectModifier{{attr: "strength", add: true, value: -2}},
	})
	z.defs.affect.register("root", &affectDef{
		ref: "root", name: "Rooted", stacking: stackRefresh, maxStacks: 1, duration: 12,
		prevents: []string{"move"},
	})
	z.defs.affect.register("poison", &affectDef{
		ref: "poison", name: "Poisoned", stacking: stackCount, maxStacks: 5, duration: 30,
		modifiers: []affectModifier{{attr: "strength", add: true, value: -2}},
		hasTick:   true, tickInterval: 6, onTick: nil,
	})
	z.defs.affect.register("extender", &affectDef{
		ref: "extender", stacking: stackExtend, maxStacks: 1, duration: 10,
	})
	z.defs.affect.register("oncebuff", &affectDef{
		ref: "oncebuff", stacking: stackIgnore, maxStacks: 1, duration: 10,
		modifiers: []affectModifier{{attr: "strength", add: true, value: 5}},
	})

	s := newTestPlayerEntity(z, "Hero")
	z.players["Hero"] = s
	return z, s.entity
}

// TestAffectModifierFeedsDerivation: a -2 strength affect moves attr() through the modifier stack.
func TestAffectModifierFeedsDerivation(t *testing.T) {
	z, e := affectTestZone(t)
	if got := attr(e, "strength"); got != 10 {
		t.Fatalf("base strength = %v, want 10", got)
	}
	applyAffect(e, "weaken", attachOpts{}, nil)
	if got := attr(e, "strength"); got != 8 {
		t.Fatalf("weakened strength = %v, want 8 (10-2)", got)
	}
	_ = z
}

// TestStackingRefresh: a second weaken resets the timer, never doubling the modifier.
func TestStackingRefresh(t *testing.T) {
	_, e := affectTestZone(t)
	inst := applyAffect(e, "weaken", attachOpts{}, nil)
	inst.remaining = 3 // simulate decay
	applyAffect(e, "weaken", attachOpts{}, nil)
	if inst.remaining != 20 {
		t.Fatalf("refresh remaining = %d, want reset to 20", inst.remaining)
	}
	if got := attr(e, "strength"); got != 8 {
		t.Fatalf("refresh must not double the modifier: strength = %v, want 8", got)
	}
}

// TestStackingCount: poison stacks up to max_stacks; magnitude (the -2 strength) scales with stacks.
func TestStackingCount(t *testing.T) {
	_, e := affectTestZone(t)
	applyAffect(e, "poison", attachOpts{}, nil) // 1 stack: -2
	if got := attr(e, "strength"); got != 8 {
		t.Fatalf("1-stack strength = %v, want 8", got)
	}
	applyAffect(e, "poison", attachOpts{}, nil) // 2 stacks: -4
	applyAffect(e, "poison", attachOpts{}, nil) // 3 stacks: -6
	if got := attr(e, "strength"); got != 4 {
		t.Fatalf("3-stack strength = %v, want 4 (10 - 2*3)", got)
	}
	// Push past max_stacks (5): a 6th application stays capped at 5.
	for i := 0; i < 5; i++ {
		applyAffect(e, "poison", attachOpts{}, nil)
	}
	a, _ := Get[*Affected](e)
	if a.list[0].stacks != 5 {
		t.Fatalf("stacks = %d, want capped at 5", a.list[0].stacks)
	}
	if got := attr(e, "strength"); got != 0 {
		t.Fatalf("5-stack strength = %v, want 0 (10 - 2*5)", got)
	}
}

// TestStackingExtend: a second application sums the durations.
func TestStackingExtend(t *testing.T) {
	_, e := affectTestZone(t)
	inst := applyAffect(e, "extender", attachOpts{}, nil) // remaining 10
	applyAffect(e, "extender", attachOpts{}, nil)         // remaining 20
	if inst.remaining != 20 {
		t.Fatalf("extend remaining = %d, want 20 (10+10)", inst.remaining)
	}
}

// TestStackingIgnore: first wins — a second application is a no-op (timer + modifier unchanged).
func TestStackingIgnore(t *testing.T) {
	_, e := affectTestZone(t)
	inst := applyAffect(e, "oncebuff", attachOpts{}, nil)
	inst.remaining = 4
	applyAffect(e, "oncebuff", attachOpts{}, nil) // ignored
	if inst.remaining != 4 {
		t.Fatalf("ignore remaining = %d, want unchanged 4", inst.remaining)
	}
	if got := attr(e, "strength"); got != 15 {
		t.Fatalf("ignore strength = %v, want 15 (single +5)", got)
	}
}

// TestAffectExpires: duration decrements each tick and the affect EXPIRES at 0 — modifiers + prevents
// cleared, cache re-dirtied (the next attr() reads the base again).
func TestAffectExpires(t *testing.T) {
	z, e := affectTestZone(t)
	z.defs.affect.register("brief", &affectDef{
		ref: "brief", stacking: stackRefresh, maxStacks: 1, duration: 3,
		modifiers: []affectModifier{{attr: "strength", add: true, value: -2}},
		prevents:  []string{"move"},
	})
	applyAffect(e, "brief", attachOpts{}, nil)
	if got := attr(e, "strength"); got != 8 || !preventsTag(e, "move") {
		t.Fatalf("pre-expire: strength=%v prevents-move=%v", got, preventsTag(e, "move"))
	}
	// 3 ticks: remaining 3 -> 2 -> 1 -> 0 (expire on the third).
	z.pulses.tick()
	z.pulses.tick()
	z.pulses.tick()
	if got := attr(e, "strength"); got != 10 {
		t.Fatalf("post-expire strength = %v, want base 10 (modifier cleared)", got)
	}
	if preventsTag(e, "move") {
		t.Fatal("post-expire still prevents move (prevents not cleared)")
	}
	a, _ := Get[*Affected](e)
	if a.hasActiveAffects() {
		t.Fatal("expired affect still in the active list")
	}
}

// TestPreventsTagQuery: the standalone tag-CC query over the prevents set (§6).
func TestPreventsTagQuery(t *testing.T) {
	_, e := affectTestZone(t)
	if preventsTag(e, "move") {
		t.Fatal("no affect yet, must not prevent move")
	}
	applyAffect(e, "root", attachOpts{}, nil)
	if !preventsTag(e, "move") {
		t.Fatal("root must prevent move")
	}
	if preventsTag(e, "verbal") {
		t.Fatal("root must not prevent verbal")
	}
	if tag, blocked := preventsAny(e, []string{"verbal", "move", "fire"}); !blocked || tag != "move" {
		t.Fatalf("preventsAny = (%q,%v), want (move,true)", tag, blocked)
	}
}

// TestResourceRegenTick: regen moves a wounded pool toward its derived max and clamps (no overshoot).
func TestResourceRegenTick(t *testing.T) {
	z, e := affectTestZone(t)
	e.SetHP(90) // max_hp = 100, regen = 5; below max so the tick is registered by setResourceCurrent
	z.pulses.tick()
	if got := e.HP(); got != 95 {
		t.Fatalf("after 1 regen tick HP = %d, want 95", got)
	}
	z.pulses.tick() // 95 -> 100 (clamp; +5 would be exactly 100)
	if got := e.HP(); got != 100 {
		t.Fatalf("after 2 regen ticks HP = %d, want 100 (clamped)", got)
	}
	z.pulses.tick() // already full: no overshoot, tick retires
	if got := e.HP(); got != 100 {
		t.Fatalf("regen overshot the max: HP = %d, want 100", got)
	}
}

// TestResourceRegenPausesInCombat: the engine "no rest mid-fight" default — a resource with the default
// regen_in_combat=false does NOT regenerate while its owner is posFighting, then resumes the instant the
// fight ends. This is the mechanism that stops a mob's hp regen from clawing back a player's per-round
// damage (the starter-combat slog). The affectTestZone hp def declares no regen_in_combat, so the
// pause-in-combat default applies.
func TestResourceRegenPausesInCombat(t *testing.T) {
	z, e := affectTestZone(t)
	e.SetHP(90) // max_hp 100, regen 5; wounded so the tick is registered

	setPosition(e, posFighting) // in combat -> regen paused (default regen_in_combat=false)
	z.pulses.tick()
	if got := e.HP(); got != 90 {
		t.Fatalf("hp regenerated mid-fight: HP = %d, want 90 (paused in combat)", got)
	}

	setPosition(e, posStanding) // fight over -> regen resumes on the next tick
	z.pulses.tick()
	if got := e.HP(); got != 95 {
		t.Fatalf("hp did not resume regen after combat: HP = %d, want 95", got)
	}
}

// TestResourceRegenInCombatOptIn: a resource that sets regen_in_combat=true KEEPS regenerating while its
// owner is fighting (a troll's regeneration). This guards the content opt-out of the pause default.
func TestResourceRegenInCombatOptIn(t *testing.T) {
	z, e := affectTestZone(t)
	// Override the hp def to regen-in-combat (a troll-style pool that ticks through a fight).
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true, regen: 5, regenInCombat: true})
	e.SetHP(90)

	setPosition(e, posFighting)
	z.pulses.tick()
	if got := e.HP(); got != 95 {
		t.Fatalf("regen_in_combat pool did not tick in a fight: HP = %d, want 95", got)
	}
}

// TestPoisonTickHookFires: an affect with tick.interval fires its (reserved) on_tick hook at the
// interval — proven by the sinceTick counter resetting AND the affect still decrementing/expiring.
func TestPoisonTickHookFires(t *testing.T) {
	z, e := affectTestZone(t)
	inst := applyAffect(e, "poison", attachOpts{}, nil)
	// poison: duration 30, tick interval 6. After 6 ticks the hook fires (sinceTick resets to 0) and
	// remaining has dropped by 6.
	for i := 0; i < 6; i++ {
		z.pulses.tick()
	}
	if inst.sinceTick != 0 {
		t.Fatalf("sinceTick = %d after a tick-interval boundary, want reset to 0", inst.sinceTick)
	}
	if inst.remaining != 24 {
		t.Fatalf("remaining = %d after 6 ticks, want 24 (30-6)", inst.remaining)
	}
}

// TestAffectSurvivesSaveLoad: an affect round-trips with the CORRECT remaining duration (not reset to
// full), no double-tick, no re-fired on_apply.
func TestAffectSurvivesSaveLoad(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Wynne"}
	e := z.newPlayerEntity(src, "Wynne")
	z.players["Wynne"] = src

	// Apply a demo poison, then decay it to a partial remaining.
	inst := applyAffect(e, "poison", attachOpts{}, nil)
	inst.remaining = 11
	inst.stacks = 3

	snap := dumpCharacter(src)
	if len(snap.State.Affects) != 1 {
		t.Fatalf("dumped affects = %d, want 1", len(snap.State.Affects))
	}
	if a := snap.State.Affects[0]; a.ID != "poison" || a.Remaining != 11 || a.Stacks != 3 {
		t.Fatalf("dumped affect = %+v, want poison remaining=11 stacks=3", a)
	}

	// Load into a fresh entity.
	dst := &session{character: "Wynne"}
	z.newPlayerEntity(dst, "Wynne")
	loadCharacter(z, dst, snap)
	de := dst.entity

	a, ok := Get[*Affected](de)
	if !ok || len(a.list) != 1 {
		t.Fatalf("loaded affects = %v", a)
	}
	li := a.list[0]
	if li.def.ref != "poison" || li.remaining != 11 || li.stacks != 3 {
		t.Fatalf("loaded affect = ref=%s remaining=%d stacks=%d, want poison/11/3",
			li.def.ref, li.remaining, li.stacks)
	}
	// The modifier (poison's -2 strength * 3 stacks) is re-seeded into derivation.
	base := de.Attr("strength")
	wantBase := 10.0 - 6 // demo strength base 10, minus 2*3
	if base != wantBase {
		t.Fatalf("loaded strength = %v, want %v (modifier re-seeded)", base, wantBase)
	}
}

// TestLoadAffectlessSnapshotSane: a pre-5.2 snapshot (no affects array) loads with no affects.
func TestLoadAffectlessSnapshotSane(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := &session{character: "Old"}
	z.newPlayerEntity(s, "Old")
	loadCharacter(z, s, CharSnapshot{Name: "Old", State: StateJSON{}})
	if _, ok := Get[*Affected](s.entity); ok {
		t.Fatal("affectless snapshot must not install an Affected component")
	}
}

// TestTickCancelsWhenPlayerAbsent: the resolve-by-id contract — once a player leaves z.players, the
// tick re-resolves by id, finds nothing, and CANCELS (does not touch a stale captured entity).
func TestTickCancelsWhenPlayerAbsent(t *testing.T) {
	z, e := affectTestZone(t)
	applyAffect(e, "weaken", attachOpts{}, nil)
	a, _ := Get[*Affected](e)
	if a.tick == nil {
		t.Fatal("tick not registered after attach")
	}
	// Player departs: remove from z.players (the entity pointer is now logically owned elsewhere).
	delete(z.players, "Hero")
	// The next tick re-resolves "Hero" by id, finds it absent, returns false (cancels). It must NOT
	// decrement the captured entity's affect.
	before := a.list[0].remaining
	z.pulses.tick()
	if a.list[0].remaining != before {
		t.Fatalf("tick decremented an affect on a DEPARTED player: %d -> %d",
			before, a.list[0].remaining)
	}
	// The scheduler dropped the cancelled callback: another tick is a no-op too.
	z.pulses.tick()
	if a.list[0].remaining != before {
		t.Fatal("tick kept running after the player departed")
	}
}

// TestTickSkipsFrozenPlayer: a frozen (mid-handoff) player is not ticked — durations are conserved
// across the handoff seam (only the owning zone's pulse decrements them).
func TestTickSkipsFrozenPlayer(t *testing.T) {
	z, e := affectTestZone(t)
	s := z.players["Hero"]
	applyAffect(e, "weaken", attachOpts{}, nil)
	a, _ := Get[*Affected](e)
	before := a.list[0].remaining

	s.frozen = true
	z.pulses.tick()
	if a.list[0].remaining != before {
		t.Fatalf("frozen player was ticked: remaining %d -> %d", before, a.list[0].remaining)
	}

	// Thawing resumes ticking — but the cancelled callback is gone, so re-arm via a fresh attach path.
	s.frozen = false
	applyAffect(e, "weaken", attachOpts{}, nil) // refresh re-ensures the tick
	z.pulses.tick()
	if a.list[0].remaining == 20 {
		t.Fatal("thawed player still not ticking (remaining never decremented)")
	}
}

// TestTickContractUnderZoneLoop runs the real zone loop (not tick()-by-hand) to prove the by-id
// re-resolution holds under the goroutine the -race detector watches: the callback reads z.players on
// the zone goroutine each fire. A departed player cancels cleanly with no race.
func TestTickContractUnderZoneLoop(t *testing.T) {
	z, e := affectTestZone(t)
	applyAffect(e, "poison", attachOpts{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	// Let a few ticks happen, then remove the player by posting through... there is no command for
	// it; instead drive a clean leave so the removal is single-writer. Use leaveMsg via post.
	time.Sleep(3 * pulseInterval)
	z.post(leaveMsg{id: "Hero"})
	// Give the loop time to process the leave and a few more ticks; the tick must cancel, not panic
	// or race on the departed entity.
	time.Sleep(3 * pulseInterval)
	// If we got here without the -race detector firing or a panic, the contract held.
}

// TestAffectedCOWReset: cloning an Affected component (the COW path) yields an EMPTY runtime — no
// aliased instances, maps, or tick handle.
func TestAffectedCOWReset(t *testing.T) {
	_, e := affectTestZone(t)
	applyAffect(e, "poison", attachOpts{}, nil)
	orig, _ := Get[*Affected](e)
	clone := cloneComponent(orig).(*Affected)
	if len(clone.list) != 0 || clone.byKey == nil || len(clone.byKey) != 0 {
		t.Fatalf("COW Affected not reset: list=%d byKey=%v", len(clone.list), clone.byKey)
	}
	if clone.tick != nil || clone.registered {
		t.Fatal("COW Affected inherited tick/registered state")
	}
	if clone.flat != nil || clone.prevents != nil {
		t.Fatal("COW Affected inherited modifier/prevents maps")
	}
}
