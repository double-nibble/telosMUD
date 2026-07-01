-- +goose Up
-- +goose StatementBegin

-- Drop the passphrase (account_auth) and SSH pubkey (ssh_keys) tables. They were created by
-- 00015_accounts.sql for the Phase-14 passphrase + SSH login paths, but Phase 15 made auth OAuth-ONLY and
-- deleted every code path that reads or writes them (docs/ACCOUNT.md, docs/REMAINING.md §8 housekeeping).
-- They have held no live data and no reader since Phase 15, so this is a pure dead-schema cleanup.
DROP TABLE IF EXISTS ssh_keys;
DROP TABLE IF EXISTS account_auth;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Recreate the tables exactly as 00015_accounts.sql defined them (so a down-migration restores the
-- pre-cleanup schema shape). They are inert without the Phase-14 code paths, which no longer exist.
CREATE TABLE account_auth (
  account_id      UUID PRIMARY KEY REFERENCES accounts(id),
  passphrase_hash TEXT,
  failed_attempts INT NOT NULL DEFAULT 0,
  locked_until    TIMESTAMPTZ,
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ssh_keys (
  fingerprint TEXT PRIMARY KEY,
  account_id  UUID NOT NULL REFERENCES accounts(id),
  pubkey      TEXT NOT NULL,
  label       TEXT,
  added_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ssh_keys_account_idx ON ssh_keys (account_id);

-- +goose StatementEnd
