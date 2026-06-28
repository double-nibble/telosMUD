package commbus

import (
	"sync"
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
}

// NewMemJetStream returns an empty durable-log stand-in.
func NewMemJetStream() *MemJetStream {
	return &MemJetStream{
		log:    map[string][]Message{},
		seen:   map[string]map[string]struct{}{},
		cursor: map[string]map[string]int{},
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
