package world

import (
	"context"
	"testing"
	"time"
)

// fakeRebalancePort records the directive ops maybeRebalance makes.
type fakeRebalancePort struct {
	to        string
	found     bool
	cleared   []string
	refreshed []string
}

func (f *fakeRebalancePort) ReadRebalance(_ context.Context, _ string) (string, bool, error) {
	return f.to, f.found, nil
}

func (f *fakeRebalancePort) RefreshRebalance(_ context.Context, zoneID, _ string, _ time.Duration) error {
	f.refreshed = append(f.refreshed, zoneID)
	return nil
}

func (f *fakeRebalancePort) ClearRebalance(_ context.Context, zoneID, _ string) error {
	f.cleared = append(f.cleared, zoneID)
	return nil
}

// TestMaybeRebalanceSelfTargetClears pins the critical #42 slice-3 guard: once a zone's ownership flips to
// B, B also renews the lease and re-reads the still-present directive — whose target is B ITSELF. It must
// NOT self-handover (which would abandon the live zone's lease → double-own); it clears its own now-
// satisfied directive and launches no drain.
func TestMaybeRebalanceSelfTargetClears(t *testing.T) {
	sh := NewDemoShard()
	sh.shardID = "shard-b" // white-box: we are now the OWNER named by the directive
	f := &fakeRebalancePort{to: "shard-b", found: true}
	sh.WithRebalance(f)

	sh.maybeRebalance(context.Background(), "midgaard")

	if len(f.cleared) != 1 || f.cleared[0] != "midgaard" {
		t.Fatalf("self-target directive not cleared: cleared=%v", f.cleared)
	}
	sh.mu.Lock()
	launched := sh.rebalancing["midgaard"]
	sh.mu.Unlock()
	if launched {
		t.Fatal("self-target must NOT launch a drain (would self-handover and abandon the live lease)")
	}
}

// TestMaybeRebalanceNoDirectiveNoop: no directive => a clean no-op (no clear, no launch).
func TestMaybeRebalanceNoDirectiveNoop(t *testing.T) {
	sh := NewDemoShard()
	sh.shardID = "shard-a"
	f := &fakeRebalancePort{found: false}
	sh.WithRebalance(f)

	sh.maybeRebalance(context.Background(), "midgaard")
	if len(f.cleared) != 0 || len(f.refreshed) != 0 {
		t.Fatalf("no directive should be a no-op: cleared=%v refreshed=%v", f.cleared, f.refreshed)
	}
}
