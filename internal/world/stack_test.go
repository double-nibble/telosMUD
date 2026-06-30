package world

import "testing"

// stack_test.go — Phase 13.2 stackable materials: merge-on-pickup (bounded by max), the split command, and
// the stack count surviving a reload.

// leatherStacks returns the leather-material entities in e's contents + their summed count.
func leatherStacks(e *Entity) (entities int, total int) {
	for _, it := range e.contents {
		if string(it.proto) == "midgaard:obj:leather" {
			entities++
			total += itemStackCount(it)
		}
	}
	return
}

func TestStackMergeOnPickup(t *testing.T) {
	e := newCmdEnv(t)
	// Two separate leather stacks on the floor (each count 1).
	Move(e.z.spawn(ProtoRef("midgaard:obj:leather")), e.room)
	Move(e.z.spawn(ProtoRef("midgaard:obj:leather")), e.room)

	e.run("get leather")
	e.run("get leather")

	ents, total := leatherStacks(e.actor.entity)
	if ents != 1 || total != 2 {
		t.Fatalf("after picking up two stacks: %d entities / %d total, want 1 entity / 2 total (merged)", ents, total)
	}
}

func TestStackMergeBoundedByMax(t *testing.T) {
	e := newCmdEnv(t)
	// A held stack near the cap (max 20) + a floor stack that overflows it.
	held := e.z.spawn(ProtoRef("midgaard:obj:leather"))
	setItemStackCount(held, 19)
	Move(held, e.actor.entity)
	floor := e.z.spawn(ProtoRef("midgaard:obj:leather"))
	setItemStackCount(floor, 5)
	Move(floor, e.room)

	e.run("get leather") // 19 + 5 -> 20 held (capped) + 4 left in a separate stack

	ents, total := leatherStacks(e.actor.entity)
	if total != 24 {
		t.Fatalf("total leather = %d, want 24 (nothing lost)", total)
	}
	if ents != 2 {
		t.Fatalf("entities = %d, want 2 (a full stack of 20 + an overflow of 4)", ents)
	}
}

func TestStackSplit(t *testing.T) {
	e := newCmdEnv(t)
	stack := e.z.spawn(ProtoRef("midgaard:obj:leather"))
	setItemStackCount(stack, 5)
	Move(stack, e.actor.entity)

	e.run("split 2 leather")

	ents, total := leatherStacks(e.actor.entity)
	if ents != 2 || total != 5 {
		t.Fatalf("after split: %d entities / %d total, want 2 entities / 5 total (3 + 2)", ents, total)
	}
	// Splitting the whole stack (or more) is refused.
	whole := e.z.spawn(ProtoRef("midgaard:obj:leather"))
	setItemStackCount(whole, 3)
	Move(whole, e.actor.entity)
	before, _ := leatherStacks(e.actor.entity)
	e.run("split 3 leather") // == the whole stack -> no-op
	after, _ := leatherStacks(e.actor.entity)
	if after != before {
		t.Fatal("splitting the whole stack should be a no-op")
	}
}

func TestStackCountSurvivesReload(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Tinker"}
	pe := z.newPlayerEntity(src, "Tinker")
	stack := z.spawn(ProtoRef("midgaard:obj:leather"))
	setItemStackCount(stack, 7)
	Move(stack, pe)

	snap := dumpCharacter(src)
	dst := &session{character: "Tinker"}
	z.newPlayerEntity(dst, "Tinker")
	loadCharacter(z, dst, snap)

	_, total := leatherStacks(dst.entity)
	if total != 7 {
		t.Fatalf("reloaded stack count = %d, want 7 (a partial stack must survive)", total)
	}
}
