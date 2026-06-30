package world

import "testing"

// recipe_test.go — Phase 13.5: recipe-driven crafting. The done-when: a character with the profession crafts
// an item at the required station from its component inputs (consumed), the output lands in inventory, and
// crafting without the station/skill/components is refused.

// giveComponents puts the leather (×3) + essence (×1) the leather-vest recipe needs into the actor.
func giveComponents(z *Zone, actor *Entity) {
	leather := z.spawn(ProtoRef("midgaard:obj:leather"))
	setItemStackCount(leather, 3)
	Move(leather, actor)
	essence := z.spawn(ProtoRef("midgaard:obj:essence"))
	setItemStackCount(essence, 1)
	Move(essence, actor)
}

// vestInInventory returns the crafted leather-vest in e's inventory (or nil).
func vestInInventory(e *Entity) *Entity {
	for _, it := range e.contents {
		if string(it.proto) == "midgaard:obj:leather-vest" {
			return it
		}
	}
	return nil
}

func TestCraftRecipeAtStation(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	// Stand at the forge (the smithy room carries the `forge` station flag).
	smithy := e.z.rooms["midgaard:room:smithy"]
	Move(actor, smithy)

	applyBundleTo(e.z, actor, "leatherworking")
	giveComponents(e.z, actor)

	e.run("forge")

	vest := vestInInventory(actor)
	if vest == nil {
		t.Fatal("forging at the station should produce a leather vest")
	}
	if !isBound(vest) {
		t.Fatal("the recipe output is bind: bound — the vest should be bound")
	}
	// Inputs consumed (3 leather + 1 essence -> 0 of each).
	if _, leather := leatherStacks(actor); leather != 0 {
		t.Fatalf("craft must consume all 3 leather: %d left", leather)
	}
	if findHeldByProto(actor, "midgaard:obj:essence") != nil {
		t.Fatal("craft must consume the essence")
	}
	// Coarse quality band: quality_base (5) + the crafter's leatherworking skill level (0 here) = 5.
	if q, ok := Get[*Quality](vest); !ok || q.Level != 5 {
		t.Fatalf("vest quality level = %v (present=%v), want 5 (base + skill 0)", q, ok)
	}
}

// TestCraftRecipeScalesWithSkill: a higher leatherworking level raises the crafted item's quality band.
func TestCraftRecipeScalesWithSkill(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	Move(actor, e.z.rooms["midgaard:room:smithy"])
	applyBundleTo(e.z, actor, "leatherworking")
	setAttrBase(actor, "leatherworking", 4) // a skilled crafter
	giveComponents(e.z, actor)

	e.run("forge")

	vest := vestInInventory(actor)
	if vest == nil {
		t.Fatal("expected a crafted vest")
	}
	if q, ok := Get[*Quality](vest); !ok || q.Level != 9 {
		t.Fatalf("vest quality level = %v, want 9 (base 5 + skill 4)", q)
	}
}

// TestCraftRecipeRefusedOffStation: the recipe requires the forge; crafting elsewhere is refused and the
// inputs are untouched.
func TestCraftRecipeRefusedOffStation(t *testing.T) {
	e := newCmdEnv(t) // actor starts in the temple (no forge flag)
	actor := e.actor.entity
	applyBundleTo(e.z, actor, "leatherworking")
	giveComponents(e.z, actor)

	aout, _ := e.run("forge")
	if !has(aout, "station") {
		t.Fatalf("crafting off-station should be refused with a station message; got %v", aout)
	}
	if vestInInventory(actor) != nil {
		t.Fatal("a refused craft must not produce the vest")
	}
	if _, leather := leatherStacks(actor); leather != 3 {
		t.Fatalf("a refused craft must not consume inputs: leather = %d, want 3", leather)
	}
}

// TestCraftRecipeRefusedWithoutComponents: at the station with the profession but no components, the craft
// is refused and nothing is produced.
func TestCraftRecipeRefusedWithoutComponents(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	Move(actor, e.z.rooms["midgaard:room:smithy"])
	applyBundleTo(e.z, actor, "leatherworking")

	aout, _ := e.run("forge")
	if !has(aout, "components") {
		t.Fatalf("crafting without components should be refused; got %v", aout)
	}
	if vestInInventory(actor) != nil {
		t.Fatal("a refused craft must not produce the vest")
	}
}
