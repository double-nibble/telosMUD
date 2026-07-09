package world

import (
	"reflect"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// wander_test.go — #202 scenario 1 (wandering mob). A scripted mob armed with a self-rescheduling mud.after
// loop should start moving on its OWN, from the moment it is reset-spawned, WITHOUT a player ever reaching it.
// The enabling engine change is fireSpawn (reset.go): priming the entity script at spawn + firing on("spawn"),
// so the wander loop arms immediately rather than lazily on the first player-driven trigger.

// wanderZone builds a two-room zone (A --east--> B --west--> A) with a wandering-mob prototype whose on("spawn")
// arms a mud.after loop that steps through the first available exit each pulse.
func wanderZone(t *testing.T) (*Zone, *Entity, *Entity) {
	t.Helper()
	z := newZone("wz")
	roomA := z.newEntity("wz:room:a")
	Add(roomA, &Room{exits: map[string]ProtoRef{"east": "wz:room:b"}})
	z.rooms["wz:room:a"] = roomA
	roomB := z.newEntity("wz:room:b")
	Add(roomB, &Room{exits: map[string]ProtoRef{"west": "wz:room:a"}})
	z.rooms["wz:room:b"] = roomB

	z.protos.define("wz:mob:wanderer", nil, "a wanderer", "A restless wanderer paces here.", componentSet{
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

func mobIn(room *Entity, proto string) *Entity {
	for _, e := range room.contents {
		if string(e.proto) == proto {
			return e
		}
	}
	return nil
}

// TestWanderingMobStartsUnprompted: a reset-spawned wanderer begins moving on the pulse wheel with NO player
// interaction — the fireSpawn prime is what arms its loop at spawn (previously it stayed inert until greeted).
func TestWanderingMobStartsUnprompted(t *testing.T) {
	z, roomA, roomB := wanderZone(t)

	// Reset-spawn the wanderer into A. No player is ever in the zone.
	z.applyReset(&content.ResetDTO{Op: "spawn_mob", Proto: "wz:mob:wanderer", Room: "wz:room:a", Count: 1})
	w := mobIn(roomA, "wz:mob:wanderer")
	if w == nil {
		t.Fatal("wanderer was not spawned into room A")
	}
	// The on("spawn") hook armed a mud.after loop at spawn — before any pulse, a live wheel entry exists.
	if z.pulses.pulse != 0 {
		t.Fatalf("precondition: expected pulse 0, got %d", z.pulses.pulse)
	}

	// Drive the wheel. The loop steps the mob through the single exit each fire, so within a few pulses it
	// has left A and reached B — entirely unprompted.
	sawB := false
	for i := 0; i < 6 && !sawB; i++ {
		z.pulses.tick()
		if mobIn(roomB, "wz:mob:wanderer") != nil {
			sawB = true
		}
	}
	if !sawB {
		t.Fatal("the wanderer never moved from A to B on its own (fireSpawn did not arm the wander loop at spawn)")
	}
}

// TestDemoWispWandersDarkwood drives the ACTUAL shipped demo wanderer (darkwood:mob:wisp), so a runtime
// error in its wander Lua — self:room():exits(), the cross-zone `to` filter, self:move, mud.after — is caught
// here, not just in the synthetic-mob test above. Grove's only SAME-ZONE exit is north→hollow (south leaves
// the zone and is filtered out), so the wisp deterministically drifts grove→hollow with no player present.
func TestDemoWispWandersDarkwood(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	z.applyReset(&content.ResetDTO{Op: "spawn_mob", Proto: "darkwood:mob:wisp", Room: "darkwood:room:grove", Count: 1})
	if mobIn(z.rooms["darkwood:room:grove"], "darkwood:mob:wisp") == nil {
		t.Fatal("demo wisp was not spawned into grove")
	}
	// The wisp steps every ~20 pulses (mud.after(20)); drive well past one interval.
	hollow := z.rooms["darkwood:room:hollow"]
	moved := false
	for i := 0; i < 40 && !moved; i++ {
		z.pulses.tick()
		if mobIn(hollow, "darkwood:mob:wisp") != nil {
			moved = true
		}
	}
	if !moved {
		t.Fatal("the shipped demo wisp did not wander grove->hollow (its wander Lua errored, or fireSpawn did not arm it)")
	}
}

// TestRoamResetDoesNotLeakAcrossRepops (#202) pins the fix for the wandering-mob repop leak: a room-scoped
// top-up would see the spawn room empty once the wisp wanders off and spawn a replacement EVERY repop
// (unbounded population + timer growth). A roam:true reset counts the proto ZONE-WIDE, so once the single
// wisp has drifted to another room, re-running the reset is a no-op — exactly one wisp, no matter how many
// repop ticks fire.
func TestRoamResetDoesNotLeakAcrossRepops(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	roam := content.ResetDTO{Op: "spawn_mob", Proto: "darkwood:mob:wisp", Room: "darkwood:room:grove", Count: 1, Roam: true}

	// Boot: one wisp in grove.
	z.applyReset(&roam)
	if got := countProtoInZone(z, "darkwood:mob:wisp"); got != 1 {
		t.Fatalf("after boot reset: %d wisps, want 1", got)
	}
	// Let it wander off grove, then simulate repeated repop ticks re-running the same reset. Because the
	// count is zone-wide, each is a no-op — the population stays pinned at 1.
	for i := 0; i < 60; i++ {
		z.pulses.tick()
	}
	if mobIn(z.rooms["darkwood:room:grove"], "darkwood:mob:wisp") != nil {
		t.Fatal("precondition: the wisp should have wandered off grove by now")
	}
	for r := 0; r < 5; r++ {
		z.applyReset(&roam) // a repop tick
	}
	if got := countProtoInZone(z, "darkwood:mob:wisp"); got != 1 {
		t.Fatalf("after 5 repops of a roam reset: %d wisps, want 1 (a room-scoped count would have leaked replacements)", got)
	}

	// Contrast: the SAME reset WITHOUT roam leaks — once grove is empty, each repop spawns a replacement.
	z2 := newDemoZone("darkwood", newProtoCache())
	roomScoped := content.ResetDTO{Op: "spawn_mob", Proto: "darkwood:mob:wisp", Room: "darkwood:room:grove", Count: 1}
	z2.applyReset(&roomScoped)
	for i := 0; i < 60; i++ {
		z2.pulses.tick()
	}
	for r := 0; r < 5; r++ {
		z2.applyReset(&roomScoped)
	}
	if got := countProtoInZone(z2, "darkwood:mob:wisp"); got <= 1 {
		t.Fatalf("room-scoped control: expected the leak (>1 wisp after repops), got %d — the roam fix is untested", got)
	}
}

// TestUnprimedMobStaysPutWithoutSpawnHook is the negative control: an IDENTICAL wander loop that arms only
// from a player-driven trigger (on("greet")) does NOT start moving with no player present — proving the
// unprompted movement above is the fireSpawn prime, not incidental.
func TestUnprimedMobStaysPutWithoutSpawnHook(t *testing.T) {
	z := newZone("wz2")
	roomA := z.newEntity("wz2:room:a")
	Add(roomA, &Room{exits: map[string]ProtoRef{"east": "wz2:room:b"}})
	z.rooms["wz2:room:a"] = roomA
	roomB := z.newEntity("wz2:room:b")
	Add(roomB, &Room{exits: map[string]ProtoRef{"west": "wz2:room:a"}})
	z.rooms["wz2:room:b"] = roomB
	z.protos.define("wz2:mob:idler", nil, "an idler", "An idler loiters here.", componentSet{
		reflect.TypeFor[*Living](): &Living{},
		reflect.TypeFor[*Scripted](): &Scripted{source: `
			on("greet", function()
				local function step()
					local ex = self:room():exits()
					if #ex > 0 then self:move(ex[1].dir) end
					mud.after(1, step)
				end
				mud.after(1, step)
			end)`},
	})
	z.applyReset(&content.ResetDTO{Op: "spawn_mob", Proto: "wz2:mob:idler", Room: "wz2:room:a", Count: 1})
	if mobIn(roomA, "wz2:mob:idler") == nil {
		t.Fatal("idler was not spawned into A")
	}
	for i := 0; i < 6; i++ {
		z.pulses.tick()
	}
	if mobIn(roomA, "wz2:mob:idler") == nil {
		t.Fatal("the greet-armed idler moved with no player present — its loop should NOT have armed at spawn")
	}
}
