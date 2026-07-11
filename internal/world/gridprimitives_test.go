package world

import (
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
)

// gridprimitives_test.go — the overworld grid + traversal primitives (#360): the `enter`/`exit`/`out`
// movement verbs (so a pack can author non-cardinal named exits like a city gate) and the
// self:room():coord() Lua accessor (the room's authored [x,y,z] the overworld map centres on).

// gridZone builds a small zone THROUGH THE REAL CONTENT MAPPING (like mazeZone): three rooms wired with
// the non-cardinal `enter`/`exit`/`out` exit keys + authored coords, and a player at the gate. Returns the
// zone and the player session.
func gridZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	rooms := []content.RoomDTO{
		// gate -> plains via `enter`; plains -> gate via `exit`; plains -> hut via `out`.
		{Ref: "grid:room:gate", Name: "The Gate", Coord: []int{2, 0, 0}, Exits: map[string]string{"enter": "grid:room:plains"}},
		{Ref: "grid:room:plains", Name: "The Plains", Coord: []int{2, 1, 0}, Exits: map[string]string{"exit": "grid:room:gate", "out": "grid:room:hut"}},
		{Ref: "grid:room:hut", Name: "A Hut", Exits: map[string]string{}}, // no coord — the "not on a grid" case
	}
	protos := newProtoCache()
	z := newZone("grid")
	z.protos = protos
	defineContent(protos, &content.LoadedContent{Zones: []content.ZoneDTO{{Ref: "grid", StartRoom: "grid:room:gate", Rooms: rooms}}})
	for _, r := range rooms {
		z.spawnRoom(ProtoRef(r.Ref))
	}
	z.startRoom = "grid:room:gate"

	s := &session{character: "Walker", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Walker")
	Move(s.entity, z.rooms["grid:room:gate"])
	z.players["Walker"] = s
	return z, s
}

func gridAt(s *session) string { return string(s.entity.location.proto) }

// TestEnterExitOutMovementVerbs proves the three non-cardinal movement verbs walk a player through an exit
// keyed by that word — the city-gate / portal traversal the overworld wiring needs. Before #360 these words
// had no verb, so the exits existed but were unreachable by typing them.
func TestEnterExitOutMovementVerbs(t *testing.T) {
	z, s := gridZone(t)
	z.dispatch(s, "enter") // gate -> plains
	if got := gridAt(s); got != "grid:room:plains" {
		t.Fatalf("`enter` did not walk gate->plains: at %s", got)
	}
	z.dispatch(s, "out") // plains -> hut
	if got := gridAt(s); got != "grid:room:hut" {
		t.Fatalf("`out` did not walk plains->hut: at %s", got)
	}
	// A room with no such exit refuses (proving the verb routes a real exit, not a blanket teleport).
	z.dispatch(s, "enter")
	if got := gridAt(s); got != "grid:room:hut" {
		t.Fatalf("`enter` with no such exit relocated the player: at %s", got)
	}
}

// TestExitVerbWalksBack proves `exit` (a distinct verb from `enter`) routes its own exit key.
func TestExitVerbWalksBack(t *testing.T) {
	z, s := gridZone(t)
	z.dispatch(s, "enter") // gate -> plains
	z.dispatch(s, "exit")  // plains -> gate
	if got := gridAt(s); got != "grid:room:gate" {
		t.Fatalf("`exit` did not walk plains->gate: at %s", got)
	}
}

// TestRoomCoordAccessor proves self:room():coord() returns the authored [x,y,z] as {x,y,z}, and NIL for a
// room with no authored coord — the read the overworld `room` template gates its viewport on.
func TestRoomCoordAccessor(t *testing.T) {
	z, s := gridZone(t)
	// At the gate (coord [2,0,0]).
	runSelf(t, z.lua, s.entity, `
		local c = self:room():coord()
		assert(c ~= nil, "gate has a coord")
		assert(c.x == 2 and c.y == 0 and c.z == 0, "gate coord = (2,0,0), got ("..c.x..","..c.y..","..c.z..")")
	`)
	// Walk to the coord-less hut: coord() reads nil.
	z.dispatch(s, "enter")
	z.dispatch(s, "out")
	if got := gridAt(s); got != "grid:room:hut" {
		t.Fatalf("setup: expected to be in the hut, at %s", got)
	}
	runSelf(t, z.lua, s.entity, `assert(self:room():coord() == nil, "a coord-less room reads nil")`)
	// A NON-room subject (calling coord() on the player handle itself, not its room) reads nil — no panic.
	runSelf(t, z.lua, s.entity, `assert(self:coord() == nil, "coord() on a non-room subject reads nil")`)
}
