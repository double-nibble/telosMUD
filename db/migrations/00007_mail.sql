-- +goose Up
-- +goose StatementBegin

-- Durable player MAIL inbox (docs/PHASE8-PLAN.md slice 8.7, P8-D6; docs/PERSISTENCE.md already lists
-- `mail` under the durable Postgres player tier). Mail is the persistent, queryable read/send model —
-- a relational row per message fits the inbox (list / read / mark-read / delete) better than a log
-- (JetStream, which P8-D6 rejected for the read model). Unlike a `*_def` content table this is MUTABLE
-- player state, so it is engine mechanism (not content): an empty pack still has mail; no row exists
-- until a player sends one. Both to_player and from_player are CITEXT (case-insensitive), matching the
-- `characters.name` identity key — a player id today is the login name (OQ-5, Phase-14 auth migrates it).
--
-- SECURITY (P8-A2 / read-delete scoping): from_player is ENGINE-SET by the source world from the live
-- *Entity, never a client field — the store never trusts a caller-supplied sender. Every read/delete is
-- player-scoped at the QUERY (WHERE ... AND to_player = $self), so a player cannot read or delete another
-- player's mail by guessing an id (the access control lives in the SQL, not just the command).
CREATE TABLE mail (
  id          UUID PRIMARY KEY,            -- the message id (server-minted v4 UUID)
  to_player   CITEXT NOT NULL,             -- recipient (the inbox owner); the read/delete scope key
  from_player CITEXT NOT NULL,             -- ENGINE-SET sender identity (never client-supplied)
  subject     TEXT NOT NULL DEFAULT '',
  body        TEXT NOT NULL DEFAULT '',
  sent_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  read_at     TIMESTAMPTZ                  -- NULL => unread; set on first read
);

-- The inbox read is "newest-first for one recipient", so index to_player (the scope key). The
-- (to_player, sent_at DESC) shape serves the ORDER BY without a separate sort.
CREATE INDEX mail_to_player_idx ON mail (to_player, sent_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS mail;
-- citext is left installed (other tables use it); dropping a shared extension is out of scope here.
-- +goose StatementEnd
