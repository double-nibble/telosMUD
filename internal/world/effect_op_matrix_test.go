package world

import (
	"strings"
	"testing"
)

// effect_op_matrix_test.go fills the direct-unit-coverage gaps in the declarative effect-op vocabulary
// (effect_op.go effectOpHandlers): opRestore, opSend, and opAct had no focused handler test — every
// other op (deal_damage/heal/modify_resource/apply_affect/remove_affect/dispel/if/chance/check) is
// pinned in ability_test.go or check_test.go. These three are the "flavor/utility" ops, but they ship
// in content op-lists and their contracts (alias equivalence, no-session no-op, to-routing) are exactly
// the kind of quiet behavior a refactor can break without any other test noticing.

// TestOpRestoreRaisesPoolClampedAtMax pins opRestore's OWN contract: it raises a depleted pool and
// clamps at the pool max. It deliberately does NOT assert equivalence with heal — opRestore delegates to
// opHeal TODAY (effect_op_handlers.go), but the handler doc names a planned divergence (a later slice
// where restore ignores healing-reduction debuffs), so equivalence is an implementation detail the
// design intends to remove, not an invariant to lock. These raise+clamp assertions stay true across
// that future specialization.
func TestOpRestoreRaisesPoolClampedAtMax(t *testing.T) {
	z, caster := abilityTestZone(t)

	// restore raises a depleted pool...
	setResourceCurrent(caster.entity, "hp", 40)
	c := seededCtx(z, caster.entity, caster.entity, dispHelpful)
	if err := opRestore(c, &effectOp{resource: "hp", amount: 25}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := resourceCurrent(caster.entity, "hp"); got != 65 {
		t.Fatalf("restore 25 from 40 -> %d, want 65", got)
	}
	// ...and clamps at the pool max (max_hp = 100 in abilityTestZone).
	if err := opRestore(c, &effectOp{resource: "hp", amount: 1000}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := resourceCurrent(caster.entity, "hp"); got != 100 {
		t.Fatalf("restore past max -> %d, want clamp 100", got)
	}
}

// TestOpHealRollsDice pins the docs/REMAINING.md §4 fix: a restorative op honors a dice roll (and dice_num/
// dice_size), not just a flat amount — a `2d8` heal raises the pool by a rolled 2..16, and `amount` stacks
// on top of the dice. restore delegates to heal, so it inherits the dice form. (The `bonus` formula path is
// the same evalCheckFormula call opDealDamage already exercises.)
func TestOpHealRollsDice(t *testing.T) {
	z, caster := abilityTestZone(t)

	// A pure-dice heal (2d8, no flat amount) raises hp by a rolled amount within [2, 16].
	setResourceCurrent(caster.entity, "hp", 0)
	c := seededCtx(z, caster.entity, caster.entity, dispHelpful)
	if err := opHeal(c, &effectOp{resource: "hp", diceNum: 2, diceSize: 8}); err != nil {
		t.Fatalf("heal dice: %v", err)
	}
	if got := resourceCurrent(caster.entity, "hp"); got < 2 || got > 16 {
		t.Fatalf("2d8 heal from 0 -> %d, want within [2,16] (dice must contribute)", got)
	}

	// amount stacks on top of the dice: 5 + 2d8 lands in [7, 21].
	setResourceCurrent(caster.entity, "hp", 0)
	if err := opHeal(c, &effectOp{resource: "hp", amount: 5, diceNum: 2, diceSize: 8}); err != nil {
		t.Fatalf("heal amount+dice: %v", err)
	}
	if got := resourceCurrent(caster.entity, "hp"); got < 7 || got > 21 {
		t.Fatalf("5+2d8 heal from 0 -> %d, want within [7,21]", got)
	}

	// restore (delegates to heal) inherits the dice form.
	setResourceCurrent(caster.entity, "hp", 0)
	if err := opRestore(c, &effectOp{resource: "hp", diceNum: 1, diceSize: 6}); err != nil {
		t.Fatalf("restore dice: %v", err)
	}
	if got := resourceCurrent(caster.entity, "hp"); got < 1 || got > 6 {
		t.Fatalf("1d6 restore from 0 -> %d, want within [1,6]", got)
	}
}

// TestOpSendDeliversToPlayerAndNoOpsOtherwise pins opSend's three cases: it puts the text on a PLAYER
// target's session, and is a clean no-op (no panic, no error) for a session-less MOB target and for a
// nil target. opSend is the direct-to-target whisper op; a mob/nil target reaching the s.send path would
// panic, so the no-op guards matter.
func TestOpSendDeliversToPlayerAndNoOpsOtherwise(t *testing.T) {
	z, caster := abilityTestZone(t)

	// PLAYER target: the text lands on their session.
	target := makePlayerTargetInRoom(z, caster.entity, "Listener")
	if err := opSend(seededCtx(z, caster.entity, target.entity, dispHelpful), &effectOp{text: "a whisper reaches you"}); err != nil {
		t.Fatalf("send to player: %v", err)
	}
	if !drainContains(t, target, "a whisper reaches you") {
		t.Fatal("opSend did not deliver its text to the player target's session")
	}

	// MOB target (no session): a silent, clean no-op.
	mob := makeMobTarget(z, caster.entity, "goblin")
	if err := opSend(seededCtx(z, caster.entity, mob, dispHelpful), &effectOp{text: "ignored"}); err != nil {
		t.Fatalf("send to a session-less mob should be a clean no-op, got %v", err)
	}

	// nil target: a clean no-op.
	if err := opSend(seededCtx(z, caster.entity, nil, dispHelpful), &effectOp{text: "void"}); err != nil {
		t.Fatalf("send to a nil target should be a clean no-op, got %v", err)
	}

	// VERBATIM (markup-is-data): opSend delivers op.text RAW — it does NOT route through act-style
	// $-token rendering (contrast opAct below). A text full of would-be tokens must arrive byte-for-byte,
	// so a refactor that "unifies the comms ops" and starts rendering opSend is caught here.
	raw := "you owe $n 50% of the {gold} — $N keeps the rest"
	if err := opSend(seededCtx(z, caster.entity, target.entity, dispHelpful), &effectOp{text: raw}); err != nil {
		t.Fatalf("send verbatim: %v", err)
	}
	if !drainContains(t, target, raw) {
		t.Fatal("opSend rendered/altered its text; it must deliver markup verbatim ($-tokens are data)")
	}
}

// TestOpActRoutesByTo pins opAct's to-routing: `to: actor` reaches only the actor, `to: victim` reaches
// only the bound target, and the default (`to: room`) reaches the room's other occupants but NOT the
// actor. opAct maps op.to -> ToActor/ToVictim/ToRoom and calls z.act with the target as the VICTIM
// referent; this asserts each fan-out lands on exactly the right session(s).
func TestOpActRoutesByTo(t *testing.T) {
	z, caster := abilityTestZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	bystander := makePlayerTargetInRoom(z, caster.entity, "Bystander")
	// caster, victim, bystander now share caster's room.

	flushAll := func() { drainCombat(caster); drainCombat(victim); drainCombat(bystander) }
	got := func(s *session) []string { return drainCombat(s) }
	has := func(lines []string, sub string) bool {
		for _, l := range lines {
			if strings.Contains(l, sub) {
				return true
			}
		}
		return false
	}

	// to: actor — only the actor sees it.
	flushAll()
	opAct(seededCtx(z, caster.entity, victim.entity, dispHelpful), &effectOp{text: "ACTOR_ONLY_LINE", to: "actor"})
	if !has(got(caster), "ACTOR_ONLY_LINE") {
		t.Fatal("opAct to:actor did not reach the actor")
	}
	if v, b := got(victim), got(bystander); has(v, "ACTOR_ONLY_LINE") || has(b, "ACTOR_ONLY_LINE") {
		t.Fatal("opAct to:actor leaked to the victim/bystander")
	}

	// to: victim — only the bound target sees it.
	flushAll()
	opAct(seededCtx(z, caster.entity, victim.entity, dispHelpful), &effectOp{text: "VICTIM_ONLY_LINE", to: "victim"})
	if !has(got(victim), "VICTIM_ONLY_LINE") {
		t.Fatal("opAct to:victim did not reach the victim")
	}
	if cl, b := got(caster), got(bystander); has(cl, "VICTIM_ONLY_LINE") || has(b, "VICTIM_ONLY_LINE") {
		t.Fatal("opAct to:victim leaked to the actor/bystander")
	}

	// default (to: room) — every other room occupant sees it, the actor does not.
	flushAll()
	opAct(seededCtx(z, caster.entity, victim.entity, dispHelpful), &effectOp{text: "ROOM_LINE"})
	if v, b := got(victim), got(bystander); !has(v, "ROOM_LINE") || !has(b, "ROOM_LINE") {
		t.Fatal("opAct default to:room did not reach both other room occupants")
	}
	if has(got(caster), "ROOM_LINE") {
		t.Fatal("opAct default to:room echoed back to the acting player (should be room-except-actor)")
	}

	// $N RENDERING (the load-bearing contract): opAct threads c.target as the VICTIM referent, so a
	// `$N` token in the text must render the bound target's name. Without this, swapping the obj/vict
	// arg positions in the handler would pass every routing case above silently. A bystander sees the
	// room line with the victim's name filled in.
	flushAll()
	opAct(seededCtx(z, caster.entity, victim.entity, dispHelpful), &effectOp{text: "$N is struck by the spell!"})
	by := got(bystander)
	if !has(by, "Victim") || !has(by, "is struck by the spell!") {
		t.Fatalf("opAct $N did not render the bound target's name into the room line; bystander saw %v", by)
	}
}

// TestEffectOpMissingArgsErrorNotPanic pins the BOUNDARY guards on the harmful/utility ops: a missing
// target (deal_damage/heal/modify_resource) or a missing required field (heal's resource, apply_affect's
// ref) returns a descriptive error and does NOT panic. runOps logs+skips an op error, so a content
// op-list with a malformed op degrades to a no-op rather than crashing the zone goroutine — this is the
// contract that keeps bad content from taking a shard down.
func TestEffectOpMissingArgsErrorNotPanic(t *testing.T) {
	z, caster := abilityTestZone(t)

	// Missing TARGET → error (not panic) for every target-requiring op.
	noTarget := seededCtx(z, caster.entity, nil, dispHarmful)
	for name, fn := range map[string]func(*effectCtx, *effectOp) error{
		"deal_damage":     opDealDamage,
		"heal":            opHeal,
		"restore":         opRestore,
		"modify_resource": opModifyResource,
	} {
		if err := fn(noTarget, &effectOp{resource: "hp", amount: 5, dmgType: "fire"}); err == nil {
			t.Fatalf("%s with a nil target should error, got nil", name)
		}
	}

	// Missing RESOURCE on heal → error.
	if err := opHeal(seededCtx(z, caster.entity, caster.entity, dispHelpful), &effectOp{amount: 5}); err == nil {
		t.Fatal("heal with no resource should error, got nil")
	}
	// Missing AFFECT ref on apply_affect → error (even with a valid target).
	if err := opApplyAffect(seededCtx(z, caster.entity, caster.entity, dispHelpful), &effectOp{}); err == nil {
		t.Fatal("apply_affect with no affect ref should error, got nil")
	}
}

// TestEffectOpAmountClamps pins the two amount-boundary clamps that are quietly security-relevant:
//   - heal can NEVER lower a pool: a negative heal amount is clamped to 0 (so heal can't be weaponized
//     into a cross-player drain — that path is modify_resource, which is gated). The pool is unchanged.
//   - a resource write FLOORS at 0: a huge negative modify_resource on self drives the pool to exactly 0,
//     never negative (setResourceCurrent clamp). modify_resource also does NOT route through the death
//     funnel, so a vital pool floored this way sits at 0 without a death checkpoint (distinct from
//     deal_damage) — pinning that the clamp, not death, is what happens here.
func TestEffectOpAmountClamps(t *testing.T) {
	z, caster := abilityTestZone(t)

	// Negative heal is a no-op on the pool (clamped to 0 magnitude).
	setResourceCurrent(caster.entity, "hp", 60)
	if err := opHeal(seededCtx(z, caster.entity, caster.entity, dispHelpful), &effectOp{resource: "hp", amount: -50}); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if got := resourceCurrent(caster.entity, "hp"); got != 60 {
		t.Fatalf("a negative heal changed the pool to %d, want 60 unchanged (heal must never drain)", got)
	}

	// A massive negative resource write floors at 0, never negative.
	setResourceCurrent(caster.entity, "hp", 50)
	if err := opModifyResource(seededCtx(z, caster.entity, caster.entity, dispHelpful), &effectOp{resource: "hp", amount: -1000}); err != nil {
		t.Fatalf("modify_resource: %v", err)
	}
	if got := resourceCurrent(caster.entity, "hp"); got != 0 {
		t.Fatalf("modify_resource underflow floored at %d, want exactly 0", got)
	}
}

// TestOpIfResourceThresholdBranch covers opIf's RESOURCE-threshold condition (ifResource/ifResourceMin)
// — the existing opIf test only exercises the has-affect condition. A pool at/above the threshold runs
// `then`; below it runs `els`. The subject is the resolved ctx target. Asserted via the branch's send op
// landing (or not) on the player session.
func TestOpIfResourceThresholdBranch(t *testing.T) {
	z, caster := abilityTestZone(t)

	ifOp := &effectOp{
		kind: "if", ifResource: "hp", ifResourceMin: 50,
		then: []effectOp{{kind: "send", text: "THEN_BRANCH"}},
		els:  []effectOp{{kind: "send", text: "ELS_BRANCH"}},
	}

	// hp at/above the threshold → `then`.
	setResourceCurrent(caster.entity, "hp", 80)
	if err := opIf(seededCtx(z, caster.entity, caster.entity, dispHelpful), ifOp); err != nil {
		t.Fatalf("opIf (then): %v", err)
	}
	if !drainContains(t, caster, "THEN_BRANCH") {
		t.Fatal("opIf with hp>=min did not run the `then` branch")
	}

	// hp below the threshold → `els`.
	setResourceCurrent(caster.entity, "hp", 20)
	if err := opIf(seededCtx(z, caster.entity, caster.entity, dispHelpful), ifOp); err != nil {
		t.Fatalf("opIf (els): %v", err)
	}
	if !drainContains(t, caster, "ELS_BRANCH") {
		t.Fatal("opIf with hp<min did not run the `els` branch")
	}
}
