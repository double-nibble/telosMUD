package world

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// saverbarrier_test.go pins #282: a graceful shutdown waits for the saver's queue to drain before it
// cancels the saver's context.
//
// The saver's drainer returns on ctx cancel WITHOUT emptying its buffer. On a graceful drain the reclaimed
// stragglers' flush is enqueued LAST, microseconds before shutdown cancels that context — so the one cohort
// whose only durability path is that flush was exactly the cohort most likely to lose it. (Redirected
// players are unaffected: their state crosses in the handoff snapshot, not through the saver.)

// countingStore records how many save calls reached the store AND how many actually applied their CAS, so a
// test can assert DURABILITY (the row landed) rather than merely dequeue. It can be made slow so the barrier
// has something to wait for.
type countingStore struct {
	*MemStore
	saved   atomic.Int64 // calls that reached the store
	applied atomic.Int64 // calls whose CAS actually persisted
	delay   time.Duration
	started chan struct{}
	once    sync.Once
}

func newCountingStore(delay time.Duration) *countingStore {
	return &countingStore{MemStore: NewMemStore(), delay: delay, started: make(chan struct{})}
}

func (c *countingStore) SaveCharacter(ctx context.Context, snap CharSnapshot) (SaveResult, error) {
	c.once.Do(func() { close(c.started) })
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-ctx.Done():
			return SaveResult{}, ctx.Err()
		}
	}
	c.saved.Add(1)
	res, err := c.MemStore.SaveCharacter(ctx, snap)
	if err == nil && res.Outcome == SaveApplied {
		c.applied.Add(1)
	}
	return res, err
}

// runDrainShard starts the shard and returns a stop func that blocks until Run returns.
func runDrainShard(t *testing.T, sh *Shard) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sh.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sh.mu.Lock()
		ready := sh.runCtx != nil
		sh.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("shard never became ready")
		}
		time.Sleep(time.Millisecond)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("shard did not stop")
			}
		})
	}
}

// TestSaverFlushWaitsForQueuedWrites is the core barrier contract, and it asserts DURABILITY, not dequeue.
//
// An earlier version queued snapshots for characters that had no row, so every CAS missed, nothing was ever
// persisted, and the test passed anyway on a call count. The property #282 is about is "the queued writes
// LANDED before flush returned", so the rows are created first and the store's contents are read back.
// (persistence review.)
func TestSaverFlushWaitsForQueuedWrites(t *testing.T) {
	store := newCountingStore(30 * time.Millisecond)
	sv := newSaver(store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sv.run(ctx)

	// A live-player flush bounces a CAS miss back to its zone, so give each request one; nothing drains its
	// inbox, and the 256-slot buffer absorbs any conflicts.
	z := newZone("t")
	const n = 5
	names := make([]string, n)
	for i := range n {
		name := string(rune('A' + i))
		names[i] = name
		pid, err := store.CreateCharacter(context.Background(), name, "midgaard", "midgaard:room:temple")
		if err != nil {
			t.Fatal(err)
		}
		snap := CharSnapshot{PID: pid, Name: name, ZoneRef: "midgaard", RoomRef: "midgaard:room:temple", StateVersion: 0}
		sv.enqueue(saveRequest{id: name, zone: z, reason: saveFlush, snap: snap})
	}

	fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer fcancel()
	if err := sv.flush(fctx, nil); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if got := store.applied.Load(); got != n {
		t.Fatalf("flush returned with %d of %d queued saves PERSISTED — the barrier does not actually drain "+
			"the queue, or the writes are not landing (#282)", got, n)
	}
	// And read the rows back: a version bump is the durable evidence.
	for _, name := range names {
		snap, found, err := store.LoadCharacter(context.Background(), name)
		if err != nil || !found {
			t.Fatalf("row for %q missing after the barrier: found=%v err=%v", name, found, err)
		}
		if snap.StateVersion == 0 {
			t.Fatalf("row for %q was never CAS'd (version still 0) — flush returned before it was durable", name)
		}
	}
}

// TestDrainEnqueueBlocksRatherThanDroppingOnAFullQueue is the hole both reviewers found: the ordinary
// enqueue DROPS on a full queue, which is right for the ~60s cadence (the next tick re-enqueues) and
// catastrophic at drain, where there is no next tick and this flush is the straggler cohort's only
// durability path. A barrier in front of a lossy queue reports success while saves vanish.
//
// The drain path therefore uses a blocking, ctx-bounded enqueue. Push far more than saveQueueDepth through
// it against a slow drainer and nothing may be lost.
func TestDrainEnqueueBlocksRatherThanDroppingOnAFullQueue(t *testing.T) {
	store := newCountingStore(0)
	sv := newSaver(store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const n = saveQueueDepth * 2 // comfortably past the buffer
	z := newZone("t")
	enqueued := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		ectx, ecancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ecancel()
		for i := range n {
			if sv.enqueueCtx(ectx, saveRequest{id: strconv.Itoa(i), zone: z, reason: saveCheckpoint, snap: CharSnapshot{Name: strconv.Itoa(i)}}) {
				enqueued++
			}
		}
	}()

	// The drainer starts only after the producer is already blocked on a full queue.
	time.Sleep(50 * time.Millisecond)
	go sv.run(ctx)

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the blocking drain enqueue never completed")
	}
	if enqueued != n {
		t.Fatalf("only %d of %d drain saves reached the queue — the drain path must not drop on a full "+
			"queue, or the barrier's success is a lie (#282)", enqueued, n)
	}
}

// TestDrainEnqueueGivesUpWhenItsDeadlineExpires: blocking must still be bounded. A zone blocked forever on a
// full saver queue, while the drainer is blocked posting a conflict into that zone's full inbox, is a
// deadlock; the drain deadline is what breaks it. The saves that could not get in are REPORTED, not hidden.
func TestDrainEnqueueGivesUpWhenItsDeadlineExpires(t *testing.T) {
	sv := newSaver(newCountingStore(0), nil) // no drainer running: the queue never empties
	z := newZone("t")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	ok := true
	start := time.Now()
	for i := range saveQueueDepth + 5 {
		if !sv.enqueueCtx(ctx, saveRequest{id: strconv.Itoa(i), zone: z, reason: saveCheckpoint}) {
			ok = false
			break
		}
	}
	if ok {
		t.Fatal("enqueueCtx never reported a failure against a queue that can never drain")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("enqueueCtx took %v to give up; the drain deadline must bound it", elapsed)
	}
}

// TestSaverFlushIsANoOpWhenDisabled: a storeless (ephemeral) shard never queues anything, so the barrier
// must return immediately rather than blocking on a drainer that does not exist.
func TestSaverFlushIsANoOpWhenDisabled(t *testing.T) {
	sv := newSaver(nil, nil)
	done := make(chan error, 1)
	go func() { done <- sv.flush(context.Background(), nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a disabled saver's flush must succeed immediately, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("flush blocked on a disabled saver (no drainer runs, so it would hang shutdown forever)")
	}
}

// TestSaverFlushRespectsItsDeadline: a wedged store must delay shutdown, never hang it. The barrier returns
// its context's error and the caller proceeds.
func TestSaverFlushRespectsItsDeadline(t *testing.T) {
	store := newCountingStore(time.Hour) // effectively wedged
	sv := newSaver(store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sv.run(ctx)

	sv.enqueue(saveRequest{id: "Stuck", zone: newZone("t"), reason: saveFlush, snap: CharSnapshot{Name: "Stuck"}})
	<-store.started // the wedged write is in progress

	fctx, fcancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer fcancel()
	start := time.Now()
	if err := sv.flush(fctx, nil); err == nil {
		t.Fatal("flush must report its deadline was exceeded when the store is wedged")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("flush took %v to give up; a wedged store must not hang shutdown", elapsed)
	}
}

// TestSaverBarrierSentinelWritesNothing: the sentinel is not a save. It must not reach the store, or a
// graceful shutdown would write a zero-valued character row.
func TestSaverBarrierSentinelWritesNothing(t *testing.T) {
	store := newCountingStore(0)
	sv := newSaver(store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sv.run(ctx)

	fctx, fcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer fcancel()
	if err := sv.flush(fctx, nil); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := store.saved.Load(); got != 0 {
		t.Fatalf("the barrier sentinel reached the store %d times; it must never be treated as a save", got)
	}
	if _, found, _ := store.LoadCharacter(context.Background(), ""); found {
		t.Fatal("the barrier sentinel persisted an empty character")
	}
}

// TestDrainWaitsForZonesToEnqueue is the other half of the barrier, and the reason Drain now takes a
// context. Posting drainFlushMsg proves nothing: if the saver barrier is taken before the zone has
// PROCESSED that message, it drains an empty queue and reports success while the zone's saves are still
// sitting unposted in its inbox.
func TestDrainWaitsForZonesToEnqueue(t *testing.T) {
	store := newCountingStore(0)
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	sh := NewDemoShard().WithPersistence(store, nil)
	sh.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	stop := runDrainShard(t, sh)
	defer stop()

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := playv1.NewPlayClient(cc).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, stream, attach("Resident"))
	recvAttached(t, stream)

	before := store.saved.Load()
	if dropped := sh.Drain(context.Background()); dropped != 0 {
		t.Fatalf("Drain reported %d dropped saves on an idle queue", dropped)
	}

	// Drain returned, so every zone has DUMPED its residents onto the saver queue. The barrier now proves
	// those writes actually landed. Without Drain's wait, this flush could drain an empty queue and report
	// success while the zone's saves were still sitting unposted in its inbox.
	fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer fcancel()
	if err := sh.FlushSaver(fctx); err != nil {
		t.Fatalf("FlushSaver: %v", err)
	}
	if got := store.saved.Load(); got <= before {
		t.Fatal("no save reached the store after drain + barrier — the resident's flush was never durable (#282)")
	}
}

// TestDrainHonoursItsContext: a zone whose loop has stopped consuming must not wedge shutdown.
func TestDrainHonoursItsContext(t *testing.T) {
	store := newCountingStore(0)
	sh := NewDemoShard().WithPersistence(store, nil)
	// Deliberately NOT running the shard: no zone loop drains the inbox.

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { sh.Drain(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Drain blocked on a zone that is not consuming its inbox — a wedged zone must not hang shutdown")
	}
}

// TestSaverFlushReturnsAtOnceWhenTheDrainerIsGone: a lease fence can cancel the world context mid-drain,
// killing the drainer. Without watching for that, the barrier's blocking send would stall for the caller's
// whole timeout on a shutdown path with nothing left to flush.
func TestSaverFlushReturnsAtOnceWhenTheDrainerIsGone(t *testing.T) {
	sv := newSaver(newCountingStore(0), nil)
	ctx, cancel := context.WithCancel(context.Background())
	go sv.run(ctx)
	cancel() // the drainer exits
	time.Sleep(50 * time.Millisecond)

	dead := make(chan struct{})
	close(dead)

	fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer fcancel()
	start := time.Now()
	if err := sv.flush(fctx, dead); err == nil {
		t.Fatal("flush must report that the drainer is gone, not silently succeed")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("flush waited %v for a drainer that had already exited", elapsed)
	}
}
