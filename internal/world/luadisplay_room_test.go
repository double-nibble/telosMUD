package world

import (
	"context"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// luadisplay_room_test.go — #24 part (b): the `room` display surface and the three handle accessors a room
// template needs (self:room():exits() / :occupants() / :room_items()).
//
// The LOAD-BEARING tests here are the occupants() visibility ones. Round 12 established canSee/visibleTo as
// THE perception chokepoint and flagged "room/GMCP occupant lists" as the canSee-BYPASS leak trap: a new
// content-facing accessor that walked room.contents directly would hand a template every hidden/sneaking/
// dark-concealed player in the room. Every concealment mechanic (#28 invisible, #99 dark, #100 hidden,
// #30 wizinvis) is asserted through the accessor, plus the fail-closed no-perspective case.

// roomTmplZone builds a zone with a `room` template, a player in a registered room, and returns both. The
// template is rendered by `look`, so the assertions read the look output.
func roomTmplZone(t *testing.T, body string) (*Zone, *session) {
	t.Helper()
	z := newZone("harm") // "harm" so harmZone-style refs ("harm:room:...") parse as LOCAL
	if body != "" {
		z.defBundle().displayDefs["room"] = body
	}
	room := z.newEntity("harm:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["harm:room:hall"] = room
	s := newTestPlayerEntity(z, "Viewer")
	s.entity.short = "Viewer"
	Move(s.entity, room)
	z.players["Viewer"] = s
	return z, s
}

// --- self:room():exits() ---------------------------------------------------------------------

// TestRoomHandleExits pins the exits() record shape: every authored direction (canonical first, then the
// data-only maze directions sorted), a `ref` string always, and `to` = a room HANDLE for a local destination
// / the bare ref STRING for a cross-zone or dangling one (T7: no handle names a foreign-zone entity).
func TestRoomHandleExits(t *testing.T) {
	z, s := roomTmplZone(t, `
		local out = {}
		for _, e in ipairs(self:room():exits()) do
			local to = "?"
			if type(e.to) == "string" then to = "REF:" .. e.to else to = "HANDLE:" .. e.to:name() end
			out[#out+1] = e.dir .. "|" .. e.ref .. "|" .. to
		end
		return table.concat(out, "\n")`)

	// A local destination (resolves to a handle), a cross-zone one (stays a ref string), a dangling local ref
	// (no such room => ref string), and a NON-CANONICAL data-only maze exit (must still be enumerated).
	market := z.newEntity("harm:room:market")
	market.short = "The Market"
	Add(market, &Room{exits: map[string]ProtoRef{}})
	z.rooms["harm:room:market"] = market

	hall := z.rooms["harm:room:hall"]
	hall.room.exits["north"] = "harm:room:market"   // local -> handle
	hall.room.exits["east"] = "darkwood:room:grove" // cross-zone -> ref string
	hall.room.exits["west"] = "harm:room:nowhere"   // dangling -> ref string
	hall.room.exits["portal"] = "harm:room:market"  // data-only maze exit (not a movement direction)
	hall.room.exits["down"] = "harm:room:market"    // canonical, so it orders before the non-canonical "portal"

	z.dispatch(s, "look")
	out := drainAllText(s.out)

	// Canonical order first (north, east, west, down per dirOrder N/E/S/W/U/D), then the sorted non-canonical
	// tail. The data-only "portal" exit MUST appear — it is a real edge of the room graph (the maze model).
	wantOrder := []string{"north|", "east|", "west|", "down|", "portal|"}
	at := -1
	for _, want := range wantOrder {
		i := strings.Index(out, want)
		if i < 0 {
			t.Fatalf("exits() omitted %q (data-only exits must be enumerated too); got:\n%s", want, out)
		}
		if i < at {
			t.Fatalf("exits() out of order at %q (canonical dirOrder first, then sorted extras); got:\n%s", want, out)
		}
		at = i
	}
	if !strings.Contains(out, "north|harm:room:market|HANDLE:The Market") {
		t.Fatalf("a LOCAL exit destination must bind as a walkable room handle; got:\n%s", out)
	}
	if !strings.Contains(out, "east|darkwood:room:grove|REF:darkwood:room:grove") {
		t.Fatalf("a CROSS-ZONE exit must bind `to` as the bare ref string (no foreign-zone handle, T7); got:\n%s", out)
	}
	if !strings.Contains(out, "west|harm:room:nowhere|REF:harm:room:nowhere") {
		t.Fatalf("a DANGLING exit ref must bind `to` as the ref string, not nil/panic; got:\n%s", out)
	}
}

// TestRoomHandleExitsEmpty: a room with no exits (and a non-room subject) yields an empty table, never nil —
// so `for _, e in ipairs(self:room():exits())` is always safe.
func TestRoomHandleExitsEmpty(t *testing.T) {
	_, s := roomTmplZone(t, `
		local n = #self:room():exits()
		local m = #self:exits()   -- a PLAYER has no Room component: empty, not an error
		return "exits=" .. n .. " selfexits=" .. m`)
	dispatchLook(t, s)
	if out := drainAllText(s.out); !strings.Contains(out, "exits=0 selfexits=0") {
		t.Fatalf("exits() on an exit-less room / a non-room handle must be an empty table: %q", out)
	}
}

// --- self:room():occupants() — THE security surface --------------------------------------------

// occupantsZone builds a zone whose `room` template dumps the visible occupant names, plus a viewer.
func occupantsZone(t *testing.T) (*Zone, *Entity, *Entity, *session) {
	t.Helper()
	z, _, room := harmZone(t)
	z.defBundle().displayDefs["room"] = `
		local out = {}
		for _, o in ipairs(self:room():occupants()) do out[#out+1] = o:name() end
		return "OCC[" .. table.concat(out, ",") .. "]"`
	viewer := harmPlayer(z, room, "Viewer")
	other := harmPlayer(z, room, "Sneak")
	return z, viewer, other, z.players["Viewer"]
}

// lookOcc renders the room template for the viewer and returns the OCC[...] payload.
func lookOcc(t *testing.T, z *Zone, s *session) string {
	t.Helper()
	drainAllText(s.out)
	z.dispatch(s, "look")
	return drainAllText(s.out)
}

// TestRoomOccupantsExcludesConcealed is THE load-bearing security test for #24: an occupant the viewer cannot
// see NEVER reaches the content template. Each concealment mechanic is exercised through the accessor, and
// each of the engine's senses is shown to pierce it — so occupants() demonstrably routes canSee rather than
// re-implementing (or bypassing) the rule.
func TestRoomOccupantsExcludesConcealed(t *testing.T) {
	t.Run("baseline: a visible occupant is listed", func(t *testing.T) {
		z, _, _, s := occupantsZone(t)
		if out := lookOcc(t, z, s); !strings.Contains(out, "OCC[Sneak]") {
			t.Fatalf("an unconcealed occupant must be listed: %q", out)
		}
	})

	t.Run("#100 hidden: omitted, and sense_hidden pierces", func(t *testing.T) {
		z, viewer, sneak, s := occupantsZone(t)
		setFlag(sneak, flagHidden, true)
		if out := lookOcc(t, z, s); strings.Contains(out, "Sneak") {
			t.Fatalf("LEAK: a hidden occupant reached the room template: %q", out)
		}
		setFlag(viewer, flagSenseHidden, true)
		if out := lookOcc(t, z, s); !strings.Contains(out, "OCC[Sneak]") {
			t.Fatalf("sense_hidden must pierce a hidden occupant: %q", out)
		}
	})

	t.Run("#28 invisible: omitted, and detect_invis pierces", func(t *testing.T) {
		z, viewer, sneak, s := occupantsZone(t)
		setFlag(sneak, flagInvisible, true)
		if out := lookOcc(t, z, s); strings.Contains(out, "Sneak") {
			t.Fatalf("LEAK: an invisible occupant reached the room template: %q", out)
		}
		setFlag(viewer, flagDetectInvis, true)
		if out := lookOcc(t, z, s); !strings.Contains(out, "OCC[Sneak]") {
			t.Fatalf("detect_invis must pierce an invisible occupant: %q", out)
		}
	})

	t.Run("#99 dark room: infravision sees, an ordinary viewer never reaches the template", func(t *testing.T) {
		z, viewer, _, s := occupantsZone(t)
		markRoomFlag(viewer.location, flagDark)

		// An ordinary viewer is short-circuited by lookRoom's pitch-black gate — the ENGINE rule sits ABOVE
		// the content template, so no pack can template its way into a dark room.
		if out := lookOcc(t, z, s); !strings.Contains(out, "pitch black") || strings.Contains(out, "OCC[") {
			t.Fatalf("a dark room must render pitch-black and NEVER run the room template: %q", out)
		}
		// With infravision the viewer reaches the template — and sees the occupant (the same chokepoint).
		setFlag(viewer, flagInfravision, true)
		if out := lookOcc(t, z, s); !strings.Contains(out, "OCC[Sneak]") {
			t.Fatalf("infravision must see the occupant through the template: %q", out)
		}
	})

	t.Run("#99 dark room: an infravision viewer still cannot see a HIDDEN occupant", func(t *testing.T) {
		z, viewer, sneak, s := occupantsZone(t)
		markRoomFlag(viewer.location, flagDark)
		setFlag(viewer, flagInfravision, true)
		setFlag(sneak, flagHidden, true)
		if out := lookOcc(t, z, s); strings.Contains(out, "Sneak") {
			t.Fatalf("LEAK: darkness+infravision must not bypass the hidden check: %q", out)
		}
	})

	t.Run("holylight sees everyone", func(t *testing.T) {
		z, viewer, sneak, s := occupantsZone(t)
		setFlag(sneak, flagHidden, true)
		setFlag(sneak, flagInvisible, true)
		setFlag(viewer, flagHolylight, true)
		if out := lookOcc(t, z, s); !strings.Contains(out, "OCC[Sneak]") {
			t.Fatalf("holylight must see every concealed occupant: %q", out)
		}
	})

	t.Run("the viewer is excluded (look semantics)", func(t *testing.T) {
		z, _, _, s := occupantsZone(t)
		if out := lookOcc(t, z, s); strings.Contains(out, "Viewer") {
			t.Fatalf("occupants() must exclude the viewer themselves, like `look`: %q", out)
		}
	})

	t.Run("items and corpses are not occupants", func(t *testing.T) {
		z, viewer, _, s := occupantsZone(t)
		addTestItem(z, viewer.location, "a rusty sword", []string{"sword"})
		out := lookOcc(t, z, s)
		if strings.Contains(out, "sword") {
			t.Fatalf("occupants() must list only LIVING things, not ground items: %q", out)
		}
		if !strings.Contains(out, "OCC[Sneak]") {
			t.Fatalf("occupants() lost the living occupant: %q", out)
		}
	})

	t.Run("a mob is an occupant", func(t *testing.T) {
		z, viewer, _, s := occupantsZone(t)
		harmMob(z, viewer.location, "a goblin")
		if out := lookOcc(t, z, s); !strings.Contains(out, "a goblin") {
			t.Fatalf("occupants() must include mobs (Living, not PlayerControlled): %q", out)
		}
	})
}

// TestRoomOccupantsFailClosedWithoutViewer proves the fail-closed branch: with NO invocation actor there is no
// perspective to filter from, so occupants() discloses NOTHING rather than inheriting visibleTo's
// "nil viewer => conceal nothing" system-render default. (visibleTo's own doc comment obliges every
// room-level surface to make this decision itself; this is that decision, asserted.)
func TestRoomOccupantsFailClosedWithoutViewer(t *testing.T) {
	z, rt, room := harmZone(t)
	harmPlayer(z, room, "Alice")
	harmPlayer(z, room, "Bob")

	ch := rt.chunkFor("test:occ", `
		local out = {}
		for _, o in ipairs(self:occupants()) do out[#out+1] = o:name() end
		return "OCC[" .. table.concat(out, ",") .. "]"`)
	binds := map[string]lua.LValue{"self": rt.newHandle(room)}

	// An invocation with NO actor: the accessor has no viewer to answer as.
	got, ok := rt.invokeForString(ch, &luaInvocation{}, binds)
	if !ok {
		t.Fatalf("the chunk should still run cleanly, just disclose nothing; ok=false")
	}
	if got != "OCC[]" {
		t.Fatalf("no invocation actor must fail CLOSED to an empty occupant list, got %q "+
			"(an unfiltered room roster is the canSee-bypass leak trap)", got)
	}

	// Sanity: the SAME chunk with a real actor does see the other player — so the empty result above is the
	// fail-closed branch, not a broken accessor.
	got, ok = rt.invokeForString(ch, &luaInvocation{actor: z.players["Alice"].entity}, binds)
	if !ok || !strings.Contains(got, "Bob") {
		t.Fatalf("with a viewer, occupants() must list the visible other player; got %q ok=%v", got, ok)
	}
	if strings.Contains(got, "Alice") {
		t.Fatalf("occupants() must exclude the viewer: %q", got)
	}
}

// TestContentsStaysUnfilteredOccupantsIsFiltered pins the split in a MECHANICS invocation (the default —
// inv.display is false here):
//
//   - contents() is the MECHANICS traversal and stays UNFILTERED (an AoE must hit the hidden rogue; a
//     room-scoped affect must land on the invisible mage). Filtering it would silently break those.
//   - occupants() is the PERCEPTION traversal and is canSee-filtered — the accessor a display surface uses.
//
// #250 additionally closes the leak the OLD comment documented as accepted ("a room template reaching for
// contents() CAN enumerate concealed occupants"): in a DISPLAY invocation contents() of a non-viewer container
// is now canSee-filtered too (TestContentsFilteredInDisplayContext below). This test holds the MECHANICS half.
func TestContentsStaysUnfilteredOccupantsIsFiltered(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	sneak := harmPlayer(z, room, "Sneak")
	setFlag(sneak, flagHidden, true)

	rt := z.lua
	ch := rt.chunkFor("test:split", `
		local c, o = {}, {}
		for _, e in ipairs(self:contents())  do c[#c+1] = e:name() end
		for _, e in ipairs(self:occupants()) do o[#o+1] = e:name() end
		return "C[" .. table.concat(c, ",") .. "] O[" .. table.concat(o, ",") .. "]"`)
	// display:false (a mechanics invocation) — contents() must be raw.
	got, ok := rt.invokeForString(ch, &luaInvocation{actor: viewer},
		map[string]lua.LValue{"self": rt.newHandle(room)})
	if !ok {
		t.Fatal("chunk failed to run")
	}
	if !strings.Contains(got, "C[Viewer,Sneak]") {
		t.Fatalf("contents() must stay UNFILTERED (mechanics: AoE/room affects reach concealed entities): %q", got)
	}
	if !strings.Contains(got, "O[]") {
		t.Fatalf("occupants() must be canSee-FILTERED (perception: the hidden player is omitted; the viewer "+
			"excludes themselves, so the list is empty): %q", got)
	}
}

// TestContentsFilteredInDisplayContext is the #250 regression: in a DISPLAY invocation (inv.display=true, set
// by renderDisplaySheet/renderDisplayList), contents() of a container that is NOT the viewer is canSee-filtered,
// so a `room` template that reaches for self:room():contents() instead of occupants()/room_items() can no
// longer enumerate a concealed occupant. Same fixture as the mechanics test above; only the invocation context
// differs — proving the split is the CONTEXT, not the accessor.
func TestContentsFilteredInDisplayContext(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	sneak := harmPlayer(z, room, "Sneak")
	setFlag(sneak, flagHidden, true)

	rt := z.lua
	ch := rt.chunkFor("display:room:leak", `
		local c = {}
		for _, e in ipairs(self:contents()) do c[#c+1] = e:name() end
		return "C[" .. table.concat(c, ",") .. "]"`)
	// display:true (a display render) — the hidden Sneak must be absent from a NON-viewer container's contents.
	got, ok := rt.invokeForString(ch, &luaInvocation{actor: viewer, display: true},
		map[string]lua.LValue{"self": rt.newHandle(room)})
	if !ok {
		t.Fatal("chunk failed to run")
	}
	if strings.Contains(got, "Sneak") {
		t.Fatalf("#250: a DISPLAY render's contents() of the ROOM leaked a concealed occupant: %q", got)
	}
	if !strings.Contains(got, "Viewer") {
		t.Fatalf("the viewer (who canSee themselves) must still appear in the room's display contents: %q", got)
	}

	// The fail-closed guard: a display render with no viewer perspective discloses nothing.
	got2, ok := rt.invokeForString(ch, &luaInvocation{actor: nil, display: true},
		map[string]lua.LValue{"self": rt.newHandle(room)})
	if !ok {
		t.Fatal("chunk failed to run (nil-viewer)")
	}
	if got2 != "C[]" {
		t.Fatalf("#250 fail-closed: a display contents() with no viewer must disclose nothing; got %q", got2)
	}
}

// TestDisplayContentsOfSelfInventoryStaysRaw guards the inventory template against over-filtering: in a DISPLAY
// render, self:contents() (the viewer's OWN inventory: the container IS the viewer) stays RAW — a player always
// sees their own carried items, even an INVISIBLE one that canSee would hide in a room. Only a NON-viewer
// container is filtered (#250). Without the `e != viewer` guard this would silently drop a player's own
// invisible item from their inventory sheet.
func TestDisplayContentsOfSelfInventoryStaysRaw(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	ring := addTestItem(z, viewer, "an invisible ring", []string{"ring"})
	setFlag(ring, flagInvisible, true) // the viewer has no detect-invis; canSee(viewer, ring) would be false

	rt := z.lua
	ch := rt.chunkFor("display:inventory:self", `
		local c = {}
		for _, e in ipairs(self:contents()) do c[#c+1] = e:name() end
		return "C[" .. table.concat(c, ",") .. "]"`)
	got, ok := rt.invokeForString(ch, &luaInvocation{actor: viewer, display: true},
		map[string]lua.LValue{"self": rt.newHandle(viewer)})
	if !ok {
		t.Fatal("chunk failed to run")
	}
	if !strings.Contains(got, "an invisible ring") {
		t.Fatalf("#250: self:contents() (own inventory) must stay RAW in a display — the player's own invisible "+
			"item was wrongly filtered out: %q", got)
	}
}

// TestMudScanFilteredInDisplayContext is the #250 sibling for the GLOBAL scan primitive: mud.scan(room) also
// returns a room's RAW contents, so a display template reaching mud.scan(self:room()) must be canSee-filtered
// exactly like self:room():contents() — else the leak reopens through the other door. A mechanics scan stays raw.
func TestMudScanFilteredInDisplayContext(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	sneak := harmPlayer(z, room, "Sneak")
	setFlag(sneak, flagHidden, true)

	rt := z.lua
	ch := rt.chunkFor("test:scan", `
		local c = {}
		for _, e in ipairs(mud.scan(self:room())) do c[#c+1] = e:name() end
		return "S[" .. table.concat(c, ",") .. "]"`)
	// display:true — mud.scan of the room must drop the concealed occupant.
	got, ok := rt.invokeForString(ch, &luaInvocation{actor: viewer, display: true},
		map[string]lua.LValue{"self": rt.newHandle(viewer)})
	if !ok {
		t.Fatal("chunk failed to run (display)")
	}
	if strings.Contains(got, "Sneak") {
		t.Fatalf("#250: mud.scan in a DISPLAY render leaked a concealed occupant: %q", got)
	}
	// display:false — a mechanics scan stays raw (a script scanning a room must reach a hidden entity).
	raw, ok := rt.invokeForString(ch, &luaInvocation{actor: viewer},
		map[string]lua.LValue{"self": rt.newHandle(viewer)})
	if !ok {
		t.Fatal("chunk failed to run (mechanics)")
	}
	if !strings.Contains(raw, "Sneak") {
		t.Fatalf("mud.scan in a MECHANICS invocation must stay raw: %q", raw)
	}
}

// TestRoomDisplayTemplateThroughRenderPathFiltersContents pins the PRODUCTION wiring (#250): display:true is
// set by renderDisplaySheet ITSELF, not just by hand in the harness. A leaky `room` template reaching for
// self:room():contents(), rendered through the REAL renderDisplaySheet("room", viewer) path, must NOT disclose
// a concealed occupant. Reverting the display:true at the render site (luadisplay.go) — which the harness tests
// above cannot catch — fails this.
func TestRoomDisplayTemplateThroughRenderPathFiltersContents(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	sneak := harmPlayer(z, room, "Sneak")
	setFlag(sneak, flagHidden, true)

	z.defBundle().displayDefs["room"] = `
		local c = {}
		for _, e in ipairs(self:room():contents()) do c[#c+1] = e:name() end
		return "ROOM[" .. table.concat(c, ",") .. "]"`

	got, ok := z.renderDisplaySheet("room", viewer)
	if !ok {
		t.Fatal("the room template should have rendered")
	}
	if strings.Contains(got, "Sneak") {
		t.Fatalf("#250 wiring: renderDisplaySheet must set display:true — a leaky room template disclosed a "+
			"concealed occupant through the real render path: %q", got)
	}
	if !strings.Contains(got, "Viewer") {
		t.Fatalf("the viewer must still appear in their own room's display contents: %q", got)
	}
}

// --- self:room():room_items() -----------------------------------------------------------------

// TestRoomHandleCoalescedItems pins the coalesced ground-item accessor: identical items merge into ONE record
// carrying a count (the SAME rule `look` uses), while materials/containers and delta-varied items never merge.
func TestRoomHandleCoalescedItems(t *testing.T) {
	z, s := roomTmplZone(t, `
		local out = {}
		for _, g in ipairs(self:room():room_items()) do
			out[#out+1] = g.name .. " x" .. g.count .. " [" .. g.item:name() .. "]"
		end
		return "ITEMS{" .. table.concat(out, ";") .. "}"`)
	room := s.entity.location

	// Three identical swords (same proto, same delta) coalesce; the lone shield stands alone.
	for i := 0; i < 3; i++ {
		addTestItem(z, room, "a rusty sword", []string{"sword"})
	}
	addTestItem(z, room, "a dented shield", []string{"shield"})
	// A container NEVER coalesces (its hidden contents differ) — two chests are two records.
	addTestItem(z, room, "a chest", []string{"chest"}, &Container{})
	addTestItem(z, room, "a chest", []string{"chest"}, &Container{})

	z.dispatch(s, "look")
	out := drainAllText(s.out)

	if !strings.Contains(out, "a rusty sword x3 [a rusty sword]") {
		t.Fatalf("identical ground items must coalesce with a count, and expose a representative handle: %q", out)
	}
	if !strings.Contains(out, "a dented shield x1") {
		t.Fatalf("a lone item must be a count-1 record: %q", out)
	}
	if strings.Contains(out, "a chest x2") {
		t.Fatalf("containers must never coalesce (their contents differ): %q", out)
	}
	if n := strings.Count(out, "a chest x1"); n != 2 {
		t.Fatalf("two containers must be two count-1 records, got %d: %q", n, out)
	}
}

// TestRoomItemsExcludeCreatures: room_items() is the ground-item view, so players/mobs never appear in it
// (they are occupants()) — the same split `look` renders.
func TestRoomItemsExcludeCreatures(t *testing.T) {
	z, s := roomTmplZone(t, `
		local out = {}
		for _, g in ipairs(self:room():room_items()) do out[#out+1] = g.name end
		return "ITEMS{" .. table.concat(out, ";") .. "}"`)
	room := s.entity.location
	harmMob(z, room, "a goblin")
	addTestItem(z, room, "a rusty sword", []string{"sword"})

	z.dispatch(s, "look")
	out := drainAllText(s.out)
	if strings.Contains(out, "goblin") {
		t.Fatalf("room_items() must exclude creatures (they are occupants()): %q", out)
	}
	if !strings.Contains(out, "a rusty sword") {
		t.Fatalf("room_items() lost the ground item: %q", out)
	}
}

// TestCoalesceItemsMatchesLookLines is the anti-drift gate: room_items()'s counts come from the SAME
// coalesceItems rule the built-in `look` line renders, so a template can never disagree with the fallback.
func TestCoalesceItemsMatchesLookLines(t *testing.T) {
	z := newZone("coalesce")
	room := z.newEntity("coalesce:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	items := []*Entity{
		addTestItem(z, room, "a rusty sword", []string{"sword"}),
		addTestItem(z, room, "a rusty sword", []string{"sword"}),
		addTestItem(z, room, "a dented shield", []string{"shield"}),
	}
	groups := coalesceItems(items)
	if len(groups) != 2 || groups[0].n != 2 || groups[1].n != 1 {
		t.Fatalf("coalesceItems: want [sword x2, shield x1], got %+v", groups)
	}
	if groups[0].rep != items[0] {
		t.Fatal("the representative must be the FIRST item of the run (appearance order)")
	}
	lines := coalesceItemLines(items, (*Entity).Name)
	if len(lines) != 2 || !strings.Contains(lines[0], "(2)") {
		t.Fatalf("coalesceItemLines must still render the counted line: %v", lines)
	}
}

// --- the `room` surface: render + fallback -----------------------------------------------------

// dispatchLook drains any pending frames, runs `look`, and leaves the output on the channel.
func dispatchLook(t *testing.T, s *session) {
	t.Helper()
	drainAllText(s.out)
	s.entity.zone.dispatch(s, "look")
}

// TestRoomSurfaceFallbacks: with NO `room` template (or a broken one) `look` renders the built-in room
// description, unchanged — the verb works for any pack.
func TestRoomSurfaceFallbacks(t *testing.T) {
	t.Run("no template", func(t *testing.T) {
		z, s := roomTmplZone(t, "")
		z.rooms["harm:room:hall"].short = "The Hall"
		z.rooms["harm:room:hall"].long = "A dusty hall."
		z.dispatch(s, "look")
		out := drainAllText(s.out)
		if !strings.Contains(out, "The Hall") || !strings.Contains(out, "Exits: none") {
			t.Fatalf("no-template look must render the built-in room description: %q", out)
		}
	})
	t.Run("non-string template falls back", func(t *testing.T) {
		z, s := roomTmplZone(t, `return 42`)
		z.rooms["harm:room:hall"].short = "The Hall"
		z.dispatch(s, "look")
		if out := drainAllText(s.out); !strings.Contains(out, "The Hall") {
			t.Fatalf("a non-string room template must fail closed to the built-in: %q", out)
		}
	})
	t.Run("erroring template falls back", func(t *testing.T) {
		z, s := roomTmplZone(t, `error("boom")`)
		z.rooms["harm:room:hall"].short = "The Hall"
		z.dispatch(s, "look")
		if out := drainAllText(s.out); !strings.Contains(out, "The Hall") {
			t.Fatalf("an erroring room template must fail closed to the built-in: %q", out)
		}
	})
}

// TestRoomTemplateDoesNotAffectLookAtTarget: `look <thing>` is a DIFFERENT surface (lookAt) and must keep its
// built-in behavior even when a `room` template exists.
func TestRoomTemplateDoesNotAffectLookAtTarget(t *testing.T) {
	z, s := roomTmplZone(t, `return "CUSTOM-ROOM"`)
	sword := addTestItem(z, s.entity.location, "a rusty sword", []string{"sword"})
	sword.long = "A rusty sword lies here, pitted with age."

	z.dispatch(s, "look")
	if out := drainAllText(s.out); !strings.Contains(out, "CUSTOM-ROOM") {
		t.Fatalf("a bare look must render the room template: %q", out)
	}
	z.dispatch(s, "look sword")
	out := drainAllText(s.out)
	if strings.Contains(out, "CUSTOM-ROOM") {
		t.Fatalf("`look <target>` must NOT render the room template: %q", out)
	}
	if !strings.Contains(out, "pitted with age") {
		t.Fatalf("`look <target>` must still render the target's long: %q", out)
	}
}

// TestRoomSurfaceIsolation: defining `room` leaves the other surfaces on their built-ins (the wrong-surface-key
// bug class).
func TestRoomSurfaceIsolation(t *testing.T) {
	z, s := roomTmplZone(t, `return "CUSTOM-ROOM"`)
	z.dispatch(s, "inventory")
	if out := drainAllText(s.out); !strings.Contains(out, "You are carrying") {
		t.Fatalf("inventory must stay on its built-in when only `room` is templated: %q", out)
	}
}

// TestRoomSurfaceIsConsulted guards the defineGlobals dead-seam warning: `room` and `who` must be registered as
// consulted, or an author's template silently never renders.
func TestRoomSurfaceIsConsulted(t *testing.T) {
	for _, surface := range []string{"score", "inventory", "equipment", "room", "who"} {
		if !consultedDisplaySurfaces[surface] {
			t.Fatalf("surface %q is wired to a command but missing from consultedDisplaySurfaces "+
				"(defineGlobals would warn a real template is a dead seam)", surface)
		}
	}
}

// TestRoomTemplateThroughZoneLoop is the in-package integration pass: a real demo zone, its Run loop, a joining
// player. lookRoom is the ONE room-render chokepoint, so the template must serve the ARRIVAL look and the
// post-move look, not just an explicit `look` — and the exits/occupants accessors must resolve against the real
// demo room graph (which has genuine cross-room exits).
func TestRoomTemplateThroughZoneLoop(t *testing.T) {
	sh := NewDemoShard()
	z := sh.Zone()
	z.defBundle().displayDefs["room"] = `
		local dirs = {}
		for _, e in ipairs(self:room():exits()) do dirs[#dirs+1] = e.dir end
		return "ROOMTMPL " .. self:room():name() .. " exits=" .. table.concat(dirs, "/")`
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go z.Run(ctx)

	s := newTestPlayerEntity(z, "Wanderer")
	z.post(joinMsg{s: s})
	// The ARRIVAL look renders the template (not the built-in description) and sees the real exit set.
	waitMarkup(t, s, "ROOMTMPL")

	// And so does the look after a move.
	drain(s)
	z.post(inputMsg{id: "Wanderer", line: "north"})
	waitMarkup(t, s, "ROOMTMPL")
}

// TestRoomTemplateStillEmitsGMCP: the Room.Info emit is a client-contract side effect of `look`, so it must
// survive on the template path exactly as on the built-in path (a minimap must not go dark because a pack
// authored a room sheet).
func TestRoomTemplateStillEmitsGMCP(t *testing.T) {
	z, s := roomTmplZone(t, `return "CUSTOM-ROOM"`)
	z.dispatch(s, "look")
	var sawGMCP bool
	for len(s.out) > 0 {
		f := <-s.out
		if g := f.GetGmcp(); g != nil && g.GetPkg() == "Room.Info" {
			sawGMCP = true
		}
	}
	if !sawGMCP {
		t.Fatal("the room template path must still emit GMCP Room.Info (minimap contract)")
	}
}
