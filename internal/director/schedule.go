package director

import (
	"time"

	"github.com/double-nibble/telosmud/internal/content"
)

// schedule.go — the Phase-12.4 SCHEDULED-SPAWN decision engine (docs/LOOT-AND-SPAWNS.md §1): the pure
// logic a director uses to spawn long-timer bosses (a weekly world boss) on schedule, restart-safe. It is
// PURE (no I/O, deterministic given a `now`) so the done-when — a boss spawns when due, its death
// schedules the next, a restart resumes correctly with no double spawn or lost schedule — is exhaustively
// testable. The director wires it: load schedules + persisted state, check due on its heartbeat, broadcast
// the spawn DOWN to the owning zone, and reschedule on the boss's death (signals.go).

// Schedule is the runtime form of a content SpawnScheduleDTO.
type Schedule struct {
	Ref      string
	Proto    string
	Zone     string
	Room     string
	Interval time.Duration // respawn this long AFTER the boss dies
	OnMissed string        // "spawn_if_overdue" (default) | "skip_to_next"
	Announce string
}

// BuildSchedule maps a content.SpawnScheduleDTO onto a Schedule.
func BuildSchedule(d content.SpawnScheduleDTO) Schedule {
	return Schedule{
		Ref:      d.Ref,
		Proto:    d.Proto,
		Zone:     d.Zone,
		Room:     d.Room,
		Interval: time.Duration(d.IntervalAfterDeathSec) * time.Second,
		OnMissed: d.OnMissed,
		Announce: d.Announce,
	}
}

// ScheduleState is a schedule's persisted runtime: when the boss should next spawn, and whether one is
// currently alive. Stored in director scope state (one key per schedule) so it survives a restart. The
// zero value (NextSpawnAt zero, Active false) means "never spawned" — due immediately on first boot.
type ScheduleState struct {
	NextSpawnAt time.Time `json:"next_spawn_at"`
	Active      bool      `json:"active"` // a boss is currently spawned/alive (do not re-spawn)
}

// IsDue reports whether a schedule should spawn at `now`: no boss currently alive AND the next-spawn time
// has arrived (a zero NextSpawnAt is always due — the first-ever spawn).
func IsDue(st ScheduleState, now time.Time) bool {
	return !st.Active && !now.Before(st.NextSpawnAt)
}

// AfterSpawn is the state right after the director spawns the boss: active, so it does not re-spawn each
// tick. NextSpawnAt is left as-is; it is recomputed from the death time when the boss dies.
func AfterSpawn(st ScheduleState) ScheduleState {
	st.Active = true
	return st
}

// AfterDeath is the state after the boss dies: inactive, with the next spawn one Interval out from the
// death. This is what makes "weekly = 7d after it dies" hold.
func AfterDeath(s Schedule, deathTime time.Time) ScheduleState {
	return ScheduleState{NextSpawnAt: deathTime.Add(s.Interval), Active: false}
}

// ApplyMissed adjusts a schedule's state at director STARTUP for a window that passed during downtime (the
// restart-safety policy). A schedule that is not active and whose NextSpawnAt is in the past is "overdue":
//   - spawn_if_overdue (the default): leave it — the next IsDue check spawns it immediately.
//   - skip_to_next: advance NextSpawnAt to now + Interval, skipping the missed window (the boss does not
//     pop the instant the director comes back; it waits a fresh interval).
//
// An active schedule (a boss the director believes is alive) is left untouched — the zone still hosts it
// (or it died during downtime and the zone's death signal, replayed durably, will reschedule it).
func ApplyMissed(s Schedule, st ScheduleState, now time.Time) ScheduleState {
	if st.Active || !st.NextSpawnAt.Before(now) {
		return st // not overdue
	}
	if s.OnMissed == "skip_to_next" {
		st.NextSpawnAt = now.Add(s.Interval)
	}
	// spawn_if_overdue: leave st due (the default) — the next IsDue spawns it.
	return st
}
