-- +goose Up
-- +goose StatementBegin

-- Pack-scoped, ZONE-INDEPENDENT definition tables (docs/PHASE5-PLAN.md §2.1, docs/ABILITIES.md
-- §1-2). Unlike the Phase-4 definition tables (zones/rooms/prototypes), these are GLOBAL to a
-- pack: a `strength` attribute or a `fire` damage type is not owned by Midgaard. Same shape as
-- everything else — ref/pack PK + a few stable relational columns + a JSONB `body` tail — so
-- adding a new attribute/resource/damage-type/affect/ability is a content WRITE, never a
-- migration. Slice 5.1 USES attribute_defs/resource_defs/damage_type_defs; affect_defs/ability_defs
-- are created here so the schema is whole (filled by 5.2/5.3). class_defs/race_defs are RESERVED
-- (deferred chargen/progression, docs/PHASE5-PLAN.md §2.1) — created, empty, unused.

CREATE TABLE attribute_defs (
  ref          TEXT PRIMARY KEY,            -- "strength", "max_hp"
  pack         TEXT NOT NULL,
  display_name TEXT NOT NULL,               -- "Strength"
  value_kind   TEXT NOT NULL,               -- 'int' | 'float' | 'derived'
  default_base JSONB,                        -- literal {"lit":n} or formula AST {"expr":[...]} (§1.1)
  body         JSONB NOT NULL DEFAULT '{}'   -- min/max/clamp, display
);

CREATE TABLE resource_defs (
  ref          TEXT PRIMARY KEY,            -- "hp", "mana"
  pack         TEXT NOT NULL,
  display_name TEXT NOT NULL,
  max_attr     TEXT,                         -- derived-attr ref that caps it (e.g. "max_hp")
  vital        BOOLEAN NOT NULL DEFAULT false,
  body         JSONB NOT NULL DEFAULT '{}'   -- regen, depleted_threshold, on_depleted op-list
);

CREATE TABLE damage_type_defs (
  ref          TEXT PRIMARY KEY,            -- "fire", "slash"
  pack         TEXT NOT NULL,
  display_name TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'   -- resist/vuln/immune matrix, color
);

-- Created now so the schema is whole; first USED in slice 5.2 (affects) / 5.3 (abilities).
CREATE TABLE affect_defs (
  ref          TEXT PRIMARY KEY,
  pack         TEXT NOT NULL,
  name         TEXT NOT NULL,
  category     TEXT,                         -- dispel/cure targeting
  stacking     TEXT NOT NULL DEFAULT 'refresh',
  max_stacks   INT  NOT NULL DEFAULT 1,
  stack_scope  TEXT NOT NULL DEFAULT 'source', -- 'source' (per (ref,source)) | 'target' (per ref)
  dispellable  BOOLEAN NOT NULL DEFAULT true,
  body         JSONB NOT NULL DEFAULT '{}'   -- duration, modifiers, prevents[], tick{}, on_apply/expire
);

CREATE TABLE ability_defs (
  ref            TEXT PRIMARY KEY,
  pack           TEXT NOT NULL,
  name           TEXT NOT NULL,
  invocation     TEXT NOT NULL,              -- 'command' | 'proc' | 'passive'
  targeting      JSONB NOT NULL DEFAULT '{}',-- mode/scope/range/disposition
  tags           TEXT[] NOT NULL DEFAULT '{}',-- §6 CC tags
  requires       JSONB NOT NULL DEFAULT '{}',
  costs          JSONB NOT NULL DEFAULT '{}',
  cast_time      INT NOT NULL DEFAULT 0,
  lag            INT NOT NULL DEFAULT 0,
  cooldown       INT NOT NULL DEFAULT 0,
  on_resolve     JSONB,                       -- declarative op-list (Phase 5.3)
  on_resolve_lua TEXT,                        -- RESERVED, read-not-run (Phase 7)
  messages       JSONB
);

-- RESERVED (deferred): chargen/progression class & race definitions. Created so a later slice
-- (point-buy, level-up grants) adds the logic without a migration; unused in Phase 5.
CREATE TABLE class_defs (
  ref          TEXT PRIMARY KEY,
  pack         TEXT NOT NULL,
  display_name TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'
);

CREATE TABLE race_defs (
  ref          TEXT PRIMARY KEY,
  pack         TEXT NOT NULL,
  display_name TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'
);

CREATE INDEX attribute_defs_pack_idx   ON attribute_defs (pack);
CREATE INDEX resource_defs_pack_idx    ON resource_defs (pack);
CREATE INDEX damage_type_defs_pack_idx ON damage_type_defs (pack);
CREATE INDEX affect_defs_pack_idx      ON affect_defs (pack);
CREATE INDEX ability_defs_pack_idx     ON ability_defs (pack);
CREATE INDEX class_defs_pack_idx       ON class_defs (pack);
CREATE INDEX race_defs_pack_idx        ON race_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS race_defs;
DROP TABLE IF EXISTS class_defs;
DROP TABLE IF EXISTS ability_defs;
DROP TABLE IF EXISTS affect_defs;
DROP TABLE IF EXISTS damage_type_defs;
DROP TABLE IF EXISTS resource_defs;
DROP TABLE IF EXISTS attribute_defs;
-- +goose StatementEnd
