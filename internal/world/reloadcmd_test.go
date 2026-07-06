package world

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// TestReloadScopePacks covers the `reload` scope resolution: bare/"all" => every loaded pack, a valid
// name => just it, an unknown name => a loud error (never a silent no-op).
func TestReloadScopePacks(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer func() { _ = bus.Close() }()
	s := newReloadShard(t, src, bus)
	r := s.reloader

	for _, arg := range []string{"", "all"} {
		got, msg := r.scopePacks(arg)
		if msg != "" {
			t.Fatalf("scopePacks(%q) unexpected message: %s", arg, msg)
		}
		if len(got) != 1 || got[0] != "reloadtest" {
			t.Fatalf("scopePacks(%q) = %v, want [reloadtest]", arg, got)
		}
	}

	got, msg := r.scopePacks("reloadtest")
	if msg != "" || len(got) != 1 || got[0] != "reloadtest" {
		t.Fatalf("scopePacks(reloadtest) = %v, msg=%q", got, msg)
	}

	if _, msg := r.scopePacks("nope"); msg == "" {
		t.Fatal("scopePacks(nope) should return an error message for an unloaded pack")
	}
}

// TestReloadRepublish proves the command's engine: republish re-reads the pack from the shard's content
// source and publishes a per-ref invalidation for every prototype, so a subscribed shard hot-swaps. The
// reloadtest pack has one room + one item => two invalidations.
func TestReloadRepublish(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer func() { _ = bus.Close() }()
	s := newReloadShard(t, src, bus)

	if !s.reloader.canRepublish() {
		t.Fatal("MemSource should implement content.Source (canRepublish=true)")
	}

	var count int64
	sub, err := bus.Subscribe(func(contentbus.Invalidation) { atomic.AddInt64(&count, 1) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	s.reloader.republish(context.Background(), []string{"reloadtest"})

	// Delivery is async (a per-subscription drain goroutine); poll until both refs land or time out.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&count) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&count); got != 2 {
		t.Fatalf("republish published %d invalidations, want 2 (room + item)", got)
	}
}
