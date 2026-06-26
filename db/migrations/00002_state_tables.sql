-- +goose Up
-- +goose StatementBegin

-- Durable STATE tables (docs/PERSISTENCE.md §2, docs/PHASE4-PLAN.md §2). A few engine-
-- universal relational columns (identity, location, save bookkeeping) plus one `state` JSONB
-- carrying ALL content-defined state (== the PlayerSnapshot shape). No level/class/hp/str
-- column: those are content concepts living in `state`. characters/object_instances are
-- created now so the schema is whole; they are first USED in slice 4.2.

CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE accounts (                   -- minimal stub; full account model is Phase 13
  id         UUID PRIMARY KEY,
  status     TEXT NOT NULL DEFAULT 'active',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE characters (
  id            UUID PRIMARY KEY,         -- the PersistID, real in Phase 4
  account_id    UUID REFERENCES accounts(id),  -- nullable until Phase 13 auth
  name          CITEXT UNIQUE NOT NULL,   -- engine-universal: one name, one char
  zone_ref      TEXT,                     -- where to rehydrate
  room_ref      TEXT,
  state_version BIGINT NOT NULL DEFAULT 0, -- optimistic concurrency (§7)
  state         JSONB  NOT NULL DEFAULT '{}', -- ALL content-defined state == PlayerSnapshot shape
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_saved_at TIMESTAMPTZ,
  last_login_at TIMESTAMPTZ,
  playtime_secs BIGINT NOT NULL DEFAULT 0,
  deleted_at    TIMESTAMPTZ
);

CREATE TABLE object_instances (           -- world-persistent items; defined but unused in v1
  id            UUID PRIMARY KEY,
  proto         TEXT NOT NULL,            -- prototype ref (flyweight source)
  delta         JSONB NOT NULL DEFAULT '{}',
  location_kind TEXT NOT NULL,            -- 'room' | 'container' | 'mailbox' | ...
  location_ref  TEXT NOT NULL,
  state_version BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX object_instances_location_idx ON object_instances (location_kind, location_ref);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS object_instances;
DROP TABLE IF EXISTS characters;
DROP TABLE IF EXISTS accounts;
-- citext is left installed: dropping an extension other objects may use is intentionally
-- out of scope for a down-migration.
-- +goose StatementEnd
