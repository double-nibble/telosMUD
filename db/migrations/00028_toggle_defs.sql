-- +goose Up
-- +goose StatementBegin

-- Content-defined PLAYER TOGGLE content table (#358). A toggle is pure CONTENT: an on/off player
-- preference (e.g. an `overworld` minimap switch) whose verb the engine registers and whose state Lua
-- content reads via self:toggle(); the engine names no toggle. Like every other def table it is a ref/pack
-- PK + a JSONB `body` tail (name / words / default_on / desc). Boot-load-only, mirroring help_defs /
-- display_defs (no single-ref hot-reload kind yet). An empty pack ships no rows => no toggle verbs.
CREATE TABLE toggle_defs (
  ref          TEXT PRIMARY KEY,            -- "overworld"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- name + words + default_on + desc
);
CREATE INDEX toggle_defs_pack_idx ON toggle_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS toggle_defs;
-- +goose StatementEnd
