package world

import (
	"encoding/json"

	lua "github.com/yuin/gopher-lua"
)

// luascope.go — the Lua READ surface over a zone's region/world scope replica (docs/WORLD-EVENTS.md §5,
// Phase 10.3b). A zone script reads supra-zone state synchronously and lock-free (the replica is updated
// only by the director's broadcast, applied on this same zone goroutine — scope.go):
//
//	world.flag("invasion_active")   -- is a world flag set (truthy)?
//	world.get("invasion_phase")     -- a world value (number/string/bool/table) or nil
//	region:get("mood")              -- a value from THIS zone's region, or nil (nil if region-less)
//
// These are READS ONLY. A script never mutates region/world state here; it signals UP to the director
// (signal_region/signal_world, 10.3c), which is the single writer. Both tables are read-only globals (a
// script cannot clobber world.flag), the same discipline as the `mud` table.

// installScopeTables registers the `world` and `region` read-only globals. Called once at sandbox build,
// after installMudTable. The functions close over rt, reading rt.zone.scopes on the zone goroutine.
func (rt *luaRuntime) installScopeTables() {
	L := rt.L

	world := L.NewTable()
	L.SetFuncs(world, map[string]lua.LGFunction{
		"flag": rt.scopeWorldFlag,
		"get":  rt.scopeWorldGet,
	})

	region := L.NewTable()
	L.SetFuncs(region, map[string]lua.LGFunction{
		"get": rt.scopeRegionGet,
		"id":  rt.scopeRegionID,
	})

	g := L.Get(lua.GlobalsIndex).(*lua.LTable)
	g.RawSetString("world", rt.readOnly(world))
	g.RawSetString("region", rt.readOnly(region))
}

// scopeKey reads the key argument tolerating both call forms: world.get("k") / region.get("k") (key at
// arg 1) and region:get("k") (colon desugars to region.get(region, "k"), key at arg 2). Returns "" if no
// string key is present (the caller then returns nil/false).
func scopeKey(l *lua.LState) string {
	// If arg 1 is a table (the colon `self`), the key is arg 2; else the key is arg 1.
	if _, ok := l.Get(1).(*lua.LTable); ok {
		return l.OptString(2, "")
	}
	return l.OptString(1, "")
}

// rawValue returns the stored JSON for key in the named scope ("world"/"region"), or nil if absent. nil
// rt/zone (a standalone test runtime) yields nil — every read is then "unset".
func (rt *luaRuntime) rawValue(scope, key string) json.RawMessage {
	if rt.zone == nil || rt.zone.scopes == nil || key == "" {
		return nil
	}
	switch scope {
	case "world":
		return rt.zone.scopes.world[key]
	case "region":
		if rt.zone.scopes.regionID == "" {
			return nil
		}
		return rt.zone.scopes.region[key]
	}
	return nil
}

// pushScopeValue decodes raw JSON to a natural Lua value and pushes it (nil if absent/malformed). Uses
// the same goToLua converter as self.state, so a scope value is a number/string/bool/table exactly as
// the director set it.
func (rt *luaRuntime) pushScopeValue(l *lua.LState, raw json.RawMessage) int {
	if len(raw) == 0 {
		l.Push(lua.LNil)
		return 1
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		l.Push(lua.LNil)
		return 1
	}
	l.Push(rt.goToLua(v, 0))
	return 1
}

// scopeWorldFlag implements world.flag(name) -> bool: true iff the world key is set to a TRUTHY value
// (present and not JSON false/null). A missing flag is false. This is the common "is the event on" read.
func (rt *luaRuntime) scopeWorldFlag(l *lua.LState) int {
	raw := rt.rawValue("world", scopeKey(l))
	l.Push(lua.LBool(jsonTruthy(raw)))
	return 1
}

// scopeWorldGet implements world.get(name) -> value|nil.
func (rt *luaRuntime) scopeWorldGet(l *lua.LState) int {
	return rt.pushScopeValue(l, rt.rawValue("world", scopeKey(l)))
}

// scopeRegionGet implements region:get(name) -> value|nil (nil on a region-less zone).
func (rt *luaRuntime) scopeRegionGet(l *lua.LState) int {
	return rt.pushScopeValue(l, rt.rawValue("region", scopeKey(l)))
}

// scopeRegionID implements region.id() -> the zone's region ref, or nil if region-less. Lets a script
// branch on which region it is in.
func (rt *luaRuntime) scopeRegionID(l *lua.LState) int {
	if rt.zone == nil || rt.zone.scopes == nil || rt.zone.scopes.regionID == "" {
		l.Push(lua.LNil)
		return 1
	}
	l.Push(lua.LString(rt.zone.scopes.regionID))
	return 1
}

// jsonTruthy reports whether a stored scope value counts as a SET flag: present and not JSON false/null.
// (A JSON 0 or "" is "set" — only an absent key or an explicit false/null is "off"; the director clears a
// flag by deleting it or setting false.)
func jsonTruthy(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	switch string(raw) {
	case "false", "null":
		return false
	}
	return true
}
