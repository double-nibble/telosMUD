package world

import (
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// luahook_test.go — slice 7.8 (the hookability obligations, docs/PHASE7-PLAN.md §5 7.8 + pillar 2):
//
//	(a) the content-namespaced CUSTOM-EVENT lane — a pack mud.fire("pack:Name", subject, data)s an
//	    event the engine never heard of and on("pack:Name", fn)-subscribes a handler, still
//	    depth/width-budgeted + harm-gated like an engine event (NO privileged status), namespaced by
//	    pack to avoid collision; a bare unknown kind is rejected.
//	(b) the reserved-kind LIGHTING — OnApplyAffect / OnAffectTick / OnAffectExpire (the affect
//	    runtime) and a new OnEnter (the move path) now actually fire to content/Lua handlers.

// --- (a) the custom-event lane -----------------------------------------------------------------

// fireCustomZone builds a scripted-mob zone whose mob can both fire and handle custom events. It
// returns the zone, the room, the player, and the scripted mob.
func fireCustomZone(t *testing.T, mobLua string) (*Zone, *Entity, *Entity, *Entity) {
	t.Helper()
	z, room, player := scriptedZone(t)
	mob := addScriptedMob(z, room, "captain", mobLua)
	return z, room, player, mob
}

// TestCustomEventRoundTrips is the headline 7.8a done-when: a pack defines, fires, and handles a
// custom event ("sailing:OnShipDock") the engine never heard of — entirely in content. The mob
// subscribes a handler that records into self.state; another script fires the event via mud.fire;
// the handler runs and the data payload threads through ev.data.
func TestCustomEventRoundTrips(t *testing.T) {
	z, _, player, mob := fireCustomZone(t, `
		on("sailing:OnShipDock", function(ev)
			state.docked = true
			state.port = ev.data and ev.data.port or "?"
			-- self is the subject the event was fired ABOUT (the captain).
			state.who = self:name()
		end)
	`)

	// Build the mob's handler table (register its on(...) subscriptions).
	z.lua.ensureEntityScript(mob)

	// Fire the custom event FROM a separate script body (the firer = the player), about the mob
	// (subject), with a data table: mud.fire("sailing:OnShipDock", mobHandle, {port="Tortuga"}).
	fireCustomFromBody(t, z, player, mob,
		`mud.fire("sailing:OnShipDock", subject, {port = "Tortuga"})`)

	es := z.lua.entityScripts[mob.rid]
	if es == nil {
		t.Fatal("the captain's script did not register")
	}
	if got := es.state.RawGetString("docked"); got != lua.LTrue {
		t.Fatalf("custom event handler did not run: state.docked = %v, want true", got)
	}
	if got := es.state.RawGetString("port").String(); got != "Tortuga" {
		t.Fatalf("ev.data did not thread: state.port = %q, want Tortuga", got)
	}
	if got := es.state.RawGetString("who").String(); got != "captain" {
		t.Fatalf("self did not bind to the subject: state.who = %q, want captain", got)
	}
}

// TestCustomEventBudgetAndGate is the SECURITY test (7.8a): a custom event obeys the SAME depth/
// width budget as an engine event (a handler that re-fires the SAME custom event is bounded by
// maxEventDepth, not infinite), AND a harmful op in a custom handler funnels guardHarmful against a
// non-consenting player exactly like an engine-event handler.
func TestCustomEventBudgetAndGate(t *testing.T) {
	// --- depth bound: a self-re-firing custom handler terminates at maxEventDepth ---
	t.Run("depth-bounded", func(t *testing.T) {
		z, _, player, mob := fireCustomZone(t, `
			on("pack:Loop", function(ev)
				state.count = (state.count or 0) + 1
				mud.fire("pack:Loop", self) -- re-fire the SAME custom event about self
			end)
		`)
		z.lua.ensureEntityScript(mob)
		fireCustomFromBody(t, z, player, mob, `mud.fire("pack:Loop", subject)`)

		es := z.lua.entityScripts[mob.rid]
		got := es.state.RawGetString("count")
		// The first fire runs at depth 1; each re-fire nests one deeper; fireEvent refuses past
		// maxEventDepth. So the handler runs exactly maxEventDepth times (depth 1..maxEventDepth) —
		// a finite, bounded count, NOT a stack overflow / hang.
		if got.String() != itoa(maxEventDepth) {
			t.Fatalf("self-re-firing custom handler ran %s times, want %d (bounded by maxEventDepth)",
				got.String(), maxEventDepth)
		}
	})

	// --- gate: a harmful op in a custom handler is blocked vs a non-consenting player ---
	t.Run("harm-gated", func(t *testing.T) {
		z, caster := eventTestZone(t)
		// The harming SUBJECT is a PLAYER (caster) carrying a custom-event handler that tries to
		// damage the target in ev.data. A player harming a non-consenting player is exactly what the
		// PvP gate blocks — and the custom handler must funnel guardHarmful like any other handler.
		Add(caster.entity, &Scripted{source: `
			on("pack:Strike", function(ev) ev.data.target:damage{amount = 50, type = "fire"} end)
		`})
		z.lua.ensureEntityScript(caster.entity)
		registerRoom(z, caster.entity.location)

		victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
		setResourceCurrent(victim.entity, "hp", 100)
		setFlag(caster.entity, flagPvP, true) // the attacker flags; the VICTIM does NOT consent

		fireCustomFromBody(t, z, caster.entity, caster.entity,
			`mud.fire("pack:Strike", subject, {target = other})`, withOther(victim.entity))

		if got := resourceCurrent(victim.entity, "hp"); got != 100 {
			t.Fatalf("custom-handler harm hit a non-consenting player: hp = %d, want 100 (gated)", got)
		}

		// CONTROL: the SAME handler DOES land on a CONSENTING player — proving the block above is the
		// gate, not a broken harm path.
		consenting := makePlayerTargetInRoom(z, caster.entity, "Willing")
		setResourceCurrent(consenting.entity, "hp", 100)
		setFlag(consenting.entity, flagPvP, true)
		fireCustomFromBody(t, z, caster.entity, caster.entity,
			`mud.fire("pack:Strike", subject, {target = other})`, withOther(consenting.entity))
		if got := resourceCurrent(consenting.entity, "hp"); got != 50 {
			t.Fatalf("custom-handler harm did NOT land on a consenting player: hp = %d, want 50", got)
		}
	})
}

// TestCustomEventRejectsBareKind asserts the namespace gate: mud.fire of a BARE name (no "<pack>:"
// separator) is rejected — the lane does not open a hole where any string is a valid engine event.
// Both an unknown bare name and a real-but-bare engine kind are rejected from content.
func TestCustomEventRejectsBareKind(t *testing.T) {
	z, _, player := scriptedZone(t)
	cases := []string{
		`mud.fire("OnTotallyMadeUp", self)`, // an unknown bare kind
		`mud.fire("OnHit", self)`,           // a REAL engine kind — content may not synthesize it
		`mud.fire("OnEnter", self)`,         // the new engine kind is engine-fired, not content-fired
	}
	for _, body := range cases {
		err := z.lua.runChunkAs("test:badfire", body, &luaInvocation{actor: player.location})
		if err == nil {
			t.Fatalf("mud.fire of a bare engine kind was accepted, want a clean error: %q", body)
		}
	}
	// A namespaced kind is accepted (no error) even with no subscriber (a clean no-op fire).
	if err := z.lua.runChunkAs("test:okfire", `mud.fire("pack:OnWhatever", self)`,
		&luaInvocation{actor: player}); err != nil {
		t.Fatalf("mud.fire of a namespaced kind with no subscriber errored: %v", err)
	}
}

// TestCustomEventPackNamespacing asserts two packs' SAME-named events do not collide: a subject
// subscribed to "alpha:OnDock" is NOT triggered by a fire of "beta:OnDock" (the pack prefix
// namespaces the kind). Only the matching namespaced fire runs the handler.
func TestCustomEventPackNamespacing(t *testing.T) {
	z, _, player, mob := fireCustomZone(t, `
		on("alpha:OnDock", function(ev) state.alpha = (state.alpha or 0) + 1 end)
		on("beta:OnDock",  function(ev) state.beta  = (state.beta  or 0) + 1 end)
	`)
	z.lua.ensureEntityScript(mob)

	fireCustomFromBody(t, z, player, mob, `mud.fire("alpha:OnDock", subject)`)
	es := z.lua.entityScripts[mob.rid]
	if es.state.RawGetString("alpha").String() != "1" {
		t.Fatalf("alpha:OnDock handler did not run; alpha = %s", es.state.RawGetString("alpha").String())
	}
	if es.state.RawGetString("beta") != lua.LNil {
		t.Fatalf("beta:OnDock handler ran on an alpha:OnDock fire (namespace collision); beta = %s",
			es.state.RawGetString("beta").String())
	}

	// Now fire beta: only beta increments.
	fireCustomFromBody(t, z, player, mob, `mud.fire("beta:OnDock", subject)`)
	if es.state.RawGetString("alpha").String() != "1" {
		t.Fatalf("alpha re-ran on a beta fire; alpha = %s", es.state.RawGetString("alpha").String())
	}
	if es.state.RawGetString("beta").String() != "1" {
		t.Fatalf("beta:OnDock handler did not run on its own fire; beta = %s", es.state.RawGetString("beta").String())
	}
}

// TestEventKindClassification asserts the namespace discriminator: a bare OnX is an ENGINE kind
// (fireable only if known), a "<pack>:Name" is a CUSTOM kind (always fireable), and a bare unknown
// kind is NEITHER — so the pack: lane does not open a hole where any string is a valid engine event.
// This is the lint property: an unknown bare kind is still rejected.
func TestEventKindClassification(t *testing.T) {
	cases := []struct {
		kind     eventKind
		custom   bool // is it a namespaced custom kind?
		fireable bool // may content fire/subscribe it?
	}{
		{evOnHit, false, true},                        // a known engine kind
		{evOnEnter, false, true},                      // the newly-lit engine kind
		{eventKind("OnNonsense"), false, false},       // a BARE unknown kind — rejected (the lint property)
		{eventKind("sailing:OnShipDock"), true, true}, // a namespaced custom kind — fireable
		{eventKind("a:OnDock"), true, true},
		{eventKind("OnApplyAffect"), false, true}, // a reserved-now-lit engine kind
	}
	for _, c := range cases {
		if got := isCustomEventKind(c.kind); got != c.custom {
			t.Errorf("isCustomEventKind(%q) = %v, want %v", c.kind, got, c.custom)
		}
		if got := isFireableEventKind(c.kind); got != c.fireable {
			t.Errorf("isFireableEventKind(%q) = %v, want %v", c.kind, got, c.fireable)
		}
	}
}

// TestOnEventParserRejectsBareUnknown asserts the content on_event parser still drops a bare unknown
// engine kind (the lint catches a bogus bare kind), and does NOT accept a namespaced custom kind into
// the engine-bus op-list lane (custom events are the on(name,fn) trigger lane, not on_event).
func TestOnEventParserRejectsBareUnknown(t *testing.T) {
	in := map[string]any{
		"OnHit":           []any{map[string]any{"op": "heal", "resource": "hp", "amount": 1.0}},
		"OnTotallyMadeUp": []any{map[string]any{"op": "heal", "resource": "hp", "amount": 1.0}}, // bare unknown — dropped
		"pack:OnShipDock": []any{map[string]any{"op": "heal", "resource": "hp", "amount": 1.0}}, // custom — not an on_event engine kind
	}
	out := parseEventMap(in, "test")
	if _, ok := out[evOnHit]; !ok {
		t.Fatal("parseEventMap dropped the valid OnHit subscription")
	}
	if _, ok := out[eventKind("OnTotallyMadeUp")]; ok {
		t.Fatal("parseEventMap kept a bare unknown engine kind (lint regression)")
	}
	if _, ok := out[eventKind("pack:OnShipDock")]; ok {
		t.Fatal("parseEventMap accepted a custom kind into the engine-bus op-list lane")
	}
}

// --- (b) reserved-kind lighting ----------------------------------------------------------------

// TestOnEnterFiresToHandler asserts the new OnEnter movement hook fires to a bus handler. A resource
// the entrant HAS subscribes an OnEnter op-list that builds; entering a room fires it.
func TestOnEnterFiresToHandler(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnEnter: {{kind: "modify_resource", resource: "rage", amount: 7}},
	})
	setResourceCurrent(caster.entity, "rage", 0)
	room := z.newEntity("test:room:dest")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["test:room:dest"] = room

	// fireRoomEntry is the trigger fire point; the engine fires OnEnter on the move path. Drive the
	// bus directly (the unit under test is the event-kind lighting, not the command plumbing).
	z.fireEvent(nil, evOnEnter, caster.entity, room, 1)

	if got := resourceCurrent(caster.entity, "rage"); got != 7 {
		t.Fatalf("OnEnter handler did not fire: rage = %d, want 7", got)
	}
}

// TestOnApplyAffectFires asserts applying an affect fires the reserved OnApplyAffect bus kind. The
// AFFECTED entity (the subject) carries a resource subscribed to OnApplyAffect that builds.
func TestOnApplyAffectFires(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnApplyAffect: {{kind: "modify_resource", resource: "rage", amount: 3}},
	})
	setResourceCurrent(caster.entity, "rage", 0)

	applyAffect(caster.entity, "haste", attachOpts{}, nil) // attach fires OnApplyAffect about caster

	if got := resourceCurrent(caster.entity, "rage"); got != 3 {
		t.Fatalf("OnApplyAffect did not fire on attach: rage = %d, want 3", got)
	}
}

// TestOnAffectExpireFires asserts expiry fires the reserved OnAffectExpire bus kind.
func TestOnAffectExpireFires(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnAffectExpire: {{kind: "modify_resource", resource: "rage", amount: 4}},
	})
	setResourceCurrent(caster.entity, "rage", 0)

	inst := applyAffect(caster.entity, "haste", attachOpts{duration: 1}, nil)
	if inst == nil {
		t.Fatal("affect did not attach")
	}
	// Expire it directly (the tick path drives expiry; here we exercise the expire seam's fire).
	a, _ := Get[*Affected](caster.entity)
	a.expire(caster.entity, inst, nil)

	if got := resourceCurrent(caster.entity, "rage"); got != 4 {
		t.Fatalf("OnAffectExpire did not fire on expire: rage = %d, want 4", got)
	}
}

// TestOnAffectTickFires asserts a tick-interval boundary fires the reserved OnAffectTick bus kind —
// independent of whether the affect's def carries tick ops (a subscriber reacts to the tick even for
// a no-op-list affect). poison ticks every 6 pulses; on the 6th the subject's rage builds.
func TestOnAffectTickFires(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnAffectTick: {{kind: "modify_resource", resource: "rage", amount: 2}},
	})
	setResourceCurrent(caster.entity, "rage", 0)

	inst := applyAffect(caster.entity, "poison", attachOpts{duration: 100}, nil)
	if inst == nil {
		t.Fatal("affect did not attach")
	}
	a, _ := Get[*Affected](caster.entity)
	// poison tickInterval is 6: ticks 1..5 don't reach the boundary; tick 6 fires OnAffectTick.
	for p := uint64(1); p <= 6; p++ {
		a.tickOnce(caster.entity, p)
	}
	if got := resourceCurrent(caster.entity, "rage"); got != 2 {
		t.Fatalf("OnAffectTick did not fire at the tick-interval boundary: rage = %d, want 2", got)
	}
}

// --- (b) recursion safety: the affect-lifecycle fires must NOT reset the cascade budget ---------

// TestAffectApplyLoopBounded is the CRITICAL regression (the auditor's exploit): two mutually-
// applying affects whose OnApplyAffect op-list re-applies the other. Pre-fix this recursed the Go
// stack unbounded (each apply fired OnApplyAffect at a fresh depth-0 root) until a FATAL process
// panic — no Lua VM, so no sandbox defense. Post-fix the apply threads the cascade ctx (so the
// nested OnApplyAffect trips maxEventDepth) AND the zone-level backstop trips regardless. The test
// asserts it TERMINATES (returns, no crash) and the cascade-depth counter is restored. Under -race.
func TestAffectApplyLoopBounded(t *testing.T) {
	z, caster := eventTestZone(t)
	// Two pure-buff affects (no stat reductions -> non-detrimental -> the ungated applyAffect path),
	// each re-applying the OTHER on OnApplyAffect — the self-perpetuating cycle.
	z.defs.affect.register("loopA", &affectDef{
		ref: "loopA", name: "LoopA", stacking: stackRefresh, maxStacks: 1, duration: 50,
		onEvent: map[eventKind][]effectOp{
			evOnApplyAffect: {{kind: "apply_affect", affect: "loopB", tgt: "self"}},
		},
	})
	z.defs.affect.register("loopB", &affectDef{
		ref: "loopB", name: "LoopB", stacking: stackRefresh, maxStacks: 1, duration: 50,
		onEvent: map[eventKind][]effectOp{
			evOnApplyAffect: {{kind: "apply_affect", affect: "loopA", tgt: "self"}},
		},
	})

	done := make(chan struct{})
	go func() {
		// A genuine ROOT apply (a cast step would pass nil). The loop must terminate via the
		// depth/backstop guard, not run forever.
		applyAffect(caster.entity, "loopA", attachOpts{}, nil)
		close(done)
	}()
	select {
	case <-done:
		if z.eventCascadeDepth != 0 {
			t.Fatalf("eventCascadeDepth leaked = %d, want 0 (the defer must restore it)", z.eventCascadeDepth)
		}
	case <-timeAfter():
		t.Fatal("the mutually-applying-affect loop did NOT terminate (recursion unbounded)")
	}
}

// TestAffectApplyLoopSelfBounded is the single-affect variant: one affect whose own OnApplyAffect
// re-applies itself. Same unbounded-recursion class; must terminate.
func TestAffectApplyLoopSelfBounded(t *testing.T) {
	z, caster := eventTestZone(t)
	z.defs.affect.register("selfloop", &affectDef{
		ref: "selfloop", name: "SelfLoop", stacking: stackRefresh, maxStacks: 1, duration: 50,
		onEvent: map[eventKind][]effectOp{
			evOnApplyAffect: {{kind: "apply_affect", affect: "selfloop", tgt: "self"}},
		},
	})
	done := make(chan struct{})
	go func() {
		applyAffect(caster.entity, "selfloop", attachOpts{}, nil)
		close(done)
	}()
	select {
	case <-done:
		if z.eventCascadeDepth != 0 {
			t.Fatalf("eventCascadeDepth leaked = %d, want 0", z.eventCascadeDepth)
		}
	case <-timeAfter():
		t.Fatal("the self-applying-affect loop did NOT terminate (recursion unbounded)")
	}
}

// TestAffectExpireLoopBounded is the OnAffectExpire variant: an affect whose OnAffectExpire op-list
// removes another affect, whose own OnAffectExpire re-applies the first — a mutual apply/expire
// loop. Must terminate (the auditor's dispel/remove_affect variant).
func TestAffectExpireLoopBounded(t *testing.T) {
	z, caster := eventTestZone(t)
	z.defs.affect.register("expA", &affectDef{
		ref: "expA", name: "ExpA", stacking: stackRefresh, maxStacks: 1, duration: 50, dispellable: true,
		onEvent: map[eventKind][]effectOp{
			evOnAffectExpire: {{kind: "apply_affect", affect: "expB", tgt: "self"}},
		},
	})
	z.defs.affect.register("expB", &affectDef{
		ref: "expB", name: "ExpB", stacking: stackRefresh, maxStacks: 1, duration: 50, dispellable: true,
		onEvent: map[eventKind][]effectOp{
			evOnAffectExpire: {{kind: "remove_affect", affect: "expA", tgt: "self"}},
		},
	})
	applyAffect(caster.entity, "expA", attachOpts{}, nil)
	applyAffect(caster.entity, "expB", attachOpts{}, nil)

	done := make(chan struct{})
	go func() {
		a, _ := Get[*Affected](caster.entity)
		c := seededCtx(z, caster.entity, caster.entity, dispNeutral)
		if inst, ok := a.byKey[keyFor(z.defs.affect.get("expA"), nil)]; ok {
			a.expire(caster.entity, inst, c) // start the apply/expire cascade
		}
		close(done)
	}()
	select {
	case <-done:
		if z.eventCascadeDepth != 0 {
			t.Fatalf("eventCascadeDepth leaked = %d, want 0", z.eventCascadeDepth)
		}
	case <-timeAfter():
		t.Fatal("the apply/expire loop did NOT terminate (recursion unbounded)")
	}
}

// TestLegitimateFiniteAffectCascadeCompletes asserts the backstop does NOT false-trip a normal,
// finite affect cascade: an affect whose OnApplyAffect applies a DIFFERENT one-shot affect (no
// cycle) completes fully — all three land, a few levels deep, well under maxEventCascadeDepth.
func TestLegitimateFiniteAffectCascadeCompletes(t *testing.T) {
	z, caster := eventTestZone(t)
	z.defs.affect.register("chain3", &affectDef{ref: "chain3", name: "C3", stacking: stackRefresh, maxStacks: 1, duration: 50})
	z.defs.affect.register("chain2", &affectDef{
		ref: "chain2", name: "C2", stacking: stackRefresh, maxStacks: 1, duration: 50,
		onEvent: map[eventKind][]effectOp{evOnApplyAffect: {{kind: "apply_affect", affect: "chain3", tgt: "self"}}},
	})
	z.defs.affect.register("chain1", &affectDef{
		ref: "chain1", name: "C1", stacking: stackRefresh, maxStacks: 1, duration: 50,
		onEvent: map[eventKind][]effectOp{evOnApplyAffect: {{kind: "apply_affect", affect: "chain2", tgt: "self"}}},
	})
	applyAffect(caster.entity, "chain1", attachOpts{}, nil)

	a, ok := Get[*Affected](caster.entity)
	if !ok {
		t.Fatal("no affects component after the cascade")
	}
	// Presence by def ref, regardless of source key (chain1 is self-applied source=nil; chain2/chain3
	// are applied by the handler with source=the subject).
	landed := map[string]bool{}
	for _, inst := range a.list {
		landed[inst.def.ref] = true
	}
	for _, ref := range []string{"chain1", "chain2", "chain3"} {
		if !landed[ref] {
			t.Fatalf("finite cascade did not land %q (the backstop false-tripped a legitimate cascade)", ref)
		}
	}
	if z.eventCascadeDepth != 0 {
		t.Fatalf("eventCascadeDepth leaked = %d, want 0", z.eventCascadeDepth)
	}
}

// TestEventCascadeBackstopTrips proves the ZONE-LEVEL backstop (maxEventCascadeDepth) fires
// INDEPENDENT of the per-ctx depth guard — the can't-forget property. With the counter pinned at the
// cap (simulating a deeply-nested cascade a forgotten thread could produce), fireEvent refuses to run
// any handler regardless of a nil parent (depth would otherwise read 0). A handler that WOULD build a
// resource does NOT run; one notch below the cap, it DOES — proving the cap is the gate.
func TestEventCascadeBackstopTrips(t *testing.T) {
	z, caster := eventTestZone(t)
	registerRage(z, map[eventKind][]effectOp{
		evOnCheck: {{kind: "modify_resource", resource: "rage", amount: 1}},
	})
	setResourceCurrent(caster.entity, "rage", 0)

	// At the cap: the fire is refused (the backstop truncates) even with a nil parent (depth 0).
	z.eventCascadeDepth = maxEventCascadeDepth
	z.fireEvent(nil, evOnCheck, caster.entity, nil, 1)
	if got := resourceCurrent(caster.entity, "rage"); got != 0 {
		t.Fatalf("backstop did not truncate at the cap: rage = %d, want 0", got)
	}
	if z.eventCascadeDepth != maxEventCascadeDepth {
		t.Fatalf("backstop leaked the counter on a refused fire: %d, want %d", z.eventCascadeDepth, maxEventCascadeDepth)
	}

	// One below the cap: the fire runs (one handler) — proving the cap is the gate, not a blanket off.
	z.eventCascadeDepth = maxEventCascadeDepth - 1
	z.fireEvent(nil, evOnCheck, caster.entity, nil, 1)
	if got := resourceCurrent(caster.entity, "rage"); got != 1 {
		t.Fatalf("backstop false-tripped below the cap: rage = %d, want 1", got)
	}
	if z.eventCascadeDepth != maxEventCascadeDepth-1 {
		t.Fatalf("backstop did not restore the counter: %d, want %d", z.eventCascadeDepth, maxEventCascadeDepth-1)
	}
}

// --- helpers -----------------------------------------------------------------------------------

// fireCustomFromBody runs a Lua body that fires a custom event, with `subject` bound to the given
// subject handle and the firer as the invocation actor. Options can bind `other`. This wraps the
// raw runtime invoke so the test body can reference `subject` (and optionally `other`).
type fireOpt struct{ other *Entity }

func withOther(e *Entity) fireOpt { return fireOpt{other: e} }

func fireCustomFromBody(t *testing.T, z *Zone, firer, subject *Entity, body string, opts ...fireOpt) {
	t.Helper()
	rt := z.lua
	ch := rt.chunkFor("test:custom-fire", body)
	if ch == nil {
		t.Fatalf("custom-fire body failed to compile: %q", body)
	}
	binds := map[string]lua.LValue{"subject": rt.newHandle(subject)}
	for _, o := range opts {
		if o.other != nil {
			binds["other"] = rt.newHandle(o.other)
		}
	}
	if err := rt.invoke(ch, &luaInvocation{actor: firer}, binds); err != nil {
		t.Fatalf("custom-fire body errored: %v", err)
	}
}
