package commbus

import (
	"sync"
	"testing"
)

// nats_availability_test.go hermetically pins the #80 availability-listener mechanism on NATSBus WITHOUT a
// real broker: OnAvailabilityChange / fireAvailability / cancel operate only on the in-memory listener set
// (the disconnect/reconnect wiring to a live nats.Conn is exercised by the gated real-NATS tests).

func newTestNATSBus() *NATSBus { return &NATSBus{listeners: map[int]func(bool){}} }

// TestNATSBusAvailabilityListenersFireAndCancel: a registered listener receives each transition in order, and
// stops receiving once cancelled.
func TestNATSBusAvailabilityListenersFireAndCancel(t *testing.T) {
	b := newTestNATSBus()
	var mu sync.Mutex
	var got []bool
	cancel := b.OnAvailabilityChange(func(a bool) { mu.Lock(); got = append(got, a); mu.Unlock() })

	b.fireAvailability(false) // went offline
	b.fireAvailability(true)  // recovered

	mu.Lock()
	if len(got) != 2 || got[0] != false || got[1] != true {
		mu.Unlock()
		t.Fatalf("listener got %v, want [false true]", got)
	}
	mu.Unlock()

	cancel()
	b.fireAvailability(false) // must NOT reach the cancelled listener

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("a cancelled listener still fired: %v", got)
	}
}

// TestNATSBusAvailabilityMultipleListeners: every registered listener is fired, and cancelling one does not
// affect the others.
func TestNATSBusAvailabilityMultipleListeners(t *testing.T) {
	b := newTestNATSBus()
	var a, c int
	cancelA := b.OnAvailabilityChange(func(bool) { a++ })
	_ = b.OnAvailabilityChange(func(bool) { c++ })

	b.fireAvailability(false)
	if a != 1 || c != 1 {
		t.Fatalf("both listeners should fire once: a=%d c=%d", a, c)
	}
	cancelA()
	b.fireAvailability(true)
	if a != 1 {
		t.Fatalf("cancelled listener A fired again: a=%d", a)
	}
	if c != 2 {
		t.Fatalf("listener C should have fired twice: c=%d", c)
	}
}

// TestNATSBusAvailabilityCancelDuringFireNoDeadlock: fireAvailability snapshots the listener set OUTSIDE the
// lock, so a listener that cancels itself (or registers another) mid-fire cannot deadlock.
func TestNATSBusAvailabilityCancelDuringFireNoDeadlock(t *testing.T) {
	b := newTestNATSBus()
	fired := 0
	var cancel func()
	cancel = b.OnAvailabilityChange(func(bool) {
		fired++
		cancel() // cancel self while being fired — must not deadlock on b.mu
	})

	done := make(chan struct{})
	go func() { b.fireAvailability(false); close(done) }()
	<-done

	if fired != 1 {
		t.Fatalf("self-cancelling listener fired %d times, want 1", fired)
	}
	// It cancelled itself, so a second fire is a no-op.
	b.fireAvailability(true)
	if fired != 1 {
		t.Fatalf("self-cancelled listener fired again: %d", fired)
	}
}

// TestNATSBusAvailabilityDedupsSameState pins the "only on an actual transition, never a repeat" contract:
// two same-state fires in a row deliver only ONE (NATS can emit a duplicate, e.g. its Close() path).
func TestNATSBusAvailabilityDedupsSameState(t *testing.T) {
	b := newTestNATSBus()
	var got []bool
	b.OnAvailabilityChange(func(a bool) { got = append(got, a) })

	b.fireAvailability(false) // real transition (was connected) → fires
	b.fireAvailability(false) // duplicate → suppressed
	b.fireAvailability(true)  // real transition → fires
	b.fireAvailability(true)  // duplicate → suppressed
	b.fireAvailability(false) // real transition → fires

	want := []bool{false, true, false}
	if len(got) != len(want) {
		t.Fatalf("dedup: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dedup: got %v, want %v", got, want)
		}
	}
}

// Compile-time proof that NATSBus satisfies the optional interface the gate type-asserts (#80).
var _ AvailabilityWatcher = (*NATSBus)(nil)
