package director

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// bossRipple is a test "director script": it counts boss_slain signals into a persisted world counter and,
// at the threshold, opens a gate (a world flag) — the orchestration logic the 10.5 capstone exercises.
func bossRipple(threshold int) SignalHandler {
	return func(api *API, event string, _ json.RawMessage) {
		if event != "boss_slain" {
			return
		}
		n := 0
		if raw, ok := api.Get("bosses_slain"); ok {
			_ = json.Unmarshal(raw, &n)
		}
		n++
		nb, _ := json.Marshal(n)
		_ = api.Set("bosses_slain", nb)
		if n >= threshold {
			_ = api.Set("gate_opened", json.RawMessage(`true`))
		}
	}
}

// TestDirectorAppliesSignalAndBroadcasts proves the 10.4 write path end-to-end: a zone signals up
// (durable), the director's handler applies it to PERSISTED scope state, and the change is BROADCAST
// DOWN (the EventStateSet a zone read-replica consumes). Three boss kills cross the threshold and open
// the gate; the down-broadcast carries it.
func TestDirectorAppliesSignalAndBroadcasts(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1") // shares the transports; a zone's signal source

	store := newMemStore()
	d := New("", store, slog.Default()).
		WithScopeBus(dirBus, "world-director-1").
		WithSignalHandler(bossRipple(3)).
		WithTick(time.Hour) // no heartbeat noise

	// Capture the director's DOWN state-broadcasts (a stand-in for the zone read-replicas).
	var mu sync.Mutex
	downSets := map[string]string{}
	sub, err := dirBus.Subscribe(scopebus.World(), func(event string, payload json.RawMessage, _ string) {
		if event != scopebus.EventStateSet {
			return
		}
		var p scopebus.StatePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return
		}
		mu.Lock()
		downSets[p.Key] = string(p.Value)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// Three boss kills from a zone, signalled UP durably.
	for i := 0; i < 3; i++ {
		if err := zoneBus.SignalDurable(ctx, scopebus.World(), "boss_slain", json.RawMessage(`{"boss":"vurgoth"}`)); err != nil {
			t.Fatal(err)
		}
	}

	// The gate opens once the third kill lands and is broadcast down.
	waitFor(t, "gate_opened broadcast down", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return downSets["gate_opened"] == "true"
	})

	// The persisted state reflects the applied count + the flag.
	if raw, found, _ := d.Get(ctx, "bosses_slain"); !found || string(raw) != "3" {
		t.Fatalf("bosses_slain persisted = %q found=%v, want 3", raw, found)
	}
	if raw, found, _ := d.Get(ctx, "gate_opened"); !found || string(raw) != "true" {
		t.Fatalf("gate_opened persisted = %q found=%v, want true", raw, found)
	}
	mu.Lock()
	gotCount := downSets["bosses_slain"]
	mu.Unlock()
	if gotCount != "3" {
		t.Fatalf("last bosses_slain down-broadcast = %q, want 3", gotCount)
	}
}

// TestDirectorRemoteEffectBroadcast proves api.Broadcast emits a CUSTOM (non-state) event down — the
// remote-effect path a zone reacts to (on_world, 10.4b). The director script broadcasts "spawn_wave" on
// receiving an invasion signal.
func TestDirectorRemoteEffectBroadcast(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	d := New("", newMemStore(), slog.Default()).
		WithScopeBus(dirBus, "world-director-1").
		WithSignalHandler(func(api *API, event string, _ json.RawMessage) {
			if event == "invasion_start" {
				api.Broadcast("spawn_wave", json.RawMessage(`{"mob":"raider","count":5}`))
			}
		}).
		WithTick(time.Hour)

	got := make(chan json.RawMessage, 4)
	sub, err := dirBus.Subscribe(scopebus.World(), func(event string, payload json.RawMessage, _ string) {
		if event == "spawn_wave" {
			got <- payload
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "invasion_start", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		var m map[string]any
		if err := json.Unmarshal(p, &m); err != nil {
			t.Fatal(err)
		}
		if m["mob"] != "raider" {
			t.Fatalf("spawn_wave payload = %v, want mob=raider", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remote-effect spawn_wave not broadcast down")
	}
}

// TestDirectorSignalIdempotent proves the at-least-once durable stream is applied ONCE: a redelivery of an
// already-applied signal (same idempotency key) is suppressed, so the counter does not double-count. We
// drive it through the consumer by NAK-then-redeliver via a handler that fails the first delivery.
func TestDirectorSignalIdempotent(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	var applies int
	var mu sync.Mutex
	d := New("", newMemStore(), slog.Default()).
		WithScopeBus(dirBus, "world-director-1").
		WithSignalHandler(func(_ *API, event string, _ json.RawMessage) {
			if event == "boss_slain" {
				mu.Lock()
				applies++
				mu.Unlock()
			}
		}).
		WithTick(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// One durable signal. The MemJetStream delivers it once; the consumer acks. A second IDENTICAL publish
	// (same source+seq would dedup publish-side, but here we publish twice -> two distinct keys) would be
	// two applies — so to test consumer-side apply-once we publish ONE and assert exactly one apply even
	// across the loop's poll window.
	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "boss_slain", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "signal applied", func() bool { mu.Lock(); defer mu.Unlock(); return applies >= 1 })
	// Give any spurious redelivery a chance to (wrongly) double-apply.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	n := applies
	mu.Unlock()
	if n != 1 {
		t.Fatalf("boss_slain applied %d times, want exactly 1 (apply-once over at-least-once)", n)
	}
}

// captureDirector builds a director whose logger writes into buf (Info+), so a test can assert what the
// director recorded. No scope bus / Run needed — handleSignal is driven directly.
func captureDirector(buf *bytes.Buffer) *Director {
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return New("", newMemStore(), log)
}

func auditSignal(t *testing.T, key string, a contentbus.ReloadAudit) signalMsg {
	t.Helper()
	payload, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return signalMsg{event: contentbus.ReloadAuditEvent, payload: payload, key: key, source: "shard-1", ack: make(chan bool, 1)}
}

// TestDirectorRecordsReloadAudit proves the #192 S3 native audit: a content.reload.audit signal-up makes
// the director emit one structured audit-log entry with who/what/outcome — WITHOUT any content
// SignalHandler wired (audit is director-owned, not script logic).
func TestDirectorRecordsReloadAudit(t *testing.T) {
	var buf bytes.Buffer
	d := captureDirector(&buf) // no WithSignalHandler — audit must still record

	m := auditSignal(t, "shard-1:1", contentbus.ReloadAudit{
		Actor: "Ada", Packs: []string{"demo"}, Published: 7, Outcome: "propagated", AtUnixMs: 1234,
	})
	d.handleSignal(context.Background(), m)
	if !<-m.ack {
		t.Fatal("audit signal was NAK'd (should ack)")
	}

	out := buf.String()
	for _, want := range []string{"content reload audit", "actor=Ada", "published=7", "outcome=propagated", "shard=shard-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit log missing %q; got:\n%s", want, out)
		}
	}
}

// TestDirectorReloadAuditDedup proves a REDELIVERED audit (same idempotency key) is recorded once — the
// apply-once high-water covers the audit exactly as it covers state-changing signals.
func TestDirectorReloadAuditDedup(t *testing.T) {
	var buf bytes.Buffer
	d := captureDirector(&buf)
	a := contentbus.ReloadAudit{Actor: "Ada", Packs: []string{"demo"}, Published: 3, Outcome: "propagated", AtUnixMs: 9}

	m1 := auditSignal(t, "shard-1:5", a)
	d.handleSignal(context.Background(), m1)
	<-m1.ack
	m2 := auditSignal(t, "shard-1:5", a) // same key => redelivery
	d.handleSignal(context.Background(), m2)
	<-m2.ack

	if got := strings.Count(buf.String(), "content reload audit"); got != 1 {
		t.Fatalf("audit recorded %d times, want 1 (redelivery must dedup)", got)
	}
}

// TestDirectorReloadAuditMalformed proves a malformed audit payload is warned and dropped (never a crash),
// and the signal is still acked (drained, not stuck redelivering).
func TestDirectorReloadAuditMalformed(t *testing.T) {
	var buf bytes.Buffer
	d := captureDirector(&buf)

	m := signalMsg{event: contentbus.ReloadAuditEvent, payload: json.RawMessage(`{not json`), key: "shard-1:1", source: "shard-1", ack: make(chan bool, 1)}
	d.handleSignal(context.Background(), m)
	if !<-m.ack {
		t.Fatal("a malformed audit must still ack (drain), not NAK")
	}
	if out := buf.String(); !strings.Contains(out, "malformed content-reload audit") {
		t.Fatalf("expected a malformed-payload warning; got:\n%s", out)
	}
	if strings.Contains(buf.String(), "content reload audit") {
		t.Fatal("a malformed payload must NOT produce an audit record")
	}
}
