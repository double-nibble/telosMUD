package gate

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
)

// comms_transition_test.go pins #80: the gate surfaces a MID-SESSION comms up/down transition to the player,
// which the one-shot #61 login probe cannot cover.

// watchableBus wraps a real Bus and adds the #80 AvailabilityWatcher surface plus a test-driven fire() to
// simulate the transport crossing the available/unavailable boundary mid-session.
type watchableBus struct {
	inner     commbus.Bus
	mu        sync.Mutex
	available bool
	listeners map[int]func(bool)
	nextID    int
}

func newWatchableBus(inner commbus.Bus) *watchableBus {
	return &watchableBus{inner: inner, available: true, listeners: map[int]func(bool){}}
}

func (b *watchableBus) Role() commbus.Role { return b.inner.Role() }
func (b *watchableBus) Publish(ctx context.Context, subj string, msg commbus.Message) error {
	return b.inner.Publish(ctx, subj, msg)
}

func (b *watchableBus) Subscribe(subj string, h func(commbus.Message)) (commbus.Subscription, error) {
	return b.inner.Subscribe(subj, h)
}

func (b *watchableBus) Available() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.available
}
func (b *watchableBus) Close() error { return b.inner.Close() }

func (b *watchableBus) OnAvailabilityChange(fn func(bool)) func() {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.listeners[id] = fn
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.listeners, id)
		b.mu.Unlock()
	}
}

// fire simulates a transport transition: set the new availability + notify listeners.
func (b *watchableBus) fire(available bool) {
	b.mu.Lock()
	b.available = available
	fns := make([]func(bool), 0, len(b.listeners))
	for _, fn := range b.listeners {
		fns = append(fns, fn)
	}
	b.mu.Unlock()
	for _, fn := range fns {
		fn(available)
	}
}

// TestCommsMidSessionUpDownNotice: with comms configured and AVAILABLE at login (no #61 notice), a mid-session
// DROP produces an offline notice and a RECOVERY produces a back-online notice — both driven by the bus's
// availability transition, not a poll.
func TestCommsMidSessionUpDownNotice(t *testing.T) {
	const addr = "addr-transition"
	worldBus, gateBus := commbus.NewWorldBus()
	t.Cleanup(func() { _ = worldBus.Close() })
	wb := newWatchableBus(gateBus) // available at login

	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGateWithComms(directory.Static{Addr: addr}, wb)
	h.srv.WithCommsExpected(true)

	term := h.dial(t)
	term.login(t, "Alice")
	term.expect(t, "Temple Square") // in-world; the availability listener is now registered

	// The bus was up at login, so there was NO login-time offline notice.
	if got := term.acc.String(); strings.Contains(got, "chat is currently offline") {
		t.Fatalf("an available-at-login bus wrongly emitted the login offline notice: %q", got)
	}

	// Mid-session DROP → offline notice.
	wb.fire(false)
	term.expect(t, "chat went offline")

	// RECOVERY → back-online notice.
	wb.fire(true)
	term.expect(t, "chat is back online")
}

// TestCommsTransitionListenerCancelledOnSessionEnd: after the session ends, firing a transition must not
// panic or write to a closed connection (the deferred cancelWatch tore the listener down).
func TestCommsTransitionListenerCancelledOnSessionEnd(t *testing.T) {
	const addr = "addr-transition-end"
	worldBus, gateBus := commbus.NewWorldBus()
	t.Cleanup(func() { _ = worldBus.Close() })
	wb := newWatchableBus(gateBus)

	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGateWithComms(directory.Static{Addr: addr}, wb)
	h.srv.WithCommsExpected(true)

	term := h.dial(t)
	term.login(t, "Alice")
	term.expect(t, "Temple Square")
	term.close(t) // end the session → the deferred cancelWatch runs as the session goroutine unwinds

	// The listener is cancelled as the session goroutine unwinds its defers (asynchronously), so poll for it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		wb.mu.Lock()
		n := len(wb.listeners)
		wb.mu.Unlock()
		if n == 0 {
			return // cancelled — no leak
		}
		if time.Now().After(deadline) {
			t.Fatalf("the availability listener was not cancelled on session end: %d remain", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
