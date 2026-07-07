-- +goose Up
-- +goose StatementBegin

-- Content version + pack registry (#212 slice 4). These make Postgres self-describing about WHICH
-- published content version it currently serves, so (a) a shard can tell if it has applied the current
-- version (reconcile-on-join), (b) the world serves exactly the pulled version's packs without an
-- operator-maintained enabled list (manifest-driven), and (c) the importer can PRUNE a pack a new
-- version drops (the registry is the diff source). Written together with the pack rows in ONE tx by
-- store.ImportVersion, so a crash never leaves rows without a version marker, or a marker without rows.

-- content_version is a SINGLETON (one row, id pinned to 1): the version currently materialized in this
-- database. `version` is the monotonic LOGICAL version (#209) — nanos-scale, bumped GREATEST(version+1,
-- now_nanos) inside the import tx so it never inverts and never collides with a wall-clock fallback. The
-- git SHA is the immutable identity; the manifest tag + content hash are the human/audit view.
CREATE TABLE content_version (
  id               INT PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  version          BIGINT NOT NULL DEFAULT 0,   -- the monotonic logical content version (0 = never imported)
  content_sha      TEXT   NOT NULL DEFAULT '',   -- immutable git SHA of the published tree
  manifest_version TEXT   NOT NULL DEFAULT '',   -- the human manifest tag (e.g. "v1.4.0")
  content_hash     TEXT   NOT NULL DEFAULT '',   -- corroborating packs/ content hash
  imported_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Seed the singleton at version 0 so ImportVersion can UPDATE ... RETURNING the bumped value.
INSERT INTO content_version (id) VALUES (1);

-- content_pack_registry is the LIVE pack set the database currently serves — the manifest-driven world
-- reads it as the enabled-pack list, and the importer diffs it against a new version's packs to find the
-- packs to prune. Overwritten to exactly the new version's pack set on each import. `version` records the
-- version that last wrote each pack (all rows share the current version after an import).
CREATE TABLE content_pack_registry (
  pack    TEXT PRIMARY KEY,
  version BIGINT NOT NULL
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS content_pack_registry;
DROP TABLE IF EXISTS content_version;
-- +goose StatementEnd
