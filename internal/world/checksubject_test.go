package world

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// checksubject_test.go covers the check SUBJECT selector (the #511-review follow-up): a check may name
// WHO rolls it. Default = the ctx actor (a skill use, a to-hit). `subject: target` = the ctx target
// rolls — the SAVING-THROW idiom, where the saver must own the roll's default scope, its narration, and
// its OnCheck event, so "on a successful save, the SAVER gains X" is authorable. Before this, all three
// were hardwired to the actor (the caster), making a save's reaction land on the wrong entity.

// subjectZone registers a `dex_save` attribute, a `nimble` affect that grants +10 to it (so actor and
// target can carry different values), and a `checkwatch` affect whose OnCheck handler bumps a `checked`
// counter on its bearer (so we can observe which entity the OnCheck event fired ON).
func subjectZone(t *testing.T) (*Zone, *session, *session) {
	t.Helper()
	z, caster := abilityTestZone(t)
	z.defs.attr.register("dex_save", &attributeDef{ref: "dex_save"})
	z.defs.attr.register("checked", &attributeDef{ref: "checked"})
	z.defs.affect.register("nimble", &affectDef{
		ref: "nimble", duration: 100,
		modifiers: []affectModifier{{attr: "dex_save", add: true, value: 10}},
	})
	z.defs.affect.register("checkwatch", &affectDef{
		ref: "checkwatch", duration: 100,
		onEvent: map[eventKind][]effectOp{
			evOnCheck: {{kind: "modify_attribute_base", attr: "checked", amount: 1, tgt: "self"}},
		},
	})
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	return z, caster, victim
}

// bareSaveSpec is a Dex save: 1d1 (deterministic total 1) + a BARE `dex_save` ref (defaults to the
// roller), classified into high/low bands so the roller's own bonus decides the outcome.
func bareSaveSpec(subject checkSubject) *checkSpec {
	return &checkSpec{
		dice:    mustDiceOrPanic("1d1"),
		bonus:   attrNode{ref: "dex_save"}, // BARE ref: resolves against the ROLLER
		subject: subject,
		bands: []checkBand{
			{min: bn(6), label: "high"}, // total >= 6 only if the roller's dex_save is high
			{label: "low"},
		},
	}
}

func mustDiceOrPanic(s string) diceSpec {
	d, err := parseDiceSpec(s)
	if err != nil {
		panic(err)
	}
	return d
}

// TestSubjectTargetRollerScopeIsTheTarget pins that with `subject: target` a BARE ref reads the TARGET's
// attribute, not the actor's — the roller is the default scope.
func TestSubjectTargetRollerScopeIsTheTarget(t *testing.T) {
	z, caster, victim := subjectZone(t)
	applyAffect(victim.entity, "nimble", attachOpts{}, nil) // ONLY the target has +10 dex_save
	c := checkCtx(z, caster.entity, caster.entity, victim.entity)

	// subject: target — bare dex_save reads the TARGET (10), so total 1+10 = 11 -> "high".
	require.Equal(t, "high", resolveCheck(c, bareSaveSpec(subjTarget)).bandLabel,
		"subject: target must roll with the TARGET's bonus")
	res := resolveCheck(c, bareSaveSpec(subjTarget))
	require.Equal(t, 10.0, res.bonus, "the target's dex_save (+10) is the roller bonus")

	// Default (actor) — the actor has no nimble, so dex_save 0, total 1 -> "low".
	require.Equal(t, "low", resolveCheck(c, bareSaveSpec(subjActor)).bandLabel,
		"the default subject rolls with the ACTOR's bonus (unchanged behaviour)")
}

// TestSubjectTargetNarratesToSaver pins that a visible `subject: target` check narrates to the SAVER
// (the target's own stream), not the actor (the caster). This is the narration half of the fix.
func TestSubjectTargetNarratesToSaver(t *testing.T) {
	z, caster, victim := subjectZone(t)
	c := checkCtx(z, caster.entity, caster.entity, victim.entity)

	spec := bareSaveSpec(subjTarget)
	spec.visibility = visShow
	spec.label = "Dex Save"
	resolveCheck(c, spec)

	require.Contains(t, drainAllText(victim.out), "Dex Save", "the SAVER (target) is narrated to")
	require.NotContains(t, drainAllText(caster.out), "Dex Save", "the caster (actor) is NOT narrated the save")
}

// TestSubjectActorNarratesToActor is the default-path mirror: an ordinary (actor-subject) check narrates
// to the actor.
func TestSubjectActorNarratesToActor(t *testing.T) {
	z, caster, victim := subjectZone(t)
	c := checkCtx(z, caster.entity, caster.entity, victim.entity)

	spec := bareSaveSpec(subjActor)
	spec.visibility = visShow
	spec.label = "Skill"
	resolveCheck(c, spec)

	require.Contains(t, drainAllText(caster.out), "Skill", "the default subject narrates to the actor")
	require.NotContains(t, drainAllText(victim.out), "Skill", "the target is not narrated an actor-subject check")
}

// TestSubjectTargetOnCheckFiresOnSaver pins the OnCheck event flip: with `subject: target` the OnCheck
// event fires with the SAVER as subject, so a handler subscribed on the saver runs. This is what makes
// "on a successful save, the saver gains rage" authorable.
func TestSubjectTargetOnCheckFiresOnSaver(t *testing.T) {
	z, caster, victim := subjectZone(t)
	applyAffect(caster.entity, "checkwatch", attachOpts{}, nil)
	applyAffect(victim.entity, "checkwatch", attachOpts{}, nil)
	c := checkCtx(z, caster.entity, caster.entity, victim.entity)

	// subject: target — OnCheck fires on the TARGET; only the target's watcher bumps.
	resolveCheck(c, bareSaveSpec(subjTarget))
	require.Equal(t, 1.0, attr(victim.entity, "checked"), "OnCheck fires on the SAVER (target)")
	require.Equal(t, 0.0, attr(caster.entity, "checked"), "OnCheck does NOT fire on the caster for a save")
}

// TestSubjectActorOnCheckFiresOnActor is the default mirror: an actor-subject check fires OnCheck on the
// actor.
func TestSubjectActorOnCheckFiresOnActor(t *testing.T) {
	z, caster, victim := subjectZone(t)
	applyAffect(caster.entity, "checkwatch", attachOpts{}, nil)
	applyAffect(victim.entity, "checkwatch", attachOpts{}, nil)
	c := checkCtx(z, caster.entity, caster.entity, victim.entity)

	resolveCheck(c, bareSaveSpec(subjActor))
	require.Equal(t, 1.0, attr(caster.entity, "checked"), "the default subject fires OnCheck on the actor")
	require.Equal(t, 0.0, attr(victim.entity, "checked"), "OnCheck does not fire on the target for an actor check")
}

// TestSubjectExplicitRefsBindFixed pins that EXPLICIT $actor/$target refs still bind the fixed ctx
// entities regardless of subject — only the BARE-ref default and the narration/event identity follow the
// roller. A save that explicitly reads `$actor.dex_save` still reads the caster.
func TestSubjectExplicitRefsBindFixed(t *testing.T) {
	z, caster, victim := subjectZone(t)
	applyAffect(caster.entity, "nimble", attachOpts{}, nil) // ONLY the actor (caster) has +10
	c := checkCtx(z, caster.entity, caster.entity, victim.entity)

	// subject: target, but the bonus EXPLICITLY reads $actor — so it reads the caster (10), not the roller.
	spec := &checkSpec{dice: mustDiceOrPanic("1d1"), bonus: attrNode{ref: "$actor.dex_save"}, subject: subjTarget}
	require.Equal(t, 10.0, resolveCheck(c, spec).bonus,
		"$actor stays bound to the ctx actor even under subject: target")
}

// TestSubjectTargetNilTargetDoesNotPanic pins the fail-safe: a `subject: target` check with NO bound
// ctx target (a self-cast, an untargeted ability, an affect tick) must resolve without panicking — the
// roller is nil, so emitCheck simply does not narrate. Before the nil guard this deref-panicked the
// resolution mid-op-list.
func TestSubjectTargetNilTargetDoesNotPanic(t *testing.T) {
	z, caster, _ := subjectZone(t)
	c := checkCtx(z, caster.entity, caster.entity, nil) // target is nil

	spec := bareSaveSpec(subjTarget)
	spec.visibility = visShow // force the narration path — that is where the deref lived
	require.NotPanics(t, func() {
		res := resolveCheck(c, spec)
		require.NotNil(t, res.band, "the roll still resolves to a band")
	}, "a subject: target check with a nil target must fail safe, not panic")
}

// TestParseSubjectTargetWiredFromContent pins the parser: `subject: target` -> subjTarget, absent ->
// subjActor.
func TestParseSubjectTargetWiredFromContent(t *testing.T) {
	spec, err := parseCheckSpec(map[string]any{
		"dice": "1d20", "subject": "target", "vs": 15.0,
		"bands": []any{map[string]any{"label": "ok"}},
	})
	require.NoError(t, err)
	require.Equal(t, subjTarget, spec.subject)

	def, err := parseCheckSpec(map[string]any{
		"dice": "1d20", "bands": []any{map[string]any{"label": "ok"}},
	})
	require.NoError(t, err)
	require.Equal(t, subjActor, def.subject, "absent subject defaults to the actor")
}

// TestParseSubjectRejectsBadValues pins the parse-time gates: an unknown subject value and a
// subject:target combined with a contested vs are both errors (not silent no-ops).
func TestParseSubjectRejectsBadValues(t *testing.T) {
	_, err := parseCheckSpec(map[string]any{
		"dice": "1d20", "subject": "caster", // not actor/target
		"bands": []any{map[string]any{"label": "ok"}},
	})
	require.Error(t, err, "an unknown subject value must be rejected")
	require.Contains(t, err.Error(), "subject")

	_, err = parseCheckSpec(map[string]any{
		"dice":    "1d20",
		"subject": "target",
		"vs":      map[string]any{"contested": map[string]any{"dice": "1d20", "bands": []any{map[string]any{"label": "x"}}}},
		"bands":   []any{map[string]any{"label": "ok"}},
	})
	require.Error(t, err, "subject: target with a contested vs must be rejected")
	require.Contains(t, err.Error(), "contested")
}
