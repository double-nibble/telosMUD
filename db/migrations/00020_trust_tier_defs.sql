-- +goose Up
-- +goose StatementBegin

-- Content-defined trust ladder (#27/#29, Round 9 Slice 0): the ordered set of account trust tiers a pack
-- declares (player/moderator/builder/architect/admin/…) with their ordinal RANK and granted capability
-- flags. Like every other def table it is pure CONTENT: a (pack, name) PK + rank + a JSONB `body` tail (the
-- granted-flags list). BOTH telos-account (tier validation + promote authz) and the world (rank + flag
-- derivation, command gating) load it, so tiers are a single authority. The key is (pack, name) — NOT name
-- alone — because tier NAMES are shared vocabulary ("admin" is "admin" in every pack), so two packs can each
-- define one and load-time last-write-wins picks the winner (mirrors the loader's per-pack accumulation). An
-- empty pack ships no rows => the engine's DEFAULT ladder (player/builder/admin) — the round-8 behavior.
CREATE TABLE trust_tier_defs (
  name TEXT NOT NULL,               -- "player", "builder", "admin", "moderator", …
  pack TEXT NOT NULL,
  rank INTEGER NOT NULL,            -- ordinal; higher = more trusted (gated commands compare ranks)
  body JSONB NOT NULL DEFAULT '{}', -- the granted reserved-flag list ({"flags": [...]})
  PRIMARY KEY (pack, name)
);
CREATE INDEX trust_tier_defs_pack_idx ON trust_tier_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS trust_tier_defs;
-- +goose StatementEnd
