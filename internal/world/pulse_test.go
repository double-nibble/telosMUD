package world

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Tests for the heartbeat / pulse scheduler (pulse.go). The load-bearing properties:
//
//   - a registered callback fires, and fires ON THE ZONE GOROUTINE (single-writer);
//   - a periodic callback fires repeatedly; a one-shot fires once;
//   - cancel stops a callback;
//   - a callback registered from INSIDE a firing callback is not lost (the combat/affects
//     self-rescheduling pattern);
//   - registering callbacks does not perturb the deterministic message-path tests (those
//     register none, so this is implicitly covered by the rest of the suite staying green).

// TestPulseFiresOnZoneGoroutine verifies a callback registered on the scheduler runs, and
// runs on the zone loop goroutine — proven by having the callback read zone-owned state
// (the players map) without a lock and post a sentinel out, exactly as a command handler
// would. If it ran on another goroutine the -race detector would flag the players-map read.
func TestPulseFiresOnZoneGoroutine(t *testing.T) {
	z := NewDemoShard().Zone()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Capture the goroutine the callback runs on by recording a value the zone loop sets.
	fired := make(chan int, 4)
	var firedFromZone atomic.Bool

	// A player so the callback has zone-owned state to touch race-free.
	alice := newTestPlayerEntity(z, "Alice")
	z.post(joinMsg{s: alice})

	// Register a one-shot via a small bootstrap message so it lands on the zone goroutine
	// (registration is single-writer). We reuse inputMsg-style posting through a closure
	// run inside the loop: post a func through the inbox is not a thing, so register before
	// Run starts — the scheduler is plain zone data and Run reads it.
	z.pulses.after(1, func(pulse uint64) bool {
		// Reading z.players here is only safe because this runs on the zone goroutine.
		_ = len(z.players)
		firedFromZone.Store(true)
		fired <- pulsesToInt(pulse)
		return false
	})

	go z.Run(ctx)

	select {
	case p := <-fired:
		if p < 1 {
			t.Fatalf("callback fired with pulse %d, want >=1", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pulse callback never fired")
	}
	if !firedFromZone.Load() {
		t.Fatal("callback did not record running")
	}
}

// TestPulsePeriodicAndCancel verifies a periodic callback fires more than once and that
// cancel stops it. Counts ticks over a short window.
func TestPulsePeriodicAndCancel(t *testing.T) {
	z := newZone("test")
	var count atomic.Int64
	h := z.pulses.every(1, func(_ uint64) bool {
		count.Add(1)
		return true
	})
	// Register the canceller BEFORE Run starts (registration must be single-writer; once
	// Run owns the scheduler the test goroutine must not touch p.due). It fires on the zone
	// goroutine after 4 pulses and cancels the periodic callback there — exactly how a
	// command/another callback would cancel a timer.
	z.pulses.after(4, func(uint64) bool { h.cancel(); return false })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	// Wait for a few ticks (the periodic ran before the cancel landed).
	deadline := time.After(2 * time.Second)
	for count.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("periodic callback fired only %d times, want >=3", count.Load())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Let the canceller fire and a few more ticks elapse; the count must stop climbing.
	time.Sleep(6 * pulseInterval)
	got := count.Load()
	time.Sleep(4 * pulseInterval)
	if delta := count.Load() - got; delta != 0 {
		t.Fatalf("callback kept firing after cancel: +%d", delta)
	}
}

// TestPulseOneShotRetires verifies a one-shot fires exactly once.
func TestPulseOneShotRetires(t *testing.T) {
	z := newZone("test")
	var count atomic.Int64
	z.pulses.after(1, func(uint64) bool {
		count.Add(1)
		return false
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	time.Sleep(5 * pulseInterval)
	if got := count.Load(); got != 1 {
		t.Fatalf("one-shot fired %d times, want exactly 1", got)
	}
}

// TestPulseRegisterDuringTick guards the bug where a callback registered from INSIDE a
// firing callback was silently dropped (the old in-place-compaction tick lost it). Combat
// scheduling its next round, or an affect re-arming, depends on this working. Drives tick()
// directly for determinism — no goroutine/sleep.
func TestPulseRegisterDuringTick(t *testing.T) {
	p := newPulseScheduler()
	followFired := false
	// A one-shot that, when it fires, registers a follow-up one-shot for the next pulse.
	p.after(1, func(uint64) bool {
		p.after(1, func(uint64) bool { followFired = true; return false })
		return false
	})
	p.tick() // pulse 1: registrar fires and schedules the follow-up (due at pulse 2)
	if followFired {
		t.Fatal("follow-up fired too early (should be due next pulse)")
	}
	p.tick() // pulse 2: the follow-up must fire — it was registered DURING pulse 1's tick
	if !followFired {
		t.Fatal("callback registered during a tick was lost (never fired)")
	}
}

// TestPulseSelfReschedulingChain models the combat-round pattern: each firing schedules the
// next via after(1) from inside itself, a chain that must keep going across ticks. Exercises
// the register-during-tick path repeatedly.
func TestPulseSelfReschedulingChain(t *testing.T) {
	p := newPulseScheduler()
	fires := 0
	var rearm pulseFunc
	rearm = func(uint64) bool {
		fires++
		p.after(1, rearm) // schedule the next "round" from inside this one
		return false      // one-shot; the re-arm above is what continues the chain
	}
	p.after(1, rearm)
	for i := 0; i < 5; i++ {
		p.tick()
	}
	if fires != 5 {
		t.Fatalf("self-rescheduling chain fired %d times over 5 ticks, want 5", fires)
	}
}
