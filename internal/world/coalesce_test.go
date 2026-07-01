package world

import (
	"reflect"
	"testing"
)

// coalesce_test.go — identical-item coalescing in listings (Track 1, coalesceItemLines): group identical
// DISCRETE items into "<Name> (N)"; a bound/quality-varied item (different per-instance delta) never merges
// with a plain one; materials + containers list individually; lines are presentation-capped.

func TestCoalesceItemLines(t *testing.T) {
	z := newZone("test")
	mk := func(proto, short string, comps ...Component) *Entity {
		it := z.newEntity(ProtoRef(proto))
		it.setShort(short)
		for _, c := range comps {
			addAny(it, c)
		}
		return it
	}
	boundTorch := mk("test:obj:torch", "a torch")
	bindItem(boundTorch) // a distinct per-instance delta

	items := []*Entity{
		mk("test:obj:torch", "a torch"),
		mk("test:obj:torch", "a torch"),
		mk("test:obj:torch", "a torch"), // three plain torches → group of 3
		boundTorch,                      // same proto+short, but BOUND → separate line
		mk("test:obj:coin", "a gold coin"),
		mk("test:obj:scrap", "a leather scrap", &ItemMeta{maxStack: 20}),
		mk("test:obj:scrap", "a leather scrap", &ItemMeta{maxStack: 20}), // materials never group
		mk("test:obj:chest", "a chest", &Container{}),
		mk("test:obj:chest", "a chest", &Container{}), // containers never group
	}
	got := coalesceItemLines(items, (*Entity).Name)
	want := []string{
		"A torch (3)",     // 3 plain torches, coalesced + initial-capped
		"A torch",         // the BOUND torch — distinct delta, not merged
		"A gold coin",     // single, uncounted
		"A leather scrap", // materials list individually...
		"A leather scrap",
		"A chest", // ...as do containers (hidden contents differ)
		"A chest",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coalesceItemLines =\n  %q\nwant\n  %q", got, want)
	}
}

func TestInventoryCoalesces(t *testing.T) {
	e := newCmdEnv(t)
	for i := 0; i < 3; i++ {
		addTestItem(e.z, e.actor.entity, "a torch", []string{"torch"})
	}
	addTestItem(e.z, e.actor.entity, "a gold coin", []string{"coin"})
	bindItem(addTestItem(e.z, e.actor.entity, "a torch", []string{"torch"})) // bound → distinct

	aout, _ := e.run("inventory")
	if !has(aout, "A torch (3)") {
		t.Fatalf("identical torches did not coalesce: %v", aout)
	}
	if has(aout, "A torch (4)") {
		t.Fatalf("the bound torch was wrongly merged with the plain three: %v", aout)
	}
	if !has(aout, "A gold coin") || has(aout, "A gold coin (") {
		t.Fatalf("the single coin should render uncounted: %v", aout)
	}
}

func TestLookRoomCoalescesGroundItems(t *testing.T) {
	z := newZone("test")
	s := makeRoomPlayer(z, "Looker")
	for i := 0; i < 2; i++ {
		it := z.newEntity(ProtoRef("test:obj:torch"))
		it.setShort("a torch")
		it.setLong("a torch lies here.")
		Move(it, s.entity.location)
	}
	gob := makeMobTarget(z, s.entity, "a goblin")
	gob.setLong("A goblin snarls.")

	z.lookRoom(s)
	lines := drainCombat(s)
	if !contains(lines, "A torch lies here. (2)") {
		t.Fatalf("identical ground items did not coalesce: %v", lines)
	}
	if !contains(lines, "A goblin snarls.") {
		t.Fatalf("the (individual) goblin did not render: %v", lines)
	}
}
