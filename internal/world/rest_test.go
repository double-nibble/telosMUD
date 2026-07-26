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

// restKindZone registers a "vigor" resource whose OnRest/OnShortRest/OnLongRest handlers each bump a
// DISTINCT resource, so a test can read exactly which events fired from one rest command. Returns the
// zone + caster.
func restKindZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	z, caster := abilityTestZone(t)
	for _, ref := range []string{"any_rests", "short_rests", "long_rests"} {
		z.defs.res.register(ref, &resourceDef{ref: ref})
		setResourceCurrent(caster.entity, ref, 0)
	}
	z.defs.attr.register("max_vigor", &attributeDef{ref: "max_vigor", base: litNode{v: 100}})
	// vigor carries the OnRest/OnShortRest/OnLongRest subscriptions AND a regen rate, so that (a) the
	// entity "has" the resource and its handlers are gathered, and (b) needsRegen(caster) is true — the
	// per-entity tick actually PROCESSES the entity, so a tick-based once-per-rest guard is a LIVE check,
	// not a no-op against a never-scheduled entity.
	z.defs.res.register("vigor", &resourceDef{
		ref: "vigor", maxAttr: "max_vigor", regen: 1,
		onEvent: map[eventKind][]effectOp{
			evOnRest:      {{kind: "modify_resource", resource: "any_rests", amount: 1, tgt: "self"}},
			evOnShortRest: {{kind: "modify_resource", resource: "short_rests", amount: 1, tgt: "self"}},
			evOnLongRest:  {{kind: "modify_resource", resource: "long_rests", amount: 1, tgt: "self"}},
		},
	})
	setResourceCurrent(caster.entity, "vigor", 0) // below max => needsRegen true => the tick runs regen
	return z, caster
}

// TestShortRestFiresShortNotLong (#512): a bare `rest` (and `rest short`) is a SHORT rest — it fires
// OnRest + OnShortRest, but NOT OnLongRest.
func TestShortRestFiresShortNotLong(t *testing.T) {
	for _, cmd := range []string{"rest", "rest short", "sit"} {
		t.Run(cmd, func(t *testing.T) {
			z, caster := restKindZone(t)
			z.dispatch(caster, cmd)
			if position(caster.entity) != posResting {
				t.Fatalf("%q did not set posResting", cmd)
			}
			if got := resourceCurrent(caster.entity, "any_rests"); got != 1 {
				t.Fatalf("%q: OnRest fired %d times, want 1", cmd, got)
			}
			if got := resourceCurrent(caster.entity, "short_rests"); got != 1 {
				t.Fatalf("%q: OnShortRest fired %d times, want 1", cmd, got)
			}
			if got := resourceCurrent(caster.entity, "long_rests"); got != 0 {
				t.Fatalf("%q: OnLongRest fired %d times, want 0 (a short rest must not fire OnLongRest)", cmd, got)
			}
		})
	}
}

// TestLongRestFiresLongNotShort (#512): `rest long` (and `rest full`) is a LONG rest — it fires
// OnRest + OnLongRest, but NOT OnShortRest.
func TestLongRestFiresLongNotShort(t *testing.T) {
	for _, cmd := range []string{"rest long", "rest full"} {
		t.Run(cmd, func(t *testing.T) {
			z, caster := restKindZone(t)
			z.dispatch(caster, cmd)
			if position(caster.entity) != posResting {
				t.Fatalf("%q did not set posResting", cmd)
			}
			if got := resourceCurrent(caster.entity, "any_rests"); got != 1 {
				t.Fatalf("%q: OnRest fired %d times, want 1", cmd, got)
			}
			if got := resourceCurrent(caster.entity, "long_rests"); got != 1 {
				t.Fatalf("%q: OnLongRest fired %d times, want 1", cmd, got)
			}
			if got := resourceCurrent(caster.entity, "short_rests"); got != 0 {
				t.Fatalf("%q: OnShortRest fired %d times, want 0 (a long rest must not fire OnShortRest)", cmd, got)
			}
		})
	}
}

// TestRestKindFiresOncePerRest (#512): the kind events, like OnRest, are on-ENTER — a resting tick must
// not re-fire them.
func TestRestKindFiresOncePerRest(t *testing.T) {
	z, caster := restKindZone(t)
	z.dispatch(caster, "rest long")
	// Precondition: the tick must actually process this entity, or the "no re-fire on tick" assertion
	// below is vacuous (a tick that skips the entity trivially can't re-fire anything). vigor regen + a
	// below-max current make needsRegen true.
	if !needsRegen(caster.entity) {
		t.Fatal("precondition failed: needsRegen(caster) is false, so the tick would not process the entity")
	}
	z.pulses.tick()
	if got := resourceCurrent(caster.entity, "long_rests"); got != 1 {
		t.Fatalf("OnLongRest fired %d times after a resting tick, want 1 (on-enter only)", got)
	}
	if got := resourceCurrent(caster.entity, "any_rests"); got != 1 {
		t.Fatalf("OnRest fired %d times after a resting tick, want 1 (on-enter only)", got)
	}
}

// TestRestUnknownKindRefuses (#512): an unrecognized rest argument is a typo — cmdRest refuses (no state
// change, no events) rather than silently defaulting to a short rest.
func TestRestUnknownKindRefuses(t *testing.T) {
	z, caster := restKindZone(t)
	z.dispatch(caster, "rest lng") // typo for "long"
	if position(caster.entity) != posStanding {
		t.Fatal("an unknown rest kind should not change position")
	}
	for _, ref := range []string{"any_rests", "short_rests", "long_rests"} {
		if got := resourceCurrent(caster.entity, ref); got != 0 {
			t.Fatalf("an unknown rest kind fired %s (%d), want no events", ref, got)
		}
	}
}

// TestParseRestKind (#512): the pure argument→kind mapping.
func TestParseRestKind(t *testing.T) {
	cases := []struct {
		arg  string
		want restKind
		ok   bool
	}{
		{"", restShort, true},
		{"short", restShort, true},
		{"SHORT", restShort, true},
		{"long", restLong, true},
		{"Full", restLong, true},
		{"  long  ", restLong, true},
		{"lng", restShort, false},
		{"nap", restShort, false},
	}
	for _, tc := range cases {
		got, ok := parseRestKind(tc.arg)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("parseRestKind(%q) = (%d,%v), want (%d,%v)", tc.arg, got, ok, tc.want, tc.ok)
		}
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
