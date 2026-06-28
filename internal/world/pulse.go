package world

import (
	"math"
	"time"
)

// pulseCount converts a content-defined, non-negative pulse count (an ability castTime/cooldown)
// to the wheel's uint64, flooring a nonsensical negative at 0 — the explicit non-negative bound
// the gosec G115 conversion check requires, and a defensive floor against malformed content.
func pulseCount(n int) uint64 {
	if n < 0 {
		return 0
	}
	return uint64(n)
}

// pulsesToInt narrows a BOUNDED pulse delta (a remaining-cooldown count: small and non-negative,
// after callers apply the `at > now` guard) to int, capping an implausibly large value at MaxInt
// so the narrowing can never wrap to a negative.
func pulsesToInt(d uint64) int {
	if d > uint64(math.MaxInt) {
		return math.MaxInt
	}
	return int(d)
}

// Heartbeat / pulse scheduler (docs/PHASE3-PLAN.md slice 4; the COMBAT.md/affects
// substrate). A MUD's world ticks: items decay, affects wear off, mobs wander, combat
// rounds resolve. All of that is periodic work that must mutate zone-owned entity state,
// so — by the single-writer invariant (MUDLIB §4) — it MUST run on the owning zone
// goroutine, never on a timer goroutine of its own. This file is the substrate that makes
// that true: a per-zone scheduler whose callbacks fire INSIDE the zone loop.
//
// # How it stays single-writer
//
// The scheduler holds NO timer of its own that touches entity state. Instead Zone.Run
// owns a single time.Ticker and adds one case to its existing select (inbox / ctx.Done /
// tick). When the ticker fires, Run calls scheduler.tick ON THE ZONE GOROUTINE, which
// runs every due callback synchronously, in line, before the loop returns to draining the
// inbox. A callback therefore has exactly the same access guarantees a command handler
// does: it is the sole writer of zone state for the duration of the call. Nothing here
// ever spawns a goroutine or reaches into another zone — cross-zone effects post to the
// other zone's inbox exactly as a command would.
//
// This is deliberately distinct from the existing time.AfterFunc calls (link-death reap,
// pending-TTL): those fire on an arbitrary timer goroutine and so only ever POST a message
// to the inbox (reapMsg, pendingExpireMsg) — they never touch entity state directly. The
// pulse scheduler is the complementary tool for work that wants to run as the zone: it is
// driven by the loop, not by a detached timer, so its callbacks may mutate freely.
//
// # Interval
//
// pulseInterval is the heartbeat granularity — the quantum every periodic/delayed callback
// is rounded to. Diku's PULSE is a quarter-second; we match that order of magnitude. It is
// long enough that an idle zone with no registered callbacks costs one cheap select wakeup
// per quarter-second (negligible) and short enough to drive combat rounds and affect ticks
// at the resolution those phases need. Combat (Phase 6) and affects (Phase 5) hang their
// own multiples off this (every-N-pulses), so the base interval is the only timing knob.
const pulseInterval = 250 * time.Millisecond

// pulseFunc is a unit of periodic or delayed work. It runs on the zone goroutine with full
// single-writer access to zone state. It returns whether it wants to be rescheduled: a
// periodic callback returns true to keep firing, a one-shot returns false to retire. The
// argument is the scheduler's monotonically increasing pulse counter, so a callback can key
// off "every Nth pulse" without keeping its own clock.
//
// Contract for callbacks that touch a PLAYER (none do this phase; combat/affects in Phase
// 5/6 will): a player can be transferred to another zone (transferIn re-homes the entity) or
// frozen mid-handoff between when the callback is registered and when it fires. A callback
// must therefore re-resolve the player by id through z.players each tick and stop (return
// false) if the player is absent or s.frozen — never close over and mutate a stale *Entity
// it captured at registration, or it would write an entity another zone now owns. The first
// real registrant should land a resolve-by-id-or-cancel helper for this.
type pulseFunc func(pulse uint64) (reschedule bool)

// scheduled is one registered callback plus the pulse number it next fires on. The
// scheduler keeps a flat slice and scans it each tick; at MUD per-zone callback counts
// (combat participants + active affects + a handful of zone timers) a linear scan per
// quarter-second is trivial and avoids a heap's allocation/complexity. If a zone ever holds
// thousands of concurrent timers this becomes a min-heap keyed by `at` — noted, not built.
type scheduled struct {
	at     uint64    // pulse number this callback next fires on
	every  uint64    // reschedule stride in pulses; 0 means one-shot (don't reschedule)
	fn     pulseFunc // the work, run on the zone goroutine
	cancel bool      // set by cancel(); the next tick drops it without firing
}

// pulseHandle is an opaque cancel token returned by registration. Holding it lets the
// caller stop a periodic callback (e.g. combat ending cancels its round timer). Cancelling
// is itself single-writer: it only ever runs on the zone goroutine (from a command handler
// or another callback), so it just flips a flag the next tick observes — no lock.
type pulseHandle struct{ s *scheduled }

// pulseScheduler is the per-zone heartbeat state. It is plain zone-owned data: only the
// zone goroutine (Run.tick and the registration helpers it calls from handlers/callbacks)
// ever touches it, so it needs no lock. `pulse` is the monotonic tick counter; `due` is the
// flat list of registered callbacks.
type pulseScheduler struct {
	pulse uint64
	due   []*scheduled
}

// newPulseScheduler returns an empty scheduler. Every zone gets one (newZone); a zone with
// no registered callbacks simply has tick fall through, so the ticker is a cheap no-op until
// content/combat/affects register work.
func newPulseScheduler() *pulseScheduler { return &pulseScheduler{} }

// every registers fn to fire every `pulses` heartbeats (minimum 1), starting `pulses` ticks
// from now. Returns a handle to cancel it. Called on the zone goroutine (a command handler,
// a zone-reset, or another callback); the scheduler is zone-owned so this is lock-free.
func (p *pulseScheduler) every(pulses uint64, fn pulseFunc) *pulseHandle {
	if pulses == 0 {
		pulses = 1
	}
	s := &scheduled{at: p.pulse + pulses, every: pulses, fn: fn}
	p.due = append(p.due, s)
	return &pulseHandle{s: s}
}

// after registers fn to fire ONCE, `pulses` heartbeats from now (minimum 1), then retire.
// Returns a handle so it can be cancelled before it fires. Zone-goroutine only.
func (p *pulseScheduler) after(pulses uint64, fn pulseFunc) *pulseHandle {
	if pulses == 0 {
		pulses = 1
	}
	s := &scheduled{at: p.pulse + pulses, every: 0, fn: fn}
	p.due = append(p.due, s)
	return &pulseHandle{s: s}
}

// cancel stops a registered callback from firing again. Idempotent and safe to call from
// inside the callback itself. Runs on the zone goroutine (the only writer of the scheduler),
// so it just marks the entry; tick reclaims it.
func (h *pulseHandle) cancel() {
	if h != nil && h.s != nil {
		h.s.cancel = true
	}
}

// tick advances the heartbeat by one and runs every callback that has come due, ALL on the
// zone goroutine (Run calls this from its select). It:
//
//   - increments the pulse counter;
//   - fires each due, non-cancelled callback in registration order;
//   - reschedules a periodic callback (every>0) by advancing its `at`, unless it returned
//     false or was cancelled; retires a one-shot;
//   - drops cancelled/retired entries.
//
// A callback may register MORE callbacks during the tick — combat scheduling its next round,
// an affect re-arming itself. To make that safe we SNAPSHOT the due list and reset p.due to
// empty BEFORE firing, so any every/after a callback runs appends to a fresh p.due that we
// merge back at the end (rather than clobbering it when we store the survivors). New entries
// always have an `at` strictly in the future (every/after add p.pulse+stride), so they are
// not picked up until a later tick — no same-tick cascade. tick never blocks (a callback that
// wants slow work posts to an inbox / does it async, exactly as a command must).
//
// (An earlier version compacted in place via p.due[:0] and silently LOST any callback
// registered during the tick; the snapshot/merge below is what makes register-during-tick
// actually safe — see TestPulseRegisterDuringTick.)
func (p *pulseScheduler) tick() {
	p.pulse++
	now := p.pulse
	// Hand callbacks a fresh p.due: anything they register during this tick lands on it and
	// is merged below, instead of being discarded when we store the compacted survivors.
	current := p.due
	p.due = nil
	var kept []*scheduled
	for _, s := range current {
		if s.cancel {
			continue // retired/cancelled: drop without firing
		}
		if s.at > now {
			kept = append(kept, s) // not due yet
			continue
		}
		reschedule := s.fn(now)
		if s.cancel { // the callback cancelled itself
			continue
		}
		if s.every > 0 && reschedule {
			s.at = now + s.every
			kept = append(kept, s)
		}
		// else: one-shot, or a periodic callback that asked to stop -> dropped.
	}
	// Merge in anything a callback registered during this tick (p.due was reset to nil above).
	p.due = append(kept, p.due...)
}
