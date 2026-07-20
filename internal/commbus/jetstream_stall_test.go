package commbus

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// captureStalls installs the stall test seam and returns the accumulated observations.
func captureStalls(t *testing.T) *[]string {
	t.Helper()
	var seen []string
	obs := stallObserver(func(_, subject string, _ int) { seen = append(seen, subject) })
	stallObserverPtr.Store(&obs)
	t.Cleanup(func() { stallObserverPtr.Store(nil) })
	return &seen
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
func TestOneStuckMessageStallsExactlyOnce(t *testing.T) {
	seen := captureStalls(t)
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
	assert.Len(t, *seen, 1,
		"a single stuck message must be counted as ONE stall, not one per redelivery — otherwise the "+
			"counter cannot distinguish one badly-stuck message from several hiccuping ones")
}

// TestAHealthyMessageNeverStalls pins the no-false-positive half. An alert that fires on the happy path is
// an alert nobody reads.
func TestAHealthyMessageNeverStalls(t *testing.T) {
	seen := captureStalls(t)
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
	assert.Empty(t, *seen, "a message delivered on the first attempt must never be reported as stalled")
}

// TestATransientThatClearsBeforeTheThresholdNeverStalls pins the threshold's actual justification: attempts
// 2 and 3 are where NakBackoff's own schedule says routine transients clear. Firing there would alert on
// the design working as intended.
func TestATransientThatClearsBeforeTheThresholdNeverStalls(t *testing.T) {
	seen := captureStalls(t)
	js := NewMemJetStream()

	// The oracle is FROZEN at a literal 3, deliberately not derived from stallAttempt. A test written as
	// `calls < stallAttempt` moves with the constant it is supposed to pin, so lowering the threshold to 2
	// would still pass it — which is precisely the vacuity this asserts against. NakBackoff's doc puts a
	// cross-shard handoff at attempt 2 and a bus blip at attempt 4, so a message clearing on attempt 3 is
	// the budget working as designed and must never alert.
	const clearsOnAttempt = 3
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
	assert.Empty(t, *seen,
		"a transient that clears within the backoff schedule's own expected range must not alert — that is "+
			"the redelivery budget doing its job")
}

// TestEachStuckMessageStallsIndependently is the other side of the anti-conflation property: three stuck
// messages must read as three, not as one. Without this, an implementation that fired once per CONSUMER
// (rather than once per message) would pass every other test here.
func TestEachStuckMessageStallsIndependently(t *testing.T) {
	seen := captureStalls(t)
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
	assert.Len(t, *seen, 3, "three independently stuck messages must read as three stalls, not one")
}

// TestPoisonNeverStalls pins that the two failure classes stay separate. A poison drop acks immediately
// and spends no redelivery budget, so it is not a stall and must never inflate the early-warning signal.
func TestPoisonNeverStalls(t *testing.T) {
	seen := captureStalls(t)
	js := NewMemJetStream()

	c, err := js.Consume("player:alice", "alice", func(Message, bool) AckDecision {
		return DropPoison
	})
	require.NoError(t, err)
	defer func() { _ = c.Stop() }()

	require.NoError(t, js.PublishDurable(context.Background(), "player:alice",
		Message{AuthorID: "bob", Body: "bad", IdempotencyKey: "bob:1"}))

	require.Eventually(t, func() bool { return len(js.Poisoned("alice")) == 1 }, 2*time.Second, 5*time.Millisecond)
	assert.Empty(t, *seen, "a poison drop spends no retry budget, so it is not a stall")
	assert.Empty(t, js.Parked("alice"), "and it is not a park either")
}
