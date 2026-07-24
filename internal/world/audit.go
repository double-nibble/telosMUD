package world

import (
	"context"
	"time"
)

// audit.go is the WORLD-side contract + DTOs for the #350 durable audit trail: an append-only,
// staff-queryable record of the PERMANENT things that happen to a character or account — creation,
// death, a permanent attribute-base grant, an advancement-track step, an account tier change. It is
// the single unified trail (a deliberate SUPERSET of account_role_audit — the redundancy is
// intentional so a staff member reads ONE table), backed by the shared `character_audit` table that
// both telos-world and telos-account write through the shared internal/store package.
//
// The sink is OPTIONAL exactly like MailStore: a nil AuditSink means "auditing disabled" and every
// emit site is a clean no-op (the bare-engine invariant — a storeless shard audits nothing and is
// byte-identical to a pre-#350 shard). Two implementations satisfy it: the pgx store against the
// `character_audit` table (internal/store/audit.go) and the in-memory MemStore (memstore.go), so the
// whole trail is hermetically testable with no live Postgres, and a gated round-trip pins the SQL.
//
// AT-MOST-ONCE at the DB (the idempotency contract every implementation MUST honor): AppendAudit is keyed
// on (subject_id, event_kind, dedup_key) and DOES NOTHING on a conflict — it returns recorded=false. A
// re-fired track step (the same high-water step) or a replayed create records at most one row. An event
// with no per-lifetime natural key — a death (the deaths counter is transient per-process, so it would
// collide across relogs), a discrete attribute grant — passes a FRESH UUID as dedup_key, so distinct
// events each get their own row and never collapse.
//
// DURABILITY is asymmetric by design. The in-transaction account/create writes (character_created,
// tier_changed) are atomic with the change they record — never lost. The async world writes (died,
// attribute_base_changed, track_advanced) ride the auditor's background drainer (auditor.go), which flushes
// on graceful shutdown but is BEST-EFFORT: an event is dropped on a full queue under a wedged DB. The
// unique index prevents doubles, not drops — this is an accountability aid for the async kinds, not a
// gameplay-durability guarantee.
//
// The store methods do blocking pool I/O and run OFF the zone goroutine — the async auditor drains
// AppendAudit on a background goroutine (auditor.go), and the staff read command spawns a short-lived
// goroutine (auditcmds.go), the same mailList / saver discipline. An emit site NEVER blocks a tick.

// auditVersion stamps every payload's "v" field. It exists so a later schema evolution (a renamed or
// added payload field) is a readable, forward-compatible change rather than a silent format break: a
// reader keys off "v" to know which shape to expect. Bumped only when a payload's meaning changes.
const auditVersion = 1

// The stable event_kind strings. These are DURABLE (they are written into the `character_audit` row and
// queried by staff), so they are frozen contract — never renamed, only added to. Each names one
// permanent character/account change #350 tracks. EXPORTED because the audit trail is written by TWO
// packages against the ONE shared table: internal/world (the emit helpers below) and internal/store's
// in-transaction account/create paths (which telos-account drives), so both share this one source of
// truth rather than duplicating the literals.
const (
	AuditKindCharacterCreated = "character_created"
	AuditKindDied             = "died"
	AuditKindAttributeBase    = "attribute_base_changed"
	AuditKindTrackAdvanced    = "track_advanced"
	AuditKindTierChanged      = "tier_changed"
)

// The subject_type / actor_type strings. subject_type says WHAT a row is about (a character or an
// account); actor_type says WHO caused it (a character, an account, or the system — a mob kill, a
// bootstrap grant, a CLI recovery). Frozen contract like the event kinds; exported for the same
// two-writer reason.
const (
	AuditSubjectCharacter = "character"
	AuditSubjectAccount   = "account"

	AuditActorCharacter = "character"
	AuditActorAccount   = "account"
	AuditActorSystem    = "system"
)

// AuditEvent is one append request: everything a writer stamps onto a new `character_audit` row. It is
// the write DTO the store maps to the row columns, keeping the on-disk shape independent of any caller.
// At is optional: a zero At lets the store default the column to now() (the common case — the emit site
// need not read a clock); a caller that owns an authoritative timestamp may set it.
type AuditEvent struct {
	SubjectType string // auditSubject* — 'character' | 'account'
	SubjectID   string // the character UUID (entity.pid) or the account UUID
	SubjectName string // denormalized character name (the staff `audit <name>` key); "" for account-only rows
	ActorType   string // auditActor* — 'character' | 'account' | 'system'; "" when there is no actor
	ActorID     string // the actor's UUID; "" = system/none (a mob kill, a bootstrap grant)
	EventKind   string // auditKind* — the stable event string
	DedupKey    string // the per-kind idempotency key (a counter, a step, or a fresh UUID)
	Payload     map[string]any
	At          time.Time // zero => let the store default now()
}

// AuditEntry is the read DTO the staff command renders: one stored `character_audit` row in its
// world-facing form. It omits DedupKey (an internal idempotency detail no reader needs).
type AuditEntry struct {
	SubjectType string
	SubjectID   string
	SubjectName string
	ActorType   string
	ActorID     string
	EventKind   string
	Payload     map[string]any
	At          time.Time
}

// AuditSink is the durable audit trail (docs/PERSISTENCE.md durable Postgres tier). Append is
// exactly-once (ON CONFLICT DO NOTHING on the idempotency key); the two reads back the staff `audit`
// command — a by-subject-id trail and a by-character-name trail, both newest-first. nil disables
// auditing entirely (the never-fatal degradation).
type AuditSink interface {
	// AppendAudit records ev, returning recorded=false (nil error) when the idempotency key already
	// exists (a retry / replay — not an error). An error is an infrastructure failure the caller logs;
	// it never propagates onto the zone goroutine (the auditor drains this off-goroutine).
	AppendAudit(ctx context.Context, ev AuditEvent) (recorded bool, err error)

	// AppendAuditBatch records many events in ONE round-trip (#399), returning how many were newly
	// recorded (the rest were idempotent no-ops). The async auditor's drainer coalesces a burst of
	// events and flushes them here, so a death-storm costs one round-trip, not one per death. Each event
	// keeps the same per-row ON CONFLICT DO NOTHING idempotency as AppendAudit.
	AppendAuditBatch(ctx context.Context, evs []AuditEvent) (recorded int, err error)

	// ListAccountTierAudit returns the tier_changed trail for the ACCOUNT that owns character NAME,
	// newest-first, capped at limit (#399). tier_changed rows are account-subject (subject_name NULL) and
	// so are reachable by neither by-id nor by-name reads; this resolves character name -> account -> the
	// account's tier rows so the staff `audit <name>` view can surface an account's tier history.
	ListAccountTierAudit(ctx context.Context, name string, limit int) ([]AuditEntry, error)

	// ListAuditForSubject returns the trail for one subject UUID, newest-first, capped at limit. Used
	// by the account/by-id read path.
	ListAuditForSubject(ctx context.Context, subjectID string, limit int) ([]AuditEntry, error)

	// ListAuditForCharacterName returns the trail for one character NAME (CITEXT, case-insensitive),
	// newest-first, capped at limit. This is what the staff `audit <name>` verb queries, and the player
	// self-view (scoped to the caller's own name).
	ListAuditForCharacterName(ctx context.Context, name string, limit int) ([]AuditEntry, error)
}

// AuditPayload builds a payload map pre-stamped with the schema version. Every emit site (in this
// package AND the store's in-tx paths) funnels its event-specific fields through here so "v":1 is stamped
// in exactly one place (a caller can never forget it, and a version bump is a one-line change). Extra
// fields are the caller's event-specific detail. Exported for the two-writer reason above.
func AuditPayload(fields map[string]any) map[string]any {
	p := make(map[string]any, len(fields)+1)
	p["v"] = auditVersion
	for k, v := range fields {
		p[k] = v
	}
	return p
}
