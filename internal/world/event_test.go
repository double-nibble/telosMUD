package world

import (
	"math/rand"
	"testing"
)

// event_test.go exercises the in-zone event bus (event.go [G3]): subscription gathering from the
// resources an entity HAS + its active affects, the OnCheck / OnAbilityResolved fire points, the
// `target: other` handler selector, the re-entrancy DEPTH guard, and the security property that a
// harmful handler op vs a protected player still funnels the PvP gate.

// eventTestZone extends abilityTestZone with a content-defined `rage` pool whose max is a per-entity
// attribute (0 by default, so only an entity granted a rage base "has" it) — the canonical resource
// that builds via events. onEvent is left to each test to set on a fresh def.
func eventTestZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	z, caster := abilityTestZone(t)
	// max_rage defaults to 0 (no rage) unless a per-entity base grants it — so entityHasResource
	// distinguishes a barbarian (granted rage) from everyone else.
	z.defs.attr.register("max_rage", &attributeDef{ref: "max_rage", base: litNode{v: 0}})
	setAttrBase(caster.entity, "max_rage", 100) // the caster is granted a 100 rage pool
	return z, caster
}

// registerRage installs the rage resource with the given event subscriptions.
func registerRage(z *Zone, onEvent map[eventKind][]effectOp) {
	z.defs.res.register("rage", &resourceDef{ref: "rage", maxAttr: "max_rage", onEvent: onEvent})
}

func TestEventBuildsResourceOnCheck(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnCheck: {{kind: "modify_resource", resource: "rage", amount: 5}},
	})
	setResourceCurrent(caster.entity, "rage", 0)
	mob := makeMobTarget(z, caster.entity, "goblin")

	// A trivial check by the caster fires OnCheck -> the rage handler runs modify_resource +5 on self.
	c := checkCtx(z, caster.entity, caster.entity, mob)
	resolveCheck(c, &checkSpec{dice: d1(t), bands: []checkBand{{label: "ok"}}})

	if got := resourceCurrent(caster.entity, "rage"); got != 5 {
		t.Fatalf("rage after one OnCheck = %d, want 5 (handler built it)", got)
	}
}

func TestEventOnAbilityResolved(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnAbilityResolved: {{kind: "modify_resource", resource: "rage", amount: 10}},
	})
	setResourceCurrent(caster.entity, "rage", 0)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)

	// Casting fireball fires OnAbilityResolved at step 10 -> rage builds.
	def := z.defs.ability.get("fireball")
	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))

	if got := resourceCurrent(caster.entity, "rage"); got != 10 {
		t.Fatalf("rage after casting fireball = %d, want 10 (OnAbilityResolved handler)", got)
	}
}

// TestEventTargetOtherHitsCounterpart proves the `target: other` selector binds the event counterpart:
// a handler that deals damage to `other` hits the mob passed as the counterpart, not self.
func TestEventTargetOtherHitsCounterpart(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnCheck: {{kind: "deal_damage", dmgType: "fire", amount: 20, tgt: "other"}},
	})
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)

	// Fire OnCheck about the caster, with the mob as the counterpart.
	z.fireEvent(nil, evOnCheck, caster.entity, mob, 1)
	if got := resourceCurrent(mob, "hp"); got != 80 {
		t.Fatalf("target:other handler -> mob hp = %d, want 80 (20 damage to the counterpart)", got)
	}
}

// TestEventHandlerStillGated is the SECURITY property: a harmful handler op against a non-consenting
// player is blocked by the same PvP gate — an event handler is not a bypass.
func TestEventHandlerStillGated(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnCheck: {{kind: "deal_damage", dmgType: "fire", amount: 50, tgt: "other"}},
	})
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	setResourceCurrent(victim.entity, "hp", 100)

	z.fireEvent(nil, evOnCheck, caster.entity, victim.entity, 1)
	if got := resourceCurrent(victim.entity, "hp"); got != 100 {
		t.Fatalf("harmful handler hit a non-consenting player: hp = %d, want 100 (gated)", got)
	}
}

// TestEventDepthGuard proves a self-firing handler (one that re-fires OnCheck) terminates at the depth
// budget instead of recursing forever. The handler bumps rage +1 then runs a nested check that re-
// fires OnCheck; it runs at depths 1..maxEventDepth, so rage lands at exactly maxEventDepth.
func TestEventDepthGuard(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnCheck: {
			{kind: "modify_resource", resource: "rage", amount: 1},
			{kind: "check", check: &checkSpec{dice: d1(t), bands: []checkBand{{label: "x"}}}},
		},
	})
	setResourceCurrent(caster.entity, "rage", 0)
	mob := makeMobTarget(z, caster.entity, "goblin")

	c := checkCtx(z, caster.entity, caster.entity, mob)
	resolveCheck(c, &checkSpec{dice: d1(t), bands: []checkBand{{label: "ok"}}})

	if got := resourceCurrent(caster.entity, "rage"); got != maxEventDepth {
		t.Fatalf("depth-guarded self-firing handler built rage=%d, want %d (bounded by maxEventDepth)",
			got, maxEventDepth)
	}
}

// TestEventSubscriptionGathering proves an entity only subscribes to a resource handler if it HAS the
// resource (a positive max or a stored current) — a barbarian builds rage, a commoner does not.
func TestEventSubscriptionGathering(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnCheck: {{kind: "modify_resource", resource: "rage", amount: 1}},
	})
	commoner := makeMobTarget(z, caster.entity, "commoner") // max_rage defaults to 0 -> no rage

	if got := len(gatherEventHandlers(caster.entity, evOnCheck)); got != 1 {
		t.Fatalf("caster (has rage) OnCheck handlers = %d, want 1", got)
	}
	if got := len(gatherEventHandlers(commoner, evOnCheck)); got != 0 {
		t.Fatalf("commoner (no rage) OnCheck handlers = %d, want 0", got)
	}
	// And no handlers for an event nobody subscribed to.
	if got := len(gatherEventHandlers(caster.entity, evOnKill)); got != 0 {
		t.Fatalf("OnKill handlers = %d, want 0 (nobody subscribed)", got)
	}
}

// TestEventApplyAffectDetrimentalStillGated: a handler ctx has disp=dispNeutral, so a detrimental
// affect applied to another player via a handler is gated SOLELY by affectIsDetrimental (the derived
// check) — the auditor's highest-risk untested branch. poison (a −2 strength affect) must NOT land on
// a non-consenting player.
func TestEventApplyAffectDetrimentalStillGated(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnCheck: {{kind: "apply_affect", affect: "poison", tgt: "other"}}, // no harmful flag; disp neutral
	})
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")

	z.fireEvent(nil, evOnCheck, caster.entity, victim.entity, 1)
	if a, ok := Get[*Affected](victim.entity); ok && len(a.list) != 0 {
		t.Fatalf("detrimental affect landed on a non-consenting player via a handler: %d affects, want 0", len(a.list))
	}
}

// TestEventModifyResourceOtherStillGated: a cross-player modify_resource through a handler is gated
// (any sign — a content pool's polarity is unknown). The victim's mana must be untouched.
func TestEventModifyResourceOtherStillGated(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnCheck: {{kind: "modify_resource", resource: "mana", amount: 25, tgt: "other"}},
	})
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	setResourceCurrent(victim.entity, "mana", 50)

	z.fireEvent(nil, evOnCheck, caster.entity, victim.entity, 1)
	if got := resourceCurrent(victim.entity, "mana"); got != 50 {
		t.Fatalf("cross-player modify_resource via handler hit a non-consenting player: mana = %d, want 50 (gated)", got)
	}
}

// TestGuardHarmfulFailsClosedOnDetached: a harm op against a DETACHED counterpart (location nil — being
// reaped / handed off) is denied, never evaluating the gate against a stale pointer (MUST-FIX 1).
func TestGuardHarmfulFailsClosedOnDetached(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnCheck: {{kind: "deal_damage", dmgType: "fire", amount: 50, tgt: "other"}},
	})
	// A living entity with NO location (never Moved into a room): the detached/in-transit shape.
	ghost := z.newEntity(ProtoRef("test:ghost"))
	Add(ghost, &Living{})
	setResourceCurrent(ghost, "hp", 100)

	z.fireEvent(nil, evOnCheck, caster.entity, ghost, 1) // must not panic
	if got := resourceCurrent(ghost, "hp"); got != 100 {
		t.Fatalf("harm hit a detached entity: hp = %d, want 100 (fail-closed)", got)
	}
}

// TestEventWidthBudget: a WIDE + self-firing cascade (two handlers per level, each re-firing OnCheck)
// fans out multiplicatively. The depth cap alone would allow up to 2^1+..+2^8 = 510 handler runs; the
// total-work budget truncates it at maxEventHandlers, proving width (not just depth) is bounded — and
// the test TERMINATES (no heartbeat starvation).
func TestEventWidthBudget(t *testing.T) {
	z, caster := eventTestZone(t)
	z.defs.attr.register("max_tally", &attributeDef{ref: "max_tally", base: litNode{v: 0}})
	setAttrBase(caster.entity, "max_tally", 100000)
	z.defs.res.register("tally", &resourceDef{ref: "tally", maxAttr: "max_tally"})
	// Two resources the caster HAS, each subscribing a handler that bumps the shared tally then re-fires
	// OnCheck via a nested check.
	handler := []effectOp{
		{kind: "modify_resource", resource: "tally", amount: 1},
		{kind: "check", check: &checkSpec{dice: d1(t), bands: []checkBand{{label: "x"}}}},
	}
	z.defs.res.register("rageA", &resourceDef{
		ref: "rageA", maxAttr: "max_rage",
		onEvent: map[eventKind][]effectOp{evOnCheck: handler},
	})
	z.defs.res.register("rageB", &resourceDef{
		ref: "rageB", maxAttr: "max_rage",
		onEvent: map[eventKind][]effectOp{evOnCheck: handler},
	})
	setResourceCurrent(caster.entity, "tally", 0)
	mob := makeMobTarget(z, caster.entity, "goblin")

	c := checkCtx(z, caster.entity, caster.entity, mob)
	resolveCheck(c, &checkSpec{dice: d1(t), bands: []checkBand{{label: "ok"}}})

	got := resourceCurrent(caster.entity, "tally")
	if got != maxEventHandlers {
		t.Fatalf("width-budget cascade ran %d handlers, want exactly %d (budget cap)", got, maxEventHandlers)
	}
}

func TestParseEventMap(t *testing.T) {
	// Valid kinds parse; an unknown kind is dropped (content can't invent events).
	in := map[string]any{
		"OnHit":      []any{map[string]any{"op": "modify_resource", "resource": "rage", "amount": 5.0}},
		"OnNonsense": []any{map[string]any{"op": "heal", "resource": "hp", "amount": 1.0}},
	}
	out := parseEventMap(in, "test")
	if len(out) != 1 {
		t.Fatalf("parseEventMap kinds = %d, want 1 (OnNonsense dropped)", len(out))
	}
	if _, ok := out[evOnHit]; !ok {
		t.Fatalf("parseEventMap dropped the valid OnHit subscription")
	}
	if _, ok := out[eventKind("OnNonsense")]; ok {
		t.Fatalf("parseEventMap kept an unknown event kind")
	}
	if parseEventMap(nil, "test") != nil {
		t.Fatalf("parseEventMap(nil) should be nil")
	}
}
