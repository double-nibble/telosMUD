package world

import "testing"

// encumbrance_test.go — #548 carrying capacity + the prevents:move walk fix.

// TestCarriedWeight sums Physical weight across carried (and worn — same contents) items.
func TestCarriedWeight(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	if w := carriedWeight(actor); w != 0 {
		t.Fatalf("empty carriedWeight = %d, want 0", w)
	}
	addTestItem(e.z, actor, "a rock", []string{"rock"}, &Physical{weight: 5})
	addTestItem(e.z, actor, "a boulder", []string{"boulder"}, &Physical{weight: 20})
	addTestItem(e.z, actor, "a feather", []string{"feather"}) // no Physical => weight 0
	if w := carriedWeight(actor); w != 25 {
		t.Fatalf("carriedWeight = %d, want 25", w)
	}
}

// TestItemWeightCountsStacks (#548): a stackable's weight is its unit weight × its stack count — closing the
// bypass where a big heavy stack registered as one unit inside the gated pickup seams.
func TestItemWeightCountsStacks(t *testing.T) {
	e := newCmdEnv(t)
	item := addTestItem(e.z, e.room, "iron ingots", []string{"ingots", "iron"}, &Physical{weight: 10})
	setItemStackCount(item, 7) // adds a Stack component (count 7)
	if w := itemWeight(item); w != 70 {
		t.Fatalf("itemWeight of a 7-count weight-10 stack = %d, want 70", w)
	}
	// A single non-stackable item is one unit.
	single := addTestItem(e.z, e.room, "a rock", []string{"rock"}, &Physical{weight: 5})
	if w := itemWeight(single); w != 5 {
		t.Fatalf("itemWeight of a single item = %d, want 5", w)
	}
}

// TestCanCarryUngatedWithoutCapacity: a pack that models no encumbrance (no carry_capacity attr) never gates.
func TestCanCarryUngatedWithoutCapacity(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	if !canCarry(actor, 1_000_000) {
		t.Fatal("with no carry_capacity attribute the gate must be a no-op (ungated)")
	}
}

// TestCanCarryEnforcesPositiveCapacity: with a positive carry_capacity, an over-cap load is refused.
func TestCanCarryEnforcesPositiveCapacity(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	e.z.defs.attr.register("carry_capacity", &attributeDef{ref: "carry_capacity", base: litNode{v: 30}})

	if !canCarry(actor, 30) {
		t.Fatal("a load exactly at capacity should be allowed")
	}
	if canCarry(actor, 31) {
		t.Fatal("a load over capacity must be refused")
	}
	addTestItem(e.z, actor, "a rock", []string{"rock"}, &Physical{weight: 25}) // now carrying 25 of 30
	if !canCarry(actor, 5) {
		t.Fatal("5 more on a 25/30 load fits")
	}
	if canCarry(actor, 6) {
		t.Fatal("6 more on a 25/30 load exceeds and must be refused")
	}
}

// TestCarryCapacityDerivedFromStrength (#548): carry_capacity is PURE CONTENT — a derived attribute
// (the 5e Str×15), evaluated by the attribute machinery with no engine code. The gate reads attr(e,
// "carry_capacity"); the formula is flavor.
func TestCarryCapacityDerivedFromStrength(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	e.z.defs.attr.register("carry_capacity", &attributeDef{
		ref:  "carry_capacity",
		base: opNode{op: "*", args: []formulaNode{attrNode{ref: "strength"}, litNode{v: 15}}},
	})
	str := attr(actor, "strength")
	if str <= 0 {
		t.Fatal("precondition: the demo actor should have a positive strength")
	}
	if got := attr(actor, "carry_capacity"); got != str*15 {
		t.Fatalf("carry_capacity = %v, want strength*15 = %v (derived-attr formula)", got, str*15)
	}
}

// TestCmdGetRefusesOverCapacity: `get` refuses an item that would exceed carry_capacity; the item stays.
func TestCmdGetRefusesOverCapacity(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	e.z.defs.attr.register("carry_capacity", &attributeDef{ref: "carry_capacity", base: litNode{v: 10}})
	heavy := addTestItem(e.z, e.room, "a boulder", []string{"boulder"}, &Physical{weight: 50})

	e.run("get boulder")
	if heavy.location == actor {
		t.Fatal("an over-capacity item was picked up")
	}
	if heavy.location != e.room {
		t.Fatalf("the boulder should stay on the floor (in %v)", heavy.location)
	}

	// A light item still gets picked up (the gate is not a blanket refusal).
	light := addTestItem(e.z, e.room, "a pebble", []string{"pebble"}, &Physical{weight: 3})
	e.run("get pebble")
	if light.location != actor {
		t.Fatal("a within-capacity item should still be pickup-able")
	}
}

// TestGetFromSkipsOverCapacity: `get all <container>` takes what fits and leaves the rest.
func TestGetFromSkipsOverCapacity(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	e.z.defs.attr.register("carry_capacity", &attributeDef{ref: "carry_capacity", base: litNode{v: 10}})
	box := addTestItem(e.z, e.room, "a chest", []string{"chest"}, &Container{capacity: 10})
	light := addTestItem(e.z, box, "a coin", []string{"coin"}, &Physical{weight: 4})
	heavy := addTestItem(e.z, box, "an anvil", []string{"anvil"}, &Physical{weight: 40})

	e.run("get all chest")
	if light.location != actor {
		t.Fatal("the light coin should have been taken")
	}
	if heavy.location == actor {
		t.Fatal("the over-capacity anvil should have been left in the chest")
	}
}

// TestPreventsMoveBlocksWalk (#548): the CONFIRMED walk-path bug — a `prevents: move` affect (root/web/
// grapple) now bars a normal directional walk, not only `flee`. An unaffected player walks; a rooted one is
// held; removing the root restores movement.
func TestPreventsMoveBlocksWalk(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := newTestPlayerEntity(z, "Rooted")
	Move(s.entity, z.rooms["midgaard:room:temple"])
	z.defs.affect.register("root", &affectDef{ref: "root", name: "Rooted", indefinite: true, prevents: []string{"move"}})

	applyAffect(s.entity, "root", attachOpts{}, nil)
	z.dispatch(s, "north") // temple --north--> market
	if s.entity.location != z.rooms["midgaard:room:temple"] {
		t.Fatalf("a rooted player walked out (now in %v)", s.entity.location)
	}

	// Remove the root: movement is restored.
	a, _ := Get[*Affected](s.entity)
	a.expire(s.entity, a.byKey[keyFor(z.defs.affect.get("root"), nil)], nil)
	z.dispatch(s, "north")
	if s.entity.location != z.rooms["midgaard:room:market"] {
		t.Fatalf("an un-rooted player could not walk (in %v)", s.entity.location)
	}
}

// TestPreventsMoveRefusesRestingWithoutStanding (#548): a rooted RESTING player is refused the walk WITHOUT
// being stood up first (the guard sits before auto-stand), mirroring flee.
func TestPreventsMoveRefusesRestingWithoutStanding(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := newTestPlayerEntity(z, "Rooted")
	Move(s.entity, z.rooms["midgaard:room:temple"])
	z.defs.affect.register("root", &affectDef{ref: "root", name: "Rooted", indefinite: true, prevents: []string{"move"}})
	applyAffect(s.entity, "root", attachOpts{}, nil)
	setPosition(s.entity, posResting)

	z.dispatch(s, "north")
	if s.entity.location != z.rooms["midgaard:room:temple"] {
		t.Fatal("a rooted resting player walked out")
	}
	if position(s.entity) != posResting {
		t.Fatal("a refused walk must not stand the rooted player up (the guard precedes auto-stand)")
	}
}
