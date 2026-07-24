package world

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// audit_test.go — hermetic (MemStore-backed) tests for the #350 durable audit trail: the world emit
// points (death / attribute-base grant / track step) record exactly-once with the right subject/actor,
// a mob or a not-yet-saved player emits nothing, and the `audit` read command scopes the self-view to
// the caller while gating a named lookup to staff. No live Postgres — the MemStore mirrors the pgx
// contract, and a gated store test (internal/store/audit_test.go) pins the SQL.

// withAuditor attaches an audit-enabled shard (a MemStore sink) to a bare test zone and starts the
// auditor drainer for the test's lifetime, so an emit helper's enqueue actually reaches the store off
// the zone goroutine. Returns the sink for inspection.
func withAuditor(t *testing.T, z *Zone) *MemStore {
	t.Helper()
	ms := NewMemStore()
	z.shard = NewDemoShard().WithAudit(ms)
	ctx, cancel := context.WithCancel(context.Background())
	go z.shard.auditor.run(ctx)
	t.Cleanup(cancel)
	return ms
}

// setPID stamps a durable persist id on a test player's entity, so the audit emit guards (which skip a
// pid==nil player) treat it as a saved character.
func setPID(s *session, id string) {
	p := PersistID(id)
	s.entity.pid = &p
}

// waitAuditKind polls the trail for `name` until at least `want` rows of `kind` exist (or times out),
// returning those rows. The emit path is async (the auditor drains on its own goroutine), so a read
// must poll rather than assume synchronous visibility.
func waitAuditKind(t *testing.T, ms *MemStore, name, kind string, want int) []AuditEntry {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		all, _ := ms.ListAuditForCharacterName(context.Background(), name, 100)
		var got []AuditEntry
		for _, e := range all {
			if e.EventKind == kind {
				got = append(got, e)
			}
		}
		if len(got) >= want {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d %q rows for %s (have %d)", want, kind, name, len(got))
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// settle gives the async auditor a moment to drain any (unexpected) enqueued event, so a "no new rows"
// assertion is meaningful — a mis-fire would have landed by the time this returns.
func settle() { time.Sleep(50 * time.Millisecond) }

// TestAuditDeathRecordsOnce proves a player death through the real funnel records exactly one `died` row
// (the die() l.dying latch is the re-entrancy guard — a re-entrant die() never reaches the emit), and a
// mob death emits nothing.
func TestAuditDeathRecordsOnce(t *testing.T) {
	z, _ := abilityTestZone(t)
	ms := withAuditor(t, z)

	victim := makeRoomPlayer(z, "Victim")
	setPID(victim, "11111111-1111-1111-1111-111111111111")
	setResourceCurrent(victim.entity, "hp", 10)

	// A system death (no killer) through the real funnel: one row, attributed to the system.
	z.die(victim.entity, nil, nil)
	rows := waitAuditKind(t, ms, "Victim", AuditKindDied, 1)
	if len(rows) != 1 {
		t.Fatalf("one death -> %d died rows, want 1", len(rows))
	}
	if rows[0].ActorType != AuditActorSystem || rows[0].SubjectType != AuditSubjectCharacter {
		t.Fatalf("death row subject/actor = %s/%s, want character/system", rows[0].SubjectType, rows[0].ActorType)
	}

	// A mob death emits nothing (the isPlayer guard).
	before := ms.auditCount()
	mob := makeMobTarget(z, victim.entity, "goblin")
	setResourceCurrent(mob, "hp", 5)
	z.die(mob, victim.entity, nil)
	settle()
	if after := ms.auditCount(); after != before {
		t.Fatalf("a mob death recorded %d audit rows, want 0", after-before)
	}
}

// TestAuditDeathDistinctAcrossGenerationReset is the HIGH-1 regression: the died dedup_key must NOT be the
// transient Living.deaths counter. That counter resets to its low values in a fresh process (a relog / a
// cross-shard handoff), so two real deaths would present the SAME (pid, "died", generation) key and the
// second would be silently dropped by ON CONFLICT DO NOTHING — losing nearly every death after the first.
// With a fresh per-death UUID key, two deaths at the SAME generation value each record their own row.
func TestAuditDeathDistinctAcrossGenerationReset(t *testing.T) {
	z, _ := abilityTestZone(t)
	ms := withAuditor(t, z)

	victim := makeRoomPlayer(z, "Victim")
	setPID(victim, "22222222-2222-2222-2222-222222222222")

	// Two deaths BOTH at generation 1 — the exact collision a relog produces (deaths reset to 0, bumped to 1
	// again). Each must record its own row; the transient-counter bug would have dropped the second.
	z.auditPlayerDeath(victim.entity, nil, 1)
	z.auditPlayerDeath(victim.entity, nil, 1)
	rows := waitAuditKind(t, ms, "Victim", AuditKindDied, 2)
	if len(rows) != 2 {
		t.Fatalf("two deaths at the same generation -> %d died rows, want 2 (no counter collision)", len(rows))
	}
}

// TestAuditDeathByPlayerCarriesKiller proves a player-on-player kill attributes the killer as the actor
// (a character with a pid) and carries the killer name in the payload.
func TestAuditDeathByPlayerCarriesKiller(t *testing.T) {
	z, _ := abilityTestZone(t)
	ms := withAuditor(t, z)

	victim := makeRoomPlayer(z, "Prey")
	setPID(victim, "22222222-2222-2222-2222-222222222222")
	setResourceCurrent(victim.entity, "hp", 5)
	killer := makeRoomPlayer(z, "Hunter")
	setPID(killer, "33333333-3333-3333-3333-333333333333")

	z.die(victim.entity, killer.entity, nil)
	rows := waitAuditKind(t, ms, "Prey", AuditKindDied, 1)
	if rows[0].ActorType != AuditActorCharacter || rows[0].ActorID != "33333333-3333-3333-3333-333333333333" {
		t.Fatalf("killer actor = %s/%s, want character/<hunter pid>", rows[0].ActorType, rows[0].ActorID)
	}
	if kn, _ := rows[0].Payload["killer_name"].(string); kn != "Hunter" {
		t.Fatalf("payload killer_name = %q, want Hunter", kn)
	}
}

// TestAuditAttributeBase proves one attribute_base_changed row per grant with old/new/delta, a MOB target
// records nothing, and a pid==nil (not-yet-saved) player records nothing.
func TestAuditAttributeBase(t *testing.T) {
	z, caster := abilityTestZone(t)
	ms := withAuditor(t, z)
	setPID(caster, "44444444-4444-4444-4444-444444444444")

	c := seededCtx(z, caster.entity, caster.entity, dispHelpful)
	if err := opModifyAttributeBase(c, &effectOp{attr: "strength", amount: 4}); err != nil {
		t.Fatal(err)
	}
	rows := waitAuditKind(t, ms, "Caster", AuditKindAttributeBase, 1)
	e := rows[0]
	if a, _ := e.Payload["attr"].(string); a != "strength" {
		t.Fatalf("attr = %q, want strength", a)
	}
	if o, _ := e.Payload["old"].(float64); o != 10 {
		t.Fatalf("old = %v, want 10", e.Payload["old"])
	}
	if n, _ := e.Payload["new"].(float64); n != 14 {
		t.Fatalf("new = %v, want 14", e.Payload["new"])
	}
	if d, _ := e.Payload["delta"].(float64); d != 4 {
		t.Fatalf("delta = %v, want 4", e.Payload["delta"])
	}

	// A second grant is a DISTINCT row (a fresh-UUID dedup key — each grant is its own event).
	if err := opModifyAttributeBase(c, &effectOp{attr: "strength", amount: 1}); err != nil {
		t.Fatal(err)
	}
	waitAuditKind(t, ms, "Caster", AuditKindAttributeBase, 2)

	// A MOB target records nothing (no pid / not a player).
	before := ms.auditCount()
	mob := makeMobTarget(z, caster.entity, "goblin")
	cm := seededCtx(z, caster.entity, mob, dispHelpful)
	if err := opModifyAttributeBase(cm, &effectOp{attr: "strength", amount: 1}); err != nil {
		t.Fatal(err)
	}
	settle()
	if ms.auditCount() != before {
		t.Fatalf("a mob attribute grant recorded a row, want 0")
	}

	// A pid==nil player records nothing (the async-create window).
	caster.entity.pid = nil
	if err := opModifyAttributeBase(c, &effectOp{attr: "strength", amount: 1}); err != nil {
		t.Fatal(err)
	}
	settle()
	if ms.auditCount() != before {
		t.Fatalf("a pid-less player attribute grant recorded a row, want 0")
	}
}

// TestAuditTrackAdvance proves crossing N thresholds records N track_advanced rows with ascending steps
// (distinct dedup keys — proven by their coexistence under the unique index), and a re-advance crossing
// no new step records nothing more.
func TestAuditTrackAdvance(t *testing.T) {
	z, caster := trackTestZone(t) // "hero" track, thresholds 100/250/500
	ms := withAuditor(t, z)
	setPID(caster, "55555555-5555-5555-5555-555555555555")

	c := seededCtx(z, caster.entity, caster.entity, dispHelpful)
	// 600 xp crosses all three thresholds at once.
	if err := opAdvanceTrack(c, &effectOp{track: "hero", amount: 600}); err != nil {
		t.Fatal(err)
	}
	rows := waitAuditKind(t, ms, "Caster", AuditKindTrackAdvanced, 3)
	if len(rows) != 3 {
		t.Fatalf("crossing 3 thresholds -> %d track rows, want 3", len(rows))
	}
	// The three rows carry steps 1/2/3 (order-independent: collect the set).
	steps := map[int]bool{}
	for _, e := range rows {
		if s, ok := e.Payload["step"].(float64); ok {
			steps[int(s)] = true
		}
		if e.Payload["track"] != "hero" {
			t.Fatalf("track row payload track = %v, want hero", e.Payload["track"])
		}
	}
	for want := 1; want <= 3; want++ {
		if !steps[want] {
			t.Fatalf("missing track row for step %d (have %v)", want, steps)
		}
	}

	// A re-advance that crosses no NEW step (already at the top) records no more track rows.
	trackRows := len(rows)
	if err := opAdvanceTrack(c, &effectOp{track: "hero", amount: 0}); err != nil {
		t.Fatal(err)
	}
	settle()
	if got := waitAuditKind(t, ms, "Caster", AuditKindTrackAdvanced, trackRows); len(got) != trackRows {
		t.Fatalf("re-advance crossing no step -> %d track rows, want %d", len(got), trackRows)
	}
}

// waitAuditLine polls a session's out channel for an output frame containing substr (draining prompts).
func waitAuditLine(t *testing.T, s *session, substr string) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
				return o.GetMarkup()
			}
		case <-deadline:
			t.Fatalf("player %s: timed out waiting for %q", s.character, substr)
			return ""
		}
	}
}

// TestAuditCommand covers the read surface: a storeless shard degrades, the self-view is scoped to the
// caller's own name, a mortal cannot query another subject, and a staff member can.
func TestAuditCommand(t *testing.T) {
	// Storeless: `audit` degrades to a notice, never a crash.
	bare := NewDemoShard().Zone()
	mort := newTestPlayerEntity(bare, "Nobody")
	bare.dispatch(mort, "audit")
	waitAuditLine(t, mort, "Audit is unavailable.")

	// Audit-enabled shard, seeded directly (the command reads via its own goroutine; no drainer needed).
	ms := NewMemStore()
	sh := NewDemoShard().WithAudit(ms)
	z := sh.Zone()
	ctx := context.Background()
	seed := func(name string) {
		if _, err := ms.AppendAudit(ctx, AuditEvent{
			SubjectType: AuditSubjectCharacter, SubjectID: "id-" + name, SubjectName: name,
			ActorType: AuditActorSystem, EventKind: AuditKindDied, DedupKey: "1",
			Payload: AuditPayload(map[string]any{"room_ref": "midgaard:room:temple"}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("Alice")
	seed("Bob")

	// Self-view: Alice sees her OWN trail with a bare `audit`. The self-view is scoped by the caller's
	// stable pid (subject_id), so Alice's entity carries the pid the seed used ("id-Alice").
	alice := newTestPlayerEntity(z, "Alice")
	setPID(alice, "id-Alice")
	z.dispatch(alice, "audit")
	waitAuditLine(t, alice, "Audit history for Alice")

	// A mortal cannot query ANOTHER subject — refused before the store is touched.
	z.dispatch(alice, "audit Bob")
	waitAuditLine(t, alice, "only view your own")

	// A staff member (admin tier) CAN query another subject.
	admin := newTestPlayerEntity(z, "Admin")
	admin.tier = tierAdmin
	z.dispatch(admin, "audit Bob")
	waitAuditLine(t, admin, "Audit history for Bob")
}

// TestAuditTierReachableByCharacter is #399 item 4: an ACCOUNT tier_changed row (subject_type=account,
// subject_name NULL) is invisible to the by-name character read, but the STAFF `audit <name>` view now
// merges in the account's tier history via the name -> account resolution — so tier changes are reachable
// through a character. The mortal self-view stays character-scoped.
func TestAuditTierReachableByCharacter(t *testing.T) {
	ms := NewMemStore()
	sh := NewDemoShard().WithAudit(ms)
	z := sh.Zone()
	ctx := context.Background()

	// Bob (a character) owned by account acct-1.
	if _, err := ms.AppendAudit(ctx, AuditEvent{
		SubjectType: AuditSubjectCharacter, SubjectID: "id-Bob", SubjectName: "Bob",
		ActorType: AuditActorSystem, EventKind: AuditKindDied, DedupKey: "1",
		Payload: AuditPayload(map[string]any{"room_ref": "midgaard:room:temple"}),
	}); err != nil {
		t.Fatal(err)
	}
	// An ACCOUNT tier change for acct-1 — subject is the account, no subject_name, so neither the by-id
	// self-view nor the by-name staff read can see it directly.
	if _, err := ms.AppendAudit(ctx, AuditEvent{
		SubjectType: AuditSubjectAccount, SubjectID: "acct-1", SubjectName: "",
		ActorType: AuditActorAccount, ActorID: "acct-1", EventKind: AuditKindTierChanged, DedupKey: "tier-1",
		Payload: AuditPayload(map[string]any{"old_tier": "player", "new_tier": "builder"}),
	}); err != nil {
		t.Fatal(err)
	}
	ms.SetCharacterAccount("Bob", "acct-1")

	// Precondition: the plain by-name read sees ONLY Bob's own character row, not the account tier row.
	if rows, _ := ms.ListAuditForCharacterName(ctx, "Bob", 100); len(rows) != 1 {
		t.Fatalf("by-name read should see only Bob's own row, got %d (the account tier row must not leak in)", len(rows))
	}

	// Staff `audit Bob` now surfaces BOTH Bob's death AND the account tier change.
	admin := newTestPlayerEntity(z, "Admin")
	admin.tier = tierAdmin
	z.dispatch(admin, "audit Bob")
	out := waitAuditLine(t, admin, "Audit history for Bob")
	if !strings.Contains(out, AuditKindTierChanged) {
		t.Fatalf("staff `audit <name>` did not surface the account tier change: %q", out)
	}
	if !strings.Contains(out, "player -> builder") {
		t.Fatalf("the tier-change payload was not rendered: %q", out)
	}
}

// TestAuditBatchCoalescesBurst is #399 items 1+2: the drainer coalesces a queued burst into ONE
// AppendAuditBatch round-trip instead of one call per event, and every event is still recorded. White-box:
// we fill the queue, then drive coalesce+handleBatch directly (no drainer goroutine) so the batching is
// deterministic. An instrumented sink counts batch calls vs total events to prove coalescing happened.
func TestAuditBatchCoalescesBurst(t *testing.T) {
	ms := NewMemStore()
	sink := &batchCountingSink{AuditSink: ms}
	a := newAuditor(sink)

	const burst = 10
	for i := 0; i < burst; i++ {
		a.reqs <- AuditEvent{
			SubjectType: AuditSubjectCharacter, SubjectID: "id-Cara", SubjectName: "Cara",
			ActorType: AuditActorSystem, EventKind: AuditKindAttributeBase, DedupKey: "grant-" + strconv.Itoa(i),
			Payload: AuditPayload(map[string]any{"attr": "str", "old": float64(i), "new": float64(i + 1)}),
		}
	}
	// Drain: take the first, coalesce the rest, write one batch. The whole burst fits under auditBatchMax,
	// so it drains as a SINGLE batch call.
	first := <-a.reqs
	a.handleBatch(context.Background(), a.coalesce(first))

	if sink.batchCalls != 1 {
		t.Fatalf("a %d-event burst drained in %d batch calls, want 1 (coalescing did not happen)", burst, sink.batchCalls)
	}
	if sink.totalEvents != burst {
		t.Fatalf("batch wrote %d events, want %d", sink.totalEvents, burst)
	}
	rows, _ := ms.ListAuditForCharacterName(context.Background(), "Cara", 100)
	if len(rows) != burst {
		t.Fatalf("recorded %d rows, want %d (every event in the batch must land)", len(rows), burst)
	}
}

// TestAppendAuditBatchIdempotent is #399: a batch containing a duplicate idempotency key records the row
// ONCE and reports the true recorded count — the per-row ON CONFLICT DO NOTHING semantics survive batching.
func TestAppendAuditBatchIdempotent(t *testing.T) {
	ms := NewMemStore()
	ctx := context.Background()
	ev := func(dk string) AuditEvent {
		return AuditEvent{
			SubjectType: AuditSubjectCharacter, SubjectID: "id-Dex", SubjectName: "Dex",
			ActorType: AuditActorSystem, EventKind: AuditKindTrackAdvanced, DedupKey: dk,
			Payload: AuditPayload(map[string]any{"track": "combat", "step": 1}),
		}
	}
	// Two distinct + one duplicate of the first, all in one batch.
	recorded, err := ms.AppendAuditBatch(ctx, []AuditEvent{ev("a"), ev("b"), ev("a")})
	if err != nil {
		t.Fatal(err)
	}
	if recorded != 2 {
		t.Fatalf("batch recorded %d, want 2 (the duplicate key must be an idempotent no-op)", recorded)
	}
	rows, _ := ms.ListAuditForCharacterName(ctx, "Dex", 100)
	if len(rows) != 2 {
		t.Fatalf("store holds %d rows, want 2 (the duplicate must not double-record)", len(rows))
	}
}

// TestAuditBatchFallbackRecordsPerRow is #399 (the review's failure-atomicity finding): when a batch write
// FAILS (an implicit-transaction rollback commits nothing), handleBatch falls back to per-row appends under
// the same bounded ctx — so a single poison event no longer takes the whole coalesced batch down with it.
// A fault-injecting sink forces every AppendAuditBatch to error; every event must still land via the fallback.
func TestAuditBatchFallbackRecordsPerRow(t *testing.T) {
	ms := NewMemStore()
	a := newAuditor(&failBatchSink{MemStore: ms})
	ctx := context.Background()

	evs := make([]AuditEvent, 3)
	for i := range evs {
		evs[i] = AuditEvent{
			SubjectType: AuditSubjectCharacter, SubjectID: "id-Eve", SubjectName: "Eve",
			ActorType: AuditActorSystem, EventKind: AuditKindAttributeBase, DedupKey: "g-" + strconv.Itoa(i),
			Payload: AuditPayload(map[string]any{"attr": "str", "new": float64(i)}),
		}
	}
	a.handleBatch(ctx, evs) // the batch errors -> per-row fallback records all three

	rows, _ := ms.ListAuditForCharacterName(ctx, "Eve", 100)
	if len(rows) != 3 {
		t.Fatalf("batch-fallback recorded %d rows, want 3 (a failed batch must recover per-row)", len(rows))
	}
}

// batchCountingSink wraps an AuditSink and counts AppendAuditBatch invocations + total events written, so a
// test can prove a burst was COALESCED into few batch calls rather than one call per event.
type batchCountingSink struct {
	AuditSink
	batchCalls  int
	totalEvents int
}

func (s *batchCountingSink) AppendAuditBatch(ctx context.Context, evs []AuditEvent) (int, error) {
	s.batchCalls++
	s.totalEvents += len(evs)
	return s.AuditSink.AppendAuditBatch(ctx, evs)
}

// failBatchSink forces AppendAuditBatch to error (delegating per-row AppendAudit + reads to the wrapped
// MemStore), so a test can prove handleBatch's per-row fallback still records every event when the batch
// path fails — the review's failure-atomicity mitigation.
type failBatchSink struct {
	*MemStore
}

func (s *failBatchSink) AppendAuditBatch(context.Context, []AuditEvent) (int, error) {
	return 0, errBatchInjected
}

var errBatchInjected = errors.New("injected batch failure")

// TestAuditLowPriorityWatermarkReservesHeadroom (#443 Finding B): once the shared auditor queue is past the
// reserve watermark, a low-priority item_transferred enqueue is SHED, while a critical kind still enqueues —
// so a transfer flood can never starve died/tier_changed out of the queue. Deterministic: the drainer is
// NOT started, so the buffered channel fills and stays filled.
func TestAuditLowPriorityWatermarkReservesHeadroom(t *testing.T) {
	a := newAuditor(NewMemStore()) // enabled sink; run() NOT called, so nothing drains the channel
	for i := 0; i < auditLowPriorityWatermark; i++ {
		a.enqueueLowPriority(AuditEvent{EventKind: AuditKindItemTransferred})
	}
	if got := len(a.reqs); got != auditLowPriorityWatermark {
		t.Fatalf("queue depth after filling to the watermark = %d, want %d", got, auditLowPriorityWatermark)
	}
	// At the watermark a further LOW-priority event is shed (queue unchanged)...
	a.enqueueLowPriority(AuditEvent{EventKind: AuditKindItemTransferred})
	if got := len(a.reqs); got != auditLowPriorityWatermark {
		t.Fatalf("a low-priority event was NOT shed past the watermark: depth %d, want %d", got, auditLowPriorityWatermark)
	}
	// ...but a CRITICAL kind still enqueues into the reserved headroom.
	a.enqueue(AuditEvent{EventKind: AuditKindDied})
	if got := len(a.reqs); got != auditLowPriorityWatermark+1 {
		t.Fatalf("a critical kind must still enqueue past the watermark: depth %d, want %d", got, auditLowPriorityWatermark+1)
	}
}
