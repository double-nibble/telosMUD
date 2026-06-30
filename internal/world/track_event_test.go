package world

import (
	"math/rand"
	"testing"
)

// track_event_test.go — the Phase-11.3 progression events: OnTrackStep/OnLevel fired by the track
// machinery, OnSkillUse fired by a skill ability, and the done-when proof that XP-auto (OnKill→advance)
// and use-based (OnSkillUse→advance) are BOTH pure content on top of 11.2 — no engine code per mode.

// trackEventZone is eventTestZone (a rage resource carrying on_event handlers) plus xp/level attributes
// and the registered "hero" XP→level track (thresholds 100/250, each step bumping level + strength).
func trackEventZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	z, caster := eventTestZone(t)
	z.defs.attr.register("xp", &attributeDef{ref: "xp", base: litNode{v: 0}})
	z.defs.attr.register("level", &attributeDef{ref: "level", base: litNode{v: 1}})
	step := []effectOp{
		{kind: "modify_attribute_base", attr: "level", amount: 1},
		{kind: "modify_attribute_base", attr: "strength", amount: 1},
	}
	z.defs.track.register("hero", &trackDef{
		ref: "hero", progressAttr: "xp", levelAttr: "level",
		thresholds: []float64{100, 250},
		steps:      [][]effectOp{step, step},
	})
	return z, caster
}

// TestAdvanceTrackFiresOnLevelAndStep proves advance_track fires OnTrackStep (every track) and OnLevel (a
// level track) about the advancing entity, so content reacts to a level-up (the "ding!" hook). The rage
// resource (which the caster has) subscribes both and sets a flag.
func TestAdvanceTrackFiresOnLevelAndStep(t *testing.T) {
	z, caster := trackEventZone(t)
	e := caster.entity
	registerRage(z, map[eventKind][]effectOp{
		evOnLevel:     {{kind: "set_flag", flag: "leveled"}},
		evOnTrackStep: {{kind: "set_flag", flag: "stepped"}},
	})
	c := seededCtx(z, e, e, dispHelpful)

	// Advance past the first threshold (100) -> step 1 -> OnTrackStep + OnLevel fire.
	if err := opAdvanceTrack(c, &effectOp{track: "hero", amount: 150}); err != nil {
		t.Fatalf("advance_track: %v", err)
	}
	if !hasFlag(e, "stepped") {
		t.Fatal("OnTrackStep did not fire on a step crossing")
	}
	if !hasFlag(e, "leveled") {
		t.Fatal("OnLevel did not fire on a level-track step crossing")
	}
}

// TestOnLevelSuppressedForLevellessTrack proves a track with NO level_attr fires OnTrackStep but NOT
// OnLevel — a use-based skill track has no "level" (the constraint: the engine grows no level concept).
func TestOnLevelSuppressedForLevellessTrack(t *testing.T) {
	z, caster := trackEventZone(t)
	e := caster.entity
	// A level-less skill track: progress_attr only, no level_attr.
	z.defs.attr.register("mining", &attributeDef{ref: "mining", base: litNode{v: 0}})
	z.defs.track.register("mining_skill", &trackDef{
		ref: "mining_skill", progressAttr: "mining",
		thresholds: []float64{10},
		steps:      [][]effectOp{{{kind: "set_flag", flag: "mined"}}},
	})
	registerRage(z, map[eventKind][]effectOp{
		evOnLevel:     {{kind: "set_flag", flag: "leveled"}},
		evOnTrackStep: {{kind: "set_flag", flag: "stepped"}},
	})
	c := seededCtx(z, e, e, dispHelpful)

	if err := opAdvanceTrack(c, &effectOp{track: "mining_skill", amount: 15}); err != nil {
		t.Fatalf("advance_track: %v", err)
	}
	if !hasFlag(e, "stepped") {
		t.Fatal("OnTrackStep should fire for a level-less track")
	}
	if hasFlag(e, "leveled") {
		t.Fatal("OnLevel must NOT fire for a level-less (use-based) track — the engine grows no level concept")
	}
}

// TestXPAutoOnKillAdvancesTrack is the done-when (XP-auto, Diku): a content resource the KILLER has
// subscribes OnKill and advances a track — killing auto-levels, entirely as content. OnKill fires about
// the killer (death.go), so the killer's resource handler is where the XP grant lives.
func TestXPAutoOnKillAdvancesTrack(t *testing.T) {
	z, caster := trackEventZone(t)
	killer := caster.entity
	registerRage(z, map[eventKind][]effectOp{
		evOnKill: {{kind: "advance_track", track: "hero", amount: 120}},
	})
	victim := makeMobTarget(z, killer, "goblin")

	// Fire OnKill about the killer (the death.go contract) -> the rage OnKill handler advances `hero`.
	z.fireEvent(nil, evOnKill, killer, victim, 1)

	if trackStep(killer, "hero") != 1 {
		t.Fatalf("after a kill: hero step = %d, want 1 (120 xp crossed the 100 threshold)", trackStep(killer, "hero"))
	}
	if attr(killer, "level") != 2 {
		t.Fatalf("after auto-level: level = %v, want 2", attr(killer, "level"))
	}
}

// TestUseBasedSkillAdvancesViaOnSkillUse is the done-when (use-based, LP/Discworld): using a SKILL ability
// fires OnSkillUse, and a content handler advances the skill's track — advance-through-use, level-less,
// pure content. Casting the skill ability twice crosses the skill track's threshold.
func TestUseBasedSkillAdvancesViaOnSkillUse(t *testing.T) {
	z, caster := trackEventZone(t)
	e := caster.entity
	z.defs.attr.register("woodcutting", &attributeDef{ref: "woodcutting", base: litNode{v: 0}})
	z.defs.track.register("woodcutting_skill", &trackDef{
		ref: "woodcutting_skill", progressAttr: "woodcutting",
		thresholds: []float64{2},
		steps:      [][]effectOp{{{kind: "set_flag", flag: "journeyman"}}},
	})
	// A skill ability: using it fires OnSkillUse. The rage handler advances the woodcutting track by 1.
	z.defs.ability.register("chop", &abilityDef{
		ref: "chop", name: "chop", invocation: "command", words: []string{"chop"},
		mode: tmSelf, skill: "woodcutting",
	})
	registerRage(z, map[eventKind][]effectOp{
		evOnSkillUse: {{kind: "advance_track", track: "woodcutting_skill", amount: 1}},
	})
	def := z.defs.ability.get("chop")

	z.castAbility(caster, def, "", rand.New(rand.NewSource(1)))
	if attr(e, "woodcutting") != 1 {
		t.Fatalf("after one use: woodcutting = %v, want 1 (OnSkillUse advanced the track)", attr(e, "woodcutting"))
	}
	z.castAbility(caster, def, "", rand.New(rand.NewSource(2)))
	if trackStep(e, "woodcutting_skill") != 1 {
		t.Fatalf("after two uses: woodcutting_skill step = %d, want 1 (crossed threshold 2)", trackStep(e, "woodcutting_skill"))
	}
	if !hasFlag(e, "journeyman") {
		t.Fatal("the skill track's step grant (set journeyman flag) did not run")
	}
}
