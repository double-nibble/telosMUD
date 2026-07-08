package world

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
)

// comms_chaos_test.go is W8 failure-injection for the comms boundary: the world publishes channel lines
// over a bus that can FAIL mid-session, and the zone must DEGRADE, never die (the PHASE8 "never-fatal"
// rule). The only failure double shipped before was the disabled no-op bus (Publish returns nil), so the
// `if err := bus.Publish(...); err != nil` branches (channel.go, the "Channels are temporarily offline."
// path) were unreachable in tests. flakyBus closes that gap by returning a real error on demand.

// flakyBus wraps a real Bus and returns an injected error from Publish while `fail` is set. Role,
// Subscribe, and Close delegate to the inner bus, so a test can flip the publish path between healthy
// and failing on a LIVE wiring (the transient-broker-outage shape) without rebuilding the shard.
type flakyBus struct {
	inner commbus.Bus
	fail  atomic.Bool // hard-fail EVERY publish while set
	// failTellN fails the next N publishes to a TELL subject (telos.comms.tell.*) then auto-recovers.
	// This models a delivery-time broker blip scoped to tells, so a redelivery test can fail the first
	// emit attempt and let the bounded-redelivery loop succeed on retry without racing a manual flag.
	failTellN atomic.Int64
	// failSubject, when set, fails publishes to EXACTLY that subject (and counts them). Models a blip
	// scoped to one target's subject — used to fail an AFK auto-reply (published to the SENDER's tell
	// subject) while the primary tell to the TARGET's subject still succeeds, proving the auto-reply is
	// best-effort and its failure never loses the tell (#62).
	failSubject     atomic.Pointer[string]
	failSubjectHits atomic.Int64
}

func (b *flakyBus) Role() commbus.Role { return b.inner.Role() }

func (b *flakyBus) Publish(ctx context.Context, subj string, msg commbus.Message) error {
	if b.fail.Load() {
		return errors.New("injected: comms bus publish failure")
	}
	if fs := b.failSubject.Load(); fs != nil && subj == *fs {
		b.failSubjectHits.Add(1)
		return errors.New("injected: comms bus publish failure (targeted subject)")
	}
	if strings.HasPrefix(subj, commbus.TellPrefix) && b.failTellN.Load() > 0 {
		b.failTellN.Add(-1)
		return errors.New("injected: comms bus tell-emit failure")
	}
	return b.inner.Publish(ctx, subj, msg)
}

func (b *flakyBus) Subscribe(subj string, handler func(commbus.Message)) (commbus.Subscription, error) {
	return b.inner.Subscribe(subj, handler)
}

func (b *flakyBus) Available() bool { return b.inner.Available() }

func (b *flakyBus) Close() error { return b.inner.Close() }

// TestZoneSurvivesCommsBusPublishFailure pins the never-fatal contract: when a channel publish fails
// (a closed/unreachable broker mid-session), the speaker is told comms are offline, the ZONE KEEPS
// SERVING, and when the bus recovers, publishing resumes with no lingering offline notice. This is the
// player-facing degradation path (channel.go's err branch) that the no-op disabled bus could never
// exercise — proven by injecting a real publish error and recovering from it on a live zone.
func TestZoneSurvivesCommsBusPublishFailure(t *testing.T) {
	wbus, _ := commbus.NewWorldBus()
	t.Cleanup(func() { _ = wbus.Close() })
	flaky := &flakyBus{inner: wbus}
	sh := NewDemoShard().WithComms(flaky)
	z := sh.Zone()

	alice := newTestPlayerEntity(z, "Alice")
	Move(alice.entity, z.rooms[z.startRoom]) // place her in the start room so `look` (the serves-probe) renders

	// drain returns every markup line queued to alice since the last drain (non-blocking — dispatch is
	// synchronous here, so all sends are already enqueued when it returns).
	drain := func() []string { return drainCombat(alice) }
	has := func(lines []string, sub string) bool {
		for _, l := range lines {
			if strings.Contains(l, sub) {
				return true
			}
		}
		return false
	}

	// 1. HEALTHY: a gossip publishes fine — no offline notice.
	flaky.fail.Store(false)
	z.dispatch(alice, "gossip hello")
	if has(drain(), "temporarily offline") {
		t.Fatal("got an offline notice while the bus was healthy")
	}

	// 2. BUS FAILS mid-session: the next gossip's publish errors. The speaker is told comms are offline.
	flaky.fail.Store(true)
	z.dispatch(alice, "gossip into the void")
	if !has(drain(), "Channels are temporarily offline.") {
		t.Fatal("a failed channel publish did not produce the graceful offline notice")
	}

	// 3. THE ZONE STILL SERVES after the bus failure — a normal command round-trips (never-fatal).
	z.dispatch(alice, "look")
	if !has(drain(), "Exits:") {
		t.Fatal("the zone stopped serving a normal command after a comms-bus publish failure")
	}

	// 4. RECOVERY: the bus comes back; gossip publishes again with no lingering offline notice.
	flaky.fail.Store(false)
	z.dispatch(alice, "gossip back online")
	if has(drain(), "temporarily offline") {
		t.Fatal("the offline notice persisted after the bus recovered")
	}
}

// TestDurableTellNotLostOnEmitFailure pins the ORDERING contract inside deliverDrainedTell that MAKES
// "never lose the tell" safe: when the render/emit to the gate FAILS (a broker blip at delivery time),
// deliverDrainedTell must NAK (return false) AND must NOT advance the per-sender delivered-cursor — the
// cursor advance lives strictly AFTER the successful emit (tell.go), so a failed emit leaves the cursor
// untouched and a later redelivery is NOT suppressed-as-already-delivered (which would silently lose the
// tell). The test re-presents the same message DIRECTLY (a tellDeliverMsg post) to model what a
// redelivery feeds back in; it deliberately bypasses the Consume/deliverBounded redelivery MACHINERY
// (that bounded-retry loop is a separate concern — the Consume-driven end-to-end recover-within-maxDeliver
// path is TestDurableTellRedeliversWithinMaxDeliver, and the MemJetStream park-at-maxDeliver divergence from
// real NATS is pinned in commbus.TestMemJetStreamRedeliveryIsSynchronousInOrder +
// TestJetStreamRealBoundedRedelivery, #62). What is pinned here is the cursor-after-emit ordering, the
// actual safety invariant.
func TestDurableTellNotLostOnEmitFailure(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	flaky := &flakyBus{inner: core.WorldHandle()}
	z := tellShard(t, flaky, js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")

	joinTellPlayer(t, z, "Alice")
	bob := joinTellPlayer(t, z, "Bob") // starts Bob's real Consume consumer; the stream is empty, so it
	// delivers nothing and never races the direct tellDeliverMsg posts below (which intentionally
	// bypass the live consumer to drive the deliverDrainedTell ordering in isolation).

	cursor := func() uint64 {
		done := make(chan uint64, 1)
		z.post(tellCursorProbeMsg{id: "Bob", author: "Alice", reply: done})
		return <-done
	}
	deliver := func() bool {
		ack := make(chan bool, 1)
		z.post(tellDeliverMsg{target: "Bob", msg: commbus.Message{
			AuthorID: "Alice", AuthorName: "Alice", Seq: 1, Body: "do not lose me",
		}, ack: ack})
		return <-ack
	}

	// FAILURE at delivery: the emit errors → NAK (false), cursor NOT advanced, nothing reaches the gate.
	flaky.fail.Store(true)
	if deliver() {
		t.Fatal("a failed tell emit must NAK (false) — it ACKed, so a redelivery would be suppressed and the tell lost")
	}
	if c := cursor(); c != 0 {
		t.Fatalf("the delivered-cursor advanced to %d on a FAILED emit — a redelivery would now be suppressed and the tell lost", c)
	}
	assertNoTell(t, bobInbox) // nothing was emitted to the gate

	// RECOVERY: redeliver the SAME tell. Now it emits once, the gate renders it, and the cursor advances.
	flaky.fail.Store(false)
	if !deliver() {
		t.Fatal("the recovered redelivery should ACK (true)")
	}
	if m := recvTell(t, bobInbox); !strings.Contains(m.Body, "do not lose me") {
		t.Fatalf("recovered tell not rendered to the gate: %q", m.Body)
	}
	waitTellCursor(t, z, bob, "Alice", 1) // the cursor advanced exactly once, post-recovery
}

// TestDurableTellRedeliversWithinMaxDeliver is the END-TO-END never-lost pin the distsys review asked
// for: it drives a real `tell` through the full durable path — PublishDurable → the target's live
// Consume consumer → routeTellDeliver → deliverDrainedTell — with the FIRST emit attempt failing, and
// asserts the bounded-redelivery loop (DefaultMaxDeliver=5) RETRIES and delivers the tell EXACTLY ONCE.
// Unlike TestDurableTellNotLostOnEmitFailure (which re-presents the message directly), this exercises
// the actual Consume/deliverBounded redelivery machinery — a NAK-then-recover within the window must
// self-heal, never lose and never duplicate the tell.
func TestDurableTellRedeliversWithinMaxDeliver(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	flaky := &flakyBus{inner: core.WorldHandle()}
	z := tellShard(t, flaky, js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")

	joinTellPlayer(t, z, "Alice")
	bob := joinTellPlayer(t, z, "Bob")

	// Fail exactly the FIRST tell emit; the bounded-redelivery loop must retry (attempt 2) and succeed —
	// 1 < DefaultMaxDeliver (5), so the message redelivers rather than parks.
	flaky.failTellN.Store(1)

	z.post(inputMsg{id: "Alice", line: "tell Bob arrive once"})

	if m := recvTell(t, bobInbox); !strings.Contains(m.Body, "arrive once") {
		t.Fatalf("tell not delivered via the redelivery loop: %q", m.Body)
	}
	assertNoTell(t, bobInbox)             // EXACTLY once — the failed first attempt emitted nothing, no dup
	waitTellCursor(t, z, bob, "Alice", 1) // cursor advanced once, after the successful retry
	if n := flaky.failTellN.Load(); n != 0 {
		t.Fatalf("the injected tell-emit failure was never consumed (%d left) — the redelivery path was not exercised", n)
	}
}

// TestAFKAutoReplyFailureDoesNotLoseTell pins the BEST-EFFORT contract of the AFK auto-reply (#62): a live
// tell to an AFK target both delivers the tell AND fires a one-line "X is AFK: …" back to the sender. The
// auto-reply is a separate publish whose result is deliberately ignored (tell.go) — so if IT fails (a blip on
// the sender's subject), the PRIMARY tell must still be delivered exactly once and its per-sender cursor must
// still advance. If the auto-reply failure leaked into the deliver result it would NAK the tell (redelivery /
// duplicate) or block the cursor. We fail only the auto-reply's subject (the sender's) and confirm the tell
// is unaffected.
func TestAFKAutoReplyFailureDoesNotLoseTell(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	js := commbus.NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	dir := newFakeLocator("Alice", "Bob")

	flaky := &flakyBus{inner: core.WorldHandle()}
	z := tellShard(t, flaky, js, dir)
	bobInbox := subscribeTell(t, core.GateHandle(), "Bob")

	joinTellPlayer(t, z, "Alice")
	bob := joinTellPlayer(t, z, "Bob")

	// Bob goes AFK (drives the real `afk` command on the zone goroutine — no state race).
	z.post(inputMsg{id: "Bob", line: "afk lunch"})

	// Fail ONLY the auto-reply, which publishes to the SENDER's (Alice's) tell subject. The primary tell to
	// Bob's subject is untouched.
	replySubj := commbus.TellSubject("Alice")
	flaky.failSubject.Store(&replySubj)

	z.post(inputMsg{id: "Alice", line: "tell Bob you there?"})

	// The primary tell is delivered to Bob exactly once despite the auto-reply failing.
	if m := recvTell(t, bobInbox); !strings.Contains(m.Body, "you there?") {
		t.Fatalf("primary tell not delivered: %q", m.Body)
	}
	assertNoTell(t, bobInbox) // exactly once — no redelivery from a leaked auto-reply failure
	// The primary tell's delivered-cursor advanced despite the auto-reply failure. The cursor lives on the
	// RECIPIENT's session (Bob) keyed by the SENDER (Alice); a leaked auto-reply error would have NAK'd the
	// tell and left it unadvanced (Seq 0).
	waitTellCursor(t, z, bob, "Alice", 1)

	// The auto-reply WAS attempted (and failed) — proving we exercised the best-effort branch, not skipped it.
	if hits := flaky.failSubjectHits.Load(); hits == 0 {
		t.Fatal("the AFK auto-reply was never attempted — the best-effort failure path was not exercised")
	}
}

// droppyBus wraps a Bus and, while dropDeliveries is set, silently DROPS messages on the SUBSCRIBE (delivery)
// side — the handler is never invoked, though the publish itself succeeded. This is the complement of
// flakyBus, which only fails the PUBLISH side (#62): it models a delivery-side outage (the transport loses a
// message after a successful publish, or a subscriber that falls behind and misses a transient line). Publish
// and the rest delegate to the inner bus.
type droppyBus struct {
	inner          commbus.Bus
	dropDeliveries atomic.Bool
	dropped        atomic.Int64
}

func (b *droppyBus) Role() commbus.Role { return b.inner.Role() }

func (b *droppyBus) Publish(ctx context.Context, subj string, msg commbus.Message) error {
	return b.inner.Publish(ctx, subj, msg)
}

func (b *droppyBus) Subscribe(subj string, handler func(commbus.Message)) (commbus.Subscription, error) {
	return b.inner.Subscribe(subj, func(m commbus.Message) {
		if b.dropDeliveries.Load() {
			b.dropped.Add(1)
			return // delivery dropped: the subscriber never sees this message
		}
		handler(m)
	})
}

func (b *droppyBus) Available() bool { return b.inner.Available() }

func (b *droppyBus) Close() error { return b.inner.Close() }

// TestSubscribeSideDeliveryDrop exercises the subscribe-side failure double (#62): a TRANSIENT channel line
// published while the subscriber is dropping deliveries is silently MISSED (transient comms are fire-and-
// forget — a missed line is acceptable degradation, never a crash or a hang), and once the drop clears the
// subscriber receives subsequent lines normally. This is the delivery-side path flakyBus (publish-only)
// could not reach.
func TestSubscribeSideDeliveryDrop(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })

	// The world is the comms SOURCE (a player's `gossip` publishes a channel line); the gate is the SINK
	// (subscribes + renders). Wrap the GATE's subscription in droppyBus so we can drop DELIVERIES to it while
	// the world keeps publishing — the delivery-side outage flakyBus (publish-only) cannot model.
	droppy := &droppyBus{inner: core.GateHandle()}
	got := make(chan commbus.Message, 8)
	sub, err := droppy.Subscribe(commbus.ChanSubject("gossip"), func(m commbus.Message) { got <- m })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// A live zone driving the REAL channel-publish path (channel.go), publishing over the paired world handle.
	sh := NewDemoShard().WithComms(core.WorldHandle())
	z := sh.Zone()
	alice := newTestPlayerEntity(z, "Alice")
	Move(alice.entity, z.rooms[z.startRoom])
	drain := func() []string { return drainCombat(alice) }
	has := func(lines []string, sub string) bool {
		for _, l := range lines {
			if strings.Contains(l, sub) {
				return true
			}
		}
		return false
	}

	// DROP armed: Alice gossips. The world's publish SUCCEEDS (so she gets NO "temporarily offline" notice —
	// this is a delivery-side loss, not a publish failure), but the line is dropped at the gate subscriber.
	droppy.dropDeliveries.Store(true)
	z.dispatch(alice, "gossip lost line")
	if has(drain(), "temporarily offline") {
		t.Fatal("a subscribe-side drop wrongly produced a publish-side offline notice")
	}
	select {
	case m := <-got:
		t.Fatalf("a dropped delivery still reached the gate subscriber: %q", m.Body)
	case <-time.After(150 * time.Millisecond):
		// correct: the transient line was silently missed (fire-and-forget)
	}
	if n := droppy.dropped.Load(); n == 0 {
		t.Fatal("the delivery-drop double never dropped anything")
	}

	// THE ZONE STILL SERVES after a subscriber lost a line — a normal command round-trips (never-fatal).
	z.dispatch(alice, "look")
	if !has(drain(), "Exits:") {
		t.Fatal("the zone stopped serving after a subscribe-side delivery drop")
	}

	// DROP cleared: a subsequent gossip reaches the gate subscriber (recovered, no wedge).
	droppy.dropDeliveries.Store(false)
	z.dispatch(alice, "gossip back online")
	select {
	case m := <-got:
		if !strings.Contains(m.Body, "back online") {
			t.Fatalf("got %q, want the recovered gossip line", m.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the gate subscriber did not recover after the delivery drop cleared")
	}
}
