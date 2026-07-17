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
