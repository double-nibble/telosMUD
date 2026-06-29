-- +goose Up
-- +goose StatementBegin

-- Director scope state (docs/WORLD-EVENTS.md §7, Phase 10.1). Region and world state each have a SINGLE
-- owning writer — the region/world director (the actor model one level up) — so even global state never
-- has two writers. These tables are that durable home. Each row is one (scope, key) -> value JSONB with
-- a `version` for the SAME optimistic-concurrency backstop as characters.state_version
-- (docs/PERSISTENCE.md §7): a write CASes on the version it read, so a stale (e.g. a just-demoted leader
-- racing the new one during failover) writer is rejected rather than clobbering. This is engine MECHANISM
-- (mutable director state), not content: an empty pack still has world/region state once a director writes.
--
-- world_state is global (one logical scope), keyed by `key`. region_state is per region, keyed by
-- (region_id, key) — region_id matches a region_defs.ref (the content grouping of member zones). value is
-- a data-only JSONB blob (the director's state bag is numbers/strings/bools/nested tables — the same
-- data-only discipline as the player Lua self.state subtree). version starts at 0 on first insert.
CREATE TABLE world_state (
  key     TEXT PRIMARY KEY,
  value   JSONB  NOT NULL DEFAULT '{}'::jsonb,
  version BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE region_state (
  region_id TEXT   NOT NULL,
  key       TEXT   NOT NULL,
  value     JSONB  NOT NULL DEFAULT '{}'::jsonb,
  version   BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (region_id, key)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS region_state;
DROP TABLE IF EXISTS world_state;
-- +goose StatementEnd
