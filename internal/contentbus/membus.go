package contentbus

import (
	"context"
	"sync"
)

// membus.go is the in-memory Bus used by tests and a bare run, so the hot-reload reload path
// (publish -> subscriber -> single-ref re-read -> rebuild -> cache swap) is unit-testable WITHOUT
// a live NATS broker — exactly as MemStore makes the durability ladder testable without Postgres.
// It deliberately mirrors the NATS bus's observable semantics: a Publish fans out to every live
// subscriber, each subscriber's handler runs serially (one delivery goroutine per subscription,
// so a handler that serializes the cache swap needs no extra lock), and Unsubscribe/Close stop
// delivery. It is concurrency-safe (a mutex over the subscriber set + per-sub ordered delivery).

// MemBus is an in-process Bus. Multiple shards in one test process each Subscribe to the same
// MemBus, and a test (or the seed/publish helper) Publishes to it; every subscriber's handler
// fires, modelling the cross-shard fan-out a real NATS broker provides.
type MemBus struct {
	mu     sync.Mutex
	closed bool
	subs   map[*memSub]struct{}
}

// NewMemBus returns an empty in-memory bus.
func NewMemBus() *MemBus { return &MemBus{subs: map[*memSub]struct{}{}} }

// memSub is one subscription: a handler plus an ordered, buffered delivery channel drained by a
// single goroutine, so deliveries to one subscriber are serial (matching the NATS impl) and a
// slow handler never blocks Publish or another subscriber.
type memSub struct {
	bus     *MemBus
	handler func(Invalidation)
	ch      chan Invalidation
	once    sync.Once
	done    chan struct{}
}

// memSubDepth bounds a subscriber's delivery buffer. Invalidations are rare (one per content
// edit) so this is generous; a full buffer drops the oldest-style by blocking briefly under the
// publish lock would be wrong, so Publish enqueues non-blocking and a full channel drops the
// signal (the next edit re-publishes). Matches the at-least-once-but-droppable posture of the
// rest of the system (saver queue, session.send).
const memSubDepth = 64

// Publish delivers inv to every live subscriber. Non-blocking per subscriber: a full delivery
// buffer drops the signal rather than stalling the publisher (a dropped invalidation only delays
// a hot reload until the next edit; correctness — the next spawn eventually sees fresh data —
// holds once any later invalidation lands). Returns nil even with no subscribers (a publish to an
// empty bus is a no-op, like publishing to NATS with no listeners).
func (b *MemBus) Publish(_ context.Context, inv Invalidation) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrBusClosed
	}
	targets := make([]*memSub, 0, len(b.subs))
	for s := range b.subs {
		targets = append(targets, s)
	}
	b.mu.Unlock()
	for _, s := range targets {
		select {
		case s.ch <- inv:
		default: // buffer full: drop this signal (next edit re-publishes)
		}
	}
	return nil
}

// Subscribe registers handler and starts its delivery goroutine. Deliveries are serial per
// subscription (the goroutine ranges over the buffered channel), so a handler that mutates a
// shard's cache needs no extra synchronization against itself.
func (b *MemBus) Subscribe(handler func(Invalidation)) (Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrBusClosed
	}
	s := &memSub{
		bus:     b,
		handler: handler,
		ch:      make(chan Invalidation, memSubDepth),
		done:    make(chan struct{}),
	}
	b.subs[s] = struct{}{}
	go func() {
		for inv := range s.ch {
			s.handler(inv)
		}
		close(s.done)
	}()
	return s, nil
}

// Close stops every subscription and rejects further publishes. Idempotent.
func (b *MemBus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	subs := make([]*memSub, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.subs = map[*memSub]struct{}{}
	b.mu.Unlock()
	for _, s := range subs {
		s.stop()
	}
	return nil
}

// Unsubscribe removes this subscription and stops its delivery goroutine. Idempotent.
func (s *memSub) Unsubscribe() error {
	s.bus.mu.Lock()
	delete(s.bus.subs, s)
	s.bus.mu.Unlock()
	s.stop()
	return nil
}

// stop closes the delivery channel once (draining its goroutine) and waits for it to exit, so a
// returned Unsubscribe/Close guarantees no further handler call is in flight.
func (s *memSub) stop() {
	s.once.Do(func() { close(s.ch) })
	<-s.done
}

// Compile-time assertions.
var (
	_ Bus          = (*MemBus)(nil)
	_ Subscription = (*memSub)(nil)
)
