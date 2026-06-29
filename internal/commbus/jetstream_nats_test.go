package commbus

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jetstream_nats_test.go holds the GATED integration test for the durable-tell transport against a
// REAL NATS JetStream (mirrors nats_test.go). It requires TELOS_NATS_URL (with JetStream enabled,
// e.g. `nats-server -js`) and t.Skip when unset — so a local `go test ./...` with no broker passes,
// while CI / a dev who exports the URL exercises the real PublishDurable -> durable backlog ->
// per-player consumer round-trip and the offline-then-online delivery the MemJetStream stands in for.
//
// Run it with:  TELOS_NATS_URL=nats://127.0.0.1:4222 go test ./internal/commbus/ -run JetStreamReal

// TestJetStreamRealOfflineThenOnline proves a tell published while NO consumer exists (the offline
// case) is durably stored and delivered when a per-player consumer starts (the login drain) — the
// durable-always done-when, over the real broker. Uses a unique consumer id per run so reruns don't
// collide on a left-over durable.
func TestJetStreamRealOfflineThenOnline(t *testing.T) {
	url := natsURL(t)
	js, err := NewJetStream(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.Close() })

	// A unique target per run keeps the per-target subject + durable consumer isolated across reruns.
	// It MUST be dot-free: real NATS JetStream rejects a consumer name containing "." (the subject-token
	// separator) with "invalid consumer name", and the consumer name is derived from the target. A
	// UnixNano suffix is unique and dot-free (a timestamp like "150405.000" embeds a dot and fails only
	// against real NATS — MemJetStream does not validate the name, which is why this test must run gated
	// against a real broker, not just the mem stand-in). Real player ids are dot-free (gate-enforced).
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	target := "itplayer-" + suffix
	// The author (and thus the idempotency key) is ALSO unique per run. JetStream's publish dedup is
	// STREAM-WIDE on the Nats-Msg-Id (the idempotency key), so a constant key would be suppressed on a
	// rerun against the same broker (within the dedup window) — the publish would silently no-op and the
	// consumer would time out. A per-run author keeps the dedup assertion below honest within the run
	// while staying collision-free across runs / `-count>1`. (Real authors are distinct players.)
	author := "bob-" + suffix
	subj := DtellSubject(target)
	ctx := context.Background()

	// Publish while the target is OFFLINE (no consumer yet).
	require.NoError(t, js.PublishDurable(ctx, subj, Message{
		AuthorID: author, AuthorName: "Bob", Seq: 1, IdempotencyKey: NewIdempotencyKey(author, 1), Body: "offline tell",
	}))

	// The publish-side dedup absorbs a duplicate (same Nats-Msg-Id) within the window.
	require.NoError(t, js.PublishDurable(ctx, subj, Message{
		AuthorID: author, AuthorName: "Bob", Seq: 1, IdempotencyKey: NewIdempotencyKey(author, 1), Body: "offline tell (dup)",
	}))

	got := make(chan Message, 4)
	cons, err := js.Consume(subj, target, func(m Message, _ bool) bool { got <- m; return true })
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	select {
	case m := <-got:
		assert.Equal(t, "offline tell", m.Body, "the durable tell is delivered on consumer start")
		assert.Equal(t, "Bob", m.AuthorName)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the durable offline tell")
	}
	// No duplicate (publish-side dedup).
	select {
	case m := <-got:
		t.Fatalf("unexpected duplicate delivery: %+v", m)
	case <-time.After(500 * time.Millisecond):
	}
}
