-- +goose Up
-- +goose StatementBegin

-- Template/bundle definition content table (docs/PHASE11-PLAN.md §11.4, gap [G6c]). A bundle is a
-- content-defined class/race/background/feat/talent — a set of grants applied as a unit when the bundle
-- is chosen (chargen) or a track step grants it. Like the other def tables it is pure CONTENT: a ref/pack
-- PK + a JSONB `body` tail, so the whole bundle SHAPE (its kind discriminator + its grant op-list) is a
-- content WRITE, never a migration. The engine knows the KIND "bundle" (apply its grants), never
-- "fighter". An empty pack ships no rows => no bundles (the empty-boot invariant). The loader reads this
-- back into the same BundleDTO the embedded YAML produces.
CREATE TABLE bundle_defs (
  ref          TEXT PRIMARY KEY,            -- "fighter" | "elf" | "soldier-background"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- kind ("class"/"race"/...) + grants[] (a grant op-list)
);

CREATE INDEX bundle_defs_pack_idx ON bundle_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS bundle_defs;
-- +goose StatementEnd
