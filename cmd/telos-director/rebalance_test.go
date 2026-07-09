package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/placement"
)

func testDir(t *testing.T) *directory.Redis {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return directory.NewRedis(rdb, "test")
}

// TestIssueRebalanceHappyPathAndSelfBlocks pins the #42 slice-3b gating: a fresh move is issued (directive +
// cooldown), and a second attempt is blocked because the zone is now on cooldown AND has an in-flight
// directive (the folding + these gates are what stop the stateless plan re-issuing every tick).
func TestIssueRebalanceHappyPathAndSelfBlocks(t *testing.T) {
	dir := testDir(t)
	ctx := context.Background()
	move := placement.Move{Zone: "midgaard", From: "shard-a", To: "shard-b"}

	if !issueRebalance(ctx, dir, move, nil) {
		t.Fatal("a fresh move should be issued")
	}
	if to, found, _ := dir.ReadRebalance(ctx, "midgaard"); !found || to != "shard-b" {
		t.Fatalf("directive = (%q, %v); want shard-b, true", to, found)
	}
	if on, _ := dir.OnCooldown(ctx, "midgaard"); !on {
		t.Fatal("cooldown should be set after an issue")
	}
	if issueRebalance(ctx, dir, move, nil) {
		t.Fatal("a zone on cooldown / with an in-flight directive must not be re-issued")
	}
}

// TestIssueRebalanceSkipsCooldown: a zone within its post-move cooldown is not re-issued.
func TestIssueRebalanceSkipsCooldown(t *testing.T) {
	dir := testDir(t)
	ctx := context.Background()
	if err := dir.SetCooldown(ctx, "midgaard", time.Minute); err != nil {
		t.Fatal(err)
	}
	if issueRebalance(ctx, dir, placement.Move{Zone: "midgaard", From: "a", To: "b"}, nil) {
		t.Fatal("a zone on cooldown must not be issued")
	}
	if _, found, _ := dir.ReadRebalance(ctx, "midgaard"); found {
		t.Fatal("no directive should be written for a cooled-down zone")
	}
}

// TestIssueRebalanceSkipsDrainingTarget: don't hand a zone to a shard that is itself draining (fleet rollout).
func TestIssueRebalanceSkipsDrainingTarget(t *testing.T) {
	dir := testDir(t)
	ctx := context.Background()
	draining := map[string]bool{"shard-b": true}
	if issueRebalance(ctx, dir, placement.Move{Zone: "midgaard", From: "a", To: "shard-b"}, draining) {
		t.Fatal("must not target a draining shard")
	}
	if _, found, _ := dir.ReadRebalance(ctx, "midgaard"); found {
		t.Fatal("no directive should be written when the target is draining")
	}
}
