package scopebus

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/double-nibble/telosmud/internal/commbus"
)

// scopebus_nats_test.go is the GATED integration test for the durable scoped-event tier against a REAL
// NATS JetStream (the WORLD_EVENTS stream over telos.scope.>). It t.Skip without TELOS_NATS_URL so a
// local `go test ./...` with no broker passes, while the CI comms job (which exports the URL) exercises
// the real PublishDurable -> WORLD_EVENTS stream -> durable per-scope consumer round-trip — the layer
// that catches MemJetStream-vs-NATS divergences the mem stand-in can't (stream subject binding, durable
// consumer-name validation, real offline->online backlog replay).
//
// Run it with:  TELOS_NATS_URL=nats://127.0.0.1:4222 go test ./internal/scopebus/ -run RealDurable

func natsURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TELOS_NATS_URL")
	if url == "" {
		t.Skip("TELOS_NATS_URL not set; skipping NATS integration test")
	}
	return url
}

// TestScopeBusRealDurableOfflineThenOnline proves a scoped event published while NO subscriber exists
// is durably stored on the real broker and replayed as BACKLOG when a (restarted) director's durable
// consumer starts — the 10.5 "survives a director restart" guarantee over real NATS.
func TestScopeBusRealDurableOfflineThenOnline(t *testing.T) {
	url := natsURL(t)
	js, err := commbus.NewScopeJetStream(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.Close() })

	// Unique per run: a dot-free region id keeps the subject + durable consumer isolated across reruns
	// (real NATS rejects a "." in a consumer name; the consumer id is derived from it). The source (and
	// thus the idempotency key) is ALSO unique per run — JetStream publish dedup is stream-wide on the
	// Nats-Msg-Id, so a constant key would be suppressed on a rerun.
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	region := "itregion-" + suffix
	b := New(commbus.NewMemBus()).WithDurable(js, "itdirector-"+suffix)
	ctx := context.Background()

	// Publish BEFORE any subscriber (the offline case).
	require.NoError(t, b.SignalDurable(ctx, Region(region), "invasion.start", json.RawMessage(`{"wave":1}`)))

	// A director starts its durable consumer afterwards and must still receive it, flagged backlog.
	got := make(chan DurableEvent, 8)
	cons, err := b.SubscribeDurable(Region(region), "dir-"+suffix, func(ev DurableEvent) bool {
		got <- ev
		return true
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cons.Stop() })

	select {
	case ev := <-got:
		assert.Equal(t, "invasion.start", ev.Event)
		assert.JSONEq(t, `{"wave":1}`, string(ev.Payload))
		assert.True(t, ev.Backlog, "an event stored before the consumer started must replay as backlog")
	case <-time.After(5 * time.Second):
		t.Fatal("durable scoped event not replayed from the real broker")
	}
}
