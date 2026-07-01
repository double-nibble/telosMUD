package world

import "testing"

// profession_test.go — Phase 13.3: professions + crafting ops. The done-when end to end: learning a
// profession (a bundle) grants its verb + skill track + membership; the craft verb is refused without the
// profession and runs the consume/produce ops with it; and the profession + skill survive a reload.

// applyBundleTo runs apply_bundle(ref) on entity ent through the real op interpreter (the path content
// uses), so the test exercises grant_ability + grant_track + learn_profession exactly as a chargen pick would.
func applyBundleTo(z *Zone, ent *Entity, ref string) {
	c := &effectCtx{z: z, actor: ent, source: ent, target: ent, mag: 1, disp: dispHelpful}
	runOps(c, []effectOp{{kind: "apply_bundle", bundle: ref}})
}

// glovesCount returns how many leather-gloves the entity holds (the craft output).
func glovesCount(e *Entity) (n int, anyBound bool) {
	for _, it := range e.contents {
		if string(it.proto) == "midgaard:obj:leather-gloves" {
			n++
			if isBound(it) {
				anyBound = true
			}
		}
	}
	return
}

func TestProfessionCraftLifecycle(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity

	// Two leather scraps on hand (the craft input).
	leather := e.z.spawn(ProtoRef("midgaard:obj:leather"))
	setItemStackCount(leather, 2)
	Move(leather, actor)

	// (1) The profession GATE in isolation: grant the ability (ownership) but NOT the profession. `craft`
	// must still be refused at step-3 on requires.profession — and the leather must be untouched.
	grantAbility(actor, "craft_gloves")
	aout, _ := e.run("craft")
	if !has(aout, "lack the training") {
		t.Fatalf("craft without the profession should be refused; got %v", aout)
	}
	if _, total := leatherStacks(actor); total != 2 {
		t.Fatalf("a refused craft must not consume input: leather = %d, want 2", total)
	}

	// (2) Learn the profession (the bundle): grants the verb + the skill track + membership.
	applyBundleTo(e.z, actor, "leatherworking")
	if !hasProfession(actor, "leatherworking") {
		t.Fatal("apply_bundle(leatherworking) should enroll the actor (learn_profession)")
	}
	if !hasTrack(actor, "leatherworking_skill") {
		t.Fatal("the profession bundle should grant the leatherworking_skill track")
	}

	// (3) Now `craft` succeeds: it consumes 2 leather and produces a BOUND pair of gloves.
	aout, _ = e.run("craft")
	if !has(aout, "pair of gloves") {
		t.Fatalf("craft should succeed once the profession is learned; got %v", aout)
	}
	if _, total := leatherStacks(actor); total != 0 {
		t.Fatalf("craft must consume both leather scraps: leather = %d, want 0", total)
	}
	n, bound := glovesCount(actor)
	if n != 1 || !bound {
		t.Fatalf("craft should produce 1 BOUND gloves: count=%d bound=%v", n, bound)
	}

	// (4) The profession + skill survive a reload (a fresh session re-hydrated from the snapshot).
	snap := dumpCharacter(e.actor)
	dst := &session{character: "Alice"}
	e.z.newPlayerEntity(dst, "Alice")
	loadCharacter(e.z, dst, snap)
	if !hasProfession(dst.entity, "leatherworking") {
		t.Fatal("learned profession must survive a reload")
	}
	if !hasTrack(dst.entity, "leatherworking_skill") {
		t.Fatal("granted skill track must survive a reload")
	}
}

// TestConsumeItemInsufficient: consume_item errors (aborting the op-list) when the actor is short, so a
// craft never produces from nothing.
func TestConsumeItemInsufficient(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	one := e.z.spawn(ProtoRef("midgaard:obj:leather"))
	setItemStackCount(one, 1)
	Move(one, actor)

	c := &effectCtx{z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral}
	err := opConsumeItem(c, &effectOp{kind: "consume_item", item: "midgaard:obj:leather", amount: 2})
	if err == nil {
		t.Fatal("consume_item should error when the actor holds fewer than the requested amount")
	}
	if _, total := leatherStacks(actor); total != 1 {
		t.Fatalf("a failed consume must not touch the stack: leather = %d, want 1", total)
	}
}

// TestProduceItemBindsOutput: produce_item with bind:bound forces the output bound on creation.
func TestProduceItemBindsOutput(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	c := &effectCtx{z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral}
	if err := opProduceItem(c, &effectOp{kind: "produce_item", item: "midgaard:obj:leather-gloves", bind: "bound"}); err != nil {
		t.Fatal(err)
	}
	n, bound := glovesCount(actor)
	if n != 1 || !bound {
		t.Fatalf("produce_item bind:bound should make 1 bound gloves: count=%d bound=%v", n, bound)
	}
}

// TestAugmentItemBumpsAffix: the augment_item stub bumps a flat affix on the held item's Quality delta.
func TestAugmentItemBumpsAffix(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	gloves := e.z.spawn(ProtoRef("midgaard:obj:leather-gloves"))
	Move(gloves, actor)

	c := &effectCtx{z: e.z, actor: actor, source: actor, target: actor, mag: 1, disp: dispNeutral}
	op := &effectOp{kind: "augment_item", item: "midgaard:obj:leather-gloves", attr: "armor", amount: 3}
	if err := opAugmentItem(c, op); err != nil {
		t.Fatal(err)
	}
	q, ok := Get[*Quality](gloves)
	if !ok || q.Affixes["armor"] != 3 {
		t.Fatalf("augment_item should add a +3 armor affix; got %+v (present=%v)", q, ok)
	}
	// A second augment ACCUMULATES on the same affix.
	if err := opAugmentItem(c, op); err != nil {
		t.Fatal(err)
	}
	if q, _ := Get[*Quality](gloves); q.Affixes["armor"] != 6 {
		t.Fatalf("a second augment should accumulate: armor = %v, want 6", q.Affixes["armor"])
	}
}

// TestProfessionCap: learnProfession refuses a NEW capped profession past the cap; an already-known one is
// idempotent. (No content cap attr / no uncapped bundles here => the defaultProfessionCap applies to all.)
func TestProfessionCap(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	for i := 0; i < defaultProfessionCap; i++ {
		ref := string(rune('a'+i)) + "_trade"
		if !e.z.learnProfession(actor, ref) {
			t.Fatalf("learning profession %d of %d should succeed", i+1, defaultProfessionCap)
		}
	}
	if e.z.learnProfession(actor, "one_too_many") {
		t.Fatalf("learning past the cap (%d) should be refused", defaultProfessionCap)
	}
	// Re-learning a KNOWN profession is idempotent — never refused, never double-counts.
	if !e.z.learnProfession(actor, "a_trade") {
		t.Fatal("re-learning a known profession should be idempotent (true)")
	}
}

// TestProfessionCapUncappedAndAttr: an `uncapped` profession bundle never counts against the cap, and the
// content attribute professionCapAttr overrides the default ceiling.
func TestProfessionCapUncappedAndAttr(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity

	// An uncapped (gathering) profession bundle: learnable without limit and never counts toward the cap.
	e.z.defs.bundle.register("mining", &bundleDef{ref: "mining", kind: "profession", uncapped: true})
	for i := 0; i < defaultProfessionCap+3; i++ {
		if !e.z.learnProfession(actor, "mining") { // idempotent, but proves it's never blocked
			t.Fatal("an uncapped profession must never be blocked by the cap")
		}
	}
	if got := e.z.cappedProfessionCount(actor); got != 0 {
		t.Fatalf("an uncapped profession must not count against the cap: cappedCount = %d, want 0", got)
	}

	// Fill the capped slots to the default, then raise the cap via the content attribute and learn one more.
	for i := 0; i < defaultProfessionCap; i++ {
		if !e.z.learnProfession(actor, string(rune('a'+i))+"_craft") {
			t.Fatalf("capped profession %d should fit under the default cap", i+1)
		}
	}
	if e.z.learnProfession(actor, "extra_craft") {
		t.Fatal("a capped profession past the default cap should be refused")
	}
	e.z.defs.attr.register(professionCapAttr, &attributeDef{ref: professionCapAttr, base: litNode{v: 0}})
	setAttrBase(actor, professionCapAttr, float64(defaultProfessionCap+1))
	if !e.z.learnProfession(actor, "extra_craft") {
		t.Fatalf("raising %s should admit one more capped profession", professionCapAttr)
	}
}
