package commbus

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jetstream_test.go pins the JetStream ABSTRACTION (jetstream.go) on the MemJetStream adapter: the
// live durable consumer (backlog drain then live delivery), the consumer-side delivered cursor
// suppressing a redelivery, per-sender order, and BOUNDED REDELIVERY (a poison message parks at
// maxDeliver, never storms) — the P8-A5 guarantees slice 8.5's world drain builds on.

func drainConsumer(t *testing.T, js JetStream, subj, consumerID string, sink chan<- Message) Consumer {
	t.Helper()
	cons, err := js.Consume(subj, consumerID, func(m Message, _ bool) AckDecision {
		sink <- m
		return AckDelivered // ack
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })
	return cons
}

// TestJetStreamBacklogThenLive proves a consumer drains the BACKLOG (messages published before it
// started) in append order, then delivers LIVE messages — the durable-always model where "online" is
// just a live consumer over the same stream that held the offline backlog.
func TestJetStreamBacklogThenLive(t *testing.T) {
	js := NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	subj := DtellSubject("alice")
	ctx := context.Background()

	// Two messages land while alice is OFFLINE (no consumer yet).
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "one"}))
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 2, IdempotencyKey: "bob:2", Body: "two"}))

	sink := make(chan Message, 8)
	drainConsumer(t, js, subj, "alice", sink) // alice "logs in": her consumer drains the backlog

	assert.Equal(t, "one", recv(t, sink).Body)
	assert.Equal(t, "two", recv(t, sink).Body)

	// A LIVE message after the drain reaches her too.
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 3, IdempotencyKey: "bob:3", Body: "three"}))
	assert.Equal(t, "three", recv(t, sink).Body)
}

// TestJetStreamPublishDedup proves the publish-side idempotency-key dedup (the Nats-Msg-Id window): a
// re-publish of the same key is absorbed, so the consumer sees the tell once.
func TestJetStreamPublishDedup(t *testing.T) {
	js := NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	subj := DtellSubject("alice")
	ctx := context.Background()

	sink := make(chan Message, 8)
	drainConsumer(t, js, subj, "alice", sink)

	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "hi"}))
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "hi (dup)"}))

	assert.Equal(t, "hi", recv(t, sink).Body)
	assertNoMsg(t, sink) // the duplicate was absorbed at publish
}

// TestJetStreamPerSenderOrder proves a single sender's tells arrive in send order (P8-A3): the stream
// is one append-ordered log and the consumer drains it in order.
func TestJetStreamPerSenderOrder(t *testing.T) {
	js := NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	subj := DtellSubject("alice")
	ctx := context.Background()

	sink := make(chan Message, 64)
	drainConsumer(t, js, subj, "alice", sink)

	for i := uint64(1); i <= 20; i++ {
		require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: i, IdempotencyKey: NewIdempotencyKey("bob", i), Body: "m"}))
	}
	var last uint64
	for i := 0; i < 20; i++ {
		m := recv(t, sink)
		assert.Greater(t, m.Seq, last, "per-sender order: seq must be strictly increasing")
		last = m.Seq
	}
}

// TestJetStreamBoundedRedelivery proves a POISON message (a handler that always NAKs) is redelivered a
// BOUNDED number of times (DefaultMaxDeliver) and then PARKED — it never storms, and a GOOD message
// after it is still delivered (the poison does not block the stream forever).
func TestJetStreamBoundedRedelivery(t *testing.T) {
	js := NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	subj := DtellSubject("alice")
	ctx := context.Background()

	old := DefaultMaxDeliver
	DefaultMaxDeliver = 3
	t.Cleanup(func() { DefaultMaxDeliver = old })

	var poisonAttempts atomic.Int64
	good := make(chan Message, 4)
	cons, err := js.Consume(subj, "alice", func(m Message, _ bool) AckDecision {
		if m.Body == "poison" {
			poisonAttempts.Add(1)
			return RetryTransient // always transient-fail -> parks after DefaultMaxDeliver
		}
		good <- m
		return AckDelivered
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "poison"}))
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 2, IdempotencyKey: "bob:2", Body: "good"}))

	// The good message lands despite the poison parking — the stream is not wedged.
	assert.Equal(t, "good", recv(t, good).Body)

	// The poison was attempted EXACTLY maxDeliver times, then parked (no infinite storm). Give the
	// consumer a beat to be sure no further redelivery sneaks in.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int64(3), poisonAttempts.Load(), "poison redelivered exactly maxDeliver times then parked")
}

// TestDurableRedeliveryProdConfig pins the PRODUCTION durable-tell redelivery config against regression
// (#62, #266). AckWait is the broker's wait for a RESPONSE before redelivering a message whose handler
// neither Ack'd nor Nak'd (a hung/crashed consumer, a lost ack) — it is NOT the interval between explicit
// NAK redeliveries. An explicit NAK is paced by NakBackoff via m.NakWithDelay (#266); a bare Nak() would
// redeliver IMMEDIATELY and burn the whole budget inside a transient window, parking (losing) the tell.
// The window the schedule covers is the never-lost guarantee's actual bound, so it is asserted here.
// These tests mutate the DefaultMaxDeliver/jsAckWait package globals, so they must stay NON-parallel.
func TestDurableRedeliveryProdConfig(t *testing.T) {
	assert.Equal(t, 10, DefaultMaxDeliver, "prod durable-tell MaxDeliver must be 10 (bounded redelivery before park)")
	assert.Equal(t, 30*time.Second, jsAckWait, "prod durable-tell AckWait must be 30s (no-response redelivery timeout)")
	assert.Equal(t, []time.Duration{200 * time.Millisecond, time.Second, 3 * time.Second, 10 * time.Second, 30 * time.Second},
		NakBackoff, "prod NAK backoff schedule (ramp, then hold at the last entry)")
	// NATS constraint (err_code 10116): MaxDeliver must be STRICTLY GREATER than len(BackOff).
	assert.Greater(t, DefaultMaxDeliver, len(NakBackoff), "MaxDeliver must be > len(BackOff)")
	assert.Equal(t, NakBackoff, consumerBackoff(), "at prod MaxDeliver the whole schedule fits")
	// The covered transient window: past this a message PARKS, which is permanent loss. It must comfortably
	// outlive a cross-shard handoff (sub-second), a bus blip (seconds) and a reconnect (tens of seconds).
	assert.Equal(t, 164200*time.Millisecond, totalNakWindow(), "covered transient window before park")
	assert.Greater(t, totalNakWindow(), 2*time.Minute, "the never-lost window must outlive a realistic transient")
}

// TestNakBackoffRampsThenHolds pins the schedule function itself: attempt N earns the Nth entry, and past
// the schedule's end the LAST entry repeats (the flat tail) rather than wrapping or going to zero.
func TestNakBackoffRampsThenHolds(t *testing.T) {
	for i, want := range NakBackoff {
		assert.Equal(t, want, nakBackoff(i+1), "attempt %d earns schedule entry %d", i+1, i)
	}
	last := NakBackoff[len(NakBackoff)-1]
	assert.Equal(t, last, nakBackoff(len(NakBackoff)+1), "past the end, the last entry repeats")
	assert.Equal(t, last, nakBackoff(100), "…and keeps repeating, never wrapping to a short delay")
	assert.Equal(t, NakBackoff[0], nakBackoff(0), "a defensive 0/negative attempt clamps to the first entry")
}

// TestMemJetStreamRedeliveryIsSynchronousInOrder pins the head-of-line invariant BOTH transports now share
// (#62 wrote this as a divergence; #266 removed it). MemJetStream.deliverBounded resolves one message
// completely — every RetryTransient attempt, then park — before run() delivers the next. The real consumer
// now does the same via MaxAckPending=1, which is REQUIRED once a NAK is delayed: were a successor delivered
// while a delay-NAK'd message is still pending, it could ack and advance the world's per-sender
// delivered-cursor past the pending seq, so the redelivery would be suppressed as a duplicate and the
// message silently LOST. So the deterministic order asserted here — [stuck ×maxDeliver, then good] — is now
// the real broker's order too, not a stand-in artifact. Blocking behind a stuck message is correct: only a
// transient can block (poison is DropPoison'd out of band), and it clears on its own.
func TestMemJetStreamRedeliveryIsSynchronousInOrder(t *testing.T) {
	js := NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	subj := DtellSubject("alice")
	ctx := context.Background()

	old := DefaultMaxDeliver
	DefaultMaxDeliver = 3
	t.Cleanup(func() { DefaultMaxDeliver = old })

	var mu sync.Mutex
	var order []string
	done := make(chan struct{})
	cons, err := js.Consume(subj, "alice", func(m Message, _ bool) AckDecision {
		mu.Lock()
		order = append(order, m.Body)
		mu.Unlock()
		if m.Body == "poison" {
			return RetryTransient // always transient-fail -> parks after DefaultMaxDeliver attempts
		}
		close(done)
		return AckDelivered
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "poison"}))
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 2, IdempotencyKey: "bob:2", Body: "good"}))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the good message was never delivered")
	}

	mu.Lock()
	defer mu.Unlock()
	// The Mem divergence: the poison's 3 synchronous attempts ALL precede the good delivery.
	assert.Equal(t, []string{"poison", "poison", "poison", "good"}, order,
		"MemJetStream redelivers synchronously in-order (poison parks before good); real NATS would interleave")
}

// TestJetStreamConsumerRestartIdempotent proves the JetStream-level guarantee that a consumer restart
// resumes from its cursor: messages drained-and-acked before the restart are NOT redelivered to a new
// consumer with the SAME id. (The world adds a second belt — the character-state cursor — for the case
// a redelivery DOES occur; this test pins the stream-side half.)
func TestJetStreamConsumerRestartIdempotent(t *testing.T) {
	js := NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	subj := DtellSubject("alice")
	ctx := context.Background()

	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "one"}))

	sink := make(chan Message, 8)
	cons1, err := js.Consume(subj, "alice", func(m Message, _ bool) AckDecision { sink <- m; return AckDelivered })
	require.NoError(t, err)
	assert.Equal(t, "one", recv(t, sink).Body)
	require.NoError(t, cons1.Stop())

	// A NEW consumer with the same id (a restart) drains nothing new — the cursor advanced past "one".
	cons2, err := js.Consume(subj, "alice", func(m Message, _ bool) AckDecision { sink <- m; return AckDelivered })
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons2.Stop() })
	assertNoMsg(t, sink)
}

// TestJetStreamConcurrentPublish is a -race smoke test: concurrent durable publishes + a live consumer
// must not race, and every published message is delivered exactly once.
func TestJetStreamConcurrentPublish(t *testing.T) {
	js := NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	subj := DtellSubject("alice")
	ctx := context.Background()

	const n = uint64(100)
	var got atomic.Int64
	_, err := js.Consume(subj, "alice", func(Message, bool) AckDecision { got.Add(1); return AckDelivered })
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := uint64(1); i <= n; i++ {
		wg.Add(1)
		go func(seq uint64) {
			defer wg.Done()
			key := NewIdempotencyKey("bob", seq)
			_ = js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: seq, IdempotencyKey: key, Body: "m"})
		}(i)
	}
	wg.Wait()
	deadline := time.After(2 * time.Second)
	for got.Load() < int64(n) {
		select {
		case <-deadline:
			t.Fatalf("delivered %d of %d", got.Load(), n)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func recv(t *testing.T, ch <-chan Message) Message {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a durable message")
		return Message{}
	}
}

func assertNoMsg(t *testing.T, ch <-chan Message) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("unexpected message: %+v", m)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestJetStreamBacklogFlag proves the handler's backlog bool: messages already stored when the
// consumer STARTS are flagged backlog=true (the offline catch-up the world renders "while you were
// away…"); a message published AFTER the consumer is live is flagged backlog=false.
func TestJetStreamBacklogFlag(t *testing.T) {
	js := NewMemJetStream()
	t.Cleanup(func() { _ = js.Close() })
	subj := DtellSubject("alice")
	ctx := context.Background()

	// Two messages stored BEFORE the consumer starts (the offline backlog).
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "away1"}))
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 2, IdempotencyKey: "bob:2", Body: "away2"}))

	type flagged struct {
		body    string
		backlog bool
	}
	sink := make(chan flagged, 8)
	cons, err := js.Consume(subj, "alice", func(m Message, backlog bool) AckDecision {
		sink <- flagged{m.Body, backlog}
		return AckDelivered
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	a1 := recvFlagged(t, sink)
	a2 := recvFlagged(t, sink)
	assert.True(t, a1.backlog, "a message stored before the consumer started is backlog")
	assert.True(t, a2.backlog, "the second pre-existing message is backlog too")
	assert.Equal(t, "away1", a1.body)
	assert.Equal(t, "away2", a2.body)

	// A LIVE message after catch-up is NOT backlog.
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 3, IdempotencyKey: "bob:3", Body: "live"}))
	live := recvFlagged(t, sink)
	assert.False(t, live.backlog, "a message published after the consumer is live is not backlog")
	assert.Equal(t, "live", live.body)
}

func recvFlagged[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}
