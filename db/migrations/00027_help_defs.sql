-- +goose Up
-- +goose StatementBegin

-- Builder-defined help topic content table (#64, Round 29). A help topic is pure CONTENT: a browsable
-- `help` / `help <topic>` reads it, the engine names no topic. Like every other def table it is a ref/pack
-- PK + a JSONB `body` tail (title / category / keywords / body / see_also). The built-in command set is
-- auto-included by the `help` command at runtime, so an empty pack still yields a usable command index —
-- the engine ships no help rows. Boot-load-only, mirroring recipe_defs (no single-ref hot-reload kind yet).
CREATE TABLE help_defs (
  ref          TEXT PRIMARY KEY,            -- "help:combat"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- title + category + keywords + body + see_also
);
CREATE INDEX help_defs_pack_idx ON help_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS help_defs;
-- +goose StatementEnd
