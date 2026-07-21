package commbus

import (
	"context"
	"sync"
	"time"
)

// memjs.go is the JetStream STAND-IN for the durable path (slice 8.5: offline tells). The real
// JetStream wiring (a COMMS_TELL stream, Nats-Msg-Id publish-side dedup, a per-player durable
// consumer with bounded redelivery) is NOT built here — 8.1 only lays the test double the durable
// slice will build and test against, so the offline-tell/mail logic is unit-testable with no broker
// (the MemBus does the same for the transient path).
//
// The durable model it stands in for (P8-D5 / P8-A5) has two dedup layers; this double models BOTH:
//
//   - PUBLISH-SIDE dedup via the IdempotencyKey ("<authorID>:<seq>") — a re-publish of an already-seen
//     key is a no-op append (the broker's Nats-Msg-Id dedup window). Append reports whether the message
//     was newly stored, so a redelivery storm at the source is absorbed.
//   - CONSUMER-SIDE delivered-cursor — a per-consumer monotonic cursor over the append log so a
//     redelivery AFTER the publish-dedup window is still suppressed at render time (belt and
//     suspenders, P8-A5). DeliverPending returns only entries past a consumer's cursor and advances it.
//
// It is intentionally minimal and in-memory; ordering is the append order (a single durable stream is
// one ordered log, which is the per-target order 8.5 promises). Concurrency-safe.

// MemJetStream is the in-memory durable-log stand-in: one append-ordered log per subject, with
// publish-side idempotency-key dedup and per-consumer delivered cursors.
type MemJetStream struct {
	mu sync.Mutex
	// log is the ordered append log per subject (e.g. the per-target dtell subject).
	log map[string][]Message
	// seen is the publish-side dedup set per subject: an IdempotencyKey already appended is not
	// re-appended (models the Nats-Msg-Id dedup window, treated as unbounded for the stand-in).
	seen map[string]map[string]struct{}
	// cursor is the consumer-side delivered cursor: cursor[subject][consumerID] = count of entries
	// already delivered to that consumer (the next DeliverPending starts there).
	cursor map[string]map[string]int
	// wakers is the per-subject wake channel a live Consume goroutine (jetstream.go) blocks on; an
	// Append closes-and-replaces it so a publish wakes every consumer to re-drain. Closed signals
	// "something changed"; the consumer re-reads DeliverPending after the wake. nil entry => no
	// consumer has asked for a wake channel yet (lazily created in wakeChan).
	wakers map[string]chan struct{}
	// closed marks the stand-in closed (Close): PublishDurable/Consume then refuse like a closed broker.
	closed bool
	// #266 redelivery observations. The stand-in RECORDS the redelivery schedule it would have waited
	// (it never sleeps it — see deliverBounded) plus the terminal outcomes, so a hermetic test can assert
	// the real consumer's semantics: which delays a RetryTransient earns, that a DropPoison earns none,
	// and that an exhausted message is PARKED (permanent loss) rather than silently vanishing.
	nakDelays map[string][]time.Duration // consumerID -> the backoff delay recorded per RetryTransient
	parked    map[string][]Message       // consumerID -> messages that exhausted DefaultMaxDeliver
	poisoned  map[string][]Message       // consumerID -> messages the handler dropped as DropPoison
}

// noteNak records the backoff delay a RetryTransient redelivery would have waited. Consumer goroutine.
func (js *MemJetStream) noteNak(consumerID string, d time.Duration) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.nakDelays == nil {
		js.nakDelays = map[string][]time.Duration{}
	}
	js.nakDelays[consumerID] = append(js.nakDelays[consumerID], d)
}

// notePark records a message that exhausted its redelivery budget — permanent loss in the real consumer.
func (js *MemJetStream) notePark(consumerID string, msg Message) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.parked == nil {
		js.parked = map[string][]Message{}
	}
	js.parked[consumerID] = append(js.parked[consumerID], msg)
}

// notePoison records a message the handler declared undeliverable (dropped after one call, no budget).
func (js *MemJetStream) notePoison(consumerID string, msg Message) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.poisoned == nil {
		js.poisoned = map[string][]Message{}
	}
	js.poisoned[consumerID] = append(js.poisoned[consumerID], msg)
}

// NakDelays returns the backoff schedule recorded for consumerID's RetryTransient redeliveries (a copy).
func (js *MemJetStream) NakDelays(consumerID string) []time.Duration {
	js.mu.Lock()
	defer js.mu.Unlock()
	return append([]time.Duration(nil), js.nakDelays[consumerID]...)
}

// Parked returns the messages that exhausted consumerID's redelivery budget (a copy).
func (js *MemJetStream) Parked(consumerID string) []Message {
	js.mu.Lock()
	defer js.mu.Unlock()
	return append([]Message(nil), js.parked[consumerID]...)
}

// Poisoned returns the messages consumerID's handler dropped as DropPoison (a copy).
func (js *MemJetStream) Poisoned(consumerID string) []Message {
	js.mu.Lock()
	defer js.mu.Unlock()
	return append([]Message(nil), js.poisoned[consumerID]...)
}

// NewMemJetStream returns an empty durable-log stand-in.
func NewMemJetStream() *MemJetStream {
	return &MemJetStream{
		log:    map[string][]Message{},
		seen:   map[string]map[string]struct{}{},
		cursor: map[string]map[string]int{},
		wakers: map[string]chan struct{}{},
	}
}

// Append durably stores msg on subj, deduping by msg.IdempotencyKey (the publish-side dedup). It
// returns true if the message was NEWLY stored, false if its key was already present (a duplicate
// publish — absorbed, not re-appended). A message with an empty IdempotencyKey is always appended
// (no dedup possible) — the durable publish path (8.5) MUST set the key, so an empty key is a caller
// bug the dedup simply cannot catch.
func (js *MemJetStream) Append(subj string, msg Message) bool {
	js.mu.Lock()
	defer js.mu.Unlock()
	if msg.IdempotencyKey != "" {
		seen := js.seen[subj]
		if seen == nil {
			seen = map[string]struct{}{}
			js.seen[subj] = seen
		}
		if _, dup := seen[msg.IdempotencyKey]; dup {
			return false
		}
		seen[msg.IdempotencyKey] = struct{}{}
	}
	msg.Subject = subj
	js.log[subj] = append(js.log[subj], msg)
	js.wakeLocked(subj) // wake any live Consume goroutine so it re-drains the new entry
	return true
}

// DeliverPending returns every entry on subj past consumerID's delivered cursor, IN APPEND ORDER, and
// advances the cursor past them — so a second call with no new appends returns nothing (the
// consumer-side dedup that suppresses a redelivery after the publish-dedup window, P8-A5). This models
// the login-time durable backlog drain (8.5): the freshly-joined consumer drains everything it has
// not yet seen, exactly once.
func (js *MemJetStream) DeliverPending(subj, consumerID string) []Message {
	js.mu.Lock()
	defer js.mu.Unlock()
	entries := js.log[subj]
	cm := js.cursor[subj]
	if cm == nil {
		cm = map[string]int{}
		js.cursor[subj] = cm
	}
	from := cm[consumerID]
	if from >= len(entries) {
		return nil
	}
	out := make([]Message, len(entries)-from)
	copy(out, entries[from:])
	cm[consumerID] = len(entries)
	return out
}

// Pending reports how many entries on subj a consumer has not yet drained (a peek that does NOT
// advance the cursor) — useful for a backlog-depth check / a paced drain (P8-A5 backlog pacing).
func (js *MemJetStream) Pending(subj, consumerID string) int {
	js.mu.Lock()
	defer js.mu.Unlock()
	n := len(js.log[subj]) - js.cursor[subj][consumerID]
	if n < 0 {
		return 0
	}
	return n
}

// PublishDurable implements the JetStream interface (jetstream.go) for the stand-in: it appends with
// publish-side idempotency dedup (Append) and discards the newly-stored bool — the durable-tell path
// cares only that the message is now in the log (a duplicate is harmlessly absorbed). A closed
// stand-in refuses, mirroring a closed broker.
func (js *MemJetStream) PublishDurable(ctx context.Context, subj string, msg Message) error {
	js.mu.Lock()
	closed := js.closed
	js.mu.Unlock()
	if closed {
		return ErrJetStreamClosed
	}
	// Producer span + traceparent into the envelope (#467), so a durable consumer links to it.
	_, msg, span := startProducer(ctx, subj, msg)
	defer span.End()
	js.Append(subj, msg)
	return nil
}

// deliverPendingForConsume is DeliverPending used by the live Consume goroutine: it returns the
// backlog past the consumer's cursor and advances it. Split out (rather than calling DeliverPending)
// so the consume path and the test-facing DeliverPending stay one implementation while reading
// clearly at the call site (the consumer DRAINS; a test PEEKS+drains). Returns nil on a closed stand-in.
func (js *MemJetStream) deliverPendingForConsume(subj, consumerID string) []Message {
	js.mu.Lock()
	if js.closed {
		js.mu.Unlock()
		return nil
	}
	js.mu.Unlock()
	return js.DeliverPending(subj, consumerID)
}

// wakeChan returns the current per-subject wake channel a consumer blocks on. It is closed-and-
// replaced by wakeLocked on the next Append, so a consumer that reads this channel AFTER its drain
// and THEN selects on it cannot miss a publish that lands in between (the publish closes the channel
// the consumer is about to wait on). Lazily creates the channel.
func (js *MemJetStream) wakeChan(subj string) <-chan struct{} {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.wakeChanLocked(subj)
}

func (js *MemJetStream) wakeChanLocked(subj string) chan struct{} {
	ch := js.wakers[subj]
	if ch == nil {
		ch = make(chan struct{})
		js.wakers[subj] = ch
	}
	return ch
}

// wake closes-and-replaces the subject's wake channel under the lock (the public form used by
// Consumer.Stop to unblock a parked run loop).
func (js *MemJetStream) wake(subj string) {
	js.mu.Lock()
	defer js.mu.Unlock()
	js.wakeLocked(subj)
}

// wakeLocked closes the current wake channel (broadcasting to every consumer blocked on it) and
// installs a fresh one for the next wait. Caller holds js.mu.
func (js *MemJetStream) wakeLocked(subj string) {
	ch := js.wakers[subj]
	if ch != nil {
		close(ch)
	}
	js.wakers[subj] = make(chan struct{})
}

// Close marks the stand-in closed and wakes every consumer so its run loop exits. Idempotent.
func (js *MemJetStream) Close() error {
	js.mu.Lock()
	if js.closed {
		js.mu.Unlock()
		return nil
	}
	js.closed = true
	for subj := range js.wakers {
		js.wakeLocked(subj)
	}
	js.mu.Unlock()
	return nil
}
