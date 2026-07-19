package director

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
	_, seq, ok := commbus.ParseIdempotencyKey(key)
	return signalMsg{event: contentbus.ReloadAuditEvent, payload: payload, seq: seq, seqOK: ok, source: "shard-1", ack: make(chan bool, 1)}
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

// TestDirectorSeqlessSignalAppliesUnconditionally proves the SeqOK=false degradation contract (#169): a
// signal whose idempotency key had no parseable seq CANNOT be deduped, so it is applied on every delivery
// and never writes the per-source high-water (writing seq 0 there would wrongly suppress a later real
// seq-1 keyed event from the same source). Foreign/corrupt keys are the only source of SeqOK=false.
func TestDirectorSeqlessSignalAppliesUnconditionally(t *testing.T) {
	var calls int
	d := New("", newMemStore(), slog.Default()).
		WithSignalHandler(func(_ *API, event string, _ json.RawMessage) {
			if event == "boss_slain" {
				calls++
			}
		})

	mk := func() signalMsg {
		return signalMsg{event: "boss_slain", seq: 0, seqOK: false, source: "shard-1", ack: make(chan bool, 1)}
	}
	m1 := mk()
	d.handleSignal(context.Background(), m1)
	if !<-m1.ack {
		t.Fatal("a seqless signal should still ack (drain), not NAK")
	}
	m2 := mk() // a "redelivery" of the same unparseable key — with no seq there is nothing to dedup on
	d.handleSignal(context.Background(), m2)
	<-m2.ack

	if calls != 2 {
		t.Fatalf("seqless signal applied %d times, want 2 (no dedup possible without a seq)", calls)
	}
	if v, ok := d.applied["shard-1"]; ok {
		t.Fatalf("seqless signal must not write the per-source high-water; got applied[shard-1]=%d", v)
	}
}

// TestDirectorReloadAuditMalformed proves a malformed audit payload is warned and dropped (never a crash),
// and the signal is still acked (drained, not stuck redelivering).
func TestDirectorReloadAuditMalformed(t *testing.T) {
	var buf bytes.Buffer
	d := captureDirector(&buf)

	m := signalMsg{event: contentbus.ReloadAuditEvent, payload: json.RawMessage(`{not json`), seq: 1, seqOK: true, source: "shard-1", ack: make(chan bool, 1)}
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

// fakePuller is a test ContentPuller: it reports each call on calls and can block (block != nil) to hold
// the single-flight slot while a test fires a second request. The block honors ctx, so a director-side
// timeout unblocks the pull (used by the timeout-releases-the-slot test) instead of wedging the test.
type fakePuller struct {
	calls chan pullArgs
	block chan struct{}
	err   error
	// forced is what the puller reports back as force-pruned, so a test can assert the record reaches the
	// operator rather than dying in the director's process (#427).
	forced []string
}

// pullArgs records what the director handed the puller. It carries `force` (#427) so a test can assert the
// override survives the whole payload -> actor -> worker path rather than being silently dropped.
type pullArgs struct {
	version, actor string
	force          bool
}

func (p *fakePuller) Pull(ctx context.Context, spec PullSpec) (PullOutcome, error) {
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return PullOutcome{}, ctx.Err() // the director's directorPullTimeout fired
		}
	}
	p.calls <- pullArgs{spec.Version, spec.Actor, spec.Force}
	return PullOutcome{ForcedPacks: p.forced}, p.err
}

func pullSignal(t *testing.T, key string, req contentbus.PullRequest) signalMsg {
	t.Helper()
	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	_, seq, ok := commbus.ParseIdempotencyKey(key)
	return signalMsg{event: contentbus.PullRequestEvent, payload: payload, seq: seq, seqOK: ok, source: "shard-1", ack: make(chan bool, 1)}
}

// pullResultCapture wires a leader director with a fakePuller and a world-scope subscriber that captures
// its pull-result down-broadcasts (#230), so a test can assert the outcome the requesting builder is told.
func pullResultCapture(t *testing.T, p *fakePuller) (*Director, <-chan contentbus.PullResult) {
	t.Helper()
	dirBus := scopebus.New(commbus.NewMemBus())
	results := make(chan contentbus.PullResult, 4)
	sub, err := dirBus.Subscribe(scopebus.World(), func(event string, payload json.RawMessage, _ string) {
		if event != contentbus.PullResultEvent {
			return
		}
		var r contentbus.PullResult
		if json.Unmarshal(payload, &r) == nil {
			results <- r
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	d := New("", newMemStore(), slog.Default()).WithContentPuller(p).WithScopeBus(dirBus, "world-director-1")
	d.leader.Store(true)
	return d, results
}

func awaitPullResult(t *testing.T, results <-chan contentbus.PullResult) contentbus.PullResult {
	t.Helper()
	select {
	case r := <-results:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("no pull-result broadcast observed")
		return contentbus.PullResult{}
	}
}

// TestDirectorPullBroadcastsSuccess (#230): a successful coordinated pull broadcasts a PullResult{OK:true}
// DOWN, carrying the version + the actor to notify — so the builder learns their install landed.
func TestDirectorPullBroadcastsSuccess(t *testing.T) {
	p := &fakePuller{calls: make(chan pullArgs, 1)}
	d, results := pullResultCapture(t, p)

	d.handleSignal(context.Background(), pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v1.2.3", Actor: "Ada"}))
	<-p.calls // the worker ran the pull; the result broadcast follows
	r := awaitPullResult(t, results)
	if !r.OK || r.Version != "v1.2.3" || r.Actor != "Ada" {
		t.Fatalf("success broadcast = %+v, want {OK:true v1.2.3 Ada}", r)
	}
}

// TestDirectorPullBroadcastsFailure (#230): a puller error broadcasts OK=false with the error detail (the
// prune-guard refusal / import failure the builder otherwise only saw in the director log).
func TestDirectorPullBroadcastsFailure(t *testing.T) {
	p := &fakePuller{calls: make(chan pullArgs, 1), err: errors.New("prune guard refused: pack in use")}
	d, results := pullResultCapture(t, p)

	d.handleSignal(context.Background(), pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v9", Actor: "Ben"}))
	<-p.calls
	r := awaitPullResult(t, results)
	if r.OK || r.Actor != "Ben" || !strings.Contains(r.Detail, "prune guard refused") {
		t.Fatalf("failure broadcast = %+v, want {OK:false Ben detail~=prune guard refused}", r)
	}
}

// TestDirectorPullBroadcastsSingleFlightDrop (#230): a second pull while one is in flight is dropped — and
// now the dropped requester is TOLD (OK=false) rather than left waiting on a request that never ran.
func TestDirectorPullBroadcastsSingleFlightDrop(t *testing.T) {
	p := &fakePuller{calls: make(chan pullArgs, 1), block: make(chan struct{})}
	d, results := pullResultCapture(t, p)

	// First request takes and HOLDS the single-flight slot (blocks in Pull).
	d.handleSignal(context.Background(), pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v1", Actor: "Ada"}))
	// Second request while the first is in flight => dropped => its requester is notified.
	d.handleSignal(context.Background(), pullSignal(t, "shard-1:2", contentbus.PullRequest{Version: "v2", Actor: "Ben"}))
	r := awaitPullResult(t, results)
	if r.OK || r.Actor != "Ben" || r.Version != "v2" {
		t.Fatalf("single-flight-drop broadcast = %+v, want {OK:false Ben v2}", r)
	}
	if !strings.Contains(r.Detail, "already in progress") {
		t.Fatalf("drop detail = %q, want it to mention a pull already in progress", r.Detail)
	}
	// Release the first pull so the worker exits cleanly.
	close(p.block)
	<-p.calls
	awaitPullResult(t, results) // drain the first pull's success broadcast
}

// TestDirectorPullAbortsAndWaitsOnShutdown (#230): the pull worker's ctx derives from the run ctx and the
// worker is tracked by d.workers, so cancelling the parent ABORTS an in-flight pull and d.workers.Wait()
// (which Run calls on ctx.Done) returns bounded rather than orphaning the goroutine. A blocking puller that
// honors ctx lets us prove both without a real 5-min timeout. Guards against a regression back to
// context.Background() or dropping the Wait.
func TestDirectorPullAbortsAndWaitsOnShutdown(t *testing.T) {
	// block is never closed: fakePuller.Pull blocks until the ctx is cancelled, then returns ctx.Err().
	p := &fakePuller{calls: make(chan pullArgs, 1), block: make(chan struct{})}
	d := New("", newMemStore(), slog.Default()).WithContentPuller(p)
	d.leader.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	m := pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v1", Actor: "Ada"})
	d.handleSignal(ctx, m) // spawns the worker with parent = ctx; the worker is now blocked in Pull
	if !<-m.ack {
		t.Fatal("pull request should ack immediately")
	}

	cancel() // simulate Run's ctx.Done: the worker's pullCtx cancels, so Pull returns promptly

	done := make(chan struct{})
	go func() { d.workers.Wait(); close(done) }()
	select {
	case <-done:
		// good: the worker finished after ctx-cancel aborted its blocked Pull, and Wait() is bounded
	case <-time.After(2 * time.Second):
		t.Fatal("d.workers.Wait() did not return after ctx cancel — the pull worker isn't bounded by the run ctx")
	}
}

// TestDirectorPullNoBroadcastWhenNotLeader (#230): a non-leader NAKs the request (redelivered to the live
// leader) and must NOT broadcast a result — the promoted leader owns the outcome, so a stray notice from a
// standby would be wrong.
func TestDirectorPullNoBroadcastWhenNotLeader(t *testing.T) {
	p := &fakePuller{calls: make(chan pullArgs, 1)}
	d, results := pullResultCapture(t, p)
	d.leader.Store(false) // override the leader default pullResultCapture set

	m := pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v1", Actor: "Ada"})
	d.handleSignal(context.Background(), m)
	if <-m.ack {
		t.Fatal("a non-leader must NAK the pull request")
	}
	select {
	case r := <-results:
		t.Fatalf("a non-leader must not broadcast a pull result, got %+v", r)
	case <-time.After(200 * time.Millisecond):
		// good: no result broadcast
	}
}

// TestDirectorCoordinatedPull: a content.pull.request signal makes the LEADER director run the puller with
// the requested version + actor, off the actor goroutine (#212 slice 4 PR E).
func TestDirectorCoordinatedPull(t *testing.T) {
	p := &fakePuller{calls: make(chan pullArgs, 1)}
	d := New("", newMemStore(), slog.Default()).WithContentPuller(p)
	d.leader.Store(true)

	m := pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v1.2.3", Actor: "Ada", AtUnixMs: 5})
	d.handleSignal(context.Background(), m)
	if !<-m.ack {
		t.Fatal("pull-request signal was NAK'd (should ack immediately)")
	}
	select {
	case got := <-p.calls:
		if got.version != "v1.2.3" || got.actor != "Ada" {
			t.Fatalf("puller called with %+v, want {v1.2.3 Ada}", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the leader director did not run the coordinated pull")
	}
}

// TestDirectorPullRequeuedWhenNotLeader: a non-leader director must NOT run the pull AND must NAK the
// durable message so it REDELIVERS to the live leader (a consume-then-demote handoff must not silently drop
// the request). It also must not advance the per-source high-water, or the redelivery would be suppressed
// as "already applied".
func TestDirectorPullRequeuedWhenNotLeader(t *testing.T) {
	p := &fakePuller{calls: make(chan pullArgs, 1)}
	d := New("", newMemStore(), slog.Default()).WithContentPuller(p)
	// leader defaults to false

	m := pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v1", Actor: "Ada"})
	d.handleSignal(context.Background(), m)
	if <-m.ack {
		t.Fatal("a non-leader must NAK the pull request (return false) so it redelivers to the leader")
	}
	if hw := d.applied["shard-1"]; hw != 0 {
		t.Fatalf("a NAK'd (unhandled) request must not advance the high-water; applied[shard-1]=%d", hw)
	}
	select {
	case got := <-p.calls:
		t.Fatalf("a non-leader director must not pull, but it called the puller: %+v", got)
	case <-time.After(300 * time.Millisecond):
		// good: no pull ran
	}
}

// TestDirectorPullRedeliversToNewLeader: the failover handoff — the SAME request (same source:seq) first
// lands on a non-leader (NAK, not applied), then redelivers to a director that is now leader, which runs
// the pull. Proves the NAK path plus the high-water NOT suppressing the redelivery.
func TestDirectorPullRedeliversToNewLeader(t *testing.T) {
	p := &fakePuller{calls: make(chan pullArgs, 1)}
	d := New("", newMemStore(), slog.Default()).WithContentPuller(p)

	m := pullSignal(t, "shard-1:7", contentbus.PullRequest{Version: "v9", Actor: "Ada"})
	// First delivery while NOT leader: NAK'd, nothing applied.
	d.handleSignal(context.Background(), m)
	if <-m.ack {
		t.Fatal("first (non-leader) delivery should NAK")
	}
	// Promotion, then redelivery of the same request.
	d.leader.Store(true)
	m2 := pullSignal(t, "shard-1:7", contentbus.PullRequest{Version: "v9", Actor: "Ada"})
	d.handleSignal(context.Background(), m2)
	if !<-m2.ack {
		t.Fatal("the redelivered request should ack on the now-leader")
	}
	select {
	case got := <-p.calls:
		if got.version != "v9" {
			t.Fatalf("redelivered pull ran with %+v, want v9", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the now-leader did not run the redelivered pull (high-water wrongly suppressed it?)")
	}
}

// TestDirectorPullSingleFlight: while one pull is in flight, a second request is dropped (not
// double-imported).
func TestDirectorPullSingleFlight(t *testing.T) {
	p := &fakePuller{calls: make(chan pullArgs, 2), block: make(chan struct{})}
	d := New("", newMemStore(), slog.Default()).WithContentPuller(p)
	d.leader.Store(true)

	// First request: acquires the single-flight slot and blocks inside Pull.
	d.handleSignal(context.Background(), pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v1", Actor: "Ada"}))
	// Give the worker goroutine a moment to reach the block.
	time.Sleep(50 * time.Millisecond)
	// Second request while the first is in flight: dropped by the single-flight guard.
	d.handleSignal(context.Background(), pullSignal(t, "shard-1:2", contentbus.PullRequest{Version: "v2", Actor: "Ben"}))

	close(p.block) // release the first pull
	select {
	case got := <-p.calls:
		if got.version != "v1" {
			t.Fatalf("first pull was %+v, want v1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first pull never ran")
	}
	// The second must NOT have run (dropped while the first held the slot).
	select {
	case got := <-p.calls:
		t.Fatalf("a second concurrent pull ran (%+v) — single-flight failed", got)
	case <-time.After(300 * time.Millisecond):
		// good
	}
}

// TestDirectorPullNilPullerAndMalformed: no puller wired, or a malformed/empty payload, is dropped
// cleanly (logged, acked, never a crash).
func TestDirectorPullNilPullerAndMalformed(t *testing.T) {
	// Nil puller: a state-only director acks + ignores.
	d := New("", newMemStore(), slog.Default())
	d.leader.Store(true)
	m := pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v1", Actor: "Ada"})
	d.handleSignal(context.Background(), m)
	if !<-m.ack {
		t.Fatal("nil-puller pull request should still ack")
	}
	// Malformed payload with a puller wired: dropped, no call.
	p := &fakePuller{calls: make(chan pullArgs, 1)}
	d2 := New("", newMemStore(), slog.Default()).WithContentPuller(p)
	d2.leader.Store(true)
	bad := signalMsg{event: contentbus.PullRequestEvent, payload: []byte("{not json"), seq: 1, seqOK: true, source: "shard-1", ack: make(chan bool, 1)}
	d2.handleSignal(context.Background(), bad)
	if !<-bad.ack {
		t.Fatal("a malformed pull request should ack (a bad payload never parses on redelivery)")
	}
	select {
	case <-p.calls:
		t.Fatal("a malformed pull request must not call the puller")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestDirectorPullEmptyVersionAcks: an empty-version request (should not happen — the command guards it —
// but defensive) is dropped-and-ACKED (not NAK'd; a redelivery of the same empty payload never improves).
func TestDirectorPullEmptyVersionAcks(t *testing.T) {
	p := &fakePuller{calls: make(chan pullArgs, 1)}
	d := New("", newMemStore(), slog.Default()).WithContentPuller(p)
	d.leader.Store(true)
	m := pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "", Actor: "Ada"})
	d.handleSignal(context.Background(), m)
	if !<-m.ack {
		t.Fatal("an empty-version request should ack (drop, not requeue)")
	}
	select {
	case <-p.calls:
		t.Fatal("an empty-version request must not call the puller")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestDirectorPullTimeoutReleasesSlot: when a pull exceeds directorPullTimeout, the worker's ctx cancels,
// Pull returns, and the single-flight slot is released — so a subsequent request is not permanently wedged.
func TestDirectorPullTimeoutReleasesSlot(t *testing.T) {
	orig := directorPullTimeout
	directorPullTimeout = 50 * time.Millisecond
	defer func() { directorPullTimeout = orig }()

	// First pull blocks past the timeout (never released via p.block); the ctx deadline unblocks it.
	p := &fakePuller{calls: make(chan pullArgs, 1), block: make(chan struct{})}
	d := New("", newMemStore(), slog.Default()).WithContentPuller(p)
	d.leader.Store(true)
	d.handleSignal(context.Background(), pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v1", Actor: "Ada"}))

	// Wait for the slot to free after the timeout fires (the deferred pulling.Store(false)).
	deadline := time.Now().Add(2 * time.Second)
	for d.pulling.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if d.pulling.Load() {
		t.Fatal("the single-flight slot was not released after the pull timed out")
	}

	// A subsequent request now proceeds (the slot is free). This puller does not block.
	p2 := &fakePuller{calls: make(chan pullArgs, 1)}
	d.puller = p2
	d.handleSignal(context.Background(), pullSignal(t, "shard-1:2", contentbus.PullRequest{Version: "v2", Actor: "Ben"}))
	select {
	case got := <-p2.calls:
		if got.version != "v2" {
			t.Fatalf("post-timeout pull ran with %+v, want v2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a request after a timed-out pull did not proceed (slot still held?)")
	}
}

// TestDirectorPassesForceThroughToThePuller pins the #427 override across the full payload -> actor ->
// worker path. The gate on Force is entirely shard-side (the pull signal is not signed), so the director's
// only job is to carry the flag faithfully; a director that dropped it would silently turn every forced
// pull back into a vetoed one, and the operator's override would appear to do nothing.
func TestDirectorPassesForceThroughToThePuller(t *testing.T) {
	for _, force := range []bool{false, true} {
		p := &fakePuller{calls: make(chan pullArgs, 1)}
		d := New("", newMemStore(), slog.Default()).WithContentPuller(p)
		d.leader.Store(true)

		// A FRESH timestamp: a forced request older than forceMaxAge is deliberately downgraded (see
		// forceTooStale), so a 1970 stamp would test the downgrade rather than the pass-through.
		m := pullSignal(t, "shard-1:1", contentbus.PullRequest{
			Version: "v1", Actor: "Ada", AtUnixMs: time.Now().UnixMilli(), Force: force,
		})
		d.handleSignal(context.Background(), m)
		if !<-m.ack {
			t.Fatal("pull-request signal was NAK'd")
		}
		select {
		case got := <-p.calls:
			if got.force != force {
				t.Fatalf("puller received force=%v, want %v", got.force, force)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the leader director did not run the coordinated pull")
		}
	}
}

// TestDirectorDowngradesAStaleForcedPull covers the freshness bound (#427). A forced pull waives a guard
// whose entire value is that it reflects CURRENT occupancy, on the strength of a judgement the operator made
// when they typed the command. The request rides a DURABLE at-least-once stream that deliberately NAKs on a
// non-leader, so a failover can delay delivery arbitrarily — long enough for the one idle player the
// operator waived to have become a raid. A stale override is downgraded to an ordinary guarded pull.
func TestDirectorDowngradesAStaleForcedPull(t *testing.T) {
	p := &fakePuller{calls: make(chan pullArgs, 1)}
	d := New("", newMemStore(), slog.Default()).WithContentPuller(p)
	d.leader.Store(true)

	stale := time.Now().Add(-2 * forceMaxAge).UnixMilli()
	m := pullSignal(t, "shard-1:1", contentbus.PullRequest{Version: "v1", Actor: "Ada", AtUnixMs: stale, Force: true})
	d.handleSignal(context.Background(), m)
	require.True(t, <-m.ack)

	select {
	case got := <-p.calls:
		require.False(t, got.force,
			"a forced pull older than forceMaxAge must be downgraded to a guarded pull — the guard re-vetoes "+
				"and the operator re-issues against what is true NOW")
	case <-time.After(2 * time.Second):
		t.Fatal("the pull did not run")
	}
}

// TestForceTooStale covers the boundary directly, including the zero-timestamp case: an absent AtUnixMs
// predates the field rather than being ancient, so treating it as stale would silently disable the override
// for an older shard instead of protecting anything.
func TestForceTooStale(t *testing.T) {
	require.False(t, forceTooStale(0), "an absent timestamp must not be treated as stale")
	require.False(t, forceTooStale(-1))
	require.False(t, forceTooStale(time.Now().UnixMilli()))
	require.False(t, forceTooStale(time.Now().Add(-forceMaxAge/2).UnixMilli()))
	require.True(t, forceTooStale(time.Now().Add(-2*forceMaxAge).UnixMilli()))
}

// TestPullResultDetailCarriesTheForcedPacks is the fix for the finding that Result.PruneForced was
// write-only state. The director runs on a different host from the builder who typed the command; before
// this, the packs an operator overrode were computed, logged locally, and dropped — so the in-game success
// line for a forced pull was byte-identical to an ordinary one. An override whose consequences never reach
// the operator is not an audited action.
func TestPullResultDetailCarriesTheForcedPacks(t *testing.T) {
	require.Empty(t, pullResultDetail(PullOutcome{}),
		"an ordinary pull's success line must be unchanged")
	require.Empty(t, pullResultDetail(PullOutcome{ForcedPacks: nil}),
		"a forced pull that blocked nothing is an ordinary pull")
	require.Equal(t, "dungeons, raids", pullResultDetail(PullOutcome{ForcedPacks: []string{"dungeons", "raids"}}),
		"a real override must name the packs, so the operator learns what they overrode")
}
