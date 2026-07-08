package commbus

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jetstream_nats_test.go holds the GATED integration test for the durable-tell transport against a
// REAL NATS JetStream (mirrors nats_test.go). It requires TELOS_NATS_URL (with JetStream enabled,
// e.g. `nats-server -js`) and t.Skip when unset — so a local `go test ./...` with no broker passes,
// while CI / a dev who exports the URL exercises the real PublishDurable -> durable backlog ->
// per-player consumer round-trip and the offline-then-online delivery the MemJetStream stands in for.
//
// Run it with:  TELOS_NATS_URL=nats://127.0.0.1:4222 go test ./internal/commbus/ -run JetStreamReal

// TestJetStreamRealOfflineThenOnline proves a tell published while NO consumer exists (the offline
// case) is durably stored and delivered when a per-player consumer starts (the login drain) — the
// durable-always done-when, over the real broker. Uses a unique consumer id per run so reruns don't
// collide on a left-over durable.
func TestJetStreamRealOfflineThenOnline(t *testing.T) {
	url := natsURL(t)
	js, err := NewJetStream(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.Close() })

	// A unique target per run keeps the per-target subject + durable consumer isolated across reruns.
	// It MUST be dot-free: real NATS JetStream rejects a consumer name containing "." (the subject-token
	// separator) with "invalid consumer name", and the consumer name is derived from the target. A
	// UnixNano suffix is unique and dot-free (a timestamp like "150405.000" embeds a dot and fails only
	// against real NATS — MemJetStream does not validate the name, which is why this test must run gated
	// against a real broker, not just the mem stand-in). Real player ids are dot-free (gate-enforced).
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	target := "itplayer-" + suffix
	// The author (and thus the idempotency key) is ALSO unique per run. JetStream's publish dedup is
	// STREAM-WIDE on the Nats-Msg-Id (the idempotency key), so a constant key would be suppressed on a
	// rerun against the same broker (within the dedup window) — the publish would silently no-op and the
	// consumer would time out. A per-run author keeps the dedup assertion below honest within the run
	// while staying collision-free across runs / `-count>1`. (Real authors are distinct players.)
	author := "bob-" + suffix
	subj := DtellSubject(target)
	ctx := context.Background()

	// Publish while the target is OFFLINE (no consumer yet).
	require.NoError(t, js.PublishDurable(ctx, subj, Message{
		AuthorID: author, AuthorName: "Bob", Seq: 1, IdempotencyKey: NewIdempotencyKey(author, 1), Body: "offline tell",
	}))

	// The publish-side dedup absorbs a duplicate (same Nats-Msg-Id) within the window.
	require.NoError(t, js.PublishDurable(ctx, subj, Message{
		AuthorID: author, AuthorName: "Bob", Seq: 1, IdempotencyKey: NewIdempotencyKey(author, 1), Body: "offline tell (dup)",
	}))

	got := make(chan Message, 4)
	cons, err := js.Consume(subj, target, func(m Message, _ bool) bool { got <- m; return true })
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	select {
	case m := <-got:
		assert.Equal(t, "offline tell", m.Body, "the durable tell is delivered on consumer start")
		assert.Equal(t, "Bob", m.AuthorName)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the durable offline tell")
	}
	// No duplicate (publish-side dedup).
	select {
	case m := <-got:
		t.Fatalf("unexpected duplicate delivery: %+v", m)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestJetStreamRealBoundedRedelivery confirms the PRODUCTION redelivery/park config against a REAL broker
// (#62): a durable tell whose handler always NAKs is redelivered EXACTLY DefaultMaxDeliver times then PARKED,
// never storming; and a GOOD message published alongside it is delivered PROMPTLY rather than blocked behind
// the poison. On an explicit NAK real NATS redelivers IMMEDIATELY (no BackOff is configured), so the poison
// exhausts its attempts fast; the good message still lands promptly because the consumer has MaxAckPending in
// flight and delivers it concurrently. That concurrent interleaving is the property the MemJetStream stand-in
// does NOT have (it retries synchronously in-order — see TestMemJetStreamRedeliveryIsSynchronousInOrder),
// which is exactly why this confirmation runs gated against real NATS. Shrinks MaxDeliver so the bounded run
// is quick. (AckWait is left at prod: it governs a HUNG handler, not an explicit NAK, so it is irrelevant
// here — a deliberately-immediate redelivery a live never-lost concern (#266) rests on.)
func TestJetStreamRealBoundedRedelivery(t *testing.T) {
	url := natsURL(t)
	js, err := NewJetStream(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.Close() })

	// Shrink only MaxDeliver for the test (a package var Consume reads when it builds the consumer). Restore
	// after so no sibling test sees the shrunk value.
	oldMax := DefaultMaxDeliver
	DefaultMaxDeliver = 3
	t.Cleanup(func() { DefaultMaxDeliver = oldMax })

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	target := "itpoison-" + suffix
	author := "bob-" + suffix
	subj := DtellSubject(target)
	ctx := context.Background()

	require.NoError(t, js.PublishDurable(ctx, subj, Message{
		AuthorID: author, AuthorName: "Bob", Seq: 1, IdempotencyKey: NewIdempotencyKey(author, 1), Body: "poison",
	}))
	require.NoError(t, js.PublishDurable(ctx, subj, Message{
		AuthorID: author, AuthorName: "Bob", Seq: 2, IdempotencyKey: NewIdempotencyKey(author, 2), Body: "good",
	}))

	var poisonAttempts atomic.Int64
	good := make(chan Message, 4)
	cons, err := js.Consume(subj, target, func(m Message, _ bool) bool {
		if m.Body == "poison" {
			poisonAttempts.Add(1)
			return false // always NAK
		}
		good <- m
		return true
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	// The good message is delivered PROMPTLY — the consumer delivers it concurrently with the poison's
	// (immediate) redeliveries rather than blocking behind them, unlike the mem stand-in.
	select {
	case m := <-good:
		assert.Equal(t, "good", m.Body)
	case <-time.After(5 * time.Second):
		t.Fatal("the good message was blocked behind the poison's redeliveries (real NATS should interleave)")
	}

	// The poison NAKs redeliver immediately (no BackOff), so a short settle is enough for it to exhaust
	// MaxDeliver and PARK; confirm it was attempted exactly DefaultMaxDeliver times and never storms further.
	time.Sleep(2 * time.Second)
	assert.Equal(t, int64(DefaultMaxDeliver), poisonAttempts.Load(),
		"real NATS must redeliver a NAKing tell exactly MaxDeliver times, then park")
}
