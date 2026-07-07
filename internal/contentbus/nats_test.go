package contentbus

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"
)

// nats_test.go holds the GATED integration test against a real NATS broker. It requires
// TELOS_NATS_URL pointing at a running nats-server and t.Skip when unset — so a local
// `go test ./...` with no broker passes, while CI (or a dev who exports the URL) exercises the
// real publish/subscribe round-trip. It mirrors the gated Postgres store tests (store_test.go).

// natsURL returns the gated broker URL, skipping the test when TELOS_NATS_URL is unset.
func natsURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TELOS_NATS_URL")
	if url == "" {
		t.Skip("TELOS_NATS_URL not set; skipping NATS integration test")
	}
	return url
}

// TestNATSPublishSubscribe proves a published invalidation is delivered to a subscriber over a real
// broker — the production transport behind the world's hot-reload applier.
func TestNATSPublishSubscribe(t *testing.T) {
	url := natsURL(t)

	bus, err := Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer bus.Close()

	got := make(chan Invalidation, 1)
	sub, err := bus.Subscribe(func(inv Invalidation) { got <- inv })
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()

	want := Invalidation{Kind: "item", Ref: "rt:obj:torch", Pack: "reloadtest"}
	if err := bus.Publish(context.Background(), want); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case inv := <-got:
		if !reflect.DeepEqual(inv, want) {
			t.Fatalf("received %+v, want %+v", inv, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for invalidation delivery")
	}
}

// TestNATSDisabledFallback proves an unreachable broker fails fast into an error (the caller's
// disabled-bus fallback), not a hang. Uses a definitely-dead address; no broker required.
func TestNATSDisabledFallback(t *testing.T) {
	// A port nothing listens on: Connect must return an error promptly (within the connect timeout),
	// so buildShard degrades to hot-reload-disabled rather than blocking boot.
	start := time.Now()
	bus, err := Connect("nats://127.0.0.1:1")
	if err == nil {
		bus.Close()
		t.Fatal("connect to a dead broker should error")
	}
	if elapsed := time.Since(start); elapsed > connectTimeout+2*time.Second {
		t.Fatalf("connect took %v; should fail fast", elapsed)
	}
}
