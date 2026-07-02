package world

import "testing"

// effect_op_grant_test.go — the Phase-11.1 grant ops (modify_attribute_base, set_flag/clear_flag): the
// op behavior + the grant-survives-a-reload guarantee the progression machinery rests on.

// TestOpModifyAttributeBaseRaisesAndAccumulates proves modify_attribute_base adds a signed delta to the
// per-entity base, seeding from the attribute def's default on the first touch and accumulating after.
func TestOpModifyAttributeBaseRaisesAndAccumulates(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	if got := attr(e, "strength"); got != 10 {
		t.Fatalf("strength default = %v, want 10 (test precondition)", got)
	}
	c := seededCtx(z, e, e, dispHelpful)

	// First touch: +4 from the def default (10) -> 14.
	if err := opModifyAttributeBase(c, &effectOp{attr: "strength", amount: 4}); err != nil {
		t.Fatalf("modify_attribute_base: %v", err)
	}
	if got := attr(e, "strength"); got != 14 {
		t.Fatalf("strength after +4 = %v, want 14", got)
	}
	// Accumulates on the override: +1 -> 15.
	if err := opModifyAttributeBase(c, &effectOp{attr: "strength", amount: 1}); err != nil {
		t.Fatalf("modify_attribute_base: %v", err)
	}
	if got := attr(e, "strength"); got != 15 {
		t.Fatalf("strength after +1 = %v, want 15 (delta must accumulate on the override)", got)
	}
	// A negative delta (a revoke / penalty) lowers it.
	if err := opModifyAttributeBase(c, &effectOp{attr: "strength", amount: -5}); err != nil {
		t.Fatalf("modify_attribute_base: %v", err)
	}
	if got := attr(e, "strength"); got != 10 {
		t.Fatalf("strength after -5 = %v, want 10", got)
	}
}

// TestOpSetClearFlag proves set_flag/clear_flag toggle a named open-set flag on the target.
func TestOpSetClearFlag(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	c := seededCtx(z, e, e, dispHelpful)

	if hasFlag(e, "guildmember") {
		t.Fatal("flag set before grant (test precondition)")
	}
	if err := opSetFlag(c, &effectOp{flag: "guildmember"}); err != nil {
		t.Fatalf("set_flag: %v", err)
	}
	if !hasFlag(e, "guildmember") {
		t.Fatal("set_flag did not set the flag")
	}
	if err := opClearFlag(c, &effectOp{flag: "guildmember"}); err != nil {
		t.Fatalf("clear_flag: %v", err)
	}
	if hasFlag(e, "guildmember") {
		t.Fatal("clear_flag did not clear the flag")
	}
}

// TestGrantOpsRepublishCommsOnAccessChange proves a flag grant op that crosses a channel's access
// predicate re-publishes the target's comms config so the gate's hear-set stops (or starts) matching a
// restricted channel WITHOUT waiting for the player's next toggle/handoff/relog — the round-5 security
// follow-up. Before this the affect apply/expire sites republished but the grant ops did not, so a
// guild-leave via clear_flag left the player still hearing guild chat.
func TestGrantOpsRepublishCommsOnAccessChange(t *testing.T) {
	_, z, gate := restrictedHearShard(t) // pack has `secret` (require_flag: insider), default_on
	s := newTestPlayerEntity(z, "Insider")
	setFlag(s.entity, "insider", true) // grants access → hears `secret`
	cfg := drainConfig(t, gate, "Insider")

	c := seededCtx(z, s.entity, s.entity, dispHelpful)

	// Revoke via clear_flag: the republish must DROP `secret` from the hear-set (the eavesdropping fix).
	if err := opClearFlag(c, &effectOp{flag: "insider"}); err != nil {
		t.Fatalf("clear_flag: %v", err)
	}
	p, ok := recvConfig(t, cfg)
	if !ok {
		t.Fatal("clear_flag grant op did not republish comms config — the hear-set stays stale (still hearing a revoked channel)")
	}
	if containsStr(p.HearChannels, "secret") {
		t.Fatalf("hear-set %v still includes `secret` after clear_flag revoked access", p.HearChannels)
	}

	// Re-grant via set_flag: the republish must ADD it back.
	if err := opSetFlag(c, &effectOp{flag: "insider"}); err != nil {
		t.Fatalf("set_flag: %v", err)
	}
	p, ok = recvConfig(t, cfg)
	if !ok {
		t.Fatal("set_flag grant op did not republish comms config")
	}
	if !containsStr(p.HearChannels, "secret") {
		t.Fatalf("hear-set %v missing `secret` after set_flag granted access", p.HearChannels)
	}
}

// TestGrantOpsMissingArgsError proves a malformed grant op returns a descriptive error (not a panic) so
// runOps logs+skips it — the content-lint-is-the-gate discipline.
func TestGrantOpsMissingArgsError(t *testing.T) {
	z, caster := abilityTestZone(t)
	c := seededCtx(z, caster.entity, caster.entity, dispHelpful)
	if err := opModifyAttributeBase(c, &effectOp{amount: 1}); err == nil {
		t.Fatal("modify_attribute_base with no attr should error")
	}
	if err := opSetFlag(c, &effectOp{}); err == nil {
		t.Fatal("set_flag with no flag should error")
	}
	if err := opClearFlag(c, &effectOp{}); err == nil {
		t.Fatal("clear_flag with no flag should error")
	}
}

// TestGrantOpsSurviveReload is the headline 11.1 guarantee: the grants a level-up applies (a raised
// attribute base + a set flag) round-trip through dumpCharacter/loadCharacter — they live in the persisted
// state subtree, so a reload restores them without re-running the op (the foundation the track machinery
// builds on).
func TestGrantOpsSurviveReload(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Aragorn"}
	e := z.newPlayerEntity(src, "Aragorn")
	c := seededCtx(z, e, e, dispHelpful)

	base := attr(e, "strength")
	if err := opModifyAttributeBase(c, &effectOp{attr: "strength", amount: 3}); err != nil {
		t.Fatalf("modify_attribute_base: %v", err)
	}
	if err := opSetFlag(c, &effectOp{flag: "ranger"}); err != nil {
		t.Fatalf("set_flag: %v", err)
	}
	want := base + 3

	snap := dumpCharacter(src)

	// Load into a fresh entity (a new login) and assert the grants came back.
	dst := &session{character: "Aragorn"}
	z.newPlayerEntity(dst, "Aragorn")
	loadCharacter(z, dst, snap)
	de := dst.entity

	if got := attr(de, "strength"); got != want {
		t.Fatalf("reloaded strength = %v, want %v (a granted base must survive a reload)", got, want)
	}
	if !hasFlag(de, "ranger") {
		t.Fatal("a granted flag did not survive the reload")
	}
}
