package commbus

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// jetstream_stall_test.go — #390: the EARLY WARNING that durable_parked_total is not.
//
// A park is permanent loss reported after it has already happened. A stall is the ~164s redelivery window
// before it, while the message is still recoverable — and under MaxAckPending=1 a stalled message blocks
// every later message on its consumer for that whole window. For WORLD_EVENTS, which has one consumer per
// SCOPE fleet-wide, that means a scope's orchestration is wedged.
//
// These tests are hermetic because the hook is wired into BOTH the real consumer and the in-memory
// stand-in. Wiring it only into the NATS path would have made the sole possible test broker-gated, which
// is how a threshold quietly rots.
//
// # What each layer here pins, and what it deliberately does not
//
// The seam alone is not enough, and an earlier revision of this file proved it: with only the observer
// asserted, ELEVEN mutations survived — including deleting the metric call, mislabeling the metric,
// swapping the stream and subject arguments, and removing the hook from the production NATS path
// altogether. So:
//
//   - the observer pins WHEN it fires and with WHAT arguments (stream, subject, attempt — all three, since
//     an assertion on the count alone cannot tell a subject from a stream name);
//   - TestStallRecordsTheMetric pins that the COUNTER — the thing an operator actually alerts on — is
//     recorded, via a real ManualReader rather than the seam;
//   - TestNoteStallIfCrossedFiresOnceWithStreamAndSubject pins the PRODUCTION (NATS) emitter directly;
//   - the gated TestJetStreamRealStallWarning pins that `attempt` derives from the real broker's
//     NumDelivered, which is the only thing that makes the threshold mean what it claims.

// stallRecord is one observed stall crossing, captured whole: asserting only the COUNT cannot distinguish a
// correct call from one with its stream and subject arguments swapped.
type stallRecord struct {
	stream  string
	subject string
	attempt int
}

// stallCapture accumulates stall observations. The mutex is load-bearing, not defensive: noteStall runs on
// the consumer's delivery goroutine while the test body reads. The happens-before that currently saves an
// unguarded slice is incidental (it runs through the MemJetStream's own mutex on the following noteNak),
// which is exactly the kind of accident that becomes a -race flake the first time the delivery path is
// reordered.
type stallCapture struct {
	mu   sync.Mutex
	seen []stallRecord
}

func (c *stallCapture) add(r stallRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, r)
}

func (c *stallCapture) records() []stallRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]stallRecord(nil), c.seen...)
}

// subjects returns just the observed subjects, for the tests whose property is about which MESSAGES stalled.
func (c *stallCapture) subjects() []string {
	recs := c.records()
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.subject)
	}
	return out
}

// captureStalls installs the stall test seam and returns the accumulated observations.
func captureStalls(t *testing.T) *stallCapture {
	t.Helper()
	stalls := &stallCapture{}
	obs := stallObserver(func(stream, subject string, attempt int) {
		stalls.add(stallRecord{stream: stream, subject: subject, attempt: attempt})
	})
	stallObserverPtr.Store(&obs)
	t.Cleanup(func() { stallObserverPtr.Store(nil) })
	return stalls
}

// TestStalledFiresExactlyOnceAcrossTheAttemptRange is the pure half, and it pins the property the whole
// design rests on: EQUALITY, not >=. attempt is monotonic per message, so equality crosses the threshold
// exactly once. With >= the predicate would be true for attempts 4..10 and the counter would conflate
// "one message stuck badly" with "seven messages each hiccuping" — opposite responses.
func TestStalledFiresExactlyOnceAcrossTheAttemptRange(t *testing.T) {
	fired := 0
	for attempt := 1; attempt <= DefaultMaxDeliver+3; attempt++ {
		if stalled(attempt) {
			fired++
		}
	}
	assert.Equal(t, 1, fired, "the stall predicate must be true for exactly ONE attempt in a message's life")
	assert.True(t, stalled(stallAttempt))
	assert.False(t, stalled(stallAttempt-1), "the threshold must not fire early")
	assert.False(t, stalled(stallAttempt+1), "the threshold must not fire again after it has fired")
}

// TestStallThresholdSitsInsideTheRedeliveryBudget pins that the warning is actually EARLY. A threshold at
// or past the park is dead code — it would announce a stall at the moment the loss becomes permanent,
// which is what durable_parked_total already does.
//
// Be honest about the elapsed-time bound below: it is a LOOSE guard, not the oracle that fixes the
// threshold. Against the shipping schedule it forbids a threshold of 6 or later (attempts 1..5 spend 44.2s,
// past a quarter of the 164.2s window) but tolerates 5 (14.2s). The threshold is pinned to exactly 4 by the
// bracket pair below — TestATransientThatClearsAtTheBlipBoundaryNeverStalls (kills any lower value) and
// TestAMessageStillFailingPastTheBlipWindowStalls (kills any higher one). What this test adds on top of
// those is a guard on the SCHEDULE: if NakBackoff is ever reshaped so that attempt 4 lands late in the
// window, the threshold stops being an early warning even though its numeric value never moved.
func TestStallThresholdSitsInsideTheRedeliveryBudget(t *testing.T) {
	require.Less(t, stallAttempt, DefaultMaxDeliver,
		"a stall threshold at or past MaxDeliver is dead code — the park already covers that moment")

	// And it must leave most of the window to act in, which is the point of the signal existing.
	var elapsed time.Duration
	for a := 1; a < stallAttempt; a++ {
		elapsed += nakBackoff(a)
	}
	assert.Less(t, elapsed, totalNakWindow()/4,
		"the stall must fire early in the redelivery window, not at the end of it (got %s of %s)",
		elapsed, totalNakWindow())
}

// TestOneStuckMessageStallsExactlyOnce is the anti-conflation test, driven through the real delivery loop
// rather than the predicate. A message that never succeeds is redelivered to exhaustion and parks — and
// must be counted as ONE stalled message, not one per attempt.
//
// It also pins the ARGUMENTS, which the count alone cannot: the stream label must be the stream and the
// subject must be the subject. Swapping the two arguments at the call site changes nothing observable
// about how many stalls fired, so a length-only assertion accepts a counter whose only label is a
// per-player subject — the exact high-cardinality mistake DurableStalled's doc forbids.
func TestOneStuckMessageStallsExactlyOnce(t *testing.T) {
	stalls := captureStalls(t)
	js := NewMemJetStream()

	c, err := js.Consume("player:alice", "alice", func(Message, bool) AckDecision {
		return RetryTransient // never succeeds -> redelivered to the park
	})
	require.NoError(t, err)
	defer func() { _ = c.Stop() }()

	require.NoError(t, js.PublishDurable(context.Background(), "player:alice",
		Message{AuthorID: "bob", Body: "doomed", IdempotencyKey: "bob:1"}))

	require.Eventually(t, func() bool { return len(js.Parked("alice")) == 1 }, 2*time.Second, 5*time.Millisecond,
		"precondition: the message must actually exhaust its budget and park")
	recs := stalls.records()
	require.Len(t, recs, 1,
		"a single stuck message must be counted as ONE stall, not one per redelivery — otherwise the "+
			"counter cannot distinguish one badly-stuck message from several hiccuping ones")
	assert.Equal(t, memStreamName, recs[0].stream,
		"the first argument is the STREAM label (low-cardinality); a subject here would blow up the counter's cardinality")
	assert.Equal(t, "player:alice", recs[0].subject,
		"the second argument is the SUBJECT — for WORLD_EVENTS this is the wedged scope, the whole diagnostic value of the log")
	// Frozen to a literal, deliberately NOT stallAttempt: derived from the constant it is meant to pin, this
	// assertion would move with any threshold change and pin nothing.
	assert.Equal(t, 5, recs[0].attempt, "the reported attempt must be the real delivery attempt that crossed the threshold")
}

// TestAHealthyMessageNeverStalls pins the no-false-positive half. An alert that fires on the happy path is
// an alert nobody reads.
func TestAHealthyMessageNeverStalls(t *testing.T) {
	stalls := captureStalls(t)
	js := NewMemJetStream()

	done := make(chan struct{}, 1)
	c, err := js.Consume("player:alice", "alice", func(Message, bool) AckDecision {
		done <- struct{}{}
		return AckDelivered
	})
	require.NoError(t, err)
	defer func() { _ = c.Stop() }()

	require.NoError(t, js.PublishDurable(context.Background(), "player:alice",
		Message{AuthorID: "bob", Body: "fine", IdempotencyKey: "bob:1"}))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the message was never delivered")
	}
	assert.Empty(t, stalls.records(), "a message delivered on the first attempt must never be reported as stalled")
}

// TestATransientThatClearsAtTheBlipBoundaryNeverStalls is the LOWER bracket on the threshold, and it is the
// half that a naive version of this test gets wrong.
//
// The oracle comes from NakBackoff's own documented schedule, which is frozen relative to this change:
// "a cross-shard handoff (sub-second) clears by attempt 2; a NATS/gate bus blip (seconds) by attempt 4".
// So a message whose transient is still clearing at the BLIP BOUNDARY — it fails attempts 1, 2 and 3 and
// succeeds on attempt 4 — is the redelivery budget working exactly as designed, and must not alert.
//
// The boundary matters. An earlier version of this test used a message that cleared on attempt 3, which
// looks like it pins the threshold but does not: a message acking on attempt 3 never reaches the
// RetryTransient arm on attempt 3, so a threshold of 3 emits nothing and the test stays green. Verified by
// mutation — stallAttempt 4 -> 3 survived it. Clearing on attempt 4 is the smallest case that actually
// kills a threshold of 3 (and of 2, and of 1).
func TestATransientThatClearsAtTheBlipBoundaryNeverStalls(t *testing.T) {
	stalls := captureStalls(t)
	js := NewMemJetStream()

	// Frozen at a literal, deliberately not derived from stallAttempt: a test written as `calls <
	// stallAttempt` moves with the constant it is supposed to pin.
	const clearsOnAttempt = 4
	var calls int
	done := make(chan struct{}, 1)
	c, err := js.Consume("player:alice", "alice", func(Message, bool) AckDecision {
		calls++
		if calls < clearsOnAttempt {
			return RetryTransient
		}
		done <- struct{}{}
		return AckDelivered
	})
	require.NoError(t, err)
	defer func() { _ = c.Stop() }()

	require.NoError(t, js.PublishDurable(context.Background(), "player:alice",
		Message{AuthorID: "bob", Body: "blip", IdempotencyKey: "bob:1"}))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the message never cleared")
	}
	assert.Empty(t, stalls.records(),
		"a transient that clears within the backoff schedule's own documented range must not alert — that is "+
			"the redelivery budget doing its job")
}

// TestAMessageStillFailingPastTheBlipWindowStalls is the UPPER bracket. Its partner above says a message
// clearing at the blip boundary is fine; this one says a message still failing PAST it is the incident the
// signal exists for. Together they fix the threshold at exactly one value — without either, a threshold of
// 3 or of 5 passes the rest of this file.
//
// The message here fails attempts 1..4 and succeeds on attempt 5: it never parks, so this is specifically
// the recoverable-but-stuck case the early warning is for, not a restatement of the park test.
func TestAMessageStillFailingPastTheBlipWindowStalls(t *testing.T) {
	stalls := captureStalls(t)
	js := NewMemJetStream()

	const clearsOnAttempt = 6 // frozen literal: it is still failing on delivery 5, past the documented blip range
	var calls int
	done := make(chan struct{}, 1)
	c, err := js.Consume("player:alice", "alice", func(Message, bool) AckDecision {
		calls++
		if calls < clearsOnAttempt {
			return RetryTransient
		}
		done <- struct{}{}
		return AckDelivered
	})
	require.NoError(t, err)
	defer func() { _ = c.Stop() }()

	require.NoError(t, js.PublishDurable(context.Background(), "player:alice",
		Message{AuthorID: "bob", Body: "stuck-then-clears", IdempotencyKey: "bob:1"}))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the message never cleared")
	}
	recs := stalls.records()
	require.Len(t, recs, 1,
		"a message still failing past the backoff schedule's documented transient range must raise the early warning")
	assert.Equal(t, 5, recs[0].attempt,
		"and it must raise it at the FIRST delivery past that range — a later one wastes the window the signal exists to buy")
	assert.Empty(t, js.Parked("alice"), "precondition: this message recovered, so the park path is not what was tested")
}

// TestEachStuckMessageStallsIndependently is the other side of the anti-conflation property: three stuck
// messages must read as three, not as one. Without this, an implementation that fired once per CONSUMER
// (rather than once per message) would pass every other test here.
func TestEachStuckMessageStallsIndependently(t *testing.T) {
	stalls := captureStalls(t)
	js := NewMemJetStream()

	c, err := js.Consume("player:alice", "alice", func(Message, bool) AckDecision {
		return RetryTransient
	})
	require.NoError(t, err)
	defer func() { _ = c.Stop() }()

	for _, key := range []string{"bob:1", "bob:2", "bob:3"} {
		require.NoError(t, js.PublishDurable(context.Background(), "player:alice",
			Message{AuthorID: "bob", Body: key, IdempotencyKey: key}))
	}

	require.Eventually(t, func() bool { return len(js.Parked("alice")) == 3 }, 5*time.Second, 5*time.Millisecond,
		"precondition: all three must park")
	assert.Len(t, stalls.subjects(), 3, "three independently stuck messages must read as three stalls, not one")
}

// TestPoisonNeverStalls pins that the two failure classes stay separate. A poison drop acks immediately
// and spends no redelivery budget, so it is not a stall and must never inflate the early-warning signal.
func TestPoisonNeverStalls(t *testing.T) {
	stalls := captureStalls(t)
	js := NewMemJetStream()

	c, err := js.Consume("player:alice", "alice", func(Message, bool) AckDecision {
		return DropPoison
	})
	require.NoError(t, err)
	defer func() { _ = c.Stop() }()

	require.NoError(t, js.PublishDurable(context.Background(), "player:alice",
		Message{AuthorID: "bob", Body: "bad", IdempotencyKey: "bob:1"}))

	require.Eventually(t, func() bool { return len(js.Poisoned("alice")) == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Empty(t, stalls.records(), "a poison drop spends no retry budget, so it is not a stall")
	assert.Empty(t, js.Parked("alice"), "and it is not a park either")
}

// TestNoteStallIfCrossedFiresOnceWithStreamAndSubject pins the PRODUCTION emitter, hermetically. Every other
// test in this file drives the MemJetStream, so all of them stayed green when the hook was deleted from
// jetstream_nats.go — the shipped path was, by mutation, entirely unpinned. This is the same hermetic-wiring
// / gated-behavior split TestHandleParkAdvisoryCountsAndObserves uses for #311.
//
// WORLD_EVENTS is not an arbitrary choice of stream here: it is the consumer the issue says the signal
// exists for, since one stalled scope event blocks that scope's whole orchestration under MaxAckPending=1.
func TestNoteStallIfCrossedFiresOnceWithStreamAndSubject(t *testing.T) {
	stalls := captureStalls(t)
	b := &NATSJetStream{name: "WORLD_EVENTS", log: slog.Default()}
	msg := Message{AuthorID: "bob", Seq: 7}

	for attempt := 1; attempt <= DefaultMaxDeliver; attempt++ {
		b.noteStallIfCrossed("scope.region.darkwood", "director-darkwood", msg, attempt)
	}

	recs := stalls.records()
	require.Len(t, recs, 1, "the production emitter must fire exactly once across a message's whole delivery life")
	assert.Equal(t, "WORLD_EVENTS", recs[0].stream, "the counter must be labeled with the STREAM, not the subject")
	assert.Equal(t, "scope.region.darkwood", recs[0].subject,
		"the subject must be the wedged scope — swapping these two arguments is invisible to a count-only assertion")
	assert.Equal(t, 5, recs[0].attempt)
}

// TestStallRecordsTheMetric pins the thing an operator actually alerts on. Every other test here asserts the
// test SEAM, and the seam is not the deliverable: removing metrics.DurableStalled from noteStall entirely,
// or hard-coding its stream label, left the whole suite green. This closes that by collecting from a real
// ManualReader instead — the same instrument path a scrape would take.
//
// It sets the global meter provider, so it must be the only test in this package that does; OTel's global
// delegation binds the init-created instruments once.
func TestStallRecordsTheMetric(t *testing.T) {
	rdr := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(rdr)))

	b := &NATSJetStream{name: "WORLD_EVENTS", log: slog.Default()}
	b.noteStallIfCrossed("scope.region.darkwood", "director-darkwood", Message{}, stallAttempt)

	var rm metricdata.ResourceMetrics
	require.NoError(t, rdr.Collect(context.Background(), &rm))

	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "telos.commbus.durable_stalled_total" {
				continue
			}
			found = true
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "durable_stalled_total must be an int64 sum")
			require.Len(t, sum.DataPoints, 1)
			assert.Equal(t, int64(1), sum.DataPoints[0].Value, "one crossing must record exactly one increment")
			stream, ok := sum.DataPoints[0].Attributes.Value("stream")
			require.True(t, ok, "the counter must carry a stream label — it is what an alert routes on")
			assert.Equal(t, "WORLD_EVENTS", stream.AsString(),
				"the label must be the emitting stream; a constant here makes COMMS_TELL and WORLD_EVENTS indistinguishable")
		}
	}
	require.True(t, found, "telos.commbus.durable_stalled_total was never recorded — the seam fired but the counter did not")
}
