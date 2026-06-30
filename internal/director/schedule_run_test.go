package director

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// schedule_run_test.go — the director-side scheduled-spawn loop end to end (hermetic, -race): spawn-when-
// due (broadcast DOWN), reschedule-on-death (signal UP), no double spawn, and the restart resume — the
// Phase-12.4 done-when, driven by an injected clock.

// fakeClock is an advanceable clock for the scheduler tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestSchedulerSpawnsRescheduleNoDoubleSpawnAndRestart(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")
	store := newMemStore()
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}

	// Capture the spawn commands the director broadcasts DOWN.
	var mu sync.Mutex
	var spawns []SpawnEvent
	sub, err := dirBus.Subscribe(scopebus.World(), func(event string, payload json.RawMessage, _ string) {
		if event != SpawnBossEvent {
			return
		}
		var se SpawnEvent
		if err := json.Unmarshal(payload, &se); err == nil {
			mu.Lock()
			spawns = append(spawns, se)
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	spawnCount := func() int { mu.Lock(); defer mu.Unlock(); return len(spawns) }

	sched := Schedule{Ref: "boss:warden", Proto: "warden", Zone: "duskwall", Interval: time.Hour, OnMissed: "spawn_if_overdue"}
	d1 := newSchedulerDirector(store, dirBus, "world-director-1", []Schedule{sched}, clock)
	ctx1, stop1 := context.WithCancel(context.Background())
	go d1.Run(ctx1)

	// 1. The boss has never spawned (zero state) -> due on the first tick -> one spawn broadcast.
	waitFor(t, "first spawn", func() bool { return spawnCount() == 1 })

	// 2. NO double spawn: while active, more ticks do not re-spawn.
	time.Sleep(120 * time.Millisecond)
	if spawnCount() != 1 {
		t.Fatalf("double spawn: %d spawns while the boss is active, want 1", spawnCount())
	}

	// 3. The boss dies (a zone signals UP) -> the director reschedules one interval out.
	bd, _ := json.Marshal(BossDied{Ref: "boss:warden"})
	if err := zoneBus.SignalDurable(context.Background(), scopebus.World(), BossDiedEvent, bd); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rescheduled (not due yet)", func() bool {
		raw, found, _ := d1.Get(context.Background(), scheduleKey("boss:warden"))
		if !found {
			return false
		}
		var st ScheduleState
		_ = json.Unmarshal(raw, &st)
		return !st.Active && st.NextSpawnAt.Equal(clock.now().Add(time.Hour))
	})
	beforeAdvance := spawnCount()
	time.Sleep(80 * time.Millisecond)
	if spawnCount() != beforeAdvance {
		t.Fatal("respawned before the interval elapsed")
	}

	// 4. Advance past the interval -> due again -> a second spawn.
	clock.advance(2 * time.Hour)
	waitFor(t, "respawn after the interval", func() bool { return spawnCount() == beforeAdvance+1 })

	// 5. Restart the director: stop #1, bring up #2 on the SAME store + bus. The boss is currently active
	// (spawned, not yet died), so #2 must NOT re-spawn it.
	stop1()
	time.Sleep(100 * time.Millisecond)
	atRestart := spawnCount()

	d2 := newSchedulerDirector(store, dirBus, "world-director-2", []Schedule{sched}, clock)
	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	go d2.Run(ctx2)
	time.Sleep(150 * time.Millisecond)
	if spawnCount() != atRestart {
		t.Fatalf("restart re-spawned an already-active boss: %d spawns, want %d (no double spawn across restart)", spawnCount(), atRestart)
	}
}

// newSchedulerDirector builds a director wired for the scheduler test. (A tiny constructor keeps the test
// body focused; it lives in the test file only.)
func newSchedulerDirector(store ScopeStore, bus *scopebus.Bus, source string, schedules []Schedule, clock *fakeClock) *Director {
	return New("", store, slog.Default()).
		WithScopeBus(bus, source).
		WithSchedules(schedules).
		WithNow(clock.now).
		WithTick(20 * time.Millisecond)
}
