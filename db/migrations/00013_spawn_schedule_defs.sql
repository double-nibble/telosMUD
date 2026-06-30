-- +goose Up
-- +goose StatementBegin

-- Scheduled-spawn definition content table (docs/LOOT-AND-SPAWNS.md §1, Phase 12.4). A scheduled spawn is
-- a long-timer boss the DIRECTOR owns (a weekly world boss) — distinct from per-zone resets. Like every
-- other def table it is pure CONTENT: a ref/pack PK + a JSONB `body` tail (proto / zone / room / interval
-- / on_missed / announce). The engine runs the scheduler + names no boss; a pack supplies the schedule.
-- An empty pack ships no rows => no scheduled spawns. The director loads these + persists each schedule's
-- next-spawn time in scope state (restart-safe).
CREATE TABLE spawn_schedule_defs (
  ref          TEXT PRIMARY KEY,            -- "boss:duskwall_warden"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- proto + zone + room + interval_after_death_sec + on_missed + announce
);
CREATE INDEX spawn_schedule_defs_pack_idx ON spawn_schedule_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS spawn_schedule_defs;
-- +goose StatementEnd
