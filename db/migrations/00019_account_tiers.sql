-- +goose Up
-- +goose StatementBegin

-- Builder trust tiers (#27/#97): the account-level role that gates builder/admin powers. telos-account is
-- the authority; it signs the tier into the session assertion so the world can trust it OFFLINE. Kept as a
-- TEXT column with a RELAXABLE check (NOT a rigid enum) so a future content-defined permission model can add
-- tiers with just a migration + a content table, not a schema rework (user direction on #27).
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT 'player';
ALTER TABLE accounts ADD CONSTRAINT accounts_tier_check CHECK (tier IN ('player', 'builder', 'admin'));

-- account_role_audit: one row per promote/demote — who changed whom, from what to what, when. The bootstrap
-- admin (config-pin at first sign-in) is recorded with a NULL actor_account (the system granted it).
CREATE TABLE account_role_audit (
  id             UUID PRIMARY KEY,
  actor_account  UUID REFERENCES accounts(id),          -- who made the change; NULL = system (bootstrap)
  target_account UUID NOT NULL REFERENCES accounts(id), -- whose tier changed
  old_tier       TEXT,                                   -- NULL for the initial grant at account creation
  new_tier       TEXT NOT NULL,
  at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX account_role_audit_target_idx ON account_role_audit (target_account);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS account_role_audit;
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_tier_check;
ALTER TABLE accounts DROP COLUMN IF EXISTS tier;
-- +goose StatementEnd
