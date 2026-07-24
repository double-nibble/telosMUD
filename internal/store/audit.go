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
// AppendAuditBatch is the async auditor's coalesced multi-row flush (#399) — one round-trip per drain
// batch, same per-row ON CONFLICT idempotency.
//
// RETENTION (ops follow-up, #399 item 3): character_audit is APPEND-ONLY and long-lived, with two
// unbounded high-volume kinds (`died` especially). There is no TTL/partitioning/archival, so the table +
// its indexes grow without bound over a shard's lifetime. The recommended fix is TIME-BASED PARTITIONING
// on `at` (monthly range partitions), which also keeps the newest-first reads scanning only recent
// partitions and makes archival a partition DETACH rather than a mass DELETE — but partitioning a live
// table and choosing a retention window are an OPS decision (how long staff accountability must reach
// back), not something the engine should hard-code. Left as a documented growth bound until an operator
// sets the policy; today's ~1-2k/box bar does not hit it in a reasonable operating window.

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
//
// ATOMICITY TRADE-OFF (conscious choice, #399 item 7): because this runs INSIDE the create/tier tx, an
// audit-write error ROLLS BACK the primary mutation — a latent audit bug could become a character-create
// or tier-change OUTAGE. That is the deliberate price of strict-durable accountability on those paths (the
// alternative, a best-effort async audit for create/tier, would let the recorded-vs-happened invariant
// drift). The trigger surface is tiny: the payloads are trivially serializable and the ids are valid
// UUIDs, so the only realistic failure is the DB itself being down — in which case the primary mutation
// could not commit anyway. Accepted over the async kinds' best-effort posture precisely because these
// events (who created a character, who changed an account's tier) are the ones staff must be able to trust.
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

// AppendAuditBatch inserts many audit rows in ONE pipelined round-trip (#399 items 1+2). The async
// auditor's drainer coalesces the events it can pull from its queue and hands them here, so a death-storm
// (or any burst) costs one network round-trip and one server-side transaction instead of N — raising the
// throughput ceiling that a single-row-per-event drainer hit under a momentarily-slow Postgres. Returns
// the number of rows actually recorded (RowsAffected==1 per statement) — the rest were idempotent
// no-ops (a replay) — so the caller can log recorded-vs-submitted without per-row bookkeeping.
//
// Each event is its OWN queued statement (not one multi-row VALUES). The win is a PER-ROW recorded count:
// each statement's own RowsAffected() tells us exactly which events were new vs an idempotent replay, so
// the returned `recorded` is precise — a single multi-row INSERT reports only one aggregate count. (A
// multi-row VALUES with an in-batch duplicate is NOT itself an error under ON CONFLICT DO NOTHING — that
// "cannot affect row a second time" hazard is DO UPDATE-only — so this is a precision choice, not a
// correctness one.) pgx wraps the batch in one implicit transaction, so the whole flush is atomic
// (all-or-nothing on a mid-batch error — the caller's handleBatch falls back to per-row on an error, which
// a rolled-back batch makes safe), and every row shares the transaction's now() when At is zero — which is
// exactly why the reads tie-break on the monotonic `seq`, not on `at`, within a batch (migration 00031).
func (p *Pool) AppendAuditBatch(ctx context.Context, evs []world.AuditEvent) (int, error) {
	if len(evs) == 0 {
		return 0, nil
	}
	batch := &pgx.Batch{}
	for _, ev := range evs {
		payload, err := marshalAuditPayload(ev)
		if err != nil {
			return 0, err
		}
		id := uuid.New()
		if ev.At.IsZero() {
			batch.Queue(auditInsertSQL(false),
				id, ev.SubjectType, ev.SubjectID, nullStr(ev.SubjectName),
				nullStr(ev.ActorType), nullStr(ev.ActorID), ev.EventKind, ev.DedupKey, payload)
		} else {
			batch.Queue(auditInsertSQL(true),
				id, ev.SubjectType, ev.SubjectID, nullStr(ev.SubjectName),
				nullStr(ev.ActorType), nullStr(ev.ActorID), ev.EventKind, ev.DedupKey, payload, ev.At)
		}
	}
	br := p.pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }() // Close returns the first unread Exec error; every Exec is read below
	recorded := 0
	for range evs {
		tag, err := br.Exec()
		if err != nil {
			return recorded, fmt.Errorf("store: append audit batch: %w", err)
		}
		if tag.RowsAffected() == 1 {
			recorded++
		}
	}
	return recorded, nil
}

// ListAccountTierAudit returns the tier_changed trail for the ACCOUNT that owns character NAME, newest-
// first, capped at limit (#399 item 4). tier_changed rows are stored with subject_type='account' and
// subject_id = the account UUID (subject_name NULL), so they are invisible to both the pid-scoped self-view
// and the name-scoped staff view — an account's tier history was unreachable through any character. This
// resolves name -> characters.account_id -> the account's tier rows, closing that gap for the staff
// `audit <name>` read. A character with no account (a dev/login character: account_id NULL) or an account
// with no tier changes yields no rows. Scoped to tier_changed on the resolved account — it cannot surface
// any other account's rows or any non-tier kind.
func (p *Pool) ListAccountTierAudit(ctx context.Context, name string, limit int) ([]world.AuditEntry, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT a.subject_type, a.subject_id, COALESCE(a.subject_name,''), COALESCE(a.actor_type,''),
		        COALESCE(a.actor_id::text,''), a.event_kind, a.payload, a.at
		   FROM character_audit a
		   JOIN characters c ON c.account_id = a.subject_id
		  WHERE c.name = $1
		    AND a.subject_type = 'account'
		    AND a.event_kind = $2
		  ORDER BY a.at DESC, a.seq DESC
		  LIMIT $3`, name, world.AuditKindTierChanged, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list account tier audit for name %q: %w", name, err)
	}
	return scanAuditRows(rows, "account-tier "+name)
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

// ListAuditForSubject returns subjectID's trail newest-first, capped at limit. `seq DESC` breaks a
// same-instant `at` tie by true insertion order (#399 item 5) — later-inserted first, matching the
// MemStore reverse-insertion tie-break so the hermetic and gated tests assert the same order. Scoped to
// subject_id — it can only ever return that subject's rows.
func (p *Pool) ListAuditForSubject(ctx context.Context, subjectID string, limit int) ([]world.AuditEntry, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT subject_type, subject_id, COALESCE(subject_name,''), COALESCE(actor_type,''),
		        COALESCE(actor_id::text,''), event_kind, payload, at
		   FROM character_audit
		  WHERE subject_id = $1
		  ORDER BY at DESC, seq DESC
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
		  ORDER BY at DESC, seq DESC
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
