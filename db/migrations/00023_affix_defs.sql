-- +goose Up
-- +goose StatementBegin

-- Content-defined NAMED affixes (#37, Round 11): reusable attribute+range rolls a loot entry's item-quality
-- pool references by ref (AffixRollDTO.ref), so a shared affix ("of the bear" = +strength) is authored ONCE
-- and reused across many drops instead of being inlined into every pool. Like every other def table it is
-- pure CONTENT: a (pack, ref) PK + a JSONB `body` tail (the target attribute + the roll [min, max] range).
-- The world resolves an entry's `ref` against these at loot-table build time, so editing an affix_def
-- propagates to every referencing pool on the next reload (the normalization win). An empty pack ships no
-- rows => pools inline their affixes (the pre-#37 form) — the bare engine is unchanged.
CREATE TABLE affix_defs (
  ref  TEXT NOT NULL,                -- the stable affix id ("of_the_bear")
  pack TEXT NOT NULL,
  body JSONB NOT NULL DEFAULT '{}',  -- {"attr": "strength", "min": 1, "max": 3}
  PRIMARY KEY (pack, ref)
);
CREATE INDEX affix_defs_pack_idx ON affix_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS affix_defs;
-- +goose StatementEnd
