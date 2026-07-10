package world

import (
	"reflect"
	"testing"
)

// persistent_spawn_test.go — #304. The ephemeral repop path primes a scripted entity and fires on("spawn")
// (applyReset -> fireSpawn, reset.go), which is what arms a wander/behavior loop the instant a mob is placed.
// The PERSISTENT path (rehydrateObjects -> spawnPersistent), which loads a durable mob from object_instances
// rather than re-spawning it each repop, did not — so a durable scripted mob would load inert and never
// receive on("spawn").
//
// The fix fires spawn the instant the entity is placed, BEFORE its durable contents are rehydrated, so the two
// paths share one contract: on("spawn") fires with an empty inventory (the ephemeral path arms inventory via a
// separate later reset op), and every scripted child spawns under a fully-spawned parent.

// persistentWanderZone builds the same two-room wander zone as wander_test, so the SAME scripted-mob fixture
// (its on("spawn") arms a mud.after step loop) can be driven through the PERSISTENT load path.
func persistentWanderZone(t *testing.T) (*Zone, *Entity, *Entity) {
	t.Helper()
	z := newZone("pwz")
	roomA := z.newEntity("pwz:room:a")
	Add(roomA, &Room{exits: map[string]ProtoRef{"east": "pwz:room:b"}})
	z.rooms["pwz:room:a"] = roomA
	roomB := z.newEntity("pwz:room:b")
	Add(roomB, &Room{exits: map[string]ProtoRef{"west": "pwz:room:a"}})
	z.rooms["pwz:room:b"] = roomB

	z.protos.define("pwz:mob:wanderer", nil, "a wanderer", "A restless wanderer paces here.", componentSet{
		reflect.TypeFor[*Living](): &Living{},
		reflect.TypeFor[*Scripted](): &Scripted{source: `
			on("spawn", function()
				local function step()
					local ex = self:room():exits()
					if #ex > 0 then self:move(ex[1].dir) end
					mud.after(1, step)
				end
				mud.after(1, step)
			end)`},
	})
	return z, roomA, roomB
}

// TestPersistentScriptedMobReceivesSpawn is the #304 headline. A durable scripted mob loaded through the
// persistent path must wander on its own, exactly as an ephemerally reset-spawned one does — proving
// on("spawn") fired and armed its loop. Before the fix it stayed inert (loaded but never primed).
func TestPersistentScriptedMobReceivesSpawn(t *testing.T) {
	z, roomA, roomB := persistentWanderZone(t)

	// Load the wanderer via the PERSISTENT path — the loadObjectsMsg a durable object_instances load posts,
	// applied on the zone goroutine (rehydrateObjects), NOT the ephemeral applyReset repop.
	z.rehydrateObjects(loadObjectsMsg{
		target:  roomA,
		objects: []PersistentObject{{ProtoRef: "pwz:mob:wanderer"}},
	})
	w := mobIn(roomA, "pwz:mob:wanderer")
	if w == nil {
		t.Fatal("the persistent wanderer was not loaded into room A")
	}

	sawB := false
	for i := 0; i < 6 && !sawB; i++ {
		z.pulses.tick()
		if mobIn(roomB, "pwz:mob:wanderer") != nil {
			sawB = true
		}
	}
	if !sawB {
		t.Fatal("the persistent wanderer never moved on its own — the persistent load path did not fire " +
			"on(\"spawn\") to arm its wander loop (#304)")
	}
}

// TestPersistentScriptedChildIsPrimed pins that the recursion fires on("spawn") on nested scripted children,
// not just the top-level object. A durable object is a container tree, and a scripted item deeper in it must
// be armed too. The child here is a scripted parcel INSIDE a persistent chest; its on("spawn") arms a
// pulse-wheel callback, so a non-empty wheel after the load proves the child was primed — only a scripted
// entity's on("spawn") could have registered it.
func TestPersistentScriptedChildIsPrimed(t *testing.T) {
	z := newZone("psc")
	room := z.newEntity("psc:room:hall")
	Add(room, &Room{})
	z.rooms["psc:room:hall"] = room

	z.protos.define("psc:obj:chest", []string{"chest"}, "a chest", "A heavy chest.", componentSet{})
	z.protos.define("psc:obj:parcel", []string{"parcel"}, "a parcel", "A small parcel.", componentSet{
		reflect.TypeFor[*Scripted](): &Scripted{source: `
			on("spawn", function() mud.after(1, function() end) end)`},
	})

	if len(z.pulses.due) != 0 {
		t.Fatalf("precondition: the wheel must start empty, got %d entries", len(z.pulses.due))
	}
	// A chest containing the scripted parcel, loaded through the persistent path.
	z.rehydrateObjects(loadObjectsMsg{
		target: room,
		objects: []PersistentObject{{
			ProtoRef: "psc:obj:chest",
			Contents: []PersistentObject{{ProtoRef: "psc:obj:parcel"}},
		}},
	})

	if len(z.pulses.due) == 0 {
		t.Fatal("the nested scripted parcel was not primed — spawnPersistent's recursion did not fire " +
			"on(\"spawn\") on the child (#304)")
	}
}

// TestPersistentNonScriptedObjectIsInert is the no-op guarantee: a durable NON-scripted object — the common
// case — must load with no spurious behavior. fireSpawn returns early for a scriptless entity, so nothing is
// armed on the wheel.
func TestPersistentNonScriptedObjectIsInert(t *testing.T) {
	z := newZone("pni")
	room := z.newEntity("pni:room:vault")
	Add(room, &Room{})
	z.rooms["pni:room:vault"] = room
	z.protos.define("pni:obj:anvil", []string{"anvil"}, "an anvil", "A plain anvil.", componentSet{})

	z.rehydrateObjects(loadObjectsMsg{
		target:  room,
		objects: []PersistentObject{{ProtoRef: "pni:obj:anvil"}},
	})

	if mobIn(room, "pni:obj:anvil") == nil {
		t.Fatal("the durable object did not load")
	}
	if n := len(z.pulses.due); n != 0 {
		t.Fatalf("a non-scripted durable object armed %d pulse callback(s); it must be inert", n)
	}
}
