package commbus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// commbus_test.go covers the MemBus on its own (no NATS), so the substrate the Phase-8 comms tests
// depend on is itself trusted on the two load-bearing seams: the PUBLISH ACL (P8-A2, the impersonation
// gate) and PER-SUBJECT / PER-AUTHOR ordering (P8-A3), plus the disabled-bus no-op, wildcard
// subscription, and a cross-shard round-trip across two in-process worlds + gates.

// collect drains a subscription's deliveries into a slice under a mutex, returning the slice + a
// snapshot helper. A small ordered sink for the order/round-trip assertions.
type collector struct {
	mu   sync.Mutex
	msgs []Message
}

func (c *collector) handle(m Message) {
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
}

func (c *collector) snapshot() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, len(c.msgs))
	copy(out, c.msgs)
	return out
}

func (c *collector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

// waitForLen polls until the collector holds n messages or the deadline lapses (deterministic enough
// for the in-process MemBus, which delivers within microseconds).
func waitForLen(t *testing.T, c *collector, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.len() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.Failf(t, "timed out", "wanted %d messages, got %d", n, c.len())
}

// TestPublishACL is the SECURITY test (P8-A2, the impersonation gate): a GATE-role handle CANNOT
// publish on a chan/tell subject; a WORLD-role handle CAN; presence (engine mechanism) is not
// ACL-guarded for either. Table-driven over (role, subject) -> expected error.
func TestPublishACL(t *testing.T) {
	world, gate := NewWorldBus() // two handles over one shared in-process core
	defer world.Close()

	require.Equal(t, RoleWorld, world.Role())
	require.Equal(t, RoleGate, gate.Role())

	tests := []struct {
		name    string
		bus     *MemBus
		subject string
		wantErr error
	}{
		{"world may publish chan", world, ChanSubject("gossip"), nil},
		{"world may publish tell", world, TellSubject("alice"), nil},
		{"world may publish presence", world, PresenceSubject, nil},
		{"gate may NOT publish chan", gate, ChanSubject("gossip"), ErrPublishForbidden},
		{"gate may NOT publish tell", gate, TellSubject("alice"), ErrPublishForbidden},
		// presence is engine mechanism (a shard heartbeats its own residents) — NOT the impersonation
		// surface, so it is deliberately not gate-forbidden. (Phase 8.4 publishes presence from the
		// world; a gate has no reason to, but the ACL line is drawn at chan/tell.)
		{"gate may publish presence (not ACL-guarded)", gate, PresenceSubject, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.bus.Publish(context.Background(), tc.subject,
				Message{AuthorID: "system", AuthorName: "System", Body: "x"})
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

// TestGatePublishReachesNoSubscriber proves the ACL refusal happens BEFORE anything is enqueued: a
// gate's forbidden publish must not deliver to a world's subscriber on the same core (the
// impersonation gate is not merely an error return — the message never reaches the wire).
func TestGatePublishReachesNoSubscriber(t *testing.T) {
	world, gate := NewWorldBus()
	defer world.Close()

	var got collector
	_, err := world.Subscribe(ChanSubject("gossip"), got.handle)
	require.NoError(t, err)

	err = gate.Publish(context.Background(), ChanSubject("gossip"),
		Message{AuthorID: "mallory", AuthorName: "Admin", Body: "forged"})
	require.ErrorIs(t, err, ErrPublishForbidden)

	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, got.len(), "a forbidden gate publish must reach no subscriber")
}

// TestPerSubjectOrderAndAuthorSequence is the ORDERING test (P8-A3): a single subject preserves
// publish order to a subscriber, and the per-author Seq is monotonic in that order. The author/seq
// are stamped by the publisher (the engine-set discipline) — here the test plays the source world.
func TestPerSubjectOrderAndAuthorSequence(t *testing.T) {
	bus := NewMemBus()
	defer bus.Close()

	var got collector
	_, err := bus.Subscribe(ChanSubject("gossip"), got.handle)
	require.NoError(t, err)

	const n = 100
	for i := 0; i < n; i++ {
		seq := uint64(i + 1)
		msg := Message{
			AuthorID:       "alice",
			AuthorName:     "Alice",
			Seq:            seq,
			IdempotencyKey: NewIdempotencyKey("alice", seq),
			Body:           "line",
		}
		require.NoError(t, bus.Publish(context.Background(), ChanSubject("gossip"), msg))
	}
	waitForLen(t, &got, n)

	msgs := got.snapshot()
	require.Len(t, msgs, n)
	for i, m := range msgs {
		// Order preserved: the i-th delivered message is the i-th published.
		assert.Equal(t, uint64(i+1), m.Seq, "per-subject order must match publish order")
		assert.Equal(t, NewIdempotencyKey("alice", uint64(i+1)), m.IdempotencyKey)
		assert.Equal(t, "alice", m.AuthorID)
	}
	// Monotonic per-author sequence: strictly increasing across the stream.
	for i := 1; i < len(msgs); i++ {
		assert.Greater(t, msgs[i].Seq, msgs[i-1].Seq, "per-author sequence must be monotonic")
	}
}

// TestCrossShardRoundTrip is the topology done-when: a message published by SHARD-A's WORLD is
// received by SHARD-B's GATE over one MemBus — two in-process shards + gates, exactly the model the
// later Phase-8 slices run at scale. The author field is publisher-set and arrives intact.
func TestCrossShardRoundTrip(t *testing.T) {
	// One shared in-process core models the broker; each shard/gate is a handle over it.
	worldA, _ := NewWorldBus()
	defer worldA.Close()
	// Shard B's world and both gates derive sibling handles over the SAME core.
	gateB := worldA.GateHandle()

	var gotB collector
	subB, err := gateB.Subscribe(ChanSubject("gossip"), gotB.handle)
	require.NoError(t, err)
	defer subB.Unsubscribe()

	// Shard A's world publishes with an ENGINE-SET author (the publisher sets it, never the client).
	published := Message{
		AuthorID:       "alice",
		AuthorName:     "Alice the Bard",
		Seq:            1,
		IdempotencyKey: NewIdempotencyKey("alice", 1),
		Body:           "hi from shard A",
	}
	require.NoError(t, worldA.Publish(context.Background(), ChanSubject("gossip"), published))

	waitForLen(t, &gotB, 1)
	rcv := gotB.snapshot()[0]
	assert.Equal(t, "alice", rcv.AuthorID, "author id is publisher-set and arrives intact")
	assert.Equal(t, "Alice the Bard", rcv.AuthorName)
	assert.Equal(t, "hi from shard A", rcv.Body)
	assert.Equal(t, ChanSubject("gossip"), rcv.Subject, "subject is stamped for the sink's dispatch")
}

// TestWildcardSubscription proves a trailing-* subscription receives every matching subject and a
// gate dispatches on the concrete subject — the taxonomy's wildcard subscribe (telos.comms.chan.*).
func TestWildcardSubscription(t *testing.T) {
	bus := NewMemBus()
	defer bus.Close()

	var got collector
	_, err := bus.Subscribe(ChanPrefix+"*", got.handle)
	require.NoError(t, err)

	require.NoError(t, bus.Publish(context.Background(), ChanSubject("gossip"), Message{AuthorID: "a", Body: "1"}))
	require.NoError(t, bus.Publish(context.Background(), ChanSubject("newbie"), Message{AuthorID: "a", Body: "2"}))
	// A different root must NOT match the chan wildcard.
	require.NoError(t, bus.Publish(context.Background(), TellSubject("bob"), Message{AuthorID: "a", Body: "3"}))

	waitForLen(t, &got, 2)
	time.Sleep(50 * time.Millisecond) // give a stray (wrongly-matched) tell a chance to arrive
	msgs := got.snapshot()
	require.Len(t, msgs, 2, "chan.* must match both channels and NOT the tell subject")
	subjects := []string{msgs[0].Subject, msgs[1].Subject}
	assert.ElementsMatch(t, []string{ChanSubject("gossip"), ChanSubject("newbie")}, subjects)
}

// TestWildcardMatches unit-tests the matcher directly (the subset of NATS wildcards the taxonomy
// uses: exact + single trailing-token `*`; NOT mid-subject `*` or multi-token `>`).
func TestWildcardMatches(t *testing.T) {
	tests := []struct {
		pattern, subject string
		want             bool
	}{
		{"telos.comms.chan.gossip", "telos.comms.chan.gossip", true},
		{"telos.comms.chan.*", "telos.comms.chan.gossip", true},
		{"telos.comms.chan.*", "telos.comms.chan.newbie", true},
		{"telos.comms.chan.*", "telos.comms.tell.bob", false},
		{"telos.comms.chan.*", "telos.comms.chan.a.b", false}, // single token only, no further dots
		{"telos.comms.chan.*", "telos.comms.chan.", false},    // empty token does not match
		{"telos.comms.tell.*", "telos.comms.tell.alice", true},
		{"telos.comms.chan.gossip", "telos.comms.chan.newbie", false},
	}
	for _, tc := range tests {
		assert.Equalf(t, tc.want, wildcardMatches(tc.pattern, tc.subject),
			"wildcardMatches(%q,%q)", tc.pattern, tc.subject)
	}
}

// TestDisabledBusNoOp proves the NATS-down fallback (PHASE8-PLAN never-fatal rule): a Disabled bus's
// Publish/Subscribe are safe no-ops (no crash, no delivery), AND it still honors the ACL so a gate's
// forbidden publish is reported consistently whether or not the broker is up.
func TestDisabledBusNoOp(t *testing.T) {
	w := Disabled(RoleWorld)
	g := Disabled(RoleGate)

	// Subscribe is a no-op subscription; it never delivers.
	var got collector
	sub, err := w.Subscribe(ChanSubject("gossip"), got.handle)
	require.NoError(t, err)
	require.NoError(t, sub.Unsubscribe())

	// World publish on a disabled bus is a silent no-op (nil error, no delivery).
	require.NoError(t, w.Publish(context.Background(), ChanSubject("gossip"), Message{Body: "x"}))

	// The ACL still holds on a disabled gate: a chan/tell publish is forbidden; presence is allowed.
	require.ErrorIs(t, g.Publish(context.Background(), ChanSubject("gossip"), Message{Body: "x"}), ErrPublishForbidden)
	require.ErrorIs(t, g.Publish(context.Background(), TellSubject("bob"), Message{Body: "x"}), ErrPublishForbidden)
	require.NoError(t, g.Publish(context.Background(), PresenceSubject, Message{Body: "x"}))

	require.NoError(t, w.Close())
	require.NoError(t, g.Close())
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, 0, got.len(), "a disabled bus never delivers")
}

// TestOpenFallbackNeverFatal proves the wiring helpers degrade to a Disabled bus on an unreachable /
// empty broker, never returning nil and never panicking — the openContentBus discipline.
func TestOpenFallbackNeverFatal(t *testing.T) {
	var loggedDead bool
	// A dead address: OpenWorld must return a usable Disabled bus, not nil.
	bus := OpenWorld("nats://127.0.0.1:1", func(error) { loggedDead = true })
	require.NotNil(t, bus)
	assert.Equal(t, RoleWorld, bus.Role())
	assert.True(t, loggedDead, "a dead broker must be logged")
	require.NoError(t, bus.Publish(context.Background(), ChanSubject("gossip"), Message{Body: "x"}))
	require.NoError(t, bus.Close())

	// An empty URL also yields a Disabled bus (comms simply off), still role-correct + ACL-correct.
	gate := OpenGate("", nil)
	require.NotNil(t, gate)
	assert.Equal(t, RoleGate, gate.Role())
	require.ErrorIs(t, gate.Publish(context.Background(), ChanSubject("gossip"), Message{Body: "x"}), ErrPublishForbidden)
	require.NoError(t, gate.Close())
}

// TestClosedRejects mirrors contentbus: publish/subscribe after Close return ErrBusClosed (closing
// any handle closes the shared core).
func TestClosedRejects(t *testing.T) {
	world, gate := NewWorldBus()
	require.NoError(t, world.Close())
	_, err := gate.Subscribe(ChanSubject("gossip"), func(Message) {})
	assert.ErrorIs(t, err, ErrBusClosed)
	assert.ErrorIs(t, world.Publish(context.Background(), ChanSubject("gossip"), Message{}), ErrBusClosed)
}

// TestNewIdempotencyKey pins the "<authorID>:<seq>" format the 8.5 durable dedup depends on.
func TestNewIdempotencyKey(t *testing.T) {
	assert.Equal(t, "alice:1", NewIdempotencyKey("alice", 1))
	assert.Equal(t, "alice:0", NewIdempotencyKey("alice", 0))
	assert.Equal(t, "bob:1234567890", NewIdempotencyKey("bob", 1234567890))
}
