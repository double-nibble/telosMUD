package director

import (
	"context"
	"encoding/json"
)

// schedule_run.go — the director-side wiring of the scheduled-spawn engine (schedule.go, Phase 12.4): the
// tick-driven spawn loop + the boss-death reschedule, persisting each schedule's ScheduleState in scope
// state so the whole thing is restart-safe. The decision logic is the pure engine; this is the I/O.

// SpawnEvent is the remote-effect payload the director broadcasts DOWN to spawn a scheduled boss. The
// hosting zone's on_world handler matches Zone + spawns Proto (in Room), announcing if set. (Phase 12.4)
type SpawnEvent struct {
	Ref      string `json:"ref"`
	Proto    string `json:"proto"`
	Zone     string `json:"zone"`
	Room     string `json:"room,omitempty"`
	Announce string `json:"announce,omitempty"`
}

// BossDied is the signal-up payload a zone sends when a scheduled boss dies, so the director reschedules.
type BossDied struct {
	Ref string `json:"ref"`
}

// SpawnBossEvent / BossDiedEvent are the reserved scoped-event names for the spawn command (DOWN) and the
// death report (UP). Content's spawn handler subscribes on_world(SpawnBossEvent); the boss's death content
// signals_world(BossDiedEvent).
const (
	SpawnBossEvent = "spawn.boss"
	BossDiedEvent  = "boss.died"
)

// WithSchedules wires the scheduled-spawn loop (Phase 12.4): the director spawns these bosses when due
// (the tick) and reschedules each on its death (the boss.died signal-up). It composes any existing signal
// handler — a boss.died event drives the scheduler; every other event still reaches the prior handler.
// Call before Run.
func (d *Director) WithSchedules(schedules []Schedule) *Director {
	d.schedules = schedules
	prev := d.handler
	d.handler = func(api *API, event string, payload json.RawMessage) {
		if event == BossDiedEvent {
			d.onBossDied(api, payload)
			return
		}
		if prev != nil {
			prev(api, event, payload)
		}
	}
	return d
}

// scheduleInitDone tracks whether the startup on_missed pass has run this session. Reset is implicit: a
// fresh Director (a restart) re-applies on_missed against the reloaded state.
func (d *Director) runSchedules(ctx context.Context) {
	now := d.now()
	if !d.scheduleInit {
		// Startup pass: apply the on_missed policy to any window that passed during downtime, once.
		for _, s := range d.schedules {
			st := d.loadScheduleState(ctx, s.Ref)
			adjusted := ApplyMissed(s, st, now)
			if adjusted != st {
				d.saveScheduleState(ctx, s.Ref, adjusted)
			}
		}
		d.scheduleInit = true
	}
	for _, s := range d.schedules {
		st := d.loadScheduleState(ctx, s.Ref)
		if !IsDue(st, now) {
			continue
		}
		// Spawn: broadcast the command DOWN to the owning zone, then mark active + persist so it does not
		// re-spawn each tick (the no-double-spawn guarantee).
		body, err := json.Marshal(SpawnEvent{Ref: s.Ref, Proto: s.Proto, Zone: s.Zone, Room: s.Room, Announce: s.Announce})
		if err != nil {
			d.log.Warn("scheduler: marshal spawn event", "schedule", s.Ref, "err", err)
			continue
		}
		d.broadcastDown(ctx, SpawnBossEvent, body)
		d.saveScheduleState(ctx, s.Ref, AfterSpawn(st))
		d.log.Info("scheduler: spawned scheduled boss", "schedule", s.Ref, "proto", s.Proto, "zone", s.Zone)
	}
}

// onBossDied reschedules a schedule when its boss dies (the next spawn is one interval out from now).
// Runs on the actor goroutine (via the signal handler), so the scope-state write is single-writer.
func (d *Director) onBossDied(api *API, payload json.RawMessage) {
	var bd BossDied
	if err := json.Unmarshal(payload, &bd); err != nil || bd.Ref == "" {
		return
	}
	for _, s := range d.schedules {
		if s.Ref == bd.Ref {
			if !d.saveScheduleState(api.ctx, s.Ref, AfterDeath(s, d.now())) {
				// Do NOT claim a reschedule that did not persist. A failed write leaves the row Active,
				// which makes IsDue false forever and ApplyMissed skip it on restart — the boss never
				// respawns again. The old unconditional Info line reported success for exactly that
				// permanent wedge, making the only operator-visible artifact of the bug a lie.
				//
				// On a CAS loss the write is also recoverable now: d.set recorded it, so handleSignal
				// NAKs this signal and the redelivery re-runs the reschedule against fresh state (#354).
				d.log.Warn("scheduler: boss death reschedule did NOT persist; the schedule stays active "+
					"until a redelivery or the next tick reconciles it", "schedule", s.Ref)
				return
			}
			d.log.Info("scheduler: scheduled boss died; rescheduled", "schedule", s.Ref, "next_in", s.Interval)
			return
		}
	}
}

// loadScheduleState reads a schedule's persisted ScheduleState from scope state (the zero value when
// unset — never spawned). Actor goroutine.
func (d *Director) loadScheduleState(ctx context.Context, ref string) ScheduleState {
	r := d.get(ctx, scheduleKey(ref))
	if r.err != nil || !r.found || len(r.value) == 0 {
		return ScheduleState{}
	}
	var st ScheduleState
	if err := json.Unmarshal(r.value, &st); err != nil {
		return ScheduleState{}
	}
	return st
}

// saveScheduleState persists a schedule's ScheduleState to scope state (versioned CAS via d.set) and
// reports whether the write LANDED. Actor goroutine.
//
// The bool is not decoration: the caller cannot infer success from anything else, and a dropped write here
// is not the benign "retry next tick" the old comment claimed. AfterSpawn persists Active:true, and an
// active schedule is skipped by IsDue AND left alone by ApplyMissed on restart — so a lost write there
// wedges that boss's respawn permanently, across restarts.
//
// Only onBossDied consumes the result today, because only it can act on the answer: its caller is a
// SIGNAL, so a failure NAKs and the redelivery re-runs the reschedule (#354). The two runSchedules call
// sites deliberately do NOT branch on it — they run on the TICK, which has no redelivery to fall back on,
// and the pre-existing broadcast-then-persist ordering there means a lost persist re-broadcasts a spawn
// next tick (a double spawn) rather than losing one. Correcting that needs a spawn-ordering change with
// its own failure analysis, not a bool check; the failure is at least no longer silent, since this logs
// a Warn on every dropped write.
func (d *Director) saveScheduleState(ctx context.Context, ref string, st ScheduleState) bool {
	body, err := json.Marshal(st)
	if err != nil {
		d.log.Warn("scheduler: marshal schedule state", "schedule", ref, "err", err)
		return false
	}
	if _, err := d.set(ctx, scheduleKey(ref), body); err != nil {
		d.log.Warn("scheduler: persist schedule state failed", "schedule", ref, "err", err)
		return false
	}
	return true
}

func scheduleKey(ref string) string { return "schedule:" + ref }
