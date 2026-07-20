package commbus

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jetstream_stall_gated_test.go — the BROKER-GATED half of #390.
//
// jetstream_stall_test.go pins the stall threshold hermetically, but it drives the MemJetStream stand-in,
// so every one of its assertions still holds with the hook DELETED from jetstream_nats.go — the production
// consumer. (Verified by mutation: removing the whole `if stalled(attempt)` block from jetstream_nats.go
// leaves commbus, scopebus, world and director green.) This file is what makes the SHIPPED path pinned:
// the stall must fire off the broker's own NumDelivered, on the real consumer, labeled with the real
// stream.
//
// Run it with:  TELOS_NATS_URL=nats://127.0.0.1:4222 go test ./internal/commbus/ -run JetStreamRealStall
//
// Unlike the other gated tests here, these deliberately do NOT shrink DefaultMaxDeliver/NakBackoff. Those
// are plain package vars read on the consumer's delivery goroutine (nakBackoff, totalNakWindow in the WARN
// line this very change added), so restoring them in a t.Cleanup while a consumer is still live is a
// genuine -race failure — sleeping first does not help, because a sleep creates no happens-before edge.
// Running on the real schedule costs ~44s to reach one delivery past the crossing, and keeps the test honest about the
// production timing anyway.

// stallSighting is one observed crossing, captured whole: asserting the COUNT alone cannot tell a correct
// call from one whose stream and subject arguments are swapped.
type stallSighting struct {
	stream, subject string
	attempt         int
}

// observeStalls installs the stall seam, filtered to subj so a sibling gated test on the same stream
// cannot satisfy the assertion.
func observeStalls(t *testing.T, subj string) func() []stallSighting {
	t.Helper()
	var mu sync.Mutex
	var seen []stallSighting
	obs := stallObserver(func(stream, subject string, attempt int) {
		if subject != subj {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, stallSighting{stream, subject, attempt})
	})
	stallObserverPtr.Store(&obs)
	t.Cleanup(func() { stallObserverPtr.Store(nil) })
	return func() []stallSighting {
		mu.Lock()
		defer mu.Unlock()
		return append([]stallSighting{}, seen...)
	}
}

// TestJetStreamRealStallFiresOnceOffBrokerNumDelivered is the wiring proof for the production consumer.
// A message that transient-fails forever crosses stallAttempt exactly ONCE over a real broker, at the
// broker's own NumDelivered, labeled with the stream the handle is bound to.
func TestJetStreamRealStallFiresOnceOffBrokerNumDelivered(t *testing.T) {
	url := natsURL(t)
	require.Less(t, stallAttempt, DefaultMaxDeliver, "the threshold must be reachable inside the budget")

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	target := "itstall-" + suffix
	author := "bob-" + suffix
	subj := DtellSubject(target)
	sightings := observeStalls(t, subj)

	js, err := NewJetStream(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.Close() })

	require.NoError(t, js.PublishDurable(context.Background(), subj, Message{
		AuthorID: author, AuthorName: "Bob", Seq: 1,
		IdempotencyKey: NewIdempotencyKey(author, 1), Body: "stuck",
	}))

	var attempts atomic.Int64
	cons, err := js.Consume(subj, target, func(Message, bool) AckDecision {
		attempts.Add(1)
		return RetryTransient // never clears -> redelivered toward the park
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	// Run PAST the crossing, so "exactly once" is an assertion about a RANGE of attempts rather than about
	// the first one that happens to fire — without reaching stallAttempt+1 an `==`-to-`>=` mutation would
	// survive this test, and that mutation is the whole anti-conflation property.
	//
	// On the real (unshrunk) schedule that costs real time: the crossing delivery lands at
	// 200ms+1s+3s+10s = 14.2s and the one after it at +30s = 44.2s. The timeout is generous relative to
	// that so a slow CI runner does not flake, at the price of a slow test — which is the trade the gated
	// tier exists to make. See the file header for why the schedule is not shrunk instead.
	require.Eventually(t, func() bool { return attempts.Load() >= int64(stallAttempt+1) },
		90*time.Second, 50*time.Millisecond,
		"precondition: the message must be redelivered past the stall threshold")

	got := sightings()
	require.Len(t, got, 1,
		"the PRODUCTION consumer must report a stuck message exactly once — this is the assertion the "+
			"hermetic mem-path tests cannot make, since they all hold with the hook deleted from "+
			"jetstream_nats.go")
	assert.Equal(t, jsStreamName, got[0].stream,
		"the counter's only label must be the STREAM this handle is bound to; a hardcoded label routes a "+
			"WORLD_EVENTS stall to the tell alert")
	assert.Equal(t, subj, got[0].subject, "stream and subject must not be swapped")
	assert.Equal(t, stallAttempt, got[0].attempt,
		"attempt must come from the BROKER's NumDelivered, not a process-local counter")
}

// TestJetStreamRealStallLabelsTheScopeStream pins the other half of the label: the same code path bound to
// WORLD_EVENTS must report WORLD_EVENTS. Without it, noteStall(b.name, ...) can be replaced by a literal
// "COMMS_TELL" and every test in the package still passes — mis-routing the alert for the exact stream
// #390 argues the signal matters most for (one consumer per scope, MaxAckPending=1, so a stall wedges a
// whole scope's orchestration).
func TestJetStreamRealStallLabelsTheScopeStream(t *testing.T) {
	url := natsURL(t)

	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	subj := ScopeSubjectPrefix + "region.itstall" + suffix
	consumerID := "itstallscope-" + suffix
	sightings := observeStalls(t, subj)

	js, err := NewScopeJetStream(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.Close() })

	require.NoError(t, js.PublishDurable(context.Background(), subj, Message{
		AuthorID: "director", IdempotencyKey: NewIdempotencyKey("director-"+suffix, 1), Body: "{}",
	}))
	cons, err := js.Consume(subj, consumerID, func(Message, bool) AckDecision { return RetryTransient })
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.Eventually(t, func() bool { return len(sightings()) > 0 }, 60*time.Second, 50*time.Millisecond,
		"the scoped-event consumer never reported a stall (#390)")
	assert.Equal(t, jsScopeStreamName, sightings()[0].stream,
		"a stall on the scoped-event stream must be labeled WORLD_EVENTS, not the tell stream")
}
