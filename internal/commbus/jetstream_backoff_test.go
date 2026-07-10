package commbus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jetstream_backoff_test.go — #266. A transient NAK must redeliver on the NakBackoff SCHEDULE, not
// immediately; a poison message must be dropped without spending the transient budget; and an exhausted
// message must PARK (permanent loss) rather than vanish silently. The stand-in records the schedule it
// would have waited (it never sleeps it — wall time is not the property under test), so these assertions
// are exact and hermetic. This is the model #62 taught us to keep honest: the old double encoded "NAK
// redelivers with backoff spaced by AckWait", a false timing model that hid this very bug.

// TestDurableConsumerConfigPinsMaxAckPending is the HERMETIC guard on the single most load-bearing line of
// the #266 fix. MaxAckPending=1 is not tuning: a delayed NAK plus a pipelined successor lets the successor
// ack and advance the world's per-sender delivered-cursor past the pending message, so the delayed
// redelivery is suppressed as a duplicate and the message is silently LOST. Deleting that line reintroduces
// exactly the loss the backoff was added to prevent — and no stand-in can catch it (the MemJetStream is
// single-goroutine, so it blocks head-of-line whatever the real config says). Only the broker-gated tier
// observed it before, which left the default CI blind. So we pin the CONFIG itself.
func TestDurableConsumerConfigPinsMaxAckPending(t *testing.T) {
	cfg := durableConsumerConfig(DtellSubject("alice"), "alice")
	assert.Equal(t, 1, cfg.MaxAckPending,
		"MaxAckPending must be 1: a successor must never overtake a delay-NAK'd message and advance the cursor past it")
	assert.Equal(t, DefaultMaxDeliver, cfg.MaxDeliver)
	assert.Equal(t, consumerBackoff(), cfg.BackOff)
	assert.Equal(t, jsAckWait, cfg.AckWait)
	assert.Greater(t, cfg.MaxDeliver, len(cfg.BackOff), "NATS err_code 10116: MaxDeliver must exceed len(BackOff)")
}

// TestConsumerBackoffFitsMaxDeliver pins the NATS constraint the real broker enforces (err_code 10116:
// "max deliver is required to be > length of backoff values"). A schedule longer than the budget must be
// TRUNCATED, not sent whole — otherwise CreateOrUpdateConsumer fails and the player's tells go silently
// undelivered. The gated real-NATS test caught exactly this when a test shrank DefaultMaxDeliver to 3.
func TestConsumerBackoffFitsMaxDeliver(t *testing.T) {
	old := DefaultMaxDeliver
	t.Cleanup(func() { DefaultMaxDeliver = old })

	for _, tc := range []struct {
		maxDeliver int
		want       []time.Duration
		why        string
	}{
		{10, NakBackoff, "prod: the whole schedule fits"},
		{6, NakBackoff, "exactly one more than the schedule: still fits whole"},
		{5, NakBackoff[:4], "THE BOUNDARY: MaxDeliver == len(schedule) must still truncate (strictly-greater rule)"},
		{3, NakBackoff[:2], "a shrunk budget truncates to MaxDeliver-1 entries"},
		{2, NakBackoff[:1], "room for exactly one entry"},
		{1, nil, "no room for any backoff entry"},
	} {
		DefaultMaxDeliver = tc.maxDeliver
		got := consumerBackoff()
		if tc.want == nil {
			assert.Nil(t, got, tc.why)
			continue
		}
		assert.Equal(t, tc.want, got, tc.why)
		assert.Greater(t, DefaultMaxDeliver, len(got), "the truncation must always satisfy MaxDeliver > len(BackOff)")
	}
}

// TestTransientNakRedeliversOnBackoffSchedule: a message that fails transiently k times then succeeds is
// rendered exactly ONCE, and earns exactly the first k schedule entries — not k zero-delay retries.
func TestTransientNakRedeliversOnBackoffSchedule(t *testing.T) {
	js := NewMemJetStream()
	subj := DtellSubject("alice")

	var attempts int
	delivered := make(chan Message, 2)
	cons, err := js.Consume(subj, "alice", func(m Message, _ bool) AckDecision {
		attempts++
		if attempts <= 3 { // the transient window: target mid-handoff / gate bus blip
			return RetryTransient
		}
		delivered <- m
		return AckDelivered
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.NoError(t, js.PublishDurable(context.Background(), subj,
		Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "hi"}))

	select {
	case m := <-delivered:
		assert.Equal(t, "hi", m.Body)
	case <-time.After(2 * time.Second):
		t.Fatal("the tell was never delivered after its transient cleared")
	}
	assert.Equal(t, 4, attempts, "3 transient failures then one success")
	// The three NAKs earned the first three schedule entries, in order — NOT three immediate retries.
	assert.Equal(t, []time.Duration{200 * time.Millisecond, time.Second, 3 * time.Second}, js.NakDelays("alice"))
	assert.Empty(t, js.Parked("alice"), "a tell whose transient cleared must never park")
}

// TestTransientNakSurvivesTheWholeTransientWindow pins that the budget is spent across the WHOLE NakBackoff
// window: nine gaps, the tail holding at the last entry, summing to totalNakWindow. Those delay assertions
// are what distinguish the fix — the "it still delivers on the last attempt" half would also pass against
// the pre-fix immediate-NAK code, since the stand-in never depended on real spacing. What changed is that
// the same nine attempts now span ~164s of wall clock on a real broker instead of ~0ms, which is the entire
// point: a target mid-handoff, or a gate bus blip, has time to clear before the budget runs out.
func TestTransientNakSurvivesTheWholeTransientWindow(t *testing.T) {
	js := NewMemJetStream()
	subj := DtellSubject("alice")

	var attempts int
	delivered := make(chan struct{}, 1)
	cons, err := js.Consume(subj, "alice", func(Message, bool) AckDecision {
		attempts++
		if attempts < DefaultMaxDeliver { // clears on the very last attempt
			return RetryTransient
		}
		delivered <- struct{}{}
		return AckDelivered
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.NoError(t, js.PublishDurable(context.Background(), subj,
		Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "hi"}))

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("a transient that cleared inside the budget still lost the tell")
	}
	assert.Empty(t, js.Parked("alice"))
	// Every gap came off the schedule, and the tail held at the last entry rather than collapsing to 0.
	delays := js.NakDelays("alice")
	require.Len(t, delays, DefaultMaxDeliver-1)
	assert.Equal(t, NakBackoff[len(NakBackoff)-1], delays[len(delays)-1], "the tail holds at the last entry")
	var total time.Duration
	for _, d := range delays {
		total += d
	}
	assert.Equal(t, totalNakWindow(), total, "the retries span the full covered window")
}

// TestPoisonDropsWithoutSpendingTransientBudget: an undeliverable message is dropped after ONE call, earns
// NO backoff delay, and never parks — it must not consume the budget a real transient needs.
func TestPoisonDropsWithoutSpendingTransientBudget(t *testing.T) {
	js := NewMemJetStream()
	subj := DtellSubject("alice")

	var attempts int
	good := make(chan Message, 2)
	cons, err := js.Consume(subj, "alice", func(m Message, _ bool) AckDecision {
		if m.Body == "poison" {
			attempts++
			return DropPoison
		}
		good <- m
		return AckDelivered
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	ctx := context.Background()
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "poison"}))
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 2, IdempotencyKey: "bob:2", Body: "good"}))

	select {
	case m := <-good:
		assert.Equal(t, "good", m.Body)
	case <-time.After(2 * time.Second):
		t.Fatal("the good message never landed behind the poison")
	}
	assert.Equal(t, 1, attempts, "poison is handled exactly once, never redelivered")
	assert.Empty(t, js.NakDelays("alice"), "poison must not spend a single backoff delay")
	assert.Empty(t, js.Parked("alice"), "poison is dropped, not parked")
	assert.Len(t, js.Poisoned("alice"), 1, "the drop is recorded, not silent")
}

// TestExhaustedTransientParksObservably: a transient that never clears exhausts the schedule and PARKS.
// Park is permanent loss (the durable consumer is never deleted, so it never replays), so the stand-in
// records it — the loss is observable, exactly as the real consumer logs it at ERROR.
func TestExhaustedTransientParksObservably(t *testing.T) {
	js := NewMemJetStream()
	subj := DtellSubject("alice")

	old := DefaultMaxDeliver
	DefaultMaxDeliver = 3
	t.Cleanup(func() { DefaultMaxDeliver = old })

	var attempts int
	var mu sync.Mutex
	cons, err := js.Consume(subj, "alice", func(Message, bool) AckDecision {
		mu.Lock()
		attempts++
		mu.Unlock()
		return RetryTransient // an outage longer than the whole schedule
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.NoError(t, js.PublishDurable(context.Background(), subj,
		Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "doomed"}))

	require.Eventually(t, func() bool { return len(js.Parked("alice")) == 1 }, 2*time.Second, 5*time.Millisecond)
	mu.Lock()
	assert.Equal(t, 3, attempts, "exactly MaxDeliver attempts, then park")
	mu.Unlock()
	assert.Equal(t, "doomed", js.Parked("alice")[0].Body)
	assert.Len(t, js.NakDelays("alice"), 3, "each attempt recorded its scheduled delay")
}

// TestPendingMessageBlocksItsSuccessor pins the DOUBLE'S head-of-line contract: memConsumer resolves one
// message completely (retries, then park) before the next is delivered, so a test written against the
// stand-in sees the same ordering production has.
//
// It does NOT pin MaxAckPending=1 — the double is single-goroutine and would order this way whatever the
// real consumer is configured with. That production invariant is pinned in two other places, deliberately:
// hermetically by TestDurableConsumerConfigPinsMaxAckPending (the config value itself), and behaviourally by
// the broker-gated TestJetStreamRealBoundedRedelivery (a successor really is held back on a live server).
func TestPendingMessageBlocksItsSuccessor(t *testing.T) {
	js := NewMemJetStream()
	subj := DtellSubject("alice")

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
		if m.Body == "first" {
			return RetryTransient // stuck: it must fully resolve (park) before "second" is delivered
		}
		close(done)
		return AckDelivered
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	ctx := context.Background()
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 1, IdempotencyKey: "bob:1", Body: "first"}))
	require.NoError(t, js.PublishDurable(ctx, subj, Message{AuthorID: "bob", Seq: 2, IdempotencyKey: "bob:2", Body: "second"}))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the successor was never delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"first", "first", "first", "second"}, order,
		"the pending message resolves completely before its successor is delivered (MaxAckPending=1)")
}
