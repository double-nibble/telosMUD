package world

import (
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// nearby_disclosure_test.go — #363: white-box tests for the has_visible_creature disclosure primitive
// (luahandle.go), the security surface behind the overworld nearby-mob marker. Each gating condition
// (open_sight on BOTH rooms, canSee, mobs-only, display-only) gets its own assertion, and the general #253
// foreign-room occupants() anti-scry is proven STILL closed alongside the narrow new disclosure.

// discloseZone builds a two-room zone — `anchor` (the viewer's room) and `neighbour` (a foreign room) — each
// optionally open_sight-flagged. Returns the zone, the viewer entity, and the neighbour room entity.
func discloseZone(t *testing.T, anchorFlag, neighbourFlag bool) (*Zone, *Entity, *Entity) {
	t.Helper()
	z := newZone("dz")
	anchor := z.newEntity("dz:room:anchor")
	ar := &Room{exits: map[string]ProtoRef{"north": "dz:room:neighbour"}}
	if anchorFlag {
		ar.namedFlags = map[string]bool{roomFlagOpenSight: true}
	}
	Add(anchor, ar)
	z.rooms["dz:room:anchor"] = anchor

	neighbour := z.newEntity("dz:room:neighbour")
	nr := &Room{exits: map[string]ProtoRef{"south": "dz:room:anchor"}}
	if neighbourFlag {
		nr.namedFlags = map[string]bool{roomFlagOpenSight: true}
	}
	Add(neighbour, nr)
	z.rooms["dz:room:neighbour"] = neighbour

	viewer := newTestPlayerEntity(z, "Viewer").entity
	Move(viewer, anchor)
	return z, viewer, neighbour
}

// hasVisible invokes `neighbour:has_visible_creature()` in a DISPLAY render anchored at the viewer's room,
// returning the primitive's bool as a string ("true"/"false").
func hasVisible(t *testing.T, z *Zone, viewer, neighbour *Entity, display bool) string {
	t.Helper()
	rt := z.lua
	ch := rt.chunkFor("test:disclose", `return tostring(nb:has_visible_creature())`)
	inv := &luaInvocation{actor: viewer, display: display, displayRoom: viewer.location}
	got, ok := rt.invokeForString(ch, inv, map[string]lua.LValue{"nb": rt.newHandle(neighbour)})
	if !ok {
		t.Fatal("has_visible_creature probe failed to render")
	}
	return got
}

func addBeast(z *Zone, room *Entity) *Entity {
	mob := z.newEntity("dz:mob:beast")
	Add(mob, &Living{})
	mob.short = "a wild beast"
	Move(mob, room)
	return mob
}

// TestHasVisibleCreatureDisclosesWhenBothFlagged: the happy path — both rooms open_sight, a visible mob in the
// neighbour → the presence primitive returns true.
func TestHasVisibleCreatureDisclosesWhenBothFlagged(t *testing.T) {
	z, viewer, neighbour := discloseZone(t, true, true)
	addBeast(z, neighbour)
	if got := hasVisible(t, z, viewer, neighbour, true); got != "true" {
		t.Fatalf("both-flagged + visible mob = %q, want true", got)
	}
}

// TestHasVisibleCreatureRequiresNeighbourFlag: the TARGET room must carry open_sight.
func TestHasVisibleCreatureRequiresNeighbourFlag(t *testing.T) {
	z, viewer, neighbour := discloseZone(t, true, false) // neighbour NOT flagged
	addBeast(z, neighbour)
	if got := hasVisible(t, z, viewer, neighbour, true); got != "false" {
		t.Fatalf("unflagged neighbour = %q, want false (disclosure confined to open_sight terrain)", got)
	}
}

// TestHasVisibleCreatureRequiresAnchorFlag: the VIEWER's room must carry open_sight too — so a non-open_sight
// room's template (e.g. a dungeon) can never scry an adjacent open field.
func TestHasVisibleCreatureRequiresAnchorFlag(t *testing.T) {
	z, viewer, neighbour := discloseZone(t, false, true) // anchor NOT flagged
	addBeast(z, neighbour)
	if got := hasVisible(t, z, viewer, neighbour, true); got != "false" {
		t.Fatalf("unflagged anchor = %q, want false (a non-open_sight room can't scry a neighbour)", got)
	}
}

// TestHasVisibleCreatureIsDisplayOnly: outside a display render the primitive discloses nothing (fail-closed) —
// a mechanics script uses occupants()/mud.scan, this exists only to widen a DISPLAY where #253 shuts it.
func TestHasVisibleCreatureIsDisplayOnly(t *testing.T) {
	z, viewer, neighbour := discloseZone(t, true, true)
	addBeast(z, neighbour)
	if got := hasVisible(t, z, viewer, neighbour, false); got != "false" {
		t.Fatalf("non-display invocation = %q, want false", got)
	}
}

// TestHasVisibleCreatureRespectsCanSee: a concealed (invisible) mob does not mark — wizinvis/concealment stay
// hidden through the canSee chokepoint.
func TestHasVisibleCreatureRespectsCanSee(t *testing.T) {
	z, viewer, neighbour := discloseZone(t, true, true)
	mob := addBeast(z, neighbour)
	setFlag(mob, flagInvisible, true)
	if got := hasVisible(t, z, viewer, neighbour, true); got != "false" {
		t.Fatalf("concealed mob = %q, want false (canSee must hide it)", got)
	}
}

// TestHasVisibleCreatureExcludesPlayers: another PLAYER in the neighbour does NOT mark — mobs only, so the
// primitive can never become a PvP position-tracker (even bare presence of a player is withheld).
func TestHasVisibleCreatureExcludesPlayers(t *testing.T) {
	z, viewer, neighbour := discloseZone(t, true, true)
	other := newTestPlayerEntity(z, "Other").entity
	Move(other, neighbour)
	if got := hasVisible(t, z, viewer, neighbour, true); got != "false" {
		t.Fatalf("player occupant = %q, want false (mobs only — no PvP tracking)", got)
	}
}

// TestHasVisibleCreatureEmptyRoom: an open_sight room with no creature returns false.
func TestHasVisibleCreatureEmptyRoom(t *testing.T) {
	z, viewer, neighbour := discloseZone(t, true, true)
	if got := hasVisible(t, z, viewer, neighbour, true); got != "false" {
		t.Fatalf("empty neighbour = %q, want false", got)
	}
}

// TestForeignOccupantsStillBlockedAlongsideMarker is the load-bearing anti-scry regression: in the SAME display
// context where the presence primitive discloses (true), the general occupants() enumeration on that foreign
// room STILL returns nothing (#253). This proves the narrow primitive did not widen the general scry path.
func TestForeignOccupantsStillBlockedAlongsideMarker(t *testing.T) {
	z, viewer, neighbour := discloseZone(t, true, true)
	addBeast(z, neighbour)

	// Presence primitive discloses...
	if got := hasVisible(t, z, viewer, neighbour, true); got != "true" {
		t.Fatalf("presence primitive = %q, want true", got)
	}
	// ...but occupants() on the SAME foreign room, same display context, stays empty (#253 holds).
	rt := z.lua
	ch := rt.chunkFor("test:occ", `local n = 0; for _ in ipairs(nb:occupants()) do n = n + 1 end; return tostring(n)`)
	inv := &luaInvocation{actor: viewer, display: true, displayRoom: viewer.location}
	got, ok := rt.invokeForString(ch, inv, map[string]lua.LValue{"nb": rt.newHandle(neighbour)})
	if !ok {
		t.Fatal("occupants probe failed to render")
	}
	if got != "0" {
		t.Fatalf("foreign-room occupants() disclosed %s occupant(s) — the #253 anti-scry was widened", got)
	}
}
