package commbus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memjs_test.go pins the JetStream STAND-IN's two dedup layers (the model slice 8.5's durable-tell
// logic builds on): publish-side idempotency-key dedup + a consumer-side delivered cursor, both
// suppressing a redelivery (P8-A5). Append order is the per-target order 8.5 promises.

// TestMemJetStreamAppendDedup proves publish-side dedup: a re-Append of an already-seen
// IdempotencyKey is absorbed (returns false, not re-stored).
func TestMemJetStreamAppendDedup(t *testing.T) {
	js := NewMemJetStream()
	subj := TellSubject("alice")

	first := js.Append(subj, Message{IdempotencyKey: "bob:1", Body: "hi"})
	assert.True(t, first, "first append of a key is newly stored")

	dup := js.Append(subj, Message{IdempotencyKey: "bob:1", Body: "hi (redelivered)"})
	assert.False(t, dup, "a re-publish of the same key is absorbed (publish-side dedup)")

	// Only one entry survived the dedup.
	pending := js.DeliverPending(subj, "alice")
	require.Len(t, pending, 1)
	assert.Equal(t, "hi", pending[0].Body, "the original, not the redelivered duplicate, is stored")
}

// TestMemJetStreamDeliverCursor proves the consumer-side cursor: a second DeliverPending with no new
// appends returns nothing (suppressing a redelivery after the publish-dedup window), and a later
// append delivers only the new entry — the login-backlog-drain-exactly-once model.
func TestMemJetStreamDeliverCursor(t *testing.T) {
	js := NewMemJetStream()
	subj := TellSubject("alice")

	js.Append(subj, Message{IdempotencyKey: "bob:1", Body: "one"})
	js.Append(subj, Message{IdempotencyKey: "bob:2", Body: "two"})

	first := js.DeliverPending(subj, "alice")
	require.Len(t, first, 2)
	assert.Equal(t, "one", first[0].Body)
	assert.Equal(t, "two", first[1].Body)

	// A redelivery with no new appends: the cursor suppresses everything (exactly-once).
	again := js.DeliverPending(subj, "alice")
	assert.Empty(t, again, "the delivered cursor suppresses a redelivery")

	// A new append delivers only the new entry.
	js.Append(subj, Message{IdempotencyKey: "bob:3", Body: "three"})
	next := js.DeliverPending(subj, "alice")
	require.Len(t, next, 1)
	assert.Equal(t, "three", next[0].Body)
}

// TestMemJetStreamPerConsumerCursor proves cursors are per-consumer: two consumers on one subject
// each drain the full backlog independently (a future fan-out / a re-login under a fresh consumer id).
func TestMemJetStreamPerConsumerCursor(t *testing.T) {
	js := NewMemJetStream()
	subj := ChanSubject("gossip")
	js.Append(subj, Message{IdempotencyKey: "a:1", Body: "x"})

	assert.Len(t, js.DeliverPending(subj, "c1"), 1)
	assert.Len(t, js.DeliverPending(subj, "c2"), 1, "a second consumer drains independently")
	assert.Empty(t, js.DeliverPending(subj, "c1"), "c1's cursor is past the entry")
}

// TestMemJetStreamPending proves Pending peeks the backlog depth without advancing the cursor (the
// paced-drain backlog check, P8-A5).
func TestMemJetStreamPending(t *testing.T) {
	js := NewMemJetStream()
	subj := TellSubject("alice")
	js.Append(subj, Message{IdempotencyKey: "b:1", Body: "x"})
	js.Append(subj, Message{IdempotencyKey: "b:2", Body: "y"})

	assert.Equal(t, 2, js.Pending(subj, "alice"))
	assert.Equal(t, 2, js.Pending(subj, "alice"), "Pending does not advance the cursor")
	js.DeliverPending(subj, "alice")
	assert.Equal(t, 0, js.Pending(subj, "alice"))
}
