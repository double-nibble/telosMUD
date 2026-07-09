package directory

import (
	"context"
	"testing"
	"time"
)

// TestRebalanceDirectiveLifecycle pins the #42 slice-3 directive: issue → read → fenced refresh → fenced
// clear. The fences (refresh/clear only when the directive still points at the given target) stop a
// completing/in-flight drain from disturbing a directive the coordinator has since re-pointed.
func TestRebalanceDirectiveLifecycle(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if _, found, err := d.ReadRebalance(ctx, "midgaard"); err != nil || found {
		t.Fatalf("ReadRebalance on empty = found %v, %v; want not found", found, err)
	}
	if err := d.IssueRebalance(ctx, "midgaard", "shard-b", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	to, found, err := d.ReadRebalance(ctx, "midgaard")
	if err != nil || !found || to != "shard-b" {
		t.Fatalf("ReadRebalance = (%q, %v, %v); want (shard-b, true, nil)", to, found, err)
	}

	// Re-issue to a DIFFERENT target is last-write-wins.
	if err := d.IssueRebalance(ctx, "midgaard", "shard-c", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	to, _, _ = d.ReadRebalance(ctx, "midgaard")
	if to != "shard-c" {
		t.Fatalf("after re-issue, target = %q; want shard-c", to)
	}

	// A fenced clear for the WRONG (stale) target is a no-op.
	if err := d.ClearRebalance(ctx, "midgaard", "shard-b"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := d.ReadRebalance(ctx, "midgaard"); !found {
		t.Fatal("a stale-target clear wrongly removed the directive")
	}
	// A fenced clear for the CURRENT target removes it.
	if err := d.ClearRebalance(ctx, "midgaard", "shard-c"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := d.ReadRebalance(ctx, "midgaard"); found {
		t.Fatal("the directive should be cleared for the current target")
	}
}

// TestRebalanceCooldown pins the anti-ping-pong cooldown.
func TestRebalanceCooldown(t *testing.T) {
	d := newTestRedis(t)
	ctx := context.Background()

	if on, err := d.OnCooldown(ctx, "midgaard"); err != nil || on {
		t.Fatalf("OnCooldown on empty = %v, %v; want false", on, err)
	}
	if err := d.SetCooldown(ctx, "midgaard", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if on, err := d.OnCooldown(ctx, "midgaard"); err != nil || !on {
		t.Fatalf("OnCooldown after set = %v, %v; want true", on, err)
	}
	// A different zone is independent.
	if on, _ := d.OnCooldown(ctx, "darkwood"); on {
		t.Fatal("darkwood should not be on cooldown")
	}
}
