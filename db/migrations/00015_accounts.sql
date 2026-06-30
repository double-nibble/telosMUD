-- +goose Up
-- +goose StatementBegin

-- Phase 14 (auth & website, docs/ACCOUNT.md §11): the real account model on top of the Phase-2 `accounts`
-- stub. telos-account is the ONLY service that touches these tables + OAuth providers; the gate/world reach
-- accounts only through its gRPC API. accounts/account_identities are the OAuth identity layer;
-- account_auth holds the optional passphrase (Argon2id); ssh_keys the SSH pubkey identities.

-- accounts gains a display name for the dashboard (the stub was id/status/created_at). Fix the stale
-- "Phase 13" provenance note from 00002 while we are here.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS display_name TEXT;
COMMENT ON TABLE accounts IS 'Account root (Phase 14 auth). Identity rows live in account_identities.';

-- account_identities: one row per linked OAuth identity. (provider, provider_uid) is the identity key — NEVER
-- email (provider emails may be unverified/reused, so we never auto-merge by email; a collision is surfaced).
CREATE TABLE account_identities (
  provider     TEXT NOT NULL,                     -- 'github' | 'google' | 'discord'
  provider_uid TEXT NOT NULL,                     -- the provider's stable user id
  account_id   UUID NOT NULL REFERENCES accounts(id),
  email        TEXT,                              -- informational ONLY; not an identity key
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (provider, provider_uid)
);
CREATE INDEX account_identities_account_idx ON account_identities (account_id);

-- account_auth: the OPTIONAL MUD passphrase (Argon2id) + the rate-limit/lockout state the gate enforces.
-- One row per account; absent/null hash => passphrase auth not set for that account.
CREATE TABLE account_auth (
  account_id      UUID PRIMARY KEY REFERENCES accounts(id),
  passphrase_hash TEXT,                           -- Argon2id encoded hash; null if not set
  failed_attempts INT NOT NULL DEFAULT 0,
  locked_until    TIMESTAMPTZ,                    -- backoff window after repeated failures
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ssh_keys: SSH public-key identities. The fingerprint (SHA256 of the pubkey) is the lookup key the gate's
-- SSH server maps to an account on connect.
CREATE TABLE ssh_keys (
  fingerprint TEXT PRIMARY KEY,                   -- SHA256 of the pubkey
  account_id  UUID NOT NULL REFERENCES accounts(id),
  pubkey      TEXT NOT NULL,
  label       TEXT,
  added_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ssh_keys_account_idx ON ssh_keys (account_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS ssh_keys;
DROP TABLE IF EXISTS account_auth;
DROP TABLE IF EXISTS account_identities;
ALTER TABLE accounts DROP COLUMN IF EXISTS display_name;
-- +goose StatementEnd
