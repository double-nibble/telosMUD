package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/double-nibble/telosmud/internal/world"
)

// audit_test.go is the gated (TELOS_TEST_DSN) Postgres test for the #350 audit trail store (audit.go):
// AppendAudit records once and DEDUPS a duplicate idempotency triple, the two list reads return
// newest-first and are scoped to their subject, and appendAuditTx rolls back with its parent transaction.
// Skipped when TELOS_TEST_DSN is unset (testPool), so a DB-less `go test ./...` still passes.

// TestAppendAuditIdempotent proves AppendAudit records a first event (recorded=true) and a DUPLICATE
// (same subject_id, event_kind, dedup_key) is a benign no-op (recorded=false) — the exactly-once guard.
func TestAppendAuditIdempotent(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	subject := uuid.NewString()
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM character_audit WHERE subject_id = $1`, subject)
	})

	ev := world.AuditEvent{
		SubjectType: world.AuditSubjectCharacter,
		SubjectID:   subject,
		SubjectName: "AuditGated-" + time.Now().Format("150405.000000"),
		ActorType:   world.AuditActorSystem,
		EventKind:   world.AuditKindDied,
		DedupKey:    "1",
		Payload:     world.AuditPayload(map[string]any{"room_ref": "midgaard:room:temple"}),
	}
	recorded, err := p.AppendAudit(ctx, ev)
	if err != nil || !recorded {
		t.Fatalf("first append: recorded=%v err=%v, want true/nil", recorded, err)
	}
	// The same idempotency triple: DO NOTHING -> recorded=false, no error, still one row.
	recorded, err = p.AppendAudit(ctx, ev)
	if err != nil || recorded {
		t.Fatalf("duplicate append: recorded=%v err=%v, want false/nil", recorded, err)
	}
	rows, err := p.ListAuditForSubject(ctx, subject, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("after a duplicate append there are %d rows, want 1", len(rows))
	}
	if rows[0].Payload["room_ref"] != "midgaard:room:temple" {
		t.Fatalf("payload round-trip wrong: %v", rows[0].Payload)
	}

	// A DIFFERENT dedup_key for the same subject+kind is a distinct row (a second death).
	ev.DedupKey = "2"
	recorded, err = p.AppendAudit(ctx, ev)
	if err != nil || !recorded {
		t.Fatalf("distinct-key append: recorded=%v err=%v, want true/nil", recorded, err)
	}
}

// TestListAuditNewestFirst proves both list reads return newest-first and are scoped (a query for one
// subject/name never returns another's rows).
func TestListAuditNewestFirst(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	subject := uuid.NewString()
	other := uuid.NewString()
	name := "AuditOrder-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM character_audit WHERE subject_id = ANY($1)`,
			[]string{subject, other})
	})

	base := time.Now().Add(-time.Hour)
	add := func(sid, sname, kind string, at time.Time) {
		if _, err := p.AppendAudit(ctx, world.AuditEvent{
			SubjectType: world.AuditSubjectCharacter, SubjectID: sid, SubjectName: sname,
			ActorType: world.AuditActorSystem, EventKind: kind, DedupKey: kind,
			Payload: world.AuditPayload(nil), At: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add(subject, name, world.AuditKindCharacterCreated, base)
	add(subject, name, world.AuditKindAttributeBase, base.Add(30*time.Minute))
	add(subject, name, world.AuditKindDied, base.Add(45*time.Minute))
	add(other, "SomeoneElse", world.AuditKindDied, base.Add(50*time.Minute)) // must NOT appear in subject's trail

	bySubject, err := p.ListAuditForSubject(ctx, subject, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bySubject) != 3 {
		t.Fatalf("subject trail = %d rows, want 3 (scoped; other subject excluded)", len(bySubject))
	}
	// Newest-first: died (45m) > attribute (30m) > created (0m).
	wantOrder := []string{world.AuditKindDied, world.AuditKindAttributeBase, world.AuditKindCharacterCreated}
	for i, w := range wantOrder {
		if bySubject[i].EventKind != w {
			t.Fatalf("row %d = %s, want %s (newest-first)", i, bySubject[i].EventKind, w)
		}
	}

	byName, err := p.ListAuditForCharacterName(ctx, name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(byName) != 3 || byName[0].EventKind != world.AuditKindDied {
		t.Fatalf("by-name trail = %d rows (newest %s), want 3 newest died", len(byName), byName[0].EventKind)
	}
}

// TestAppendAuditBatchGated proves AppendAuditBatch inserts many rows in ONE round-trip (#399), records
// each once, and an in-batch DUPLICATE idempotency triple is a no-op — the per-row ON CONFLICT DO NOTHING
// survives batching (each event is its own queued statement).
func TestAppendAuditBatchGated(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	subject := uuid.NewString()
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM character_audit WHERE subject_id = $1`, subject)
	})

	name := "AuditBatch-" + time.Now().Format("150405.000000")
	ev := func(dk string) world.AuditEvent {
		return world.AuditEvent{
			SubjectType: world.AuditSubjectCharacter, SubjectID: subject, SubjectName: name,
			ActorType: world.AuditActorSystem, EventKind: world.AuditKindTrackAdvanced, DedupKey: dk,
			Payload: world.AuditPayload(map[string]any{"step": 1}),
		}
	}
	recorded, err := p.AppendAuditBatch(ctx, []world.AuditEvent{ev("a"), ev("b"), ev("a"), ev("c")})
	if err != nil {
		t.Fatal(err)
	}
	if recorded != 3 {
		t.Fatalf("batch recorded %d, want 3 (the duplicate 'a' is an idempotent no-op)", recorded)
	}
	rows, err := p.ListAuditForSubject(ctx, subject, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("batch left %d rows, want 3", len(rows))
	}

	// WITHIN-BATCH ORDERING (#399 item 5): a batch with NO explicit At — every row shares the batch
	// transaction's now() — must still read back in insertion order via the `seq` tie-break. The last
	// event queued (highest seq) sorts FIRST. This is the exact case the migration comment justifies and
	// that the single-At two-call TestAuditSeqTieBreak doesn't cover.
	sub2 := uuid.NewString()
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM character_audit WHERE subject_id = $1`, sub2)
	})
	batchEv := func(kind string) world.AuditEvent {
		return world.AuditEvent{
			SubjectType: world.AuditSubjectCharacter, SubjectID: sub2, SubjectName: name + "-seq",
			ActorType: world.AuditActorSystem, EventKind: kind, DedupKey: kind,
			Payload: world.AuditPayload(nil), // no At -> shared now()
		}
	}
	if _, err := p.AppendAuditBatch(ctx, []world.AuditEvent{
		batchEv(world.AuditKindCharacterCreated), batchEv(world.AuditKindDied),
	}); err != nil {
		t.Fatal(err)
	}
	ordered, err := p.ListAuditForSubject(ctx, sub2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != 2 || ordered[0].EventKind != world.AuditKindDied {
		t.Fatalf("within-batch order = %d rows newest %q, want 2 newest died (seq DESC on shared now())",
			len(ordered), ordered[0].EventKind)
	}
}

// TestAppendAuditBatchAtomicOnError pins the all-or-nothing store semantic the async auditor's per-row
// fallback relies on (#399): a batch whose MIDDLE row is a hard DB error (a non-UUID subject_id fails the
// UUID column cast) rolls the WHOLE batch back — zero rows commit, including the valid rows around it.
func TestAppendAuditBatchAtomicOnError(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	good := uuid.NewString()
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM character_audit WHERE subject_id = $1`, good)
	})
	ok := func() world.AuditEvent {
		return world.AuditEvent{
			SubjectType: world.AuditSubjectCharacter, SubjectID: good, SubjectName: "AtomicOK",
			ActorType: world.AuditActorSystem, EventKind: world.AuditKindDied, DedupKey: uuid.NewString(),
			Payload: world.AuditPayload(nil),
		}
	}
	poison := ok()
	poison.SubjectID = "not-a-uuid" // the UUID column cast fails at this statement -> the whole batch aborts

	if _, err := p.AppendAuditBatch(ctx, []world.AuditEvent{ok(), poison, ok()}); err == nil {
		t.Fatal("a batch with a poison row must return an error")
	}
	rows, err := p.ListAuditForSubject(ctx, good, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a failed batch committed %d rows, want 0 (all-or-nothing — the fallback depends on it)", len(rows))
	}
}

// TestAuditSeqTieBreak proves the newest-first reads break a same-`at` tie by INSERTION ORDER (the seq
// column, #399 item 5, migration 00031) — later-inserted first — not by the random UUID id. Two rows share
// an explicit At; the second inserted (higher seq) must sort first.
func TestAuditSeqTieBreak(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	subject := uuid.NewString()
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM character_audit WHERE subject_id = $1`, subject)
	})

	at := time.Now().Truncate(time.Microsecond)
	add := func(kind string) {
		if _, err := p.AppendAudit(ctx, world.AuditEvent{
			SubjectType: world.AuditSubjectCharacter, SubjectID: subject, SubjectName: "SeqTie",
			ActorType: world.AuditActorSystem, EventKind: kind, DedupKey: kind, At: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add(world.AuditKindCharacterCreated) // inserted first  -> lower seq
	add(world.AuditKindDied)             // inserted second -> higher seq -> must sort FIRST at the same at

	rows, err := p.ListAuditForSubject(ctx, subject, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].EventKind != world.AuditKindDied {
		t.Fatalf("same-at tie order wrong: %d rows, newest %q; want 2 rows, newest died (seq DESC)", len(rows), rows[0].EventKind)
	}
}

// TestListAccountTierAudit proves the tier-by-character staff read (#399 item 4): an account's tier_changed
// rows are reachable through a character it owns, while the plain by-name character read does NOT surface
// them (they are account-subject rows with a NULL subject_name).
func TestListAccountTierAudit(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	acct := uuid.NewString()
	acctB := uuid.NewString() // a SECOND account, to prove the read never crosses accounts
	name := "TierChar-" + time.Now().Format("150405.000000")
	nameB := "TierCharB-" + time.Now().Format("150405.000000")
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM character_audit WHERE subject_id = ANY($1)`, []string{acct, acctB})
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM characters WHERE account_id = ANY($1)`, []string{acct, acctB})
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = ANY($1)`, []string{acct, acctB})
	})

	if _, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status, tier) VALUES ($1, 'active', 'player')`, acct); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateAccountCharacter(ctx, acct, name, "midgaard", "midgaard:room:temple", nil, nil); err != nil {
		t.Fatal(err)
	}
	// A tier change writes an account-subject tier_changed row into character_audit.
	if _, err := p.SetAccountTier(ctx, acct, acct, TierBuilder, TierPlayer); err != nil {
		t.Fatal(err)
	}

	// A SECOND account + character with its OWN, distinct tier change — the isolation control. If the join
	// ever dropped its predicate, account A's read would pick up B's row; the assertions below catch that.
	if _, err := p.pool.Exec(ctx, `INSERT INTO accounts (id, status, tier) VALUES ($1, 'active', 'player')`, acctB); err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateAccountCharacter(ctx, acctB, nameB, "midgaard", "midgaard:room:temple", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetAccountTier(ctx, acctB, acctB, TierBuilder, TierPlayer); err != nil {
		t.Fatal(err)
	}

	// The plain by-name CHARACTER read must NOT include the account tier row.
	byName, err := p.ListAuditForCharacterName(ctx, name, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range byName {
		if e.EventKind == world.AuditKindTierChanged {
			t.Fatal("the by-name character read leaked an account tier_changed row")
		}
	}

	// The tier-by-character read DOES surface it.
	tier, err := p.ListAccountTierAudit(ctx, name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tier) != 1 || tier[0].EventKind != world.AuditKindTierChanged {
		t.Fatalf("tier-by-character read = %d rows, want 1 tier_changed (only account A's own row)", len(tier))
	}
	if tier[0].Payload["new_tier"] != "builder" {
		t.Fatalf("tier row payload new_tier = %v, want builder", tier[0].Payload["new_tier"])
	}
	// CROSS-ACCOUNT ISOLATION: account A's read returns ONLY account A's row, never account B's — the join
	// is bound to the single account owning `name`, so exactly one row and it is A's, not both accounts'.
	if tier[0].SubjectID != acct {
		t.Fatalf("tier read for %s returned account %s's row, want %s (cross-account leak)", name, tier[0].SubjectID, acct)
	}
	// And B's character reads back only B's row (the two accounts never bleed together).
	tierB, err := p.ListAccountTierAudit(ctx, nameB, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tierB) != 1 || tierB[0].SubjectID != acctB {
		t.Fatalf("tier read for %s = %d rows (subject %v), want 1 row for account %s", nameB, len(tierB), firstSubject(tierB), acctB)
	}

	// An unknown character name resolves to no account -> no rows (not an error).
	none, err := p.ListAccountTierAudit(ctx, "NoSuchChar-"+name, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("tier read for an unknown name = %d rows, want 0", len(none))
	}
}

// firstSubject returns the SubjectID of the first entry (or "" for empty) — a tiny helper for a clearer
// failure message in the isolation assertion.
func firstSubject(rows []world.AuditEntry) string {
	if len(rows) == 0 {
		return ""
	}
	return rows[0].SubjectID
}

// TestAppendAuditTxRollsBack proves the in-transaction audit insert rolls back with its parent tx: a
// character-create that fails after the audit write leaves NO audit row (the atomicity #350 relies on).
func TestAppendAuditTxRollsBack(t *testing.T) {
	p := testPool(t)
	ctx := context.Background()
	subject := uuid.NewString()
	t.Cleanup(func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM character_audit WHERE subject_id = $1`, subject)
	})

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendAuditTx(ctx, tx, world.AuditEvent{
		SubjectType: world.AuditSubjectCharacter, SubjectID: subject, SubjectName: "RolledBack",
		ActorType: world.AuditActorSystem, EventKind: world.AuditKindCharacterCreated, DedupKey: "1",
		Payload: world.AuditPayload(nil),
	}); err != nil {
		t.Fatal(err)
	}
	// Abandon the transaction WITHOUT committing: the audit row must not survive.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := p.ListAuditForSubject(ctx, subject, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rolled-back tx left %d audit rows, want 0", len(rows))
	}
}
