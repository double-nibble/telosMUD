package world

import (
	"github.com/double-nibble/telosmud/internal/textsan"
	lua "github.com/yuin/gopher-lua"
)

// luamud.go — the global `mud` world/util table (docs/PHASE7-PLAN.md slice 7.3b, threat rows
// T2/T6/T9/T15). This is the NON-HARM half of the world API: RNG, the deterministic clock,
// structured logging, room scan/broadcast, bounded entity creation, and zone-wheel scheduling.
//
// NO harm surface here (no dealDamage/applyDebuff/guardHarmful) — that is slice 7.3c. Every
// function runs INLINE on the single-writer zone goroutine (the runtime is only entered from
// Zone.Run); mud.after schedules on the zone TIMER WHEEL (pulse.go), never the OS scheduler,
// never a new goroutine (T6). Determinism (T9): mud.random/mud.roll draw the per-zone seeded
// RNG (the same source 7.1 rebound math.random to); no other entropy. The clock (mud.now) is
// the deterministic pulse counter, never wall-clock (T2).

const (
	// luaTimerTypeName is the metatable registry key for the mud.after timer-handle userdata.
	luaTimerTypeName = "telos.timer"

	// luaSpawnPerCallCap bounds how many entities a SINGLE entry-point call may spawn via
	// mud.spawn (T-abuse/DoS — a script must not exhaust the zone). 64 is generous for any
	// legitimate effect (an AoE summon, a loot scatter) while making a spawn-bomb a clean
	// error. Reset at the runChunk chokepoint (per entry-point call), like the instruction
	// count. Tunable.
	luaSpawnPerCallCap = 64

	// luaSpawnPerZoneCap bounds the LIVE script-spawned population a zone may hold at once (the
	// standing population a script is responsible for). It backstops the per-call cap against a
	// script that spawns a few per call across many calls/timers. 1024 is well past any legitimate
	// scripted population in one zone while bounding the worst case. Tunable. As of slice 7.5 this
	// is a LIVE CENSUS (luaSpawnsLive): incremented on a mud.spawn, decremented when such an entity
	// dies/despawns — so a long-lived zone keeps spawning as its Lua-spawned mobs die (it bounds
	// the live population, not the lifetime count).
	luaSpawnPerZoneCap = 1024

	// luaTimerLiveCap bounds the number of LIVE Lua-scheduled mud.after wheel entries (slice 7.5
	// distsys carry-forward). A callback that schedules ≥1 timer each fire would otherwise grow the
	// wheel unboundedly across ticks — bounded by neither the per-call instruction budget nor the
	// spawn cap. Over the cap, mud.after is a clean error. 256 is far past any legitimate scripted
	// scheduling while bounding the wheel. Tunable.
	luaTimerLiveCap = 256
)

// luaTimer is the Go payload of a mud.after timer-handle userdata. It holds the pulse-wheel
// cancel handle and a generation tag (the hot-reload drop seam — wired but inert until 7.7,
// mirroring the zone-gen handle guard). NO *Entity, no closure state beyond the wheel handle.
type luaTimer struct {
	h        *pulseHandle
	chunkGen uint64 // the runtime's chunk generation at scheduling time (7.7 hot-reload drop seam)
	durable  bool   // mud.after{durable=true}: complete even across a reload (a finalizer)
	fired    bool   // the wheel fired/retired it (so cancel doesn't double-decrement the live census)
}

// installMudTable builds the `mud` global and registers the timer-handle userdata type. Called
// once at sandbox build, after the allowlist env is in place. The timer type's metatable
// carries the pointer-safe __tostring (T15 — the 7.2 carry-forward: every new userdata gets a
// <…> form, never 0x…). The mud table itself is read-only (a script cannot clobber mud.spawn).
func (rt *luaRuntime) installMudTable() {
	L := rt.L

	// The timer-handle userdata type + its pointer-safe __tostring (T15).
	tmt := L.NewTypeMetatable(luaTimerTypeName)
	L.SetField(tmt, "__tostring", L.NewFunction(func(l *lua.LState) int {
		l.Push(lua.LString("<timer>"))
		return 1
	}))
	L.SetField(tmt, "__metatable", lua.LString("locked"))

	mud := L.NewTable()
	fns := map[string]lua.LGFunction{
		"random":      rt.mudRandom,
		"roll":        rt.mudRoll,
		"now":         rt.mudNow,
		"log":         rt.mudLog,
		"scan":        rt.mudScan,
		"broadcast":   rt.mudBroadcast,
		"spawn":       rt.mudSpawn,
		"transform":   rt.mudTransform,
		"summon":      rt.mudSummon,
		"after":       rt.mudAfter,
		"cancel":      rt.mudCancel,
		"pvp_allowed": rt.mudPvpAllowed,
		"fire":        rt.mudFire,
	}
	L.SetFuncs(mud, fns)

	// Expose `mud` as a read-only proxy global (a script cannot replace mud.spawn with its own
	// — the same read-only discipline the string/table/math namespaces use). The underlying
	// table is engine-held; only the proxy is reachable.
	g := L.Get(lua.GlobalsIndex).(*lua.LTable)
	g.RawSetString("mud", rt.readOnly(mud))
}

// resetSpawnBudget zeroes the per-call spawn counter. Called at the runChunk chokepoint so each
// entry-point call gets the full per-call spawn cap (mirrors ResetInstructionCount).
func (rt *luaRuntime) resetSpawnBudget() { rt.spawnThisCall = 0 }

// --- RNG + clock (T9 / T2) ----------------------------------------------------------------

// mudRandom mirrors mud.random()/(n)/(m,n) drawing the per-zone seeded RNG (T9). Same
// semantics + same source as the rebound math.random.
func (rt *luaRuntime) mudRandom(l *lua.LState) int { return rt.luaMathRandom(l) }

// mudRoll rolls an engine dice-notation string ("2d6", "4dF", "3d6kh2", "5d10>=7", ...) against
// the per-zone RNG and returns the integer total (T9). The notation is exactly what
// parseDiceSpec accepts (NdS / fudge / keep / pool); a flat "+N" modifier is a formula concern,
// not dice notation, so it is not part of the grammar. A malformed spec is a clean error. It
// builds a minimal effectCtx carrying ONLY the zone + rng (no actor/target/disp) — rollDiceSpec
// reads only c.rng, so this touches no harm path.
func (rt *luaRuntime) mudRoll(l *lua.LState) int {
	spec := l.CheckString(1)
	d, err := parseDiceSpec(spec)
	if err != nil {
		l.RaiseError("mud.roll: %v", err)
		return 0
	}
	total, _ := rollDiceSpec(&effectCtx{z: rt.zone, rng: rt.rng}, d)
	l.Push(lua.LNumber(total))
	return 1
}

// mudNow returns the zone's deterministic pulse counter (T2/T9) — NOT wall-clock. Monotonic,
// reproducible in tests/replays. There is no os.time anywhere.
func (rt *luaRuntime) mudNow(l *lua.LState) int {
	var pulse uint64
	if rt.zone != nil && rt.zone.pulses != nil {
		pulse = rt.zone.pulses.pulse
	}
	l.Push(lua.LNumber(pulse))
	return 1
}

// mudLog routes a structured slog line (the same sink print redirects to). Signature
// mud.log(level, msg) with level one of "debug"/"info"/"warn"/"error" (default info).
func (rt *luaRuntime) mudLog(l *lua.LState) int {
	level := l.OptString(1, "info")
	msg := l.CheckString(2)
	switch level {
	case "debug":
		rt.log.Debug("lua log", "msg", msg)
	case "warn":
		rt.log.Warn("lua log", "msg", msg)
	case "error":
		rt.log.Error("lua log", "msg", msg)
	default:
		rt.log.Info("lua log", "msg", msg)
	}
	return 0
}

// --- room scan / broadcast (read / message; no harm) --------------------------------------

// mudScan returns a table of handles for the occupants + contents of the given room handle
// (mud.scan(room)). An unresolved/non-room handle yields an empty table.
func (rt *luaRuntime) mudScan(l *lua.LState) int {
	room := resolveHandle(l, 1)
	if room == nil {
		return rt.pushHandleList(l, nil)
	}
	return rt.pushHandleList(l, room.contents)
}

// mudBroadcast sends markup to every player session in the given room (mud.broadcast(room,
// markup)). Message-only — no game-state writes. A non-room / unresolved handle no-ops. The
// markup is SCRIPT-SUPPLIED, so it is textsan.CleanMarkup'd at the world boundary (ISSUE-B):
// control/ESC stripped, legitimate markup/color preserved.
func (rt *luaRuntime) mudBroadcast(l *lua.LState) int {
	room := resolveHandle(l, 1)
	markup := textsan.CleanMarkup(l.CheckString(2))
	if room == nil {
		return 0
	}
	for _, occ := range room.contents {
		if pc, ok := sessionOf(occ); ok {
			pc.send(textFrame(markup))
		}
	}
	return 0
}

// --- entity creation / transform (BOUNDED; no harm) ---------------------------------------

// mudSpawn spawns a prototype into a room and returns a handle to the new entity
// (mud.spawn(proto, room)). SECURITY (T-abuse/DoS): bounded by BOTH a per-call cap
// (luaSpawnPerCallCap) and a per-zone standing cap (luaSpawnPerZoneCap) — a script cannot spawn
// unbounded entities to exhaust the zone. Over either cap is a clean Lua error, not a silent
// drop. An unknown prototype or unresolved room handle is a clean nil (no spawn). No harm path.
//
// SECURITY (ISSUE-A, the force-inject guard): the destination MUST be a ROOM — spawning an
// item directly into a player's inventory / a mob / a container would bypass pickup/binding/
// weight rules (Move has no kind check), so a non-room destination is a clean Lua error, never
// a silent inject. And a player-controlled prototype is rejected (a "spawned player" is an
// inert session-less shell, but the invariant is made explicit). Neither rejection counts
// against the spawn budget (like the unknown-proto nil path).
func (rt *luaRuntime) mudSpawn(l *lua.LState) int {
	proto := l.CheckString(1)
	dest := resolveHandle(l, 2)
	if dest == nil {
		l.Push(lua.LNil)
		return 1
	}
	// Destination must be a room (ISSUE-A): no spawning into a player's inventory / a mob / a
	// container through the non-harm mud.spawn. This is a clean error, not a silent inject.
	if !Has[*Room](dest) {
		l.RaiseError("mud.spawn: destination must be a room")
		return 0
	}
	// Reject a player-controlled prototype (ISSUE-A): the engine never spawns a player from a
	// proto; the guard makes that explicit rather than producing an inert shell. Checked BEFORE
	// the budget so it does not consume the spawn budget.
	if rt.zone.protoIsPlayerControlled(ProtoRef(proto)) {
		l.RaiseError("mud.spawn: cannot spawn a player-controlled prototype")
		return 0
	}
	if rt.spawnThisCall >= luaSpawnPerCallCap {
		l.RaiseError("mud.spawn: per-call spawn cap (%d) exceeded", luaSpawnPerCallCap)
		return 0
	}
	// Per-zone cap is now a LIVE CENSUS (slice 7.5): luaSpawnsLive bounds the standing population,
	// decremented when a Lua-spawned entity dies/despawns — so a long-lived zone keeps spawning as
	// its Lua-spawned mobs die (it bounds the live population, not the lifetime count).
	if rt.luaSpawnsLive >= luaSpawnPerZoneCap {
		l.RaiseError("mud.spawn: per-zone LIVE spawn cap (%d) exceeded", luaSpawnPerZoneCap)
		return 0
	}
	e := rt.zone.spawn(ProtoRef(proto))
	if e == nil {
		l.Push(lua.LNil) // unknown prototype: a clean nil, not a counted spawn
		return 1
	}
	rt.spawnThisCall++
	rt.luaSpawnsLive++
	if rt.luaSpawnedRIDs == nil {
		rt.luaSpawnedRIDs = map[RuntimeID]bool{}
	}
	rt.luaSpawnedRIDs[e.rid] = true // tag for the census decrement on death/despawn
	Move(e, dest)
	l.Push(rt.newHandle(e))
	return 1
}

// dropLuaSpawn decrements the live Lua-spawn census when a Lua-spawned entity leaves the world
// (death/despawn). A no-op for an entity that was not Lua-spawned. Called at the same extraction
// chokepoint as dropEntityScript (death.go makeCorpse). Idempotent (the rid is removed from the
// set so a double-call can't double-decrement). Zone goroutine only.
func (rt *luaRuntime) dropLuaSpawn(rid RuntimeID) {
	if rt == nil || rt.luaSpawnedRIDs == nil || !rt.luaSpawnedRIDs[rid] {
		return
	}
	delete(rt.luaSpawnedRIDs, rid)
	if rt.luaSpawnsLive > 0 {
		rt.luaSpawnsLive--
	}
}

// mudTransform re-points an existing entity at a new prototype (mud.transform(h, proto)) — a
// reskin/morph, NOT a spawn, so it is not spawn-capped (it creates no new entity). For 7.3b
// this is a RESERVED STUB: the engine has no in-place prototype-swap primitive yet (it is a
// COW-aware operation the morph follow-up owns), so it validates the handle and returns it
// unchanged, never inventing a partial swap. Returns nil for an unresolved handle.
func (rt *luaRuntime) mudTransform(l *lua.LState) int {
	e := resolveHandle(l, 1)
	_ = l.CheckString(2) // proto: validated as a string; the swap primitive is a follow-up
	if e == nil {
		l.Push(lua.LNil)
		return 1
	}
	rt.log.Debug("mud.transform (reserved stub; no-op until the morph primitive lands)", "rid", e.rid)
	l.Push(rt.newHandle(e))
	return 1
}

// mudSummon moves an entity to the caller's room (mud.summon(h)) — a teleport-to-here. For
// 7.3b this is a RESERVED STUB: cross-room movement of an arbitrary target is a 7.3c-adjacent
// capability (it can pull a player), so it is deferred to the harm/movement slice rather than
// landing in the non-harm half. It validates the handle and no-ops. Returns false.
func (rt *luaRuntime) mudSummon(l *lua.LState) int {
	e := resolveHandle(l, 1)
	if e == nil {
		l.Push(lua.LFalse)
		return 1
	}
	rt.log.Debug("mud.summon (reserved; deferred to the movement slice)", "rid", e.rid)
	l.Push(lua.LFalse)
	return 1
}

// --- zone-wheel scheduling (T6 — never a goroutine, never a real sleep) -------------------

// mudAfter schedules a Lua callback to fire `pulses` heartbeats from now ON THE ZONE TIMER
// WHEEL (pulse.go after), NOT the OS scheduler and NOT a new goroutine — the callback runs
// INLINE on the zone goroutine, with the same single-writer access a command has (T6). It
// returns a timer-handle userdata (cancellable via mud.cancel) carrying a pointer-safe
// __tostring (T15). The callback is invoked through the chokepoint (pcall-isolated, slice 7.5).
//
// HOT-RELOAD DROP (slice 7.7, P7-D7): the callback captures the runtime's chunk generation at
// SCHEDULE time; on a content reload (chunkFor bumps chunkGen) a pending old-gen timer is a clean
// NO-OP at fire — it never runs OLD code against NEW state. The OPT-IN `mud.after(pulses, fn,
// {durable=true})` exempts a timer from the drop (a state-cleanup finalizer that must complete
// even across a reload — release a held resource, clear a flag).
func (rt *luaRuntime) mudAfter(l *lua.LState) int {
	pulses := l.CheckInt(1)
	fn := l.CheckFunction(2)
	if pulses < 1 {
		pulses = 1
	}
	// Optional opts table (arg 3): {durable=true} exempts the timer from the hot-reload drop.
	durable := false
	if l.GetTop() >= 3 {
		if opts, ok := l.Get(3).(*lua.LTable); ok {
			durable = lua.LVAsBool(opts.RawGetString("durable"))
		}
	}
	if rt.zone == nil || rt.zone.pulses == nil {
		l.Push(lua.LNil)
		return 1
	}
	// LIVE-TIMER CAP (slice 7.5): a callback that schedules ≥1 timer each fire would grow the wheel
	// unboundedly across ticks. Over the cap, this is a clean error (not a silent drop).
	if rt.luaTimersLive >= luaTimerLiveCap {
		l.RaiseError("mud.after: live timer cap (%d) exceeded", luaTimerLiveCap)
		return 0
	}
	gen := rt.chunkGen
	rt.luaTimersLive++
	t := &luaTimer{chunkGen: gen, durable: durable}
	handle := rt.zone.pulses.after(uint64(pulses), func(uint64) bool {
		// The wheel calls this ON THE ZONE GOROUTINE. The timer is firing (and retiring), so it
		// leaves the live census BEFORE the callback runs — so a callback that re-schedules nets to
		// the same count, not +1 each tick. The live count decrements exactly once per timer.
		t.fired = true
		rt.luaTimersLive--
		// HOT-RELOAD DROP (P7-D7): an old-gen callback is dropped (don't run old code against new
		// state) UNLESS it opted into durable=true (a finalizer that must complete).
		if gen != rt.chunkGen && !durable {
			rt.log.Debug("mud.after callback dropped (old chunk generation, hot-reload)", "sched_gen", gen, "cur_gen", rt.chunkGen)
			return false
		}
		rt.runCallback("mud.after", fn)
		return false // one-shot
	})
	t.h = handle
	ud := l.NewUserData()
	ud.Value = t
	l.SetMetatable(ud, l.GetTypeMetatable(luaTimerTypeName))
	l.Push(ud)
	return 1
}

// mudCancel cancels a mud.after timer by its handle (mud.cancel(timer)). A non-timer / already-
// fired handle is a safe no-op. Cancelling an UNFIRED timer frees its live-census slot (the wheel
// will never fire it, so its decrement must happen here instead).
func (rt *luaRuntime) mudCancel(l *lua.LState) int {
	ud, ok := l.Get(1).(*lua.LUserData)
	if !ok {
		return 0
	}
	t, ok := ud.Value.(*luaTimer)
	if !ok || t.h == nil {
		return 0
	}
	if !t.fired {
		t.fired = true // latch so a double-cancel can't double-decrement
		if rt.luaTimersLive > 0 {
			rt.luaTimersLive--
		}
	}
	t.h.cancel()
	return 0
}

// --- pvp policy query (read-only) ---------------------------------------------------------

// mudPvpAllowed is a READ-ONLY query of the existing PvP gate (mud.pvp_allowed(a, b)): may a
// harm `a` causes land on `b`? It calls pvpAllowed, which is a pure read of zone/flag state —
// no side effect, no harm applied (that is the gate's whole point). Returns false for an
// unresolved actor/target.
func (rt *luaRuntime) mudPvpAllowed(l *lua.LState) int {
	a := resolveHandle(l, 1)
	b := resolveHandle(l, 2)
	if a == nil || b == nil {
		l.Push(lua.LFalse)
		return 1
	}
	l.Push(lua.LBool(pvpAllowed(a, b)))
	return 1
}

// --- custom-event lane (the pack: hookability obligation) ---------------------------------

// mudFire fires a CONTENT-NAMESPACED custom event the engine never heard of (the pack: lane,
// PHASE7-PLAN.md §5 7.8a): mud.fire("sailing:OnShipDock", subject, data). A sailing/quest system
// defines, fires, and handles its own events ENTIRELY in content. The event has NO privileged
// status — it goes through the SAME z.fireEvent path engine kinds use, so it inherits the depth
// (maxEventDepth re-entrancy) + width (maxEventHandlers) budget AND the harm gate (a harmful op in
// the subscribed handler funnels guardHarmful through the threaded cascade ctx, invariant 1).
//
// SECURITY / NAMESPACE (7.8a): the kind MUST be a namespaced custom kind ("<pack>:Name") — a bare
// name with no separator is an ENGINE kind, and firing an arbitrary bare name is REJECTED (a clean
// error). This keeps the lane from becoming a hole where any string is a valid engine event, and the
// pack prefix namespaces it so two packs' same-named events never collide. An unknown bare engine
// kind (e.g. mud.fire("OnTotallyMadeUp", …)) is likewise rejected.
//
// The cascade is threaded FROM rt.inv (the current invocation): if mud.fire is called from inside an
// event handler, the SAME depth + shared eventBudget pointer thread through, so a handler that
// re-fires is bounded by the same caps it ran under (no escape). Fired from a top-level trigger
// (rt.inv with nil eventBudget), z.fireEvent allocates the cascade's root budget. An unresolved
// subject is a clean no-op. The data table (arg 3, optional) is bound as `ev.data` on the handler.
func (rt *luaRuntime) mudFire(l *lua.LState) int {
	name := eventKind(l.CheckString(1))
	subject := resolveHandle(l, 2)
	// NAMESPACE GATE: only a namespaced custom kind may be fired from content. A bare name is an
	// engine kind whose fire points the ENGINE owns — content may not synthesize one (and a bare name
	// not even in knownEventKinds is a typo/abuse). Reject cleanly, never silently.
	if !isCustomEventKind(name) {
		l.RaiseError("mud.fire: %q is not a namespaced custom event (use \"<pack>:Name\"); engine events are fired by the engine, not content", string(name))
		return 0
	}
	if subject == nil || rt.zone == nil {
		return 0 // unresolved subject: a clean no-op (the firer doesn't know who, if anyone, subscribes)
	}
	// Optional data table (arg 3): the firer's arbitrary plain-data payload, bound as ev.data on the
	// handler. Threaded via rt.fireData (NOT the engine fireEvent signature). Save/restore around the
	// fire so a NESTED custom fire (a handler that fires another) restores the outer firer's data.
	var data *lua.LTable
	if l.GetTop() >= 3 {
		if t, ok := l.Get(3).(*lua.LTable); ok {
			data = t
		}
	}
	prevData := rt.fireData
	rt.fireData = data
	defer func() { rt.fireData = prevData }()

	// Build the cascade ctx FROM the current invocation (invariant 1/5): the subject is the event
	// subject (who it is ABOUT); the threaded depth/eventBudget come from rt.inv so a fire from inside
	// a handler shares the cascade's caps. A top-level trigger fire (nil eventBudget) starts a fresh
	// cascade in z.fireEvent.
	parent := &effectCtx{z: rt.zone, actor: subject, source: subject, rng: rt.rng}
	if rt.inv != nil {
		parent.depth = rt.inv.depth
		parent.eventBudget = rt.inv.eventBudget
	}
	rt.zone.fireEvent(parent, name, subject, nil, 1)
	return 0
}

// runCallback invokes a stored Lua function pcall-isolated, logging (not propagating) any
// error — a buggy timer/callback fails just itself, never the zone (the T11 isolation shape;
// the full breaker is 7.5). It re-arms the per-call deadline + budget so a callback is bounded
// like a top-level entry (the chokepoint slice 7.5 generalizes). Single-writer: zone goroutine.
func (rt *luaRuntime) runCallback(what string, fn *lua.LFunction) {
	if rt == nil || rt.L == nil || fn == nil {
		return
	}
	if err := rt.callFn(fn); err != nil {
		rt.log.Warn("lua callback error (isolated; zone unaffected)", "where", what, "err", err.Error())
	}
}

// callFn runs fn() through THE chokepoint (pcallGuarded) — so a mud.after timer callback is
// bounded by the deadline + instruction count + spawn budget exactly like a top-level entry, and
// is breaker-tracked. Returns the pcall error if any. The breaker key is the shared "mud.after"
// origin (a runaway timer callback quarantines the timer surface, not the whole zone).
func (rt *luaRuntime) callFn(fn *lua.LFunction) error {
	return rt.runGuardedFn(breakerKeyShared("mud.after"), "mud.after", fn, 0, lua.MultRet)
}
