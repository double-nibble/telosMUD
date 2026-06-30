package director

import (
	"testing"
	"time"
)

// schedule_test.go — the pure Phase-12.4 scheduled-spawn engine: due-checking, reschedule-on-death, and
// the restart on_missed policy. Deterministic given a `now`, so the done-when is exhaustively testable.

func TestIsDue(t *testing.T) {
	now := time.Unix(1000, 0)
	cases := []struct {
		name string
		st   ScheduleState
		want bool
	}{
		{"never spawned (zero state)", ScheduleState{}, true},
		{"due", ScheduleState{NextSpawnAt: now.Add(-time.Minute)}, true},
		{"not yet", ScheduleState{NextSpawnAt: now.Add(time.Minute)}, false},
		{"active (boss alive) — never due", ScheduleState{NextSpawnAt: now.Add(-time.Hour), Active: true}, false},
	}
	for _, c := range cases {
		if got := IsDue(c.st, now); got != c.want {
			t.Errorf("%s: IsDue = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAfterDeathSchedulesNextInterval(t *testing.T) {
	s := Schedule{Ref: "boss", Interval: 7 * 24 * time.Hour}
	death := time.Unix(1000, 0)
	st := AfterDeath(s, death)
	if st.Active {
		t.Fatal("after death the boss must be inactive")
	}
	if !st.NextSpawnAt.Equal(death.Add(7 * 24 * time.Hour)) {
		t.Fatalf("next spawn = %v, want death + 7d", st.NextSpawnAt)
	}
}

func TestApplyMissedSpawnIfOverdue(t *testing.T) {
	s := Schedule{Ref: "boss", Interval: time.Hour, OnMissed: "spawn_if_overdue"}
	now := time.Unix(10000, 0)
	// Overdue (next spawn passed during downtime): spawn_if_overdue leaves it due.
	st := ScheduleState{NextSpawnAt: now.Add(-time.Hour)}
	got := ApplyMissed(s, st, now)
	if !IsDue(got, now) {
		t.Fatal("spawn_if_overdue must leave an overdue schedule DUE (spawns on restart)")
	}
}

func TestApplyMissedSkipToNext(t *testing.T) {
	s := Schedule{Ref: "boss", Interval: time.Hour, OnMissed: "skip_to_next"}
	now := time.Unix(10000, 0)
	st := ScheduleState{NextSpawnAt: now.Add(-time.Hour)} // overdue
	got := ApplyMissed(s, st, now)
	if IsDue(got, now) {
		t.Fatal("skip_to_next must NOT spawn the missed window immediately")
	}
	if !got.NextSpawnAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("skip_to_next: next = %v, want now + interval", got.NextSpawnAt)
	}
}

func TestApplyMissedLeavesFutureAndActiveAlone(t *testing.T) {
	s := Schedule{Ref: "boss", Interval: time.Hour, OnMissed: "skip_to_next"}
	now := time.Unix(10000, 0)
	future := ScheduleState{NextSpawnAt: now.Add(time.Hour)}
	if got := ApplyMissed(s, future, now); got != future {
		t.Fatal("a not-yet-due schedule must be left untouched")
	}
	active := ScheduleState{NextSpawnAt: now.Add(-time.Hour), Active: true}
	if got := ApplyMissed(s, active, now); got != active {
		t.Fatal("an active (boss-alive) schedule must be left untouched")
	}
}
