package world

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/directory"
)

// placementbarrier_test.go pins #331: a graceful shutdown drains the placement writer's queue through a
// barrier BEFORE stopWorld cancels the world context.
//
// The placement writer is a background goroutine SEPARATE from the saver, and its drain loop returns on ctx
// cancel WITHOUT emptying its pending map. A player who quits during a graceful drain enqueues a clean-logout
// tombstone on it. Without the barrier, stopWorld cancels the context first and that tombstone is thrown
// away — the placement record keeps naming a shard that is exiting, and the tell/mail existence oracle
// reports the player as hosted on a dead shard until their next login. FlushPlacement is the barrier that
// closes this window.

// slowClearDir is a directory whose ClearPlayerShard blocks for `delay`, so a test can prove FlushPlacement
// actually WAITS for the write rather than merely enqueueing it. It embeds a real (miniredis) directory so
// every other Locator method behaves normally.
type slowClearDir struct {
	*directory.Redis
	delay time.Duration
}

func (d *slowClearDir) ClearPlayerShard(ctx context.Context, playerID, shardID, zoneID string, epoch uint64) (bool, error) {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return d.Redis.ClearPlayerShard(ctx, playerID, shardID, zoneID, epoch)
}

// barrierShard builds a minimal shard wired to `dir` with a running placement writer, and a run context the
// FlushPlacement barrier watches. It returns the shard and a cancel that stops the writer.
func barrierShard(t *testing.T, dir Locator) (*Shard, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Shard{placement: newPlacementWriter(), dir: dir, shardID: "shard-a"}
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()
	go s.runPlacementWriter(ctx)
	t.Cleanup(cancel)
	return s, cancel
}

func newBarrierDir(t *testing.T, delay time.Duration) *slowClearDir {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &slowClearDir{Redis: directory.NewRedis(rdb, "test"), delay: delay}
}

// TestFlushPlacementDrainsAPendingTombstone is the #331 core: a clean-logout tombstone enqueued during a
// drain is WRITTEN by the time FlushPlacement returns — not left in the queue for stopWorld to discard.
// The directory delays the clear, so a barrier that merely enqueued (or didn't wait) would return before the
// tombstone landed and the assertion below would fail.
func TestFlushPlacementDrainsAPendingTombstone(t *testing.T) {
	dir := newBarrierDir(t, 150*time.Millisecond)
	s, _ := barrierShard(t, dir)
	ctx := context.Background()

	// A live placement to tombstone: (shard-a, midgaard, epoch 5).
	if _, err := dir.RegisterPlacement(ctx, "Ghost", "shard-a", "midgaard", 5); err != nil {
		t.Fatal(err)
	}
	s.placement.offer(placementOp{playerID: "Ghost", zoneID: "midgaard", epoch: 5, clear: true})

	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.FlushPlacement(fctx); err != nil {
		t.Fatalf("FlushPlacement: %v", err)
	}

	// The barrier returned, so the tombstone MUST already be durable — read it synchronously, no polling.
	p, err := dir.PlayerPlacement(ctx, "Ghost")
	if err != nil {
		t.Fatalf("PlayerPlacement: %v", err)
	}
	if p.ShardID != "" {
		t.Fatalf("shard field = %q, want \"\" — FlushPlacement returned before the tombstone was written (#331)", p.ShardID)
	}
	if p.ZoneID != "midgaard" || p.Epoch != 5 {
		t.Fatalf("tombstone dropped the routing keys: %+v — epoch/zone must survive", p)
	}
}

// TestFlushPlacementIsANoOpWhenEmpty: with nothing pending, the barrier still completes promptly (it must
// not hang waiting for work that will never come).
func TestFlushPlacementIsANoOpWhenEmpty(t *testing.T) {
	dir := newBarrierDir(t, 0)
	s, _ := barrierShard(t, dir)

	fctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.FlushPlacement(fctx); err != nil {
		t.Fatalf("FlushPlacement on an empty queue: %v", err)
	}
}

// TestFlushPlacementReturnsWhenWriterAlreadyStopped pins the dead-writer guard (mirrors FlushSaver): a lease
// fence cancels the run context and stops the writer, so the barrier can never be honored. FlushPlacement
// must return promptly rather than block for its whole timeout on a shutdown path with nothing left to flush.
//
// The writer goroutine is deliberately NOT started and runCtx is pre-cancelled: with no writer alive nothing
// can EVER close the barrier, so the only way this returns is via the dead-channel watch. That removes the
// coin-flip a running-but-exiting writer would introduce (it could still drain an empty queue and close the
// barrier), matching the saver's equivalent TestSaverFlushReturnsAtOnceWhenTheDrainerIsGone.
func TestFlushPlacementReturnsWhenWriterAlreadyStopped(t *testing.T) {
	dir := newBarrierDir(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // lease fence: run context already dead, writer never started
	s := &Shard{placement: newPlacementWriter(), dir: dir, shardID: "shard-a"}
	s.mu.Lock()
	s.runCtx = ctx
	s.mu.Unlock()

	fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer fcancel()
	start := time.Now()
	err := s.FlushPlacement(fctx)
	if err == nil {
		t.Fatal("FlushPlacement must report the writer stopped, not succeed silently")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("FlushPlacement blocked for %v — it must watch the run context and return promptly", time.Since(start))
	}
}

// TestFlushPlacementReportsAHardCancelMidWrite pins the graceful-barrier-races-a-lease-fence-mid-drain seam
// (placement.go: the ctx.Err() guard inside the op loop returns WITHOUT closing collected barriers, on the
// contract that FlushPlacement's dead-channel watch unblocks the caller instead). A barrier collected
// ALONGSIDE ops, with the run context cancelled mid-write, must surface as the dead error, not a hang and
// not a false success.
func TestFlushPlacementReportsAHardCancelMidWrite(t *testing.T) {
	dir := newBarrierDir(t, 300*time.Millisecond) // slow enough that we can cancel mid-write
	s, cancel := barrierShard(t, dir)
	ctx := context.Background()
	if _, err := dir.RegisterPlacement(ctx, "Casualty", "shard-a", "keep", 3); err != nil {
		t.Fatal(err)
	}

	// Offer an op and a barrier together, let the writer pick them up and enter the slow write, then fence.
	s.placement.offer(placementOp{playerID: "Casualty", zoneID: "keep", epoch: 3, clear: true})
	barrier := s.placement.addBarrier()

	errc := make(chan error, 1)
	go func() {
		fctx, fcancel := context.WithTimeout(ctx, 5*time.Second)
		defer fcancel()
		errc <- s.FlushPlacement(fctx)
	}()

	time.Sleep(50 * time.Millisecond) // writer is now inside the 300ms clear
	cancel()                          // lease fence mid-write

	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("a hard cancel mid-write must surface as an error, not a false success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FlushPlacement hung on a hard cancel — the dead-channel watch did not fire")
	}
	// The barrier must NOT have been closed on the hard-cancel path.
	select {
	case <-barrier:
		t.Fatal("the barrier was closed on a hard cancel — the writer must not honor a partial flush")
	default:
	}
}

// TestFlushPlacementIsSkippedWithoutADirectory: a single-shard/dev world has no placement writer to drain,
// so the barrier is a no-op that never blocks.
func TestFlushPlacementIsSkippedWithoutADirectory(t *testing.T) {
	s := &Shard{placement: newPlacementWriter()} // dir == nil
	if err := s.FlushPlacement(context.Background()); err != nil {
		t.Fatalf("FlushPlacement with no directory: %v", err)
	}
}

// TestFlushPlacementDrainsARealQuitTombstone is the end-to-end proof #331 actually asked for: a player who
// QUITs during shutdown enqueues their tombstone through the REAL detach -> clearPlacement -> offer path (not
// a hand-crafted offer), and FlushPlacement drains it before the world context is cancelled. The directory
// delays the clear, so a barrier that returned early would see the shard field still set.
func TestFlushPlacementDrainsARealQuitTombstone(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := &slowClearDir{Redis: directory.NewRedis(rdb, "test"), delay: 150 * time.Millisecond}

	ctx := context.Background()
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a"))
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-a"))

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "addr-a", dir, nil).
		WithPersistence(NewMemStore(), nil).
		WithZoneLeasing(dir, "shard-a", directory.DefaultZoneLease, 0, nil)
	shard.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	zctx, zcancel := context.WithCancel(context.Background())
	t.Cleanup(zcancel)
	go shard.Run(zctx)

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	client := playv1.NewPlayClient(cc)

	cctx, cdrop := context.WithCancel(ctx)
	s, err := client.Connect(cctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Departing"))
	recvAttached(t, s)
	waitPlacement(t, dir.Redis, "Departing", func(p directory.Placement) bool { return p.ShardID == "shard-a" },
		"login did not register a placement")

	send(t, s, inputSeq(1, "quit"))
	recvOutputContaining(t, s, "Farewell")
	cdrop()

	// The real detach -> clearPlacement -> offer runs on the zone goroutine, asynchronously to the Farewell we
	// just saw. Gate on the offer having happened (pending, or already written by the slow-but-eventual writer)
	// so the barrier we take next is guaranteed to COVER it — otherwise FlushPlacement could drain an empty
	// queue before the clear is even enqueued. Once offered, FlushPlacement drains it no matter the writer's
	// timing (collected with it if pending, or after the in-flight write via the doorbell).
	offeredOrWritten := func() bool {
		shard.placement.mu.Lock()
		_, pending := shard.placement.pending["Departing"]
		shard.placement.mu.Unlock()
		if pending {
			return true
		}
		p, err := dir.PlayerPlacement(ctx, "Departing")
		return err == nil && p.ShardID == ""
	}
	deadline := time.Now().Add(5 * time.Second)
	for !offeredOrWritten() {
		if time.Now().After(deadline) {
			t.Fatal("the quit never enqueued a placement tombstone through the real detach path")
		}
		time.Sleep(5 * time.Millisecond)
	}

	fctx, fcancel := context.WithTimeout(ctx, 5*time.Second)
	defer fcancel()
	if err := shard.FlushPlacement(fctx); err != nil {
		t.Fatalf("FlushPlacement: %v", err)
	}
	p, err := dir.PlayerPlacement(ctx, "Departing")
	if err != nil {
		t.Fatalf("PlayerPlacement: %v", err)
	}
	if p.ShardID != "" {
		t.Fatalf("shard field = %q, want \"\" — the real quit tombstone was not drained by FlushPlacement (#331)", p.ShardID)
	}
}

// TestPlacementBarrierOrdersAfterPendingOps is the writer-level ordering guarantee the barrier rests on:
// every op collected alongside a barrier is written before the barrier closes. Proven by a slow clear —
// the barrier channel must not be closed until after the (delayed) write completes.
func TestPlacementBarrierOrdersAfterPendingOps(t *testing.T) {
	dir := newBarrierDir(t, 120*time.Millisecond)
	s, _ := barrierShard(t, dir)
	ctx := context.Background()
	if _, err := dir.RegisterPlacement(ctx, "Straggler", "shard-a", "crypt", 9); err != nil {
		t.Fatal(err)
	}

	s.placement.offer(placementOp{playerID: "Straggler", zoneID: "crypt", epoch: 9, clear: true})
	barrier := s.placement.addBarrier()

	select {
	case <-barrier:
		// It closed. The delayed write must therefore have completed already.
		if p, _ := dir.PlayerPlacement(ctx, "Straggler"); p.ShardID != "" {
			t.Fatalf("barrier closed before the op it covers was written: %+v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("barrier never closed")
	}
}
