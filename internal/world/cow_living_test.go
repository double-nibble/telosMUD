package world

import (
	"reflect"
	"testing"
)

// cow_living_test.go is the REGRESSION for the copy-on-write (COW) corruption bug: a spawned mob's
// combat/death MUST NOT mutate its SHARED PROTOTYPE's Living component, or the next mob spawned from
// that prototype (a repop) is born broken (posDead, 0 hp, stale fighting pointer). The bug was latent
// until repop (reset_secs) re-read the corrupted prototype; a killed-then-repopped goblin came back
// un-attackable (startFight rejects a posDead target). The fix routes every Living mutator through the
// mutableLiving COW choke-point (component.go) so an instance's mutation forks its own Living.
//
// These tests assert BOTH directions of the invariant:
//   - a SECOND mob spawned from a prototype is pristine after the first ran a full combat/death cycle;
//   - the PROTOTYPE's Living is byte-for-byte untouched by an instance's mutations.

// TestProtoLivingNotCorruptedByInstanceMutation runs the exact mutator sequence a goblin's fight +
// death performs (fighting pointer + position + hp damage + a flag) on a spawned mob, then asserts the
// prototype's Living template is untouched and a freshly-spawned SIBLING is pristine. Pre-fix, the
// mutators wrote `e.living.X` straight through the shared prototype pointer, corrupting both.
func TestProtoLivingNotCorruptedByInstanceMutation(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	const ref ProtoRef = "darkwood:mob:goblin"

	proto := z.protos.get(ref)
	if proto == nil {
		t.Fatalf("demo pack missing prototype %q", ref)
	}
	protoLiving, ok := proto.comps[reflect.TypeFor[*Living]()].(*Living)
	if !ok {
		t.Fatalf("prototype %q has no Living template", ref)
	}
	// Snapshot the prototype's template state so we can prove it is untouched afterward.
	wantPos := protoLiving.position
	wantResLen := len(protoLiving.resCur)
	wantFlagsLen := len(protoLiving.flags)
	if protoLiving.fighting != nil {
		t.Fatalf("prototype Living.fighting must start nil")
	}

	// First mob: run the full combat/death mutator sequence.
	mob := z.spawn(ref)
	foe := z.spawn(ref)
	if mob == nil || foe == nil {
		t.Fatalf("spawn returned nil")
	}
	z.startFight(mob, foe) // sets mob.fighting=foe, position=posFighting (and foe retaliates)
	setResourceCurrent(mob, "hp", 0)
	setFlag(mob, "tagged", true)
	setPosition(mob, posDead) // the death path's final write

	// Invariant 1: the PROTOTYPE's Living template is byte-for-byte untouched.
	if protoLiving.position != wantPos {
		t.Fatalf("prototype Living.position corrupted: got %d, want %d", protoLiving.position, wantPos)
	}
	if protoLiving.fighting != nil {
		t.Fatalf("prototype Living.fighting corrupted: got a stale pointer, want nil")
	}
	if len(protoLiving.resCur) != wantResLen {
		t.Fatalf("prototype Living.resCur corrupted: got %v (len %d), want len %d",
			protoLiving.resCur, len(protoLiving.resCur), wantResLen)
	}
	if len(protoLiving.flags) != wantFlagsLen {
		t.Fatalf("prototype Living.flags corrupted: got %v (len %d), want len %d",
			protoLiving.flags, len(protoLiving.flags), wantFlagsLen)
	}

	// Invariant 2: a SECOND mob spawned from the same prototype (the repop) is PRISTINE.
	repop := z.spawn(ref)
	if repop == nil {
		t.Fatalf("repop spawn returned nil")
	}
	if got := position(repop); got != posStanding {
		t.Fatalf("repopped mob born in position %d, want posStanding (%d) — proto was corrupted",
			got, posStanding)
	}
	if repop.living.fighting != nil {
		t.Fatalf("repopped mob born with a stale fighting pointer — proto was corrupted")
	}
	if hasFlag(repop, "tagged") {
		t.Fatalf("repopped mob born with the first mob's 'tagged' flag — proto/flags-map was corrupted")
	}
	// Full vital: max_hp is con 1 => 15; an un-mutated (full) goblin reads its derived max.
	if got, maxV := resourceCurrent(repop, "hp"), resourceMax(repop, "hp"); got != maxV {
		t.Fatalf("repopped mob born at hp %d/%d, want full — proto/resCur-map was corrupted", got, maxV)
	}
}

// TestSpawnedMobMutationDoesNotAliasSibling proves two instances of one prototype do not alias each
// other's Living through the shared template: mutating mob A's hp/position/flags leaves mob B (spawned
// before A's mutation) reading its own pristine values. This is the sibling-aliasing facet of the bug.
func TestSpawnedMobMutationDoesNotAliasSibling(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	const ref ProtoRef = "darkwood:mob:goblin"

	a := z.spawn(ref)
	b := z.spawn(ref)
	if a == nil || b == nil {
		t.Fatalf("spawn returned nil")
	}

	// Mutate A through every Living write path.
	setPosition(a, posDead)
	setResourceCurrent(a, "hp", 3)
	setFlag(a, "tagged", true)
	setAttrBase(a, "strength", 99)

	// B must be untouched.
	if got := position(b); got != posStanding {
		t.Fatalf("sibling B position %d after A mutated, want posStanding", got)
	}
	if got, maxV := resourceCurrent(b, "hp"), resourceMax(b, "hp"); got != maxV {
		t.Fatalf("sibling B hp %d/%d after A wounded, want full", got, maxV)
	}
	if hasFlag(b, "tagged") {
		t.Fatalf("sibling B inherited A's 'tagged' flag")
	}
	if got := attr(b, "strength"); got == 99 {
		t.Fatalf("sibling B inherited A's strength override (99) — attrBase aliased")
	}
}
