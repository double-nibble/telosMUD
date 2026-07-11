package world

import (
	"fmt"
	"strings"
	"testing"
)

// overworld_map_test.go — the overworld minimap content (#361): the pack-global `room` display template
// that draws a 5x5 minimap for the plains when the viewer has the `overworld` toggle on, plus the
// Midgaard/Darkwood wiring. Structural assertions (not byte-exact) so the rendering can be tuned freely.

// overworldViewer builds the demo overworld zone with a player who has the `overworld` toggle ON, and a
// helper that moves them to a room and returns the rendered room sheet (+ whether the template took over).
func overworldViewer(t *testing.T) (*Zone, *session, func(ref string) (string, bool)) {
	t.Helper()
	z := newDemoZone("overworld", newProtoCache())
	s := newTestPlayerEntity(z, "Ranger")
	commsOf(s).toggleOverride["overworld"] = true
	render := func(ref string) (string, bool) {
		Move(s.entity, z.rooms[ProtoRef(ref)])
		return z.renderDisplaySheet("room", s.entity)
	}
	return z, s, render
}

// TestOverworldMapCentresOnPlayer: deep in the plains the template takes over, marks the player, and drops
// the room description below the map.
func TestOverworldMapCentresOnPlayer(t *testing.T) {
	_, _, render := overworldViewer(t)
	sheet, ok := render("overworld:room:c2_r10")
	if !ok {
		t.Fatal("the overworld template did not take over a plains room for a toggle-on viewer")
	}
	if !strings.Contains(sheet, "@") {
		t.Fatalf("map has no player marker (@):\n%s", sheet)
	}
	// The room's description prints below the map (not its exit list).
	if !strings.Contains(sheet, "windswept") {
		t.Fatalf("map is missing the room description below it:\n%s", sheet)
	}
	if strings.Contains(sheet, "Exits:") {
		t.Fatalf("map should NOT list exits:\n%s", sheet)
	}
}

// TestOverworldMapEdgeLabels: at the south gate the Midgaard label box appears; at the north edge, Darkwood.
func TestOverworldMapEdgeLabels(t *testing.T) {
	_, _, render := overworldViewer(t)
	south, _ := render("overworld:room:c2_r0")
	if !strings.Contains(south, "Midgaard") {
		t.Fatalf("south-gate map is missing the Midgaard label:\n%s", south)
	}
	north, _ := render("overworld:room:c2_r19")
	if !strings.Contains(north, "Darkwood") {
		t.Fatalf("north-edge map is missing the Darkwood label:\n%s", north)
	}
	// Deep in the middle, neither city is in view.
	mid, _ := render("overworld:room:c2_r10")
	if strings.Contains(mid, "Midgaard") || strings.Contains(mid, "Darkwood") {
		t.Fatalf("mid-plains map should show no city label:\n%s", mid)
	}
}

// TestOverworldMapLandmarkIcons: a landmark room renders its icon (lake ~, hill ^, house H) when in view.
func TestOverworldMapLandmarkIcons(t *testing.T) {
	_, _, render := overworldViewer(t)
	// The lake sits at (1,6)/(2,6) and the hill at (3,9); standing at (2,6) puts the lake at centre and the
	// hill within the 5x5 window to the north-east.
	sheet, _ := render("overworld:room:c2_r6")
	if !strings.Contains(sheet, "~") {
		t.Fatalf("lake icon (~) not rendered near the lake:\n%s", sheet)
	}
	// Standing next to the house (4,13) shows H.
	house, _ := render("overworld:room:c3_r13")
	if !strings.Contains(house, "H") {
		t.Fatalf("house icon (H) not rendered next to the cottage:\n%s", house)
	}
}

// TestOverworldMapDoesNotDiscloseForeignOccupants documents (and guards) the current SECURE behaviour: a
// display template may not read a FOREIGN room's occupants (#253 anti-scry), so a mob in an ADJACENT room is
// NOT drawn on the map. Nearby-creature markers are a deferred follow-up needing a purpose-built disclosure
// primitive. This test pins that the template does not leak an adjacent creature today.
func TestOverworldMapDoesNotDiscloseForeignOccupants(t *testing.T) {
	z, _, render := overworldViewer(t)
	mob := z.newEntity("overworld:mob:probe")
	Add(mob, &Living{})
	mob.short = "a test beast"
	Move(mob, z.rooms["overworld:room:c2_r11"]) // one room north of the viewer's centre

	sheet, ok := render("overworld:room:c2_r10")
	if !ok {
		t.Fatal("template did not render")
	}
	// The adjacent mob must NOT surface as a marker (the #253 guard makes h:occupants() empty for a foreign
	// room). The only non-map glyphs are the player @ and any landmark; `!` is reserved for the deferred
	// nearby-creature feature, so it must be absent here.
	if strings.Contains(sheet, "!") {
		t.Fatalf("map disclosed an adjacent-room creature (violates the #253 display anti-scry guard):\n%s", sheet)
	}
}

// TestOverworldToggleOffFallsBack: with the toggle OFF, the template returns nil so the built-in room
// render (name/desc/exits) is used — the plains walk as ordinary rooms.
func TestOverworldToggleOffFallsBack(t *testing.T) {
	z := newDemoZone("overworld", newProtoCache())
	s := newTestPlayerEntity(z, "Ranger") // toggle NOT set -> default off
	Move(s.entity, z.rooms["overworld:room:c2_r10"])
	if _, ok := z.renderDisplaySheet("room", s.entity); ok {
		t.Fatal("the overworld template took over even though the `overworld` toggle is OFF")
	}
}

// TestOverworldTemplateSkipsNonPlainsRooms: the pack-global room template must return nil for a non-plains
// room (e.g. Midgaard), so those zones keep the built-in render even for a toggle-on player.
func TestOverworldTemplateSkipsNonPlainsRooms(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := newTestPlayerEntity(z, "Ranger")
	commsOf(s).toggleOverride["overworld"] = true // toggle ON, but this is not the plains
	Move(s.entity, z.rooms["midgaard:room:temple"])
	if _, ok := z.renderDisplaySheet("room", s.entity); ok {
		t.Fatal("the overworld template took over a Midgaard room (must be plains-only)")
	}
}

// TestOverworldWiring: the city↔plains↔forest gateway exits are wired both ways.
func TestOverworldWiring(t *testing.T) {
	over := newDemoZone("overworld", newProtoCache())
	// Entry room bridges back to Midgaard (enter/south) and up into the grid (north).
	gate := over.rooms["overworld:room:c2_r0"].room
	if gate.exits["enter"] != "midgaard:room:market" {
		t.Fatalf("c2_r0 enter = %q, want midgaard:room:market", gate.exits["enter"])
	}
	if gate.exits["north"] != "overworld:room:c2_r1" {
		t.Fatalf("c2_r0 north = %q, want overworld:room:c2_r1", gate.exits["north"])
	}
	// North-edge room bridges into Darkwood.
	top := over.rooms["overworld:room:c2_r19"].room
	if top.exits["enter"] != "darkwood:room:grove" {
		t.Fatalf("c2_r19 enter = %q, want darkwood:room:grove", top.exits["enter"])
	}
	// Midgaard's market and Darkwood's grove each open onto the plains.
	mid := newDemoZone("midgaard", newProtoCache())
	if mid.rooms["midgaard:room:market"].room.exits["exit"] != "overworld:room:c2_r0" {
		t.Fatal("market `exit` does not lead to the overworld entry room")
	}
	dark := newDemoZone("darkwood", newProtoCache())
	if dark.rooms["darkwood:room:grove"].room.exits["exit"] != "overworld:room:c2_r19" {
		t.Fatal("grove `exit` does not lead to the overworld north edge")
	}
}

// TestOverworldInZoneTraversalRedrawsMap: walking north within the plains moves the player and the map
// redraws (via lookRoom) around the new centre — the core "map scrolls, player stays centred" behaviour.
func TestOverworldInZoneTraversalRedrawsMap(t *testing.T) {
	z := newDemoZone("overworld", newProtoCache())
	s := newTestPlayerEntity(z, "Ranger")
	commsOf(s).toggleOverride["overworld"] = true
	Move(s.entity, z.rooms["overworld:room:c2_r0"])

	z.move(s, "north") // c2_r0 -> c2_r1
	if got := string(s.entity.location.proto); got != "overworld:room:c2_r1" {
		t.Fatalf("north from c2_r0 landed at %s, want c2_r1", got)
	}
	// The redraw on arrival is a plains map (player still centred).
	sheet, ok := z.renderDisplaySheet("room", s.entity)
	if !ok || !strings.Contains(sheet, "@") {
		t.Fatalf("map did not redraw around the new position (ok=%v):\n%s", ok, sheet)
	}
}

// --- unit tests for the two new handle reads (#361) ------------------------------------------

// TestHandleRoomFlagAndLong exercises has_room_flag (room content flags, distinct from has_flag's Living
// flags) and long (the entity's long description) directly from Lua.
func TestHandleRoomFlagAndLong(t *testing.T) {
	z, rt, self := handleTestZone(t)
	// Flag the guard's room and give it a long, then read both from Lua.
	room := self.location
	room.room.namedFlags = map[string]bool{"overworld": true}
	room.long = "A long grassy plain."
	rt.L.SetGlobal("self", rt.newHandle(self))

	self.long = "" // the guard entity itself has no long
	runSelf(t, rt, self, `
		local r = self:room()
		assert(r:has_room_flag("overworld") == true, "room flag present")
		assert(r:has_room_flag("nonesuch") == false, "absent room flag is false")
		assert(self:has_flag("overworld") == false, "has_flag (Living flags) does NOT see room flags")
		assert(self:has_room_flag("overworld") == false, "has_room_flag on a NON-room subject is false")
		assert(r:long() == "A long grassy plain.", "room long: "..tostring(r:long()))
		assert(self:long() == "", "an entity with no long reads empty string")
	`)
	_ = z
}

// TestOverworldGeneratedZoneShape guards the generated 6x20 grid against hand-edit corruption: exactly 120
// rooms, each with its [col,row,0] coord, and the landmark flags at the authored landmark coords (the map's
// icons depend on them). Regenerate via `go run gen.go` in the zone dir; do not hand-edit 10-rooms.yaml.
func TestOverworldGeneratedZoneShape(t *testing.T) {
	z := newDemoZone("overworld", newProtoCache())
	n := 0
	for col := 0; col < 6; col++ {
		for row := 0; row < 20; row++ {
			ref := ProtoRef(fmt.Sprintf("overworld:room:c%d_r%d", col, row))
			e := z.rooms[ref]
			if e == nil {
				t.Fatalf("missing generated room %s", ref)
			}
			if c := e.room.coord; len(c) != 3 || c[0] != col || c[1] != row || c[2] != 0 {
				t.Fatalf("%s coord = %v, want [%d %d 0]", ref, c, col, row)
			}
			if !e.room.namedFlags["overworld"] {
				t.Fatalf("%s is missing the `overworld` flag (map scoping depends on it)", ref)
			}
			n++
		}
	}
	if n != 120 {
		t.Fatalf("generated %d overworld rooms, want 120", n)
	}
	// Landmark flags at the authored coords (icons depend on them).
	landmarks := map[string]string{
		"overworld:room:c1_r6": "landmark_lake", "overworld:room:c2_r6": "landmark_lake",
		"overworld:room:c4_r13": "landmark_house", "overworld:room:c3_r9": "landmark_hill",
	}
	for ref, flag := range landmarks {
		if !z.rooms[ProtoRef(ref)].room.namedFlags[flag] {
			t.Fatalf("%s is missing landmark flag %q", ref, flag)
		}
	}
}
