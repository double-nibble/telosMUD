package commbus

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nats_test.go holds the GATED integration test against a real NATS broker (mirrors
// contentbus/nats_test.go + the gated Postgres store tests). It requires TELOS_NATS_URL and t.Skip
// when unset — so a local `go test ./...` with no broker passes, while CI (or a dev who exports the
// URL) exercises the real publish/subscribe round-trip AND the parity-with-MemBus claim (the same
// payload + ACL behavior over the real transport).

func natsURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TELOS_NATS_URL")
	if url == "" {
		t.Skip("TELOS_NATS_URL not set; skipping NATS integration test")
	}
	return url
}

// TestNATSPublishSubscribeParity proves the real broker delivers the same payload the MemBus does
// (mem-vs-NATS parity): a world publishes, a gate (subscribe-only) receives, the engine-set author
// and per-author seq arrive intact.
func TestNATSPublishSubscribeParity(t *testing.T) {
	url := natsURL(t)

	world, err := NewWorld(url)
	require.NoError(t, err)
	defer world.Close()
	gate, err := NewGate(url)
	require.NoError(t, err)
	defer gate.Close()

	got := make(chan Message, 1)
	sub, err := gate.Subscribe(ChanSubject("gossip"), func(m Message) { got <- m })
	require.NoError(t, err)
	defer sub.Unsubscribe()
	time.Sleep(100 * time.Millisecond) // let the subscription register on the broker

	want := Message{
		AuthorID:       "alice",
		AuthorName:     "Alice",
		Seq:            7,
		IdempotencyKey: NewIdempotencyKey("alice", 7),
		Body:           "hello over real nats",
	}
	require.NoError(t, world.Publish(context.Background(), ChanSubject("gossip"), want))

	select {
	case m := <-got:
		assert.Equal(t, want.AuthorID, m.AuthorID)
		assert.Equal(t, want.AuthorName, m.AuthorName)
		assert.Equal(t, want.Seq, m.Seq)
		assert.Equal(t, want.IdempotencyKey, m.IdempotencyKey)
		assert.Equal(t, want.Body, m.Body)
		assert.Equal(t, ChanSubject("gossip"), m.Subject)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for comms message delivery")
	}
}

// TestNATSGateACL proves the publish ACL holds over the REAL transport too: a gate handle's chan/tell
// publish is refused in-process (ErrPublishForbidden) and never reaches the broker.
func TestNATSGateACL(t *testing.T) {
	url := natsURL(t)
	gate, err := NewGate(url)
	require.NoError(t, err)
	defer gate.Close()

	require.ErrorIs(t, gate.Publish(context.Background(), ChanSubject("gossip"), Message{AuthorID: "m", Body: "forged"}), ErrPublishForbidden)
	require.ErrorIs(t, gate.Publish(context.Background(), TellSubject("bob"), Message{AuthorID: "m", Body: "forged"}), ErrPublishForbidden)
}

// TestNATSDisabledFallback proves an unreachable broker fails fast into an error (the caller's
// Disabled-bus fallback), not a hang. No broker required.
func TestNATSDisabledFallback(t *testing.T) {
	start := time.Now()
	bus, err := NewWorld("nats://127.0.0.1:1")
	if err == nil {
		bus.Close()
		t.Fatal("connect to a dead broker should error")
	}
	if elapsed := time.Since(start); elapsed > connectTimeout+2*time.Second {
		t.Fatalf("connect took %v; should fail fast", elapsed)
	}
}
