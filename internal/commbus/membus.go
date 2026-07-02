package commbus

import (
	"context"
	"sync"
)

// membus.go is the in-process Bus used by tests and a bare run, so the whole comms path (world
// publishes -> cross-shard fan-out -> gate renders) is unit-testable WITHOUT a live NATS broker —
// exactly as contentbus.MemBus / store.MemStore make their layers testable without a broker/DB. The
// cross-shard tests run N world shards + N gates in ONE process against ONE MemBus.
//
// It mirrors the NATS bus's OBSERVABLE semantics on the two seams the security/distsys reviewers
// care about:
//
//   - PER-SUBSCRIPTION ORDERED delivery (P8-A3): each subscription drains a single buffered channel
//     with one goroutine, so a single subject's messages reach that subscriber IN PUBLISH ORDER. A
//     slow handler never blocks Publish or another subscriber.
//   - The PUBLISH ACL (P8-A2): a RoleGate handle's Publish on a chan/tell subject is refused with
//     ErrPublishForbidden BEFORE anything is enqueued — the gate role structurally cannot author a
//     chan/tell message even in-process. The ACL is enforced on the HANDLE, and many handles can
//     share one underlying MemBus core (so a "world" handle and a "gate" handle in the same test see
//     the same fan-out but have different publish rights).
//
// The MemBus CORE holds the subscriber set + fan-out; a memHandle wraps the core with a Role. This
// is the in-process model of "one broker, many clients with different roles."

// MemBus is an in-process Bus core shared by any number of role handles. Construct role-scoped
// handles with NewWorldBus / NewGateBus (which both point at the same core), or use a single MemBus
// directly as a world-role bus (the zero-config convenience for non-ACL tests).
type MemBus struct {
	core *memCore
	role Role
}

// memCore is the shared fan-out state behind every handle on one MemBus. Splitting it out lets a
// world handle and a gate handle share one fan-out while carrying different publish rights.
type memCore struct {
	mu     sync.Mutex
	closed bool
	subs   map[*memSub]struct{}
}

// NewMemBus returns a MemBus with the WORLD role (it may publish chan/tell) — the convenient default
// for tests that do not exercise the ACL asymmetry. For an ACL test, build a world handle AND a gate
// handle over the SAME core with NewWorldBus/NewGateBus.
func NewMemBus() *MemBus {
	return &MemBus{core: newMemCore(), role: RoleWorld}
}

// NewWorldBus / NewGateBus return two handles over ONE shared in-process core with the WORLD and GATE
// roles respectively — the in-process model of "the same broker, a publishing world client and a
// subscribe-only gate client." The gate handle's Publish on a chan/tell subject returns
// ErrPublishForbidden (P8-A2). Subscriptions on either handle see the same fan-out.
func NewWorldBus() (*MemBus, *MemBus) {
	core := newMemCore()
	return &MemBus{core: core, role: RoleWorld}, &MemBus{core: core, role: RoleGate}
}

// WorldHandle / GateHandle return a sibling handle over the same core with the given role, so a test
// (or wiring) can derive a gate handle from a world MemBus and vice versa without a second broker.
func (b *MemBus) WorldHandle() *MemBus { return &MemBus{core: b.core, role: RoleWorld} }

// GateHandle returns a RoleGate sibling over the same core — see WorldHandle.
func (b *MemBus) GateHandle() *MemBus { return &MemBus{core: b.core, role: RoleGate} }

func newMemCore() *memCore { return &memCore{subs: map[*memSub]struct{}{}} }

// memSub is one subscription: a subject PATTERN (exact or trailing-* wildcard), a handler, and an
// ordered buffered delivery channel drained by a single goroutine — so deliveries to one subscriber
// are SERIAL and IN ORDER (the P8-A3 per-subject ordering the reviewers check).
type memSub struct {
	core    *memCore
	pattern string
	handler func(Message)
	ch      chan Message
	once    sync.Once
	done    chan struct{}
}

// memSubDepth bounds a subscriber's delivery buffer. A full buffer drops the message rather than
// stalling the publisher (one slow gate must never stall channel fan-out — P8-A1 slow-consumer; the
// same at-least-once-but-droppable posture as contentbus / the saver queue / session.send). Transient
// chan/tell are at-most-once anyway; the durable path (8.5) gets JetStream, not this buffer.
const memSubDepth = 256

// Role reports this handle's publish capability.
func (b *MemBus) Role() Role { return b.role }

// Available is always true for an in-process bus: there is no transport to lose.
func (b *MemBus) Available() bool { return true }

// Publish enforces the ACL then fans msg out to every subscriber whose pattern matches subj, IN
// PUBLISH ORDER per subscriber. The ACL check is FIRST (P8-A2): a RoleGate handle publishing a
// chan/tell subject returns ErrPublishForbidden and NOTHING is enqueued — the impersonation gate.
// msg.Subject is stamped to subj so the sink can dispatch on it regardless of a wildcard subscription.
func (b *MemBus) Publish(_ context.Context, subj string, msg Message) error {
	if b.role == RoleGate && isACLGuarded(subj) {
		return ErrPublishForbidden
	}
	msg.Subject = subj
	c := b.core
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrBusClosed
	}
	// Deliver UNDER the lock. The send is non-blocking (select default), so holding c.mu during it
	// cannot deadlock — and it closes a real race: Unsubscribe/Close remove a sub from c.subs under
	// THIS lock and only then close s.ch, so a send (which only happens for a sub still in c.subs, while
	// we hold the lock) can never race or hit a closed channel. Collecting targets and sending after
	// unlocking left a window where a concurrent Unsubscribe closed s.ch between collect and send — a
	// data race AND a latent send-on-closed-channel panic (the select `default` guards a full buffer,
	// not a closed channel).
	for s := range c.subs {
		if wildcardMatches(s.pattern, subj) {
			select {
			case s.ch <- msg:
			default: // buffer full: drop (slow-consumer protection; transient at-most-once)
			}
		}
	}
	c.mu.Unlock()
	return nil
}

// Subscribe registers handler for subj (exact or trailing-* wildcard) and starts its single ordered
// delivery goroutine. Subscribe is role-agnostic: a gate (the normal sink) and a world may both
// subscribe.
func (b *MemBus) Subscribe(subj string, handler func(Message)) (Subscription, error) {
	c := b.core
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrBusClosed
	}
	s := &memSub{
		core:    c,
		pattern: subj,
		handler: handler,
		ch:      make(chan Message, memSubDepth),
		done:    make(chan struct{}),
	}
	c.subs[s] = struct{}{}
	go func() {
		for msg := range s.ch {
			s.handler(msg)
		}
		close(s.done)
	}()
	return s, nil
}

// Close stops every subscription on the shared core and rejects further publishes. Idempotent.
// Closing ANY handle closes the shared core (the in-process model of the broker going away).
func (b *MemBus) Close() error {
	c := b.core
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	subs := make([]*memSub, 0, len(c.subs))
	for s := range c.subs {
		subs = append(subs, s)
	}
	c.subs = map[*memSub]struct{}{}
	c.mu.Unlock()
	for _, s := range subs {
		s.stop()
	}
	return nil
}

// Unsubscribe removes this subscription and stops its delivery goroutine. Idempotent.
func (s *memSub) Unsubscribe() error {
	s.core.mu.Lock()
	delete(s.core.subs, s)
	s.core.mu.Unlock()
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
