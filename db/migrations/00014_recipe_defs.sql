-- +goose Up
-- +goose StatementBegin

-- Crafting-recipe definition content table (docs/CRAFTING.md, Phase 13.5). A recipe is the data a `craft`
-- ability runs: the profession + skill it needs, an optional station (a room flag, D3), the component
-- inputs it consumes, and the item output (+ a coarse quality band). Like every other def table it is pure
-- CONTENT: a ref/pack PK + a JSONB `body` tail (profession / skill / min_skill / station / inputs / output /
-- quality_base). The engine runs the craft op + names no recipe; a pack supplies them. An empty pack ships
-- no rows => no recipes.
CREATE TABLE recipe_defs (
  ref          TEXT PRIMARY KEY,            -- "craft:leather_vest"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- profession + skill + min_skill + station + inputs + output + quality_base
);
CREATE INDEX recipe_defs_pack_idx ON recipe_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS recipe_defs;
-- +goose StatementEnd
