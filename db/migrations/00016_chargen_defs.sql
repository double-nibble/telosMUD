-- +goose Up
-- +goose StatementBegin

-- Character-generation flow content table (docs/ACCOUNT.md, Phase 14.8). A chargen flow is an ORDERED list
-- of STEPS the signup page walks — each a kind (`bundle_choice` to pick a race/class/background bundle,
-- `point_buy` to allocate attributes under a cost curve, and future kinds like array/roll). It is pure
-- CONTENT: a ref/pack PK + a JSONB `body` tail (the steps). The engine/website knows the STEP KINDS, never a
-- specific ruleset — content drives HOW generation works (point-buy, standard array, roll-and-assign, …), so
-- one MUD can be 5e point-buy and another a roll-stats game with no code change. One flow per pack by
-- convention. An empty pack ships no rows => chargen falls back to a bare name+create.
CREATE TABLE chargen_defs (
  ref          TEXT PRIMARY KEY,            -- "demo:chargen"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- { steps: [ {kind, id, prompt, …kind-specific} ] }
);
CREATE INDEX chargen_defs_pack_idx ON chargen_defs (pack);

-- The chargen RESULT for a not-yet-spawned character (Phase 14.8, Model A): telos-account writes the chosen
-- bundles + bought attribute values here at create time; the WORLD applies them on FIRST spawn (sets the
-- attribute bases, runs apply_bundle for each chosen bundle) and clears this back to NULL on the next save.
-- A dedicated column (not the `state` JSONB) keeps telos-account decoupled from the world's PlayerSnapshot
-- shape — it writes a small, account-owned { bundles, attrs } object. NULL => nothing pending (the common
-- case: every returning character).
ALTER TABLE characters ADD COLUMN chargen JSONB;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE characters DROP COLUMN IF EXISTS chargen;
DROP TABLE IF EXISTS chargen_defs;
-- +goose StatementEnd
