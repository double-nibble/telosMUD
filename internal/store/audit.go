package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/double-nibble/telosmud/internal/world"
)

// audit.go is the pgx implementation of world.AuditSink against the `character_audit` table
// (00029_character_audit.sql; #350). The audit trail is durable, append-only, staff-queryable player/
// account state — append is an INSERT, the two reads are newest-first SELECTs. Every method does
// blocking pool I/O and runs OFF the zone goroutine (the async auditor drains AppendAudit; the staff
// command spawns a goroutine), so the synchronous pool calls are fine.
//
// EXACTLY-ONCE: AppendAudit is `INSERT ... ON CONFLICT (subject_id, event_kind, dedup_key) DO NOTHING`,
// so a retried death / re-fired track step / replayed create records at most one row. recorded is
// tag.RowsAffected()==1 — the caller learns whether THIS call was the one that recorded it. The
// unexported appendAuditTx runs the same insert inside a caller-supplied transaction (the in-tx
// create/tier paths), where exactly-once falls out of the enclosing tx and recorded is not needed.

// AppendAudit inserts one audit row, minting its id, and returns whether this call recorded it (false =
// the idempotency key already existed, a benign retry). The payload marshals to JSONB. A zero ev.At
// omits the column so SQL defaults it to now() — the common case, so an emit site never reads a clock.
func (p *Pool) AppendAudit(ctx context.Context, ev world.AuditEvent) (bool, error) {
	payload, err := marshalAuditPayload(ev)
	if err != nil {
		return false, err
	}
	id := uuid.New()
	// The column list / conflict target is shared with appendAuditTx via auditInsertSQL; here we bind on
	// the pool directly. On a zero At we omit the `at` column so its DEFAULT now() applies; otherwise we
	// pass the caller's authoritative timestamp.
	var tag pgconn.CommandTag
	if ev.At.IsZero() {
		tag, err = p.pool.Exec(ctx, auditInsertSQL(false),
			id, ev.SubjectType, ev.SubjectID, nullStr(ev.SubjectName),
			nullStr(ev.ActorType), nullStr(ev.ActorID), ev.EventKind, ev.DedupKey, payload)
	} else {
		tag, err = p.pool.Exec(ctx, auditInsertSQL(true),
			id, ev.SubjectType, ev.SubjectID, nullStr(ev.SubjectName),
			nullStr(ev.ActorType), nullStr(ev.ActorID), ev.EventKind, ev.DedupKey, payload, ev.At)
	}
	if err != nil {
		return false, fmt.Errorf("store: append audit (%s/%s): %w", ev.SubjectType, ev.EventKind, err)
	}
	return tag.RowsAffected() == 1, nil
}

// appendAuditTx runs the same idempotent insert inside a caller-supplied transaction — the in-tx
// character-create and tier-change paths, where the audit row must land atomically with the change it
// records (a crash can't leave a change unrecorded or a record without its change). recorded is not
// returned: the enclosing transaction is the exactly-once boundary, and a conflict (a replay) is a
// benign no-op the caller ignores. A zero At defaults to now() in SQL, same as AppendAudit.
func appendAuditTx(ctx context.Context, tx pgx.Tx, ev world.AuditEvent) error {
	payload, err := marshalAuditPayload(ev)
	if err != nil {
		return err
	}
	id := uuid.New()
	if ev.At.IsZero() {
		_, err = tx.Exec(ctx, auditInsertSQL(false),
			id, ev.SubjectType, ev.SubjectID, nullStr(ev.SubjectName),
			nullStr(ev.ActorType), nullStr(ev.ActorID), ev.EventKind, ev.DedupKey, payload)
	} else {
		_, err = tx.Exec(ctx, auditInsertSQL(true),
			id, ev.SubjectType, ev.SubjectID, nullStr(ev.SubjectName),
			nullStr(ev.ActorType), nullStr(ev.ActorID), ev.EventKind, ev.DedupKey, payload, ev.At)
	}
	if err != nil {
		return fmt.Errorf("store: append audit tx (%s/%s): %w", ev.SubjectType, ev.EventKind, err)
	}
	return nil
}

// auditInsertSQL returns the ON CONFLICT DO NOTHING insert, with or without the explicit `at` column.
// Shared by AppendAudit and appendAuditTx so the column list + conflict target live in one place. When
// withAt is false the `at` column is omitted so its DEFAULT now() applies.
func auditInsertSQL(withAt bool) string {
	if withAt {
		return `INSERT INTO character_audit
		          (id, subject_type, subject_id, subject_name, actor_type, actor_id, event_kind, dedup_key, payload, at)
		        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		        ON CONFLICT (subject_id, event_kind, dedup_key) DO NOTHING`
	}
	return `INSERT INTO character_audit
	          (id, subject_type, subject_id, subject_name, actor_type, actor_id, event_kind, dedup_key, payload)
	        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	        ON CONFLICT (subject_id, event_kind, dedup_key) DO NOTHING`
}

// marshalAuditPayload serializes ev.Payload to JSONB, defaulting an empty/nil payload to `{}` (the
// column is NOT NULL DEFAULT '{}', and the emit sites always stamp at least "v":1, so this is a
// defensive floor).
func marshalAuditPayload(ev world.AuditEvent) ([]byte, error) {
	if len(ev.Payload) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(ev.Payload)
	if err != nil {
		return nil, fmt.Errorf("store: marshal audit payload (%s/%s): %w", ev.SubjectType, ev.EventKind, err)
	}
	return b, nil
}

// ListAuditForSubject returns subjectID's trail newest-first, capped at limit. `id DESC` breaks a
// same-instant `at` tie deterministically (matching the MemStore tie-break so the hermetic and gated
// tests assert the same order). Scoped to subject_id — it can only ever return that subject's rows.
func (p *Pool) ListAuditForSubject(ctx context.Context, subjectID string, limit int) ([]world.AuditEntry, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT subject_type, subject_id, COALESCE(subject_name,''), COALESCE(actor_type,''),
		        COALESCE(actor_id::text,''), event_kind, payload, at
		   FROM character_audit
		  WHERE subject_id = $1
		  ORDER BY at DESC, id DESC
		  LIMIT $2`, subjectID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit for subject %s: %w", subjectID, err)
	}
	return scanAuditRows(rows, "subject "+subjectID)
}

// ListAuditForCharacterName returns the trail for character NAME (CITEXT, case-insensitive) newest-first,
// capped at limit — the staff `audit <name>` query and the player self-view. Scoped to subject_name.
func (p *Pool) ListAuditForCharacterName(ctx context.Context, name string, limit int) ([]world.AuditEntry, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT subject_type, subject_id, COALESCE(subject_name,''), COALESCE(actor_type,''),
		        COALESCE(actor_id::text,''), event_kind, payload, at
		   FROM character_audit
		  WHERE subject_name = $1
		  ORDER BY at DESC, id DESC
		  LIMIT $2`, name, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit for name %q: %w", name, err)
	}
	return scanAuditRows(rows, "name "+name)
}

// scanAuditRows maps a result set to []world.AuditEntry, unmarshaling each payload JSONB. `what`
// names the scope for error context. Shared by the two read methods.
func scanAuditRows(rows pgx.Rows, what string) ([]world.AuditEntry, error) {
	defer rows.Close()
	var out []world.AuditEntry
	for rows.Next() {
		var (
			e       world.AuditEntry
			payload []byte
		)
		if err := rows.Scan(&e.SubjectType, &e.SubjectID, &e.SubjectName, &e.ActorType,
			&e.ActorID, &e.EventKind, &payload, &e.At); err != nil {
			return nil, fmt.Errorf("store: scan audit (%s): %w", what, err)
		}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &e.Payload); err != nil {
				return nil, fmt.Errorf("store: unmarshal audit payload (%s): %w", what, err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list audit (%s): %w", what, err)
	}
	return out, nil
}

// Compile-time assertion that *Pool satisfies world.AuditSink.
var _ world.AuditSink = (*Pool)(nil)
