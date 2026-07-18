package world

import (
	"context"
	"encoding/json"
	"math/rand"
	"testing"

	lua "github.com/yuin/gopher-lua"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// boss_loop_test.go — #55: the demo's scheduled-boss loop, end to end through the DEMO CONTENT (the engine
// mechanisms — director broadcast, mud.spawn, signal_world, the death trigger — are proven in scope_test.go;
// these tests prove the demo pack WIRES them correctly). Director->zone: the warstone-herald's on_world("spawn.
// boss") reactor does the mud.spawn. Zone->director: the goblin-chief's on("death") reports boss.died UP.

// countInRoom returns how many live (non-corpse) instances of proto stand in room.
func countInRoom(room *Entity, proto string) int {
	n := 0
	for _, e := range room.contents {
		if string(e.proto) == proto {
			n++
		}
	}
	return n
}

// TestBossLoopSpawnReactor: firing the director's spawn.boss world broadcast runs the warstone-herald's
// on_world reactor, which mud.spawns the boss into the lair (the director->zone half).
func TestBossLoopSpawnReactor(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())

	// The persistent warstone herald (reset-placed in the lair) holds the world-scope reactor.
	lair := z.rooms["darkwood:room:lair"]
	if lair == nil {
		t.Fatal("darkwood:room:lair missing from the demo zone")
	}
	if countInRoom(lair, "darkwood:mob:boss-herald") != 1 {
		t.Fatalf("the warstone herald must be reset-placed in the lair (found %d)",
			countInRoom(lair, "darkwood:mob:boss-herald"))
	}
	before := countInRoom(lair, "darkwood:mob:goblin-chief")

	// The director broadcasts spawn.boss down with the SpawnEvent payload (the same shape director.runSchedules
	// marshals: ref/proto/zone/room/announce). The warstone-herald's on_world handler reacts — matches ev.zone,
	// mud.spawns ev.proto — so a NEW chief appears in the lair.
	z.lua.fireScopeEvent("world", "spawn.boss",
		json.RawMessage(`{"ref":"boss:warden","proto":"darkwood:mob:goblin-chief","zone":"darkwood"}`))

	if after := countInRoom(lair, "darkwood:mob:goblin-chief"); after != before+1 {
		t.Fatalf("spawn.boss should have spawned one boss into the lair: %d -> %d (want +1)", before, after)
	}
}

// TestBossLoopDeathSignal: killing the scheduled boss fires its on("death") handler, which signal_world's
// boss.died UP to the world director (the zone->director half), carrying the boss identity.
func TestBossLoopDeathSignal(t *testing.T) {
	regions, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	s := NewMultiShard([]string{"darkwood"}, "darkwood", "", nil, nil)
	js := commbus.NewMemJetStream()
	bus := scopebus.New(commbus.NewMemBus()).WithDurable(js, "boss-test")
	s.WithScopeBus(bus, regions.Regions)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.scopes.signalLoop(ctx) // the off-zone-goroutine durable publisher

	worldEvents := make(chan scopebus.DurableEvent, 4)
	wc, err := bus.SubscribeDurable(scopebus.World(), "world-dir", func(ev scopebus.DurableEvent) bool {
		worldEvents <- ev
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wc.Stop() }()

	z := s.zones["darkwood"]
	lair := z.rooms["darkwood:room:lair"]
	if lair == nil {
		t.Fatal("darkwood:room:lair missing")
	}
	// Spawn the scheduled boss (the same reset op the director/zone uses); this registers its on("death")
	// handler on the fresh instance.
	z.runResets([]content.ResetDTO{{Op: "spawn_mob", Proto: "darkwood:mob:goblin-chief", Room: "darkwood:room:lair", Count: 1}})
	var chief *Entity
	for _, e := range lair.contents {
		if string(e.proto) == "darkwood:mob:goblin-chief" {
			chief = e
		}
	}
	if chief == nil {
		t.Fatal("goblin-chief did not spawn into the lair")
	}

	// Kill it through the real death pipeline (dealDamage -> depletion -> die -> the "death" trigger).
	hp := resourceCurrent(chief, "hp")
	c := &effectCtx{z: z, actor: chief, source: chief, target: chief, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
	dealDamage(c, chief, float64(hp)+50, "slash", "")

	// The death handler signal_world's boss.died UP to the world scope. The LOAD-BEARING key is `ref` (the
	// SCHEDULE ref the director matches on to reschedule the weekly timer, director.onBossDied) — assert it,
	// not just the flavor fields, so a payload that omits it (the loop silently never closing) fails here.
	ev := waitEvent(t, worldEvents, "boss.died world signal")
	if ev.Event != "boss.died" {
		t.Fatalf("world event = %q, want boss.died", ev.Event)
	}
	var p map[string]any
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("boss.died payload: %v", err)
	}
	if p["ref"] != "boss:warden" {
		t.Fatalf("boss.died payload = %v, want ref=boss:warden (the schedule ref director.onBossDied matches)", p)
	}
}

// TestScopeHandlerPrimedOnRoomEntity: a ROOM-level on_world handler (a room's own lua block) is primed and
// fires on a broadcast, not just a room OCCUPANT's — primeScopeHandlers must cover the room entity itself
// (it is a resolvable rid like any other), else the gap this fix closes just moves up one level.
func TestScopeHandlerPrimedOnRoomEntity(t *testing.T) {
	z, room, _ := scriptedZone(t)
	Add(room, &Scripted{source: `
		on_world("ping", function(ev)
			state.pinged = true
		end)
	`})
	// No trigger has primed the room script yet — the broadcast must prime it before firing.
	z.lua.fireScopeEvent("world", "ping", nil)
	es := z.lua.entityScripts[room.rid]
	if es == nil || es.state.RawGetString("pinged") != lua.LTrue {
		t.Fatal("a room-level on_world handler must be primed + fired by a broadcast")
	}
}
