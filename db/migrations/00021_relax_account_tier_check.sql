-- +goose Up
-- +goose StatementBegin

-- Content-defined trust tiers (#27/#29, Round 9 Slice 0b): drop the rigid {player,builder,admin} CHECK on
-- accounts.tier so a pack's content ladder (trust_tier_defs) can introduce its own tiers (moderator,
-- architect, …). The column stays TEXT NOT NULL DEFAULT 'player'; validation moves UP to the account
-- service's SetAccountTier, which now refuses a tier that is not in the loaded content ladder. This mirrors
-- the migration-00019 note ("RELAXABLE for future content tiers"): the enforcement is the ladder, not a
-- frozen DB enum.
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_tier_check;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restore the original 3-value CHECK. This can FAIL if content tiers were assigned while the constraint was
-- absent (a row would violate it); that is the expected signal to first demote any non-default-tier account.
ALTER TABLE accounts ADD CONSTRAINT accounts_tier_check CHECK (tier IN ('player', 'builder', 'admin'));

-- +goose StatementEnd
