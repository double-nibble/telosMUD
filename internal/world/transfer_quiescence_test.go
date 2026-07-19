package world

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/test/bufconn"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/directory"
)

// transfer_quiescence_test.go — #409: the intra-shard transfer window in Zone.quiescent().
//
// THE BUG. quiescent() was `pop == 0 && stashed == 0`. On the intra-shard transfer path transferOut removes
// the player from the SOURCE zone's players map and then hands the session to the destination through the
// destination's INBOX; the destination's pop only bumps when its goroutine dequeues that transferInMsg. For
// the width of that queue hop the session is in NO zone's players map and pop reads 0 on BOTH sides. A
// teardown consulting quiescent() in that window — UnhostZone, and by extension RebalanceZone's wait loop —
// passed its check, stopped the destination actor, closed z.dead, and abandoned the queued handover. The
// result is a live session attached to no room, owned by no zone, with the source's `forwarding` entry
// pointing at a stopped actor: a player who exists, holds a socket, and can never act again.
//
// THE FIX. The destination carries an `incoming` claim for every handover queued to it: Shard.claimTransferTarget
// resolves the destination AND claims the arrival in ONE hold of s.mu (the same mutex UnhostZone checks
// quiescence under), transferIn releases the claim unconditionally (deferred, so the no-rooms rejection
// releases it too), and quiescent() — hence UnhostZone, RebalanceZone's wait loop, and BeginDrain's — folds
// the claim in.
//
// The single lock hold is load-bearing, not tidiness. Claiming AFTER resolving the destination leaves the
// teardown a gap to complete in, and the claim then lands on a zone whose `post` abandons its send on z.dead:
// the handover is dropped AND the counter never comes back down. That variant is what the sweep stress below
// caught; TestClaimTransferTargetIsAtomicWithTeardown pins it deterministically.
//
// These tests pin the window from every side: the deterministic refusal (the reproduction), the atomicity of
// the claim against a teardown, WHO this shard may accept a transfer for at all (claimTransferTarget's
// refusals, and the cross-shard route a refused walker takes instead), the release on EVERY exit from
// transferIn AND from transferOut (including a panic), the underflow report from the other side, the
// concurrent sweep-vs-walk (the -race guard and the probabilistic net), and the two other consumers of
// quiescence — BeginDrain's wait-until-empty and RebalanceZone's.

// parkZoneActor stops a zone's actor goroutine between messages, deterministically and without a sleep, and
// returns the release. It works by posting a presenceMsg whose reply channel is UNBUFFERED: the handler
// answers with a blocking send, so the loop sits inside that send until the test reads the reply.
//
// The park needs no synchronization of its own. The inbox is FIFO, so anything posted after this message
// cannot be handled until the release runs — whether the zone had already dequeued the park (blocked in the
// send) or has not reached it yet (both messages still queued). Either way "the destination has not processed
// the handover" is guaranteed, which is exactly the state #409 is about.
func parkZoneActor(t *testing.T, z *Zone) (release func()) {
	t.Helper()
	reply := make(chan presence) // UNBUFFERED: the handler's send is what parks the loop
	z.post(presenceMsg{id: "park", reply: reply})
	var once sync.Once
	return func() {
		once.Do(func() {
			select {
			case <-reply:
			case <-time.After(5 * time.Second):
				t.Errorf("zone %q never reached the parking message; the actor is wedged elsewhere", z.id)
			}
		})
	}
}

// stallProducersInto opens the window the `incoming` claim actually exists for, deterministically and without
// any test hook in production code: ANY producer that has resolved dst and is about to hand it a message sits
// blocked INSIDE its send, with the claim taken and the message enqueued nowhere.
//
// It is deliberately producer-AGNOSTIC — nothing here touches a source zone — because all three producers
// have the same shape (#409/#413): the intra-shard transfer's transferOut, the login attach in server.go, and
// the cross-shard Handoff.Prepare. It works by parking dst's actor and then filling dst's inbox to CAPACITY.
// Zone.post blocks on a full inbox, so the next producer stops there, mid-flight. The fill needs no
// synchronization of its own: its last post can only complete once the actor has dequeued the parking message
// and blocked in its reply send, so on return the actor is provably parked and the inbox provably full.
// Releasing drains the fillers, and the producer's pending send then lands.
//
// This is strictly stronger than parking the destination with the arrival ALREADY QUEUED, which is what the
// drain and rebalance tests below used to do. In that shape the inbox's FIFO order alone puts the arrival
// ahead of any later reclaim, so those tests passed even against the pre-fix predicate — they documented the
// shape without pinning it. Here the arrival is in NO queue at all: the ONLY trace of the in-flight session
// anywhere in the process is the claim, so a predicate that does not consult `incoming` cannot see them, and
// the test fails against pre-fix semantics as it should.
func stallProducersInto(t *testing.T, dst *Zone) (release func()) {
	t.Helper()
	release = parkZoneActor(t, dst)
	filled := make(chan struct{})
	go func() {
		defer close(filled)
		for i := 0; i < cap(dst.inbox); i++ {
			dst.post(presenceMsg{id: "inbox-filler", reply: make(chan presence, 1)}) // buffered: never parks
		}
	}()
	select {
	case <-filled:
	case <-time.After(10 * time.Second):
		release()
		t.Fatalf("could not fill zone %q's inbox: its actor never reached the parking message", dst.id)
	}
	if got, want := len(dst.inbox), cap(dst.inbox); got != want {
		release()
		t.Fatalf("zone %q inbox len = %d, want %d (full): a post to it would not block, so the source "+
			"cannot be stalled between taking its claim and sending the handover", dst.id, got, want)
	}
	return release
}

// interceptArrival runs `trigger` (anything that resolves dst as an arrival destination and then delivers to
// it asynchronously) and INTERCEPTS the message: it returns the message itself, with dst's claim still held,
// dst's actor fully LIVE, and dst's inbox empty. The caller completes the delivery whenever it likes with
// dst.post(arrival).
//
// This is the window the fix exists for, held open for as long as a test needs it: a live session that is in
// no zone's players map and is queued NOWHERE — its entire existence recorded by the `incoming` claim and
// nothing else. Any quiescence consumer that does not read the claim must conclude the zone is empty.
//
// `want` identifies the intercepted message among the inbox fillers (everything else is discarded). `stalled`
// is the PREMISE predicate: it must become true once the producer is provably blocked in its send with the
// message enqueued nowhere. It must be CLAIM-FREE — never `dst.incoming == 1` — or the harness cannot be
// pointed at an unfixed path: it would hang here and fail by TIMEOUT rather than by assertion, and red-by-
// timeout is indistinguishable from a broken test. The claim is asserted AFTER the intercept instead, below.
//
// Why it goes to this trouble rather than simply parking dst with the arrival queued (what these tests used to
// do): a queued arrival is ordered by FIFO ahead of any later flush/reclaim the consumer posts, so the test
// passes whether or not the predicate reads the claim — it documents the shape without pinning it. And a
// destination that stays PARKED is no good either, because BeginDrain and RebalanceZone both post their flush
// and straggler-reclaim to that same inbox: a parked destination blocks them for reasons unrelated to
// quiescence, which again hides the pre-fix behavior. Handing the message to the test is what gives the window
// negative power against a live destination.
//
// The mechanics: stall the producer in its send (stallProducersInto), then drain dst's inbox from the TEST
// goroutine until the now-unblocked send lands in our hands. Releasing the park then leaves an ordinary idle
// zone.
func interceptArrival(t *testing.T, dst *Zone, want func(msg) bool, stalled func() bool, trigger func()) msg {
	t.Helper()
	release := stallProducersInto(t, dst)
	trigger()
	waitCond(t, "the producer is stalled mid-delivery (claim taken, message not yet queued)", stalled)

	var arrival msg
	deadline := time.After(10 * time.Second)
	for arrival == nil {
		select {
		case m := <-dst.inbox:
			if want(m) {
				arrival = m
			}
			// Anything else is an inbox filler (or unrelated traffic): discard it and keep making room for
			// the stalled send.
		case <-deadline:
			release()
			t.Fatalf("the stalled producer never landed its message on zone %q", dst.id)
		}
	}

	// The park is no longer needed: with the arrival in hand, the destination can run freely and still not
	// receive the session. Wait for a full round trip through its goroutine so "live and idle" is asserted, not
	// assumed, before a test starts drawing conclusions from what it does next.
	release()
	waitCond(t, "the destination drains the remaining inbox fillers", func() bool { return len(dst.inbox) == 0 })
	if _, ok := probePresent(dst, "nobody"); !ok {
		t.Fatalf("zone %q did not answer a presence probe: its actor is not running, so a test that reads "+
			"its quiescence would be measuring a wedged zone rather than an in-flight arrival", dst.id)
	}
	// THE CLAIM, asserted rather than waited on (see the `stalled` note above). This is the red line for an
	// unfixed producer: the message is in the test's hands and in no queue, so if the producer took no claim
	// this reads 0 and the zone looks empty to every quiescence consumer.
	if n := dst.incoming.Load(); n != 1 {
		t.Fatalf("zone %q incoming = %d after intercepting the arrival, want 1: the claim is the ONLY "+
			"record that a live session is on its way here (#409/#413)", dst.id, n)
	}
	if n := dst.pop.Load(); n != 0 {
		t.Fatalf("zone %q pop = %d: the arrival was delivered after all, so the window is not open", dst.id, n)
	}
	return arrival
}

// interceptTransfer is interceptArrival specialized to the intra-shard transfer producer (#409): `walk` is a
// crossing out of src, and the premise is that src's goroutine is inside transferOut past delPlayer with the
// handover not yet enqueued. The premise predicate is claim-free for the reason interceptArrival documents.
func interceptTransfer(t *testing.T, src, dst *Zone, walk func()) transferInMsg {
	t.Helper()
	m := interceptArrival(t, dst,
		func(m msg) bool { _, ok := m.(transferInMsg); return ok },
		func() bool { return src.pop.Load() == 0 && len(dst.inbox) == cap(dst.inbox) },
		walk)
	return m.(transferInMsg)
}

// newWalker builds a hand-made session with its own currentZone pointer (what a real connection has and what
// transferIn repoints) and places it in midgaard's market — the room whose north exit crosses into darkwood's
// grove, so the very next "north" is an intra-shard transfer. It returns the session and its currentZone.
func newWalker(t *testing.T, src *Zone, name string) (*session, *atomic.Pointer[Zone]) {
	t.Helper()
	cur := &atomic.Pointer[Zone]{}
	cur.Store(src)
	s := &session{
		character:   name,
		out:         make(chan *playv1.ServerFrame, 64),
		epoch:       1,
		currentZone: cur,
	}
	src.newPlayerEntity(s, name)
	placeTestPlayer(t, src, s, "midgaard:room:market")
	waitMarkup(t, s, "Market Square")
	return s, cur
}

// TestUnhostZoneRefusesADestinationWithATransferInFlight is #409's reproduction, made deterministic.
//
// The original race needed the destination's goroutine to be between "the source posted the handover" and "I
// dequeued it" at the instant UnhostZone read quiescence. Rather than hope for that interleaving, the handover
// is INTERCEPTED (interceptTransfer): the destination is live and idle, the walker is in nobody's players map
// and in no queue, and the claim is the only record that they exist. Against the old
// `pop == 0 && stashed == 0` the teardown here succeeds and the walker is orphaned.
//
// It then walks the counter all the way back down: delivering the handover lands the player (the refusal
// reason changes from "in flight" to "resident" — proving the claim was RELEASED and not merely leaked, which
// would wedge the zone against ever being unhosted), and once they log out the teardown is finally allowed.
func TestUnhostZoneRefusesADestinationWithATransferInFlight(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"}) // a peer owns darkwood: ownership is not the blocker
	defer stop()

	src := sh.zoneByID("midgaard")
	dst := sh.zoneByID("darkwood")
	walker, cur := newWalker(t, src, "Walker")

	// market.north => darkwood:grove, so this "north" is the intra-shard transfer.
	arrival := interceptTransfer(t, src, dst, func() {
		src.post(inputMsg{id: "Walker", seq: 1, line: "north"})
	})

	// THE ASSERTION. Both zones read pop 0 and neither has a parked flush, yet a live session is in flight.
	// A pre-#409 quiescent() passes the check here and tears the zone down, abandoning the handover.
	err := sh.UnhostZone(context.Background(), "darkwood")
	if err == nil || !strings.Contains(err.Error(), "not quiescent") {
		t.Fatalf("UnhostZone did not REFUSE a zone with an intra-shard transfer in flight to it (err=%v): "+
			"it passed its quiescence check, so the queued handover is abandoned and the walker is left "+
			"attached to no room and owned by no zone, with the source forwarding their input to a stopped "+
			"actor (#409)", err)
	}
	if !strings.Contains(err.Error(), "1 inbound arrival") {
		t.Fatalf("the refusal must name the in-flight transfer, got %v", err)
	}
	if sh.zoneByID("darkwood") == nil {
		t.Fatal("a refused UnhostZone must leave the zone hosted so it can still drain its own inbox")
	}

	// Deliver the intercepted handover — the send the stalled source would have made — and the claim is
	// released as the walker lands.
	dst.post(arrival)
	waitCond(t, "the walker lands in the destination", func() bool {
		return dst.pop.Load() == 1 && dst.incoming.Load() == 0 && cur.Load() == dst
	})
	waitMarkup(t, walker, "Moonlit Grove")

	// The zone is still unhostable — but now for the RESIDENT reason. A claim that leaked instead of being
	// released would report the in-flight reason forever and wedge this zone against every future teardown.
	err = sh.UnhostZone(context.Background(), "darkwood")
	if err == nil {
		t.Fatal("UnhostZone tore down a zone with a resident player")
	}
	if strings.Contains(err.Error(), "1 inbound arrival") {
		t.Fatalf("transferIn leaked its in-flight claim; the zone can never be unhosted again: %v", err)
	}
	if !strings.Contains(err.Error(), "1 resident player") {
		t.Fatalf("the refusal must name the resident, got %v", err)
	}

	// And once the walker is gone the teardown is allowed: the counter really did return to zero.
	quit(t, dst, "Walker")
	waitCond(t, "the destination becomes quiescent", dst.quiescent)
	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("a quiescent zone must be unhostable, got %v", err)
	}
}

// TestTransferInReleasesItsInFlightClaim pins the release on EVERY path out of transferIn, driven by hand
// (dequeue the message, call the handler) so no goroutine timing is involved.
//
// The no-rooms rejection is the one that matters and the reason the decrement is `defer`red as transferIn's
// FIRST statement. A destination hosting no rooms disconnects the arrival and returns EARLY; a release placed
// at the bottom of the happy path would never run, the claim would be permanent, and quiescent() would report
// false forever — turning a fix for "torn down too eagerly" into "can never be torn down". Same shape as the
// counter it sits beside: an unbalanced pop is a wedge, not a leak of memory.
func TestTransferInReleasesItsInFlightClaim(t *testing.T) {
	tests := []struct {
		name       string
		zone       func(t *testing.T) *Zone
		room       ProtoRef
		wantPop    int64
		wantPlaced bool
	}{
		{
			name: "destination places the arrival",
			zone: func(*testing.T) *Zone {
				return NewMultiShard([]string{"darkwood"}, "darkwood", "", nil, nil).zoneByID("darkwood")
			},
			room:       "darkwood:room:grove",
			wantPop:    1,
			wantPlaced: true,
		},
		{
			name:       "destination hosts no rooms and rejects the arrival",
			zone:       func(*testing.T) *Zone { return newZone("empty") },
			room:       "darkwood:room:grove",
			wantPop:    0,
			wantPlaced: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := tc.zone(t)
			s := newTestPlayerEntity(z, "Arriver")

			z.claimInboundArrival()
			z.postTransferIn(s, tc.room)
			if got := z.incoming.Load(); got != 1 {
				t.Fatalf("incoming after the claim = %d, want 1 (the claim is taken BEFORE the send, "+
					"or the destination can be torn down under the queued handover)", got)
			}
			if z.quiescent() {
				t.Fatal("a zone with a transfer in flight to it reported quiescent (#409)")
			}

			// Dequeue and run the handler exactly as the actor loop would.
			m, ok := <-z.inbox
			if !ok {
				t.Fatal("postTransferIn enqueued nothing")
			}
			tm, ok := m.(transferInMsg)
			if !ok {
				t.Fatalf("postTransferIn enqueued %T, want transferInMsg", m)
			}
			z.transferIn(tm)

			if got := z.incoming.Load(); got != 0 {
				t.Fatalf("incoming after transferIn = %d, want 0 — the claim leaked and this zone can "+
					"never be unhosted or drained again", got)
			}
			if got := z.pop.Load(); got != tc.wantPop {
				t.Fatalf("pop after transferIn = %d, want %d", got, tc.wantPop)
			}
			if got := z.quiescent(); got != !tc.wantPlaced {
				t.Fatalf("quiescent after transferIn = %v, want %v", got, !tc.wantPlaced)
			}
		})
	}
}

// TestTransferOutReleasesItsClaimWhenItPanics pins transferOut's deferred compensator: the claim move() took
// on the destination belongs to transferOut, and must be released on EVERY exit — including the one nobody
// writes deliberately.
//
// transferOut is the only place that hands a claim over to transferIn, and it does so at ONE point: the send.
// Anything that leaves the function before that point — a panic today, an early return added tomorrow —
// leaves a claim no transferIn will ever release. The zone dispatch recovers panics by design (dispatchSafe:
// a buggy command must never crash a goroutine hosting every player in the zone), so the failure is SILENT:
// the zone survives, the player is dropped, and the counter stays up forever. From then on the destination
// can never be unhosted or rebalanced and every BeginDrain on this process burns its whole deadline waiting
// for a quiescence that will never come.
//
// THE INJECTION. There is no test hook in transferOut, and adding one would put test scaffolding on the hot
// intra-shard walk path. Instead the fault is synthetic but real: the SOURCE zone's `forwarding` map is nil'd
// before its actor goroutine ever starts (so the write is ordered before any concurrent reader), which makes
// `z.forwarding[s.character] = dest` panic with "assignment to entry in nil map". That statement sits inside
// the exact window under test — after delPlayer, before the send — so any panic there has this shape. What is
// pinned is the compensator, not this particular fault.
func TestTransferOutReleasesItsClaimWhenItPanics(t *testing.T) {
	sh, stop := panicOnTransferShard(t)
	defer stop()

	src := sh.zoneByID("midgaard")
	dst := sh.zoneByID("darkwood")
	walker, _ := newWalker(t, src, "Boom")

	src.post(inputMsg{id: "Boom", seq: 1, line: "north"})

	// dispatchSafe's recovery notice: the panic fired and the zone survived it, which is the premise.
	waitMarkup(t, walker, "Something went wrong")

	// The panic landed INSIDE the window: the walker is already out of the source's players map, which
	// transferOut does after taking the claim and before the failing statement.
	if n := src.pop.Load(); n != 0 {
		t.Fatalf("midgaard pop = %d: the panic fired before transferOut reached the claim window, so this "+
			"test is not exercising the compensator", n)
	}
	if n := dst.pop.Load(); n != 0 {
		t.Fatalf("darkwood pop = %d: the handover was delivered, so nothing panicked in the window", n)
	}

	// THE ASSERTION. No polling: transferOut's deferred compensator runs during the unwind, strictly BEFORE
	// dispatchSafe's recover sends the notice already received above, so the claim is provably back down by now.
	if n := dst.incoming.Load(); n != 0 {
		t.Fatalf("darkwood incoming = %d after transferOut panicked inside its claim window, want 0: the "+
			"claim leaked and NOTHING will ever release it — only transferIn does, and no transferIn is "+
			"coming (#409)", n)
	}
	if !dst.quiescent() {
		t.Fatalf("darkwood is not quiescent after a panicked transferOut (pop=%d stashed=%d incoming=%d): "+
			"a leaked claim is PERMANENT — nothing releases it, so this zone is wedged against every future "+
			"teardown and every drain deadline (#409)", dst.pop.Load(), dst.stashed.Load(), dst.incoming.Load())
	}

	// And the wedge is proven by the primitive that would suffer it, not only by the counter.
	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("a zone whose inbound transfer panicked on the SOURCE side must still be unhostable, "+
			"got %v (#409)", err)
	}
}

// panicOnTransferShard builds the two-zone shard TestTransferOutReleasesItsClaimWhenItPanics runs on: the same
// shape as unhostShard, except midgaard's `forwarding` map is nil'd BEFORE the shard's actors start, so the
// intra-shard transfer out of midgaard panics mid-window. The nil is invisible to every other path — a read
// and a delete on a nil map are both legal no-ops, and only the assignment in transferOut writes to it.
func panicOnTransferShard(t *testing.T) (*Shard, func()) {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil).
		WithZoneLeasing(unhostLeaser{owner: "shard-b"}, "shard-a", 0, 0, nil)
	// Ordered before `go sh.Run`, so the zone goroutine that later reads this map never races the write.
	sh.zoneByID("midgaard").forwarding = nil
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sh.Run(ctx); close(done) }()
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})
	return sh, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("shard did not stop")
		}
	}
}

// TestTransferInReportsClaimUnderflow pins the OTHER end of the same wedge, for ALL THREE producers that
// release an arrival claim (#409/#413). Each handler releases unconditionally, so a producer that delivers
// without claiming first drives the counter NEGATIVE — and a negative counter never reaches zero either. The
// zone is then permanently non-quiescent: unhostable, un-rebalanceable, and a drain deadline burned in full,
// with no symptom at the point of the bug.
//
// It cannot happen through the claiming resolvers, which is the whole reason it must be LOUD: it is the
// failure mode of a future path that delivers the message directly, and a silent hang gives that author
// nothing to go on. The report must also NAME the producer — one shared releaseInboundArrival serves three
// handlers, and "some arrival was unbalanced" would leave the reader to guess which of the three.
func TestTransferInReportsClaimUnderflow(t *testing.T) {
	demoZone := func() *Zone {
		return NewMultiShard([]string{"darkwood"}, "darkwood", "", nil, nil).zoneByID("darkwood")
	}
	tests := []struct {
		name         string
		deliver      func(t *testing.T, z *Zone)
		wantProducer string
	}{
		{
			name:         "intra-shard transfer",
			wantProducer: "transfer",
			deliver: func(t *testing.T, z *Zone) {
				t.Helper()
				z.transferIn(transferInMsg{s: newTestPlayerEntity(z, "Arriver"), room: "darkwood:room:grove"})
			},
		},
		{
			name:         "login attach",
			wantProducer: "attach",
			deliver: func(t *testing.T, z *Zone) {
				t.Helper()
				z.attach(attachMsg{character: "Newbie", out: make(chan *playv1.ServerFrame, 64)})
			},
		},
		{
			name:         "cross-shard prepare",
			wantProducer: "prepare",
			deliver: func(t *testing.T, z *Zone) {
				t.Helper()
				reply := make(chan error, 1)
				z.prepare(prepareMsg{
					snap:  &handoffv1.PlayerSnapshot{CharacterId: "Wayfarer", Name: "Wayfarer"},
					room:  "darkwood:room:grove",
					epoch: 1, token: "t", reply: reply,
				})
				<-reply
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("claimed by the producer", func(t *testing.T) {
				z := demoZone()
				errs := &errorLog{}
				z.log = slog.New(errs) // set before any actor exists; this test drives the handler by hand
				z.claimInboundArrival()
				tc.deliver(t, z)
				if got := z.incoming.Load(); got != 0 {
					t.Fatalf("incoming after a CLAIMED delivery = %d, want 0", got)
				}
				if errs.contains("underflow") {
					t.Fatalf("a correctly-claimed delivery reported an underflow: %v", errs.messages())
				}
			})
			t.Run("delivered without a claim", func(t *testing.T) {
				z := demoZone()
				errs := &errorLog{}
				z.log = slog.New(errs)
				tc.deliver(t, z)
				if got := z.incoming.Load(); got != -1 {
					t.Fatalf("incoming after an UNCLAIMED delivery = %d, want -1", got)
				}
				if !errs.contains("underflow") {
					t.Fatalf("no claim-underflow report: a negative counter wedges this zone against ever "+
						"being unhosted or drained and must not be silent (#409); errors seen: %v",
						errs.messages())
				}
				// And it names WHICH producer is unbalanced. Three handlers share one release site, so a
				// report that did not say which would leave an operator to guess among all three.
				if !errs.contains(tc.wantProducer) {
					t.Fatalf("the underflow report does not name the producer %q, so an operator cannot "+
						"tell which of the three arrival paths is unbalanced; got: %v",
						tc.wantProducer, errs.messages())
				}
			})
		})
	}
}

// errorLog is a slog.Handler that records ERROR-level messages, so a test can assert that a bug the engine
// can only report (not fix) was actually reported.
type errorLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *errorLog) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= slog.LevelError }

// Handle records the message WITH its attributes rendered in. The attrs are not decoration here: the
// arrival-claim underflow report names the offending producer in an attr ("producer", "attach"), and a
// recorder that kept only the message could not tell the three apart.
func (l *errorLog) Handle(_ context.Context, r slog.Record) error {
	line := r.Message
	r.Attrs(func(a slog.Attr) bool {
		line += " " + a.Key + "=" + a.Value.String()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, line)
	return nil
}

func (l *errorLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *errorLog) WithGroup(string) slog.Handler      { return l }

func (l *errorLog) messages() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.msgs...)
}

func (l *errorLog) contains(substr string) bool {
	for _, m := range l.messages() {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

// sweepShard builds the two-zone shard the teardown-sweep stress runs on: the same shape as unhostShard, but
// with a real (EMPTY) Redis directory wired in.
//
// The directory is what makes the sweep survivable rather than fatal. When the sweeper has darkwood torn down,
// a walker's "north" out of the market stops being an intra-shard transfer and falls through to the CROSS-shard
// branch, which resolves the destination through the directory. With no directory at all that branch reaches
// s.dir.ShardForZone on a nil Locator and panics the handoff goroutine — a latent nil-deref that only a shard
// configured with cross-zone exits and no directory can reach (see the note in the review). With an empty
// directory it resolves to "destination unreachable", the handoff fails cleanly, and the zone thaws the walker
// in place — which is exactly the transient the walker's retry loop is written for.
func sweepShard(t *testing.T) (*Shard, func()) {
	t.Helper()
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
	dir := directory.NewRedis(rdb, "test") // deliberately EMPTY: nothing is registered

	unreachable := func(string) (handoffv1.HandoffClient, error) {
		return nil, fmt.Errorf("no peers in this test")
	}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", dir, unreachable).
		WithZoneLeasing(unhostLeaser{owner: "shard-b"}, "shard-a", 0, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sh.Run(ctx); close(done) }()
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})
	return sh, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("shard did not stop")
		}
	}
}

// TestClaimTransferTargetIsAtomicWithTeardown pins the CONTRACT at each of the two orders a transfer and a
// teardown may interleave in. That only these two orders are reachable is a property of the single lock hold
// and can only be caught by a race, which is the sweep stress below's job; this is what that race is a race
// BETWEEN, stated deterministically.
//
// The claim is what makes quiescence honest, but taking it just before the send is not enough — the source
// resolves the destination, and only THEN mutates itself and hands the session over. A teardown landing inside
// that gap removes the zone, closes z.dead, and the claim that follows lands on a corpse: `post` abandons the
// send, so the handover is dropped AND the counter never comes back down. That is strictly worse than no
// claim at all. Resolving and claiming in one hold of the shard mutex — the same one UnhostZone checks
// quiescence under — is what leaves only the two safe orders:
//
//   - CLAIM first: the zone is still hosted, the claim is taken, and the teardown must refuse.
//   - UNHOST first: the zone is gone from s.zones, the claim resolves nil, and the caller falls through to the
//     cross-shard path — before the source has mutated anything.
func TestClaimTransferTargetIsAtomicWithTeardown(t *testing.T) {
	t.Run("claim first: the teardown must refuse", func(t *testing.T) {
		sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
		defer stop()

		dst := sh.claimTransferTarget("darkwood")
		if dst == nil {
			t.Fatal("premise: darkwood is hosted, so the claim must resolve it")
		}
		if got := dst.incoming.Load(); got != 1 {
			t.Fatalf("incoming after claimTransferTarget = %d, want 1", got)
		}
		err := sh.UnhostZone(context.Background(), "darkwood")
		if err == nil || !strings.Contains(err.Error(), "1 inbound arrival") {
			t.Fatalf("a claimed destination must refuse teardown, got %v", err)
		}

		// Deliver + release, and the zone becomes tearable again.
		dst.postTransferIn(newTestPlayerEntity(dst, "Arriver"), "darkwood:room:grove")
		waitCond(t, "the claim is released", func() bool { return dst.incoming.Load() == 0 })
	})

	t.Run("unhost first: the claim resolves nil and is not taken", func(t *testing.T) {
		sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
		defer stop()

		dead := sh.zoneByID("darkwood")
		if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
			t.Fatalf("premise: an empty darkwood must be unhostable, got %v", err)
		}
		if got := sh.claimTransferTarget("darkwood"); got != nil {
			t.Fatal("claimTransferTarget resolved a zone this shard no longer hosts: the source would hand " +
				"the session to a stopped actor, whose `post` abandons the send (#409)")
		}
		if got := dead.incoming.Load(); got != 0 {
			t.Fatalf("a refused claim left %d on the torn-down zone's counter: nothing will ever release "+
				"it, and a re-adoption of this object could never be unhosted", got)
		}
	})
}

// TestClaimTransferTargetRefusesAZoneThisShardNoLongerOwns pins that HOSTING a zone object is not the same as
// being a legitimate destination for it, and that a refusal takes NO claim.
//
// A drain or a rebalance flips a zone's LEASE to a peer (markZoneHandedOff) BEFORE the zone drains, and this
// shard goes on hosting the object until it empties. Admitting a walker there would keep them resident — and,
// now that quiescence blocks teardown on a resident, keep THIS shard hosting and mutating a zone whose lease
// and adopted copy live on another shard. Two live writers on one zone scope is the invariant the entire
// single-writer design exists to protect, and the quiescence fix would be what pins it open.
//
// `draining` refuses one step earlier and closes a second gap: BeginDrain's wait set SKIPS local bootstrap
// zones, so a resident of one could otherwise walk into a leased zone after the drain had already flushed and
// reclaimed it — the original #409 orphan, reached down the drain path.
//
// The leak assertion is the other half of each row. A claim taken on a zone we then refuse to transfer into is
// never released by anything (only transferIn releases, and no transferIn is coming), and a permanently
// non-zero counter means that zone can never be unhosted or rebalanced again and every later BeginDrain on
// this process burns its whole deadline.
func TestClaimTransferTargetRefusesAZoneThisShardNoLongerOwns(t *testing.T) {
	tests := []struct {
		name      string
		arrange   func(*Shard)
		wantClaim bool
		why       string
	}{
		{
			name:      "hosted, owned, not draining: the claim is taken",
			arrange:   func(*Shard) {},
			wantClaim: true,
			why:       "this shard hosts and owns the zone, so the intra-shard transfer is the right route",
		},
		{
			name:    "lease handed to a peer",
			arrange: func(s *Shard) { s.markZoneHandedOff("darkwood") },
			why: "the zone's lease and adopted copy live on another shard; admitting a walker into our " +
				"leftover copy makes this shard a second live writer on that zone scope",
		},
		{
			name:    "shard draining",
			arrange: func(s *Shard) { s.mu.Lock(); s.draining = true; s.mu.Unlock() },
			why: "the drain's wait set skips local bootstrap zones, so a resident of one walking into a " +
				"leased zone would land after that zone was flushed and reclaimed",
		},
		{
			name: "draining and handed off",
			arrange: func(s *Shard) {
				s.markZoneHandedOff("darkwood")
				s.mu.Lock()
				s.draining = true
				s.mu.Unlock()
			},
			why: "neither condition may be masked by the other",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
			defer stop()

			dst := sh.zoneByID("darkwood")
			if dst == nil {
				t.Fatal("premise: darkwood must be hosted")
			}
			tc.arrange(sh)

			got := sh.claimTransferTarget("darkwood")
			switch {
			case tc.wantClaim && got != dst:
				t.Fatalf("claimTransferTarget returned %v, want the hosted zone: %s", got, tc.why)
			case !tc.wantClaim && got != nil:
				t.Fatalf("claimTransferTarget admitted a transfer into darkwood: %s (#409)", tc.why)
			}
			want := int64(0)
			if tc.wantClaim {
				want = 1
			}
			if n := dst.incoming.Load(); n != want {
				t.Fatalf("darkwood incoming = %d, want %d — a claim taken on a REFUSED destination is "+
					"never released, so the zone can never be unhosted or drained again (#409)", n, want)
			}
		})
	}
}

// TestAWalkerIntoAHandedOffZoneFollowsItCrossShard is the end-to-end behavior behind claimTransferTarget's
// handed-off refusal, driven through the real Play stream: a player must FOLLOW a zone to its new owner, not
// be pinned into the stale local copy this shard is still winding down.
//
// The setup is the mid-drain/mid-rebalance steady state: shard A still HOSTS a darkwood object, but the lease
// (and the directory's routing) has gone to shard B, which has adopted its own copy. A player standing in A's
// midgaard walks north across the zone boundary.
//
// With `handedOff` unconsulted, A's move() sees a locally-hosted darkwood and transfers the walker into it
// in-process: the player is now resident in a zone A does not own and B is simultaneously serving, and — with
// quiescence now blocking teardown on that resident — A can never finish letting the zone go. The walk must
// instead fall through to the cross-shard branch, which resolves darkwood through the directory to B.
func TestAWalkerIntoAHandedOffZoneFollowsItCrossShard(t *testing.T) {
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
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-b")) // darkwood already routes to B

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

	// A hosts BOTH zones: the darkwood copy it has not finished letting go of. B hosts the live darkwood.
	shardA := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", dir, peers)
	shardB := NewShardFromContent(lc, []string{"darkwood"}, "darkwood", "addr-b", dir, peers)
	aPlay := serveShard(t, shardA, lisA)
	bPlay := serveShard(t, shardB, lisB)
	shardA.markZoneHandedOff("darkwood") // the lease flip a drain/rebalance performs before the zone drains

	stale := shardA.ZoneByID("darkwood")
	if stale == nil {
		t.Fatal("premise: shard A must still HOST the handed-off zone; that is what makes the walk tempting")
	}

	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sA, err := aPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sA, attach("Follower"))
	recvAttached(t, sA)
	send(t, sA, inputSeq(1, "north")) // temple -> market, still inside midgaard
	send(t, sA, inputSeq(2, "north")) // market -> darkwood: the boundary under test

	// Read until the routing decision shows itself. Arriving in the grove on THIS stream without a redirect is
	// the bug, not a timeout: A transferred the walker in-process into the copy it is winding down.
	var redir *playv1.Redirect
	for redir == nil {
		f, rerr := sA.Recv()
		if rerr != nil {
			t.Fatalf("recv waiting for the walk's routing decision: %v", rerr)
		}
		if r := f.GetRedirect(); r != nil {
			redir = r
			break
		}
		if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), "Moonlit Grove") {
			t.Fatalf("the walker was transferred into shard A's STALE local darkwood instead of being "+
				"routed to shard B, which owns and serves the zone: two shards now write one zone scope, "+
				"and A can never finish letting the zone go (#409); markup=%q", o.GetMarkup())
		}
	}
	if redir.GetTargetShardAddr() != "addr-b" {
		t.Fatalf("redirect target = %q, want addr-b: the walker must follow the zone to its NEW owner "+
			"instead of being transferred into this shard's stale local copy (#409)",
			redir.GetTargetShardAddr())
	}

	// And they really land there: the redirect is a routing decision the gate completes, not a message.
	sB, err := bPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sB, attachWithToken("Follower", redir.GetHandoffToken()))
	recvAttached(t, sB)
	recvUntilOutput(t, sB, "Moonlit Grove")

	// A's leftover copy was never touched: no resident, and no claim leaked on the refused route. Either one
	// would keep A hosting — and, through quiescence, mutating — a zone whose owner is now B.
	if n := stale.pop.Load(); n != 0 {
		t.Fatalf("shard A's handed-off darkwood has %d resident player(s): a second live writer on a zone "+
			"shard B owns and serves (#409)", n)
	}
	if n := stale.incoming.Load(); n != 0 {
		t.Fatalf("shard A's handed-off darkwood holds %d inbound claim(s) after a REFUSED transfer; nothing "+
			"will release them and the zone can never be unhosted (#409)", n)
	}
	if !stale.quiescent() {
		t.Fatal("shard A's handed-off darkwood is not quiescent, so A can never finish letting it go (#409)")
	}
}

// TestUnhostSweepNeverOrphansATransferringSession is the concurrent guard, and the shape the bug actually had
// in production: a teardown sweep running against a zone that players are walking into.
//
// Several walkers bounce across the same-shard zone boundary while a sweeper hammers UnhostZone on the
// destination, re-hosting it whenever a teardown legitimately succeeds (an empty darkwood between crossings
// genuinely is unhostable — that is the point of the primitive). The sweep must never win against a session
// that is mid-crossing.
//
// A lost walker is detected DIRECTLY rather than inferred from a hang: a walker whose currentZone stops
// advancing while it is in neither zone's players map is the orphan #409 produced. Under -race this is also
// the standing guard that the claim's cross-goroutine writer (the SOURCE zone's goroutine increments a counter
// the DESTINATION's goroutine decrements, and a third goroutine reads) stays on atomics.
func TestUnhostSweepNeverOrphansATransferringSession(t *testing.T) {
	sh, stop := sweepShard(t)
	defer stop()

	src := sh.zoneByID("midgaard")
	const (
		walkers = 3
		bounces = 15
	)

	// The sweeper: tear darkwood down whenever it will let us, and put it straight back so the walkers have
	// somewhere to walk to. Errors are recorded, not asserted, from this goroutine.
	sweepStop := make(chan struct{})
	var swept, refused atomic.Int64
	hostErr := make(chan error, 1)
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		for {
			select {
			case <-sweepStop:
				return
			default:
			}
			if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
				refused.Add(1)
			} else {
				swept.Add(1)
				if _, err := sh.HostZone(context.Background(), "darkwood"); err != nil {
					select {
					case hostErr <- err:
					default:
					}
					return
				}
			}
			time.Sleep(2 * time.Millisecond) // throttle only: the loop's correctness never depends on this
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < walkers; i++ {
		name := fmt.Sprintf("Bouncer%d", i)
		s, cur := newWalker(t, src, name)
		// Keep the out channel moving so the session never sits on a full buffer (s.send is drop-on-full,
		// which would be silent, but a drained channel keeps this test honest about what the walker sees).
		frames := make(chan struct{})
		go func() {
			for {
				select {
				case <-s.out:
				case <-frames:
					return
				}
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(frames)
			for b := 0; b < bounces; b++ {
				if !crossZone(t, sh, s, cur) {
					return // crossZone already failed the test
				}
			}
		}()
	}
	wg.Wait()
	close(sweepStop)
	<-sweepDone

	select {
	case err := <-hostErr:
		t.Fatalf("the sweeper could not re-host darkwood: %v", err)
	default:
	}
	t.Logf("teardown sweep: %d succeeded, %d refused", swept.Load(), refused.Load())

	// Nothing is left in flight, on either side, once the walking stops.
	dst := sh.zoneByID("darkwood")
	if dst == nil {
		t.Fatal("darkwood is not hosted at the end of the sweep")
	}
	// A BOUNDED SETTLE FIRST, because "the walking stopped" is NOT "the last transferIn returned".
	//
	// crossZone declares a crossing complete when the walker's currentZone flips, and transferIn Stores that
	// pointer roughly two thirds of the way through its body — before registerPlacement, the arrival look
	// (which can enter Lua), the aggro scan and the prompt, and therefore well before the deferred
	// releaseInboundArrival that its RETURN runs. So the last crossing of the run is routinely still inside
	// transferIn when this assertion is reached, with its claim legitimately still held. The counter reaching
	// 0 here was relying on the sweeper's shutdown taking longer than that tail; under -race it does not
	// always, and this failed ~1 run in 50 with the message that names the #409 wedge — the worst possible
	// false positive, since it accuses the fix of the exact bug it prevents.
	//
	// The settle is a loop and NOT a waitCond, deliberately: a genuinely leaked claim never converges, and
	// reporting that as "timed out waiting for ..." would bury the finding. The loop only gives the tail room;
	// the assertions below still name what went wrong.
	settle := time.Now().Add(5 * time.Second)
	for time.Now().Before(settle) && (src.incoming.Load() != 0 || dst.incoming.Load() != 0) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := src.incoming.Load(); got != 0 {
		t.Fatalf("midgaard incoming = %d after the walk settled, want 0 (a claim leaked)", got)
	}
	if got := dst.incoming.Load(); got != 0 {
		t.Fatalf("darkwood incoming = %d after the walk settled, want 0 (a claim leaked)", got)
	}
}

// crossZone drives one intra-shard crossing for a walker and waits for the destination zone to take ownership
// (transferIn is what Stores the new currentZone). It reports whether the crossing completed.
//
// The direction is derived from the zone the walker is in RIGHT NOW rather than tracked by the caller, which
// makes the walk self-correcting: in midgaard "north" leads temple -> market -> the darkwood boundary, and in
// darkwood "south" leads hollow -> grove -> the midgaard boundary. So a duplicate move that lands after a
// crossing (the retry below can race an arrival) just walks the player one room further along the same route
// instead of desynchronizing the test.
//
// It RETRIES the move: the sweeper may have darkwood torn down at the moment the walker tries to enter, and a
// move into a zone this shard does not host is a legitimate "The way is sealed." (or a cross-shard handoff
// that fails to resolve and thaws the player in place) — the walker simply stays put and tries again. Only a
// walker that never crosses within the whole deadline is a failure, and the failure is then DIAGNOSED
// (resident where?) so an orphan is named as an orphan rather than reported as a bare timeout.
func crossZone(t *testing.T, sh *Shard, s *session, cur *atomic.Pointer[Zone]) bool {
	t.Helper()
	from := cur.Load()
	deadline := time.Now().Add(15 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		here := cur.Load()
		if here != from {
			return true
		}
		dir := "south"
		if here.id == "midgaard" {
			dir = "north"
		}
		here.post(inputMsg{id: s.character, seq: uint64(attempt) + 1, line: dir}) //nolint:gosec // monotonic test seq
		retry := time.Now().Add(20 * time.Millisecond)
		for time.Now().Before(retry) {
			if cur.Load() != from {
				return true // the destination zone took ownership: the crossing landed
			}
			time.Sleep(time.Millisecond)
		}
	}
	if host := residentIn(sh, s.character); host == "" {
		t.Errorf("walker %q left zone %q and is resident in NO zone on this shard: the queued handover was "+
			"abandoned when the teardown sweep tore the destination down mid-transfer, orphaning a live "+
			"session (#409)", s.character, from.id)
	} else {
		t.Errorf("walker %q never completed a crossing out of %q (still resident in %q)",
			s.character, from.id, host)
	}
	return false
}

// residentIn names the zone currently holding `character`, or "" if no zone on the shard does. Each zone is
// asked on its OWN goroutine (presenceMsg), so the read of z.players is race-free. A zone that cannot answer
// within zoneProbeTimeout is skipped, so this only ever reports a DEFINITE absence.
func residentIn(sh *Shard, character string) string {
	for _, z := range sh.zonesList() {
		if present, ok := probePresent(z, character); ok && present {
			return z.id
		}
	}
	return ""
}

// zoneProbeTimeout bounds the presence probe used by the stress test's orphan detector. A torn-down zone
// never drains its inbox, so an unbounded probe would hang instead of reporting.
const zoneProbeTimeout = 200 * time.Millisecond

// probePresent asks a zone's goroutine whether it holds name. ok is false when the zone did not answer within
// zoneProbeTimeout (torn down, or busy) — the caller must treat that as "unknown", never as "absent".
func probePresent(z *Zone, name string) (present, ok bool) {
	if z == nil {
		return false, false
	}
	reply := make(chan presence, 1)
	select {
	case z.inbox <- presenceMsg{id: name, reply: reply}:
	case <-z.dead:
		return false, false
	case <-time.After(zoneProbeTimeout):
		return false, false
	}
	select {
	case p := <-reply:
		return p.present, true
	case <-z.dead:
		return false, false
	case <-time.After(zoneProbeTimeout):
		return false, false
	}
}

// TestAllZonesQuiescentCountsEveryOutstandingHandover is BeginDrain's wait-until-empty predicate, pinned
// directly so the three counters that make up "this zone is finished" are covered without a timing-sensitive
// drain.
//
// The predicate used to be `sum(pop) == 0`. That is the SAME lie quiescent() told: it concludes "drained"
// while a player is in flight between two of this shard's zones, so the durable flush and the straggler
// reclaim in step 3 can be ordered ahead of the arrival — the player lands in a zone that has already been
// flushed and disconnected, and then goes down with the process, uncounted and unflushed. `stashed` was the
// same gap (RebalanceZone, the single-zone analog, has always waited for full quiescence).
func TestAllZonesQuiescentCountsEveryOutstandingHandover(t *testing.T) {
	tests := []struct {
		name     string
		pop      int64
		stashed  int64
		incoming int64
		want     bool
	}{
		{name: "nothing outstanding", want: true},
		{name: "a resident player", pop: 1},
		{name: "a parked logout flush", stashed: 1},
		{name: "an intra-shard transfer in flight", incoming: 1},
		{name: "everything at once", pop: 2, stashed: 1, incoming: 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			busy := newZone("busy")
			busy.pop.Store(tc.pop)
			busy.stashed.Store(tc.stashed)
			busy.incoming.Store(tc.incoming)
			// An idle sibling must never mask a busy zone: the predicate is an AND across the whole shard.
			zones := []*Zone{newZone("idle"), busy}

			if got := allZonesQuiescent(zones); got != tc.want {
				t.Fatalf("allZonesQuiescent(pop=%d stashed=%d incoming=%d) = %v, want %v — the drain "+
					"concludes the shard is empty while work is still outstanding (#409)",
					tc.pop, tc.stashed, tc.incoming, got, tc.want)
			}
		})
	}
}

// TestDrainAccountsForAPlayerArrivingMidTransfer is the drain's end of #409: a player in flight between two of
// the draining shard's own zones must not fall out of the drain's accounting.
//
// The walker is held in the REAL window (interceptTransfer): they have left the source, they are in no zone's
// players map, the handover is queued NOWHERE, and both zones are live and idle. The claim is the only
// evidence the player exists, so a `sum(pop) == 0` predicate reads the shard as drained the moment it looks —
// and orders the durable flush and straggler reclaim AHEAD of the arrival. The walker then lands in a zone
// that has already been flushed and disconnected and goes down with the process, uncounted and unflushed.
//
// The assertion has two halves, and the first is the one with negative power: the drain must NOT conclude its
// wait while the transfer is outstanding. Only then is the handover delivered, and the walker must still be
// reclaimed — sent the "server is restarting, reconnect" notice and counted.
func TestDrainAccountsForAPlayerArrivingMidTransfer(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
	defer stop()

	src := sh.zoneByID("midgaard")
	dst := sh.zoneByID("darkwood")
	walker, _ := newWalker(t, src, "Walker")

	arrival := interceptTransfer(t, src, dst, func() {
		src.post(inputMsg{id: "Walker", seq: 1, line: "north"})
	})

	// No target for any zone: the drain skips the ownership handover entirely (no directory involved) and
	// goes straight to waiting, flushing and reclaiming — which is the accounting under test.
	noTarget := func(string, int) (string, string, error) { return "", "", fmt.Errorf("no peer available") }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The drain's own wait deadline must be comfortably LONGER than the window this test holds open below.
	// Breaking out on the deadline is legitimate drain behavior (that is the straggler ladder); what must not
	// happen is breaking out on the PREDICATE while a transfer is in flight. Only a deadline the negative
	// assertion cannot reach tells those two apart.
	const (
		drainDeadline = 2 * time.Second
		observeFor    = 400 * time.Millisecond
	)
	resCh := make(chan DrainResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := sh.BeginDrain(ctx, noTarget, drainDeadline)
		resCh <- res
		errCh <- err
	}()
	waitCond(t, "the drain has started rejecting fresh logins", sh.isDraining)

	// NEGATIVE POWER. A drain that does not count the in-flight walker finds nothing outstanding, breaks out
	// of its wait on the FIRST evaluation, flushes, reclaims nobody, and returns here in milliseconds — while
	// a live session is still on its way into a zone it just wrote off.
	select {
	case err := <-errCh:
		res := <-resCh
		t.Fatalf("BeginDrain concluded after <%v (res=%+v err=%v) with an intra-shard transfer in flight, "+
			"far inside its own %v deadline: it read the shard as EMPTY, so its durable flush and straggler "+
			"reclaim are ordered AHEAD of the arrival and the walker goes down with the process, uncounted "+
			"and unflushed (#409)", observeFor, res, err, drainDeadline)
	case <-time.After(observeFor):
	}

	// Deliver the handover — the send the stalled source would have made — and the drain must account for the
	// player it has been waiting on.
	dst.post(arrival)
	waitCond(t, "the walker lands in the destination", func() bool {
		return dst.pop.Load() == 1 && dst.incoming.Load() == 0
	})

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("BeginDrain: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("BeginDrain never returned")
	}
	res := <-resCh
	if res.Reclaimed != 1 {
		t.Fatalf("drain result = %+v, want exactly 1 reclaimed: the walker arrived mid-drain and must be "+
			"accounted for, not silently dropped with the process (#409)", res)
	}
	// And they were told, rather than having their socket die under them.
	assertFrame(t, walker, drainReclaimNotice)
}

// assertFrame drains a session's queued frames looking for one whose markup contains substr.
func assertFrame(t *testing.T, s *session, substr string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
				return
			}
		case <-deadline:
			t.Fatalf("player %s never received %q", s.character, substr)
		}
	}
}

// TestRebalanceKeepsAZoneWithATransferInFlight pins the OTHER consumer of quiescence: RebalanceZone waits for
// z.quiescent() and then unhosts the emptied zone (#288). Both of those now see `incoming`, so a rebalance can
// no longer tear a zone down out from under a player who is walking into it from a sibling zone on the SAME
// shard.
//
// This is the one place the window is reachable without a teardown sweep: rebalances are coordinator-driven
// and routine, and the migrating zone is by definition one this shard is still serving walkers into. It uses a
// real Redis directory and a real AdoptZone over bufconn because the teardown's ownership precondition is
// answered by the directory — a fake leaser could not distinguish a correct refusal from a lucky one.
//
// Like the drain test, the walker is stalled in the REAL window (stallProducersInto): the handover is not
// queued to darkwood, so nothing but the claim records that a live session is on its way there. A rebalance
// whose wait predicate does not consult `incoming` sees an empty zone and tears it down.
func TestRebalanceKeepsAZoneWithATransferInFlight(t *testing.T) {
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
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-a"))

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
	noFence := func() {}

	shardA := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", dir, peers).
		WithZoneLeasing(dir, "shard-a", time.Second, 80*time.Millisecond, noFence)
	shardB := NewShardFromContent(lc, nil, "", "addr-b", dir, peers).
		WithZoneLeasing(dir, "shard-b", time.Second, 80*time.Millisecond, noFence)
	serveShard(t, shardA, lisA)
	serveShard(t, shardB, lisB)
	waitCond(t, "shard-a owns darkwood", func() bool {
		owner, gen, lerr := dir.ZoneLease(ctx, "darkwood")
		return lerr == nil && owner == "shard-a" && gen != 0
	})

	src := shardA.ZoneByID("midgaard")
	dst := shardA.ZoneByID("darkwood")
	walker, cur := newWalker(t, src, "Walker")

	arrival := interceptTransfer(t, src, dst, func() {
		src.post(inputMsg{id: "Walker", seq: 1, line: "north"})
	})

	// Rebalance darkwood to B while the walker is in flight to it. The ownership flip is legitimate; what must
	// not happen is the TEARDOWN at the end of the rebalance running while the handover is still outstanding.
	// It runs on its own goroutine because it must be OBSERVED not to finish.
	const rebalanceDeadline = 200 * time.Millisecond
	rebalanceErr := make(chan error, 1)
	go func() {
		_, rerr := shardA.RebalanceZone(ctx, "darkwood", "shard-b", "addr-b", rebalanceDeadline)
		rebalanceErr <- rerr
	}()

	// NEGATIVE POWER, held well past the rebalance's own wait deadline. RebalanceZone waits on z.quiescent()
	// and then unhosts what it believes is an emptied zone; with `incoming` unaccounted, both steps read this
	// zone as empty, so it is torn down (UnhostZone removes it from s.zones before it even stops the actor)
	// while a live session is on its way in. Everything the rebalance posts to darkwood along the way is
	// answered normally here — the zone is live and idle — so the ONLY thing that can hold it back is the claim.
	hold := time.Now().Add(4 * rebalanceDeadline)
	for time.Now().Before(hold) {
		if shardA.ZoneByID("darkwood") == nil {
			t.Fatal("the rebalance tore darkwood down with a transfer in flight to it: the handover is " +
				"abandoned and the walker is orphaned — attached to no room, owned by no zone (#409)")
		}
		if got := dst.incoming.Load(); got != 1 {
			t.Fatalf("darkwood incoming = %d while the walker is in flight, want 1", got)
		}
		time.Sleep(5 * time.Millisecond) // a poll interval for a NEGATIVE assertion, not a synchronization
	}

	// The zone survived, so the handover is still deliverable: deliver it and the walker lands, in the zone the
	// rebalance would have torn down under them.
	dst.post(arrival)
	waitCond(t, "the walker lands in darkwood despite the rebalance", func() bool {
		return dst.incoming.Load() == 0 && cur.Load() == dst
	})
	waitMarkup(t, walker, "Moonlit Grove")

	select {
	case rerr := <-rebalanceErr:
		if rerr != nil {
			t.Fatalf("RebalanceZone: %v", rerr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RebalanceZone never returned after the handover was delivered")
	}
}
