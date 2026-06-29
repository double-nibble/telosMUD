package world

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/director"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// orchestration_capstone_test.go is the Phase-10 DONE-WHEN (docs/PHASE10-PLAN.md §10.5): the boss ripple,
// end to end and DURABLE — a zone signals a kill UP, the world director counts + persists it, and at the
// threshold broadcasts a remote effect DOWN that a zone reacts to — and the whole thing SURVIVES A
// DIRECTOR RESTART mid-sequence (the persisted scope state reloads; the durable signal stream resumes; no
// kill is lost or double-counted). It composes every 10.1–10.4 slice: scope-state CAS store + leader
// actor (10.1), the scoped bus transient+durable (10.2), region content + zone read-replica + signal-up
// (10.3), and director consume/apply/broadcast + on_world remote effects (10.4).

// capstoneStore is a tiny in-memory director.ScopeStore that PERSISTS across director instances (the
// restart reloads from it), with the same optimistic-concurrency CAS as the Postgres store.
type capstoneStore struct {
	mu    sync.Mutex
	world map[string]capRow
}
type capRow struct {
	val []byte
	ver uint64
}

func newCapstoneStore() *capstoneStore { return &capstoneStore{world: map[string]capRow{}} }

func (s *capstoneStore) LoadWorldState(_ context.Context, key string) ([]byte, uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.world[key]
	return r.val, r.ver, ok, nil
}

func (s *capstoneStore) SaveWorldState(_ context.Context, key string, value []byte, expected uint64) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.world[key]
	if cur.ver != expected {
		return cur.ver, false, nil // CAS lost
	}
	nv := cur.ver + 1
	s.world[key] = capRow{val: value, ver: nv}
	return nv, true, nil
}

// Region methods are unused by the WORLD director under test, but satisfy the interface.
func (s *capstoneStore) LoadRegionState(context.Context, string, string) ([]byte, uint64, bool, error) {
	return nil, 0, false, nil
}

func (s *capstoneStore) SaveRegionState(_ context.Context, _, _ string, _ []byte, expected uint64) (uint64, bool, error) {
	return expected, true, nil
}

// capstoneRipple is the world director's orchestration logic: count boss_slain into a persisted counter
// and, at the threshold, open the gate (a persisted+broadcast world flag) AND fire a remote effect the
// zones react to.
func capstoneRipple(threshold int) director.SignalHandler {
	return func(api *director.API, event string, _ json.RawMessage) {
		if event != "boss_slain" {
			return
		}
		n := 0
		if raw, ok := api.Get("bosses_slain"); ok {
			_ = json.Unmarshal(raw, &n)
		}
		n++
		nb, _ := json.Marshal(n)
		_ = api.Set("bosses_slain", nb) // persist + broadcast the count DOWN (read-replica)
		if n >= threshold {
			_ = api.Set("gate_opened", json.RawMessage(`true`))
			api.Broadcast("invasion_begins", json.RawMessage(`{"wave":1}`)) // remote effect DOWN
		}
	}
}

func TestOrchestrationCapstoneBossRippleSurvivesDirectorRestart(t *testing.T) {
	// Shared transports + a persistent scope-state store.
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	store := newCapstoneStore()

	regions, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}

	// A world shard hosting midgaard, wired to the scoped bus. A scripted "watcher" mob reacts to the
	// director's remote effect via on_world — proving a zone ACTS on a director command.
	s := NewMultiShard([]string{"midgaard"}, "midgaard", "", nil, nil)
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")
	s.WithScopeBus(zoneBus, regions.Regions)

	z := s.zones["midgaard"]
	room := z.rooms["midgaard:room:temple"]
	if room == nil {
		t.Fatal("midgaard temple room missing")
	}
	reacted := make(chan int, 4)
	// __reacted is called by the mob's on_world handler ON THE ZONE GOROUTINE; the channel hands the wave
	// number to the test goroutine race-free.
	z.lua.L.SetGlobal("__reacted", z.lua.L.NewFunction(func(l *lua.LState) int {
		reacted <- l.CheckInt(1)
		return 0
	}))
	watcher := addScriptedMob(z, room, "watcher", `
		on_world("invasion_begins", function(ev)
			__reacted(ev.wave)
		end)
	`)
	z.lua.ensureEntityScript(watcher) // register the on_world handler before the zone runs

	shardCtx, stopShard := context.WithCancel(context.Background())
	defer stopShard()
	go s.Run(shardCtx)

	// --- The first director: counts two kills, then we restart it BEFORE the threshold. ---
	dir1Bus := scopebus.New(mb).WithDurable(js, "world-director-1")
	dir1Ctx, stopDir1 := context.WithCancel(context.Background())
	d1 := director.New("", store, slog.Default()).
		WithScopeBus(dir1Bus, "world-director-1").
		WithSignalHandler(capstoneRipple(3)).
		WithTick(time.Hour)
	go d1.Run(dir1Ctx)

	signalKill := func(b *scopebus.Bus) {
		if err := b.SignalDurable(context.Background(), scopebus.World(), "boss_slain", json.RawMessage(`{"boss":"vurgoth"}`)); err != nil {
			t.Fatal(err)
		}
	}
	// Two kills land on director #1.
	signalKill(zoneBus)
	signalKill(zoneBus)
	waitCond(t, "director #1 counted 2 kills", func() bool {
		raw, found, _ := d1.Get(context.Background(), "bosses_slain")
		return found && string(raw) == "2"
	})

	// --- RESTART: stop director #1; bring up director #2 on the SAME store + transports. ---
	stopDir1()
	// Give #1's durable consumer a moment to fully stop so #2 resumes the durable cursor cleanly.
	time.Sleep(100 * time.Millisecond)

	dir2Bus := scopebus.New(mb).WithDurable(js, "world-director-2")
	dir2Ctx, stopDir2 := context.WithCancel(context.Background())
	defer stopDir2()
	d2 := director.New("", store, slog.Default()).
		WithScopeBus(dir2Bus, "world-director-2").
		WithSignalHandler(capstoneRipple(3)).
		WithTick(time.Hour)
	go d2.Run(dir2Ctx)

	// Director #2 reloaded the persisted count (2). The THIRD kill crosses the threshold.
	signalKill(zoneBus)

	// The ripple completes AFTER the restart: the watcher mob reacts to the director's remote effect.
	select {
	case wave := <-reacted:
		if wave != 1 {
			t.Fatalf("on_world ev.wave = %d, want 1", wave)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the zone never reacted to the director's remote effect (ripple did not complete across the restart)")
	}

	// Durable + no double-count: exactly 3 kills counted (not 4, not lost), and the gate persisted.
	waitCond(t, "director #2 counted exactly 3 (durable, no double-count)", func() bool {
		raw, found, _ := d2.Get(context.Background(), "bosses_slain")
		return found && string(raw) == "3"
	})
	if raw, found, _ := d2.Get(context.Background(), "gate_opened"); !found || string(raw) != "true" {
		t.Fatalf("gate_opened persisted = %q found=%v, want true", raw, found)
	}
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}
