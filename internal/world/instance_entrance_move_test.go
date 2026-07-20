package world

import (
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// instance_entrance_move_test.go — #435: walking through a declared dungeon door, and the paired negative.
//
// These two tests are a matched pair ON THE SAME ROOM AND THE SAME DIRECTION, which is the point. A test that
// only asserts "Lua cannot push a player through an entrance" is vacuous if the fixture never had a working
// entrance — hMove would return false because the room has no such direction at all, and the test would pass
// against a feature that does not exist. So the door is PROVEN LIVE first, by a player walking it, and only
// then is Lua shown to be unable to use it.

// entranceZone builds a zone whose hall has a working door DOWN into the `crypt` template, plus a player
// standing in it. The shard is present but not running, so a mint is enqueued and never drained — enough to
// observe that the move REACHED requestInstanceEntry, which is what these tests are about.
func entranceZone(t *testing.T) (*Zone, *session, *Shard) {
	t.Helper()
	sh := idleShard(t) // nothing drains the mint queue, so "was a build queued?" stays observable
	z := newZone("harm")
	z.shard = sh
	room := z.newEntity("harm:room:hall")
	Add(room, &Room{
		exits:     map[string]ProtoRef{"north": "harm:room:market"},
		entrances: map[string]string{"down": "crypt"},
	})
	z.rooms["harm:room:hall"] = room
	// A second, ordinary room so the control move has somewhere to go.
	market := z.newEntity("harm:room:market")
	Add(market, &Room{exits: map[string]ProtoRef{"south": "harm:room:hall"}})
	z.rooms["harm:room:market"] = market

	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 64), epoch: 1, account: "acct-1"}
	z.newPlayerEntity(s, "Hero")
	Move(s.entity, room)
	z.setPlayer("Hero", s)
	return z, s, sh
}

// TestWalkingThroughADeclaredEntranceRequestsTheMint is the feature working end to end from the player's side:
// they type a direction and the engine asks for their instance.
func TestWalkingThroughADeclaredEntranceRequestsTheMint(t *testing.T) {
	z, s, sh := entranceZone(t)

	if moved := z.move(s, "down"); moved {
		t.Fatal("move reported that it RELEASED OWNERSHIP of the session. It did not — the player is still " +
			"standing in the entrance room and the crossing happens hops later, from instanceReady. " +
			"Returning true marks the command as having moved and suppresses their prompt until then")
	}
	if !s.instanceMintPending {
		t.Fatal("walking into the door did not mark a mint in flight, so the move never reached " +
			"requestInstanceEntry — the entrance branch is not wired into Zone.move")
	}
	select {
	case req := <-sh.mintQ:
		if req.template != "crypt" {
			t.Fatalf("the queued mint names template %q, want \"crypt\"", req.template)
		}
		if req.account != "acct-1" {
			t.Fatalf("the mint is billed to %q, want the MOVER's own account. Billing anyone else is the "+
				"harm this whole design exists to make impossible", req.account)
		}
		if req.originRoom != "harm:room:hall" {
			t.Fatalf("the exit anchor is %q, want the room the player walked in from — it is where they come "+
				"back out, and where a reconnect lands them", req.originRoom)
		}
	default:
		t.Fatal("no mint was queued")
	}
	// The player is still HERE. The door is not a transfer.
	if s.entity.location == nil || s.entity.location.proto != "harm:room:hall" {
		t.Fatal("the player left the entrance room at request time. Nothing has been built yet and the mint " +
			"may still fail; the crossing is instanceReady's job")
	}
}

// TestAnUnknownDirectionIsStillUnknown — the entrance branch must not swallow the ordinary refusal.
func TestAnUnknownDirectionIsStillUnknown(t *testing.T) {
	z, s, _ := entranceZone(t)
	if z.move(s, "west") {
		t.Fatal("moved west out of a room with no west exit")
	}
	assertTold(t, s, "can't go that way")
	if s.instanceMintPending {
		t.Fatal("an unknown direction queued a mint")
	}
}

// TestOrdinaryExitsStillWorkAlongsideADoor — a room with a door must still be a normal room.
func TestOrdinaryExitsStillWorkAlongsideADoor(t *testing.T) {
	z, s, _ := entranceZone(t)
	z.move(s, "north")
	if s.entity.location == nil || s.entity.location.proto != "harm:room:market" {
		t.Fatalf("an ordinary exit in a room that also has a door did not move the player (they are at %v)",
			s.entity.location)
	}
	if s.instanceMintPending {
		t.Fatal("walking an ORDINARY exit queued an instance mint")
	}
}

// TestLuaCannotPushAPlayerThroughADeclaredEntrance is the end-to-end confirmation of the security property,
// and it runs against the SAME room and the SAME direction the test above just walked successfully. That
// pairing is what stops it passing against a room that simply has no `down`.
//
// WHAT THIS TEST DOES *NOT* PROVE, stated because measuring it changed how much weight it deserves: it was
// mutation-tested by teaching hMove to resolve entrances as well as exits, and it STILL PASSED. The reason is
// worth knowing — hMove would then hold `ref = "crypt"`, and parseRef on a bare ref yields zone "", for which
// ownsZoneRef is TRUE; the refusal actually arrives one step later, from the room-map miss on `z.rooms["crypt"]`.
// So hMove's refusal here is INCIDENTAL, not a deliberate guard, and a future change to either of those two
// steps could unmask it without this test noticing.
//
// The real fence is structural and is tested by TestOnlyMoveReadsEntrances, which DID catch that mutation:
// entrances live in their own map with one legitimate reader. This test is the behavioural confirmation that
// the fence holds today, not the fence itself. Do not let it stand in for the lint.
func TestLuaCannotPushAPlayerThroughADeclaredEntrance(t *testing.T) {
	z, s, sh := entranceZone(t)

	// PROVE THE DOOR IS LIVE: the same room, the same direction, walked by the player themselves.
	if z.move(s, "down"); !s.instanceMintPending {
		t.Fatal("the fixture's door does not work, so the refusal below would prove nothing at all")
	}
	<-sh.mintQ
	s.instanceMintPending = false // reset: we are about to try the same crossing a different way

	// Now the same crossing, driven by a SCRIPT acting ON the player rather than by the player. `self` is the
	// PLAYER here, which is strictly MORE permissive than the real threat: a room's `enter` handler holds the
	// entrant only as `ev.actor`, and mayRelocate's self-exemption does not apply to it. If the door refuses
	// even this, it refuses the trigger shape too.
	rt := z.lua
	if rt == nil {
		t.Skip("zone has no Lua runtime")
	}
	// The control first: the script CAN walk the player through an ordinary exit, so a false below is the
	// door refusing rather than h:move being broken or the fixture being wrong.
	if err := rt.runChunkWithSelf("ctl", `assert(self:move("north") == true, "h:move could not walk an ORDINARY exit; the refusal below would prove nothing")`, s.entity); err != nil {
		t.Fatalf("control: %v", err)
	}
	if err := rt.runChunkWithSelf("back", `self:move("south")`, s.entity); err != nil {
		t.Fatalf("walk back: %v", err)
	}

	if err := rt.runChunkWithSelf("door", `assert(self:move("down") == false, "a SCRIPT moved the player through the dungeon door")`, s.entity); err != nil {
		t.Fatalf("%v\n\nhMove resolves directions through room.exits precisely so it cannot see an "+
			"entrance. If it can, a `greet` handler can force any passerby into a private instance billed "+
			"to THEIR account — the capability #435 deliberately declined to grant", err)
	}
	if s.instanceMintPending {
		t.Fatal("a script-driven move queued an instance mint")
	}
	if s.entity.location == nil || s.entity.location.proto != "harm:room:hall" {
		t.Fatal("the script moved the player somewhere")
	}
}
