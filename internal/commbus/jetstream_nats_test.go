package commbus

import (
	"context"
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
	target := "itplayer-" + time.Now().Format("150405.000")
	subj := DtellSubject(target)
	ctx := context.Background()

	// Publish while the target is OFFLINE (no consumer yet).
	require.NoError(t, js.PublishDurable(ctx, subj, Message{
		AuthorID: "bob", AuthorName: "Bob", Seq: 1, IdempotencyKey: NewIdempotencyKey("bob", 1), Body: "offline tell",
	}))

	// The publish-side dedup absorbs a duplicate (same Nats-Msg-Id) within the window.
	require.NoError(t, js.PublishDurable(ctx, subj, Message{
		AuthorID: "bob", AuthorName: "Bob", Seq: 1, IdempotencyKey: NewIdempotencyKey("bob", 1), Body: "offline tell (dup)",
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
