package world

import (
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	lua "github.com/yuin/gopher-lua"
)

// luahandle_test.go — slice 7.2 gates (docs/PHASE7-PLAN.md §1.2, threat rows T7 + T15). These
// prove the handle layer is a VALIDATED reference, never a Go pointer: a dead/departed/
// cross-zone handle no-ops (never panics, never derefs a foreign *Entity), tostring leaks no
// pointer, and the read methods read real engine state. Security-auditor reviews T15 + the
// re-validation path.

// handleTestZone builds a bare zone with one room, a "self" living entity placed in it, the
// attribute/resource/affect defs the methods read, and a flag. Returns the zone, the runtime,
// and the self entity. Mirrors the affect/attribute test setup.
func handleTestZone(t *testing.T) (*Zone, *luaRuntime, *Entity) {
	t.Helper()
	z := newZone("handle-zone")

	z.defs.attr.register("strength", &attributeDef{ref: "strength", base: litNode{v: 14}})
	z.defs.attr.register("level", &attributeDef{ref: "level", base: litNode{v: 7}})
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	z.defs.affect.register("blessed", &affectDef{
		ref: "blessed", name: "Blessed", stacking: stackRefresh, maxStacks: 1, duration: 20,
		modifiers: []affectModifier{{attr: "strength", add: true, value: 2}},
	})

	// A room entity, registered in the zone (so a room handle resolves and self:room() works).
	room := z.newEntity("handle:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	room.short = "The Hall"
	z.rooms["handle:room:hall"] = room

	// The scripted entity ("self"), living, placed in the room.
	self := z.newEntity("handle:mob:guard")
	Add(self, &Living{})
	self.short = "a stern guard"
	Move(self, room)
	setFlag(self, "aggressive", true)
	setResourceCurrent(self, "hp", 73)
	applyAffect(self, "blessed", attachOpts{magnitude: 3}, nil)

	return z, z.lua, self
}

// runSelf runs src with `self` bound to e and fails on error.
func runSelf(t *testing.T, rt *luaRuntime, e *Entity, src string) {
	t.Helper()
	if err := rt.runChunkWithSelf(t.Name(), src, e); err != nil {
		t.Fatalf("script errored: %v\nsrc: %s", err, src)
	}
}

// --- read methods against a real entity ---------------------------------------------------

func TestHandleReadMethods(t *testing.T) {
	_, rt, self := handleTestZone(t)
	checks := []string{
		`assert(self:id() ~= nil and self:id() > 0, "id")`,
		`assert(self:name() == "a stern guard", "name: "..tostring(self:name()))`,
		`assert(self:short() == "a stern guard", "short")`,
		// blessed is +2 strength scaled by the applied magnitude (3): 14 + 2*3 = 20.
		`assert(self:attr("strength") == 20, "strength 14+2*3 blessed: "..tostring(self:attr("strength")))`,
		`assert(self:attr("nonesuch") == 0, "unknown attr reads 0")`,
		`assert(self:resource("hp") == 73, "hp: "..tostring(self:resource("hp")))`,
		`assert(self:resource("nonesuch") == 0, "unknown resource reads 0")`,
		`assert(self:level() == 7, "level attr: "..tostring(self:level()))`,
		`assert(self:has_affect("blessed") == true, "has blessed")`,
		`assert(self:has_affect("cursed") == false, "no cursed")`,
		`assert(self:affect_magnitude("blessed") == 3, "blessed mag: "..tostring(self:affect_magnitude("blessed")))`,
		`assert(self:affect_magnitude("cursed") == 0, "absent mag 0")`,
		`assert(self:has_flag("aggressive") == true, "aggressive flag")`,
		`assert(self:has_flag("sleeping") == false, "no sleeping flag")`,
		`assert(self:room() ~= nil, "room handle")`,
		`assert(self:room():name() == "The Hall", "room name: "..tostring(self:room():name()))`,
		`assert(self:room():id() ~= self:id(), "room is a distinct entity")`,
	}
	for _, c := range checks {
		runSelf(t, rt, self, c)
	}
}

// --- T15: __tostring leaks no Go pointer ---------------------------------------------------

// TestHandleTostringNoPointer asserts tostring(self) is the safe "<entity #rid>" form and
// NEVER contains a Go pointer (0x...). The default gopher-lua tostring(userdata) leaks the
// pointer — an ASLR defeat — so every handle's metatable MUST define __tostring.
func TestHandleTostringNoPointer(t *testing.T) {
	_, rt, self := handleTestZone(t)
	runSelf(t, rt, self, `
		local s = tostring(self)
		assert(s:match("^<entity #%d+>$") ~= nil, "tostring form wrong: "..s)
		assert(s:find("0x") == nil, "tostring leaked a pointer: "..s)
		assert(s:find("userdata") == nil, "tostring leaked the userdata tag: "..s)
	`)
	// The rid in the string matches self:id().
	runSelf(t, rt, self, `assert(tostring(self) == ("<entity #"..tostring(self:id())..">"), "rid mismatch")`)
}

// TestHandleTypeHasTostring asserts the registered handle type's metatable defines __tostring
// (no handle type may be exposed without one — T15). A Go-side check on the metatable.
func TestHandleTypeHasTostring(t *testing.T) {
	_, rt, _ := handleTestZone(t)
	mt := rt.L.GetTypeMetatable(luaHandleTypeName)
	tbl, ok := mt.(*lua.LTable)
	if !ok {
		t.Fatal("handle type metatable is not a table")
	}
	if tbl.RawGetString("__tostring") == lua.LNil {
		t.Fatal("handle metatable lacks __tostring (T15)")
	}
}

// --- T7: no-dangling — dead/departed/removed entity no-ops --------------------------------

// TestHandleDepartedNoOps asserts a handle for an entity that has LEFT the zone (Move to nil,
// the despawn/handoff path) no-ops: its methods return nil/false, never panic, and the script
// observes a safe absence.
func TestHandleDepartedNoOps(t *testing.T) {
	_, rt, self := handleTestZone(t)
	// First, prove the handle works while resolved.
	runSelf(t, rt, self, `assert(self:name() ~= nil)`)
	// Remove the entity from the zone's containment tree (it can no longer be found by rid).
	Move(self, nil)
	// The SAME self handle must now no-op cleanly.
	runSelf(t, rt, self, `
		assert(self:name() == nil, "departed name should be nil")
		assert(self:attr("strength") == nil, "departed attr should be nil")
		assert(self:resource("hp") == nil, "departed resource should be nil")
		assert(self:has_affect("blessed") == false, "departed has_affect should be false")
		assert(self:affect_magnitude("blessed") == 0, "departed magnitude should be 0")
		assert(self:has_flag("aggressive") == false, "departed has_flag should be false")
		assert(self:room() == nil, "departed room should be nil")
		assert(self:level() == nil, "departed level should be nil")
		-- id() still works (it is the handle's own identity, not a live read) and tostring is safe.
		assert(self:id() ~= nil, "id survives")
		assert(tostring(self):find("0x") == nil, "tostring still safe on a departed handle")
	`)
}

// TestHandleStaleAfterReresolve asserts that a handle captured in one call and re-used in a
// LATER call (after the entity departed between calls) does not act and does not panic — the
// "*Entity never lives across calls" guarantee. We bind self, depart it, then re-invoke.
func TestHandleStaleAfterReuse(t *testing.T) {
	_, rt, self := handleTestZone(t)
	// Capture works in call 1.
	runSelf(t, rt, self, `assert(self:name() ~= nil)`)
	// Between calls the entity is reaped.
	Move(self, nil)
	// Call 2 with the same handle: safe no-op, zone survives, next script still runs.
	runSelf(t, rt, self, `assert(self:name() == nil)`)
	runSelf(t, rt, self, `assert(1 + 1 == 2)`)
}

// --- T7: cross-zone — a foreign-zone handle is invalid here -------------------------------

// TestHandleCrossZoneInvalid asserts resolution is ZONE-SCOPED (T7): a handle whose payload
// names a zone resolves its rid ONLY within that zone's containment walk. An rid that exists in
// zone A but not in zone B, carried by a handle pointing at zone B, does NOT resolve — the
// method no-ops. No method ever dereferences a foreign-zone *Entity, because resolveHandle only
// ever walks h.zone and a handle is minted with the entity's OWN zone pointer.
func TestHandleCrossZoneInvalid(t *testing.T) {
	zoneA, _, selfA := handleTestZone(t)
	zoneB := newZone("zone-b")
	rtB := zoneB.lua

	// Forge the cross-zone hazard directly: a handle whose payload carries zone B's pointer +
	// gen but A's self rid (an rid that is NOT registered in B's containment tree). This is the
	// exact shape the guarantee must defeat — a smuggled rid used against the wrong zone.
	ud := rtB.L.NewUserData()
	ud.Value = &luaHandle{rid: selfA.rid, zone: zoneB, zoneGen: zoneB.gen}
	rtB.L.SetMetatable(ud, rtB.L.GetTypeMetatable(luaHandleTypeName))
	rtB.L.SetGlobal("smuggled", ud)
	defer rtB.L.SetGlobal("smuggled", lua.LNil)

	// B's walk does not contain A's rid, so every method no-ops — never reaching A's entity.
	if err := rtB.runChunk(t.Name(), `
		assert(smuggled:name() == nil, "cross-zone rid resolved (name)")
		assert(smuggled:attr("strength") == nil, "cross-zone rid resolved (attr)")
		assert(smuggled:room() == nil, "cross-zone rid resolved (room)")
		assert(tostring(smuggled):find("0x") == nil, "tostring leaked a pointer")
	`); err != nil {
		t.Fatalf("cross-zone isolation failed: %v", err)
	}

	// And a proper B-minted handle for a B entity resolves B's entity — B's runtime works
	// normally for B's own world; only the foreign rid is rejected.
	bRoom := zoneB.newEntity("b:room:cell")
	Add(bRoom, &Room{exits: map[string]ProtoRef{}})
	zoneB.rooms["b:room:cell"] = bRoom
	bSelf := zoneB.newEntity("b:mob:clone")
	Add(bSelf, &Living{})
	bSelf.short = "the B clone"
	Move(bSelf, bRoom)
	runSelf(t, rtB, bSelf, `assert(self:name() == "the B clone", "B handle should resolve B entity, got "..tostring(self:name()))`)

	_ = zoneA
}

// --- -race: no method holds an *Entity across the call ------------------------------------

// TestHandleNoEntityAcrossCall exercises many resolve/read cycles under the race detector
// (run with -race) to catch any path that stashes an *Entity in a Lua value or touches one
// outside the single Go method call. The handle layer never stores an *Entity, so there is
// nothing to race; this asserts that property holds under repeated cross-call reuse.
func TestHandleNoEntityAcrossCall(t *testing.T) {
	_, rt, self := handleTestZone(t)
	for i := 0; i < 200; i++ {
		runSelf(t, rt, self, `
			local _ = self:name()
			local _ = self:attr("strength")
			local r = self:room()
			if r then local _ = r:name() end
		`)
	}
}

// --- runChunkWithSelf hygiene -------------------------------------------------------------

// TestSelfClearedBetweenCalls asserts `self` does not leak from one runChunkWithSelf call into
// an unrelated later plain runChunk call.
func TestSelfClearedBetweenCalls(t *testing.T) {
	_, rt, self := handleTestZone(t)
	runSelf(t, rt, self, `assert(self ~= nil)`)
	if err := rt.runChunk(t.Name(), `assert(self == nil, "self leaked into a later call")`); err != nil {
		t.Fatalf("self not cleared: %v", err)
	}
}

// TestHandleNilEntity asserts a handle for a nil entity is lua nil (the no-result convention),
// and binding a nil self yields nil in-script (no panic).
func TestHandleNilEntity(t *testing.T) {
	_, rt, _ := handleTestZone(t)
	if got := rt.newHandle(nil); got != lua.LNil {
		t.Fatalf("newHandle(nil) = %v, want LNil", got)
	}
	if err := rt.runChunkWithSelf(t.Name(), `assert(self == nil)`, nil); err != nil {
		t.Fatalf("nil-self script errored: %v", err)
	}
}

// TestHandleTostringStandalone is a focused T15 check independent of the zone fixture: a fresh
// runtime, a handle, tostring contains no pointer.
func TestHandleTostringStandalone(t *testing.T) {
	z := newZone("solo")
	rt := z.lua
	e := z.newEntity("solo:mob:x")
	Add(e, &Living{})
	r := z.newEntity("solo:room:x")
	Add(r, &Room{exits: map[string]ProtoRef{}})
	z.rooms["solo:room:x"] = r
	Move(e, r)
	rt.L.SetGlobal("h", rt.newHandle(e))
	defer rt.L.SetGlobal("h", lua.LNil)
	if err := rt.runChunk(t.Name(), `
		local s = tostring(h)
		assert(s:find("0x") == nil and s:find("userdata") == nil, "pointer/tag leak: "..s)
	`); err != nil {
		t.Fatal(err)
	}
}

// --- 7.3a traversal -----------------------------------------------------------------------

// TestHandleTraversal asserts the traversal methods return tables of validated handles: a
// room's contents, a wearer's equipment, the group stub, and the is_enemy/distance/can_see
// reads. Returned values are handles (have :name()).
func TestHandleTraversal(t *testing.T) {
	z, rt, self := handleTestZone(t)
	// Add two more entities into self's room so contents has >1 occupant.
	other := z.newEntity("handle:mob:rat")
	Add(other, &Living{})
	other.short = "a rat"
	Move(other, self.location)

	rt.L.SetGlobal("other", rt.newHandle(other))
	defer rt.L.SetGlobal("other", lua.LNil)

	runSelf(t, rt, self, `
		local room = self:room()
		local occ = room:contents()
		assert(type(occ) == "table", "contents is a table")
		local names = {}
		for _, h in ipairs(occ) do names[h:name()] = true end
		assert(names["a stern guard"], "self in room contents")
		assert(names["a rat"], "rat in room contents")
	`)
	runSelf(t, rt, self, `
		local eq = self:equipment()
		assert(type(eq) == "table", "equipment is a table")
		assert(#eq == 0, "guard wears nothing")
	`)
	runSelf(t, rt, self, `
		local g = self:group()
		assert(type(g) == "table" and #g == 0, "group stub is an empty table")
	`)
	runSelf(t, rt, self, `
		assert(self:is_enemy(other) == true, "co-located living = enemy (placeholder)")
		assert(self:is_enemy(self) == false, "self is not its own enemy")
		assert(self:distance(self) == 0, "distance to self is 0")
		assert(self:distance(other) == 1, "distance to co-located is 1")
		assert(self:can_see(other) == true, "can_see (trivial filter true)")
	`)
}

// TestHandleTraversalDepartedNoOps asserts a departed entity's traversal no-ops to an empty
// table, never nil and never a panic.
func TestHandleTraversalDepartedNoOps(t *testing.T) {
	_, rt, self := handleTestZone(t)
	Move(self, nil)
	runSelf(t, rt, self, `
		local c = self:contents()
		assert(type(c) == "table" and #c == 0, "departed contents = empty table")
		local e = self:equipment()
		assert(type(e) == "table" and #e == 0, "departed equipment = empty table")
	`)
}

// --- 7.3a comms reach a session/room ------------------------------------------------------

// TestHandleCommsReachSession asserts h:send / h:say reach a player's session via the existing
// act()/send messaging (no game-state writes). We drive a player entity with an out channel and
// read the frame.
func TestHandleCommsReachSession(t *testing.T) {
	z := newZone("comms")
	rt := z.lua
	room := z.newEntity("comms:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["comms:room:hall"] = room

	s := &session{character: "Listener", out: make(chan *playv1.ServerFrame, 16), epoch: 1}
	z.newPlayerEntity(s, "Listener")
	Move(s.entity, room)
	z.players["Listener"] = s

	rt.L.SetGlobal("player", rt.newHandle(s.entity))
	defer rt.L.SetGlobal("player", lua.LNil)

	// h:send reaches the session.
	if err := rt.runChunk(t.Name(), `player:send("hello there")`); err != nil {
		t.Fatal(err)
	}
	if got := drainText(t, s.out); !strings.Contains(got, "hello there") {
		t.Fatalf("h:send did not reach the session: %q", got)
	}

	// h:say reaches the speaker's own session (the "You say" perspective).
	if err := rt.runChunk(t.Name(), `player:say("greetings")`); err != nil {
		t.Fatal(err)
	}
	if got := drainText(t, s.out); !strings.Contains(got, "greetings") {
		t.Fatalf("h:say did not reach the session: %q", got)
	}
}

// TestHandleCommsSanitizesMarkup is the ISSUE-B regression: SCRIPT-SUPPLIED markup carrying a
// control/ESC sequence has it stripped at the WORLD layer (asserted on the emitted frame, not
// relying on the telnet edge), while legitimate markup/color tokens survive. Covers h:send,
// h:say, and h:act.
func TestHandleCommsSanitizesMarkup(t *testing.T) {
	z := newZone("san")
	rt := z.lua
	room := z.newEntity("san:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["san:room:hall"] = room
	s := &session{character: "Reader", out: make(chan *playv1.ServerFrame, 16), epoch: 1}
	z.newPlayerEntity(s, "Reader")
	Move(s.entity, room)
	z.players["Reader"] = s
	rt.L.SetGlobal("player", rt.newHandle(s.entity))
	defer rt.L.SetGlobal("player", lua.LNil)

	// h:send — an ESC color-injection attempt plus legitimate markup.
	if err := rt.runChunk(t.Name(), `player:send("{red}safe\27[2J\7 part{x}")`); err != nil {
		t.Fatal(err)
	}
	got := drainText(t, s.out)
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '\x07') {
		t.Fatalf("h:send leaked a control/ESC rune to the frame: %q", got)
	}
	if !strings.Contains(got, "{red}") || !strings.Contains(got, "{x}") || !strings.Contains(got, "safe") {
		t.Fatalf("h:send stripped legitimate markup: %q", got)
	}

	// h:say — the script-supplied text arg is cleaned; the engine "You say" wrapper survives.
	if err := rt.runChunk(t.Name(), `player:say("hi\27[31m there")`); err != nil {
		t.Fatal(err)
	}
	got = drainText(t, s.out)
	if strings.ContainsRune(got, '\x1b') {
		t.Fatalf("h:say leaked an ESC rune: %q", got)
	}
	if !strings.Contains(got, "You say") || !strings.Contains(got, "hi") {
		t.Fatalf("h:say lost the say wrapper or text: %q", got)
	}

	// h:act — the script-supplied template is cleaned; the $-referent survives.
	if err := rt.runChunk(t.Name(), `player:act("$n waves\27[0m gravely", nil, nil, "actor")`); err != nil {
		t.Fatal(err)
	}
	got = drainText(t, s.out)
	if strings.ContainsRune(got, '\x1b') {
		t.Fatalf("h:act leaked an ESC rune: %q", got)
	}
	if !strings.Contains(got, "waves") || !strings.Contains(got, "gravely") {
		t.Fatalf("h:act lost legitimate content: %q", got)
	}
}

// drainText pulls the next text frame's body from a session out channel (test helper).
func drainText(t *testing.T, out chan *playv1.ServerFrame) string {
	t.Helper()
	select {
	case f := <-out:
		if o := f.GetOutput(); o != nil {
			return o.GetMarkup()
		}
		return ""
	default:
		return ""
	}
}
