-- +goose Up
-- +goose StatementBegin

-- Definition (content) tables (docs/PERSISTENCE.md §1, docs/PHASE4-PLAN.md §2).
-- One table per definition KIND; each ROW is an instance (content). Every table is
-- ref/pack + stable relational columns + a JSONB `body` tail, so adding a new attribute/
-- ability/affect is a content WRITE, never a migration. Migrations stay rare and structural.

-- The zone is itself a definition: a pack can ship a whole zone, and rooms/resets FK to it.
CREATE TABLE zones (
  ref        TEXT PRIMARY KEY,            -- "midgaard"
  pack       TEXT NOT NULL,
  name       TEXT NOT NULL,              -- "The City of Midgaard"
  start_room TEXT,                        -- room a fresh login spawns in (Zone.startRoom)
  reset_secs INT  NOT NULL DEFAULT 0,     -- repop cadence; 0 = no timed reset
  body       JSONB NOT NULL DEFAULT '{}'
);

CREATE TABLE rooms (
  ref      TEXT PRIMARY KEY,             -- "midgaard:room:temple" (STABLE; not the display name)
  pack     TEXT NOT NULL,
  zone_ref TEXT NOT NULL REFERENCES zones(ref),
  name     TEXT NOT NULL,                -- display name "The Temple Square" (decoupled from ref)
  sector   TEXT,
  coord    JSONB,                         -- [x,y,z] minimap
  body     JSONB NOT NULL DEFAULT '{}'    -- flags, extra descs, etc.
);

-- exits.to_room keeps FK integrity on the world graph. The demo's
-- market --north--> darkwood:room:grove is a cross-ZONE exit; both demo zones are seeded
-- together (decision baked into Phase 4.1) so the FK holds across it.
CREATE TABLE exits (
  from_room TEXT NOT NULL REFERENCES rooms(ref),
  dir       TEXT NOT NULL,               -- "north"
  to_room   TEXT NOT NULL REFERENCES rooms(ref),
  door      JSONB,                        -- closed/locked/key
  PRIMARY KEY (from_room, dir)
);

CREATE TABLE item_prototypes (
  ref      TEXT PRIMARY KEY,             -- "midgaard:obj:torch"
  pack     TEXT NOT NULL,
  zone_ref TEXT REFERENCES zones(ref),
  short    TEXT NOT NULL,                -- "a wooden torch"
  long     TEXT NOT NULL,                -- ground line
  keywords TEXT[] NOT NULL DEFAULT '{}', -- targeting tokens
  body     JSONB NOT NULL DEFAULT '{}'   -- component template: physical/wearable/weapon/container
);

CREATE TABLE mob_prototypes (
  ref      TEXT PRIMARY KEY,
  pack     TEXT NOT NULL,
  zone_ref TEXT REFERENCES zones(ref),
  short    TEXT NOT NULL,
  long     TEXT NOT NULL,
  keywords TEXT[] NOT NULL DEFAULT '{}',
  body     JSONB NOT NULL DEFAULT '{}'   -- living/mob/AI components (stats are content, Phase 5)
);

CREATE TABLE zone_resets (
  id       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  pack     TEXT NOT NULL,
  zone_ref TEXT NOT NULL REFERENCES zones(ref),
  seq      INT  NOT NULL,                -- ordering within the reset script
  body     JSONB NOT NULL                -- {op:"spawn_item", proto:..., room:..., count:..., ...}
);

CREATE INDEX rooms_zone_ref_idx           ON rooms (zone_ref);
CREATE INDEX item_prototypes_zone_ref_idx ON item_prototypes (zone_ref);
CREATE INDEX mob_prototypes_zone_ref_idx  ON mob_prototypes (zone_ref);
CREATE INDEX zone_resets_zone_seq_idx     ON zone_resets (zone_ref, seq);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS zone_resets;
DROP TABLE IF EXISTS mob_prototypes;
DROP TABLE IF EXISTS item_prototypes;
DROP TABLE IF EXISTS exits;
DROP TABLE IF EXISTS rooms;
DROP TABLE IF EXISTS zones;
-- +goose StatementEnd
