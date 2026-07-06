package world

import (
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// TestRemoveRoomEvacuatesPlayer proves the #191 teardown: removing a live room re-places its player
// occupant to the zone start room (with a look, so they aren't stranded) and drops the room + its
// prototype from the zone.
func TestRemoveRoomEvacuatesPlayer(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	start := z.rooms[z.startRoom]
	market := z.rooms["midgaard:room:market"]
	if start == nil || market == nil {
		t.Fatal("demo midgaard should have a temple (start) and a market room")
	}

	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(s, "Hero")
	Move(s.entity, market)
	z.players["Hero"] = s

	z.removeRoom("midgaard:room:market")

	if s.entity.location != start {
		t.Fatalf("player was not evacuated to the start room; at %v", targetShort(s.entity.location))
	}
	if z.rooms["midgaard:room:market"] != nil {
		t.Fatal("removed room still live in z.rooms")
	}
	if z.protos.get("midgaard:room:market") != nil {
		t.Fatal("removed room prototype still in the cache")
	}
	if out := drainCombat(s); !contains(out, "swept somewhere safe") {
		t.Fatalf("player did not get the evacuation notice / look, got %v", out)
	}
}

// TestRemoveRoomRefusesStartRoom proves removeRoom will NOT delete the live start/login room — that would
// strand every fresh login. It stays live and the occupant is untouched.
func TestRemoveRoomRefusesStartRoom(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	start := z.rooms[z.startRoom]

	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(s, "Hero")
	Move(s.entity, start)
	z.players["Hero"] = s

	z.removeRoom(z.startRoom)

	if z.rooms[z.startRoom] == nil {
		t.Fatal("removeRoom deleted the live start room (must refuse)")
	}
	if s.entity.location != start {
		t.Fatal("player was moved out of the refused start room")
	}
}

// TestRemoveRoomIdempotent proves a second removal (a re-delivered reconcile) is a clean no-op.
func TestRemoveRoomIdempotent(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	z.removeRoom("midgaard:room:market")
	if z.rooms["midgaard:room:market"] != nil {
		t.Fatal("first removal did not drop the room")
	}
	// Second call: the ref is no longer live — must not panic and must stay removed.
	z.removeRoom("midgaard:room:market")
	if z.rooms["midgaard:room:market"] != nil {
		t.Fatal("second removal resurrected the room")
	}
	// A never-hosted ref is also a clean no-op.
	z.removeRoom("midgaard:room:does-not-exist")
}

// TestRemoveRoomDespawnsMobAndSeversCombat proves an ephemeral mob in a removed room is DESTROYED (not
// relocated to the start room), and that a combat link to a relocated player is severed — no fighting
// pointer survives the teardown.
func TestRemoveRoomDespawnsMobAndSeversCombat(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	start := z.rooms[z.startRoom]
	market := z.rooms["midgaard:room:market"]

	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(s, "Hero")
	Move(s.entity, market)
	z.players["Hero"] = s

	mob := combatMobIn(z, s.entity, "goblin") // spawned into the market (the player's room)
	z.startFight(s.entity, mob)

	z.removeRoom("midgaard:room:market")

	if s.entity.location != start {
		t.Fatal("player not evacuated to start")
	}
	if mob.location == start {
		t.Fatal("mob was relocated to the start room (should be despawned)")
	}
	if mob.location != nil {
		t.Fatalf("mob was not despawned (detached); still at %v", targetShort(mob.location))
	}
	if s.entity.living != nil && s.entity.living.fighting != nil {
		t.Fatal("player retains a fighting pointer after the room was torn down")
	}
}

// TestRemoveRoomDespawnsCarriedInventory proves the despawn recurses into a destroyed mob's held items, so
// a carried item is detached too (its Lua/spawn state would otherwise leak).
func TestRemoveRoomDespawnsCarriedInventory(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	market := z.rooms["midgaard:room:market"]

	mob := combatMobIn(z, &Entity{location: market}, "goblin") // spawned into the market
	loot := z.newEntity(ProtoRef("test:obj:coin"))
	loot.short = "a coin"
	Move(loot, mob) // the mob carries it

	z.removeRoom("midgaard:room:market")

	if mob.location != nil {
		t.Fatalf("carrier mob not despawned; at %v", targetShort(mob.location))
	}
	if loot.location != nil {
		t.Fatalf("carried item not despawned with its holder; at %v", targetShort(loot.location))
	}
}
