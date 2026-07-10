package world

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// sendstall_test.go pins #274: the world reclaims a Play stream whose peer has stopped reading.
//
// gRPC server keepalive already reclaims a gate whose TRANSPORT died. It structurally cannot see a gate whose
// transport happily acks our PINGs — the HTTP/2 stack answers them independently of application flow control —
// while its APPLICATION has stopped draining the stream. The window exhausts, stream.Send blocks forever, and
// the writer goroutine, reader goroutine, session, entity ownership, and session-lock renewer all leak until
// the GATE's own write-deadline closes its side. That is exactly the dependency on gate correctness that
// keepalive was added to remove.

// --- the watchdog in isolation --------------------------------------------------------------------

func TestWatchSendStallTripsOnALongBlockedSend(t *testing.T) {
	const timeout, interval = 100 * time.Millisecond, 5 * time.Millisecond

	var started atomic.Pointer[time.Time]
	now := time.Now()
	started.Store(&now) // a Send has just begun and never returns

	stalled := make(chan struct{})
	blockedCh := make(chan time.Duration, 1)
	go watchSendStall(context.Background(), &started, stalled, timeout, interval,
		func(d time.Duration) { blockedCh <- d })

	select {
	case <-stalled:
	case <-time.After(3 * time.Second):
		t.Fatal("the watchdog never tripped on a Send blocked past the bound")
	}
	sawBlocked := <-blockedCh
	if sawBlocked < timeout {
		t.Fatalf("onStall reported %v blocked, want >= %v", sawBlocked, timeout)
	}
}

// TestWatchSendStallIgnoresAHealthyWriter: a writer that keeps completing Sends never trips it, no matter how
// many frames go by. `started` is 0 between Sends.
func TestWatchSendStallIgnoresAHealthyWriter(t *testing.T) {
	const timeout, interval = 50 * time.Millisecond, 2 * time.Millisecond

	var started atomic.Pointer[time.Time]
	stalled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchSendStall(ctx, &started, stalled, timeout, interval, nil)

	// Simulate a busy but healthy writer: each Send takes well under the bound.
	done := time.After(400 * time.Millisecond)
	for {
		select {
		case <-stalled:
			t.Fatal("the watchdog tripped on a writer that was completing its Sends")
		case <-done:
			return
		default:
			now := time.Now()
			started.Store(&now)
			time.Sleep(time.Millisecond)
			started.Store(nil)
		}
	}
}

// TestWatchSendStallIgnoresASlowButProgressingWriter: the bound is on ONE Send, not on cumulative time. A
// writer whose Sends each take almost the whole budget but keep returning must never be reclaimed.
func TestWatchSendStallIgnoresASlowButProgressingWriter(t *testing.T) {
	const timeout, interval = 100 * time.Millisecond, 5 * time.Millisecond

	var started atomic.Pointer[time.Time]
	stalled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watchSendStall(ctx, &started, stalled, timeout, interval, nil)

	for range 5 {
		now := time.Now()
		started.Store(&now)
		time.Sleep(70 * time.Millisecond) // slow, but under the bound
		started.Store(nil)
		select {
		case <-stalled:
			t.Fatal("the watchdog tripped on a slow writer that kept making progress; the bound is per-Send")
		default:
		}
	}
}

// TestWatchSendStallExitsWithTheStream: the stream ended for some other reason (EOF, error, shutdown). The
// watchdog must return rather than leak a timer per dead stream.
func TestWatchSendStallExitsWithTheStream(t *testing.T) {
	var started atomic.Pointer[time.Time]
	now := time.Now()
	started.Store(&now)
	stalled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { watchSendStall(ctx, &started, stalled, time.Hour, 2*time.Millisecond, nil); close(done) }()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the watchdog outlived its stream context")
	}
	select {
	case <-stalled:
		t.Fatal("the watchdog signalled a stall when the stream simply ended")
	default:
	}
}

// --- end to end: a peer that stops reading is reclaimed --------------------------------------------

// TestWorldReclaimsAStreamWhosePeerStoppedReading is the #274 regression, driven through a real gRPC stream.
//
// The client attaches and then never calls Recv again. The world keeps producing frames; the stream's
// flow-control window exhausts, stream.Send blocks, and without the watchdog nothing on the world side ever
// notices — the whole session leaks until the gate closes its side.
func TestWorldReclaimsAStreamWhosePeerStoppedReading(t *testing.T) {
	// Registered FIRST so Cleanup's LIFO order restores these LAST — after gs.Stop and the zone cancel below
	// have torn down every goroutine that reads them. Restoring while a watchdog still runs is a data race.
	old, oldI := streamSendStallTimeout, streamStallCheckInterval
	t.Cleanup(func() { streamSendStallTimeout, streamStallCheckInterval = old, oldI })
	streamSendStallTimeout, streamStallCheckInterval = 300*time.Millisecond, 20*time.Millisecond

	lis := bufconn.Listen(1 << 20)
	// Pin the HTTP/2 flow-control windows. Setting them explicitly disables grpc-go's BDP-based dynamic
	// window growth, which would otherwise expand to megabytes and let the server keep writing into a peer
	// that never reads — hiding the very stall this test exists to reproduce.
	gs := grpc.NewServer(grpc.InitialWindowSize(64*1024), grpc.InitialConnWindowSize(64*1024))
	sh := NewDemoShard()
	sh.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	zctx, zcancel := context.WithCancel(context.Background())
	t.Cleanup(zcancel)
	go sh.Run(zctx)

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(64*1024), grpc.WithInitialConnWindowSize(64*1024))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := playv1.NewPlayClient(cc).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, stream, attach("Deadbeat"))
	recvAttached(t, stream)

	// From here the client NEVER reads.
	//
	// Push far more bytes than the 64KB window can hold. `say` echoes the line back, so a 4KB line (the input
	// cap) yields a ~4KB frame; 64 of them already exhaust the window, and the session's 256-slot out buffer
	// keeps the writer supplied well past that. Small frames would not do: the buffer holds only 256 of them,
	// which at a couple of hundred bytes each lands right at the window boundary and may never block.
	big := strings.Repeat("x", 4000)
	for i := range 200 {
		// Do NOT use the fatal-on-error `send` helper here. If the watchdog trips before we finish pushing,
		// the remaining client Sends fail on a reset stream — which is a VALID outcome for this test, not a
		// failure. (edge review: the only flakiness seam.)
		if err := stream.Send(inputSeq(uint64(i+1), "say "+big)); err != nil {
			break
		}
	}

	// STAY silent. Calling Recv here would drain the window and unblock the writer — the peer would be
	// "reading again" and there would be no stall to detect. Wait out the bound without reading.
	time.Sleep(2 * time.Second)

	// The world must have torn the stream down by itself. Only now do we read: the client drains whatever the
	// world managed to write, and then sees the error.
	//
	// The client's own context still has ~25s left. If the error we see is OUR deadline rather than the
	// world's teardown, nothing was reclaimed — so bound the wait well under it.
	deadline := time.Now().Add(8 * time.Second)
	var recvErr error
	for time.Now().Before(deadline) {
		if _, err := stream.Recv(); err != nil {
			recvErr = err
			break
		}
	}
	if recvErr == nil {
		t.Fatal("the world never reclaimed the stream: a peer that stops reading blocks stream.Send forever, " +
			"and the session, its goroutines, and its entity ownership leak until the GATE closes its side (#274)")
	}
	if strings.Contains(recvErr.Error(), "DeadlineExceeded") {
		t.Fatalf("the stream ended on the CLIENT's deadline, not the world's watchdog: %v", recvErr)
	}
}

// saveCounter counts flushes that reached the store.
type saveCounter struct {
	*MemStore
	saves atomic.Int64
}

func (c *saveCounter) SaveCharacter(ctx context.Context, snap CharSnapshot) (uint64, bool, error) {
	c.saves.Add(1)
	return c.MemStore.SaveCharacter(ctx, snap)
}

// TestStreamHandlerReturnsWhenTheClientCancels pins a bug the #274 refactor introduced, and that the existing
// suite caught: making Recv interruptible put it on its own goroutine, racing the handler on the SAME stream
// context. When that goroutine wins the race it returns WITHOUT ever delivering its error to the handler — so
// a handler selecting only on {stalled, frames} blocks forever, never posts the detach, and the player stays
// resident on a dead stream. The handler must also select on ctx.Done().
//
// Observable: a detach flushes the player durably. No flush means the handler never returned.
func TestStreamHandlerReturnsWhenTheClientCancels(t *testing.T) {
	store := &saveCounter{MemStore: NewMemStore()}
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	sh := NewDemoShard().WithPersistence(store, nil)
	sh.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	zctx, zcancel := context.WithCancel(context.Background())
	t.Cleanup(zcancel)
	go sh.Run(zctx)

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := playv1.NewPlayClient(cc).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, stream, attach("Vanisher"))
	recvAttached(t, stream)

	before := store.saves.Load()
	cancel() // the client goes away without a Detach frame

	// detach() flushes the player durably before the link-death grace. If the handler is wedged, no flush.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if store.saves.Load() > before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the stream handler never returned after the client cancelled: the Recv goroutine races the " +
		"handler on the same context, and when it wins it delivers no error — the handler must select on " +
		"ctx.Done() or it blocks forever and never posts the detach (#274)")
}

// --- the frozen-session ghost (#274, distsys review) -------------------------------------------------

// newFrozenPlayer puts a live player in the zone and freezes them, as a cross-shard handoff does.
func newFrozenPlayer(t *testing.T, z *Zone, name string) *session {
	t.Helper()
	s := newTestPlayerEntity(z, name)
	z.join(s, "")
	s.frozen = true
	s.frozenFrom = s.entity.location
	Move(s.entity, nil) // move() detaches the entity for the in-flight handoff
	return s
}

// TestAbortedHandoffAfterAStreamTeardownDoesNotLeaveAGhost is the regression for the one real defect the
// #274 review found.
//
// detach() correctly declines to remove a FROZEN session — the handoff owns its fate. But the writer-stall
// watchdog has ALREADY closed the stream, so that detach is the world's only link-loss signal and it is
// swallowed. If the handoff then aborts, the thaw restores a live, un-frozen, un-detached session with no
// reap timer and a dead stream: a permanent in-world ghost, visible in `who` and the room, receiving
// nothing, unable to act, never reclaimed.
//
// Before #274 this was impossible: the world never closed the stream first, so the gate's eventual close
// drove a normal (non-frozen) detach.
func TestAbortedHandoffAfterAStreamTeardownDoesNotLeaveAGhost(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := newFrozenPlayer(t, z, "Phantom")

	// The watchdog tore the stream down while the player was frozen mid-handoff.
	z.detach("Phantom", s.out)
	if !s.frozen {
		t.Fatal("premise: detach must not remove a frozen session")
	}
	if !s.streamGone {
		t.Fatal("detach must RECORD the stream loss for a frozen session; it is the only signal the world gets")
	}

	// The handoff then fails, and the session thaws in place.
	z.handoffFailed(handoffFailMsg{id: "Phantom", reason: "destination rejected"})
	if s.frozen {
		t.Fatal("premise: handoffFailed must thaw the session")
	}
	if !s.detached {
		t.Fatal("a session thawed onto a dead stream must enter link-death; otherwise it is a permanent " +
			"in-world ghost that nothing ever reclaims (#274)")
	}

	// CARDINALITY, not just liveness: once the grace lapses the zone holds nobody.
	z.reap("Phantom", s.attachGen)
	if n := len(z.players); n != 0 {
		t.Fatalf("zone still holds %d players after the reap; the thawed ghost was never reclaimed", n)
	}
}

// TestFreezeTimeoutAfterAStreamTeardownDoesNotLeaveAGhost: the same hole via the other thaw path. A handoff
// that never resolves times out and thaws in place.
func TestFreezeTimeoutAfterAStreamTeardownDoesNotLeaveAGhost(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := newFrozenPlayer(t, z, "Wraith")

	z.detach("Wraith", s.out)
	z.freezeExpire("Wraith", s.attachGen)

	if s.frozen {
		t.Fatal("premise: freezeExpire must thaw an un-handed-off session")
	}
	if !s.detached {
		t.Fatal("a session thawed by the freeze timeout onto a dead stream must enter link-death (#274)")
	}
	z.reap("Wraith", s.attachGen)
	if n := len(z.players); n != 0 {
		t.Fatalf("zone still holds %d players after the reap", n)
	}
}

// TestAThawWithALiveStreamStaysLive: the guard must be precise. A handoff that aborts while the player's
// stream is perfectly healthy thaws them back into the world, playing.
func TestAThawWithALiveStreamStaysLive(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := newFrozenPlayer(t, z, "Survivor")

	// No detach: the stream never died.
	z.handoffFailed(handoffFailMsg{id: "Survivor", reason: "destination rejected"})

	if s.frozen || s.detached {
		t.Fatalf("a thaw with a LIVE stream must leave the player playing: frozen=%v detached=%v", s.frozen, s.detached)
	}
	if z.players["Survivor"] == nil {
		t.Fatal("the player was removed from the zone")
	}
}
