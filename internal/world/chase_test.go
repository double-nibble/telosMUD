package world

import (
	"reflect"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// chase_test.go — #202 scenario 2 (chasing mob). A player who flees a room leaves via some exit; a scripted
// mob left behind should be able to learn the departure AND the direction and follow. The enabling engine
// change is fireWitnessLeave (commands.go move path): a `witness_leave` trigger on each co-located scripted
// mob carrying ev.actor (the fleer) and ev.dir (the exit taken) — which the room's directionless `leave`
// trigger cannot provide.

// chaseZone builds a two-room zone (A --east--> B --west--> A) with a chaser mob that follows on witness_leave.
func chaseZone(t *testing.T) (*Zone, *Entity, *Entity, *Entity, *session) {
	t.Helper()
	z := newZone("cz")
	roomA := z.newEntity("cz:room:a")
	Add(roomA, &Room{exits: map[string]ProtoRef{"east": "cz:room:b"}})
	z.rooms["cz:room:a"] = roomA
	roomB := z.newEntity("cz:room:b")
	Add(roomB, &Room{exits: map[string]ProtoRef{"west": "cz:room:a"}})
	z.rooms["cz:room:b"] = roomB
	z.startRoom = "cz:room:a"

	z.protos.define("cz:mob:chaser", nil, "a chaser", "A chaser watches for movement.", componentSet{
		reflect.TypeFor[*Living](): &Living{},
		// Follow the fleer: witness_leave carries the direction they took.
		reflect.TypeFor[*Scripted](): &Scripted{source: `on("witness_leave", function(ev) self:move(ev.dir) end)`},
	})
	chaser := z.spawn("cz:mob:chaser")
	Move(chaser, roomA)

	s := &session{character: "Runner", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	player := z.newPlayerEntity(s, "Runner")
	Move(player, roomA)
	z.players["Runner"] = s
	return z, roomA, roomB, chaser, s
}

// TestChaserFollowsFleeingPlayer: a player flees A→east→B and the co-located chaser follows to B, driven by
// witness_leave's ev.dir — proving a mob can learn both the departure and the exit taken.
func TestChaserFollowsFleeingPlayer(t *testing.T) {
	z, roomA, roomB, _, s := chaseZone(t)

	z.move(s, "east") // the player flees east

	if s.entity.location != roomB {
		t.Fatal("player did not move A->B")
	}
	if mobIn(roomB, "cz:mob:chaser") == nil {
		t.Fatal("the chaser did not follow the fleeing player to B (witness_leave did not fire, or ev.dir was wrong)")
	}
	if mobIn(roomA, "cz:mob:chaser") != nil {
		t.Fatal("the chaser is still in A — it did not follow")
	}
}

// TestDemoStalkerFollowsPlayer drives the ACTUAL shipped chaser (darkwood:mob:stalker), so a runtime error
// in its witness_leave Lua is caught here. It lurks in sanctum (west→webtunnel); a player leaving west is
// followed by the stalker into webtunnel.
func TestDemoStalkerFollowsPlayer(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	stalker := z.spawn("darkwood:mob:stalker")
	Move(stalker, z.rooms["darkwood:room:sanctum"])
	z.fireSpawn(stalker) // harmless prime; the stalker has no on("spawn")

	s := &session{character: "Prey", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Prey")
	Move(s.entity, z.rooms["darkwood:room:sanctum"])
	z.players["Prey"] = s

	z.move(s, "west") // flee sanctum -> webtunnel

	if mobIn(z.rooms["darkwood:room:webtunnel"], "darkwood:mob:stalker") == nil {
		t.Fatal("the shipped demo stalker did not follow the player out of sanctum (its witness_leave Lua errored)")
	}
}

// TestChaserFollowsFleeingPlayerCombat drives the FLEE path (the issue's real scenario — a mob follows a
// player FLEEING combat, who cannot walk while fighting). The chaser follows via witness_leave fired from
// cmdFlee, just as it does from the walk path.
func TestChaserFollowsFleeingPlayerCombat(t *testing.T) {
	z, roomA, roomB, _, s := chaseZone(t)

	// A foe in A the player is fighting. Bare (no combat profile) so the flee's opportunity-attack provoke
	// is a no-op — we're testing the chase, not combat damage.
	foe := z.newEntity("cz:mob:foe")
	Add(foe, &Living{})
	foe.short = "a foe"
	Move(foe, roomA)
	z.startFight(s.entity, foe)
	if position(s.entity) != posFighting {
		t.Fatal("startFight did not engage the player")
	}

	// Flee east — the only way out of combat.
	if err := cmdFlee(&Context{z: z, s: s, Actor: s.entity, arg: "east"}); err != nil {
		t.Fatalf("cmdFlee: %v", err)
	}
	if s.entity.location != roomB {
		t.Fatal("player did not flee A->B")
	}
	if mobIn(roomB, "cz:mob:chaser") == nil {
		t.Fatal("the chaser did not follow the FLEEING player (witness_leave not wired into the flee path)")
	}
}

// TestWitnessLeaveFiresOnlyOnScriptedMobWitnesses: witness_leave reaches a co-located scripted MOB (it
// follows), while a non-scripted occupant is skipped and the leaver never self-fires (the mover is a
// non-scripted player, so nothing double-moves it).
func TestWitnessLeaveFiresOnlyOnScriptedMobWitnesses(t *testing.T) {
	z := newZone("cz2")
	roomA := z.newEntity("cz2:room:a")
	Add(roomA, &Room{exits: map[string]ProtoRef{"east": "cz2:room:b"}})
	z.rooms["cz2:room:a"] = roomA
	roomB := z.newEntity("cz2:room:b")
	Add(roomB, &Room{exits: map[string]ProtoRef{"west": "cz2:room:a"}})
	z.rooms["cz2:room:b"] = roomB
	z.startRoom = "cz2:room:a"

	z.protos.define("cz2:mob:chaser", nil, "a chaser", "It stands here.", componentSet{
		reflect.TypeFor[*Living]():   &Living{},
		reflect.TypeFor[*Scripted](): &Scripted{source: `on("witness_leave", function(ev) self:move(ev.dir) end)`},
	})
	chaser := z.spawn("cz2:mob:chaser")
	Move(chaser, roomA)
	// A NON-scripted item in the room — must be ignored (not treated as a witness).
	prop := z.newEntity("cz2:obj:rock")
	prop.short = "a rock"
	Move(prop, roomA)

	s := &session{character: "Mover", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Mover")
	Move(s.entity, roomA)
	z.players["Mover"] = s

	z.move(s, "east")

	if s.entity.location != roomB {
		t.Fatal("player did not move A->B")
	}
	if mobIn(roomB, "cz2:mob:chaser") == nil {
		t.Fatal("the co-located scripted mob did not witness + follow")
	}
	if mobIn(roomB, "cz2:obj:rock") != nil {
		t.Fatal("a non-scripted item was wrongly treated as a witness and moved")
	}
}
