package directory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// evictiongate_test.go — #429: the boot gate whose severity tracks whether the coordination Redis is
// dedicated.
//
// The property under test is a CONDITIONAL refusal, so both halves matter equally and for opposite reasons.
// Refusing on a shared instance would order operators into an OOM-kill that wipes the whole directory —
// strictly worse than the eviction it prevents. NOT refusing on a dedicated instance leaves #340 exactly
// where it was: a loud log line nobody reads.

func TestEvictionGate(t *testing.T) {
	unsafe := EvictionFinding{Policy: "allkeys-lru", MaxMemory: 128 << 20, Unsafe: true}
	latent := EvictionFinding{Policy: "allkeys-lru", MaxMemory: 0} // evicting policy, no ceiling: nothing evicts
	safe := EvictionFinding{Policy: "noeviction", MaxMemory: 128 << 20}
	unknown := EvictionFinding{Unknown: true}

	cases := []struct {
		name      string
		finding   EvictionFinding
		dedicated bool
		wantFatal bool
		why       string
	}{
		{
			name:    "an evicting policy on a DEDICATED coordination redis refuses the boot",
			finding: unsafe, dedicated: true, wantFatal: true,
			why: "this is the whole point of #429. Nothing on a dedicated coordination instance wants " +
				"eviction, so there is no tradeoff left and no worse configuration to be ordered into",
		},
		{
			name:    "the same policy on a SHARED redis only warns",
			finding: unsafe, dedicated: false, wantFatal: false,
			why: "the shared instance also carries the checkpoint tier, where an evicting policy may be the " +
				"right call for the Redis the operator actually has. Refusing here orders them into " +
				"noeviction on a cache-sized instance, which OOM-kills and wipes the directory at once — " +
				"the very failure the check exists to prevent",
		},
		{
			name:    "an evicting policy with NO ceiling never refuses",
			finding: latent, dedicated: true, wantFatal: false,
			why: "with maxmemory 0 nothing is ever evicted, so this is a latent config smell rather than an " +
				"active hazard. Refusing on it would cry wolf on the most common shape of the problem",
		},
		{
			name:    "noeviction is fine on either shape",
			finding: safe, dedicated: true, wantFatal: false,
		},
		{
			name:    "an UNREADABLE config never refuses",
			finding: unknown, dedicated: true, wantFatal: false,
			why: "CONFIG GET is disabled on most managed Redis (ElastiCache, Memorystore, Upstash). " +
				"Refusing on an unreadable config would make the engine unbootable on those platforms " +
				"entirely, which is a far bigger failure than the risk it guards",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fatal := EvictionGate(tc.finding, tc.dedicated)
			if tc.wantFatal && fatal == nil {
				t.Fatalf("the boot was ALLOWED. %s", tc.why)
			}
			if !tc.wantFatal && fatal != nil {
				t.Fatalf("the boot was REFUSED (%v). %s", fatal, tc.why)
			}
			if tc.wantFatal {
				// The operator has to be able to act on it, and the remedy has a second half that is easy to
				// miss — noeviction WITHOUT a ceiling is how the directory gets wiped by an OOM-kill.
				for _, want := range []string{"noeviction", "maxmemory"} {
					if !strings.Contains(fatal.Error(), want) {
						t.Fatalf("the refusal does not mention %q, so it names a problem without its "+
							"remedy: %v", want, fatal)
					}
				}
			}
		})
	}
}

// TestEvictionGateIsTheOnlyDifferenceBetweenTheTwoShapes. The finding itself must not depend on whether the
// instance is dedicated — dedication changes what to DO about an unsafe policy, never whether one is unsafe.
// Folding the two together would make the shared shape stop reporting, which is where most deployments are.
func TestEvictionGateDoesNotChangeTheFinding(t *testing.T) {
	f := classifyEviction("allkeys-lru", 128<<20)
	if !f.Unsafe {
		t.Fatal("an evicting policy with a ceiling was not classified unsafe")
	}
	// Same finding, both shapes: only the gate's answer differs.
	if EvictionGate(f, true) == nil || EvictionGate(f, false) != nil {
		t.Fatal("the gate does not distinguish the two shapes")
	}
}

// TestWatchEvictionPolicyStopsWithItsContext. The watcher runs for the life of the process, so a leak is a
// goroutine per shard restart in a test binary and a wedged shutdown in production.
func TestWatchEvictionPolicyStopsWithItsContext(t *testing.T) {
	r := &Redis{} // never dialed: the ticker must not fire within the test's lifetime
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.WatchEvictionPolicy(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchEvictionPolicy did not return when its context was cancelled")
	}
}

// TestEvictionRecheckIsSlow guards the one way the periodic check could become a problem of its own: a tight
// interval adds a CONFIG round trip to the coordination Redis from every process in the fleet, forever, to
// watch for something that changes at human speed.
func TestEvictionRecheckIsSlow(t *testing.T) {
	if evictionRecheckInterval < time.Minute {
		t.Fatalf("evictionRecheckInterval is %v. This watches for an OPERATOR ACTION (a CONFIG SET in "+
			"response to a memory alert), so minutes of detection lag cost nothing and a tight loop bills "+
			"every process in the fleet for it", evictionRecheckInterval)
	}
}
