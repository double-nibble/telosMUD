-- +goose Up
-- +goose StatementBegin

-- command_defs / formula_defs close the Track-0 store gap (#20): a DB-seeded pack's custom Lua verbs
-- (Commands, Phase 7.4e) and ruleset-formula overrides (Formulas, Phase 7.4f) were NOT persisted through
-- ImportPack/Load — no INSERT/DELETE/SELECT existed for them, so they survived only on the embedded-YAML
-- load path and a Postgres-sourced pack silently dropped them. Both are pure CONTENT def tables in the
-- display_defs mould (migration 00018): a (pack, key) PK + a JSONB `body` tail, strips-and-replaces
-- idempotent by pack, FK-free. The trio's third member, PvpLua, is a pack SCALAR and rides the existing
-- pack_meta row (a new body field, not a new table).

-- Custom Lua verbs (Phase 7.4e): (pack, verb) PK — verb is shared vocabulary, so two packs may each define
-- one and the loader's per-pack accumulation resolves the winner. Body carries the alias list + Lua handler.
CREATE TABLE command_defs (
  verb  TEXT NOT NULL,
  pack  TEXT NOT NULL,
  body  JSONB NOT NULL DEFAULT '{}', -- {aliases, lua}
  PRIMARY KEY (pack, verb)
);
CREATE INDEX command_defs_pack_idx ON command_defs (pack);

-- Ruleset-formula overrides (Phase 7.4f): (pack, name) PK — the formula name (to_hit/soak/regen/xp_for/…)
-- is shared vocabulary, last-write-wins by name across packs. Body carries the Lua formula body.
CREATE TABLE formula_defs (
  name  TEXT NOT NULL,
  pack  TEXT NOT NULL,
  body  JSONB NOT NULL DEFAULT '{}', -- {lua}
  PRIMARY KEY (pack, name)
);
CREATE INDEX formula_defs_pack_idx ON formula_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS command_defs;
DROP TABLE IF EXISTS formula_defs;
-- +goose StatementEnd
