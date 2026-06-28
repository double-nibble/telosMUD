-- +goose Up
-- +goose StatementBegin

-- Comms channel content table (docs/PHASE8-PLAN.md slice 8.3, P8-D3). Channels are CONTENT, exactly
-- like every other pack-global definition (00003/00004): a ref/pack PK + a JSONB `body` tail, so the
-- whole channel SHAPE — verb(s), color/format template, access predicate, default_on, history — is a
-- content WRITE, never a migration. The engine names NO channel (no hardcoded `gossip`); it only knows
-- the channel_def shape and the comms transport. An empty pack ships no rows => no channel verbs (the
-- empty-boot invariant). The loader reads this back into the same ChannelDTO the embedded YAML produces.
CREATE TABLE channel_defs (
  ref          TEXT PRIMARY KEY,            -- "gossip"
  pack         TEXT NOT NULL,
  body         JSONB NOT NULL DEFAULT '{}'  -- name + words[] + color + format + access + default_on + history
);

CREATE INDEX channel_defs_pack_idx ON channel_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS channel_defs;
-- +goose StatementEnd
