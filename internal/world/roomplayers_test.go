package world

import (
	"encoding/json"
	"strings"
	"testing"
)

// roomplayers_test.go — #33: the GMCP Room.Players occupant list (players + mobs), routed through the
// canSee visibility chokepoint and change-detected in sendPrompt.

func TestRoomPlayersJSONContentAndVisibility(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	harmPlayer(z, room, "Buddy")
	ghost := harmMob(z, room, "ghost")
	ghost.setKeywords([]string{"ghost"})

	parse := func() []gmcpOccupant {
		var occ []gmcpOccupant
		if err := json.Unmarshal(z.roomPlayersJSON(viewer), &occ); err != nil {
			t.Fatalf("Room.Players not valid JSON: %v", err)
		}
		return occ
	}

	occ := parse()
	byName := map[string]string{} // name -> type
	for _, o := range occ {
		byName[o.Name] = o.Type
	}
	if byName["Buddy"] != "player" {
		t.Errorf("Buddy should be a player occupant, got %q (payload %v)", byName["Buddy"], occ)
	}
	if byName["ghost"] != "mob" {
		t.Errorf("ghost should be a mob occupant, got %q", byName["ghost"])
	}
	if _, self := byName["Viewer"]; self {
		t.Error("the viewer must not list itself in Room.Players")
	}

	// Invisibility routes through canSee: an invisible mob drops out for an ordinary viewer...
	setFlag(ghost, flagInvisible, true)
	occ = parse()
	for _, o := range occ {
		if o.Name == "ghost" {
			t.Fatalf("an invisible mob leaked into Room.Players: %v", occ)
		}
	}
	// ...but a holylight viewer sees it again.
	setFlag(viewer, flagHolylight, true)
	occ = parse()
	var sawGhost bool
	for _, o := range occ {
		if o.Name == "ghost" {
			sawGhost = true
		}
	}
	if !sawGhost {
		t.Fatal("a holylight viewer should see the invisible mob in Room.Players")
	}
}

// TestSendPromptEmitsRoomPlayersOnChange pins the change-detected emit: the first prompt sends Room.Players;
// an unchanged occupant set does not re-emit; a new arrival does.
func TestSendPromptEmitsRoomPlayersOnChange(t *testing.T) {
	z, _, room := harmZone(t)
	_ = harmPlayer(z, room, "Viewer")
	vs := z.players["Viewer"]

	z.sendPrompt(vs)
	if _, ok := drainGMCP(vs)["Room.Players"]; !ok {
		t.Fatal("first prompt did not emit Room.Players")
	}

	// No occupancy change → no re-emit.
	z.sendPrompt(vs)
	if _, ok := drainGMCP(vs)["Room.Players"]; ok {
		t.Fatal("Room.Players re-emitted with no occupant change")
	}

	// A newcomer arrives → re-emit, and the payload names them.
	harmPlayer(z, room, "Newcomer")
	z.sendPrompt(vs)
	frame, ok := drainGMCP(vs)["Room.Players"]
	if !ok {
		t.Fatal("an arrival did not re-emit Room.Players")
	}
	if !strings.Contains(frame, "Newcomer") {
		t.Fatalf("Room.Players payload missing the newcomer: %s", frame)
	}

	// The newcomer leaves the room → re-emit without them (the remove/leave path).
	Move(z.players["Newcomer"].entity, nil) // out of the room
	z.sendPrompt(vs)
	frame, ok = drainGMCP(vs)["Room.Players"]
	if !ok {
		t.Fatal("a departure did not re-emit Room.Players")
	}
	if strings.Contains(frame, "Newcomer") {
		t.Fatalf("Room.Players still lists the departed occupant: %s", frame)
	}
}
