package world

import (
	"strings"
	"testing"
)

// visibility_dark_test.go — #99: room darkness + a light-source model + infravision, all routed through the
// visibleTo/canSee chokepoint (visibility.go) so every perception surface (targeting, act(), lookRoom, GMCP)
// inherits it uniformly. A room authored with the "dark" flag conceals its occupants from an ordinary viewer
// unless a light source is present or the viewer sees in the dark (infravision/holylight).

// markRoomFlag sets a named room flag (rooms built by harmZone start with no namedFlags map).
func markRoomFlag(room *Entity, name string) {
	if room.room.namedFlags == nil {
		room.room.namedFlags = map[string]bool{}
	}
	room.room.namedFlags[name] = true
}

// lightItem builds a light-source item (an ItemMeta bearing the "light" tag) and places it in dest.
func lightItem(z *Zone, dest *Entity) *Entity {
	it := z.newEntity("harm:obj:torch")
	it.setShort("a torch")
	addAny(it, &ItemMeta{tags: []string{flagLight}})
	Move(it, dest)
	return it
}

// TestDarkRoomHidesOccupants is the headline: in an unlit dark room an ordinary viewer perceives no one but
// itself; infravision, holylight, and a light source each restore sight.
func TestDarkRoomHidesOccupants(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	buddy := harmPlayer(z, room, "Buddy")

	// A lit room (no dark flag): normal visibility.
	if !visibleTo(viewer, buddy) {
		t.Fatal("a lit room must not conceal an occupant")
	}

	// Darken it: the ordinary viewer can no longer see the buddy...
	markRoomFlag(room, flagDark)
	if visibleTo(viewer, buddy) {
		t.Fatal("a dark room must conceal an occupant from an ordinary viewer")
	}
	// ...but always sees itself (the self early-return holds in the dark).
	if !visibleTo(viewer, viewer) {
		t.Fatal("a viewer must always perceive itself, even in the dark")
	}

	// Infravision pierces darkness.
	setFlag(viewer, flagInfravision, true)
	if !visibleTo(viewer, buddy) {
		t.Fatal("infravision must let the viewer see in a dark room")
	}
	setFlag(viewer, flagInfravision, false)

	// Holylight sees everything, darkness included.
	setFlag(viewer, flagHolylight, true)
	if !visibleTo(viewer, buddy) {
		t.Fatal("holylight must see through darkness")
	}
	setFlag(viewer, flagHolylight, false)
	if visibleTo(viewer, buddy) {
		t.Fatal("clearing holylight should restore darkness concealment")
	}

	// A light source dropped in the room dispels the darkness for everyone.
	torch := lightItem(z, room)
	if !visibleTo(viewer, buddy) {
		t.Fatal("a light source on the ground must dispel darkness")
	}
	// Remove it and the room goes dark again.
	Move(torch, nil)
	if visibleTo(viewer, buddy) {
		t.Fatal("removing the only light source must re-darken the room")
	}
}

// TestDarkRoomLitByCarriedLight: a torch carried (in inventory/hand) by an occupant lights the whole room —
// one level of container nesting is scanned, so a held source counts.
func TestDarkRoomLitByCarriedLight(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	buddy := harmPlayer(z, room, "Buddy")
	markRoomFlag(room, flagDark)

	if visibleTo(viewer, buddy) {
		t.Fatal("precondition: the dark room should conceal the buddy")
	}
	// The buddy carries the torch — it still lights the room for the viewer.
	lightItem(z, buddy)
	if !visibleTo(viewer, buddy) {
		t.Fatal("a torch carried by an occupant must light the room")
	}
}

// TestDarkRoomLitByGlowFlag: a Living carrying the "light" glow flag (a light spell / luminous creature) is a
// light source in its own right.
func TestDarkRoomLitByGlowFlag(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	glowworm := harmMob(z, room, "glowworm")
	markRoomFlag(room, flagDark)

	if visibleTo(viewer, glowworm) {
		t.Fatal("precondition: the dark room should conceal the glowworm")
	}
	setFlag(glowworm, flagLight, true)
	if !visibleTo(viewer, glowworm) {
		t.Fatal("a Living with the glow flag must light the room")
	}
}

// TestLookRoomPitchBlack: looking in an unlit dark room shows the pitch-black notice in place of the room's
// name/desc/exits/occupants; a light source restores the full room render.
func TestLookRoomPitchBlack(t *testing.T) {
	z, _, room := harmZone(t)
	room.setShort("The Hall")
	room.setLong("A grand marble hall.")
	harmPlayer(z, room, "Viewer")
	harmPlayer(z, room, "Buddy")
	vs := z.players["Viewer"]

	markRoomFlag(room, flagDark)
	drainOutputs(vs)
	z.lookRoom(vs)
	out := strings.Join(drainOutputs(vs), "\n")
	if !strings.Contains(out, "pitch black") {
		t.Fatalf("dark-room look should show the pitch-black notice, got %q", out)
	}
	if strings.Contains(out, "The Hall") || strings.Contains(out, "Buddy") {
		t.Fatalf("dark-room look must not reveal the room or its occupants, got %q", out)
	}

	// Drop a light source: the full room renders again.
	lightItem(z, room)
	drainOutputs(vs)
	z.lookRoom(vs)
	out = strings.Join(drainOutputs(vs), "\n")
	if !strings.Contains(out, "The Hall") || !strings.Contains(out, "Buddy") {
		t.Fatalf("a lit room should render name + occupants, got %q", out)
	}
}

// TestRoomIsDarkNilAndUnflagged pins the two non-dark fast paths: a nil room (a viewer with no location) and a
// room without the dark flag are never dark, so the scan never runs for them.
func TestRoomIsDarkNilAndUnflagged(t *testing.T) {
	_, _, room := harmZone(t)
	if roomIsDark(nil) {
		t.Error("a nil room must not be dark")
	}
	if roomIsDark(room) {
		t.Error("a room without the dark flag must not be dark")
	}
	markRoomFlag(room, flagDark)
	if !roomIsDark(room) {
		t.Error("a dark, unlit room must be dark")
	}
}
