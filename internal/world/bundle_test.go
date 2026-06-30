package world

import "testing"

// bundle_test.go — Phase 11.4: ability ownership (grant_ability + the requires_grant gate) and the
// template/bundle machinery (apply_bundle composing a class/race's grants), plus the granted-ability
// survives-reload guarantee.

// TestApplyBundleRunsGrants proves apply_bundle composes a bundle's grants: a "knight" class bundle raises
// a stat, grants an ability, and sets a flag — all in one apply.
func TestApplyBundleRunsGrants(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity
	z.defs.ability.register("smite", &abilityDef{ref: "smite", name: "smite", invocation: "command", words: []string{"smite"}, requiresGrant: true})
	z.defs.bundle.register("knight", &bundleDef{
		ref: "knight", kind: "class",
		grants: []effectOp{
			{kind: "modify_attribute_base", attr: "strength", amount: 5},
			{kind: "grant_ability", ability: "smite"},
			{kind: "set_flag", flag: "holy"},
		},
	})
	str0 := attr(e, "strength")
	c := seededCtx(z, e, e, dispHelpful)

	if err := opApplyBundle(c, &effectOp{bundle: "knight"}); err != nil {
		t.Fatalf("apply_bundle: %v", err)
	}
	if attr(e, "strength") != str0+5 {
		t.Fatalf("after bundle: strength = %v, want %v", attr(e, "strength"), str0+5)
	}
	if !hasGrantedAbility(e, "smite") {
		t.Fatal("apply_bundle did not grant the ability")
	}
	if !hasFlag(e, "holy") {
		t.Fatal("apply_bundle did not set the flag")
	}
}

// TestAbilityOwnershipGate proves an ownership-gated ability (requires_grant) is refused at the step-3
// gate until the actor is granted it — and an un-gated ability is unaffected (the gate is opt-in).
func TestAbilityOwnershipGate(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Galahad"}
	z.newPlayerEntity(src, "Galahad")
	gated := &abilityDef{ref: "lay_on_hands", name: "lay on hands", invocation: "command", words: []string{"layhands"}, requiresGrant: true}
	z.defs.ability.register("lay_on_hands", gated)

	// Before the grant, the gate refuses it.
	if z.checkRequires(src, gated) {
		t.Fatal("an ungranted ownership-gated ability passed the requires gate")
	}
	// An UN-gated ability (no requires_grant) is allowed without a grant.
	open := &abilityDef{ref: "shout", name: "shout", invocation: "command", words: []string{"shout"}}
	if !z.checkRequires(src, open) {
		t.Fatal("an un-gated ability was wrongly refused (the gate must be opt-in)")
	}
	// After the grant, the gated ability passes.
	grantAbility(src.entity, "lay_on_hands")
	if !z.checkRequires(src, gated) {
		t.Fatal("a granted ownership-gated ability was still refused")
	}
}

// TestGrantedAbilitySurvivesReload proves the granted-ability set round-trips through dumpCharacter/
// loadCharacter, so an ownership-gated ability stays usable across a relogin.
func TestGrantedAbilitySurvivesReload(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Percival"}
	e := z.newPlayerEntity(src, "Percival")
	c := seededCtx(z, e, e, dispHelpful)

	if err := opGrantAbility(c, &effectOp{ability: "lay_on_hands"}); err != nil {
		t.Fatalf("grant_ability: %v", err)
	}
	snap := dumpCharacter(src)
	found := false
	for _, ref := range snap.State.Abilities {
		if ref == "lay_on_hands" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dumped abilities = %v, want lay_on_hands", snap.State.Abilities)
	}

	dst := &session{character: "Percival"}
	z.newPlayerEntity(dst, "Percival")
	loadCharacter(z, dst, snap)
	if !hasGrantedAbility(dst.entity, "lay_on_hands") {
		t.Fatal("a granted ability did not survive the reload")
	}
}

// TestApplyBundleDemoFighter proves the demo "fighter" class bundle applies end to end through the real
// content path (buildBundleDef from YAML): it raises strength, sets the martial flag, and grants the
// hero_advancement track.
func TestApplyBundleDemoFighter(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Boromir"}
	e := z.newPlayerEntity(src, "Boromir")
	str0 := attr(e, "strength")
	c := seededCtx(z, e, e, dispHelpful)

	if err := opApplyBundle(c, &effectOp{bundle: "fighter"}); err != nil {
		t.Fatalf("apply_bundle fighter: %v", err)
	}
	if attr(e, "strength") != str0+3 {
		t.Fatalf("fighter: strength = %v, want %v", attr(e, "strength"), str0+3)
	}
	if !hasFlag(e, "martial") {
		t.Fatal("fighter bundle did not set the martial flag")
	}
	if !hasTrack(e, "hero_advancement") {
		t.Fatal("fighter bundle did not grant the hero_advancement track")
	}
}

func TestBundleAndGrantErrors(t *testing.T) {
	z, caster := abilityTestZone(t)
	c := seededCtx(z, caster.entity, caster.entity, dispHelpful)
	if err := opApplyBundle(c, &effectOp{bundle: "nonexistent"}); err == nil {
		t.Fatal("apply_bundle on an unknown bundle should error")
	}
	if err := opApplyBundle(c, &effectOp{}); err == nil {
		t.Fatal("apply_bundle with no bundle should error")
	}
	if err := opGrantAbility(c, &effectOp{}); err == nil {
		t.Fatal("grant_ability with no ability should error")
	}
}
