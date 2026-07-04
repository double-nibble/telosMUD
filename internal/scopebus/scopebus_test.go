package scopebus

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/commbus"
)

type recvEvent struct {
	event   string
	payload string
	source  string
}

func recv(t *testing.T, ch <-chan recvEvent) (recvEvent, bool) {
	t.Helper()
	select {
	case e := <-ch:
		return e, true
	case <-time.After(time.Second):
		return recvEvent{}, false
	}
}

func TestScopeSubject(t *testing.T) {
	cases := []struct {
		scope Scope
		want  string
		err   bool
	}{
		{World(), "telos.scope.world", false},
		{Region("duskwall"), "telos.scope.region.duskwall", false},
		{ZoneScope("midgaard"), "telos.scope.zone.midgaard", false},
		{Scope{Kind: "world", ID: "x"}, "", true}, // world takes no id
		{Region("bad id!"), "", true},             // invalid charset
		{Region(""), "", true},                    // empty id
		{Scope{Kind: "bogus"}, "", true},
	}
	for _, c := range cases {
		got, err := c.scope.Subject()
		if c.err {
			assert.Error(t, err, "%+v", c.scope)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, c.want, got)
	}
}

func TestScopeBusSignalAndSubscribe(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	b := New(core)
	ctx := context.Background()

	got := make(chan recvEvent, 8)
	sub, err := b.Subscribe(World(), func(event string, payload json.RawMessage, source string) {
		got <- recvEvent{event, string(payload), source}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	require.NoError(t, b.Signal(ctx, World(), "invasion.start", json.RawMessage(`{"n":1}`), "world-director"))
	e, ok := recv(t, got)
	require.True(t, ok, "world event not delivered")
	assert.Equal(t, "invasion.start", e.event)
	assert.JSONEq(t, `{"n":1}`, e.payload)
	assert.Equal(t, "world-director", e.source)
}

func TestScopeBusRegionIsolation(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	b := New(core)
	ctx := context.Background()

	got := make(chan recvEvent, 8)
	sub, err := b.Subscribe(Region("duskwall"), func(event string, payload json.RawMessage, source string) {
		got <- recvEvent{event, string(payload), source}
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	// An event to a DIFFERENT region must NOT reach the duskwall subscriber.
	require.NoError(t, b.Signal(ctx, Region("ironhold"), "noise", nil, "z"))
	// The duskwall event does.
	require.NoError(t, b.Signal(ctx, Region("duskwall"), "city_liberated", json.RawMessage(`{"hero":"kurt"}`), "duskwall-director"))

	e, ok := recv(t, got)
	require.True(t, ok, "duskwall event not delivered")
	assert.Equal(t, "city_liberated", e.event, "region isolation broken — got %q (a foreign region's event leaked)", e.event)

	// No second event (ironhold's must not have arrived).
	if _, ok := recv(t, got); ok {
		t.Fatal("a foreign region's event leaked to the duskwall subscriber")
	}
}

func TestScopeBusRejectsBadScopeAndEmptyEvent(t *testing.T) {
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })
	b := New(core)
	ctx := context.Background()

	assert.Error(t, b.Signal(ctx, Region("bad!"), "ev", nil, "s"), "a malformed scope id must be refused")
	assert.Error(t, b.Signal(ctx, World(), "  ", nil, "s"), "an empty event name must be refused")
}

// --- durable tier (10.2b) ----------------------------------------------------------------------

func durableBus(t *testing.T, source string) (*Bus, *commbus.MemJetStream) {
	t.Helper()
	js := commbus.NewMemJetStream()
	b := New(commbus.NewMemBus()).WithDurable(js, source)
	return b, js
}

func TestScopeBusDurableRoundTrip(t *testing.T) {
	b, _ := durableBus(t, "world-director-run1")
	ctx := context.Background()

	got := make(chan DurableEvent, 8)
	cons, err := b.SubscribeDurable(World(), "world-dir", func(ev DurableEvent) bool {
		got <- ev
		return true
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.NoError(t, b.SignalDurable(ctx, World(), "invasion.phase", json.RawMessage(`{"phase":2}`)))

	select {
	case ev := <-got:
		assert.Equal(t, "invasion.phase", ev.Event)
		assert.JSONEq(t, `{"phase":2}`, string(ev.Payload))
		assert.Equal(t, "world-director-run1", ev.Source)
		assert.Equal(t, "world-director-run1:1", ev.Key, "idempotency key is <source>:<seq>")
		assert.False(t, ev.Backlog, "a live event is not backlog")
	case <-time.After(time.Second):
		t.Fatal("durable event not delivered")
	}
}

// The headline durable guarantee: an event published while NO subscriber is running survives, and a
// subscriber that starts LATER (a restarted director) replays it as BACKLOG. This is the foundation of
// the 10.5 boss ripple "survives a director restart".
func TestScopeBusDurableSurvivesLateSubscriber(t *testing.T) {
	b, _ := durableBus(t, "world-director-run1")
	ctx := context.Background()

	// Publish BEFORE any subscriber exists.
	require.NoError(t, b.SignalDurable(ctx, World(), "boss.slain", json.RawMessage(`{"boss":"vurgoth"}`)))

	// A director starts up afterwards and must still receive it, flagged backlog.
	got := make(chan DurableEvent, 8)
	cons, err := b.SubscribeDurable(World(), "world-dir", func(ev DurableEvent) bool {
		got <- ev
		return true
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	select {
	case ev := <-got:
		assert.Equal(t, "boss.slain", ev.Event)
		assert.True(t, ev.Backlog, "an event stored before the subscriber started must replay as backlog")
	case <-time.After(time.Second):
		t.Fatal("durable backlog event not replayed to a late subscriber")
	}
}

// A NAK (handler returns false on a transient failure) redelivers; once it succeeds the consumer
// advances. Proves a state-applying subscriber that fails mid-apply gets another chance.
func TestScopeBusDurableNakRedelivers(t *testing.T) {
	b, _ := durableBus(t, "run1")
	ctx := context.Background()

	var attempts atomic.Int32
	done := make(chan struct{})
	cons, err := b.SubscribeDurable(World(), "world-dir", func(_ DurableEvent) bool {
		if attempts.Add(1) < 3 {
			return false // transient failure: NAK, redeliver
		}
		close(done)
		return true
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.NoError(t, b.SignalDurable(ctx, World(), "ev", nil))
	select {
	case <-done:
		assert.GreaterOrEqual(t, attempts.Load(), int32(3), "the event was redelivered until acked")
	case <-time.After(2 * time.Second):
		t.Fatal("a NAK'd event was not redelivered to success")
	}
}

func TestScopeBusDurableRequiresConfig(t *testing.T) {
	b := New(commbus.NewMemBus()) // transient only, no WithDurable
	ctx := context.Background()

	assert.Error(t, b.SignalDurable(ctx, World(), "ev", nil), "durable signal without a durable tier must error")
	_, err := b.SubscribeDurable(World(), "c", func(_ DurableEvent) bool { return true })
	assert.Error(t, err, "durable subscribe without a durable tier must error")
}

// Idempotency keys are monotonic per process so a redelivery can be deduped by a state-applying
// subscriber (track the highest applied seq per source).
func TestScopeBusDurableKeysAreMonotonic(t *testing.T) {
	b, _ := durableBus(t, "run1")
	ctx := context.Background()

	keys := make(chan string, 8)
	cons, err := b.SubscribeDurable(World(), "world-dir", func(ev DurableEvent) bool {
		keys <- ev.Key
		return true
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.NoError(t, b.SignalDurable(ctx, World(), "a", nil))
	require.NoError(t, b.SignalDurable(ctx, World(), "b", nil))

	var got []string
	for i := 0; i < 2; i++ {
		select {
		case k := <-keys:
			got = append(got, k)
		case <-time.After(time.Second):
			t.Fatal("durable event not delivered")
		}
	}
	assert.Equal(t, []string{"run1:1", "run1:2"}, got, "keys advance monotonically per process")
}

// TestScopeBusDurableConsumerDedupsRedelivery pins the CONSUMER-SIDE idempotency contract every durable
// subscriber depends on but nothing pinned before: JetStream is at-least-once, so an ack lost AFTER a state
// mutation redelivers the SAME key. A consumer that dedups on the key (apply a key only once) must therefore
// apply EXACTLY ONCE despite >=2 deliveries. TestScopeBusDurableNakRedelivers proves the redelivery happens
// and TestScopeBusDurableKeysAreMonotonic proves the key is stable across it; this proves the dedup a
// consumer BUILDS on those — the whole point of the <source>:<seq> idempotency key.
func TestScopeBusDurableConsumerDedupsRedelivery(t *testing.T) {
	b, _ := durableBus(t, "run1")
	ctx := context.Background()

	var deliveries atomic.Int32
	var mu sync.Mutex
	appliedKeys := map[string]bool{}
	applied := 0
	var keys []string
	done := make(chan struct{})
	cons, err := b.SubscribeDurable(World(), "world-dir", func(ev DurableEvent) bool {
		n := deliveries.Add(1)
		mu.Lock()
		keys = append(keys, ev.Key)
		// The idempotent-consumer pattern: apply a key only once, even if it is redelivered.
		if !appliedKeys[ev.Key] {
			appliedKeys[ev.Key] = true
			applied++
		}
		mu.Unlock()
		if n == 1 {
			// SIMULATE a lost ack: a real idempotent consumer acks (return true) after applying, and the
			// redelivery it must survive comes from an ack lost in flight. A NAK produces the identical
			// observable input (the same key delivered again), so it is a faithful stand-in for that here.
			return false
		}
		close(done) // deterministic: deliverBounded stops the instant this ack lands, so exactly-2 deliveries
		return true
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	require.NoError(t, b.SignalDurable(ctx, World(), "ev", json.RawMessage(`{"n":1}`)))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the NAK'd event was not redelivered")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, deliveries.Load(), int32(2), "the event should be delivered at least twice (apply, then redelivery after the lost ack)")
	require.GreaterOrEqual(t, len(keys), 2, "expected at least two deliveries")
	assert.Equal(t, keys[0], keys[1], "a redelivery must carry the SAME idempotency key (so the consumer can dedup it)")
	assert.Equal(t, 1, applied, "the idempotent consumer must apply the key EXACTLY ONCE despite the redelivery")
}
