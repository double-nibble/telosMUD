-- +goose Up
-- +goose StatementBegin

-- Loot definition content tables (docs/LOOT-AND-SPAWNS.md §2, Phase 12.1). A rarity tier and a loot table
-- are pure CONTENT — the same ref/pack PK + JSONB `body` tail as every other def table. The engine runs
-- the resolver + names no tier, table, or item; a pack supplies them. An empty pack ships no rows => no
-- loot (a mob then drops only its carried inventory — the pre-12 behavior). The loader reads these back
-- into the same RarityTierDTO / LootTableDTO the embedded YAML produces.

-- rarity_tier_defs: the ordered named tiers (common→…→legendary), each with an ordinal + default weight.
CREATE TABLE rarity_tier_defs (
  ref          TEXT PRIMARY KEY,            -- "rare"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- order + weight + color
);
CREATE INDEX rarity_tier_defs_pack_idx ON rarity_tier_defs (pack);

-- loot_table_defs: a list of independent rolls a mob drops from on death (referenced by a mob prototype's
-- loot_table). The whole roll list (kind / chance / pool / quality_floor / pity) rides the JSONB body.
CREATE TABLE loot_table_defs (
  ref          TEXT PRIMARY KEY,            -- "boss:duskwall_warden"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- rolls[] (kind/chance/n/quality_floor/pool/pity)
);
CREATE INDEX loot_table_defs_pack_idx ON loot_table_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS loot_table_defs;
DROP TABLE IF EXISTS rarity_tier_defs;
-- +goose StatementEnd
