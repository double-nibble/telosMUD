-- +goose Up
-- +goose StatementBegin

-- Combat content tables (docs/COMBAT.md §3, docs/PHASE6-PLAN.md, Phase 6.3a). Same shape as every
-- other pack-global definition table (00003): a ref/pack PK + a JSONB `body` tail, so the whole
-- to-hit/avoidance/damage SHAPE of a combat profile is a content WRITE, never a migration — the
-- engine only runs the pipeline (P6-D6). The mob prototype's `living` block (its stat sheet + the
-- combat_profile ref) already rides the existing mob_prototypes.body JSONB; this migration adds the
-- pack-GLOBAL pieces that have no home yet: the named combat profiles and the pack's default_combat.

-- One named combat profile (the goblin's `melee`, the player default): the to-hit check body, the
-- ordered avoidance ladder, and the damage-bonus formula all live in the JSONB body. ref+pack is the
-- established per-kind key; the body is the CombatProfileDTO sub-shape (to_hit/avoidance/damage_bonus).
CREATE TABLE combat_profile_defs (
  ref          TEXT PRIMARY KEY,            -- "melee"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- to_hit (check body) + avoidance[] + damage_bonus (formula)
);

CREATE INDEX combat_profile_defs_pack_idx ON combat_profile_defs (pack);

-- pack_meta is the home for a pack's GLOBAL SCALARS that aren't per-kind definitions — today just
-- default_combat (the combat profile a player fights with when its prototype names none). One row per
-- pack (ref = pack name), the scalars in a JSONB body so a future pack-level global is a content write,
-- not a migration. Kept tiny and separate from the per-kind def tables on purpose (it is pack metadata,
-- not a definition kind). deletePack strips it on re-seed so a re-import replaces rather than collides.
CREATE TABLE pack_meta (
  pack         TEXT PRIMARY KEY,            -- the pack name (one row per pack)
  body         JSONB NOT NULL DEFAULT '{}'  -- pack-level scalars: { "default_combat": "melee" }
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS pack_meta;
DROP TABLE IF EXISTS combat_profile_defs;
-- +goose StatementEnd
