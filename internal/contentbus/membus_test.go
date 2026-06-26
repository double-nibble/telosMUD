package contentbus

import (
	"context"
	"sync"
	"testing"
	"time"
)

// membus_test.go covers the in-memory bus on its own (no NATS), so the test substrate the world
// reload tests depend on is itself trusted: fan-out to multiple subscribers, serial per-subscriber
// delivery, Unsubscribe/Close stopping delivery.

// TestMemBusFanOut asserts a publish reaches every live subscriber (the cross-shard fan-out a real
// broker provides).
func TestMemBusFanOut(t *testing.T) {
	bus := NewMemBus()
	defer bus.Close()

	const subs = 3
	var wg sync.WaitGroup
	wg.Add(subs)
	for i := 0; i < subs; i++ {
		_, err := bus.Subscribe(func(Invalidation) { wg.Done() })
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := bus.Publish(context.Background(), Invalidation{Kind: "room", Ref: "a", Pack: "p"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("not every subscriber received the invalidation")
	}
}

// TestMemBusUnsubscribeStops asserts an unsubscribed handler receives no further deliveries.
func TestMemBusUnsubscribeStops(t *testing.T) {
	bus := NewMemBus()
	defer bus.Close()

	var mu sync.Mutex
	count := 0
	sub, err := bus.Subscribe(func(Invalidation) {
		mu.Lock()
		count++
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), Invalidation{Kind: "room", Ref: "a", Pack: "p"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if count != 0 {
		t.Fatalf("unsubscribed handler fired %d times", count)
	}
}

// TestMemBusClosedRejects asserts publish/subscribe after Close return ErrBusClosed.
func TestMemBusClosedRejects(t *testing.T) {
	bus := NewMemBus()
	_ = bus.Close()
	if err := bus.Publish(context.Background(), Invalidation{}); err != ErrBusClosed {
		t.Fatalf("publish after close = %v, want ErrBusClosed", err)
	}
	if _, err := bus.Subscribe(func(Invalidation) {}); err != ErrBusClosed {
		t.Fatalf("subscribe after close = %v, want ErrBusClosed", err)
	}
}
