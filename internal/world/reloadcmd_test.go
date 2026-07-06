package world

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
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

	out := s.reloader.republish(context.Background(), []string{"reloadtest"}, false)
	if out.failed || len(out.rejected) > 0 {
		t.Fatalf("republish over a healthy MemSource/MemBus should succeed: %+v", out)
	}
	if out.published != 2 {
		t.Fatalf("republish published=%d, want 2 (room + item)", out.published)
	}

	// Delivery is async (a per-subscription drain goroutine); poll until both refs land or time out.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&count) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&count); got != 2 {
		t.Fatalf("republish published %d invalidations, want 2 (room + item)", got)
	}
}

// TestReloadDoneDelivery proves the async fan-out readout reaches the builder only while they are still
// present: a reloadDoneMsg for a resident player is sent to their session; one for an absent player is a
// safe no-op (the guard that keeps the off-goroutine path from sending to a torn-down session).
func TestReloadDoneDelivery(t *testing.T) {
	z := newZone("test")
	s := &session{character: "Builder", out: make(chan *playv1.ServerFrame, 8), epoch: 1}
	z.players["Builder"] = s

	z.handle(reloadDoneMsg{player: "Builder", summary: "reload: done."})
	select {
	case f := <-s.out:
		if f == nil {
			t.Fatal("builder received a nil frame")
		}
	default:
		t.Fatal("resident builder should receive the reload readout")
	}

	// An absent player id: no delivery, no panic.
	z.handle(reloadDoneMsg{player: "Ghost", summary: "reload: done."})
	select {
	case <-s.out:
		t.Fatal("readout wrongly delivered for an absent player id")
	default:
	}
}

// TestParseReloadArgs covers the reload arg/flag split: scope in either position, the --check/-n dry-run
// flag, and the bare (all-packs) form.
func TestParseReloadArgs(t *testing.T) {
	cases := []struct {
		in    string
		scope string
		check bool
	}{
		{"", "", false},
		{"demo", "demo", false},
		{"--check", "", true},
		{"demo --check", "demo", true},
		{"--check demo", "demo", true},
		{"-n demo", "demo", true},
	}
	for _, tc := range cases {
		scope, check := parseReloadArgs(tc.in)
		if scope != tc.scope || check != tc.check {
			t.Errorf("parseReloadArgs(%q) = (%q,%v), want (%q,%v)", tc.in, scope, check, tc.scope, tc.check)
		}
	}
}

// TestReloadCheckPublishesNothing proves a --check dry run over a VALID pack validates OK but publishes
// nothing (the builder's pre-flight; #192 Slice 2).
func TestReloadCheckPublishesNothing(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(reloadTestPack())
	bus := contentbus.NewMemBus()
	defer func() { _ = bus.Close() }()
	s := newReloadShard(t, src, bus)

	var count int64
	sub, err := bus.Subscribe(func(contentbus.Invalidation) { atomic.AddInt64(&count, 1) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	out := s.reloader.republish(context.Background(), []string{"reloadtest"}, true) // checkOnly
	if !out.checkOnly || out.published != 0 || len(out.rejected) > 0 || out.failed {
		t.Fatalf("check-only over a valid pack should validate + publish nothing: %+v", out)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt64(&count); n != 0 {
		t.Fatalf("--check published %d invalidations; it must publish none", n)
	}
}
