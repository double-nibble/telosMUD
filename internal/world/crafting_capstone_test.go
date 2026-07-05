package world

import "testing"

// crafting_capstone_test.go is the Phase-13 capstone (docs/PHASE13-PLAN.md §13.x, docs/CRAFTING.md §9): the
// material-economy loop, hermetic + -race. A player learns a trade, DISENCHANTS a bound epic into components
// — proving bound gear re-enters the economy as MATS while the transfer gate still holds (the epic itself
// can't be parted with; its mid-tier component can) — then CRAFTS a new item from those mats at a station,
// and the whole result SURVIVES A RESTART. It drives the real demo content (leatherworking + disenchant_arms
// + craft:leather_vest + the smithy forge), end to end through the command dispatcher.

func TestCraftingCapstoneDisenchantToCraftSurvivesRestart(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity

	// The player takes up leatherworking (grants the verbs + the skill track + membership).
	applyBundleTo(e.z, actor, "leatherworking")

	// They hold a BOUND epic — a soulbound steel longsword they can no longer use.
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	bindItem(sword)
	Move(sword, actor)

	// The transfer gate HOLDS: a bound epic can't be sold/dropped/traded away.
	aout, _ := e.run("drop sword")
	if !has(aout, "bound to you") {
		t.Fatalf("a bound epic must be undroppable (transfer gate); got %v", aout)
	}
	if findHeldByProto(actor, "midgaard:obj:sword") == nil {
		t.Fatal("the bound sword should still be held after a blocked drop")
	}

	// But the OWNER may DECONSTRUCT it: disenchant breaks the bound epic into components. #38: the verb is
	// object-targeted, so name the held sword.
	e.run("disenchant sword")
	if findHeldByProto(actor, "midgaard:obj:sword") != nil {
		t.Fatal("disenchant should consume the bound epic")
	}

	// The economy split (D1): the mid-tier leather component is UNBOUND/tradeable (it feeds the market);
	// the top-tier essence is BOUND (the no-trade sink).
	comp := findHeldByProto(actor, "midgaard:obj:leather")
	if comp == nil || isBound(comp) {
		t.Fatal("the salvaged leather component must be tradeable (unbound)")
	}
	essence := findHeldByProto(actor, "midgaard:obj:essence")
	if essence == nil || !isBound(essence) {
		t.Fatal("the salvaged essence must be bound (the top-tier sink)")
	}
	// The unbound component really IS tradeable; the bound essence is NOT (the gate, both directions).
	if out, _ := e.run("drop essence"); !has(out, "bound to you") {
		t.Fatalf("the bound essence must be undroppable; got %v", out)
	}

	// The player trades on the market for the rest of the leather the recipe needs (3 total; salvage gave 1).
	topup := e.z.spawn(ProtoRef("midgaard:obj:leather"))
	setItemStackCount(topup, 2)
	Move(topup, actor)
	mergeStackInto(actor, topup)
	actor.removeContent(topup)

	// At the forge (the smithy station), they craft a new vest from the mats.
	Move(actor, e.z.rooms["midgaard:room:smithy"])
	e.run("forge")

	vest := vestInInventory(actor)
	if vest == nil {
		t.Fatal("crafting at the forge should produce a leather vest")
	}
	if !isBound(vest) {
		t.Fatal("the crafted vest is bind: bound")
	}
	if findHeldByProto(actor, "midgaard:obj:essence") != nil {
		t.Fatal("the craft should have consumed the bound essence (an owner may use a bound mat)")
	}

	// --- RESTART: the player relogs. The crafted vest + the learned profession survive. ---
	snap := dumpCharacter(e.actor)
	dst := &session{character: "Alice"}
	e.z.newPlayerEntity(dst, "Alice")
	loadCharacter(e.z, dst, snap)

	if !hasProfession(dst.entity, "leatherworking") {
		t.Fatal("the learned profession must survive a restart")
	}
	rvest := vestInInventory(dst.entity)
	if rvest == nil {
		t.Fatal("the crafted vest must survive a restart")
	}
	if !isBound(rvest) {
		t.Fatal("the crafted vest must still be bound after a restart")
	}
}
