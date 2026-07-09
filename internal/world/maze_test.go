package world

import (
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
)

// maze_test.go — #202 scenario 3 (maze / "Eternal Forest"). A room graph where a hidden direction SEQUENCE
// reaches a secret area, and a forest whose exits loop back on themselves until the escape direction is
// walked. This ships TODAY with NO engine code: exits are a free per-room direction->room map (non-reciprocal,
// self-looping, or pointing anywhere), so a fixed-topology maze is pure authoring. This test is the acceptance
// proof that (a) such graphs load through the real content pipeline and route correctly and (b) their unusual
// exits are NOT flagged by the #197 dangling-exit lint (the rooms are real). Only PATH-DEPENDENT mazes (a room
// behaving differently by how you arrived) would need a script; the fixed sequence here does not.

// mazeRooms is the hand-authored maze + Eternal Forest as RoomDTOs — the exact data a builder writes in YAML.
// Shared by the routing test (loaded through the real pipeline) and the lint test (fed to the validator), so
// the two halves meet in the middle on ONE source of truth.
func mazeRooms() []content.RoomDTO {
	room := func(ref string, exits map[string]string) content.RoomDTO {
		return content.RoomDTO{Ref: ref, Name: ref, Exits: exits}
	}
	return []content.RoomDTO{
		// Fixed-sequence maze: only NORTH progresses; every other exit self-loops, and a wrong turn at room 2
		// drops you back to the entrance. Escape sequence: north, north -> the secret.
		room("mz:room:1", map[string]string{"north": "mz:room:2", "south": "mz:room:1", "east": "mz:room:1", "west": "mz:room:1"}),
		room("mz:room:2", map[string]string{"north": "mz:room:secret", "south": "mz:room:1", "east": "mz:room:1", "west": "mz:room:1"}),
		room("mz:room:secret", map[string]string{"south": "mz:room:2"}),
		// Eternal Forest: every exit loops back to the forest except WEST, which escapes to the clearing.
		room("mz:room:forest", map[string]string{"north": "mz:room:forest", "south": "mz:room:forest", "east": "mz:room:forest", "west": "mz:room:clearing"}),
		room("mz:room:clearing", map[string]string{"east": "mz:room:forest"}),
	}
}

func mazePack() []content.Pack {
	return []content.Pack{{Pack: "maze", Zones: []content.ZoneDTO{
		{Ref: "mz", StartRoom: "mz:room:1", Rooms: mazeRooms()},
	}}}
}

// mazeZone builds the maze zone THROUGH THE REAL CONTENT MAPPING: defineContent turns each RoomDTO into a
// room prototype via roomComponents (the RoomDTO.Exits -> *Room.exits map, including self-loops and
// non-reciprocal targets), and z.spawnRoom instantiates each — the same seam the boot loader uses, so the
// authoring shape is exercised end to end, not hand-wired. Returns the zone and a player at the entrance.
func mazeZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	protos := newProtoCache()
	z := newZone("mz")
	z.protos = protos
	lc := &content.LoadedContent{Zones: []content.ZoneDTO{{Ref: "mz", StartRoom: "mz:room:1", Rooms: mazeRooms()}}}
	defineContent(protos, lc) // RoomDTO.Exits -> *Room.exits (roomComponents)
	for _, r := range mazeRooms() {
		z.spawnRoom(ProtoRef(r.Ref)) // proto -> live room; z.spawn promotes e.room
	}
	z.startRoom = "mz:room:1"

	s := &session{character: "Explorer", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Explorer")
	Move(s.entity, z.rooms["mz:room:1"])
	z.players["Explorer"] = s
	return z, s
}

func mazeAt(s *session) string { return string(s.entity.location.proto) }

// TestMazeFixedSequenceRoutes: wrong turns self-loop / bounce to the entrance; the hidden north,north
// sequence reaches the secret — a data-only maze loaded through the real pipeline, no script.
func TestMazeFixedSequenceRoutes(t *testing.T) {
	z, s := mazeZone(t)
	// mv walks and returns just that move's output (z.move is synchronous, so the frames are already buffered).
	mv := func(dir string) string { z.move(s, dir); return drainAllText(s.out) }
	refused := "can't go that way"

	// A self-looping exit routes you back to the SAME room — but that must be a real EXIT, not a missing one.
	// A genuine self-loop move produces a room look (no refusal); a missing direction produces the refusal.
	if out := mv("east"); strings.Contains(out, refused) {
		t.Fatal("`east` is a real self-looping exit of room 1, but the engine refused it as a missing exit")
	}
	if got := mazeAt(s); got != "mz:room:1" {
		t.Fatalf("after a self-loop exit: at %s, want mz:room:1", got)
	}
	// Contrast: a direction with NO exit IS refused — proving the self-loop above really routed.
	if out := mv("up"); !strings.Contains(out, refused) {
		t.Fatal("`up` has no exit and must be refused (else the self-loop assertion proves nothing)")
	}
	if got := mazeAt(s); got != "mz:room:1" {
		t.Fatalf("a missing-exit move should not relocate: at %s", got)
	}

	mv("north") // 1 -> 2 (progress)
	if got := mazeAt(s); got != "mz:room:2" {
		t.Fatalf("north from entrance: at %s, want mz:room:2", got)
	}
	mv("east") // wrong turn at room 2 -> back to the entrance
	if got := mazeAt(s); got != "mz:room:1" {
		t.Fatalf("a wrong turn should bounce to the entrance: at %s, want mz:room:1", got)
	}
	// Walk the hidden escape sequence: north, north -> the secret.
	mv("north")
	mv("north")
	if got := mazeAt(s); got != "mz:room:secret" {
		t.Fatalf("the north,north escape sequence did not reach the secret: at %s", got)
	}
}

// TestEternalForestLoopsUntilEscape: the forest's non-escape exits all loop back to it; only west escapes.
func TestEternalForestLoopsUntilEscape(t *testing.T) {
	z, s := mazeZone(t)
	Move(s.entity, z.rooms["mz:room:forest"])

	for _, dir := range []string{"north", "south", "east"} {
		z.move(s, dir)
		if strings.Contains(drainAllText(s.out), "can't go that way") {
			t.Fatalf("forest exit %q is a real self-loop, not a missing exit", dir)
		}
		if got := mazeAt(s); got != "mz:room:forest" {
			t.Fatalf("forest exit %q should loop back to the forest, but reached %s", dir, got)
		}
	}
	z.move(s, "west") // the escape
	if got := mazeAt(s); got != "mz:room:clearing" {
		t.Fatalf("west should escape the forest to the clearing, but reached %s", got)
	}
}

// TestMazeExitsPassDanglingLint: the maze's self-looping and non-reciprocal exits all point to REAL rooms, so
// the #197 dangling-exit validator must NOT flag them (a maze is legitimate authoring, not broken content).
func TestMazeExitsPassDanglingLint(t *testing.T) {
	if problems := vRoomExits(mazePack()); len(problems) != 0 {
		t.Fatalf("maze exits (self-loop / non-reciprocal, all to real rooms) were flagged as dangling: %v", problems)
	}
	// Positive control: a TRULY dangling exit (to a room that does not exist) IS still caught — the lint
	// isn't inert. `mz:room:nowhere` shares the zone prefix so it hits the intra-zone flagging branch.
	broken := []content.Pack{{Pack: "maze", Zones: []content.ZoneDTO{
		{Ref: "mz", Rooms: []content.RoomDTO{{Ref: "mz:room:1", Exits: map[string]string{"north": "mz:room:nowhere"}}}},
	}}}
	if problems := vRoomExits(broken); len(problems) == 0 {
		t.Fatal("a genuinely dangling exit (to a nonexistent room) should still be flagged")
	}
}
