package world

import (
	"math/rand"
	"testing"
)

// salvage_test.go — Phase 13.4: deconstruction. The done-when: an OWNER disenchants a BOUND source into a
// tradeable mid-tier component (unbound) + a bound top-tier essence (tier-dependent binding, D1), the source
// is consumed, and the yield is a deterministic weighted roll under a seed.

func TestSalvageBoundSourceTierBinding(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity

	// A BOUND steel longsword in hand (a soulbound epic the owner wants to break down).
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	bindItem(sword)
	Move(sword, actor)
	if !isBound(sword) {
		t.Fatal("precondition: the source sword should be bound")
	}

	c := &effectCtx{
		z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral,
		rng: rand.New(rand.NewSource(1)),
	}
	op := &effectOp{kind: "salvage_item", item: "midgaard:obj:sword", table: "disenchant_arms"}
	if err := opSalvageItem(c, op); err != nil {
		t.Fatal(err)
	}

	// The source is consumed even though it was BOUND (owner deconstruction, §1).
	if findHeldByProto(actor, "midgaard:obj:sword") != nil {
		t.Fatal("salvage must consume the bound source item")
	}
	// A tradeable mid-tier component: leather, UNBOUND (feeds the market).
	leather := findHeldByProto(actor, "midgaard:obj:leather")
	if leather == nil {
		t.Fatal("salvage should yield a leather component")
	}
	if isBound(leather) {
		t.Fatal("a mid-tier (uncommon) component must stay UNBOUND/tradeable")
	}
	// A bound top-tier essence (the epic tier's binds flag, D1).
	essence := findHeldByProto(actor, "midgaard:obj:essence")
	if essence == nil {
		t.Fatal("salvage should yield an arcane essence")
	}
	if !isBound(essence) {
		t.Fatal("the top-tier (epic) essence must be BOUND on creation (the no-trade sink, D1)")
	}
}

// TestDisenchantRefusedWithoutProfession: the verb is gated on requires.profession — a non-member is refused
// and the source is untouched.
func TestDisenchantRefusedWithoutProfession(t *testing.T) {
	e := newCmdEnv(t)
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	bindItem(sword)
	Move(sword, e.actor.entity)

	aout, _ := e.run("disenchant")
	if !has(aout, "lack the training") {
		t.Fatalf("disenchant without the profession should be refused; got %v", aout)
	}
	if findHeldByProto(e.actor.entity, "midgaard:obj:sword") == nil {
		t.Fatal("a refused disenchant must not consume the source")
	}
}

// TestDisenchantVerbAfterLearning: once leatherworking is learned, the disenchant verb runs the salvage,
// consuming the sword and yielding the components (the end-to-end content path).
func TestDisenchantVerbAfterLearning(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	bindItem(sword)
	Move(sword, actor)

	applyBundleTo(e.z, actor, "leatherworking")
	e.run("disenchant")

	if findHeldByProto(actor, "midgaard:obj:sword") != nil {
		t.Fatal("disenchant should consume the sword")
	}
	if findHeldByProto(actor, "midgaard:obj:essence") == nil {
		t.Fatal("disenchant should yield an arcane essence")
	}
}

// TestSalvageUnknownTableErrors: a salvage op with no resolvable table errors (and does not consume the
// source before it checks).
func TestSalvageUnknownTableErrors(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	sword := e.z.spawn(ProtoRef("midgaard:obj:sword"))
	Move(sword, actor)

	c := &effectCtx{z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral}
	op := &effectOp{kind: "salvage_item", item: "midgaard:obj:sword", table: "nonexistent"}
	if err := opSalvageItem(c, op); err == nil {
		t.Fatal("salvage_item with an unknown table should error")
	}
	if findHeldByProto(actor, "midgaard:obj:sword") == nil {
		t.Fatal("a salvage that errors on the table must not have consumed the source")
	}
}
