package world

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/test/bufconn"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/directory"
)

// TestEpochResumeOnRelogin (FIX 1) reproduces the "ownership conflict" a relog hit after a
// cross-shard move. The directory's player placement persists, so a fresh login that
// restarted the epoch at 1 would compute a stale newEpoch on its NEXT move and the
// placement CAS would reject it. With epoch-resume-on-login the relogin seeds the stored
// epoch and the SECOND cross-shard move succeeds (redirect, not handoff-fail).
func TestEpochResumeOnRelogin(t *testing.T) {
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
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-b"))

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
	aPlay := serveShard(t, NewShard("midgaard", "addr-a", dir, peers), lisA)
	bPlay := serveShard(t, NewShard("darkwood", "addr-b", dir, peers), lisB)

	// Shrink the freeze TTL so A's redirected source orphan is reaped (FIX 2B) promptly,
	// freeing the character to re-login to A without hitting the frozen "mid-transfer"
	// reject. Without this the orphan would block the relogin for the full default TTL.
	oldFreeze := freezeTTL
	freezeTTL = 150 * time.Millisecond
	t.Cleanup(func() { freezeTTL = oldFreeze })

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- first cross-shard move: A -> B (epoch 1 -> 2) ---
	sA, err := aPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sA, attach("Roamer"))
	recvAttached(t, sA)
	send(t, sA, inputSeq(1, "north")) // temple -> market
	send(t, sA, inputSeq(2, "north")) // market -> darkwood
	redirB := recvRedirect(t, sA)

	sB, err := bPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sB, attachWithToken("Roamer", redirB.GetHandoffToken()))
	recvAttached(t, sB)
	recvUntilOutput(t, sB, "Moonlit Grove")

	// The character "quits" B (clean logout) and the directory still records Roamer on
	// shard-b at epoch 2. Drop the B stream; the player goes link-dead then is reaped — but
	// the placement persists (ClearPlayer is deferred). This is the relog precondition.
	send(t, sB, &playv1.ClientFrame{Payload: &playv1.ClientFrame_Detach{Detach: &playv1.Detach{}}})

	if place, _ := dir.PlayerPlacement(ctx, "Roamer"); place.Epoch != 2 {
		t.Fatalf("after first move placement epoch = %d, want 2", place.Epoch)
	}

	// --- fresh re-login to A (NEW stream, same character) ---
	// Retry until A's redirected orphan has been reaped (freezeTTL above): a relogin while
	// the orphan is still frozen is rejected with a Disconnect ("mid-transfer"). Once reaped,
	// the attach succeeds and seeds the epoch resumed from the directory (2).
	var sA2 playv1.Play_ConnectClient
	deadline := time.Now().Add(3 * time.Second)
	for {
		sA2, err = aPlay.Connect(sctx)
		if err != nil {
			t.Fatal(err)
		}
		send(t, sA2, attach("Roamer"))
		if okAttach(t, sA2) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relogin never succeeded: A's frozen orphan was not reaped")
		}
		time.Sleep(50 * time.Millisecond)
	}
	recvUntilOutput(t, sA2, "The Temple Square")

	// --- SECOND cross-shard move must succeed (no "ownership conflict") ---
	send(t, sA2, inputSeq(1, "north")) // temple -> market
	send(t, sA2, inputSeq(2, "north")) // market -> darkwood: needs newEpoch=3 > stored 2
	redirB2 := recvRedirect(t, sA2)
	if redirB2.GetTargetShardAddr() != "addr-b" {
		t.Fatalf("second move redirect = %q, want addr-b", redirB2.GetTargetShardAddr())
	}
	// The CAS accepted epoch 3 — proof the resume worked. A stale epoch would have
	// produced "The way is barred. (ownership conflict)" instead of a Redirect.
	if place, _ := dir.PlayerPlacement(ctx, "Roamer"); place.Epoch != 3 {
		t.Fatalf("after second move placement epoch = %d, want 3", place.Epoch)
	}
}

// okAttach reads the next decisive frame: true on Attached, false on a Disconnect (the
// frozen "mid-transfer" reject). Used to retry a relogin until the source orphan is reaped.
func okAttach(t *testing.T, s playv1.Play_ConnectClient) bool {
	t.Helper()
	for {
		f, err := s.Recv()
		if err != nil {
			return false
		}
		if f.GetAttached() != nil {
			return true
		}
		if f.GetDisconnect() != nil {
			return false
		}
	}
}

// TestHandoffRPCTimeoutThawsPlayer (FIX 2A) drives a cross-shard move whose destination is
// reachable in the directory but whose Prepare never completes (the dialer errors, standing
// in for a hang the bounded context would otherwise wait out). The bounded RPC context plus
// the fail->handoffFailed path must thaw the player and RESTORE them to the room they tried
// to leave, so a follow-up look and move work.
func TestHandoffRPCTimeoutThawsPlayer(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	// Both zones are registered (so resolution SUCCEEDS), but the peer dialer to B fails —
	// exercising the Prepare-reachability failure -> fail -> thaw -> restore composition.
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterShard(ctx, "shard-b", "addr-b", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a"))
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-b"))

	// Shrink the RPC timeout so the bounded-context path resolves fast and deterministically.
	old := handoffRPCTimeout
	handoffRPCTimeout = 200 * time.Millisecond
	t.Cleanup(func() { handoffRPCTimeout = old })

	lis := bufconn.Listen(1 << 20)
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		return nil, fmt.Errorf("simulated unreachable destination %q", addr)
	}
	play := serveShard(t, NewShard("midgaard", "addr-a", dir, peers), lis)

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := play.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Stuck"))
	recvAttached(t, s)
	send(t, s, inputSeq(1, "north")) // temple -> market
	expectMarkup(t, s, "Market Square")
	send(t, s, inputSeq(2, "north")) // market -> darkwood, but B can't be reached
	recvUntilOutput(t, s, "The way is barred")

	// Thawed and restored to the market: look and a normal move both work.
	send(t, s, inputSeq(3, "look"))
	recvUntilOutput(t, s, "Market Square")
	send(t, s, inputSeq(4, "south"))
	recvUntilOutput(t, s, "The Temple Square")
}

// TestFreezeExpireThawsUnredirected (FIX 2B) drives freezeExpire directly (like the pulse
// tests drive tick) for determinism: a frozen, NOT-redirected session whose freeze elapsed
// is thawed in place and restored to frozenFrom.
func TestFreezeExpireThawsUnredirected(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	market := z.rooms["midgaard:room:market"]
	out := make(chan *playv1.ServerFrame, 16)
	s := &session{character: "Ghost", out: out, epoch: 1}
	z.newPlayerEntity(s, "Ghost")
	z.players["Ghost"] = s

	// Simulate a frozen, un-redirected source copy: detached from its room, frozenFrom set.
	Move(s.entity, nil)
	s.frozen = true
	s.redirected = false
	s.frozenFrom = market
	gen := s.attachGen

	z.freezeExpire("Ghost", gen)

	if s.frozen {
		t.Fatal("freezeExpire(!redirected) should have thawed the session")
	}
	if s.entity.location != market {
		t.Fatalf("player not restored to frozenFrom: location=%v", s.entity.location)
	}
	if z.players["Ghost"] == nil {
		t.Fatal("an un-redirected thaw must keep the player present")
	}
}

// TestFreezeExpireReapsRedirected (FIX 2B) covers the other discriminator: a redirected
// frozen orphan is REMOVED so a subsequent attach for that character succeeds (rather than
// hitting the frozen "mid-transfer" reject).
func TestFreezeExpireReapsRedirected(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	out := make(chan *playv1.ServerFrame, 16)
	s := &session{character: "Mover", out: out, epoch: 2}
	z.newPlayerEntity(s, "Mover")
	z.players["Mover"] = s
	Move(s.entity, nil)
	s.frozen = true
	s.redirected = true // handoff succeeded; the directory points elsewhere
	gen := s.attachGen

	z.freezeExpire("Mover", gen)

	if z.players["Mover"] != nil {
		t.Fatal("a redirected frozen orphan must be removed from z.players")
	}

	// A fresh attach for the same character now succeeds (no frozen reject): it lands in the
	// start room. Drive attach directly with a fresh out channel.
	var cz atomic.Pointer[Zone]
	out2 := make(chan *playv1.ServerFrame, 16)
	z.attach("Mover", "", out2, &cz, 0)
	ns := z.players["Mover"]
	if ns == nil {
		t.Fatal("attach after reap should create a fresh live session")
	}
	if ns.frozen || ns.pending {
		t.Fatalf("re-attached session should be live: frozen=%v pending=%v", ns.frozen, ns.pending)
	}
}

// TestFreezeExpireGenGuard (FIX 2B) confirms a stale timer (wrong gen) is a no-op, so a
// session that has since rebound is untouched.
func TestFreezeExpireGenGuard(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	out := make(chan *playv1.ServerFrame, 16)
	s := &session{character: "Rebound", out: out, epoch: 1, frozen: true}
	z.newPlayerEntity(s, "Rebound")
	z.players["Rebound"] = s
	s.attachGen = 5 // current generation

	z.freezeExpire("Rebound", 4) // stale timer from an older generation

	if !s.frozen || z.players["Rebound"] == nil {
		t.Fatal("a stale-generation freeze timer must be ignored")
	}
}

// TestPrepareRejectsUnplaceableRoom (FIX 3A) asserts a Prepare for a zone that cannot place
// any room is REJECTED (error reply) rather than parking a nil-location pending entity that
// later null-derefs on bind.
func TestPrepareRejectsUnplaceableRoom(t *testing.T) {
	z := newZone("empty") // no rooms spawned, no start room -> resolveRoom returns nil

	reply := make(chan error, 1)
	z.prepare(prepareMsg{
		snap:  &handoffv1.PlayerSnapshot{CharacterId: "Drifter", Name: "Drifter"},
		room:  "empty:room:nowhere",
		epoch: 5,
		token: "tok",
		reply: reply,
	})

	select {
	case err := <-reply:
		if err == nil {
			t.Fatal("prepare for an unplaceable room must reply an error")
		}
	default:
		t.Fatal("prepare must reply on the channel")
	}
	if z.players["Drifter"] != nil {
		t.Fatal("rejected prepare must NOT park a pending entity")
	}
}

// TestZoneRecoversFromHandlerPanic (FIX 3A) proves the handle-level recover() is the
// process-survival net: a handler that panics must not kill the zone goroutine; the zone
// keeps processing subsequent messages. We induce a real panic through a production handler
// (a joinMsg carrying a session with a nil entity null-derefs in join's Move), then confirm
// the zone still processes a following, well-formed message.
func TestZoneRecoversFromHandlerPanic(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	// A join whose session has a nil entity: join() calls Move(s.entity, r) -> nil-deref
	// panic inside a real handler. The handle-level recover must swallow it and keep looping.
	z.post(joinMsg{s: &session{character: "Boom"}})

	// After the panic the zone must still be alive: a well-formed join goes through.
	out := make(chan *playv1.ServerFrame, 16)
	good := &session{character: "Survivor", out: out, epoch: 1}
	z.newPlayerEntity(good, "Survivor")
	z.post(joinMsg{s: good})

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("zone did not process a message after a handler panic (loop likely died)")
		case f := <-out:
			if o := f.GetOutput(); o != nil {
				return // got room/look output: the zone survived and kept serving
			}
		}
	}
}
