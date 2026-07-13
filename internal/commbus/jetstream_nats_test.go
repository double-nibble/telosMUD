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
	cons, err := js.Consume(subj, target, func(m Message, _ bool) AckDecision { got <- m; return AckDelivered })
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
// (#62, #266). It pins three things the stand-in cannot prove:
//
//  1. The ConsumerConfig is ACCEPTED. NATS rejects a consumer whose MaxDeliver is not STRICTLY GREATER than
//     len(BackOff) (err_code 10116) — which is why consumerBackoff() truncates the schedule to fit. This test
//     shrinks MaxDeliver to 3, so it fails outright if that truncation regresses.
//  2. A permanently-transient tell is retried EXACTLY DefaultMaxDeliver times, then PARKED — bounded, never
//     storming. (Park is permanent loss; the consumer logs it at ERROR.)
//  3. HEAD-OF-LINE ORDER under MaxAckPending=1: the good message published behind the stuck one is delivered
//     only AFTER the stuck one resolves. That blocking is REQUIRED, not incidental — with a delayed NAK, a
//     successor delivered concurrently could ack and advance the world's per-sender delivered-cursor past the
//     pending seq, so the delayed redelivery would be suppressed as a duplicate and the tell silently LOST.
//
// Point 3 is why the MemJetStream's synchronous in-order retry is no longer a divergence: both transports now
// resolve one message completely before the next. Shrinks MaxDeliver so the bounded run is quick.
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
	cons, err := js.Consume(subj, target, func(m Message, _ bool) AckDecision {
		if m.Body == "poison" {
			poisonAttempts.Add(1)
			return RetryTransient // always transient-fail -> parks after DefaultMaxDeliver
		}
		good <- m
		return AckDelivered
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	// HEAD-OF-LINE: the successor must NOT arrive while the stuck message is still pending (MaxAckPending=1).
	// It lands only once the stuck one has exhausted its budget and parked. Allow the full retry window plus
	// slack: with MaxDeliver=3 the schedule spends NakBackoff[0]+NakBackoff[1] before parking.
	deadline := totalNakWindow() + 5*time.Second
	select {
	case m := <-good:
		assert.Equal(t, "good", m.Body)
		assert.Equal(t, int64(DefaultMaxDeliver), poisonAttempts.Load(),
			"the stuck tell must have fully resolved (parked) BEFORE its successor was delivered — MaxAckPending=1")
	case <-time.After(deadline):
		t.Fatal("the successor was never delivered after the stuck message parked")
	}

	// It never storms further after parking.
	time.Sleep(500 * time.Millisecond)
	assert.Equal(t, int64(DefaultMaxDeliver), poisonAttempts.Load(),
		"real NATS must redeliver a transient-NAKing tell exactly MaxDeliver times, then park")
}

// TestJetStreamRealParkAdvisory is the gated end-to-end proof for #311: when a durable message PARKS
// (exhausts MaxDeliver), the broker publishes a MAX_DELIVERIES advisory, the parkmon (subscribed at
// NewJetStream time) receives it, and the AUTHORITATIVE park counter fires — regardless of the handler-side
// best-effort path. The MemJetStream cannot produce a broker advisory, so this behavior is provable only
// against a real NATS server (the hermetic wiring is pinned in jetstream_parkadvisory_test.go).
func TestJetStreamRealParkAdvisory(t *testing.T) {
	url := natsURL(t)

	oldMax := DefaultMaxDeliver
	DefaultMaxDeliver = 3 // shrink so the bounded run parks quickly
	t.Cleanup(func() { DefaultMaxDeliver = oldMax })

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	target := "itparkadv-" + suffix
	author := "bob-" + suffix
	subj := DtellSubject(target)
	ctx := context.Background()

	// Observe the authoritative park path. Filter to OUR consumer so a sibling test's park on the same stream
	// can't satisfy this assertion. Set BEFORE NewJetStream so the parkmon it subscribes is already observed.
	parked := make(chan uint64, 4)
	obs := parkObserver(func(stream, consumer string, seq uint64) {
		if stream == jsStreamName && consumer == target {
			parked <- seq
		}
	})
	parkAdvisoryObserver.Store(&obs)
	t.Cleanup(func() { parkAdvisoryObserver.Store(nil) })

	js, err := NewJetStream(url) // subscribes the parkmon (queue group) for COMMS_TELL
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.Close() })

	require.NoError(t, js.PublishDurable(ctx, subj, Message{
		AuthorID: author, AuthorName: "Bob", Seq: 1, IdempotencyKey: NewIdempotencyKey(author, 1), Body: "poison",
	}))

	cons, err := js.Consume(subj, target, func(_ Message, _ bool) AckDecision {
		return RetryTransient // always transient-fail -> parks after DefaultMaxDeliver
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	// The park (and thus the advisory) lands after the full bounded retry window. Allow generous slack.
	select {
	case <-parked:
		// The authoritative broker advisory counted the park — #311 done-when.
	case <-time.After(totalNakWindow() + 10*time.Second):
		t.Fatal("the MAX_DELIVERIES advisory never reached the parkmon after the message parked (#311)")
	}
}
