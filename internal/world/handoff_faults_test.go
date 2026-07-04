package world

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/directory"
)

// handoff_faults_test.go pins two single-writer / handoff-seam invariants that had NO test
// because reproducing them needs a controlled fault at the Prepare / placement-CAS seam
// (issue #139): a CONCURRENT double-Prepare (idempotent retry vs a duplicate at the same
// epoch) and the Prepare-OK-then-CAS-LOSES rollback.
//
// The CAS half is driven by faultLocator — the test-only fault-injecting decorator over
// world.Locator (the enabler, issue #123). It wraps a real Locator and delegates every method
// unchanged EXCEPT the ones a test explicitly arms, so destination resolution + Prepare still
// work and only the ownership claim is forced to fail. Keeping the decorator here (with its
// first consumer) avoids an unused-helper the linter would reject.

// faultLocator wraps a real Locator and injects faults at chosen methods. An unset hook
// delegates, so an unconfigured faultLocator is behaviourally identical to the wrapped
// directory. setShardHook, when set, replaces SetPlayerShard's outcome: returning
// (false, nil) simulates a placement-CAS CONFLICT (a racing writer claimed this epoch) and
// (false, err) / (true, nil) cover the directory-error and success cases — none of which are
// otherwise reachable deterministically without a second concurrent mover.
type faultLocator struct {
	Locator
	setShardHook func(playerID, shardID string, epoch uint64) (bool, error)
}

func (f *faultLocator) SetPlayerShard(ctx context.Context, playerID, shardID string, epoch uint64) (bool, error) {
	if f.setShardHook != nil {
		return f.setShardHook(playerID, shardID, epoch)
	}
	return f.Locator.SetPlayerShard(ctx, playerID, shardID, epoch)
}

// TestHandoffDoublePrepareIdempotentThenRejected pins the single-writer invariant at the
// destination Prepare seam (zone.go:1191): two Prepares for the same character must never
// leave two pending copies. The gate/source may retry Prepare (an RPC timeout, a redrive);
// because the handoff token is DETERMINISTIC from (character, epoch), a retry carries the
// SAME token and must be an idempotent no-op. A prepare at the same epoch with a DIFFERENT
// token — only reachable via a crafted/forged Prepare, since production tokens are derived —
// must be REJECTED as a stale/duplicate epoch, not parked as a rival pending entity.
//
// Driven at the handler level (z.prepare) rather than through the gRPC Prepare RPC precisely
// because the RPC re-derives the token from (character, epoch) and so cannot express the
// different-token-same-epoch case the guard defends against.
func TestHandoffDoublePrepareIdempotentThenRejected(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	const epoch = 5
	snap := &handoffv1.PlayerSnapshot{CharacterId: "Racer", Name: "Racer"}
	tok := handoffToken("Racer", epoch)

	// First Prepare parks a pending player keyed at the deterministic token.
	reply := make(chan error, 1)
	z.prepare(prepareMsg{snap: snap, room: "", epoch: epoch, token: tok, reply: reply})
	if err := <-reply; err != nil {
		t.Fatalf("first prepare replied error: %v", err)
	}
	s := z.players["Racer"]
	if s == nil || !s.pending || s.token != tok {
		t.Fatalf("first prepare did not park a pending player at the token; got %+v", s)
	}

	// IDEMPOTENT RETRY: same (character, epoch) -> same token -> reply nil, and the SAME
	// *session survives (no rival copy, no mutation).
	reply2 := make(chan error, 1)
	z.prepare(prepareMsg{snap: snap, room: "", epoch: epoch, token: tok, reply: reply2})
	if err := <-reply2; err != nil {
		t.Fatalf("idempotent retry (same token) replied error: %v", err)
	}
	if got := z.players["Racer"]; got != s {
		t.Fatal("idempotent retry replaced the pending session; want the identical *session")
	}

	// DUPLICATE AT THE SAME EPOCH, DIFFERENT TOKEN: rejected as a stale/duplicate epoch, and
	// the original pending copy is left untouched (no second pending entity).
	reply3 := make(chan error, 1)
	z.prepare(prepareMsg{snap: snap, room: "", epoch: epoch, token: "forged-different-token", reply: reply3})
	if code := status.Code(<-reply3); code != codes.FailedPrecondition {
		t.Fatalf("different-token same-epoch prepare: code = %s, want FailedPrecondition", code)
	}
	if got := z.players["Racer"]; got != s || got.token != tok {
		t.Fatalf("a rejected duplicate Prepare mutated the pending session: %+v", got)
	}
}

// TestHandoffCASConflictAbortsAndThaws pins the Prepare-OK-then-CAS-LOSES rollback
// (world.go:763-770): the source rehydrates a pending copy on the destination (Prepare
// succeeds), then the placement CAS is LOST to a racing writer. The source must Abort the
// destination's pending copy and thaw the player back into the room they tried to leave —
// EXACTLY ONE live copy, on the source, and NO phantom ownership recorded for the destination.
//
// The failure is injected strictly at the CAS via faultLocator, AFTER a real, reachable
// destination has already prepared — so this exercises the rollback path, not a
// resolution/dial failure (which TestCrossShardHandoffFailureRestoresPlayer already covers).
func TestHandoffCASConflictAbortsAndThaws(t *testing.T) {
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

	// Destination B is fully hosted + reachable, so Prepare SUCCEEDS: the only failure is the
	// ownership CAS below, after B has rehydrated a pending copy.
	lisB := bufconn.Listen(1 << 20)
	bShard := NewShard("darkwood", "addr-b", dir, nil)
	serveShard(t, bShard, lisB) // B is reached only via the handoff peer dialer; its Play client is unused here

	peers := func(addr string) (handoffv1.HandoffClient, error) {
		if addr != "addr-b" {
			return nil, fmt.Errorf("unknown shard %q", addr)
		}
		return handoffv1.NewHandoffClient(dialBuf(t, lisB)), nil
	}

	// A's directory is wrapped so the placement CAS LOSES: SetPlayerShard reports the epoch was
	// claimed by a racing writer (ok=false, err=nil), the world.go:763 conflict branch. casFired
	// is atomic — it is written on the handoff goroutine and read on the test goroutine.
	var casFired atomic.Bool
	faultDir := &faultLocator{
		Locator: dir,
		setShardHook: func(_, _ string, _ uint64) (bool, error) {
			casFired.Store(true)
			return false, nil // CAS conflict: another writer won this epoch
		},
	}
	lisA := bufconn.Listen(1 << 20)
	aPlay := serveShard(t, NewShard("midgaard", "addr-a", faultDir, peers), lisA)

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := aPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Racer"))
	recvAttached(t, s)
	send(t, s, inputSeq(1, "north")) // temple -> market
	expectMarkup(t, s, "Market Square")
	send(t, s, inputSeq(2, "north")) // market -> darkwood: Prepare OK, then the CAS loses
	recvUntilOutput(t, s, "The way is barred")

	// The CAS actually ran (guards against a test that "passes" because the handoff never got
	// that far).
	if !casFired.Load() {
		t.Fatal("SetPlayerShard was never called — the CAS-conflict path did not run")
	}

	// THAW + RESTORE: the player is back in the market and LIVE — look renders and a normal
	// move still works, proving they were truly thawed (not soft-locked).
	send(t, s, inputSeq(3, "look"))
	recvUntilOutput(t, s, "Market Square")
	send(t, s, inputSeq(4, "south")) // market -> temple, a normal move still works
	recvUntilOutput(t, s, "Temple Square")

	// NO PHANTOM OWNERSHIP: the lost CAS means the directory never recorded the player on B.
	if place, perr := dir.PlayerPlacement(ctx, "Racer"); perr == nil && place.ShardID == "shard-b" {
		t.Fatalf("a lost CAS still transferred directory ownership to shard-b (phantom placement): %+v", place)
	}

	// B HOLDS NO LINGERING PENDING: the source's synchronous Abort (world.go:768-770) discarded
	// the copy B rehydrated. The Abort enqueues abortPendingMsg on B's inbox BEFORE the Abort RPC
	// returns — which precedes the "barred" thaw the player observed above — so this presenceMsg,
	// FIFO after it on B's inbox, sees the discard with no sleep.
	//
	// LOAD-BEARING ASSUMPTION: this no-sleep determinism rests on handoffServer.Abort completing
	// its z.post(abortPendingMsg) synchronously, ON the handler's call stack, before the Abort RPC
	// returns (handoff_server.go:93-102). If Abort is ever made fire-and-forget, this probe races
	// and must gain an eventually-consistent poll.
	bZone := bShard.zoneByID("darkwood")
	probe := make(chan presence, 1)
	bZone.inbox <- presenceMsg{id: "Racer", reply: probe}
	if p := <-probe; p.present {
		t.Fatal("shard-b still holds a pending copy of Racer after the source aborted the handoff")
	}
}
