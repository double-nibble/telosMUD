package world

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
)

// transfer_reconnect_test.go — #379: a token=="" reconnect landing INSIDE an in-flight intra-shard transfer.
//
// THE WINDOW. transferOut removes the session from the source (delPlayer, which unindexes the #321 residency)
// and hands it to the destination through the destination's INBOX; the only re-index is transferIn's
// setPlayer, on the destination's goroutine. Between the two the session is in NO zone's players map, so
// zoneForResidentCharacter answers nil.
//
// WHAT THAT COST BEFORE THIS FIX. The reconnect fell through to the durable zone_ref, which transferIn never
// flushes and which is therefore stale by up to a save interval, and fresh-logged a SECOND copy from it. The
// #432 ownership fence changed the shape of the damage but not its presence: the residency miss is the same
// predicate that gates the ownership claim, so in this window the claim FIRES and the fresh copy mints a
// strictly greater epoch than the in-flight copy is carrying. The dupe is gone — only one copy can write —
// but the fresh copy is built from the stale snapshot, the older copy is evicted by ownershipLost (
// leaveNoSave, no save by design), and everything since the last flush is silently discarded. The player also
// eats an unexplained displaced kick, and #432's `event=ownership_lost` WARN — positioned as an alertable "a
// double-own is not supposed to be reachable" — fires from a benign reconnect race.
//
// THE FIX HAS TWO LAYERS, AND THE SECOND IS THE ONE THAT CLOSES THE DOUBLE-OWN.
//
//  1. RESOLVE TIME (server.go). The residency entry carries an in-flight mark (world.go, residency), set by
//     transferOut BEFORE its delPlayer and released on the same two paths that release the destination's
//     `incoming` claim. A reconnect that observes it is refused with Unavailable, which the gate retries
//     (#324: 5 times at 250ms). This layer's real job is that the refusal happens BEFORE ClaimCharacter, so
//     no ownership epoch is minted for a login that is not going to happen.
//  2. DELIVERY TIME (Zone.attach). The resolve above runs on the STREAM goroutine and the attachMsg is then
//     delivered asynchronously into a zone's inbox, so the routing decision can go stale in transit — and the
//     reachable way is not a race but ordinary QUEUEING: an attachMsg posted behind a cross-zone move that
//     this zone has not processed yet. Zone.attach re-checks residency on the zone goroutine, where it is
//     serialized against that zone's own transferOut, and refuses a fresh login when the shard holds the
//     session anywhere else. This is the layer with negative power against the two-copy bug.
//
// THE RISK THESE TESTS EXIST FOR IS THE MIRROR ONE: a refusal that gets stuck makes the character
// permanently unable to reconnect. Hence the balance test over every release path, the plain-reconnect test
// that pins the refusal as NARROW, and the TTL backstop (transferMarkTTL) with its own tests.

// parkedTransfer walks the stream's character from midgaard's temple to the darkwood boundary with darkwood's actor PARKED,
// leaving the process in exactly the #379 window: the source has run delPlayer, the destination has not run
// setPlayer, and the transferInMsg is sitting undequeued in darkwood's inbox. It returns the release.
//
// THE PREMISE IS CLAIM-FREE AND MARK-FREE. It is stated as "neither zone's players map holds them", which is
// what the window IS, rather than as "the mark is set", which is what the fix DOES. Waiting on the fix would
// make an unfixed build hang here and fail by timeout — a red that reads as a flake and names nothing.
func parkedTransfer(t *testing.T, shard *Shard, s playv1.Play_ConnectClient) (release func()) {
	t.Helper()
	src, dst := shard.ZoneByID("midgaard"), shard.ZoneByID("darkwood")

	send(t, s, inputSeq(1, "north")) // temple -> market, still inside midgaard
	recvOutputContaining(t, s, "Market Square")

	release = parkZoneActor(t, dst)
	send(t, s, inputSeq(2, "north")) // market -> darkwood: the intra-shard transfer

	waitCond(t, "the walker has left the source and not yet reached the parked destination", func() bool {
		return src.pop.Load() == 0 && dst.pop.Load() == 0 && len(dst.inbox) > 0
	})
	return release
}

// assertMidTransferRefusal dials a NEW Play stream for `name` with no handoff token — the link-dead reconnect
// — and requires it to be refused with Unavailable.
//
// It reads ONE frame and discriminates on it, rather than calling recvAttached: against an unfixed build the
// reconnect succeeds, so this must fail on the frame it receives (fast, and naming the bug) instead of
// blocking on a frame that is never coming.
func assertMidTransferRefusal(ctx context.Context, t *testing.T, client playv1.PlayClient, name string) {
	t.Helper()
	s, err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("reconnect dial: %v", err)
	}
	send(t, s, attach(name))
	f, rerr := s.Recv()
	if got := status.Code(rerr); got != codes.Unavailable {
		t.Fatalf("a reconnect landing inside an in-flight intra-shard transfer got frame %v (err=%v, code %v), "+
			"want a refusal with Unavailable: the session is held by NEITHER zone in this window, so the "+
			"residency lookup misses, the login falls through to the STALE durable zone_ref (transferIn never "+
			"flushes) and fresh-logs a SECOND copy. Since #432 that copy also mints a strictly greater epoch, "+
			"so the in-flight copy loses the fence and is evicted with NO save — the player silently loses "+
			"everything since their last flush, eats an unexplained displaced kick, and trips the "+
			"page-worthy ownership_lost alert from a benign race (#379)", f, rerr, got)
	}
}

// TestReconnectIsRefusedWhileAnIntraShardTransferIsInFlight is #379's reproduction, driven end to end over a
// REAL gRPC Play stream and made deterministic by parking the destination actor.
//
// The rig has NO persistence, which is the most direct form of "the durable record does not name the zone the
// session is in": there is no record at all, so an unfixed reconnect falls all the way to the home zone and
// spawns a second copy there. The persistence-enabled variant below adds the #432 half.
//
// CARDINALITY IS THE LOAD-BEARING ASSERTION (the #69 lesson): after the window closes there must be EXACTLY
// ONE copy of the character on the shard, not merely a copy in the right place.
func TestReconnectIsRefusedWhileAnIntraShardTransferIsInFlight(t *testing.T) {
	client, shard := indexWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src, dst := shard.ZoneByID("midgaard"), shard.ZoneByID("darkwood")

	ctx1, drop1 := context.WithCancel(ctx)
	s, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Walker"))
	recvAttached(t, s)

	release := parkedTransfer(t, shard, s)
	defer release() // idempotent; the happy path releases below

	drop1() // link death INSIDE the window: the session is owned by neither zone

	assertMidTransferRefusal(ctx, t, client, "Walker")

	// Release the park: the destination dequeues the handover and the walk completes normally.
	release()
	waitCond(t, "the walker lands in darkwood", func() bool { return dst.pop.Load() == 1 })

	// CARDINALITY. An unfixed reconnect leaves a fresh copy in the home zone AND the transferred copy in
	// darkwood: two live sessions for one character, of which the older is about to be evicted unsaved.
	if n := src.pop.Load() + dst.pop.Load(); n != 1 {
		t.Fatalf("the shard holds %d copies of Walker (midgaard=%d darkwood=%d), want exactly 1: the reconnect "+
			"fresh-logged a second copy while the first was in flight (#379)",
			n, src.pop.Load(), dst.pop.Load())
	}
	if z := shard.zoneForResidentCharacter("Walker"); z != dst {
		t.Fatalf("residency index = %v, want darkwood: the surviving copy is not the one that completed the "+
			"walk", zoneID(z))
	}
	// And the mark did not outlive its transfer — the mirror failure, which would lock the character out of
	// reconnecting for the life of the process.
	if _, inFlight := shard.residencyFor("Walker"); inFlight {
		t.Fatal("the mid-transfer mark is still set after the transfer landed: every future reconnect for this " +
			"character is refused forever (#379)")
	}
}

// claimCountingStore is a healthy MemStore that counts ownership mints. The count is the whole point of the
// test below: "no second copy" is now ALSO true of the buggy build (the #432 fence evicts one of the two), so
// the surviving-copy count alone no longer detects the bug. The spurious MINT is what remains observable.
type claimCountingStore struct {
	*MemStore
	claims atomic.Int64
}

func (c *claimCountingStore) ClaimCharacter(ctx context.Context, pid PersistID, floor uint64) (uint64, error) {
	c.claims.Add(1)
	return c.MemStore.ClaimCharacter(ctx, pid, floor)
}

// TestAReconnectDuringATransferMintsNoOwnershipEpoch is the #432 half of #379, and it is the assertion that
// still has teeth once the ownership fence is in place.
//
// Since #432 a login is an ownership assertion: it mints the next epoch atomically from the durable row. The
// residency miss this issue is about is the SAME predicate that gates that claim (route ==
// attachRouteResident), so in the transfer window the claim FIRES — the reconnect mints an epoch strictly
// greater than the one the in-flight copy carries. The in-flight copy then loses the fence and is evicted by
// ownershipLost via leaveNoSave, deliberately without a save, discarding everything since the last flush
// (transferIn never flushes, so that can be a full save interval of play).
//
// So the observable is the MINT, taken while the transfer is in flight. The first login's mint is the
// control: it proves the store claims at all, so an unchanged count on the reconnect means "refused", not
// "this store never mints".
func TestAReconnectDuringATransferMintsNoOwnershipEpoch(t *testing.T) {
	client, shard, store := persistentTransferWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src, dst := shard.ZoneByID("midgaard"), shard.ZoneByID("darkwood")

	// The durable row names MIDGAARD, and stays naming it for the whole test: an intra-shard walk never
	// flushes. That staleness is exactly what an unfixed reconnect routes by.
	if _, err := store.CreateCharacter(context.Background(), "Walker", "midgaard", "midgaard:room:temple"); err != nil {
		t.Fatal(err)
	}

	ctx1, drop1 := context.WithCancel(ctx)
	s, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Walker"))
	recvAttached(t, s)

	// THE CONTROL.
	if n := store.claims.Load(); n != 1 {
		t.Fatalf("the first login minted %d ownership epoch(s), want exactly 1: without a working mint this "+
			"test cannot tell a refusal from a store that never claims (#432)", n)
	}

	release := parkedTransfer(t, shard, s)
	defer release()
	drop1()

	assertMidTransferRefusal(ctx, t, client, "Walker")

	// THE ASSERTION, and it needs no wait: ClaimCharacter is called synchronously inside Connect, strictly
	// before the status the refusal check above already read.
	if n := store.claims.Load(); n != 1 {
		t.Fatalf("ClaimCharacter was called %d time(s), want 1 (the first login only): the reconnect minted an "+
			"ownership epoch STRICTLY GREATER than the one the in-flight copy carries, so that copy loses the "+
			"#432 fence and is evicted by ownershipLost with NO save — every point of progress since the last "+
			"flush is discarded, and the page-worthy ownership_lost alert fires on a benign race (#379)", n)
	}

	release()
	waitCond(t, "the walker lands in darkwood", func() bool { return dst.pop.Load() == 1 })
	if n := src.pop.Load() + dst.pop.Load(); n != 1 {
		t.Fatalf("the shard holds %d copies of Walker (midgaard=%d darkwood=%d), want exactly 1 (#379)",
			n, src.pop.Load(), dst.pop.Load())
	}
}

// persistentTransferWorld is indexWorld with a durable store behind it, so a login takes the durable-zone_ref
// route and reaches the #432 ownership claim. The store COUNTS its mints.
func persistentTransferWorld(t *testing.T) (playv1.PlayClient, *Shard, *claimCountingStore) {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemStore()
	store := &claimCountingStore{MemStore: mem}
	lis := bufconn.Listen(1 << 20)
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil).
		WithPersistence(store, mem)
	play := serveShard(t, sh, lis)
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})
	return play, sh, store
}

// TestAReconnectAfterTheTransferLandsIsNotRefused pins the refusal as NARROW, which is the whole risk this
// design carries: the mark's failure mode is not a missed refusal but a STUCK one, and a stuck mark means the
// character can never reconnect again for the life of the process.
//
// This is the ordinary #321 case — walk, link death, reconnect — with no park anywhere. It must re-bind the
// held session in darkwood exactly as before, with one copy on the shard.
func TestAReconnectAfterTheTransferLandsIsNotRefused(t *testing.T) {
	client, shard := indexWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src, dst := shard.ZoneByID("midgaard"), shard.ZoneByID("darkwood")

	ctx1, drop1 := context.WithCancel(ctx)
	s, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Walker"))
	recvAttached(t, s)
	send(t, s, inputSeq(1, "north"))
	recvOutputContaining(t, s, "Market Square")
	send(t, s, inputSeq(2, "north"))
	recvOutputContaining(t, s, "Moonlit Grove") // the transfer has fully LANDED

	if z, inFlight := shard.residencyFor("Walker"); z != dst || inFlight {
		t.Fatalf("residency after a COMPLETED walk = (%v, inFlight=%v), want (darkwood, false): a mark that "+
			"outlives its transfer locks the character out of every future reconnect (#379)", zoneID(z), inFlight)
	}

	drop1()

	s2, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s2, attach("Walker"))
	f, rerr := s2.Recv()
	if rerr != nil {
		t.Fatalf("an ordinary reconnect AFTER the walk landed was refused (%v): the mid-transfer mark was not "+
			"released, so this character can never reconnect again (#379)", rerr)
	}
	if f.GetAttached() == nil {
		t.Fatalf("first frame on reconnect = %v, want Attached", f)
	}
	send(t, s2, inputSeq(1, "look"))
	recvOutputContaining(t, s2, "Moonlit Grove") // re-attached where the session is held (#321)

	if n := src.pop.Load() + dst.pop.Load(); n != 1 {
		t.Fatalf("the shard holds %d copies of Walker (midgaard=%d darkwood=%d), want exactly 1",
			n, src.pop.Load(), dst.pop.Load())
	}
}

// TestTheMidTransferMarkIsReleasedOnEveryPath enumerates every release site, because the mark's lifetime is
// the argument this design rests on: it is tied to the destination's `incoming` claim precisely so it cannot
// acquire a lifecycle of its own, and an unreleased mark is a character who can never reconnect.
//
// The three paths are the two the claim has, plus the one setPlayer covers:
//
//   - transferIn DELIVERS the arrival — the ordinary walk;
//   - transferIn REJECTS the arrival (destination hosts no rooms) — an EARLY return, which is why the release
//     is transferIn's deferred statement rather than a line on the happy path;
//   - transferOut never posts (a panic inside the window) — released by the `!posted` compensator, the same
//     one that gives back the arrival claim. The zone dispatch recovers panics by design, so without the
//     compensator this failure is silent and permanent.
func TestTheMidTransferMarkIsReleasedOnEveryPath(t *testing.T) {
	t.Run("transferIn delivers the arrival", func(t *testing.T) {
		sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
		defer stop()

		src, dst := sh.zoneByID("midgaard"), sh.zoneByID("darkwood")
		walker, _ := newWalker(t, src, "Walker")
		src.post(inputMsg{id: "Walker", seq: 1, line: "north"})
		waitMarkup(t, walker, "Moonlit Grove")

		if z, inFlight := sh.residencyFor("Walker"); z != dst || inFlight {
			t.Fatalf("residency after the arrival landed = (%v, inFlight=%v), want (darkwood, false) (#379)",
				zoneID(z), inFlight)
		}
	})

	t.Run("transferIn rejects the arrival", func(t *testing.T) {
		// Hand-driven, with no actors running: the roomless destination cannot come from the demo pack, and
		// the early return it takes is the reason the release is deferred rather than inline.
		sh := NewMultiShard([]string{"midgaard"}, "midgaard", "", nil, nil)
		src := sh.zoneByID("midgaard")
		dst := newZone("roomless")
		dst.shard = sh

		s := newTestPlayerEntity(dst, "Arriver")
		// The source side of transferOut, in order: mark while still resident, then remove.
		sh.indexResident("Arriver", src)
		sh.markTransferInFlight("Arriver", src)
		src.delPlayer("Arriver")
		if z, inFlight := sh.residencyFor("Arriver"); z != nil || !inFlight {
			t.Fatalf("premise: mid-transfer residency = (%v, inFlight=%v), want (nil, true) — the mark must "+
				"survive the source's delPlayer or the window is uncovered (#379)", zoneID(z), inFlight)
		}

		dst.claimInboundArrival()
		dst.transferIn(transferInMsg{s: s, room: "darkwood:room:grove"})

		if n := dst.pop.Load(); n != 0 {
			t.Fatalf("premise: the roomless destination placed the arrival (pop=%d), so this row is not "+
				"exercising the early return it names", n)
		}
		if _, inFlight := sh.residencyFor("Arriver"); inFlight {
			t.Fatal("transferIn returned early WITHOUT releasing the mid-transfer mark: the character is now " +
				"permanently unable to reconnect — every attempt is refused for the life of the process (#379)")
		}
	})

	t.Run("transferOut panics before it posts", func(t *testing.T) {
		sh, stop := panicOnTransferShard(t)
		defer stop()

		src := sh.zoneByID("midgaard")
		walker, _ := newWalker(t, src, "Boom")
		src.post(inputMsg{id: "Boom", seq: 1, line: "north"})
		waitMarkup(t, walker, "Something went wrong") // dispatchSafe recovered: the premise

		// No polling: the compensator runs during the unwind, strictly before the recovery notice above.
		if _, inFlight := sh.residencyFor("Boom"); inFlight {
			t.Fatal("transferOut panicked inside its window and left the mid-transfer mark set. NOTHING will " +
				"ever clear it — only transferIn does, and no transferIn is coming — so this character can " +
				"never reconnect again for the life of the process (#379)")
		}
	})
}

// TestAQueuedAttachBehindACrossZoneMoveIsRefused is the second layer's reproduction, and the more serious
// half of #379: it needs no race at all, only ordinary inbox QUEUEING.
//
// The sequence a player produces by losing their connection mid-step:
//
//  1. the client's last input is `north`, a cross-zone exit, and it is sitting in midgaard's inbox;
//  2. the link dies;
//  3. the gate reconnects with token == "". Residency still (correctly) says midgaard holds the session, so
//     the login is routed there — and BECAUSE the route is "resident", server.go skips the #432 ownership
//     claim, since a re-attach must not mint;
//  4. the attachMsg lands in midgaard's inbox BEHIND the queued `north`;
//  5. midgaard processes the move — transferOut, delPlayer, hand off to darkwood — and only then the attach,
//     which finds no session and takes the fresh-login default.
//
// The result is two live copies of one character holding the SAME epoch (no mint happened), so the #432
// fence cannot even separate them: they force-write over each other. The resolve-time refusal cannot see any
// of this, because at the instant it ran the session really was resident in midgaard.
//
// The park plus waitForQueuedAttach make step 4 a PROVEN premise rather than a lucky interleaving: with
// midgaard's actor stopped between messages, the test does not proceed until it has seen the attachMsg for
// this character sitting in the same inbox as the unprocessed move. Releasing the park before that was the
// first draft of this test, and it silently tested the wrong thing — the server had not posted the attach
// yet, so the routing read happened AFTER the transfer and correctly sent the login to darkwood.
func TestAQueuedAttachBehindACrossZoneMoveIsRefused(t *testing.T) {
	client, shard := indexWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src, dst := shard.ZoneByID("midgaard"), shard.ZoneByID("darkwood")

	ctx1, drop1 := context.WithCancel(ctx)
	s, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Walker"))
	recvAttached(t, s)
	send(t, s, inputSeq(1, "north"))
	recvOutputContaining(t, s, "Market Square")

	// Park the SOURCE, then queue the cross-zone move on it. Nothing about the transfer has run yet.
	release := parkZoneActor(t, src)
	defer release()
	send(t, s, inputSeq(2, "north")) // market -> darkwood, sitting unprocessed in src's inbox
	waitForQueuedMsg(t, src, "the cross-zone move", func(m msg) bool {
		im, ok := m.(inputMsg)
		return ok && im.id == "Walker" && im.line == "north"
	})

	drop1() // link death, with the move still unprocessed

	// The reconnect. Residency names midgaard (nothing has been processed), so this is routed there and its
	// attachMsg queues BEHIND the move — and no ownership epoch is minted, because the route says "resident".
	s2, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s2, attach("Walker"))
	// The premise, now complete: the move is PROVEN to be in this inbox (above) and the attach is PROVEN to
	// be behind it (here), so the order under test is established rather than hoped for.
	waitForQueuedMsg(t, src, "the reconnect's attach", func(m msg) bool {
		am, ok := m.(attachMsg)
		return ok && am.character == "Walker"
	})

	release()

	// THE ASSERTION. Read the first frame the reconnect gets and discriminate on it: against an unfixed build
	// the attach SUCCEEDS, so waiting for a refusal would hang and fail by timeout. It must fail on the frame.
	f, rerr := s2.Recv()
	if rerr != nil {
		t.Fatalf("reconnect recv: %v", rerr)
	}
	if f.GetAttached() != nil {
		t.Fatalf("the reconnect was ATTACHED by the zone the character has just left: its attachMsg was " +
			"queued behind the cross-zone move, so by the time it was dequeued the session had already been " +
			"transferred away and the fresh-login default created a SECOND live copy. Both copies hold the " +
			"same epoch — the routing said `resident`, so no ownership epoch was minted — which is the " +
			"pre-#432 posture reached from an ordinary reconnect (#379)")
	}
	if d := f.GetDisconnect(); d == nil {
		t.Fatalf("first frame on the queued reconnect = %v, want a Disconnect naming the mid-transfer", f)
	} else if !strings.Contains(d.GetReason(), "mid-transfer") {
		t.Fatalf("disconnect reason = %q, want it to name the mid-transfer", d.GetReason())
	}

	// CARDINALITY, the load-bearing assertion (#69). The refusal frame above was produced INSIDE Zone.attach,
	// so the attach has provably been handled and this is an assertion rather than a poll.
	waitCond(t, "the transfer lands in darkwood", func() bool { return dst.pop.Load() == 1 })
	if n := src.pop.Load() + dst.pop.Load(); n != 1 {
		t.Fatalf("the shard holds %d live copies of Walker (midgaard=%d darkwood=%d), want exactly 1 (#379)",
			n, src.pop.Load(), dst.pop.Load())
	}
}

// TestAttachRefusesAFreshLoginWhenTheShardHoldsTheSessionElsewhere drives the delivery-time check directly,
// which is the only way to enumerate its predicate: the end-to-end test above can reach the marked arm or the
// already-landed arm depending on how the two zone goroutines interleave, and a test that cannot say WHICH
// arm it exercised cannot pin either.
//
// The predicate is a MISMATCH, not merely the mark. A destination that has already dequeued the handover has
// cleared the mark and re-indexed the character to itself, so `inFlight` alone would miss the interleaving
// that is easiest to hit in practice. It is also deliberately NARROW: a character this shard does not hold,
// or holds in THIS zone, must still take the ordinary fresh-login path, or an unrelated login is broken.
func TestAttachRefusesAFreshLoginWhenTheShardHoldsTheSessionElsewhere(t *testing.T) {
	tests := []struct {
		name       string
		seed       func(t *testing.T, sh *Shard, src, dst *Zone)
		wantRefuse bool
		why        string
	}{
		{
			name:       "the transfer already landed in another zone",
			seed:       func(_ *testing.T, sh *Shard, _, dst *Zone) { sh.indexResident("Walker", dst) },
			wantRefuse: true,
			why: "the destination dequeued the handover and re-indexed the character to itself, so the mark " +
				"is already gone: only the residency MISMATCH can still see that a fresh login here is a " +
				"second copy",
		},
		{
			name: "a transfer is in flight",
			seed: func(_ *testing.T, sh *Shard, src, _ *Zone) {
				sh.indexResident("Walker", src)
				sh.markTransferInFlight("Walker", src)
				src.delPlayer("Walker") // the source side of transferOut, in order
			},
			wantRefuse: true,
			why:        "the session is in no zone's players map and is on its way to a destination",
		},
		{
			name: "an OLD mark, past transferMarkTTL",
			seed: func(t *testing.T, sh *Shard, src, _ *Zone) {
				t.Helper()
				sh.indexResident("Walker", src)
				sh.markTransferInFlight("Walker", src)
				src.delPlayer("Walker")
				backdateTransferMark(t, sh, "Walker", 2*transferMarkTTL)
			},
			wantRefuse: true,
			why: "age is not this layer's business. Nobody is resident in the pre-arrival window, so the mark " +
				"is the ONLY refusal available here; a zone that aged it out on its own would be guessing the " +
				"transfer is dead, and a wrong guess is a duplicate character. Writing a mark off belongs to " +
				"expireStaleTransferMark on the login path, which pairs the age with a proof that no arrival " +
				"is outstanding — and expresses it by REMOVING the mark, which this layer then simply does " +
				"not see",
		},
		{
			name: "the shard does not hold the character at all",
			seed: func(*testing.T, *Shard, *Zone, *Zone) {},
			why:  "a brand-new character (or one whose session was already reaped) is an ordinary fresh login",
		},
		{
			name: "residency names THIS zone",
			seed: func(_ *testing.T, sh *Shard, src, _ *Zone) { sh.indexResident("Walker", src) },
			why: "the index says the character belongs here, so there is nothing to be a second copy OF; " +
				"refusing would break a login the index itself routed to us",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sh := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
			src, dst := sh.zoneByID("midgaard"), sh.zoneByID("darkwood")
			tc.seed(t, sh, src, dst)

			out := make(chan *playv1.ServerFrame, 64) // buffered: the refusal writes to it directly
			src.claimInboundArrival()                 // the bare form of the claim claimAttachTarget takes
			src.attach(attachMsg{character: "Walker", out: out})

			refused := false
			for len(out) > 0 {
				if d := (<-out).GetDisconnect(); d != nil && strings.Contains(d.GetReason(), "mid-transfer") {
					refused = true
				}
			}
			if refused != tc.wantRefuse {
				t.Fatalf("attach refused = %v, want %v: %s (#379)", refused, tc.wantRefuse, tc.why)
			}
			// The population is the fact that matters: a refusal that still created the player would be no
			// refusal at all, and an allowed login that created nothing would be a broken login.
			wantPop := int64(1)
			if tc.wantRefuse {
				wantPop = 0
			}
			if n := src.pop.Load(); n != wantPop {
				t.Fatalf("midgaard pop = %d after the attach, want %d: %s (#379)", n, wantPop, tc.why)
			}
			// And the claim is released on this path too — a refusal that leaked it would wedge the zone
			// against every future teardown (#413).
			if n := src.incoming.Load(); n != 0 {
				t.Fatalf("midgaard incoming = %d after the attach, want 0: the refusal leaked the arrival "+
					"claim and this zone can never be unhosted, rebalanced or drained again (#413)", n)
			}
		})
	}
}

// backdateTransferMark ages a character's in-flight mark by `age`, so a test can reach the TTL without
// sleeping. Same-package access to the index, under its own mutex.
func backdateTransferMark(t *testing.T, sh *Shard, character string, age time.Duration) {
	t.Helper()
	sh.residentMu.Lock()
	defer sh.residentMu.Unlock()
	r, ok := sh.residentZone[character]
	if !ok || !r.inFlight {
		t.Fatalf("premise: %q has no in-flight mark to age", character)
	}
	r.markedAt = r.markedAt.Add(-age)
	sh.residentZone[character] = r
}

// TestAStuckMidTransferMarkSelfHealsAndIsReported pins the backstop for the mark's own failure mode, and
// the two conditions that keep the backstop from becoming a bug of its own.
//
// WHY A BACKSTOP AT ALL. The mark's release sites are balanced against the destination's `incoming` claim,
// but the two failures are not equally visible: a leaked claim wedges a ZONE against unhost, which an
// operator sees and a drain deadline eventually pages on, while a leaked mark silently brick-walls exactly
// ONE player's reconnect for the life of the process, its only trace being the benign Info line the ordinary
// race also emits.
//
// WHY AGE IS NOT THE WHOLE TEST. In the pre-arrival window nothing is resident, so the mark is the only
// refusal standing between a reconnect and a second copy — Zone.attach's mismatch arm has nothing to
// mismatch against. Clearing a mark that is still live therefore re-opens the double-own rather than merely
// costing an early refusal, and a timer cannot tell a leaked mark from a destination actor that is briefly
// stalled. So the age is paired with a positive proof of the opposite: no zone on the shard holds an inbound
// arrival claim.
func TestAStuckMidTransferMarkSelfHealsAndIsReported(t *testing.T) {
	// markedShard builds a shard whose only character carries an in-flight mark in the ordinary mid-flight
	// shape: the source has already run delPlayer, so the entry is a bare mark naming no zone.
	markedShard := func(t *testing.T) (*Shard, *Zone) {
		t.Helper()
		sh := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
		src := sh.zoneByID("midgaard")
		sh.indexResident("Stuck", src)
		if !sh.markTransferInFlight("Stuck", src) {
			t.Fatal("premise: the source must be able to mark a character it holds")
		}
		src.delPlayer("Stuck")
		return sh, src
	}

	t.Run("a fresh mark is not eligible", func(t *testing.T) {
		sh, _ := markedShard(t)
		if sh.expireStaleTransferMark("Stuck") {
			t.Fatal("a mark taken microseconds ago was written off: the age bound is firing on the ordinary " +
				"transfer window it is supposed to sit three orders of magnitude above (#379)")
		}
		if _, inFlight := sh.residencyFor("Stuck"); !inFlight {
			t.Fatal("a fresh mark must still refuse: the backstop is insurance, not the mechanism (#379)")
		}
	})

	t.Run("an aged mark is kept while an arrival is outstanding", func(t *testing.T) {
		sh, _ := markedShard(t)
		backdateTransferMark(t, sh, "Stuck", 2*transferMarkTTL)
		// A destination that has been resolved but has not dequeued its handover — exactly what a stalled
		// actor looks like, and exactly what a bare timer misreads as a leak.
		sh.zoneByID("darkwood").claimInboundArrival()

		if sh.expireStaleTransferMark("Stuck") {
			t.Fatal("an aged mark was written off while an inbound arrival is still outstanding on this " +
				"shard. That is not a lockout being healed, it is the refusal being removed from a transfer " +
				"that is genuinely in flight — and in the pre-arrival window nothing else can refuse, so the " +
				"next reconnect fresh-logins a SECOND live copy (#379)")
		}
		if _, inFlight := sh.residencyFor("Stuck"); !inFlight {
			t.Fatal("the mark was cleared despite an outstanding arrival (#379)")
		}
	})

	t.Run("an aged mark with nothing outstanding is dropped and reported once", func(t *testing.T) {
		sh, _ := markedShard(t)
		backdateTransferMark(t, sh, "Stuck", 2*transferMarkTTL)

		if !sh.expireStaleTransferMark("Stuck") {
			t.Fatal("a mark that is both past its age bound and provably matched by NO outstanding arrival " +
				"was kept: nothing else will ever clear it, so this character can never reconnect again for " +
				"the life of the process, with no operator-visible symptom at all (#379)")
		}
		if _, inFlight := sh.residencyFor("Stuck"); inFlight {
			t.Fatal("the written-off mark still refuses: the self-heal must be expressed by REMOVING it, so " +
				"that every reader stops seeing it at once (#379)")
		}
		if sh.expireStaleTransferMark("Stuck") {
			t.Fatal("the stale mark was reported twice: it must be CLEARED by the sweep, or the gate's five " +
				"retries produce five ERRORs for one incident and the signal is worth less than silence (#379)")
		}
	})

	t.Run("expiring a mark never removes a live residency", func(t *testing.T) {
		// The instant between markTransferInFlight and delPlayer: the entry carries BOTH a zone and a mark,
		// and the player is still in that zone's players map.
		sh := NewMultiShard([]string{"midgaard"}, "midgaard", "", nil, nil)
		src := sh.zoneByID("midgaard")
		sh.indexResident("Live", src)
		sh.markTransferInFlight("Live", src)
		backdateTransferMark(t, sh, "Live", 2*transferMarkTTL)

		if !sh.expireStaleTransferMark("Live") {
			t.Fatal("premise: the aged mark must be written off by the sweep")
		}
		if z := sh.zoneForResidentCharacter("Live"); z != src {
			t.Fatalf("residency after the mark was released = %v, want midgaard: releasing the mark DELETED "+
				"the whole entry, punching a #321 index hole for a session that is still in that zone's "+
				"players map — a reconnect stops finding them and routes by the stale durable zone_ref, "+
				"which is the bug this file exists to close (#379)", zoneID(z))
		}
	})
}

// TestTheMidTransferMarkIsOnlyTakenByTheZoneThatHoldsTheCharacter pins the only-if-mine discipline every
// other writer in this index already follows (indexResident's counterpart unindexResident, the placement
// tombstone fence).
//
// An unconditional overwrite would let a caller that does NOT hold the character hijack the entry — pointing
// it at the wrong zone and then, on release, deleting it — and this index is what the reconnect path trusts
// to decide whether a login is a re-attach or a new copy. Unreachable today; the discipline is what keeps it
// unreachable if a fourth transferOut caller ever appears.
func TestTheMidTransferMarkIsOnlyTakenByTheZoneThatHoldsTheCharacter(t *testing.T) {
	sh := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	src, dst := sh.zoneByID("midgaard"), sh.zoneByID("darkwood")
	sh.indexResident("Walker", dst) // darkwood holds them

	if sh.markTransferInFlight("Walker", src) {
		t.Fatal("midgaard took a mid-transfer mark for a character the index attributes to darkwood: the " +
			"entry is now hijacked, and releasing it will DELETE darkwood's live residency (#379)")
	}
	if z, inFlight := sh.residencyFor("Walker"); z != dst || inFlight {
		t.Fatalf("residency after a refused mark = (%v, inFlight=%v), want (darkwood, false): a refused "+
			"mark must leave the entry exactly as it found it (#379)", zoneID(z), inFlight)
	}
}

// waitForQueuedMsg blocks until a message matching `match` is sitting in z's inbox, leaving the inbox
// exactly as it found it. z MUST be parked.
//
// A LENGTH CHECK CANNOT EXPRESS THIS, and the first draft of the queued-attach test learned it the hard way
// twice. `len(inbox) > 0` is satisfied by parkZoneActor's own presence message before the move is posted at
// all, and `len(inbox) >= 2` is satisfied by the dropped stream's detachMsg before the attach is posted — so
// both premises can be "met" with the message under test still in flight, and the test then silently
// exercises a different (and correct) ordering. Under -race that mis-premise reproduced in 9 runs out of 50.
//
// Go channels cannot be peeked, so this drains from the TEST goroutine until it captures the match and then
// restores every drained message in its original order. Safe only because the actor is parked: nothing else
// is consuming, and the restore cannot overtake anything.
func waitForQueuedMsg(t *testing.T, z *Zone, what string, match func(msg) bool) {
	t.Helper()
	var drained []msg
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-z.inbox:
			drained = append(drained, m)
			if match(m) {
				for _, back := range drained {
					z.inbox <- back // restore FIFO order; the actor is parked, so nothing raced us
				}
				return
			}
		case <-deadline:
			t.Fatalf("premise: %s never reached zone %q's inbox, so the message ordering this test is about "+
				"was never established", what, z.id)
		}
	}
}
