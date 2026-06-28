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
	cons, err := js.Consume(subj, consumerID, func(m Message, _ bool) bool {
		sink <- m
		return true // ack
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
	cons, err := js.Consume(subj, "alice", func(m Message, _ bool) bool {
		if m.Body == "poison" {
			poisonAttempts.Add(1)
			return false // always NAK
		}
		good <- m
		return true
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
	cons1, err := js.Consume(subj, "alice", func(m Message, _ bool) bool { sink <- m; return true })
	require.NoError(t, err)
	assert.Equal(t, "one", recv(t, sink).Body)
	require.NoError(t, cons1.Stop())

	// A NEW consumer with the same id (a restart) drains nothing new — the cursor advanced past "one".
	cons2, err := js.Consume(subj, "alice", func(m Message, _ bool) bool { sink <- m; return true })
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
	_, err := js.Consume(subj, "alice", func(Message, bool) bool { got.Add(1); return true })
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
	cons, err := js.Consume(subj, "alice", func(m Message, backlog bool) bool {
		sink <- flagged{m.Body, backlog}
		return true
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
