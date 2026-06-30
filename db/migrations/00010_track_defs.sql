-- +goose Up
-- +goose StatementBegin

-- Advancement-track definition content table (docs/PHASE11-PLAN.md §11.2, gap [G6a]). A track is a
-- content-defined progression mechanism — XP/level, a use-based skill, a guild rank — and is the UNION
-- abstraction for all advancement modes (XP-auto, train, point-buy, use-based) differing only in which
-- event feeds the progress attribute. Like channel_defs/region_defs it is pure CONTENT: a ref/pack PK + a
-- JSONB `body` tail, so the whole track SHAPE (progress attr, optional level attr, thresholds, and the
-- per-step grant op-lists) is a content WRITE, never a migration. The engine names NO track and grows no
-- "level" concept — `level` is just an attribute some tracks happen to raise. An empty pack ships no rows
-- => no tracks (the empty-boot invariant). The loader reads this back into the same TrackDTO the embedded
-- YAML produces.
CREATE TABLE track_defs (
  ref          TEXT PRIMARY KEY,            -- "warrior_xp"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- progress_attr + level_attr + thresholds[] + steps[] (grant op-lists)
);

CREATE INDEX track_defs_pack_idx ON track_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS track_defs;
-- +goose StatementEnd
