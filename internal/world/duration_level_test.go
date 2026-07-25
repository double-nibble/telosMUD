package world

// duration_level_test.go exercises the #545 primitives: an INDEFINITE duration kind (never counts down;
// ends only via dispel/remove/death — replacing the "huge duration" hack) and an affect LEVEL field with
// dispel ordering-by-level + an optional per-affect check gate ($affect.level).

import (
	"math/rand"
	"testing"
)

// TestIndefiniteAffectNeverExpires proves an indefinite affect survives arbitrarily many ticks and keeps
// its sentinel remaining, while a finite sibling expires on schedule.
func TestIndefiniteAffectNeverExpires(t *testing.T) {
	z, e := affectTestZone(t)
	z.defs.affect.register("ward", &affectDef{
		ref: "ward", name: "Eternal Ward", stacking: stackRefresh, maxStacks: 1,
		indefinite: true, // never counts down
		modifiers:  []affectModifier{{attr: "strength", add: true, value: 3}},
	})
	z.defs.affect.register("brief", &affectDef{
		ref: "brief", stacking: stackRefresh, maxStacks: 1, duration: 3,
		modifiers: []affectModifier{{attr: "strength", add: true, value: -2}},
	})
	inst := applyAffect(e, "ward", attachOpts{}, nil)
	applyAffect(e, "brief", attachOpts{}, nil)
	if inst.remaining != durationIndefinite {
		t.Fatalf("indefinite affect remaining = %d, want sentinel %d", inst.remaining, durationIndefinite)
	}

	// Tick well past the finite affect's duration.
	for i := 0; i < 50; i++ {
		z.pulses.tick()
	}
	if !hasAffect(e, "ward") {
		t.Fatal("indefinite affect expired from ticking (must never count down)")
	}
	if inst.remaining != durationIndefinite {
		t.Fatalf("indefinite remaining drifted to %d after 50 ticks", inst.remaining)
	}
	if hasAffect(e, "brief") {
		t.Fatal("finite affect should have expired (regression: countdown still works)")
	}
	if got := attr(e, "strength"); got != 13 {
		t.Fatalf("strength = %v, want 13 (base 10 + indefinite ward +3, brief gone)", got)
	}
}

// TestIndefiniteAffectRemovable proves an indefinite affect is NOT permanent-unto-death: remove_affect
// (and by extension dispel) still ends it — the only ways it ends.
func TestIndefiniteAffectRemovable(t *testing.T) {
	z, e := affectTestZone(t)
	z.defs.affect.register("ward", &affectDef{
		ref: "ward", name: "Ward", stacking: stackRefresh, maxStacks: 1, indefinite: true, dispellable: true,
	})
	applyAffect(e, "ward", attachOpts{source: e}, nil) // self-cast: keyed (ward, e) so remove_affect finds it
	c := &effectCtx{z: z, actor: e, source: e, target: e, mag: 1, rng: rand.New(rand.NewSource(1))}
	if err := opRemoveAffect(c, &effectOp{kind: "remove_affect", affect: "ward"}); err != nil {
		t.Fatalf("remove_affect: %v", err)
	}
	if hasAffect(e, "ward") {
		t.Fatal("remove_affect did not remove the indefinite affect")
	}
}

// TestIndefiniteAffectPersistRoundTrip proves the sentinel round-trips through save/load: a reattach
// (the persistence path) with the saved remaining (-1) restores an indefinite affect that stays indefinite.
func TestIndefiniteAffectPersistRoundTrip(t *testing.T) {
	z, e := affectTestZone(t)
	z.defs.affect.register("ward", &affectDef{
		ref: "ward", name: "Ward", stacking: stackRefresh, maxStacks: 1, indefinite: true,
	})
	applyAffect(e, "ward", attachOpts{}, nil)
	dump := dumpAffects(e)
	if len(dump) != 1 || dump[0].Remaining != durationIndefinite {
		t.Fatalf("dumpAffects = %+v, want one entry with remaining %d", dump, durationIndefinite)
	}

	// Fresh entity IN THE SAME zone (the ward def lives on z), reattach from the snapshot (remaining -1).
	e2 := newTestPlayerEntity(z, "Hero2").entity
	inst := applyAffect(e2, "ward", attachOpts{reattach: true, duration: dump[0].Remaining, magnitude: dump[0].Mag, stacks: dump[0].Stacks}, nil)
	if inst == nil || inst.remaining != durationIndefinite {
		t.Fatalf("reattached indefinite remaining = %v, want sentinel", inst)
	}
	for i := 0; i < 30; i++ {
		z.pulses.tick()
	}
	if !hasAffect(e2, "ward") {
		t.Fatal("reattached indefinite affect expired (sentinel not preserved through load)")
	}
}

// TestIndefiniteRoomFieldLeasesFinitely proves the #545-review fix: an INDEFINITE room-scoped affect
// carrying a CC/modifier leases a per-occupant copy with a FINITE duration (not the indefinite sentinel),
// so the lease still lapses after the occupant leaves — the room re-leases while they are present. Without
// the opts.duration guard the leased CC was permanent and followed the player out of the room.
func TestIndefiniteRoomFieldLeasesFinitely(t *testing.T) {
	z := newZone("test")
	z.defs.attr.register("strength", &attributeDef{ref: "strength", base: litNode{v: 10}})
	// An indefinite BENEFICIAL room field (a strength aura) — beneficial so the per-occupant lease lands
	// ungated (isolating the lease-duration fix from the PvP harm gate). roomScoped + indefinite.
	z.defs.affect.register("aura", &affectDef{
		ref: "aura", name: "Eternal Aura", stacking: stackRefresh, maxStacks: 1,
		roomScoped: true, indefinite: true, tickInterval: 2,
		modifiers: []affectModifier{{attr: "strength", add: true, value: 2}},
	})
	e := makeRoomPlayer(z, "Hero").entity
	room := e.location
	// Apply the field to the room; land it on the occupant e.
	applyRoomAffect(room, "aura", nil)
	// The occupant's LEASED copy must be finite (positive remaining), not the indefinite sentinel.
	a, ok := Get[*Affected](e)
	if !ok {
		t.Fatal("occupant did not receive a leased aura copy")
	}
	var leased *affectInstance
	for _, inst := range a.list {
		if inst.def.ref == "aura" {
			leased = inst
		}
	}
	if leased == nil {
		t.Fatal("occupant did not receive a leased aura copy")
	}
	if leased.remaining <= 0 {
		t.Fatalf("leased copy remaining = %d, want a FINITE positive lease (not the indefinite sentinel)", leased.remaining)
	}
}

// leveledZone registers three dispellable affects with distinct levels (all beneficial so a self-dispel
// stays ungated), plus applies them to e.
func leveledZone(t *testing.T) (*Zone, *Entity) {
	z, e := affectTestZone(t)
	for _, tc := range []struct {
		ref string
		lvl int
	}{{"buff_low", 1}, {"buff_high", 5}, {"buff_mid", 3}} {
		z.defs.affect.register(tc.ref, &affectDef{
			ref: tc.ref, name: tc.ref, stacking: stackRefresh, maxStacks: 1, duration: 100,
			dispellable: true, level: tc.lvl,
			modifiers: []affectModifier{{attr: "strength", add: true, value: 1}},
		})
		applyAffect(e, tc.ref, attachOpts{}, nil)
	}
	return z, e
}

// TestDispelRemovesHighestLevelFirst proves ordering: a count-limited dispel strips the HIGHEST-level
// affect first (5e Dispel Magic).
func TestDispelRemovesHighestLevelFirst(t *testing.T) {
	z, e := leveledZone(t)
	c := &effectCtx{z: z, actor: e, source: e, target: e, mag: 1, rng: rand.New(rand.NewSource(1))}
	// Remove exactly one: it must be the level-5 buff.
	if err := opDispel(c, &effectOp{kind: "dispel", amount: 1}); err != nil {
		t.Fatalf("opDispel: %v", err)
	}
	if hasAffect(e, "buff_high") {
		t.Fatal("dispel count=1 did not remove the highest-level affect first")
	}
	if !hasAffect(e, "buff_mid") || !hasAffect(e, "buff_low") {
		t.Fatal("dispel count=1 removed more than the single highest-level affect")
	}
}

// TestDispelPerAffectCheckGate proves the optional gate: a dispel roll that beats the low-level effect's
// DC but not the high-level one's removes only the weaker. DC = 10 + $affect.level; roll total = 1 + 15 =
// 16, so DC 12 (level 2) passes and DC 18 (level 8) resists.
func TestDispelPerAffectCheckGate(t *testing.T) {
	z, e := affectTestZone(t)
	z.defs.affect.register("weak_buff", &affectDef{
		ref: "weak_buff", name: "weak", stacking: stackRefresh, maxStacks: 1, duration: 100,
		dispellable: true, level: 2, modifiers: []affectModifier{{attr: "strength", add: true, value: 1}},
	})
	z.defs.affect.register("strong_buff", &affectDef{
		ref: "strong_buff", name: "strong", stacking: stackRefresh, maxStacks: 1, duration: 100,
		dispellable: true, level: 8, modifiers: []affectModifier{{attr: "strength", add: true, value: 1}},
	})
	applyAffect(e, "weak_buff", attachOpts{}, nil)
	applyAffect(e, "strong_buff", attachOpts{}, nil)

	gate := &checkSpec{
		dice:  mustDiceT("1d1"), // rolls 1
		bonus: litNode{v: 15},   // +15 -> total 16
		vs:    checkVs{dc: opFormula("+", litNode{v: 10}, an("$affect.level"))},
		bands: []checkBand{
			{marginMin: litNode{v: 0}, label: "success"}, // total >= DC -> dispelled
			{label: "resist"}, // else the affect withstands
		},
	}
	c := &effectCtx{z: z, actor: e, source: e, target: e, mag: 1, rng: rand.New(rand.NewSource(1))}
	if err := opDispel(c, &effectOp{kind: "dispel", amount: 0, check: gate}); err != nil {
		t.Fatalf("opDispel: %v", err)
	}
	if hasAffect(e, "strong_buff") == false {
		t.Fatal("level-8 affect (DC 18) should have RESISTED the total-16 dispel, but was removed")
	}
	if hasAffect(e, "weak_buff") {
		t.Fatal("level-2 affect (DC 12) should have been dispelled by the total-16 roll, but survived")
	}
}
