package world

import (
	"go/ast"
	"reflect"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// instance_entrance_test.go — #435: the declared instance entrance (the "dungeon door").
//
// The feature exists because `mud.send_to_instance` is SELF-ONLY, so a room's `enter` trigger cannot use it
// (the invoking actor there is the ROOM, not the entrant). Rather than relaxing self-only — which would let
// content decide WHO gets sent into unobservable space, billed to the VICTIM's instance quota — the crossing
// is a declared exit the player WALKS THROUGH. The mover is then the actor and self-only is satisfied by
// construction.
//
// So the security property under test is not "the gate refuses"; it is that NO PATH WHICH MOVES A PLAYER ON
// ANOTHER PARTY'S INITIATIVE CAN SEE A DOOR AT ALL. That is structural — entrances live in their own map,
// and every such mover resolves a direction through `exits`.

// entranceRoom returns a room component carrying one door, for the pure unit checks.
func entranceRoom(dir, tmpl string) *Room {
	return &Room{
		exits:     map[string]ProtoRef{"north": "midgaard:room:market"},
		entrances: map[string]string{dir: tmpl},
	}
}

// --- the structural security property ------------------------------------------------------------------

// TestEntranceIsNotAnExit is the whole design in one assertion. Every path that moves a player on someone
// else's initiative — hMove from a `greet` handler, directional flee, room-and-adjacent AoE, the cross-shard
// router — resolves its direction through `exits`. If an entrance ever appears there, all of them silently
// gain the ability to push a player through a dungeon door, which is precisely the capability this design
// exists to withhold.
func TestEntranceIsNotAnExit(t *testing.T) {
	r := entranceRoom("down", "crypt")

	if _, leaked := r.exits["down"]; leaked {
		t.Fatal("an instance entrance appeared in room.exits. hMove, flee and every other mover resolve a " +
			"direction through that map, so a `greet` handler could now call ev.actor:move(\"down\") and " +
			"force a non-consenting player into a private instance charged to THEIR account — the exact " +
			"capability #435 declined to grant")
	}
	// sortedExits feeds GMCP and room-and-adjacent AoE targeting. An entrance has no destination room, so a
	// leak here reaches an AoE toward a zone that does not exist yet.
	for _, d := range r.sortedExits() {
		if d == "down" {
			t.Fatal("an instance entrance appeared in sortedExits, which feeds AoE targeting and GMCP")
		}
	}
}

// TestOnlyMoveReadsEntrances is the anti-rot lint: it asserts, structurally, that `entrances` has exactly one
// reader in non-test code besides the display accessors and the builder.
//
// A behavioural test cannot cover this. The property is about code that does NOT exist — a future helper that
// resolves a direction against both maps would hand every mover the door, and no existing test would fail.
// This is the same argument identity_lint_test.go makes for the zone-locality lint.
func TestOnlyMoveReadsEntrances(t *testing.T) {
	// The legitimate readers: the mover, the two display enumerations, the COW, and the content mapper.
	allowed := map[string]bool{
		"commands.go":    true, // Zone.move — THE reader
		"components.go":  true, // displayExits / allExits — direction only, never the template
		"component.go":   true, // cloneComponent's deep copy
		"content_map.go": true, // the builder
	}
	pkg := loadWorldPackage(t)
	found := 0
	for _, f := range pkg.Syntax {
		name := baseName(pkg.Fset.Position(f.Pos()).Filename)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "entrances" {
				return true
			}
			found++
			if !allowed[name] {
				t.Errorf("%s:%d: reads room.entrances. A dungeon door must be reachable ONLY by a player's "+
					"own movement (Zone.move); a second resolver hands every mover — hMove, flee, a future "+
					"follow — the ability to push a player into a private instance (#435)",
					name, pkg.Fset.Position(sel.Pos()).Line)
			}
			return true
		})
	}
	if found == 0 {
		t.Fatal("the entrance-reader lint found NO references to room.entrances — it has gone inert (was the " +
			"field renamed?); fix the lint rather than deleting this guard")
	}
}

// --- the loader's structural rejections ------------------------------------------------------------------

// TestValidateEntrancesRejectsStructuralMistakes. Each case asserts the SPECIFIC violation rather than "load
// returned an error": several of these fixtures could plausibly trip a different rule, and a test that only
// checks for failure would pass while validating something else entirely.
func TestValidateEntrancesRejectsStructuralMistakes(t *testing.T) {
	cases := []struct {
		name   string
		room   content.RoomDTO
		wantIn string
	}{
		{
			// The trap this rule exists for: parseRef would keep only the leading segment, so this would MINT
			// the `crypt` zone and land the player at its own start room — appearing to work, silently
			// ignoring the room the builder named.
			name:   "a room ref where a zone ref belongs",
			room:   content.RoomDTO{Ref: "r", InstanceEntrances: map[string]string{"down": "crypt:room:altar"}},
			wantIn: "which is a ROOM ref",
		},
		{
			name:   "the instance-id separator",
			room:   content.RoomDTO{Ref: "r", InstanceEntrances: map[string]string{"down": "crypt#7"}},
			wantIn: "instance-id separator",
		},
		{
			name:   "an empty target",
			room:   content.RoomDTO{Ref: "r", InstanceEntrances: map[string]string{"down": "  "}},
			wantIn: "empty target",
		},
		{
			// The two maps share one direction namespace, so this is ambiguous. The move path resolves exits
			// first, which makes the runtime fail SAFE — but ambiguous content must not load.
			name: "the same direction is both an exit and a door",
			room: content.RoomDTO{
				Ref:               "r",
				Exits:             map[string]string{"down": "midgaard:room:cellar"},
				InstanceEntrances: map[string]string{"down": "crypt"},
			},
			wantIn: "cannot be both",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := validateEntrances(tc.room)
			if len(problems) == 0 {
				t.Fatalf("%+v loaded clean", tc.room.InstanceEntrances)
			}
			joined := strings.Join(problems, "; ")
			if !strings.Contains(joined, tc.wantIn) {
				t.Fatalf("rejected for a DIFFERENT reason than the one under test (want %q): %s",
					tc.wantIn, joined)
			}
		})
	}
}

// TestValidateEntrancesAcceptsACrossPackDoor. A door into a dungeon shipped by another pack must LOAD — the
// target zone's existence, its `instanceable` opt-in and its start room are all cross-pack facts, and
// rejecting them here would make a pack unloadable without its neighbour. MintInstance refuses all three at
// entry time and the player is told the way fails to open.
func TestValidateEntrancesAcceptsACrossPackDoor(t *testing.T) {
	r := content.RoomDTO{Ref: "r", InstanceEntrances: map[string]string{"down": "a-zone-in-another-pack"}}
	if problems := validateEntrances(r); len(problems) != 0 {
		t.Fatalf("a door naming a zone this pack does not ship was rejected at load: %v. That is a "+
			"dangling ref, which the loader tolerates for exits for the same reason", problems)
	}
}

// --- display -------------------------------------------------------------------------------------------

// TestEntranceIsVisibleButItsTemplateIsNot. An invisible door is a worse defect than any leak — a player
// cannot type a direction they were never shown — but the template ref must never reach a player-facing
// surface, or an unopened door names the dungeon behind it.
func TestEntranceIsVisibleButItsTemplateIsNot(t *testing.T) {
	r := entranceRoom("down", "secret-crypt")

	var sawDown bool
	for _, d := range r.displayExits() {
		if d == "down" {
			sawDown = true
		}
		if strings.Contains(d, "secret-crypt") {
			t.Fatalf("displayExits leaked the TEMPLATE ref %q; only the direction may be shown", d)
		}
	}
	if !sawDown {
		t.Fatal("the door is not in displayExits, so the Exits: line never shows it and no player can find " +
			"it. An unwalkable-looking door is worse than no door")
	}

	// allExits is what a room display template (the overworld minimap) enumerates.
	var inAll bool
	for _, d := range r.allExits() {
		if d == "down" {
			inAll = true
		}
	}
	if !inAll {
		t.Fatal("the door is missing from allExits, so a custom room template and the minimap silently lose " +
			"it while the built-in look line shows it")
	}
}

// TestAllExitsKeepsCanonicalOrderWithADoor. allExits promises dirOrder first, then the rest sorted; a door
// interleaved into the head must not disturb that, or every room display template's layout shifts.
func TestAllExitsKeepsCanonicalOrderWithADoor(t *testing.T) {
	r := &Room{
		exits:     map[string]ProtoRef{"north": "z:room:a", "west": "z:room:b", "portal": "z:room:c"},
		entrances: map[string]string{"down": "crypt", "alcove": "vault"},
	}
	got := strings.Join(r.allExits(), ",")
	// dirOrder is north,east,south,west,up,down — so: north, west, down; then the non-canonical tail sorted.
	const want = "north,west,down,alcove,portal"
	if got != want {
		t.Fatalf("allExits = %q, want %q (canonical dirOrder head including the door, then the tail sorted)",
			got, want)
	}
}

// --- the COW ---------------------------------------------------------------------------------------------

// TestCloneRoomDeepCopiesEntrances. cloneComponent's contract is that EVERY reference-typed field is
// reallocated; a map left out aliases the prototype. Rooms happen to be immutable at runtime, so an aliasing
// bug here is latent rather than loud — which is exactly why it needs a test rather than a bug report.
//
// It MUTATES the clone and asserts the prototype is unchanged. A test that only read the entrance would pass
// against a shared map.
func TestCloneRoomDeepCopiesEntrances(t *testing.T) {
	proto := entranceRoom("down", "crypt")
	proto.namedFlags = map[string]bool{"safe": true}

	clone, ok := cloneComponent(proto).(*Room)
	if !ok {
		t.Fatal("cloneComponent did not return a *Room")
	}
	clone.entrances["down"] = "someone-elses-dungeon"
	clone.entrances["up"] = "another"
	clone.namedFlags["safe"] = false

	if got := proto.entrances["down"]; got != "crypt" {
		t.Fatalf("mutating the CLONE's entrance changed the prototype's to %q — the map is aliased, so one "+
			"spawned copy of a room would rewrite the door for every other copy", got)
	}
	if _, leaked := proto.entrances["up"]; leaked {
		t.Fatal("adding a door to the clone added it to the prototype")
	}
	if !proto.namedFlags["safe"] {
		t.Fatal("mutating the clone's namedFlags changed the prototype's — the same aliasing, on the map the " +
			"PvP safe-room veto reads")
	}
}

// --- the build path --------------------------------------------------------------------------------------

// TestRoomComponentsCarriesEntrances is the wiring between the content DTO and the live room. Without it
// every other test here passes against hand-built fixtures while authored content has no doors at all.
func TestRoomComponentsCarriesEntrances(t *testing.T) {
	cs := roomComponents(content.RoomDTO{
		Ref:               "midgaard:room:guildhall",
		Exits:             map[string]string{"north": "midgaard:room:market"},
		InstanceEntrances: map[string]string{"down": "crypt"},
	})
	room, ok := cs[reflect.TypeFor[*Room]()].(*Room)
	if !ok {
		t.Fatal("roomComponents produced no *Room")
	}
	if got := room.entrances["down"]; got != "crypt" {
		t.Fatalf("the authored entrance did not reach the built room: got %q. Every other test in this file "+
			"builds its fixture by hand and would still pass", got)
	}
	if _, leaked := room.exits["down"]; leaked {
		t.Fatal("the builder merged the entrance into exits")
	}
}
