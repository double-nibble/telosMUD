-- +goose Up
-- +goose StatementBegin

-- character_audit: a durable, append-only trail of PERMANENT character-state changes (#350). It is the
-- single, unified staff-queryable record of the irreversible things that happen to a character or account —
-- character creation, death, a permanent attribute-base grant, an advancement-track step, an account tier
-- change — so a staff member can answer "what happened to this character, and who did it" from one table.
--
-- ONE shared table, written by TWO services. All TelosMUD services open a *store.Pool from the SAME DSN
-- against this same migration tree, so telos-world (death / attribute / track, via the async auditor) and
-- telos-account (character create / tier change, in-transaction) both append here through the shared
-- internal/store package. It is engine MECHANISM, not content: an empty pack still audits, and a storeless
-- (bare-engine) shard simply has auditing DISABLED — no row exists until something auditable happens.
--
-- IDEMPOTENCY (at-most-once at the DB): the (subject_id, event_kind, dedup_key) UNIQUE index is the dedup
-- guard. The writers use INSERT ... ON CONFLICT DO NOTHING so a replayed create or a re-fired track step
-- (keyed on the stored high-water step) can never double-record. An event with no natural per-lifetime key
-- (a death — the Living.deaths counter is transient per-process and would collide across relogs; a discrete
-- attribute grant) passes a fresh UUID as the dedup_key, so distinct events never collapse.
--
-- DURABILITY is asymmetric, by design. The telos-account writes (character_created, tier_changed) are
-- IN-TRANSACTION with the change they record — atomic, blocking, never lost. The telos-world writes (died,
-- attribute_base_changed, track_advanced) ride the async auditor (auditor.go): a background drainer flushes
-- on graceful shutdown, but an event is still BEST-EFFORT — dropped on a full queue under a wedged DB. The
-- unique index prevents DOUBLES, not DROPS; this trail is an accountability aid for the async kinds, not a
-- gameplay-durability guarantee.
CREATE TABLE character_audit (
  id           UUID PRIMARY KEY,
  subject_type TEXT NOT NULL,               -- 'character' | 'account' — WHAT the row is about
  subject_id   UUID NOT NULL,               -- the character UUID (entity.pid) or the account UUID
  subject_name CITEXT,                       -- denormalized character name for the staff `audit <name>` query; NULL for account-only rows
  actor_type   TEXT,                         -- 'character' | 'account' | 'system' — WHO caused it
  actor_id     UUID,                         -- the actor's UUID; NULL = system/none (a mob kill, a bootstrap grant)
  event_kind   TEXT NOT NULL,               -- the stable event string (character_created / died / attribute_base_changed / track_advanced / tier_changed)
  dedup_key    TEXT NOT NULL DEFAULT '',    -- the per-kind idempotency key (a counter, a step, or a fresh UUID)
  payload      JSONB NOT NULL DEFAULT '{}', -- the event-specific detail (always carries "v":1 for forward schema evolution)
  at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Idempotency: the exactly-once guard the ON CONFLICT DO NOTHING writers target.
CREATE UNIQUE INDEX character_audit_idem_idx ON character_audit (subject_id, event_kind, dedup_key);
-- The two staff read paths: by subject id (an account trail) and by character name (the `audit <name>` verb),
-- both newest-first, so each index shape (…, at DESC) serves its scan + sort without a separate sort.
CREATE INDEX character_audit_subject_idx ON character_audit (subject_id, at DESC);
CREATE INDEX character_audit_name_idx    ON character_audit (subject_name, at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS character_audit;
-- citext is left installed (other tables use it); dropping a shared extension is out of scope here.
-- +goose StatementEnd
