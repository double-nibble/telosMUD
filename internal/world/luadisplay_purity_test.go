package world

import (
	"sort"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// luadisplay_purity_test.go — #256: a DISPLAY render (renderDisplaySheet/renderDisplayList, inv.display) runs a
// builder-authored template that MUST be a PURE function of world state -> a string. Every side-effecting op is
// forbidden during such a render (denyInDisplay); read-only/query ops stay allowed. The raised error aborts the
// render exactly like any template runtime error, so the caller falls back to its BUILT-IN sheet with no partial
// side effect.
//
// The KEY completeness net is TestDisplayOpClassificationDriftGuard: it reflects EVERY script-reachable engine
// callable surface the sandbox builds (handle + the userdata builders + every readOnly table + the bare-global
// functions) and forces EVERY op into a per-surface renderOpClass decision. A future op added to ANY of these
// surfaces without a render-safety decision FAILS the build's test run — the root-cause fix for the original
// completeness gap (the guard used to see only handle + mud).

// renderOpClass classifies EVERY script-reachable engine op, keyed by SURFACE then op name. The bool is
// "forbidden in a display render?". Keying per-surface is REQUIRED because op names collide across surfaces
// (handle `send` vs gmcp `send`; world `get` vs region `get`).
//
// IMPORTANT: the drift guard forces a DECISION on every op, not a CORRECT one — a mutating op mistakenly listed
// `false` (safe) still escapes the render guard. The per-op behavioral test (TestForbiddenOpDeniedInDisplayRender)
// is the correctness check that each forbidden op actually raises; the two must agree by construction (both are
// driven off this table).
var renderOpClass = map[string]map[string]bool{
	// handle metatable methods (luahandle.go / luaharm.go)
	"handle": {
		// read/query/traversal — SAFE (a sheet is built from these)
		"id": false, "name": false, "short": false, "long": false, "attr": false, "resource": false, "resource_max": false,
		"level": false, "has_affect": false, "affect_magnitude": false, "has_flag": false, "room": false,
		"contents": false, "equipment": false, "equipment_slots": false, "group": false, "is_enemy": false,
		"distance": false, "can_see": false, "exits": false, "occupants": false, "room_items": false,
		"toggle":        false, // #358 content player-toggle read (default-aware) — pure read, safe in a sheet
		"coord":         false, // #360 room grid-coord read — pure read, safe in a sheet
		"has_room_flag": false, // #361 room named-flag read — pure read, safe in a sheet
		// comms/output + harm + forced movement — FORBIDDEN
		"send": true, "act": true, "say": true, "emote": true,
		"damage": true, "heal": true, "modify_resource": true, "drain": true,
		"apply_affect": true, "remove_affect": true, "dispel": true,
		"move": true, "teleport": true, "recall": true,
	},
	// mud table (luamud.go)
	"mud": {
		"now": false, "log": false, "scan": false, "pvp_allowed": false,
		// zone() reads the zone's own id (#411) — a pure read of immutable construction-time state, and the
		// one thing a display template may legitimately want it for is branching on which copy it is rendering.
		"zone": false,
		// random/roll draw rt.rng (the #58 combat stream) — FORBIDDEN (a render must not advance sim entropy)
		"random": true, "roll": true,
		"broadcast": true, "spawn": true, "transform": true, "summon": true,
		"after": true, "cancel": true, "fire": true,
	},
	// world/region scope READS (luascope.go) — SAFE
	"world":  {"flag": false, "get": false},
	"region": {"get": false, "id": false},
	// gmcp custom-frame emit (luagmcp.go) — FORBIDDEN (output)
	"gmcp": {"send": true},
	// ui sheet toolkit (luaui.go) — pure formatting, SAFE
	"ui":       {"sheet": false},
	"ui.sheet": {"row": false, "rows": false, "span": false, "divider": false, "banner": false, "render": false},
	// screen builder (luascreen.go) — clear/home/at/color/write mutate only the LOCAL frame buffer (pure);
	// show() emits a Screen frame to a player — FORBIDDEN
	"screen": {"clear": false, "home": false, "at": false, "color": false, "write": false, "show": true},
	// bare-global functions (luart.go base allowlist + luascope signal writes). The base Lua funcs are pure;
	// print is a log sink (like mud.log); signal_region/signal_world are the durable director WRITE surface.
	"global": {
		"assert": false, "error": false, "pcall": false, "xpcall": false, "select": false,
		"type": false, "tostring": false, "tonumber": false, "pairs": false, "ipairs": false, "unpack": false,
		"print":         false,
		"signal_region": true, "signal_world": true,
	},
}

// reflectedOpSurfaces returns, for a live runtime, the ACTUAL op names the sandbox exposes on each classified
// surface — reflected from the real Lua tables/metatables so a newly-wired op appears here automatically (the
// drift guard's whole point). string/table/math are deliberately EXCLUDED: they are Lua stdlib namespaces
// (capped but pure library surface), not engine ops in #256's mutate/emit scope.
func reflectedOpSurfaces(t *testing.T, rt *luaRuntime) map[string][]string {
	t.Helper()
	return map[string][]string{
		"handle":   metatableIndexKeys(t, rt, luaHandleTypeName),
		"ui.sheet": metatableIndexKeys(t, rt, luaSheetTypeName),
		"screen":   metatableIndexKeys(t, rt, luaScreenTypeName),
		"mud":      proxyTableKeys(t, rt, "mud"),
		"world":    proxyTableKeys(t, rt, "world"),
		"region":   proxyTableKeys(t, rt, "region"),
		"gmcp":     proxyTableKeys(t, rt, "gmcp"),
		"ui":       proxyTableKeys(t, rt, "ui"),
		"global":   globalFunctionKeys(t, rt),
	}
}

// metatableIndexKeys reflects the method table (the __index) of a registered userdata type — how handle, the ui
// sheet builder, and the screen builder expose their methods.
func metatableIndexKeys(t *testing.T, rt *luaRuntime, typeName string) []string {
	t.Helper()
	mt, ok := rt.L.GetTypeMetatable(typeName).(*lua.LTable)
	if !ok {
		t.Fatalf("%s metatable is not a table", typeName)
	}
	idx, ok := mt.RawGetString("__index").(*lua.LTable)
	if !ok {
		t.Fatalf("%s metatable __index is not a table", typeName)
	}
	return tableStringKeys(idx)
}

// proxyTableKeys reflects the underlying function table behind a readOnly() proxy global (mud/world/region/gmcp/
// ui). It reads the raw .Metatable field directly: LState.GetMetatable honors the proxy's decoy __metatable
// ("locked") and would hand back that string, not the underlying table.
func proxyTableKeys(t *testing.T, rt *luaRuntime, globalName string) []string {
	t.Helper()
	proxy, ok := rt.L.GetGlobal(globalName).(*lua.LTable)
	if !ok {
		t.Fatalf("global %q is not a table", globalName)
	}
	mt, ok := proxy.Metatable.(*lua.LTable)
	if !ok {
		t.Fatalf("global %q has no metatable (readOnly should install one)", globalName)
	}
	underlying, ok := mt.RawGetString("__index").(*lua.LTable)
	if !ok {
		t.Fatalf("global %q proxy __index is not the underlying table", globalName)
	}
	return tableStringKeys(underlying)
}

// globalFunctionKeys reflects the bare-global FUNCTIONS on the sandbox globals table (print, signal_region,
// signal_world, and the base Lua funcs). Table-valued globals (string/table/math/mud/world/region/ui/screen/
// gmcp) are skipped — each is reflected as its own named surface.
func globalFunctionKeys(t *testing.T, rt *luaRuntime) []string {
	t.Helper()
	g, ok := rt.L.Get(lua.GlobalsIndex).(*lua.LTable)
	if !ok {
		t.Fatal("globals index is not a table")
	}
	var keys []string
	g.ForEach(func(k, v lua.LValue) {
		ks, okk := k.(lua.LString)
		if !okk {
			return
		}
		if _, isFn := v.(*lua.LFunction); isFn {
			keys = append(keys, string(ks))
		}
	})
	sort.Strings(keys)
	return keys
}

// tableStringKeys collects the string keys of a Lua table (sorted for stable failures).
func tableStringKeys(tbl *lua.LTable) []string {
	var keys []string
	tbl.ForEach(func(k, _ lua.LValue) {
		if s, ok := k.(lua.LString); ok {
			keys = append(keys, string(s))
		}
	})
	sort.Strings(keys)
	return keys
}

// TestDisplayOpClassificationDriftGuard is the store-reflect-net for #256: EVERY op the sandbox exposes on any
// classified surface must have a per-surface render-safety decision in renderOpClass, and every classification
// must correspond to a real op (no stale entries). A newly-added op that no one decided about fails here —
// forcing a purity decision on every future op, across every surface (not just handle+mud, the original gap).
// NOTE: this forces a DECISION, not a CORRECT one (see renderOpClass) — the behavioral test is the correctness net.
func TestDisplayOpClassificationDriftGuard(t *testing.T) {
	z := newZone("drift")
	rt := z.lua

	surfaces := reflectedOpSurfaces(t, rt)

	for surface, ops := range surfaces {
		cls, ok := renderOpClass[surface]
		if !ok {
			t.Fatalf("surface %q is reflected but has no renderOpClass entry", surface)
		}
		reflected := map[string]bool{}
		for _, op := range ops {
			reflected[op] = true
			if _, decided := cls[op]; !decided {
				t.Errorf("UNCLASSIFIED op %q.%q — add it to renderOpClass[%q] as false (read-only) or true "+
					"(mutates/emits) and, if true, gate the op with rt.denyInDisplay (#256)", surface, op, surface)
			}
		}
		for op := range cls {
			if !reflected[op] {
				t.Errorf("stale classification %q.%q — no such op is registered on that surface", surface, op)
			}
		}
	}

	// Pin the EXACT forbidden set (27 qualified ops), so silently dropping a gate — or reclassifying a mutating
	// op as safe — fails here too. Kept in lockstep with the guarded call sites across all six files.
	wantForbidden := map[string]bool{
		"handle.send": true, "handle.act": true, "handle.say": true, "handle.emote": true,
		"handle.damage": true, "handle.heal": true, "handle.modify_resource": true, "handle.drain": true,
		"handle.apply_affect": true, "handle.remove_affect": true, "handle.dispel": true,
		"handle.move": true, "handle.teleport": true, "handle.recall": true,
		"mud.random": true, "mud.roll": true, "mud.broadcast": true, "mud.spawn": true,
		"mud.transform": true, "mud.summon": true, "mud.after": true, "mud.cancel": true, "mud.fire": true,
		"gmcp.send": true, "screen.show": true,
		"global.signal_region": true, "global.signal_world": true,
	}
	gotForbidden := map[string]bool{}
	for surface, ops := range renderOpClass {
		for op, forbidden := range ops {
			if forbidden {
				gotForbidden[surface+"."+op] = true
			}
		}
	}
	if len(gotForbidden) != len(wantForbidden) {
		t.Errorf("forbidden-op count = %d, want %d (the #256 set)", len(gotForbidden), len(wantForbidden))
	}
	for q := range wantForbidden {
		if !gotForbidden[q] {
			t.Errorf("expected forbidden op %q is not classified forbidden in renderOpClass", q)
		}
	}
	for q := range gotForbidden {
		if !wantForbidden[q] {
			t.Errorf("op %q is classified forbidden but not in the expected #256 set (intended?)", q)
		}
	}
}

// forbiddenOpCall maps each forbidden op (keyed "surface.op") to a Lua call expression that reaches its binding.
// denyInDisplay runs BEFORE any arg validation, so a zero/garbage-arg call still trips the guard — which is
// exactly what the behavioral test asserts.
var forbiddenOpCall = map[string]string{
	"handle.send": "self:send('x')", "handle.act": "self:act('x')",
	"handle.say": "self:say('x')", "handle.emote": "self:emote('x')",
	"handle.damage": "self:damage({amount=1})", "handle.heal": "self:heal('hp', 1)",
	"handle.modify_resource": "self:modify_resource('hp', 1)", "handle.drain": "self:drain('hp', 1)",
	"handle.apply_affect": "self:apply_affect('x')", "handle.remove_affect": "self:remove_affect('x')",
	"handle.dispel": "self:dispel()", "handle.move": "self:move('north')",
	"handle.teleport": "self:teleport(self:room())", "handle.recall": "self:recall()",
	"mud.random": "mud.random()", "mud.roll": "mud.roll('1d4')",
	"mud.broadcast": "mud.broadcast(self:room(), 'x')", "mud.spawn": "mud.spawn('p', self:room())",
	"mud.transform": "mud.transform(self, 'p')", "mud.summon": "mud.summon(self)",
	"mud.after": "mud.after(1, function() end)", "mud.cancel": "mud.cancel(nil)",
	"mud.fire":  "mud.fire('pack:X', self)",
	"gmcp.send": "gmcp.send(self, 'Mud.X', {})", "screen.show": "screen.frame():show(self)",
	"global.signal_region": "signal_region('e')", "global.signal_world": "signal_world('e')",
}

// TestForbiddenOpDeniedInDisplayRender behaviorally confirms EACH forbidden op (all surfaces) actually raises the
// render-purity error inside a DISPLAY invocation — the guard is wired, not just declared. It is DRIVEN OFF
// renderOpClass, so it and the drift guard cannot disagree about which ops are forbidden.
func TestForbiddenOpDeniedInDisplayRender(t *testing.T) {
	z, rt, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")

	for surface, ops := range renderOpClass {
		for op, forbidden := range ops {
			if !forbidden {
				continue
			}
			key := surface + "." + op
			call, ok := forbiddenOpCall[key]
			if !ok {
				t.Fatalf("no forbiddenOpCall expression for %q (add one when a new op joins the forbidden set)", key)
			}
			t.Run(key, func(t *testing.T) {
				ch := rt.chunkFor("display:purity:"+key, `
					local ok, err = pcall(function() `+call+` end)
					if ok then return "NO-ERROR" end
					return tostring(err)`)
				got, ran := rt.invokeForString(ch, &luaInvocation{actor: viewer, display: true},
					map[string]lua.LValue{"self": rt.newHandle(viewer)})
				if !ran {
					t.Fatalf("%s: harness chunk should run cleanly (the pcall absorbs the guard error)", key)
				}
				if got == "NO-ERROR" {
					t.Fatalf("%s: was NOT denied in a display render (#256 gate missing)", key)
				}
				if !strings.Contains(got, "not allowed in a display render") {
					t.Fatalf("%s: denied with the wrong error %q — want the render-purity message", key, got)
				}
			})
		}
	}
}

// TestReadOpsAllowedInDisplayRender: representative read-only ops across surfaces run fine in a display render
// (the guard must not over-reach — a sheet is BUILT from these).
func TestReadOpsAllowedInDisplayRender(t *testing.T) {
	z, rt, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")

	ch := rt.chunkFor("display:read", `
		local n = self:name()
		local hp = self:resource("hp")
		local occ = self:room():occupants()
		local w = world.flag("nope")                          -- scope read (safe)
		local sheet = ui.sheet():banner("Hi", "="):render()   -- ui builder (safe)
		return "R[" .. tostring(n) .. "/" .. tostring(hp) .. "/" .. #occ .. "/" .. tostring(w) .. "/" .. (#sheet > 0 and "sheet" or "empty") .. "]"`)
	got, ok := rt.invokeForString(ch, &luaInvocation{actor: viewer, display: true, displayRoom: room},
		map[string]lua.LValue{"self": rt.newHandle(viewer)})
	if !ok {
		t.Fatal("a read-only display template must render, not fall back")
	}
	if !strings.Contains(got, "R[Viewer/100/") || !strings.Contains(got, "/false/sheet]") {
		t.Fatalf("read ops returned unexpected sheet: %q", got)
	}
}

// TestForbiddenOpsStillWorkInMechanics pins that #256 is DISPLAY-only: the guarded ops still take effect in a
// NON-display invocation (display:false). It provides a mechanics passthrough witness for EACH guarded FILE —
// luaharm (damage/teleport), luahandle (send), luamud (broadcast), luagmcp (gmcp.send), luascreen (screen.show),
// luascope (signal_world) — so the guard is proven not to disarm mechanics anywhere.
func TestForbiddenOpsStillWorkInMechanics(t *testing.T) {
	z, rt, hall := harmZone(t)
	actor := harmPlayer(z, hall, "Mover")
	victim := harmPlayer(z, hall, "Victim")
	setFlag(actor, flagPvP, true) // both consent so mechanics damage is not gated (a separate axis)
	setFlag(victim, flagPvP, true)
	sess := z.players["Mover"]

	market := z.newEntity("harm:room:market")
	Add(market, &Room{exits: map[string]ProtoRef{}})
	z.rooms["harm:room:market"] = market

	self := map[string]lua.LValue{"self": rt.newHandle(actor)}
	mech := func(name, src string, binds map[string]lua.LValue) {
		t.Helper()
		if err := rt.invoke(rt.chunkFor(name, src), &luaInvocation{actor: actor}, binds); err != nil {
			t.Fatalf("%s errored in a mechanics invocation (guard must be display-only): %v", name, err)
		}
	}

	// luaharm — damage (co-located).
	mech("mech:damage", `victim:damage({amount=20, type="slash"})`,
		map[string]lua.LValue{"self": rt.newHandle(actor), "victim": rt.newHandle(victim)})
	if hp := resourceCurrent(victim, "hp"); hp != 80 {
		t.Fatalf("damage must still land in a mechanics invocation; victim hp = %d, want 80", hp)
	}

	// luahandle — send.
	drainAllText(sess.out)
	mech("mech:send", `self:send("HELLO")`, self)
	if out := drainAllText(sess.out); !strings.Contains(out, "HELLO") {
		t.Fatalf("self:send must deliver in a mechanics invocation; got %q", out)
	}

	// luamud — broadcast (actor is in the hall with a session).
	drainAllText(sess.out)
	mech("mech:broadcast", `mud.broadcast(self:room(), "BCAST")`, self)
	if out := drainAllText(sess.out); !strings.Contains(out, "BCAST") {
		t.Fatalf("mud.broadcast must deliver in a mechanics invocation; got %q", out)
	}

	// luagmcp — gmcp.send (returns true to a live session; no deny).
	mech("mech:gmcp", `assert(gmcp.send(self, "Mud.Test", {ok=true}) == true, "gmcp.send should reach the session")`, self)

	// luascreen — screen.show (co-located target = self; sends a frame, no deny).
	mech("mech:screen", `screen.frame():write("x"):show(self)`, self)

	// luascope — signal_world (no scoped bus wired -> clean no-op, but crucially NOT the display deny error).
	mech("mech:signal", `signal_world("boss_slain")`, self)

	// luaharm — teleport (do it LAST; it relocates the actor).
	mech("mech:teleport", `assert(self:teleport(dest) == true, "teleport should report moved")`,
		map[string]lua.LValue{"self": rt.newHandle(actor), "dest": rt.newHandle(market)})
	if actor.location != market {
		t.Fatalf("teleport must still relocate in a mechanics invocation; actor at %v", actor.location)
	}
}

// TestDisplayRenderPurityThroughRealRenderPath is the E2E: a `room` template that side-effects rendered through
// the REAL renderDisplaySheet("room") path returns ok=false (so the caller uses its built-in fallback) AND leaks
// NO side effect — the viewer never moves, no extra frame is emitted.
func TestDisplayRenderPurityThroughRealRenderPath(t *testing.T) {
	t.Run("teleport template falls back and does not move the viewer", func(t *testing.T) {
		z, s := roomTmplZone(t, `
			for _, e in ipairs(self:room():exits()) do
				if type(e.to) ~= "string" then self:teleport(e.to) end
			end
			return "TEMPLATE-RAN"`)
		hall := z.rooms["harm:room:hall"]
		market := z.newEntity("harm:room:market")
		market.short = "The Market"
		Add(market, &Room{exits: map[string]ProtoRef{}})
		z.rooms["harm:room:market"] = market
		hall.room.exits["north"] = "harm:room:market"

		drainAllText(s.out)
		got, ok := z.renderDisplaySheet("room", s.entity)
		if ok {
			t.Fatalf("a side-effecting room template must FAIL closed (ok=false -> built-in fallback), got %q", got)
		}
		if s.entity.location != hall {
			t.Fatalf("#256: the render's teleport must NOT have moved the viewer; now at %v", s.entity.location)
		}
	})

	t.Run("send template falls back and emits no frame", func(t *testing.T) {
		z, s := roomTmplZone(t, `self:send("PWNED") return "TEMPLATE-RAN"`)
		drainAllText(s.out)
		got, ok := z.renderDisplaySheet("room", s.entity)
		if ok {
			t.Fatalf("a send-ing room template must FAIL closed (ok=false), got %q", got)
		}
		if out := drainAllText(s.out); strings.Contains(out, "PWNED") {
			t.Fatalf("#256: the render's send must NOT have emitted a frame; got %q", out)
		}
	})

	t.Run("broadcast template falls back and emits no frame", func(t *testing.T) {
		z, s := roomTmplZone(t, `mud.broadcast(self:room(), "PWNED") return "TEMPLATE-RAN"`)
		drainAllText(s.out)
		got, ok := z.renderDisplaySheet("room", s.entity)
		if ok {
			t.Fatalf("a broadcasting room template must FAIL closed (ok=false), got %q", got)
		}
		if out := drainAllText(s.out); strings.Contains(out, "PWNED") {
			t.Fatalf("#256: the render's broadcast must NOT have emitted a frame; got %q", out)
		}
	})
}
