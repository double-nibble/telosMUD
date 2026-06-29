package world

import (
	lua "github.com/yuin/gopher-lua"

	"github.com/double-nibble/telosmud/internal/textsan"
)

// luaentry_triggers.go — the per-entity TRIGGER machinery (slice 7.4c): `on(event, fn)`
// registration + the per-instance self.state, and the engine fire points.
//
// Lifecycle of a scripted entity's triggers:
//  1. At first need, ensureEntityScript runs the entity's *Scripted source ONCE in a context
//     where the global `on(event, fn)` registers a handler into THIS entity's handler table and
//     `self` / `self.state` are bound. The registration run is itself pcall-isolated (a broken
//     trigger block leaves the entity with whatever registered before the error — fail-closed).
//  2. fireTrigger looks up the handler for an event and invokes it via invokeFromCtx, threading
//     the firing cascade's budget (invariant 1) and binding `self`/`ev` in the fresh per-call env
//     (invariant 2). A handler error fizzles just that fire (invariant 3).
//
// self.state is IN-MEMORY at 7.4c (a Lua table on the entityScript). The ↔ JSONB persistence is
// slice 7.6 — the seam: persist entityScript.state's data subtree into StateJSON.Script and
// re-hydrate it here at spawn-from-snapshot.

// entityScript is one scripted entity's per-instance runtime state: its registered event
// handlers and its self.state table, both bound to this zone's LState.
type entityScript struct {
	handlers *lua.LTable // event-name (string) -> handler (*lua.LFunction)
	state    *lua.LTable // self.state (in-memory at 7.4c; JSONB persistence is 7.6)
	rid      RuntimeID   // the owning entity (for self re-resolution + diagnostics)
	failed   bool        // the registration run errored; the entity has only partial/zero handlers
}

// ensureEntityScript returns (building if needed) the per-instance trigger state for e, or nil if
// e carries no *Scripted source. The registration run binds a per-run `on` that registers into
// the new handler table; `self` resolves to e and `self.state` to the fresh state table. Pcall-
// isolated + budget-armed (the registration is a script call like any other).
func (rt *luaRuntime) ensureEntityScript(e *Entity) *entityScript {
	if rt == nil || rt.L == nil || e == nil {
		return nil
	}
	if es, ok := rt.entityScripts[e.rid]; ok {
		return es // already built (possibly failed; not retried)
	}
	src := scriptSource(e)
	if src == "" {
		return nil
	}
	return rt.registerEntityScript(e, src, nil)
}

// registerEntityScript builds (or rebuilds) an entityScript for e from the EXPLICIT source `src`,
// running the registration body so `on(event, fn)` populates a fresh handler table. If `keepState`
// is non-nil it is re-used as the self.state table (the hot-reload path passes the existing state
// so the DATA survives the code swap, P7-D7 / §1.1); otherwise a fresh empty state is created.
// Stores and returns the new entityScript. Zone goroutine only.
func (rt *luaRuntime) registerEntityScript(e *Entity, src string, keepState *lua.LTable) *entityScript {
	L := rt.L
	state := keepState
	if state == nil {
		state = L.NewTable()
	}
	es := &entityScript{handlers: L.NewTable(), state: state, rid: e.rid}
	rt.entityScripts[e.rid] = es

	ch := rt.chunkFor("trigger:"+string(e.proto)+":register", src)
	if ch == nil {
		es.failed = true
		return es // compile failed / empty: inert
	}

	// The registration run: `on(event, fn)` registers into es.handlers; `self` is e and `state`
	// (== self.state) is es.state. A clean root invocation (the registration is not inside a
	// cascade) — it registers handlers, it does not itself harm.
	self := rt.newHandle(e)
	onFn := L.NewFunction(func(l *lua.LState) int {
		event := l.CheckString(1)
		fn := l.CheckFunction(2)
		es.handlers.RawSetString(event, fn)
		return 0
	})
	// on_world / on_region (Phase 10.4b): register a reaction to a director's REMOTE-EFFECT broadcast on
	// the world / this zone's region scope. Stored in the SAME handler table under a namespaced key
	// ("world:<event>" / "region:<event>") so the per-instance lifecycle (drop on despawn, rebuild on hot
	// reload) is shared with ordinary triggers — fireScopeEvent (scope.go) fires them when a broadcast
	// arrives. A region handler on a region-less zone simply never fires (no region broadcast reaches it).
	onWorld := L.NewFunction(func(l *lua.LState) int {
		es.handlers.RawSetString("world:"+l.CheckString(1), l.CheckFunction(2))
		return 0
	})
	onRegion := L.NewFunction(func(l *lua.LState) int {
		es.handlers.RawSetString("region:"+l.CheckString(1), l.CheckFunction(2))
		return 0
	})
	binds := map[string]lua.LValue{
		"on":        onFn,
		"on_world":  onWorld,
		"on_region": onRegion,
		"self":      self,
		"state":     es.state, // `state` global == self.state during registration + handlers
	}
	inv := &luaInvocation{actor: e} // clean root: registration acts as the entity, no cascade budget
	if err := rt.invoke(ch, inv, binds); err != nil {
		es.failed = true // partial registration; whatever registered before the error stands
	}
	return es
}

// ensureStateTable returns (creating if needed) the per-instance self.state table for entity e,
// WITHOUT requiring a *Scripted source. A scripted entity's state lives on its entityScript (built
// by ensureEntityScript); a player (or any entity) that has no trigger source but DOES carry
// self.state (a quest counter written by an ability/quest hook, or rehydrated from a save) gets a
// minimal entityScript here holding just the state table (no handlers). This is the seam the 7.6
// persistence load uses to install a rehydrated state, and the accessor a future quest hook uses to
// read/write a player's self.state. Zone goroutine only.
func (rt *luaRuntime) ensureStateTable(e *Entity) *lua.LTable {
	if rt == nil || rt.L == nil || e == nil {
		return nil
	}
	if es := rt.ensureEntityScript(e); es != nil {
		return es.state // a scripted entity already has one
	}
	// No *Scripted source: a state-only entityScript (handlers empty).
	es, ok := rt.entityScripts[e.rid]
	if !ok {
		es = &entityScript{handlers: rt.L.NewTable(), state: rt.L.NewTable(), rid: e.rid}
		rt.entityScripts[e.rid] = es
	}
	return es.state
}

// setStateTable installs a (rehydrated, plain-data) state table as entity e's self.state — used by
// the 7.6 load path. It replaces the entity's entityScript.state with `t`, creating a state-only
// entityScript if none exists. Zone goroutine only.
func (rt *luaRuntime) setStateTable(e *Entity, t *lua.LTable) {
	if rt == nil || rt.L == nil || e == nil || t == nil {
		return
	}
	es, ok := rt.entityScripts[e.rid]
	if !ok {
		es = &entityScript{handlers: rt.L.NewTable(), rid: e.rid}
		rt.entityScripts[e.rid] = es
	}
	es.state = t
}

// reloadEntityScriptsForProto re-registers the trigger handlers for every LIVE instance of proto
// `ref` from its NEW (post-swap) Scripted source, while PRESERVING each instance's self.state table
// (slice 7.7 hot reload — P7-D7 / §1.1: swap the CHUNK/handlers, keep the data). The new source is
// read from the SWAPPED prototype in the cache (a live instance still aliases its OLD prototype's
// component, so protoScriptSource gives the edit). Zone goroutine only.
func (rt *luaRuntime) reloadEntityScriptsForProto(ref ProtoRef) {
	if rt == nil || rt.zone == nil {
		return
	}
	newSrc := rt.zone.protoScriptSource(ref)
	for rid, es := range rt.entityScripts {
		if es == nil {
			continue
		}
		e := rt.zone.entityByRID(rid)
		if e == nil || e.proto != ref {
			continue // not a live instance of the reloaded proto
		}
		state := es.state
		delete(rt.entityScripts, rid)
		if newSrc == "" {
			continue // the reloaded proto dropped its script; the instance becomes scriptless
		}
		// A fix-reload re-enables a per-INSTANCE-quarantined trigger: clear this rid's breaker so the
		// corrected script runs immediately instead of staying inert until the instance repops (the
		// shared (kind,ref) breaker is reset in chunkFor; per-instance keys were previously left tripped
		// — 7.7 security-review follow-up). Availability-only, fail-toward-inert: a still-broken script
		// just re-trips on its next fire.
		rt.breakerReset(breakerKeyInstance(rid))
		// Rebuild handlers from the NEW source, KEEPING the existing self.state table (the DATA
		// survives the code swap). chunkFor recompiles because the source changed.
		rt.registerEntityScript(e, newSrc, state)
	}
}

// dropEntityScript removes the per-instance trigger state (handler table + self.state) for the
// entity with this rid, freeing the *lua.LTable values it held on the zone's LState. It MUST be
// called when a scripted entity leaves the world tree for good (death->corpse, a non-corpse reap)
// — otherwise a repop-on-timer zone leaks an entityScript per spawned-and-died mob forever (the
// RuntimeID allocator is monotonic, so a dropped rid is never reused: this is a pure leak, not a
// correctness hazard). Idempotent + nil-safe.
//
// 7.6 SEAM: when self.state persistence lands, despawn becomes "FLUSH es.state's data subtree to
// StateJSON.Script, THEN drop the in-memory entry." The drop is wired here now; 7.6 adds the flush
// immediately before it.
func (rt *luaRuntime) dropEntityScript(rid RuntimeID) {
	if rt == nil || rt.entityScripts == nil {
		return
	}
	delete(rt.entityScripts, rid)
}

// fireTrigger fires event `event` on scripted entity `e`, if it has a handler for it. The handler
// runs via invokeFromCtx threading the firing cascade ctx `c` (invariant 1) — for a trigger fired
// OUTSIDE a cascade (a plain enter/greet), `c` is a clean root ctx the caller builds (depth 0,
// nil budget), documented at each fire point. `ev` is the event table (actor/text/…) bound as the
// `ev` global. `self`/`state` are bound fresh per call. A missing handler / failed registration
// is a clean no-op; a handler error fizzles just this fire.
func (rt *luaRuntime) fireTrigger(e *Entity, event string, c *effectCtx, ev *lua.LTable) {
	if rt == nil || rt.L == nil || e == nil {
		return
	}
	es := rt.ensureEntityScript(e)
	if es == nil {
		return
	}
	h, ok := es.handlers.RawGetString(event).(*lua.LFunction)
	if !ok {
		return // no handler for this event
	}
	binds := map[string]lua.LValue{
		"self":  rt.newHandle(e),
		"state": es.state,
		"ev":    ev,
	}
	rt.fireTriggerCall(es, h, c, binds, ev)
}

// fireTriggerCall invokes a stored handler *LFunction directly (not through a compiled proto),
// under the same budget/deadline + pcall chokepoint + the threaded invocation. It binds the
// handler's environment to a fresh per-call env (invariant 2) so `self`/`state`/`ev` resolve and
// nothing leaks, threads the firing ctx's budget into rt.inv (invariant 1), and is pcall-isolated
// (invariant 3).
func (rt *luaRuntime) fireTriggerCall(es *entityScript, h *lua.LFunction, c *effectCtx, binds map[string]lua.LValue, ev *lua.LTable) {
	// PER-INSTANCE breaker (P7-D10 (a)): a trigger is entity-scoped, so one buggy mob instance is
	// quarantined, not its whole prototype. Key by the instance rid.
	key := breakerKeyInstance(es.rid)
	if rt.breakerDisabled(key) {
		return // quarantined instance: a clean no-op
	}
	L := rt.L
	// Fresh per-call env for the handler (so self/state/ev resolve via __index fall-through to the
	// sandbox globals, and any global write is discarded). NOTE: setting h.Env here is the handle
	// for binding the per-call env; the handler is a stored closure, so its Env is swapped each
	// fire (single-writer: only the zone goroutine fires it, never concurrently).
	env := rt.freshCallEnv(binds)
	h.Env = env

	inv := &luaInvocation{actor: c.actor, depth: c.depth, eventBudget: c.eventBudget, breakerKey: key}
	prev := rt.inv
	rt.inv = inv
	defer func() { rt.inv = prev }()

	// THE chokepoint: push the handler + its ev arg, run guarded. The handler signature is
	// on(event, function(ev) ... end) — one argument, no return.
	L.Push(h)
	L.Push(ev)
	if err := rt.pcallGuarded(key, "trigger:#"+ridStr(es.rid), 1, 0); err != nil {
		rt.log.Warn("lua trigger error (isolated; action fizzled, zone unaffected)",
			"event", "trigger", "rid", es.rid, "err", err.Error())
	}
}

// --- event tables ---------------------------------------------------------------------------

// evTable builds the `ev` table a trigger handler reads. actor is the other party (the entrant,
// the speaker); text is a message (speech). Both optional. The actor is a validated handle; the
// text is textsan-cleaned (it can be raw player speech).
func (rt *luaRuntime) evTable(actor *Entity, text string) *lua.LTable {
	t := rt.L.NewTable()
	if actor != nil {
		t.RawSetString("actor", rt.newHandle(actor))
	}
	if text != "" {
		t.RawSetString("text", lua.LString(textsan.CleanMarkup(text)))
	}
	return t
}

// rootCtx builds a clean ROOT effectCtx for a trigger fired OUTSIDE an event cascade (a plain
// enter/greet/leave from the movement path). depth 0, nil eventBudget — exactly like a command-
// issued action (effect_op.go). actor is the scripted entity (who the trigger acts as). A trigger
// fired INSIDE a cascade instead threads that cascade's ctx (the caller passes it).
func (rt *luaRuntime) rootCtx(actor *Entity) *effectCtx {
	return &effectCtx{z: rt.zone, actor: actor, source: actor, mag: 1, disp: dispNeutral, rng: rt.rng}
}

// --- engine fire points (the lifecycle hooks that fire triggers) ---------------------------

// fireRoomEntry fires the movement-entry triggers when `entrant` arrives in `room` (slice 7.4c):
//   - `enter` on the ROOM entity (ev.actor = the entrant) — a room reacting to who walks in.
//   - `greet` on each scripted MOB already in the room (ev.actor = the entrant) — a guard
//     greeting an arrival. The entrant itself and items are skipped.
//
// Each fire uses a clean ROOT ctx whose actor is the SCRIPTED entity (the room / the greeting
// mob) — so a harm op in the trigger is attributed to the script owner, gated per 7.3c. These
// fires are OUTSIDE any event cascade (a movement, not a fired event), so a root ctx is correct;
// a trigger that itself fires the event bus starts its own cascade with its own budget. nil-safe.
func (z *Zone) fireRoomEntry(entrant, room *Entity) {
	if z == nil || z.lua == nil || entrant == nil || room == nil {
		return
	}
	rt := z.lua
	// `enter` on the room.
	if Has[*Scripted](room) {
		rt.fireTrigger(room, "enter", rt.rootCtx(room), rt.evTable(entrant, ""))
	}
	// `greet` on each scripted mob in the room (not the entrant, not items).
	for _, occ := range room.contents {
		if occ == entrant || occ == nil || occ.living == nil {
			continue
		}
		if Has[*Scripted](occ) {
			rt.fireTrigger(occ, "greet", rt.rootCtx(occ), rt.evTable(entrant, ""))
		}
	}
}

// fireRoomLeave fires the `leave` trigger on the room when `leaver` departs (ev.actor = leaver).
// Fired BEFORE the entity detaches so the room can still see it. nil-safe.
func (z *Zone) fireRoomLeave(leaver, room *Entity) {
	if z == nil || z.lua == nil || leaver == nil || room == nil || !Has[*Scripted](room) {
		return
	}
	rt := z.lua
	rt.fireTrigger(room, "leave", rt.rootCtx(room), rt.evTable(leaver, ""))
}

// fireDeath fires the `death` trigger on a dying scripted entity (ev.actor = the killer, may be
// nil). Fired at the death point BEFORE the corpse/reap removes the victim from the world tree, so
// the handler still sees the entity in-room (it can `self:say` a death cry, drop a quest flag,
// etc.). A clean ROOT ctx whose actor is the dying entity. nil-safe / no-op when unscripted.
func (z *Zone) fireDeath(victim, killer *Entity) {
	if z == nil || z.lua == nil || victim == nil || !Has[*Scripted](victim) {
		return
	}
	rt := z.lua
	rt.fireTrigger(victim, "death", rt.rootCtx(victim), rt.evTable(killer, ""))
}

// fireSpeech fires the `speech` trigger on each scripted mob in `speaker`'s room when `speaker`
// says `text` (ev.actor = speaker, ev.text = the spoken text). The speaker itself is skipped.
// nil-safe.
func (z *Zone) fireSpeech(speaker *Entity, text string) {
	if z == nil || z.lua == nil || speaker == nil || speaker.location == nil {
		return
	}
	rt := z.lua
	for _, occ := range speaker.location.contents {
		if occ == speaker || occ == nil || occ.living == nil {
			continue
		}
		if Has[*Scripted](occ) {
			rt.fireTrigger(occ, "speech", rt.rootCtx(occ), rt.evTable(speaker, text))
		}
	}
}
