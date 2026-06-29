package world

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

// soak_test.go is the W9 stress/soak tier: a high-VOLUME churn + a concurrent command burst against the
// single-writer zone, asserting it never wedges, never panics, and does not LEAK residents or goroutines
// over thousands of cycles — the accumulation/lifecycle bugs a single-pass test cannot see. It is GATED
// on TELOS_SOAK (per-commit CI skips it; the nightly tier sets it) and the cycle count is tunable via
// TELOS_SOAK_CYCLES so the nightly run can crank the volume.

func soakEnvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// boundedProbe posts a presence probe and reports whether the zone ANSWERED within d. A wedged zone
// never answers — this is the soak's liveness signal. (zoneProbe blocks forever, which would hang the
// soak on a wedge instead of failing it.)
func boundedProbe(z *Zone, name string, d time.Duration) (present, answered bool) {
	reply := make(chan presence, 1)
	z.post(presenceMsg{id: name, reply: reply})
	select {
	case p := <-reply:
		return p.present, true
	case <-time.After(d):
		return false, false
	}
}

// waitGone blocks until `name` has left the zone (or fails fast on a wedge / a stuck resident).
func soakWaitGone(t *testing.T, z *Zone, name string, where string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		present, answered := boundedProbe(z, name, 2*time.Second)
		if !answered {
			t.Fatalf("zone wedged (%s): no probe answer for %s", where, name)
		}
		if !present {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not leave within deadline (%s)", name, where)
		}
	}
}

func TestSoakChurnAndConcurrentLoad(t *testing.T) {
	if os.Getenv("TELOS_SOAK") == "" {
		t.Skip("TELOS_SOAK not set; skipping the W9 soak/stress tier (runs in the nightly job)")
	}
	cycles := soakEnvInt("TELOS_SOAK_CYCLES", 2000)

	sh := NewDemoShard()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sh.Run(ctx)
	z := sh.Zone()

	// Warm up + baseline the goroutine count (after the zone goroutine and its helpers exist).
	if _, answered := boundedProbe(z, "warmup", 2*time.Second); !answered {
		t.Fatal("zone never became responsive at startup")
	}
	runtime.GC()
	base := runtime.NumGoroutine()

	// PHASE 1 — CHURN: many serial join → commands → leave cycles. Entity creation is serialized against
	// the zone goroutine by waiting for each leave to settle (the zone is idle-waiting on its inbox
	// between cycles, so newPlayerEntity never races newEntity). Movement is north→south only, which
	// returns to the temple (within midgaard), so every leave is clean (no cross-zone wanderer to leak).
	for i := 0; i < cycles; i++ {
		name := "Soak" + strconv.Itoa(i)
		s := newTestPlayerEntity(z, name)
		z.post(joinMsg{s: s})
		for _, line := range []string{"look", "north", "south", "say churn", "inventory"} {
			z.post(inputMsg{id: name, line: line})
		}
		z.post(leaveMsg{id: name})
		soakWaitGone(t, z, name, "churn cycle "+strconv.Itoa(i))
		if i%250 == 0 {
			if _, answered := boundedProbe(z, "probe", time.Second); !answered {
				t.Fatalf("zone wedged during churn at cycle %d", i)
			}
		}
	}

	// PHASE 2 — CONCURRENT LOAD: residents joined serially, then workers hammer STATIONARY commands
	// concurrently. Posting is concurrent-safe (the inbox channel), and the single-writer zone serializes
	// execution — this stresses the inbox under many concurrent producers. No movement, so residents stay
	// in the temple for a clean teardown.
	const residents = 16
	names := make([]string, residents)
	for i := range names {
		names[i] = "Load" + strconv.Itoa(i)
		s := newTestPlayerEntity(z, names[i])
		z.post(joinMsg{s: s})
		if present, answered := boundedProbe(z, names[i], 2*time.Second); !answered || !present {
			t.Fatalf("resident %s did not join", names[i])
		}
	}
	const workers, perWorker = 8, 1000
	var wg sync.WaitGroup
	lines := []string{"look", "say load", "inventory", "who", "equipment"}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for k := 0; k < perWorker; k++ {
				z.post(inputMsg{id: names[(seed+k)%residents], line: lines[(seed+k)%len(lines)]})
			}
		}(w)
	}
	wg.Wait()
	if _, answered := boundedProbe(z, "afterload", 3*time.Second); !answered {
		t.Fatal("zone wedged after the concurrent command burst")
	}

	// Drain the residents; all must leave cleanly.
	for _, n := range names {
		z.post(leaveMsg{id: n})
	}
	for _, n := range names {
		soakWaitGone(t, z, n, "resident drain")
	}

	// LEAK CHECK: after the churn + the burst, the goroutine count must not have grown unboundedly. A
	// per-cycle goroutine leak over `cycles` iterations would dwarf any tolerance; scheduler noise stays
	// within it.
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	if got := runtime.NumGoroutine(); got > base+30 {
		t.Fatalf("goroutine leak suspected: %d goroutines after the soak, baseline %d (tolerance +30)", got, base)
	}
}
