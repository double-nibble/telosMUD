-- +goose Up
-- +goose StatementBegin

-- Region definition content table (docs/WORLD-EVENTS.md §1, Phase 10.3). A region is a content-defined
-- grouping of member zones — an "area/city" a builder treats as one place, often several zones (a hot
-- zone can be split; region ≠ shard) — whose supra-zone state a region director owns. Like channel_defs
-- (00006) it is pure CONTENT: a ref/pack PK + a JSONB `body` tail, so the whole region SHAPE (its
-- display name + member zone refs) is a content WRITE, never a migration. The engine names NO region (no
-- hardcoded "midgaard-city"); it only knows the region_def shape + the scope hierarchy. An empty pack
-- ships no rows => no regions => only the world scope exists (the empty-boot invariant). The loader reads
-- this back into the same RegionDTO the embedded YAML produces.
CREATE TABLE region_defs (
  ref          TEXT PRIMARY KEY,            -- "midgaard-city" (also the telos.scope.region.<ref> token)
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- name + zones[] (member zone refs)
);

CREATE INDEX region_defs_pack_idx ON region_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS region_defs;
-- +goose StatementEnd
