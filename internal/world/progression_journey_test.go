package world

import (
	"math/rand"
	"testing"
)

// progression_journey_test.go is the Phase-11 capstone + the §11.5 four-modes proof (docs/PHASE11-PLAN.md):
// ONE character built from a class+race bundle advances through ALL FOUR modes — XP-auto, train-at-trainer,
// point-buy, and use-based — entirely as content on the one track/grant machinery, and the whole build
// SURVIVES A RESTART. It is the done-when of the phase: "a character is created from a class+race bundle,
// gains XP on kills (auto-leveling one track), trains a skill through use on another, and the build
// survives a restart — all content."

// progressionJourneyZone is a demo zone (it ships the fighter/elf bundles + the hero_advancement track)
// plus the extra content the four modes need: gold/talent_points/valor resources, a dodge skill track, and
// the trainer/point-buy/practice abilities. The valor resource (which every character here has) carries
// the OnKill→XP and OnSkillUse→dodge handlers — the "which event feeds the track" glue.
func progressionJourneyZone(t *testing.T) *Zone {
	t.Helper()
	z := newDemoZone("midgaard", newProtoCache())

	z.defs.attr.register("max_gold", &attributeDef{ref: "max_gold", base: litNode{v: 100000}})
	z.defs.res.register("gold", &resourceDef{ref: "gold", maxAttr: "max_gold"})
	z.defs.attr.register("max_points", &attributeDef{ref: "max_points", base: litNode{v: 100}})
	z.defs.res.register("talent_points", &resourceDef{ref: "talent_points", maxAttr: "max_points"})
	z.defs.attr.register("dodge", &attributeDef{ref: "dodge", base: litNode{v: 0}})
	z.defs.track.register("dodge_skill", &trackDef{
		ref: "dodge_skill", progressAttr: "dodge",
		thresholds: []float64{2},
		steps:      [][]effectOp{{{kind: "set_flag", flag: "nimble"}}},
	})

	// valor: a resource every character has (max 1), carrying the progression event handlers.
	z.defs.attr.register("max_valor", &attributeDef{ref: "max_valor", base: litNode{v: 1}})
	z.defs.res.register("valor", &resourceDef{ref: "valor", maxAttr: "max_valor", onEvent: map[eventKind][]effectOp{
		evOnKill:     {{kind: "advance_track", track: "hero_advancement", amount: 120}}, // XP-auto
		evOnSkillUse: {{kind: "advance_track", track: "dodge_skill", amount: 1}},        // use-based
	}})

	// train-at-trainer: an ability that spends gold and raises a stat directly (no auto-threshold).
	z.defs.ability.register("train_str", &abilityDef{
		ref: "train_str", name: "train", invocation: "command", words: []string{"train"}, mode: tmSelf,
		costs: []resourceCost{{resource: "gold", amount: 100}},
		ops:   []effectOp{{kind: "modify_attribute_base", attr: "strength", amount: 1}},
	})
	// point-buy: spend a talent point to raise a stat.
	z.defs.ability.register("spend_point", &abilityDef{
		ref: "spend_point", name: "spend", invocation: "command", words: []string{"spend"}, mode: tmSelf,
		costs: []resourceCost{{resource: "talent_points", amount: 1}},
		ops:   []effectOp{{kind: "modify_attribute_base", attr: "intellect", amount: 1}},
	})
	// use-based: a SKILL ability — using it fires OnSkillUse (valor advances the dodge track).
	z.defs.ability.register("practice_dodge", &abilityDef{
		ref: "practice_dodge", name: "practice", invocation: "command", words: []string{"practice"}, mode: tmSelf,
		skill: "dodge",
	})
	return z
}

func TestProgressionJourneyAllModesSurvivesRestart(t *testing.T) {
	z := progressionJourneyZone(t)
	src := &session{character: "Eowyn"}
	e := z.newPlayerEntity(src, "Eowyn")
	c := seededCtx(z, e, e, dispHelpful)
	rng := rand.New(rand.NewSource(1))

	baseStr := attr(e, "strength")
	baseInt := attr(e, "intellect")

	// --- Chargen: build the character from a class + race bundle. ---
	if err := opApplyBundle(c, &effectOp{bundle: "fighter"}); err != nil {
		t.Fatalf("apply fighter: %v", err)
	}
	if err := opApplyBundle(c, &effectOp{bundle: "elf"}); err != nil {
		t.Fatalf("apply elf: %v", err)
	}
	if attr(e, "strength") != baseStr+3 || attr(e, "intellect") != baseInt+2 {
		t.Fatalf("after bundles: str=%v int=%v, want +3/+2", attr(e, "strength"), attr(e, "intellect"))
	}
	if !hasFlag(e, "martial") || !hasFlag(e, "darkvision") || !hasTrack(e, "hero_advancement") {
		t.Fatal("bundles did not grant the expected flags + track")
	}

	// --- Mode 1: XP-auto. A kill fires OnKill (about the killer) -> valor advances hero_advancement. ---
	victim := makeMobTarget(z, e, "orc")
	z.fireEvent(nil, evOnKill, e, victim, 1)
	if trackStep(e, "hero_advancement") != 1 || attr(e, "level") != 2 {
		t.Fatalf("XP-auto: hero step=%d level=%v, want 1/2", trackStep(e, "hero_advancement"), attr(e, "level"))
	}

	// --- Mode 2: train-at-trainer. Spend gold at a trainer to raise strength. ---
	setResourceCurrent(e, "gold", 500)
	strBeforeTrain := attr(e, "strength")
	z.castAbility(src, z.defs.ability.get("train_str"), "", rng)
	if attr(e, "strength") != strBeforeTrain+1 {
		t.Fatalf("train: strength = %v, want %v", attr(e, "strength"), strBeforeTrain+1)
	}
	if resourceCurrent(e, "gold") != 400 {
		t.Fatalf("train: gold = %d, want 400 (100 spent)", resourceCurrent(e, "gold"))
	}

	// --- Mode 3: point-buy. A talent point spent raises intellect. ---
	setResourceCurrent(e, "talent_points", 2)
	intBeforeSpend := attr(e, "intellect")
	z.castAbility(src, z.defs.ability.get("spend_point"), "", rng)
	if attr(e, "intellect") != intBeforeSpend+1 {
		t.Fatalf("point-buy: intellect = %v, want %v", attr(e, "intellect"), intBeforeSpend+1)
	}
	if resourceCurrent(e, "talent_points") != 1 {
		t.Fatalf("point-buy: points = %d, want 1 (1 spent)", resourceCurrent(e, "talent_points"))
	}

	// --- Mode 4: use-based. Practicing the dodge skill twice fires OnSkillUse -> dodge_skill advances. ---
	z.castAbility(src, z.defs.ability.get("practice_dodge"), "", rng)
	z.castAbility(src, z.defs.ability.get("practice_dodge"), "", rng)
	if trackStep(e, "dodge_skill") != 1 {
		t.Fatalf("use-based: dodge_skill step = %d, want 1 (two uses crossed threshold 2)", trackStep(e, "dodge_skill"))
	}
	if !hasFlag(e, "nimble") {
		t.Fatal("use-based: the dodge_skill step grant (nimble flag) did not run")
	}

	// Snapshot the whole built character.
	wantStr := attr(e, "strength")
	wantInt := attr(e, "intellect")
	wantLevel := attr(e, "level")
	snap := dumpCharacter(src)

	// --- The build SURVIVES A RESTART: reload into a fresh entity and assert every gain persisted. ---
	dst := &session{character: "Eowyn"}
	z.newPlayerEntity(dst, "Eowyn")
	loadCharacter(z, dst, snap)
	de := dst.entity

	if attr(de, "strength") != wantStr || attr(de, "intellect") != wantInt || attr(de, "level") != wantLevel {
		t.Fatalf("after restart: str=%v int=%v level=%v, want %v/%v/%v",
			attr(de, "strength"), attr(de, "intellect"), attr(de, "level"), wantStr, wantInt, wantLevel)
	}
	if trackStep(de, "hero_advancement") != 1 || trackStep(de, "dodge_skill") != 1 {
		t.Fatalf("after restart: hero step=%d dodge step=%d, want 1/1",
			trackStep(de, "hero_advancement"), trackStep(de, "dodge_skill"))
	}
	for _, f := range []string{"martial", "darkvision", "nimble"} {
		if !hasFlag(de, f) {
			t.Fatalf("after restart: flag %q lost", f)
		}
	}
}
