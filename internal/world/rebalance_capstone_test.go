package world

import (
	"context"
	"fmt"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"

	"github.com/alicebob/miniredis/v2"
	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/test/bufconn"
)

// TestRebalanceZoneMigratesWithoutShardDrain is the #42 slice-3 capstone: a coordinator rebalance moves ONE
// zone (midgaard) + its players from shard A to shard B — every player migrates with their socket held open
// (zero drop), ownership flips to B — WITHOUT putting A into a shard-wide drain (A keeps serving + accepting
// logins). That last property is what distinguishes RebalanceZone from BeginDrain.
func TestRebalanceZoneMigratesWithoutShardDrain(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterShard(ctx, "shard-b", "addr-b", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a"))

	lisA := bufconn.Listen(1 << 20)
	lisB := bufconn.Listen(1 << 20)
	lisByAddr := map[string]*bufconn.Listener{"addr-a": lisA, "addr-b": lisB}
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		lis := lisByAddr[addr]
		if lis == nil {
			return nil, fmt.Errorf("unknown shard %q", addr)
		}
		return handoffv1.NewHandoffClient(dialBuf(t, lis)), nil
	}

	shardA := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "addr-a", dir, peers).
		WithZoneLeasing(dir, "shard-a", time.Second, 80*time.Millisecond, func() {})
	shardB := NewShardFromContent(lc, nil, "", "addr-b", dir, peers).
		WithZoneLeasing(dir, "shard-b", time.Second, 80*time.Millisecond, func() {}).
		WithRebalance(dir) // production wires the port on BOTH shards; exercises B's post-flip re-read
	aPlay := serveShard(t, shardA, lisA)
	bPlay := serveShard(t, shardB, lisB)

	const K = 3
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	names := make([]string, K)
	streamsA := make([]playv1.Play_ConnectClient, K)
	for i := 0; i < K; i++ {
		names[i] = fmt.Sprintf("Rebal%d", i)
		sA, err := aPlay.Connect(sctx)
		if err != nil {
			t.Fatal(err)
		}
		send(t, sA, attach(names[i]))
		recvAttached(t, sA)
		streamsA[i] = sA
	}
	waitCond(t, "all players resident on A", func() bool { return shardA.ZoneByID("midgaard").pop.Load() == K })

	type out struct {
		res DrainResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := shardA.RebalanceZone(ctx, "midgaard", "shard-b", "addr-b", 10*time.Second)
		done <- out{res, err}
	}()

	for i := 0; i < K; i++ {
		redir := recvRedirect(t, streamsA[i])
		if redir.GetTargetShardAddr() != "addr-b" {
			t.Fatalf("player %s redirected to %q, want addr-b", names[i], redir.GetTargetShardAddr())
		}
		sB, err := bPlay.Connect(sctx)
		if err != nil {
			t.Fatal(err)
		}
		send(t, sB, attachWithToken(names[i], redir.GetHandoffToken()))
		recvAttached(t, sB)
		recvUntilOutput(t, sB, "Exits:")
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("RebalanceZone: %v", got.err)
	}
	if got.res.Redirected != K || got.res.Reclaimed != 0 {
		t.Fatalf("rebalance result = %+v, want {Redirected:%d Reclaimed:0}", got.res, K)
	}
	if owner, _ := dir.ShardForZone(ctx, "midgaard"); owner != "shard-b" {
		t.Fatalf("midgaard owner after rebalance = %q, want shard-b", owner)
	}
	if shardB.ZoneByID("midgaard") == nil {
		t.Fatal("B does not host midgaard after the rebalance")
	}
	if pop := shardA.ZoneByID("midgaard").pop.Load(); pop != 0 {
		t.Fatalf("A's midgaard still has %d players after the rebalance", pop)
	}
	// The crux: a single-zone rebalance must NOT flip the shard-wide draining flag — A keeps serving.
	if shardA.isDraining() {
		t.Fatal("RebalanceZone set the shard-wide draining flag; it must only move the one zone")
	}

	// Self-target regression: B now owns midgaard AND renews its lease. Plant a stale directive naming B
	// itself as the target (what A's completion would leave in a race). B's renewal tick must CLEAR it
	// (self-target guard) and keep renewing — never self-handover into a lease-abandoning double-own.
	if err := dir.IssueRebalance(ctx, "midgaard", "shard-b", 90*time.Second); err != nil {
		t.Fatal(err)
	}
	waitCond(t, "B clears its own stale rebalance directive", func() bool {
		_, found, _ := dir.ReadRebalance(ctx, "midgaard")
		return !found
	})
	// After several more renewal ticks B still owns + hosts midgaard (no self-drain lease abandonment).
	time.Sleep(300 * time.Millisecond)
	if owner, _ := dir.ShardForZone(ctx, "midgaard"); owner != "shard-b" {
		t.Fatalf("midgaard owner after B's self-target clear = %q, want shard-b (B abandoned its lease?)", owner)
	}
	if shardB.ZoneByID("midgaard") == nil {
		t.Fatal("B no longer hosts midgaard after processing its own directive")
	}
}
