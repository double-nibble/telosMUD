package world

import (
	"fmt"

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

	// luaSpawnPerZoneCap bounds the TOTAL live script-spawned entities a zone may hold at once
	// (the standing population a script is responsible for). It backstops the per-call cap
	// against a script that spawns a few per call across many calls/timers. 1024 is well past
	// any legitimate scripted population in one zone while bounding the worst case. Tunable.
	// (This is a soft accounting cap — it counts spawns, not a live census; a future slice can
	// decrement on despawn. For 7.3b it is the monotonic-since-build spawn budget, documented.)
	luaSpawnPerZoneCap = 1024
)

// luaTimer is the Go payload of a mud.after timer-handle userdata. It holds the pulse-wheel
// cancel handle and a generation tag (the hot-reload drop seam — wired but inert until 7.7,
// mirroring the zone-gen handle guard). NO *Entity, no closure state beyond the wheel handle.
type luaTimer struct {
	h        *pulseHandle
	chunkGen uint64 // the runtime's chunk generation at scheduling time (7.7 drop seam)
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
	if rt.spawnTotal >= luaSpawnPerZoneCap {
		l.RaiseError("mud.spawn: per-zone spawn cap (%d) exceeded", luaSpawnPerZoneCap)
		return 0
	}
	e := rt.zone.spawn(ProtoRef(proto))
	if e == nil {
		l.Push(lua.LNil) // unknown prototype: a clean nil, not a counted spawn
		return 1
	}
	rt.spawnThisCall++
	rt.spawnTotal++
	Move(e, dest)
	l.Push(rt.newHandle(e))
	return 1
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
// __tostring (T15). The callback is invoked through runChunkFn (pcall-isolated); the FULL
// per-call budget/deadline chokepoint over timer callbacks is slice 7.5 — here it is pcall-
// wrapped and on the wheel, which is the 7.3b done-when.
func (rt *luaRuntime) mudAfter(l *lua.LState) int {
	pulses := l.CheckInt(1)
	fn := l.CheckFunction(2)
	if pulses < 1 {
		pulses = 1
	}
	if rt.zone == nil || rt.zone.pulses == nil {
		l.Push(lua.LNil)
		return 1
	}
	gen := rt.chunkGen
	handle := rt.zone.pulses.after(uint64(pulses), func(uint64) bool {
		// The wheel calls this ON THE ZONE GOROUTINE. Run the Lua callback pcall-isolated. The
		// generation guard (inert until 7.7) will drop a callback bound to a swapped chunk.
		if gen != rt.chunkGen {
			return false
		}
		rt.runCallback("mud.after", fn)
		return false // one-shot
	})
	ud := l.NewUserData()
	ud.Value = &luaTimer{h: handle, chunkGen: gen}
	l.SetMetatable(ud, l.GetTypeMetatable(luaTimerTypeName))
	l.Push(ud)
	return 1
}

// mudCancel cancels a mud.after timer by its handle (mud.cancel(timer)). A non-timer / already-
// fired handle is a safe no-op.
func (rt *luaRuntime) mudCancel(l *lua.LState) int {
	ud, ok := l.Get(1).(*lua.LUserData)
	if !ok {
		return 0
	}
	t, ok := ud.Value.(*luaTimer)
	if !ok || t.h == nil {
		return 0
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

// callFn runs fn() through the same fresh-deadline + instruction-budget + pcall chokepoint as
// runChunk (so a timer callback is bounded exactly like a top-level entry). Returns the pcall
// error if any.
func (rt *luaRuntime) callFn(fn *lua.LFunction) error {
	L := rt.L
	ctx, cancel := contextWithLuaDeadline()
	defer cancel()
	L.SetContext(ctx)
	L.ResetInstructionCount()
	rt.resetSpawnBudget()
	defer L.RemoveContext()
	L.Push(fn)
	if err := L.PCall(0, lua.MultRet, nil); err != nil {
		return fmt.Errorf("lua callback: %w", err)
	}
	return nil
}
