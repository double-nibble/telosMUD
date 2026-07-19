-- +goose Up
-- +goose StatementBegin

-- 00030_character_owner_epoch.sql — the OWNERSHIP fence for a character's durable row (#432).
--
-- Before this migration `state_version` was documented as "the fence that protects a genuinely-
-- rehydrated player on a new shard from a zombie original." It was not. state_version is CONTENTION
-- control: it tells a writer that somebody else wrote since it read, and every caller in the world
-- package answered that by re-reading, rebasing and writing again. A stale shard's final logout
-- flush therefore force-wrote its 60-second-old snapshot over the live owner's state — a rollback,
-- and with it a duplication primitive (externalize wealth on the live copy, let the stale copy roll
-- the character back, repeat).
--
-- owner_epoch is the missing axis: a monotonic per-character OWNERSHIP generation. Every claim of a
-- character (a fresh login, a cross-shard handoff) mints the next value atomically from THIS column,
-- so no two live copies ever hold the same epoch. A save carries the epoch its session was minted at
-- and applies only `WHERE owner_epoch <= $k` — a predicate no amount of rebasing can reach around,
-- because a rebase can only move state_version.
--
-- Why the counter lives HERE and not in the directory: the directory is Redis — TTL'd, evictable
-- coordination state (#340). Deriving a durability fence's high-water mark from an evictable cache is
-- the same mistake one layer down. The row is both the sink and the arbiter, and Postgres's row lock
-- serializes concurrent claimants for free.
--
-- DEFAULT 0 is the correct "never claimed" floor: every claim writes GREATEST(owner_epoch, floor) + 1,
-- so the first claim of a legacy row lands at >= 1 and the fence arms itself on first login. Old
-- shards in a rolling deploy never touch the column, so a mixed fleet can never LOWER it — the fence
-- is simply not yet enforced against those writers, which is the pre-migration status quo, not a
-- regression.

ALTER TABLE characters
    ADD COLUMN owner_epoch BIGINT NOT NULL DEFAULT 0;

COMMENT ON COLUMN characters.owner_epoch IS
    'Monotonic ownership generation (#432). Minted atomically by every ownership claim (login, '
    'cross-shard handoff); a save applies only WHERE owner_epoch <= the claiming session''s epoch. '
    'This — not state_version — is the fence against a zombie owner force-writing stale state.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE characters DROP COLUMN IF EXISTS owner_epoch;
-- +goose StatementEnd
