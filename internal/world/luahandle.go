package world

import (
	"fmt"

	"github.com/double-nibble/telosmud/internal/textsan"
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

	// __index is the method table. Read/query + traversal + comms (slices 7.2 + 7.3a). NO
	// effect ops / harm surface — those are 7.3c.
	methods := map[string]lua.LGFunction{
		// 7.2 identity/query (read)
		"id":               rt.hID,
		"name":             rt.hName,
		"short":            rt.hShort,
		"attr":             rt.hAttr,
		"resource":         rt.hResource,
		"resource_max":     rt.hResourceMax,
		"level":            rt.hLevel,
		"has_affect":       rt.hHasAffect,
		"affect_magnitude": rt.hAffectMagnitude,
		"has_flag":         rt.hHasFlag,
		"room":             rt.hRoom,
		// 7.3a traversal (handle-returning / bool reads)
		"contents":        rt.hContents,
		"equipment":       rt.hEquipment,
		"equipment_slots": rt.hEquipmentSlots,
		"group":           rt.hGroup,
		"is_enemy":        rt.hIsEnemy,
		"distance":        rt.hDistance,
		"can_see":         rt.hCanSee,
		// room-surface reads (#24): the three views a `room` display template needs, meaningful on a ROOM
		// handle (self:room()). occupants() is VIEWER-FILTERED through canSee — see hOccupants.
		"exits":      rt.hExits,
		"occupants":  rt.hOccupants,
		"room_items": rt.hRoomItems,
		// 7.3a comms (message via the existing act()/send — no harm)
		"send":  rt.hSend,
		"act":   rt.hAct,
		"say":   rt.hSay,
		"emote": rt.hEmote,
		// 7.3c effect ops (THE harm surface — each routes the EXISTING funnel; luaharm.go)
		"damage":          rt.hDamage,
		"heal":            rt.hHeal,
		"modify_resource": rt.hModifyResource,
		"drain":           rt.hDrain,
		"apply_affect":    rt.hApplyAffect,
		"remove_affect":   rt.hRemoveAffect,
		"dispel":          rt.hDispel,
		"move":            rt.hMove,
		"teleport":        rt.hTeleport,
		"recall":          rt.hRecall,
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

// denyInDisplay raises a clean Lua error (and returns true) when called inside a DISPLAY render
// (inv.display, set by renderDisplaySheet/renderDisplayList — luadisplay.go). A content sheet
// template must be a PURE function of world state -> a string: it may NOT mutate the world or emit
// output (#256). Every side-effecting handle/mud op calls this as its FIRST statement, so the guard
// fires BEFORE any arg validation or effect — the raised error aborts the render, invokeForString
// returns ("", false), and the caller falls back to its BUILT-IN sheet with NO partial side effect
// leaked. Read-only/query ops (name/attr/occupants/…) are NOT gated — they are what a sheet is for.
// It mirrors the luascreen.go admit() shape: RaiseError, then the caller returns 0.
func (rt *luaRuntime) denyInDisplay(l *lua.LState, op string) bool {
	if rt.inv != nil && rt.inv.display {
		l.RaiseError("%s: not allowed in a display render (a sheet template must be side-effect free)", op)
		return true
	}
	return false
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

// hResourceMax is self:resource_max(ref) — the entity's MAX for a resource (the denominator a display template
// pairs with resource() for "cur/max"). nil on a stale handle. Read-only, like resource().
func (rt *luaRuntime) hResourceMax(l *lua.LState) int {
	e := resolveHandle(l, 1)
	name := l.CheckString(2)
	if e == nil {
		l.Push(lua.LNil)
		return 1
	}
	l.Push(lua.LNumber(resourceMax(e, name)))
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

// --- 7.3a traversal (handle-returning / bool reads; no harm) ------------------------------

// pushHandleList pushes a Lua array-table of validated handles, one per entity in es (nil
// entities skipped). An empty input yields an empty table — a departed-entity traversal
// no-ops to {} rather than nil so a script can always `for _,h in ipairs(...)` safely.
func (rt *luaRuntime) pushHandleList(l *lua.LState, es []*Entity) int {
	t := l.NewTable()
	n := 0
	for _, e := range es {
		if e == nil {
			continue
		}
		n++
		t.RawSetInt(n, rt.newHandle(e))
	}
	l.Push(t)
	return 1
}

// hContents returns a table of handles for the entity's contents (a room's occupants + ground
// items, a container's items, a mob/player's inventory). A departed/unresolved handle yields
// an empty table.
//
// PERCEPTION vs MECHANICS split, resolved by the INVOCATION context (#250):
//   - In a MECHANICS invocation (the default), contents() is UNFILTERED — an AoE must hit the hidden
//     rogue; a room-scoped affect must land on the invisible mage. It does NOT consult canSee.
//   - In a DISPLAY invocation (renderDisplaySheet/renderDisplayList set inv.display), contents() of a
//     container that is NOT the viewer (a room, a chest) is canSee-FILTERED, so a `room` template that
//     reaches for contents() instead of occupants()/room_items() can no longer disclose concealed
//     occupants — the leak trap this closes. self:contents() (the viewer's OWN inventory: e is the
//     invocation actor) stays raw, so a player always sees their own items (even an invisible one).
//     Fails CLOSED: a display read with no viewer/zone discloses nothing.
//
// occupants() / room_items() remain the INTENT-revealing accessors a display surface should use (they are
// canSee-filtered unconditionally and, for occupants, exclude the viewer); this is defense in depth for the
// author who reaches for the general contents() instead.
func (rt *luaRuntime) hContents(l *lua.LState) int {
	e := resolveHandle(l, 1)
	if e == nil {
		return rt.pushHandleList(l, nil)
	}
	if rt.inv != nil && rt.inv.display && e != rt.inv.actor {
		return rt.pushHandleList(l, rt.displayVisibleContents(e))
	}
	return rt.pushHandleList(l, e.contents)
}

// displayReachesForeignRoom reports whether, in a DISPLAY render, e is a ROOM the viewer is NOT standing in —
// a NEIGHBOR reached via exits().to (hExits hands a display a walkable handle to the destination room). A
// display's perception is anchored to the viewer's own location, so an enumeration accessor
// (occupants/room_items/contents/mud.scan) must disclose NOTHING for such a room (#253): otherwise a `room`
// template could peer into an adjacent room's occupants — which `look` never shows — and the co-location-gated
// darkness check (visibility.go) would not even conceal them (the viewer isn't co-located). The viewer's own
// room (e == viewer.location), containers in it, and the viewer's inventory stay reachable (co-located / owned);
// only a FOREIGN room is blocked, and only in a display render (a mechanics script may still scan any room). It
// fails CLOSED: a display with no viewer treats every room as foreign. `to` remains a handle so a template can
// still read the destination's NAME — only enumerating its live occupants is denied.
func (rt *luaRuntime) displayReachesForeignRoom(e *Entity) bool {
	if rt.inv == nil || !rt.inv.display || e.room == nil {
		return false // not a display render, or e is not a room (a chest/inventory — #250 canSee handles it)
	}
	// Anchor to the room CAPTURED at render start (displayRoom), not the LIVE actor.location: a template can
	// teleport the viewer mid-render, and a live anchor would let it relocate into a neighbor, enumerate, and
	// relocate back — a TOCTOU scry the security review PoC'd (#253). A frozen anchor can't be shifted by the
	// render. A direct-invocation test may not set displayRoom; fall back to the live location there (tests do
	// not relocate). Fail closed: no anchor at all => every room is foreign.
	anchor := rt.inv.displayRoom
	if anchor == nil && rt.inv.actor != nil {
		anchor = rt.inv.actor.location
	}
	return anchor == nil || e != anchor
}

// displayVisibleContents is the #250 concealment filter shared by the two DISPLAY-render entity enumerators
// that would otherwise return a container's raw contents — contents() (hContents) and mud.scan (mudScan). It
// returns e.contents keeping only the entities the invocation's viewer canSee, so a sheet can't disclose a
// concealed occupant/item; a FOREIGN room (a neighbor via exits()) discloses nothing at all (#253). It fails
// CLOSED (nil viewer or zoneless container => nothing), matching the hOccupants/hRoomItems posture. Callers
// invoke it only when rt.inv.display is set AND e is not the viewer (the viewer's own inventory stays raw).
// Unlike occupants() it does NOT exclude the viewer — contents() is the whole-container list and the viewer
// canSee's themselves — and unlike room_items() it is not item-restricted.
func (rt *luaRuntime) displayVisibleContents(e *Entity) []*Entity {
	viewer := rt.inv.actor
	if viewer == nil || e.zone == nil || rt.displayReachesForeignRoom(e) {
		return nil
	}
	var visible []*Entity
	for _, occ := range e.contents {
		if e.zone.canSee(viewer, occ) { // THE chokepoint: concealed from this viewer => absent
			visible = append(visible, occ)
		}
	}
	return visible
}

// hEquipment returns a table of handles for the entity's WORN items (the Wearer component's
// worn slots). A non-wearer / unresolved handle yields an empty table.
func (rt *luaRuntime) hEquipment(l *lua.LState) int {
	e := resolveHandle(l, 1)
	if e == nil {
		return rt.pushHandleList(l, nil)
	}
	return rt.pushHandleList(l, equipmentItems(e))
}

// hEquipmentSlots returns the entity's worn items WITH their slot labels (#85), so a display template can
// render "<worn on head> an iron helmet" — the bare `equipment()` list can't. The result is an array (content
// slot order) of records: {slot=<label>, flag=<"<worn on head>">, item=<handle>}. `flag` is the ready-made
// inventory tag; `slot` is the raw label for a template that wants to format it itself. A non-wearer /
// unresolved handle yields an empty table.
func (rt *luaRuntime) hEquipmentSlots(l *lua.LState) int {
	t := l.NewTable()
	e := resolveHandle(l, 1)
	if e == nil || e.zone == nil {
		l.Push(t)
		return 1
	}
	wr, ok := Get[*Wearer](e)
	if !ok {
		l.Push(t)
		return 1
	}
	vocab := e.zone.wearSlots()
	n := 0
	for _, loc := range vocab.orderedRefs() {
		item := wr.worn[loc]
		if item == nil {
			continue
		}
		n++
		rec := l.NewTable()
		rec.RawSetString("slot", lua.LString(vocab.label(loc)))
		rec.RawSetString("flag", lua.LString(vocab.wornFlag(loc)))
		rec.RawSetString("item", rt.newHandle(item))
		t.RawSetInt(n, rec)
	}
	l.Push(t)
	return 1
}

// --- room-surface reads (#24): what a `room` display template needs -------------------------

// viewer returns the entity on whose behalf the current script call is running — the invocation actor, the
// ENGINE-owned "who is looking" (never a Lua argument, so a script cannot spoof a perspective; P7-D3
// invariant 1). nil outside any invocation.
//
// This is the perspective every VISIBILITY-FILTERED accessor must consult. It is deliberately NOT derivable
// from the handle receiver: `self:room():occupants()` asks about the ROOM's contents but must answer as the
// VIEWER, so the receiver is the subject and rt.inv.actor is the perspective.
//
// Per entry point:
//   - a DISPLAY render (renderDisplaySheet/renderDisplayList) sets actor = the viewing player, which is
//     exactly the perspective a sheet must be rendered from. This is the case that matters for disclosure.
//   - a TRIGGER/event body inherits the firing cascade's actor (invokeFromCtx), so occupants() answers "as
//     the acting entity", which may differ from the script-owning entity (`self`). No player-visible surface
//     reads it that way today; a mob reasoning about who it can see should ask self:can_see(o) explicitly.
func (rt *luaRuntime) viewer() *Entity {
	if rt == nil || rt.inv == nil {
		return nil
	}
	return rt.inv.actor
}

// hExits returns the entity's exits — meaningful on a ROOM handle (self:room():exits()). The result is an
// array (canonical N/E/S/W/U/D order first, then any other authored direction sorted; see Room.allExits) of
// read-only records:
//
//	{dir="north", ref="midgaard:room:market", to=<room handle> | "midgaard:room:market"}
//
// `to` is a validated HANDLE when the destination room lives in THIS zone (the script can walk it: to:name()),
// and the destination's bare ProtoRef STRING when it does not — a cross-zone exit has no resolvable handle
// here (T7: no handle ever names a foreign-zone entity), and neither does a dangling ref. `ref` always carries
// the raw destination ref so a template can render a cross-zone exit without branching on `to`'s type.
//
// The enumeration is TOTAL over the room's exit table, so a data-only / non-reciprocal maze exit is included
// (it is a real edge of the graph). Read-only: the records are fresh tables, mutating them changes nothing.
// A non-room / unresolved handle yields an empty table.
func (rt *luaRuntime) hExits(l *lua.LState) int {
	t := l.NewTable()
	e := resolveHandle(l, 1)
	if e == nil || e.room == nil {
		l.Push(t)
		return 1
	}
	n := 0
	for _, dir := range e.room.allExits() {
		ref := e.room.exits[dir]
		rec := l.NewTable()
		rec.RawSetString("dir", lua.LString(dir))
		rec.RawSetString("ref", lua.LString(string(ref)))
		if dest := localRoomByRef(e.zone, ref); dest != nil {
			rec.RawSetString("to", rt.newHandle(dest))
		} else {
			rec.RawSetString("to", lua.LString(string(ref)))
		}
		n++
		t.RawSetInt(n, rec)
	}
	l.Push(t)
	return 1
}

// localRoomByRef resolves an exit's destination ProtoRef to a room entity in zone z, or nil when the ref
// names a DIFFERENT zone (cross-zone: not ours to hand out) or no such room is registered here (a dangling
// authored ref). Mirrors Zone.move's parseRef routing decision.
func localRoomByRef(z *Zone, ref ProtoRef) *Entity {
	if z == nil {
		return nil
	}
	zoneID, roomRef := parseRef(ref)
	if zoneID != "" && zoneID != z.id {
		return nil // cross-zone destination: no handle in this zone (T7)
	}
	return z.rooms[roomRef]
}

// hOccupants returns the LIVING occupants (players + mobs) of the entity as handles — meaningful on a ROOM
// handle (self:room():occupants()). The viewer themselves is excluded, matching `look`'s room listing.
//
// SECURITY (#28/#99/#100 — the canSee-BYPASS leak trap): every occupant is routed through the engine's ONE
// visibility chokepoint (Zone.canSee -> visibleTo) from the VIEWER's perspective, so an occupant the viewer
// cannot perceive — invisible without detect, mundane-hidden without sense_hidden, wizinvis above the
// viewer's trust rank, or concealed by an unlit dark room — is OMITTED ENTIRELY. It is never rendered as a
// leaky "someone". A holylight viewer inherits see-all from the same chokepoint. A room occupant list handed
// to content is exactly the surface the round-12 work flagged, so it fails CLOSED:
//
//   - no invocation actor => no perspective to filter from => the EMPTY list, never the unfiltered contents.
//     (visibleTo treats a nil viewer as "system render, conceal nothing"; inheriting that here would hand a
//     template the full occupant set, so this method makes its own decision — the documented obligation in
//     visibleTo's nil-perspective note.)
//
// A non-container / unresolved handle yields an empty table.
func (rt *luaRuntime) hOccupants(l *lua.LState) int {
	e := resolveHandle(l, 1)
	if e == nil || e.zone == nil {
		return rt.pushHandleList(l, nil)
	}
	viewer := rt.viewer()
	if viewer == nil {
		return rt.pushHandleList(l, nil) // fail closed: no perspective => disclose nothing
	}
	if rt.displayReachesForeignRoom(e) {
		return rt.pushHandleList(l, nil) // #253: a display can't perceive a room the viewer isn't standing in
	}
	var visible []*Entity
	for _, occ := range e.contents {
		if occ == viewer || !isCreature(occ) {
			continue
		}
		if !e.zone.canSee(viewer, occ) {
			continue // THE chokepoint: concealed from this viewer => absent from the list
		}
		visible = append(visible, occ)
	}
	return rt.pushHandleList(l, visible)
}

// hRoomItems returns the entity's non-creature contents COALESCED — the ground-item listing `look` renders,
// in structured form so a template can show "a rusty sword (x3)" its own way. Meaningful on a ROOM handle
// (self:room():room_items()) but works on any container. The result is an array (first-appearance order) of
// read-only records:
//
//	{item=<handle to the representative>, name="a rusty sword", long="A rusty sword lies here.", count=3}
//
// The grouping is coalesceItems — the SAME rule the built-in listings use (prototype + per-instance delta;
// materials and containers never merge), so a template can never drift from `look`'s counts.
//
// Visibility: items are routed through canSee too, so the accessor inherits any future item-level concealment
// at the one chokepoint. Today no item carries a concealment flag, so this matches `look`'s "ground items
// always show". Fail-closed on a missing perspective, like hOccupants. A stale handle yields an empty table.
func (rt *luaRuntime) hRoomItems(l *lua.LState) int {
	t := l.NewTable()
	e := resolveHandle(l, 1)
	if e == nil || e.zone == nil {
		l.Push(t)
		return 1
	}
	viewer := rt.viewer()
	if viewer == nil {
		l.Push(t)
		return 1
	}
	if rt.displayReachesForeignRoom(e) {
		l.Push(t) // #253: a display can't itemize a room the viewer isn't standing in
		return 1
	}
	var items []*Entity
	for _, it := range e.contents {
		if isCreature(it) || !e.zone.canSee(viewer, it) {
			continue
		}
		items = append(items, it)
	}
	n := 0
	for _, g := range coalesceItems(items) {
		n++
		rec := l.NewTable()
		rec.RawSetString("item", rt.newHandle(g.rep))
		rec.RawSetString("name", lua.LString(g.rep.Name()))
		rec.RawSetString("long", lua.LString(g.rep.Long()))
		rec.RawSetString("count", lua.LNumber(g.n))
		t.RawSetInt(n, rec)
	}
	l.Push(t)
	return 1
}

// hGroup returns the entity's party/group members as handles. The engine has NO party/group
// model yet (it is Phase-11 progression/social work), so this is a RESERVED STUB that returns
// an empty table — never invents a grouping. When the party model lands, this method reads it
// without any script-API change.
func (rt *luaRuntime) hGroup(l *lua.LState) int {
	if e := resolveHandle(l, 1); e == nil {
		return rt.pushHandleList(l, nil)
	}
	return rt.pushHandleList(l, nil)
}

// hIsEnemy reports whether `other` is a hostile target of the entity. The engine has no
// faction/aggro-relationship model exposed as a predicate yet; the honest read available now
// is "they are different living entities in the same room" — a placeholder the faction/aggro
// follow-up will replace at this one chokepoint. Returns false for an unresolved handle or a
// missing/!living counterpart.
func (rt *luaRuntime) hIsEnemy(l *lua.LState) int {
	e := resolveHandle(l, 1)
	other := resolveHandle(l, 2)
	if e == nil || other == nil || e == other {
		l.Push(lua.LFalse)
		return 1
	}
	// Placeholder relationship: both living and co-located. NOTE: the real faction/aggro
	// predicate lands here (the single is_enemy chokepoint) when that model exists.
	enemy := e.living != nil && other.living != nil && e.location != nil && e.location == other.location
	l.Push(lua.LBool(enemy))
	return 1
}

// hDistance returns a coarse distance between the entity and `other`: 0 if the same entity, 1
// if co-located (same room), else a large sentinel (math.huge-like) for "not reachable in this
// zone's adjacency model". The engine has no room-graph distance metric yet (room coordinates
// are deferred — see the room-coordinates memory), so this is the honest same-room/elsewhere
// split; the metric lands here when coordinates do. Returns nil for an unresolved handle.
func (rt *luaRuntime) hDistance(l *lua.LState) int {
	e := resolveHandle(l, 1)
	other := resolveHandle(l, 2)
	if e == nil || other == nil {
		l.Push(lua.LNil)
		return 1
	}
	switch {
	case e == other:
		l.Push(lua.LNumber(0))
	case e.location != nil && e.location == other.location:
		l.Push(lua.LNumber(1))
	default:
		l.Push(lua.LNumber(999)) // "elsewhere in/beyond the zone" sentinel
	}
	return 1
}

// hCanSee routes the engine's existing visibility chokepoint (Zone.canSee — targeting.go),
// the SAME predicate the resolver + act() leak-surface consult. Today canSee is the trivial
// "everything in scope is visible" filter; when content supplies dark/invis/hidden flags the
// rule lands there and this method inherits it (the builder-visibility follow-up owns that
// chokepoint). Returns false for an unresolved handle or counterpart.
func (rt *luaRuntime) hCanSee(l *lua.LState) int {
	e := resolveHandle(l, 1)
	other := resolveHandle(l, 2)
	if e == nil || other == nil {
		l.Push(lua.LFalse)
		return 1
	}
	l.Push(lua.LBool(e.zone.canSee(e, other)))
	return 1
}

// --- 7.3a comms (message via the existing act()/send; no resource/affect writes) ----------

// hSend sends markup to the entity's player session (if it has one; a mob/item has no session
// and the send is a silent no-op). Read/message only — no state mutation. The markup is
// SCRIPT-SUPPLIED, so it is run through textsan.CleanMarkup at the world boundary (ISSUE-B):
// control/ESC sequences are stripped (terminal-injection defense for a non-telnet sink) while
// legitimate markup/color is preserved.
func (rt *luaRuntime) hSend(l *lua.LState) int {
	if rt.denyInDisplay(l, "send") {
		return 0
	}
	e := resolveHandle(l, 1)
	markup := textsan.CleanMarkup(l.CheckString(2))
	if e == nil {
		return 0
	}
	if pc, ok := sessionOf(e); ok {
		pc.send(textFrame(markup))
	}
	return 0
}

// hAct renders a perspective template to the room from the entity's viewpoint, the engine's
// standard act() messaging. Signature: h:act(tmpl, obj, vict, to) where obj/vict are optional
// entity handles ($p/$N referents) and `to` is an optional recipient selector string
// ("room"/"actor"/"victim"; default "room"). Message-only — act() writes no game state. The
// template is SCRIPT-SUPPLIED, so it is textsan.CleanMarkup'd (ISSUE-B); the '$'-referent
// tokens are ordinary printable chars and survive, only control/ESC sequences are stripped.
func (rt *luaRuntime) hAct(l *lua.LState) int {
	if rt.denyInDisplay(l, "act") {
		return 0
	}
	actor := resolveHandle(l, 1)
	tmpl := textsan.CleanMarkup(l.CheckString(2))
	if actor == nil {
		return 0
	}
	obj := optResolve(l, 3)
	vict := optResolve(l, 4)
	to := actToFromString(l.OptString(5, "room"))
	actor.zone.act(tmpl, actor, obj, vict, "", "", to)
	return 0
}

// hSay makes the entity say text to its room (the standard say templates: "You say,
// '<text>'" to the speaker, "<Name> says, '<text>'" to bystanders). Message-only. Only the
// script-supplied TEXT is cleaned (ISSUE-B) — the engine-generated template is already safe and
// is not re-sanitized.
func (rt *luaRuntime) hSay(l *lua.LState) int {
	if rt.denyInDisplay(l, "say") {
		return 0
	}
	e := resolveHandle(l, 1)
	text := textsan.CleanMarkup(l.CheckString(2))
	if e == nil {
		return 0
	}
	e.zone.act("You say, '$t'", e, nil, nil, text, "", ToActor)
	e.zone.act("$n says, '$t'", e, nil, nil, text, "", ToRoom)
	return 0
}

// hEmote makes the entity emote text to its room ("$n <text>" to bystanders, "You <text>" to
// the actor). Message-only. Only the script-supplied text is cleaned (ISSUE-B).
func (rt *luaRuntime) hEmote(l *lua.LState) int {
	if rt.denyInDisplay(l, "emote") {
		return 0
	}
	e := resolveHandle(l, 1)
	text := textsan.CleanMarkup(l.CheckString(2))
	if e == nil {
		return 0
	}
	e.zone.act("You $t", e, nil, nil, text, "", ToActor)
	e.zone.act("$n $t", e, nil, nil, text, "", ToRoom)
	return 0
}

// optResolve resolves an OPTIONAL handle argument at index n: nil if the slot is absent/nil or
// not a handle. Used for act()'s optional obj/vict referents.
func optResolve(l *lua.LState, n int) *Entity {
	if n > l.GetTop() || l.Get(n) == lua.LNil {
		return nil
	}
	return resolveHandle(l, n)
}

// actToFromString maps a script-supplied recipient selector to an ActTo. Unknown values
// default to ToRoom (the safe, most common broadcast). Closed mapping — never a reflection
// into the engine.
func actToFromString(s string) ActTo {
	switch s {
	case "actor":
		return ToActor
	case "victim":
		return ToVictim
	case "room_except_actor":
		return ToRoomExceptActor
	default:
		return ToRoom
	}
}

// runChunkWithSelf compiles and runs src in the sandbox with `self` bound to a handle for
// entity e — a TRIVIAL trigger context, just enough to exercise the handles + the harm methods
// from a script (slices 7.2/7.3a/7.3c). The full entry points (on(...), ability/affect hooks)
// are slice 7.4; this is not those. It binds `self` AND sets the ENGINE-owned invocation
// context (rt.inv) with e as the invocation actor/source — the sole, non-script-supplied source
// of a harm op's actor (P7-D3 invariant 1). Both are cleared after, so neither `self` nor the
// invocation leaks into an unrelated later call. Re-uses runChunk's budget/deadline chokepoint.
func (rt *luaRuntime) runChunkWithSelf(name, src string, e *Entity) error {
	return rt.runChunkAs(name, src, &luaInvocation{actor: e})
}

// runChunkAs runs src with `self` bound to inv.actor and the engine-owned invocation context
// set to inv (carrying the actor/source + the threaded depth/eventBudget). It is the single
// entry that establishes "who the script acts as" — every Lua harm op reads its effectCtx from
// rt.inv, never from a Lua argument. The invocation is cleared on return (a harm op outside any
// invocation has no actor and no-ops).
func (rt *luaRuntime) runChunkAs(name, src string, inv *luaInvocation) error {
	if rt == nil || rt.L == nil {
		return fmt.Errorf("lua runtime not initialized")
	}
	var self *Entity
	if inv != nil {
		self = inv.actor
	}
	rt.L.SetGlobal("self", rt.newHandle(self))
	rt.inv = inv
	defer func() {
		rt.L.SetGlobal("self", lua.LNil)
		rt.inv = nil
	}()
	return rt.runChunk(name, src)
}
