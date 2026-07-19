package world

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
)

// arrival_quiescence_test.go — #413: the two SIBLING resolve-then-deliver-async windows #409 left open.
//
// #409 closed the intra-shard transfer: claimTransferTarget resolves the destination AND claims the arrival
// in ONE hold of s.mu, the same mutex UnhostZone checks quiescence under. Two other paths into a zone have
// the identical shape — they resolve the zone under mu and then deliver through its INBOX, with the counter
// that would make the zone look busy bumped by the HANDLER:
//
//   - LOGIN ATTACH (server.go). This is the severe one. UnhostZone can pass quiescence in the gap, delete
//     the zone and close(z.dead); `post` then abandons the send and the player's Play stream never receives
//     Attached. The gate does not re-resolve on that (#324), so the login is a black hole: a connected
//     socket attached to nothing, waiting forever.
//   - CROSS-SHARD Handoff.Prepare (handoff_server.go). Milder — it degrades to an RPC-deadline abort rather
//     than a silent hang — but the same window.
//
// THE FIX. Two new claiming resolvers, Shard.claimAttachTarget and Shard.claimArrivalTarget, built on the
// generalized counter primitive (claimInboundArrival / releaseInboundArrival). Neither reuses
// claimTransferTarget: its `draining` refusal would break the handoff re-dial a draining shard deliberately
// still admits, and its `handedOff` refusal would strand a player prepared before a mid-drain lease flip.
// The release is Zone.attach's and Zone.prepare's first deferred statement, so it covers every bail — plus
// one release the handoff server owes itself, for the arm where the RPC context beats the inbox send.
//
// The harness is transfer_quiescence_test.go's, generalized: stallProducersInto is producer-agnostic (it
// parks dst and fills dst's inbox, so ANY producer stalls inside its send) and interceptArrival hands the
// stalled message to the test with the claim still held and the destination live and idle.

// arrivalShard builds the rig both in-flight tests run on: a two-zone shard (home=midgaard) serving REAL
// Play and Handoff over bufconn, with a MemStore behind it so a login can be routed by a durable zone_ref.
//
// The leaser reports a peer as darkwood's owner, so UnhostZone's ownership precondition is never the
// blocker — the only thing that may refuse a teardown here is quiescence, which is the whole subject.
func arrivalShard(t *testing.T) (*Shard, playv1.PlayClient, handoffv1.HandoffClient, *MemStore) {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemStore()
	lis := bufconn.Listen(1 << 20)
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil).
		WithZoneLeasing(unhostLeaser{owner: "shard-b"}, "shard-a", 0, 0, nil).
		WithPersistence(mem, mem)
	play := serveShard(t, sh, lis) // registers Play AND Handoff, and runs the shard
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})
	return sh, play, handoffv1.NewHandoffClient(dialBuf(t, lis)), mem
}

// resolveAttachZone wraps resolveAttachZoneLocked in the lock its contract requires, for the routing tests
// that only care WHICH zone the decision picks. The production caller (claimAttachTarget) takes the same lock
// and claims inside it; here there is no claim to take, so the two cannot be conflated.
func resolveAttachZone(s *Shard, character, token string, loaded CharSnapshot, loadedOK bool) *Zone {
	s.mu.Lock()
	defer s.mu.Unlock()
	z, _ := s.resolveAttachZoneLocked(character, token, loaded, loadedOK)
	return z
}

// TestUnhostZoneRefusesADestinationWithALoginAttachInFlight is #413's headline reproduction: the login black
// hole, driven through the REAL Play stream so the symptom the player experiences is the thing asserted.
//
// ROUTING THE TEST THROUGH THE ONE REACHABLE BRANCH. resolveAttachZoneLocked has four branches and only ONE
// of them can reach this window:
//
//   - the TOKEN branch is already covered — Prepare ran setPlayer for the pending session, so pop >= 1 and
//     the zone was never quiescent;
//   - the RESIDENCY branch is covered by definition — it resolves the zone that HOLDS the session, so
//     pop >= 1 there too;
//   - the HOME fallback is covered because UnhostZone flatly refuses `id == s.home`;
//   - the DURABLE zone_ref branch is the gap: it resolves a NON-home zone that need hold nothing at all.
//
// So the rig seeds a durable row whose zone_ref names darkwood, and the login is a plain relog. Anything
// else would be a test that cannot go red.
func TestUnhostZoneRefusesADestinationWithALoginAttachInFlight(t *testing.T) {
	sh, play, _, mem := arrivalShard(t)
	dst := sh.zoneByID("darkwood")

	// The durable row: a non-home zone_ref with a real PID, so the login takes the zone_ref branch AND the
	// #432 ownership claim (which sits just above the arrival claim in Connect).
	ctx := context.Background()
	if _, err := mem.CreateCharacter(ctx, "Relogger", "darkwood", "darkwood:room:grove"); err != nil {
		t.Fatal(err)
	}
	if sh.zoneForResidentCharacter("Relogger") != nil {
		t.Fatal("premise: the character must NOT be resident, or the residency branch routes the attach and " +
			"this test cannot reach the window")
	}

	sctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var stream playv1.Play_ConnectClient

	// INTERCEPT BEFORE THE TEARDOWN, deliberately. Unhosting first would close z.dead, `post` would take its
	// dead arm, and the attachMsg would never reach the test's hands — the window would be gone before it
	// could be observed.
	//
	// The premise predicate is CLAIM-FREE (see interceptArrival): asserting `incoming == 1` here would make
	// an unfixed producer hang and fail by TIMEOUT instead of by assertion. What pins the producer is the
	// message identity — an attachMsg for this character, in the test's hands and in no queue.
	arrival := interceptArrival(t, dst,
		func(m msg) bool { am, ok := m.(attachMsg); return ok && am.character == "Relogger" },
		func() bool { return dst.pop.Load() == 0 && len(dst.inbox) == cap(dst.inbox) },
		func() {
			var err error
			stream, err = play.Connect(sctx)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			// Client-side Send is non-blocking; it is the SERVER's handler goroutine that stalls in
			// zone.post, holding the claim with the attachMsg enqueued nowhere.
			send(t, stream, attach("Relogger"))
		})

	// THE ASSERTION. darkwood reads pop 0 and stashed 0, and a live login is on its way into it. Pre-fix
	// quiescent() passes, the zone is deleted, z.dead is closed, and the abandoned attachMsg is sitting right
	// here in the test's hands — which is exactly what the player's stream would be waiting on forever.
	err := sh.UnhostZone(context.Background(), "darkwood")
	if err == nil || !strings.Contains(err.Error(), "not quiescent") {
		t.Fatalf("UnhostZone did not REFUSE a zone with a login attach in flight to it (err=%v): it passed "+
			"its quiescence check, so the attachMsg is abandoned and the player's stream never receives "+
			"Attached — a connected socket attached to nothing, which the gate does not re-resolve (#413)", err)
	}
	if !strings.Contains(err.Error(), "1 inbound arrival") {
		t.Fatalf("the refusal must name the in-flight arrival, got %v", err)
	}
	if sh.zoneByID("darkwood") == nil {
		t.Fatal("a refused UnhostZone must leave the zone hosted so the login can still land")
	}

	// Deliver the intercepted attach — the send the stalled handler would have made — and the login completes
	// on the REAL stream. This frame is literally the bug: pre-fix it never arrives.
	dst.post(arrival)
	recvAttached(t, stream)
	// Then take a full round trip through the zone goroutine. The inbox is FIFO, so an answer to a message
	// posted AFTER the attach proves the attach handler has returned — including its deferred release. That
	// makes the claim an ASSERTION rather than a polled wait: a leaked claim never converges, so folding it
	// into a waitCond would report a timeout instead of naming the leak, and would stop the two
	// refusal-reason assertions below from ever being reached.
	if present, ok := probePresent(dst, "Relogger"); !ok || !present {
		t.Fatalf("darkwood does not report the logged-in player as resident (present=%v ok=%v)", present, ok)
	}
	if n := dst.pop.Load(); n != 1 {
		t.Fatalf("darkwood pop = %d after the login landed, want 1", n)
	}
	if n := dst.incoming.Load(); n != 0 {
		t.Fatalf("darkwood incoming = %d after the attach was handled, want 0: Zone.attach did not release "+
			"the claim, and nothing else ever will (#413)", n)
	}

	// Still unhostable — but now for the RESIDENT reason. A claim that LEAKED instead of being released
	// would report the arrival reason forever and wedge this zone against every future teardown, rebalance
	// and drain deadline. Distinguishing the two reasons is what makes that detectable.
	err = sh.UnhostZone(context.Background(), "darkwood")
	if err == nil {
		t.Fatal("UnhostZone tore down a zone with a resident player")
	}
	if strings.Contains(err.Error(), "1 inbound arrival") {
		t.Fatalf("Zone.attach leaked its arrival claim; the zone can never be unhosted again: %v", err)
	}
	if !strings.Contains(err.Error(), "1 resident player") {
		t.Fatalf("the refusal must name the resident, got %v", err)
	}

	// And once the player is gone the teardown is allowed: the counter really did return to zero.
	quit(t, dst, "Relogger")
	waitCond(t, "the destination becomes quiescent", dst.quiescent)
	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("a quiescent zone must be unhostable, got %v", err)
	}
}

// TestUnhostZoneRefusesADestinationWithACrossShardPrepareInFlight is the same window on the sibling path,
// driven through a REAL Handoff.Prepare RPC over bufconn.
//
// The RPC gets a LONG deadline on purpose. A short one would abort the handler while the window is held open,
// and the test would then be measuring the abort's release (which has its own test below) instead of the
// teardown refusal.
func TestUnhostZoneRefusesADestinationWithACrossShardPrepareInFlight(t *testing.T) {
	sh, _, hoff, _ := arrivalShard(t)
	dst := sh.zoneByID("darkwood")

	pctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	prepared := make(chan error, 1)

	arrival := interceptArrival(t, dst,
		func(m msg) bool { pm, ok := m.(prepareMsg); return ok && pm.snap.GetCharacterId() == "Wayfarer" },
		func() bool { return dst.pop.Load() == 0 && len(dst.inbox) == cap(dst.inbox) },
		func() {
			// The RPC blocks in its inbox send, so it must run on its own goroutine.
			go func() {
				_, err := hoff.Prepare(pctx, &handoffv1.PrepareRequest{
					Snapshot:     &handoffv1.PlayerSnapshot{CharacterId: "Wayfarer", Name: "Wayfarer"},
					Epoch:        7,
					TargetZoneId: "darkwood",
					TargetRoomId: "darkwood:room:grove",
				})
				prepared <- err
			}()
		})

	// THE ASSERTION. Same shape as the attach: nothing but the claim records that a session is on its way in.
	err := sh.UnhostZone(context.Background(), "darkwood")
	if err == nil || !strings.Contains(err.Error(), "not quiescent") {
		t.Fatalf("UnhostZone did not REFUSE a zone with a cross-shard Prepare in flight to it (err=%v): it "+
			"passed its quiescence check, deleted the zone and closed z.dead, so the prepareMsg is abandoned "+
			"and the handoff hangs until the source's RPC deadline (#413)", err)
	}
	if !strings.Contains(err.Error(), "1 inbound arrival") {
		t.Fatalf("the refusal must name the in-flight arrival, got %v", err)
	}
	if sh.zoneByID("darkwood") == nil {
		t.Fatal("a refused UnhostZone must leave the zone hosted so the handoff can still land")
	}

	// Deliver the intercepted prepare and the RPC completes: the reply channel rides the message, so the
	// still-blocked handler is answered by the zone exactly as it would have been.
	dst.post(arrival)
	select {
	case perr := <-prepared:
		if perr != nil {
			t.Fatalf("Prepare failed after the intercepted message was delivered: %v", perr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Prepare never returned after its message was delivered")
	}

	// A FIFO round trip proves Zone.prepare has RETURNED — including its deferred release — so the claim is
	// an assertion rather than a polled wait. Folding it into a waitCond made a dropped release fail as a
	// 5s timeout instead of naming the leak.
	if present, ok := probePresent(dst, "Wayfarer"); !ok || !present {
		t.Fatalf("darkwood does not hold the prepared pending player (present=%v ok=%v)", present, ok)
	}
	if n := dst.incoming.Load(); n != 0 {
		t.Fatalf("darkwood incoming = %d after Zone.prepare handled the message, want 0: the claim leaked "+
			"and nothing else will ever release it (#413)", n)
	}
	// CARDINALITY, not existence (the #69 lesson). "A pending player is present" is also true if the message
	// were delivered TWICE — and a double delivery would mean a second claim that was never taken, so the
	// counter would end up negative and the zone permanently non-quiescent. pop pins the count.
	if n := dst.pop.Load(); n != 1 {
		t.Fatalf("darkwood pop = %d after ONE Prepare, want exactly 1: a second delivery of the same "+
			"prepareMsg would release a claim that was never taken (#413)", n)
	}
}

// TestPrepareReleasesItsClaimWhenTheRPCDeadlineBeatsTheInboxSend is the release site the issue does not
// mention and the one that would have made this fix a NET REGRESSION.
//
// Handoff.Prepare does not use z.post. It does a raw select on z.inbox with a `case <-ctx.Done()` arm, and
// that arm returns with the claim taken and the message enqueued NOWHERE — so nothing downstream will ever
// release it. And it is the COMMON failure, not an exotic one: an RPC deadline elapsing while the send is
// blocked on a busy zone's full inbox is ordinary backpressure. Without an explicit release there, a routine
// cleanly-aborted handoff would convert into a PERMANENTLY un-unhostable zone — strictly worse than the race
// the claim exists to fix.
//
// The rig is stallProducersInto with no intercept: the inbox is full, so the RPC parks in its send until its
// own SHORT deadline fires. Then the zone must be tearable again.
func TestPrepareReleasesItsClaimWhenTheRPCDeadlineBeatsTheInboxSend(t *testing.T) {
	sh, _, hoff, _ := arrivalShard(t)
	dst := sh.zoneByID("darkwood")

	release := stallProducersInto(t, dst)

	pctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := hoff.Prepare(pctx, &handoffv1.PrepareRequest{
		Snapshot:     &handoffv1.PlayerSnapshot{CharacterId: "Doomed", Name: "Doomed"},
		Epoch:        3,
		TargetZoneId: "darkwood",
		TargetRoomId: "darkwood:room:grove",
	})
	if got := status.Code(err); got != codes.DeadlineExceeded {
		t.Fatalf("Prepare error = %v (code %v), want DeadlineExceeded: the premise is an RPC that gives up "+
			"while blocked in the inbox send, with its claim taken", err, got)
	}

	// GATE ON THE OBSERVABLE, NOT ON THE RPC'S RETURN. The client's deadline only makes hoff.Prepare RETURN;
	// it says nothing about when the SERVER handler goroutine observes ctx.Done(). Releasing the park here
	// would drain the fillers and free inbox space, and a handler that has not yet seen cancellation would
	// then win `case z.inbox <- m:` — parking a pending player, taking pop to 1, and never running the arm
	// under test. The test would then read UnhostZone's entirely correct "1 resident player" refusal as a
	// claim leak. That is not hypothetical: it flaked 6 times in 40 runs under -race, and it flaked with the
	// message that names the #413 wedge, which is the worst possible false positive.
	//
	// `incoming == 0` with the inbox STILL FULL is reachable only through the not-posted arm: the send never
	// completed (nothing was dequeued, so the fillers are all still there) AND the claim came back.
	//
	// A BOUNDED SETTLE, then explicit assertions — deliberately not a waitCond. The only thing being waited
	// for is the handler goroutine's unwind, which takes microseconds once the RPC has returned; a leaked
	// claim never converges, and reporting that as "timed out waiting for ..." would bury the finding. So
	// the loop just gives the unwind room, and the assertions below name what actually went wrong.
	settle := time.Now().Add(2 * time.Second)
	for time.Now().Before(settle) && dst.incoming.Load() != 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if got, want := len(dst.inbox), cap(dst.inbox); got != want {
		t.Fatalf("darkwood inbox len = %d, want %d (still full): the message WAS enqueued, so this ran the "+
			"reply-wait arm rather than the not-posted arm and proves nothing about it", got, want)
	}
	if n := dst.incoming.Load(); n != 0 {
		t.Fatalf("darkwood incoming = %d after the Prepare aborted with its message enqueued NOWHERE, want "+
			"0: the ctx arm of the inbox send returned holding the claim, and nothing downstream will ever "+
			"release it — an ordinary handoff abort has permanently wedged this zone (#413)", n)
	}

	// Only now is it safe to let the zone run: the arm under test has provably already been taken.
	release()
	waitCond(t, "the destination drains the inbox fillers", func() bool { return len(dst.inbox) == 0 })

	// The claim first — the direct statement of the invariant — and then the primitive that would suffer the
	// wedge, so a regression is reported as a leak rather than as an unexplained teardown refusal.
	if n := dst.incoming.Load(); n != 0 {
		t.Fatalf("darkwood incoming = %d after an aborted Prepare, want 0", n)
	}
	if n := dst.pop.Load(); n != 0 {
		t.Fatalf("darkwood pop = %d after an aborted Prepare, want 0: the message was enqueued after all, "+
			"so this ran the WRONG arm of the select and proves nothing about the not-posted release", n)
	}
	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("a zone whose inbound Prepare ABORTED on its RPC deadline must still be unhostable, got %v: "+
			"the ctx arm of the inbox send returned with the claim held and the message enqueued nowhere, so "+
			"NOTHING will ever release it — an ordinary handoff abort has permanently wedged this zone "+
			"against every teardown, rebalance and drain deadline (#413)", err)
	}
}

// TestPrepareDoesNotReleaseTwiceWhenTheDeadlineBeatsTheREPLY is the OTHER arm of the same select pair, and it
// is the trap the fix above sets. Handoff.Prepare selects on ctx twice: once around the inbox SEND (where a
// timeout means the message went nowhere and the claim must be given back) and once around the REPLY WAIT
// (where the message is already enqueued, so Zone.prepare owns the claim and will release it). Releasing on
// both looks symmetric and is wrong: the second release drives the counter NEGATIVE, which is the identical
// permanent-non-quiescence wedge reached from the other side — the zone can never be unhosted, rebalanced or
// drained again, and the arithmetic never recovers.
//
// The rig is the inverse of the not-posted test: the actor is PARKED but the inbox is EMPTY, so the send
// lands instantly and the RPC burns its deadline waiting for a reply that the parked zone has not produced.
func TestPrepareDoesNotReleaseTwiceWhenTheDeadlineBeatsTheReply(t *testing.T) {
	sh, _, hoff, _ := arrivalShard(t)
	dst := sh.zoneByID("darkwood")

	release := parkZoneActor(t, dst) // parked, but the inbox has room: the SEND succeeds, the REPLY does not

	pctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := hoff.Prepare(pctx, &handoffv1.PrepareRequest{
		Snapshot:     &handoffv1.PlayerSnapshot{CharacterId: "Patient", Name: "Patient"},
		Epoch:        3,
		TargetZoneId: "darkwood",
		TargetRoomId: "darkwood:room:grove",
	})
	if got := status.Code(err); got != codes.DeadlineExceeded {
		t.Fatalf("Prepare error = %v (code %v), want DeadlineExceeded: the premise is an RPC whose message "+
			"IS enqueued and which gives up waiting for the reply", err, got)
	}

	// Unpark: the zone now dequeues the prepareMsg and Zone.prepare releases the claim — the only release
	// this handoff is entitled to.
	release()
	if present, ok := probePresent(dst, "Patient"); !ok || !present {
		t.Fatalf("darkwood never handled the enqueued prepareMsg (present=%v ok=%v); the premise is that it "+
			"DID land, which is why the RPC handler must not release", present, ok)
	}

	// CARDINALITY. Exactly one pending session for exactly one Prepare: a double delivery would also satisfy
	// probePresent, and would itself be a second unmatched release.
	if n := dst.pop.Load(); n != 1 {
		t.Fatalf("darkwood pop = %d after ONE Prepare, want exactly 1", n)
	}

	// THE ASSERTION. Exactly one release for exactly one claim. -1 is not "tidier than +1": both are
	// permanently non-quiescent.
	if n := dst.incoming.Load(); n != 0 {
		t.Fatalf("darkwood incoming = %d after a Prepare whose reply wait timed out, want 0. A NEGATIVE "+
			"count means the RPC handler released a claim that Zone.prepare also released: the counter can "+
			"never reach zero again, so this zone is wedged against every teardown, rebalance and drain "+
			"deadline (#413)", n)
	}
}

// TestTheNewClaimingResolversRefuseAnUnhostedZone pins the CONTRACT of the two new resolvers at each of the
// two orders a claim and a teardown may interleave in, sequentially.
//
// WHAT THIS DOES NOT COVER, stated so it is not mistaken for more than it is. It is NOT a test of the single
// s.mu hold. Moving `z.claimInboundArrival()` outside the lock in either resolver — the exact pre-#409
// non-atomic shape — leaves this test green, because the sequential unhost-first case resolves nil (or falls
// back to home) whether the claim is inside the lock or not. The atomicity is only observable when a
// teardown completes in the gap between the resolve and the claim, which is one mutex acquisition wide and
// cannot be hit deterministically without a hook in production code. A sweep-vs-claim stress was tried and
// discarded: UnhostZone's actor wait plus HostZone's rebuild make the sweeper orders of magnitude slower
// than the claim loop (~1 completed teardown per run), so it never overlapped the window and passed under
// the mutation it was written for — false assurance is worse than an acknowledged gap.
//
// This is the same limitation TestClaimTransferTargetIsAtomicWithTeardown states for #409, where the
// probabilistic net is TestUnhostSweepNeverOrphansATransferringSession driving REAL walkers (whose crossings
// are slow enough to overlap a sweeper) under -race. The two new producers have no equivalent end-to-end
// stress; the single hold is defended structurally, by the resolvers being the only way to take these claims
// and by claimInboundArrival's doc forbidding a bare claim on a shard-hosted zone.
//
//   - CLAIM first: the zone is still hosted, the claim is taken, and the teardown must refuse (the two
//     in-flight tests above drive this end to end).
//   - UNHOST first: the zone is gone from s.zones and the resolver must not hand it out. For
//     claimArrivalTarget that is a nil return. For claimAttachTarget it is the HOME fallback — the durable
//     zone_ref no longer resolves, which is the ordinary #320 degradation — and the load-bearing part is
//     that the torn-down object's counter is untouched.
func TestTheNewClaimingResolversRefuseAnUnhostedZone(t *testing.T) {
	tests := []struct {
		name  string
		claim func(sh *Shard) *Zone
	}{
		{
			name: "claimAttachTarget (login attach)",
			claim: func(sh *Shard) *Zone {
				// A durable zone_ref naming darkwood: the one branch that resolves a non-home zone.
				z, _ := sh.claimAttachTarget("Relogger", "", CharSnapshot{ZoneRef: "darkwood"}, true)
				return z
			},
		},
		{
			name:  "claimArrivalTarget (cross-shard Prepare)",
			claim: func(sh *Shard) *Zone { return sh.claimArrivalTarget("darkwood") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("claim first: the teardown must refuse", func(t *testing.T) {
				sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
				defer stop()

				live := sh.zoneByID("darkwood")
				got := tc.claim(sh)
				if got != live {
					t.Fatalf("premise: darkwood is hosted, so the resolver must return it; got %v", got)
				}
				if n := live.incoming.Load(); n != 1 {
					t.Fatalf("incoming after the claim = %d, want 1: the resolve did not take a claim, so a "+
						"teardown can complete between it and the delivery (#413)", n)
				}
				err := sh.UnhostZone(context.Background(), "darkwood")
				if err == nil || !strings.Contains(err.Error(), "1 inbound arrival") {
					t.Fatalf("a claimed destination must refuse teardown, got %v", err)
				}
			})

			t.Run("unhost first: the resolver never hands out the torn-down zone", func(t *testing.T) {
				sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
				defer stop()

				dead := sh.zoneByID("darkwood")
				if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
					t.Fatalf("premise: an empty darkwood must be unhostable, got %v", err)
				}

				if got := tc.claim(sh); got == dead {
					t.Fatal("the resolver returned a zone this shard no longer hosts: the caller would hand " +
						"its message to a stopped actor, whose `post` abandons the send on z.dead — the " +
						"arrival is dropped AND the claim is never released (#413)")
				}
				// The counter on the torn-down object must be untouched. A claim taken on a corpse is the
				// worst of the three outcomes: nothing releases it, and if this zone object is ever
				// re-adopted it could never be unhosted again.
				if n := dead.incoming.Load(); n != 0 {
					t.Fatalf("a resolve against an unhosted zone left %d on its counter; nothing will ever "+
						"release it (#413)", n)
				}
			})
		})
	}
}

// TestALoginRefusedByAFailingOwnershipClaimLeavesNoClaimBehind covers the path that made the `posted`
// insurance defer in Connect LIVE rather than dead code (#413 review).
//
// The arrival claim is deliberately taken BEFORE ClaimCharacter, so that ONE residency observation serves
// both the routing decision and the #432 claim-skip (two reads with a blocking round trip between them
// diverge, and the dangerous direction silently drops the ownership claim entirely). The price is that
// ClaimCharacter's FAIL-CLOSED return now runs with the claim held.
//
// That makes a store outage the trigger: it is not a rare path, it is a correlated one, and every refused
// login would leak a claim on the zone it was routed to. A handful of reconnects during a Postgres blip
// would leave that zone permanently non-quiescent — unhostable, un-rebalanceable, and burning every drain
// deadline for the life of the process. The deferred release is what keeps a refusal a refusal.
func TestALoginRefusedByAFailingOwnershipClaimLeavesNoClaimBehind(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	mem := NewMemStore()
	store := claimFailingStore{MemStore: mem} // ownerepoch_test.go: a healthy store whose ownership mint fails
	lis := bufconn.Listen(1 << 20)
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil).
		WithZoneLeasing(unhostLeaser{owner: "shard-b"}, "shard-a", 0, 0, nil).
		WithPersistence(store, mem)
	play := serveShard(t, sh, lis)
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})
	dst := sh.zoneByID("darkwood")

	// The durable row routes the login to darkwood (non-home) and gives it a PID, so the ownership claim is
	// attempted — and then fails.
	if _, err := mem.CreateCharacter(context.Background(), "Unlucky", "darkwood", "darkwood:room:grove"); err != nil {
		t.Fatal(err)
	}

	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := play.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, stream, attach("Unlucky"))

	// The login is refused, as #432 requires: a session that cannot prove ownership must not play.
	_, rerr := stream.Recv()
	if got := status.Code(rerr); got != codes.Unavailable {
		t.Fatalf("login error = %v (code %v), want Unavailable: the premise is the #432 fail-closed refusal, "+
			"which is the return that now runs with the arrival claim held", rerr, got)
	}

	// THE ASSERTION, and it needs NO wait: the deferred release runs during the handler's return, strictly
	// before gRPC writes the status the client just read. So the claim is provably back down by now, and a
	// leak is reported as a leak rather than as a timeout.
	if n := dst.incoming.Load(); n != 0 {
		t.Fatalf("darkwood incoming = %d after a login REFUSED by a failing ownership claim, want 0: the "+
			"#432 fail-closed return runs holding the arrival claim, and nothing downstream releases it — a "+
			"store blip would permanently wedge every zone a refused login was routed to (#413)", n)
	}
	if n := dst.pop.Load(); n != 0 {
		t.Fatalf("darkwood pop = %d after a REFUSED login, want 0: the player was admitted anyway", n)
	}
	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("a zone whose inbound login was refused by a failing ownership claim must still be "+
			"unhostable, got %v: the fail-closed return left the arrival claim held and NOTHING releases "+
			"it, so a store blip permanently wedges every zone a refused login was routed to (#413)", err)
	}
}

// TestAReattachDoesNotMintANewOwnershipEpoch pins the #432 claim-skip that #413's reordering re-expressed.
//
// The skip used to read the residency index directly, right next to the routing decision's own read of it.
// Moving the arrival claim earlier put a blocking ClaimCharacter round trip BETWEEN those two reads, and the
// dangerous divergence is silent: resident at the claim check (so the claim is SKIPPED) but gone by the
// routing read (so attach takes its fresh-login default at the merely RESUMED epoch, unclaimed) — two live
// copies holding the same epoch, which is the pre-#432 posture the whole fence exists to remove.
// `unindexResident` runs from delPlayer, the ordinary link-dead reap, so a reconnect racing it is the normal
// case rather than an exotic one.
//
// The fix makes the two consumers share ONE observation: the skip reads `route == attachRouteResident`
// rather than taking a second look. This test pins the behaviour that encoding has to preserve — a
// re-attach must not mint — so a future edit that re-splits the read, or drops the guard entirely, is caught.
// (The interleaving itself needs a hook to trigger deterministically and is not tested; what is tested is
// the invariant it would violate.)
func TestAReattachDoesNotMintANewOwnershipEpoch(t *testing.T) {
	sh, play, _, mem := arrivalShard(t)
	dst := sh.zoneByID("darkwood")

	ctx := context.Background()
	if _, err := mem.CreateCharacter(ctx, "Resident", "darkwood", "darkwood:room:grove"); err != nil {
		t.Fatal(err)
	}

	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// FIRST login: a genuine claim. This is the control — it proves the mint happens at all, so a green
	// result on the second login means "skipped", not "the store never mints".
	first, err := play.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, first, attach("Resident"))
	recvAttached(t, first)
	waitCond(t, "the first login is resident", func() bool { return dst.pop.Load() == 1 })
	waitCond(t, "the residency index has the character", func() bool {
		return sh.zoneForResidentCharacter("Resident") == dst
	})
	row, ok, err := mem.LoadCharacter(ctx, "Resident")
	if err != nil || !ok {
		t.Fatalf("load after the first login: ok=%v err=%v", ok, err)
	}
	claimed := row.OwnerEpoch
	if claimed == 0 {
		t.Fatal("premise: the FIRST login must mint an ownership epoch, or this test cannot tell a skip " +
			"from a store that never claims (#432)")
	}

	// SECOND login for the same character while the session is STILL HELD here: the residency route, hence
	// a re-attach, hence no new mint.
	second, err := play.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, second, attach("Resident"))
	recvAttached(t, second)

	// A FIFO round trip proves the second attach has been handled before the row is read.
	if present, ok := probePresent(dst, "Resident"); !ok || !present {
		t.Fatalf("darkwood no longer holds the character after the re-attach (present=%v ok=%v)", present, ok)
	}
	if n := dst.pop.Load(); n != 1 {
		t.Fatalf("darkwood pop = %d after a re-attach, want exactly 1: the second login created a SECOND "+
			"copy instead of re-binding the held session (#321)", n)
	}

	row, ok, err = mem.LoadCharacter(ctx, "Resident")
	if err != nil || !ok {
		t.Fatalf("load after the re-attach: ok=%v err=%v", ok, err)
	}
	if row.OwnerEpoch != claimed {
		t.Fatalf("owner_epoch advanced %d -> %d on a RE-ATTACH: the claim-skip did not see the residency "+
			"route, so an ordinary reconnect raised the row above the live session's epoch — every "+
			"in-flight save then comes back not-owner and the player is unsaveable and evictable (#432)",
			claimed, row.OwnerEpoch)
	}
	if n := dst.incoming.Load(); n != 0 {
		t.Fatalf("darkwood incoming = %d after two logins, want 0 (#413)", n)
	}
}

// TestAttachReleasesItsInboundClaimOnEveryPath pins the release on EVERY path out of Zone.attach, driven by
// hand (take a bare claim, call the handler) so no goroutine timing is involved.
//
// The REJECTIONS are what matter and the reason the decrement is `defer`red as attach's FIRST statement. A
// handoff bind with a bad token, an unknown token, and a re-dial to a frozen mid-handoff copy all return
// EARLY; a release placed at the bottom of the happy path would never run on any of them. And these are not
// exotic — a stale gate retry produces the unknown-token case routinely. A leaked claim is PERMANENT:
// quiescent() reports false forever, turning a fix for "torn down too eagerly" into "can never be torn down".
func TestAttachReleasesItsInboundClaimOnEveryPath(t *testing.T) {
	demoZone := func() *Zone {
		return NewMultiShard([]string{"darkwood"}, "darkwood", "", nil, nil).zoneByID("darkwood")
	}
	tests := []struct {
		name  string
		zone  func() *Zone
		msg   func(z *Zone, out chan *playv1.ServerFrame) attachMsg
		setup func(t *testing.T, z *Zone)
	}{
		{
			name: "fresh login",
			zone: demoZone,
			msg: func(_ *Zone, out chan *playv1.ServerFrame) attachMsg {
				return attachMsg{character: "Newbie", out: out}
			},
		},
		{
			name: "re-attach of a held session",
			zone: demoZone,
			setup: func(t *testing.T, z *Zone) {
				t.Helper()
				// The seeding attach releases a claim like any other, so it must TAKE one. Driving it
				// unclaimed and then Store(0)-ing would make a PASSING run emit the real underflow ERROR —
				// training a reader to ignore the exact report mutation-testing relies on — and Store(0) is
				// a blunt reset that would also mask a double release on the seeding path itself.
				z.claimInboundArrival()
				z.attach(attachMsg{character: "Returner", out: make(chan *playv1.ServerFrame, 64)})
				if n := z.incoming.Load(); n != 0 {
					t.Fatalf("the seeding attach left incoming = %d, want 0", n)
				}
			},
			msg: func(_ *Zone, out chan *playv1.ServerFrame) attachMsg {
				return attachMsg{character: "Returner", out: out}
			},
		},
		{
			name: "handoff token with no pending player",
			zone: demoZone,
			msg: func(_ *Zone, out chan *playv1.ServerFrame) attachMsg {
				return attachMsg{character: "Ghost", token: "no-such-token", out: out}
			},
		},
		{
			name: "handoff bind with a mismatched token",
			zone: demoZone,
			setup: func(t *testing.T, z *Zone) {
				t.Helper()
				z.players["Bound"] = &session{character: "Bound", pending: true, token: "real-token"}
			},
			msg: func(_ *Zone, out chan *playv1.ServerFrame) attachMsg {
				return attachMsg{character: "Bound", token: "wrong-token", out: out}
			},
		},
		{
			name: "re-dial to a frozen mid-handoff copy",
			zone: demoZone,
			setup: func(t *testing.T, z *Zone) {
				t.Helper()
				z.players["Frozen"] = &session{character: "Frozen", frozen: true}
			},
			msg: func(_ *Zone, out chan *playv1.ServerFrame) attachMsg {
				return attachMsg{character: "Frozen", out: out}
			},
		},
		{
			name: "handoff bind activates a pending player",
			zone: demoZone,
			setup: func(t *testing.T, z *Zone) {
				t.Helper()
				reply := make(chan error, 1)
				z.claimInboundArrival() // balanced, for the reason the re-attach row states
				z.prepare(prepareMsg{
					snap:  &handoffv1.PlayerSnapshot{CharacterId: "Arriver", Name: "Arriver"},
					room:  "darkwood:room:grove",
					epoch: 5, token: "good-token", reply: reply,
				})
				if err := <-reply; err != nil {
					t.Fatalf("premise: prepare must park a pending player, got %v", err)
				}
				if n := z.incoming.Load(); n != 0 {
					t.Fatalf("the seeding prepare left incoming = %d, want 0", n)
				}
			},
			msg: func(_ *Zone, out chan *playv1.ServerFrame) attachMsg {
				return attachMsg{character: "Arriver", token: "good-token", out: out}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := tc.zone()
			if tc.setup != nil {
				tc.setup(t, z)
			}
			out := make(chan *playv1.ServerFrame, 64) // buffered: the rejection paths send on it directly

			z.claimInboundArrival() // the bare form of the claim claimAttachTarget takes under s.mu
			if got := z.incoming.Load(); got != 1 {
				t.Fatalf("incoming after the claim = %d, want 1", got)
			}
			if z.quiescent() {
				t.Fatal("a zone with a login attach in flight to it reported quiescent (#413)")
			}

			z.attach(tc.msg(z, out))

			if got := z.incoming.Load(); got != 0 {
				t.Fatalf("incoming after attach = %d, want 0 — the claim leaked on this path and the zone "+
					"can never be unhosted, rebalanced or drained again (#413)", got)
			}
		})
	}
}

// TestPrepareReleasesItsInboundClaimOnEveryPath is the same enumeration for Zone.prepare, which has SEVEN
// early returns before its success path. Every one of them is a rejection a real peer can trigger — a stale
// epoch and an already-present character are ordinary races, and the three carry guards are the security
// checks a hostile or misconfigured peer trips deliberately — so a release at the bottom of the happy path
// would leak on exactly the inputs an attacker controls: a repeatable, remote, permanent zone wedge.
func TestPrepareReleasesItsInboundClaimOnEveryPath(t *testing.T) {
	const known = "midgaard:obj:torch" // a real demo prototype, so a rejection is the CAP and not the pack set

	bigCarry := func(n int) string {
		st := StateJSON{Inventory: make([]ItemJSON, n)}
		for i := range st.Inventory {
			st.Inventory[i] = ItemJSON{ProtoRef: known}
		}
		raw, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	tests := []struct {
		name    string
		zone    func() *Zone
		setup   func(t *testing.T, z *Zone)
		msg     prepareMsg
		wantErr bool
	}{
		{
			name: "success: a clean carry parks a pending player",
			zone: func() *Zone { return newDemoZone("midgaard", newProtoCache()) },
			msg: prepareMsg{
				snap: &handoffv1.PlayerSnapshot{CharacterId: "Clean", Name: "Clean"}, epoch: 1, token: "t",
			},
		},
		{
			name: "idempotent retry of the same Prepare",
			zone: func() *Zone { return newDemoZone("midgaard", newProtoCache()) },
			setup: func(_ *testing.T, z *Zone) {
				z.players["Retry"] = &session{character: "Retry", pending: true, token: "t", epoch: 1}
			},
			msg: prepareMsg{
				snap: &handoffv1.PlayerSnapshot{CharacterId: "Retry", Name: "Retry"}, epoch: 1, token: "t",
			},
		},
		{
			name: "stale epoch",
			zone: func() *Zone { return newDemoZone("midgaard", newProtoCache()) },
			setup: func(_ *testing.T, z *Zone) {
				z.players["Stale"] = &session{character: "Stale", epoch: 9}
			},
			msg: prepareMsg{
				snap: &handoffv1.PlayerSnapshot{CharacterId: "Stale", Name: "Stale"}, epoch: 2, token: "t",
			},
			wantErr: true,
		},
		{
			name: "character already present and live",
			zone: func() *Zone { return newDemoZone("midgaard", newProtoCache()) },
			setup: func(_ *testing.T, z *Zone) {
				z.players["Here"] = &session{character: "Here", epoch: 1}
			},
			msg: prepareMsg{
				snap: &handoffv1.PlayerSnapshot{CharacterId: "Here", Name: "Here"}, epoch: 4, token: "t",
			},
			wantErr: true,
		},
		{
			name: "no placeable room",
			zone: func() *Zone { return newZone("roomless") },
			msg: prepareMsg{
				snap: &handoffv1.PlayerSnapshot{CharacterId: "Nowhere", Name: "Nowhere"},
				room: "darkwood:room:grove", epoch: 1, token: "t",
			},
			wantErr: true,
		},
		{
			name: "carried state exceeds the byte cap",
			zone: func() *Zone { return newDemoZone("midgaard", newProtoCache()) },
			msg: prepareMsg{
				snap: &handoffv1.PlayerSnapshot{
					CharacterId: "Fat", Name: "Fat",
					StateJson: `{"applied_seq":0,"pad":"` + strings.Repeat("x", maxCarryStateBytes) + `"}`,
				},
				epoch: 1, token: "t",
			},
			wantErr: true,
		},
		{
			name: "carried item prototype unknown on this shard",
			zone: func() *Zone { return newDemoZone("midgaard", newProtoCache()) },
			msg: prepareMsg{
				snap: &handoffv1.PlayerSnapshot{
					CharacterId: "Ghosty", Name: "Ghosty",
					StateJson: `{"applied_seq":0,"inventory":[{"proto_ref":"ghost:item:doesnotexist"}]}`,
				},
				epoch: 1, token: "t",
			},
			wantErr: true,
		},
		{
			name: "carried item count exceeds the node cap",
			zone: func() *Zone { return newDemoZone("midgaard", newProtoCache()) },
			msg: prepareMsg{
				snap: &handoffv1.PlayerSnapshot{
					CharacterId: "BigBag", Name: "BigBag", StateJson: bigCarry(maxCarryItemNodes + 1),
				},
				epoch: 1, token: "t",
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := tc.zone()
			if tc.setup != nil {
				tc.setup(t, z)
			}
			m := tc.msg
			m.reply = make(chan error, 1)

			z.claimInboundArrival() // the bare form of the claim claimArrivalTarget takes under s.mu
			if z.quiescent() {
				t.Fatal("a zone with a cross-shard Prepare in flight to it reported quiescent (#413)")
			}

			z.prepare(m)

			if gotErr := <-m.reply; (gotErr != nil) != tc.wantErr {
				t.Fatalf("prepare reply = %v, wantErr = %v: the row is not exercising the branch it names",
					gotErr, tc.wantErr)
			}
			if got := z.incoming.Load(); got != 0 {
				t.Fatalf("incoming after prepare = %d, want 0 — the claim leaked on this path and the zone "+
					"can never be unhosted, rebalanced or drained again (#413)", got)
			}
		})
	}
}
