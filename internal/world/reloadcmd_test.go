package world

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// auditScopeShard builds a minimal Shard with a scopeReplication whose signal queue a test can read, to
// observe what auditReload enqueues without a live scoped bus.
func auditScopeShard() *Shard {
	return &Shard{scopes: &scopeReplication{signals: make(chan scopeSignalJob, 4), log: slog.Default()}}
}

// TestAuditReloadEnqueuesSignal proves a real reload fires a world-scoped content.reload.audit signal-up
// carrying who/what/outcome (#192 S3).
func TestAuditReloadEnqueuesSignal(t *testing.T) {
	sh := auditScopeShard()
	auditReload(sh, "Ada", []string{"demo"}, reloadOutcome{published: 7})

	select {
	case j := <-sh.scopes.signals:
		if j.event != contentbus.ReloadAuditEvent {
			t.Fatalf("event = %q, want %q", j.event, contentbus.ReloadAuditEvent)
		}
		if j.scope.Label() != scopebus.World().Label() {
			t.Fatalf("scope = %q, want world (a content reload is a fleet event)", j.scope.Label())
		}
		var a contentbus.ReloadAudit
		if err := json.Unmarshal(j.payload, &a); err != nil {
			t.Fatal(err)
		}
		if a.Actor != "Ada" || a.Published != 7 || a.Outcome != "propagated" || len(a.Packs) != 1 || a.AtUnixMs == 0 {
			t.Fatalf("audit payload = %+v", a)
		}
	default:
		t.Fatal("no audit signal enqueued for a real reload")
	}
}

// TestAuditReloadSkipsCheckOnly proves a `--check` dry run is NOT audited (it changed nothing on the fleet).
func TestAuditReloadSkipsCheckOnly(t *testing.T) {
	sh := auditScopeShard()
	auditReload(sh, "Ada", []string{"demo"}, reloadOutcome{checkOnly: true})
	select {
	case <-sh.scopes.signals:
		t.Fatal("a --check dry run must not be audited")
	default:
	}
}

// TestAuditReloadOutcomeMapping pins the reloadOutcome -> audit outcome string mapping.
func TestAuditReloadOutcomeMapping(t *testing.T) {
	cases := []struct {
		out  reloadOutcome
		want string
	}{
		{reloadOutcome{published: 5}, "propagated"},
		{reloadOutcome{rejected: []string{"bad def"}}, "rejected"},
		{reloadOutcome{failed: true}, "failed"},
		{reloadOutcome{failed: true, published: 3}, "partial"},
	}
	for _, c := range cases {
		sh := auditScopeShard()
		auditReload(sh, "Ada", []string{"demo"}, c.out)
		j := <-sh.scopes.signals
		var a contentbus.ReloadAudit
		if err := json.Unmarshal(j.payload, &a); err != nil {
			t.Fatal(err)
		}
		if a.Outcome != c.want {
			t.Fatalf("outcome for %+v = %q, want %q", c.out, a.Outcome, c.want)
		}
	}
}

// TestAuditReloadNilScopesNoOp proves a shard with no scoped bus (sh.scopes nil) audits nothing and never
// panics — the audit is best-effort and must never affect the reload.
func TestAuditReloadNilScopesNoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("auditReload panicked with no scoped bus: %v", r)
		}
	}()
	sh := &Shard{} // no scoped bus
	auditReload(sh, "Ada", []string{"demo"}, reloadOutcome{published: 1})
}

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

	// Delivery is async (a per-subscription drain goroutine); poll until all three land or time out. The
	// WIRE carries three invalidations — the room, the item, AND a zone-SHAPE invalidation (which drives the
	// live-room-deletion reconcile, #191) — even though out.published counts only the 2 spawnable prototypes.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&count) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&count); got != 3 {
		t.Fatalf("republish delivered %d invalidations, want 3 (room + item + zone-shape)", got)
	}
}

// TestReloadRejectsBrokenReset proves the #197 pre-publish gate is actually WIRED into republish (the seam
// a validateResets unit test can't cover): a pack whose zone reset references an undefined intra-zone
// prototype is REJECTED, so republish publishes NOTHING and ZERO invalidations reach the bus. Without the
// validatePacks call in republish this test goes red — every other republish test stays green.
func TestReloadRejectsBrokenReset(t *testing.T) {
	pack := reloadTestPack()
	// A reset spawning a prototype the zone does not define — applyReset would log-and-skip it (spawns
	// nothing), so the gate rejects the pack before broadcasting it.
	pack.Zones[0].Resets = []content.ResetDTO{
		{Op: "spawn_mob", Proto: "rt:mob:ghost", Room: "rt:room:hall"},
	}
	src := content.NewMemSource()
	src.SetPack(pack)
	bus := contentbus.NewMemBus()
	defer func() { _ = bus.Close() }()
	s := newReloadShard(t, src, bus)

	var count int64
	sub, err := bus.Subscribe(func(contentbus.Invalidation) { atomic.AddInt64(&count, 1) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	out := s.reloader.republish(context.Background(), []string{"reloadtest"}, false)
	if len(out.rejected) == 0 {
		t.Fatalf("republish must REJECT a pack with a dangling reset proto; got %+v", out)
	}
	if out.published != 0 || out.failed {
		t.Fatalf("a rejected pack must publish nothing and not be an infra failure; got %+v", out)
	}
	// Give any (erroneous) async fan-out a chance to land, then assert the bus saw NOTHING.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&count); got != 0 {
		t.Fatalf("a rejected reload must put ZERO invalidations on the bus; got %d", got)
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

// TestMintReloadVersion covers the #222 fix: a shard-local reload's version is max(now_nanos, pgVersion+1),
// so it always advances past the current Postgres content version even when this shard's clock lags the
// host that minted a just-completed pull (the cross-host clock-skew race that would otherwise drop the
// reload's zone-shape reconcile as stale).
func TestMintReloadVersion(t *testing.T) {
	src := content.NewMemSource()
	r := &reloader{src: src, log: slog.Default()}

	// PG version well ABOVE now-nanos simulates this shard's clock lagging the puller: the pgVersion+1 floor
	// must win, so the reload still beats the pull's version.
	future := uint64(time.Now().UnixNano()) + uint64(time.Hour)
	src.SetContentVersion(future)
	if got, ok := r.mintReloadVersion(context.Background()); !ok || got != future+1 {
		t.Fatalf("mint with PG ahead = %d (ok %v), want pgVersion+1 = %d, ok", got, ok, future+1)
	}

	// PG version BELOW now-nanos is the common case: wall-clock nanos win, and the stamp still exceeds the
	// PG version (so the reconcile guard accepts it).
	src.SetContentVersion(1)
	before := uint64(time.Now().UnixNano())
	got, ok := r.mintReloadVersion(context.Background())
	if !ok {
		t.Fatal("a mem source (no PG authority) must mint ok=true via the wall-clock fallback")
	}
	if got < before {
		t.Fatalf("mint with PG behind = %d, want >= now-nanos %d (wall clock should win)", got, before)
	}
	if got <= 1 {
		t.Fatalf("mint = %d must exceed the PG version (1)", got)
	}
}

// TestMintReloadVersionFallsBackToNanos proves a source that cannot report a content version (no
// contentVersioner) degrades to bare nanos rather than failing.
func TestMintReloadVersionFallsBackToNanos(t *testing.T) {
	r := &reloader{src: noVersionSource{}, log: slog.Default()}
	before := uint64(time.Now().UnixNano())
	got, ok := r.mintReloadVersion(context.Background())
	if !ok {
		t.Fatal("a source with no PG authority must mint ok=true (bare nanos), not fail the reload")
	}
	if got < before {
		t.Fatalf("fallback mint = %d, want >= now-nanos %d", got, before)
	}
}

// noVersionSource is a content.DefinitionSource that deliberately does NOT implement contentVersioner, to
// exercise mintReloadVersion's bare-nanos fallback. Its methods are never called by the mint path.
type noVersionSource struct{}

func (noVersionSource) LoadDefinition(context.Context, string, string, string) (content.Definition, error) {
	return content.Definition{}, nil
}

// bumpSource is a content.DefinitionSource that implements BOTH contentVersioner and contentVersionBumper,
// modelling *store.Pool for the #232 durable mint path: BumpContentVersion atomically increments a counter
// (or returns bumpErr). Its content methods are never called by the mint path.
type bumpSource struct {
	ver     uint64
	bumpErr error
}

func (*bumpSource) LoadDefinition(context.Context, string, string, string) (content.Definition, error) {
	return content.Definition{}, nil
}
func (b *bumpSource) ContentVersion(context.Context) (uint64, error) { return b.ver, nil }
func (b *bumpSource) BumpContentVersion(context.Context) (uint64, error) {
	if b.bumpErr != nil {
		return 0, b.bumpErr
	}
	b.ver++
	return b.ver, nil
}

// TestMintReloadVersionBumpsPG covers the #232 DURABLE path: with a PG-authority source, a reload mints via
// an atomic version bump — a small monotonic counter, NOT wall-clock nanos — so a clock-AHEAD shard can no
// longer stamp a far-future version. Successive reloads advance monotonically off the single authority.
func TestMintReloadVersionBumpsPG(t *testing.T) {
	b := &bumpSource{ver: 5}
	r := &reloader{src: b, log: slog.Default()}
	now := uint64(time.Now().UnixNano())

	got, ok := r.mintReloadVersion(context.Background())
	if !ok || got != 6 {
		t.Fatalf("bump mint = %d (ok %v), want the atomic PG bump 6 (5+1), ok", got, ok)
	}
	// The load-bearing property: the durable path is CLOCK-FREE. A wall-clock stamp would be ~1.7e18; the
	// bump is a small counter, well below now-nanos — so no clock-ahead poisoning is possible.
	if got >= now {
		t.Fatalf("bump mint = %d must be the small PG counter, not wall-clock nanos (%d)", got, now)
	}
	if got2, ok := r.mintReloadVersion(context.Background()); !ok || got2 != 7 {
		t.Fatalf("second reload bump = %d (ok %v), want 7 (monotonic off the PG authority)", got2, ok)
	}
}

// TestMintReloadVersionBumpErrorFailsReload proves the #232 fix distsys review found: a PG-authority source
// whose bump ERRORS must FAIL the reload (ok=false), NOT stamp a wall-clock version — a wall-clock stamp
// would sit above the un-bumped PG counter and silently drop a later durable reload's shape reconcile
// fleet-wide. It also pins that the bump was genuinely attempted (the fake returns before incrementing on
// error, so ver stays 1) — distinguishing "bump failed" from "bump never ran".
func TestMintReloadVersionBumpErrorFailsReload(t *testing.T) {
	b := &bumpSource{ver: 1, bumpErr: errors.New("pg unavailable")}
	r := &reloader{src: b, log: slog.Default()}
	got, ok := r.mintReloadVersion(context.Background())
	if ok {
		t.Fatalf("a bumper's bump error must FAIL the reload (ok=false), got version %d ok=true", got)
	}
	if got != 0 {
		t.Fatalf("a failed mint must return version 0, got %d", got)
	}
	if b.ver != 1 {
		t.Fatalf("the bump must have been ATTEMPTED (fake errors before incrementing); ver=%d, want 1", b.ver)
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
