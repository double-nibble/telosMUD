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

// TestGracefulDrainZeroDrop is the Phase-16.4 capstone: K players live on shard A, A drains for a rolling
// redeploy, and every player MIGRATES to the standby B hosting the same zone with their socket held open
// across the Redirect — zero dropped connections. Asserts each player receives a Redirect to B, re-binds on
// B (their handoff completes), and the DrainResult reports all Redirected / none Reclaimed.
func TestGracefulDrainZeroDrop(t *testing.T) {
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
		WithZoneLeasing(dir, "shard-b", time.Second, 80*time.Millisecond, func() {})
	aPlay := serveShard(t, shardA, lisA)
	bPlay := serveShard(t, shardB, lisB)

	// Connect K players as fresh logins to A; each lands in midgaard.
	const K = 4
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	names := make([]string, K)
	streamsA := make([]playv1.Play_ConnectClient, K)
	for i := 0; i < K; i++ {
		names[i] = fmt.Sprintf("Drainee%d", i)
		sA, err := aPlay.Connect(sctx)
		if err != nil {
			t.Fatal(err)
		}
		send(t, sA, attach(names[i]))
		recvAttached(t, sA)
		streamsA[i] = sA
	}
	// Wait until all K are resident on A's midgaard.
	waitCond(t, "all players resident on A", func() bool { return shardA.ZoneByID("midgaard").pop.Load() == K })

	// Drain A -> B. Runs in a goroutine (it blocks until every zone empties); the players' streams below
	// consume the Redirects concurrently, which is what lets the zone empty.
	type drainOut struct {
		res DrainResult
		err error
	}
	done := make(chan drainOut, 1)
	go func() {
		res, err := shardA.BeginDrain(ctx, func(string, int) (string, string, error) {
			return "shard-b", "addr-b", nil
		}, 10*time.Second)
		done <- drainOut{res, err}
	}()

	// Every player receives a Redirect to B and re-binds there (their socket never dropped).
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
		recvUntilOutput(t, sB, "Exits:") // a room render on B: the player is live on the new shard
	}

	out := <-done
	if out.err != nil {
		t.Fatalf("BeginDrain: %v", out.err)
	}
	if out.res.Redirected != K || out.res.Reclaimed != 0 {
		t.Fatalf("drain result = %+v, want {Redirected:%d Reclaimed:0} (zero dropped connections)", out.res, K)
	}

	// The zone + its ownership moved to B; A's copy is empty.
	if owner, _ := dir.ShardForZone(ctx, "midgaard"); owner != "shard-b" {
		t.Fatalf("midgaard owner after drain = %q, want shard-b", owner)
	}
	if shardB.ZoneByID("midgaard") == nil {
		t.Fatal("B does not host midgaard after the drain")
	}
	if pop := shardA.ZoneByID("midgaard").pop.Load(); pop != 0 {
		t.Fatalf("A's midgaard still has %d players after drain", pop)
	}
}

// TestDrainingShardRefusesFreshLogin: while draining, A refuses a NEW fresh login (so it doesn't become a
// straggler) — the gate re-resolves to the peer via the directory. An in-flight handoff bind is unaffected
// (covered by the zero-drop test above).
func TestDrainingShardRefusesFreshLogin(t *testing.T) {
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
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a"))

	lis := bufconn.Listen(1 << 20)
	peers := func(string) (handoffv1.HandoffClient, error) { return nil, fmt.Errorf("no peers") }
	shardA := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "addr-a", dir, peers).
		WithZoneLeasing(dir, "shard-a", time.Second, 80*time.Millisecond, func() {})
	aPlay := serveShard(t, shardA, lis)

	// Flip the draining flag directly (no peer to hand off to in this focused test).
	shardA.mu.Lock()
	shardA.draining = true
	shardA.mu.Unlock()

	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sA, err := aPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sA, attach("Latecomer"))
	// The fresh login is refused: the stream ends with an error rather than an Attached.
	if _, err := sA.Recv(); err == nil {
		t.Fatal("a draining shard accepted a fresh login; want it refused")
	}
}
