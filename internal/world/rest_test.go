package world

import "testing"

// rest_test.go covers the #39 rest mechanic: the resting passive-regen bonus, the OnRest event firing
// once on entering rest, the rest/stand verb state transitions, and auto-stand-on-move.

// TestRestRegenBonus proves passive regen is faster while posResting (the restRegenMultiplier).
func TestRestRegenBonus(t *testing.T) {
	z, e := affectTestZone(t) // max_hp 100, regen 5

	// Standing control: one tick regens +5.
	e.SetHP(90)
	z.pulses.tick()
	if got := e.HP(); got != 95 {
		t.Fatalf("standing regen HP = %d, want 95 (+5)", got)
	}

	// Resting: one tick regens +5*restRegenMultiplier (=+10 at the default 2x).
	setPosition(e, posResting)
	e.SetHP(80)
	z.pulses.tick()
	if got := e.HP(); got != 80+5*restRegenMultiplier {
		t.Fatalf("resting regen HP = %d, want %d (+%d, the %dx bonus)", got, 80+5*restRegenMultiplier, 5*restRegenMultiplier, restRegenMultiplier)
	}
}

// TestRestFiresOnRestOnce proves `rest` fires evOnRest exactly once — on ENTER, not per tick. A resource
// with an OnRest op-list handler gains its bump when the player rests, and a subsequent resting tick does
// NOT re-fire it. (OnRest's counterpart is nil — a solo action — but an op-list handler can't OBSERVE
// that: `tgt: other` falls back to the handler ctx's default target, which is the subject either way; the
// nil counterpart matters only to a Lua handler reading `ev.other`. The fire site passes nil, matching
// the OnLevel/OnTrackStep precedent — see rest.go.)
func TestRestFiresOnRestOnce(t *testing.T) {
	z, caster := abilityTestZone(t)
	z.defs.attr.register("max_vigor", &attributeDef{ref: "max_vigor", base: litNode{v: 100}})
	z.defs.res.register("vigor", &resourceDef{
		ref: "vigor", maxAttr: "max_vigor",
		onEvent: map[eventKind][]effectOp{
			evOnRest: {{kind: "modify_resource", resource: "vigor", amount: 20, tgt: "self"}},
		},
	})
	setResourceCurrent(caster.entity, "vigor", 0)

	z.dispatch(caster, "rest")
	if position(caster.entity) != posResting {
		t.Fatal("rest did not set posResting")
	}
	if got := resourceCurrent(caster.entity, "vigor"); got != 20 {
		t.Fatalf("vigor after rest = %d, want 20 (OnRest fired on enter)", got)
	}

	// A tick while resting must NOT re-fire OnRest (it's an on-enter event, not per-tick).
	z.pulses.tick()
	if got := resourceCurrent(caster.entity, "vigor"); got != 20 {
		t.Fatalf("vigor after a resting tick = %d, want 20 (OnRest must not re-fire per tick)", got)
	}
}

// TestRestStandVerbs proves the state transitions + the idempotent notices.
func TestRestStandVerbs(t *testing.T) {
	z, caster := abilityTestZone(t)
	e := caster.entity

	z.dispatch(caster, "rest")
	if position(e) != posResting {
		t.Fatal("rest did not set posResting")
	}
	z.dispatch(caster, "sit") // alias, already resting → no-op notice, still resting
	if position(e) != posResting {
		t.Fatal("a second rest changed the position")
	}
	z.dispatch(caster, "stand")
	if position(e) != posStanding {
		t.Fatal("stand did not set posStanding")
	}

	// Registering rest/sit/stand must not steal the movement abbreviations: `s` still resolves to south
	// (an exact alias hit), not to `sit`/`stand` (aliases are exact-only; `stand` prefixes only `st...`).
	if cmd, ok := baseTable.resolve("s"); !ok || cmd.Name != "south" {
		t.Fatalf("`s` resolved to (%v,%v), want south (rest verbs must not shadow the movement short)", cmd, ok)
	}
}

// TestMoveAutoStandsFromRest proves a resting player who walks stands up first (move auto-stand).
func TestMoveAutoStandsFromRest(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := newTestPlayerEntity(z, "Walker")
	Move(s.entity, z.rooms["midgaard:room:temple"])

	z.dispatch(s, "rest")
	if position(s.entity) != posResting {
		t.Fatal("rest did not set posResting")
	}
	z.dispatch(s, "north") // temple -> market (same zone)
	if position(s.entity) != posStanding {
		t.Fatalf("moving did not auto-stand a resting player (position %d)", position(s.entity))
	}
}
