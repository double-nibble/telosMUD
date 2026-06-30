package world

import "testing"

// track_test.go — the Phase-11.2 advancement-track machinery: grant_track + advance_track, threshold
// crossings applying step grants exactly once, multi-step jumps, and the survives-a-reload guarantee.

// trackTestZone builds a bare zone with an xp/level/strength attribute set and a registered "hero" track
// (thresholds 100/250/500, each step bumping level + strength) — the unit fixture for the track ops.
func trackTestZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	z, caster := abilityTestZone(t)
	// abilityTestZone already registers strength (base 10) + max_hp/max_mana. Add xp + level.
	z.defs.attr.register("xp", &attributeDef{ref: "xp", base: litNode{v: 0}})
	z.defs.attr.register("level", &attributeDef{ref: "level", base: litNode{v: 1}})
	// Build the runtime trackDef directly (the content-DTO parse path, buildTrackDef, is covered via the
	// demo pack in TestTrackSurvivesReload). Each step bumps level + strength.
	step := []effectOp{
		{kind: "modify_attribute_base", attr: "level", amount: 1},
		{kind: "modify_attribute_base", attr: "strength", amount: 1},
	}
	z.defs.track.register("hero", &trackDef{
		ref: "hero", progressAttr: "xp", levelAttr: "level",
		thresholds: []float64{100, 250, 500},
		steps:      [][]effectOp{step, step, step},
	})
	return z, caster
}

func TestGrantTrackAddsAtStepZero(t *testing.T) {
	z, caster := trackTestZone(t)
	e := caster.entity
	c := seededCtx(z, e, e, dispHelpful)

	if hasTrack(e, "hero") {
		t.Fatal("track present before grant (precondition)")
	}
	if err := opGrantTrack(c, &effectOp{track: "hero"}); err != nil {
		t.Fatalf("grant_track: %v", err)
	}
	if !hasTrack(e, "hero") || trackStep(e, "hero") != 0 {
		t.Fatalf("after grant_track: hasTrack=%v step=%d, want true/0", hasTrack(e, "hero"), trackStep(e, "hero"))
	}
	// Re-granting must NOT reset a track that has progressed (multiclass / reload-safe re-apply).
	setTrackStep(e, "hero", 2)
	if err := opGrantTrack(c, &effectOp{track: "hero"}); err != nil {
		t.Fatalf("grant_track (re): %v", err)
	}
	if trackStep(e, "hero") != 2 {
		t.Fatalf("re-grant reset the step to %d, want it kept at 2", trackStep(e, "hero"))
	}
}

func TestAdvanceTrackCrossesThresholdAndAppliesGrants(t *testing.T) {
	z, caster := trackTestZone(t)
	e := caster.entity
	c := seededCtx(z, e, e, dispHelpful)

	if got := attr(e, "level"); got != 1 {
		t.Fatalf("level default = %v, want 1", got)
	}
	str0 := attr(e, "strength")

	// 50 xp: below the first threshold (100) — no step, no grant.
	if err := opAdvanceTrack(c, &effectOp{track: "hero", amount: 50}); err != nil {
		t.Fatalf("advance_track: %v", err)
	}
	if trackStep(e, "hero") != 0 || attr(e, "level") != 1 {
		t.Fatalf("below threshold: step=%d level=%v, want 0/1", trackStep(e, "hero"), attr(e, "level"))
	}
	if attr(e, "xp") != 50 {
		t.Fatalf("xp = %v, want 50 (progress accumulated)", attr(e, "xp"))
	}

	// +60 xp -> 110: crosses the first threshold (100) -> step 1 -> level+1, strength+1.
	if err := opAdvanceTrack(c, &effectOp{track: "hero", amount: 60}); err != nil {
		t.Fatalf("advance_track: %v", err)
	}
	if trackStep(e, "hero") != 1 {
		t.Fatalf("after crossing 100: step = %d, want 1", trackStep(e, "hero"))
	}
	if attr(e, "level") != 2 {
		t.Fatalf("after step 1: level = %v, want 2 (the step grant ran)", attr(e, "level"))
	}
	if attr(e, "strength") != str0+1 {
		t.Fatalf("after step 1: strength = %v, want %v", attr(e, "strength"), str0+1)
	}
}

// TestAdvanceTrackMultiStepJump proves a single large award crosses SEVERAL thresholds at once, applying
// every crossed step's grants in order (a Diku "you gained 3 levels!" award).
func TestAdvanceTrackMultiStepJump(t *testing.T) {
	z, caster := trackTestZone(t)
	e := caster.entity
	c := seededCtx(z, e, e, dispHelpful)

	// 600 xp at once crosses 100, 250, AND 500 -> step 3, level 1->4 (one bump per step).
	if err := opAdvanceTrack(c, &effectOp{track: "hero", amount: 600}); err != nil {
		t.Fatalf("advance_track: %v", err)
	}
	if trackStep(e, "hero") != 3 {
		t.Fatalf("after 600 xp: step = %d, want 3 (crossed all 3 thresholds)", trackStep(e, "hero"))
	}
	if attr(e, "level") != 4 {
		t.Fatalf("after 3 steps: level = %v, want 4", attr(e, "level"))
	}
	// A further small award past the last threshold adds no step (capped at len(thresholds)).
	if err := opAdvanceTrack(c, &effectOp{track: "hero", amount: 1000}); err != nil {
		t.Fatalf("advance_track: %v", err)
	}
	if trackStep(e, "hero") != 3 {
		t.Fatalf("past the last threshold: step = %d, want it capped at 3", trackStep(e, "hero"))
	}
}

func TestAdvanceTrackUnknownErrors(t *testing.T) {
	z, caster := trackTestZone(t)
	c := seededCtx(z, caster.entity, caster.entity, dispHelpful)
	if err := opAdvanceTrack(c, &effectOp{track: "nonexistent", amount: 1}); err == nil {
		t.Fatal("advance_track on an unknown track should error")
	}
	if err := opGrantTrack(c, &effectOp{}); err == nil {
		t.Fatal("grant_track with no track should error")
	}
}

// TestTrackSurvivesReload is the headline 11.2 guarantee: a leveled-up character's track STEP and the
// grants it applied (level/strength bases) round-trip through dumpCharacter/loadCharacter — the step is
// the high-water, so the reload restores it without re-running the grants (exactly once across a reload).
func TestTrackSurvivesReload(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache()) // demo pack has the hero_advancement track + xp attr
	src := &session{character: "Conan"}
	e := z.newPlayerEntity(src, "Conan")
	c := seededCtx(z, e, e, dispHelpful)

	// Grant the demo track and award enough xp to reach step 2 (thresholds 100/250/500).
	if err := opGrantTrack(c, &effectOp{track: "hero_advancement"}); err != nil {
		t.Fatalf("grant_track: %v", err)
	}
	if err := opAdvanceTrack(c, &effectOp{track: "hero_advancement", amount: 300}); err != nil {
		t.Fatalf("advance_track: %v", err)
	}
	if trackStep(e, "hero_advancement") != 2 {
		t.Fatalf("step after 300 xp = %d, want 2", trackStep(e, "hero_advancement"))
	}
	wantLevel := attr(e, "level")
	wantStr := attr(e, "strength")

	snap := dumpCharacter(src)
	if snap.State.Tracks["hero_advancement"] != 2 {
		t.Fatalf("dumped track step = %d, want 2", snap.State.Tracks["hero_advancement"])
	}

	// Reload into a fresh entity (a new login).
	dst := &session{character: "Conan"}
	z.newPlayerEntity(dst, "Conan")
	loadCharacter(z, dst, snap)
	de := dst.entity

	if trackStep(de, "hero_advancement") != 2 {
		t.Fatalf("reloaded track step = %d, want 2", trackStep(de, "hero_advancement"))
	}
	if attr(de, "level") != wantLevel {
		t.Fatalf("reloaded level = %v, want %v (the step's grant must survive, not re-run)", attr(de, "level"), wantLevel)
	}
	if attr(de, "strength") != wantStr {
		t.Fatalf("reloaded strength = %v, want %v", attr(de, "strength"), wantStr)
	}
}
