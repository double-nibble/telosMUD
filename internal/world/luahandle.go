package world

import (
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// luahandle.go — the Lua entity-handle layer (docs/PHASE7-PLAN.md slice 7.2, §1.2, threat
// rows T7 + T15). A handle is the ONLY way a script names an entity, and it is a VALIDATED
// reference, never a Go pointer:
//
//   - No dangling references (T7): a handle wraps (rid, zone, zoneGen), NOT an *Entity. Every
//     method re-resolves rid -> *Entity in Go before acting; a dead/departed/cross-zone rid is
//     a safe no-op (nil/false), never a panic, never a stale pointer. The *Entity is fetched,
//     used, and dropped INSIDE the single Go method call — it never lives in a Lua value.
//   - No cross-zone reach (T7): a handle for an entity in another zone does not resolve here
//     (the rid is absent from this zone's containment walk), so its methods no-op. No method
//     ever dereferences a foreign-zone *Entity.
//   - No pointer leak (T15): every handle's metatable defines __tostring returning
//     "<entity #rid>" — NEVER the raw Go pointer the default gopher-lua tostring(ud) leaks
//     (an ASLR-defeat). No handle userdata is ever exposed without a __tostring.
//
// This slice is READ-ONLY: identity/query methods only. No effect ops, no harm surface, no
// mud.* world table — those are slice 7.3. Single-writer: every method runs on the zone
// goroutine (the runtime is only ever entered from Zone.Run via runChunk).

// luaHandleTypeName is the metatable registry key for the entity-handle userdata type.
const luaHandleTypeName = "telos.entity"

// luaHandle is the Go payload of an entity-handle userdata. It is deliberately tiny and holds
// NO *Entity: only the identity needed to re-resolve. zoneGen is captured at handle creation
// (the forward-looking hot-reload guard — see Zone.gen; the bump point is slice 7.7) and
// re-checked on every method, so a handle minted before a reload swap becomes stale without
// any change to this layer.
type luaHandle struct {
	rid     RuntimeID
	zone    *Zone
	zoneGen uint64
}

// installHandleType registers the entity-handle metatable on the runtime's LState, exactly
// once at sandbox build. The metatable carries the curated read methods (via __index) and the
// pointer-safe __tostring (T15). The metatable itself is engine-owned and not exposed as a
// script global; scripts only ever receive userdata values that point at it.
func (rt *luaRuntime) installHandleType() {
	L := rt.L
	mt := L.NewTypeMetatable(luaHandleTypeName)

	// __tostring (T15): a safe, pointer-free representation. Resolution is not required — the
	// rid is enough — so even a stale handle stringifies cleanly without touching an *Entity.
	L.SetField(mt, "__tostring", L.NewFunction(func(l *lua.LState) int {
		h := checkHandle(l, 1)
		if h == nil {
			l.Push(lua.LString("<entity ?>"))
			return 1
		}
		l.Push(lua.LString(fmt.Sprintf("<entity #%d>", h.rid)))
		return 1
	}))

	// __metatable: hide the real metatable behind a locked string so a script (even if it
	// later gains getmetatable, which it does not in the sandbox) cannot reach or mutate the
	// method table. Belt-and-suspenders alongside the dropped getmetatable/setmetatable.
	L.SetField(mt, "__metatable", lua.LString("locked"))

	// __index is the method table — read methods only this slice.
	methods := map[string]lua.LGFunction{
		"id":               rt.hID,
		"name":             rt.hName,
		"short":            rt.hShort,
		"attr":             rt.hAttr,
		"resource":         rt.hResource,
		"level":            rt.hLevel,
		"has_affect":       rt.hHasAffect,
		"affect_magnitude": rt.hAffectMagnitude,
		"has_flag":         rt.hHasFlag,
		"room":             rt.hRoom,
	}
	L.SetField(mt, "__index", L.SetFuncs(L.NewTable(), methods))
}

// newHandle mints a handle userdata for entity e in this runtime's zone. Returns lua.LNil for
// a nil entity (so a query that finds nothing yields nil to the script, the no-result
// convention). The userdata carries only (rid, zone, gen) — never the *Entity.
func (rt *luaRuntime) newHandle(e *Entity) lua.LValue {
	if e == nil || e.zone == nil {
		return lua.LNil
	}
	ud := rt.L.NewUserData()
	ud.Value = &luaHandle{rid: e.rid, zone: e.zone, zoneGen: e.zone.gen}
	rt.L.SetMetatable(ud, rt.L.GetTypeMetatable(luaHandleTypeName))
	return ud
}

// checkHandle extracts the *luaHandle payload from the userdata at stack index n, or nil if
// the value there is not an entity handle. It does NOT resolve the entity — that is
// resolveHandle's job.
func checkHandle(l *lua.LState, n int) *luaHandle {
	ud, ok := l.Get(n).(*lua.LUserData)
	if !ok {
		return nil
	}
	h, ok := ud.Value.(*luaHandle)
	if !ok {
		return nil
	}
	return h
}

// resolveHandle is the re-validation chokepoint (T7): it reads the handle at stack index n and
// re-resolves it to a LIVE *Entity in its zone, or returns nil if the entity is gone, has left
// the zone, or the zone generation has advanced past the handle's. EVERY method calls this
// first; a nil result means the method must no-op (return nil/false). The returned *Entity is
// used and dropped within the calling Go method — it never escapes into a Lua value.
func resolveHandle(l *lua.LState, n int) *Entity {
	h := checkHandle(l, n)
	if h == nil || h.zone == nil {
		return nil
	}
	// Stale-generation guard (the hot-reload seam; gen is never bumped until 7.7, so this is
	// inert today but the check is wired so 7.7 needs no handle-layer change).
	if h.zone.gen != h.zoneGen {
		return nil
	}
	// Re-resolve rid -> *Entity in THIS zone. A cross-zone or departed/dead rid is absent from
	// the zone's containment walk, so this returns nil and the method no-ops — the structural
	// enforcement of no-dangling / no-cross-zone (T7). The *Entity does not outlive this call.
	return h.zone.entityByRID(h.rid)
}

// --- read methods (each: resolve, read, return; no writes, no harm surface) ---------------

// hID returns the entity's RuntimeID as a Lua number. Unlike the other methods it does not
// require the entity to still be live — the id is the handle's own identity — but it still
// validates the handle shape. (A script stores h:id() in self.state and re-resolves later.)
func (rt *luaRuntime) hID(l *lua.LState) int {
	h := checkHandle(l, 1)
	if h == nil {
		l.Push(lua.LNil)
		return 1
	}
	l.Push(lua.LNumber(h.rid))
	return 1
}

// hName returns the entity's display name (short), or nil if the handle no longer resolves.
func (rt *luaRuntime) hName(l *lua.LState) int {
	e := resolveHandle(l, 1)
	if e == nil {
		l.Push(lua.LNil)
		return 1
	}
	l.Push(lua.LString(e.Name()))
	return 1
}

// hShort is an alias of hName (the entity's short/inline name) — kept distinct because the
// API surface (LUA.md §3) names both h:name() and h:short(); a future slice may diverge them.
func (rt *luaRuntime) hShort(l *lua.LState) int {
	e := resolveHandle(l, 1)
	if e == nil {
		l.Push(lua.LNil)
		return 1
	}
	l.Push(lua.LString(e.Name()))
	return 1
}

// hAttr returns the named attribute's derived value as a number, or nil if the handle does
// not resolve. An unknown attribute reads 0 (the bare-engine "no def => 0" invariant), never
// an error.
func (rt *luaRuntime) hAttr(l *lua.LState) int {
	e := resolveHandle(l, 1)
	name := l.CheckString(2)
	if e == nil {
		l.Push(lua.LNil)
		return 1
	}
	l.Push(lua.LNumber(attr(e, name)))
	return 1
}

// hResource returns the named resource's CURRENT value as a number, or nil if the handle does
// not resolve. An unknown/absent resource reads 0 (bare-engine invariant).
func (rt *luaRuntime) hResource(l *lua.LState) int {
	e := resolveHandle(l, 1)
	name := l.CheckString(2)
	if e == nil {
		l.Push(lua.LNil)
		return 1
	}
	l.Push(lua.LNumber(resourceCurrent(e, name)))
	return 1
}

// hLevel returns the entity's level as a number. The engine has no first-class level concept
// yet (Phase 11 progression), so level is read as a "level" attribute if content defines one,
// else 0 — consistent with the bare-engine stat-read convention. Returns nil if the handle
// does not resolve.
func (rt *luaRuntime) hLevel(l *lua.LState) int {
	e := resolveHandle(l, 1)
	if e == nil {
		l.Push(lua.LNil)
		return 1
	}
	l.Push(lua.LNumber(attr(e, "level")))
	return 1
}

// hHasAffect reports whether the entity carries an active affect with the given ref. Returns
// false for an unresolved handle (a departed entity "has" nothing).
func (rt *luaRuntime) hHasAffect(l *lua.LState) int {
	e := resolveHandle(l, 1)
	ref := l.CheckString(2)
	if e == nil {
		l.Push(lua.LFalse)
		return 1
	}
	l.Push(lua.LBool(hasAffect(e, ref)))
	return 1
}

// hAffectMagnitude returns the magnitude of the active affect `ref` on the entity (the largest
// if several instances are active), or 0 if absent / the handle does not resolve.
func (rt *luaRuntime) hAffectMagnitude(l *lua.LState) int {
	e := resolveHandle(l, 1)
	ref := l.CheckString(2)
	if e == nil {
		l.Push(lua.LNumber(0))
		return 1
	}
	l.Push(lua.LNumber(affectMagnitude(e, ref)))
	return 1
}

// hHasFlag reports whether the entity has the named flag set. Returns false for an unresolved
// handle.
func (rt *luaRuntime) hHasFlag(l *lua.LState) int {
	e := resolveHandle(l, 1)
	name := l.CheckString(2)
	if e == nil {
		l.Push(lua.LFalse)
		return 1
	}
	l.Push(lua.LBool(hasFlag(e, name)))
	return 1
}

// hRoom returns a handle to the room the entity is in (its location), or nil if the entity is
// roomless (a freshly-spawned/detached entity) or the handle does not resolve. The returned
// value is itself a validated handle — re-resolved on its own methods.
func (rt *luaRuntime) hRoom(l *lua.LState) int {
	e := resolveHandle(l, 1)
	if e == nil || e.location == nil {
		l.Push(lua.LNil)
		return 1
	}
	l.Push(rt.newHandle(e.location))
	return 1
}

// runChunkWithSelf compiles and runs src in the sandbox with `self` bound to a handle for
// entity e — a TRIVIAL trigger context, just enough to exercise the handle methods from a
// script (slice 7.2). The full entry points (on(...), ability/affect hooks) are slice 7.4;
// this is not those. It sets `self` as a global for the duration of the call and clears it
// after, so one chunk's self cannot leak into an unrelated later call. Re-uses runChunk's
// budget/deadline chokepoint by delegating to it.
func (rt *luaRuntime) runChunkWithSelf(name, src string, e *Entity) error {
	if rt == nil || rt.L == nil {
		return fmt.Errorf("lua runtime not initialized")
	}
	rt.L.SetGlobal("self", rt.newHandle(e))
	defer rt.L.SetGlobal("self", lua.LNil)
	return rt.runChunk(name, src)
}
