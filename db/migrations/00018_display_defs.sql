-- +goose Up
-- +goose StatementBegin

-- Display-template content table (docs/REMAINING.md display-templating). A display_def is a content-authored
-- Lua render body per SURFACE ("score"/"who"/"inventory"/…) that returns the rendered sheet string (built with
-- the sandbox `ui` toolkit). Like every other def table it is pure CONTENT: a (pack, surface) PK + a JSONB
-- `body` tail (the render body). The key is (pack, surface) — NOT surface alone — because surfaces are shared
-- vocabulary ("score" is "score" in every pack), so two packs can each define one and load-time last-write-wins
-- picks the winner (mirrors the loader's per-pack accumulation). An empty pack ships no rows => no templates
-- (the command then uses the engine's built-in fallback sheet).
CREATE TABLE display_defs (
  surface  TEXT NOT NULL,               -- "score", "who", "inventory", …
  pack     TEXT NOT NULL,
  body     JSONB NOT NULL DEFAULT '{}', -- the Lua render body
  PRIMARY KEY (pack, surface)
);
CREATE INDEX display_defs_pack_idx ON display_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS display_defs;
-- +goose StatementEnd
