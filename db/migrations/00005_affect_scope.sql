-- +goose Up
-- +goose StatementBegin

-- Room-scoped affects (docs/PHASE6-PLAN.md §1.3, [G13], Phase 6.4a). An affect now declares an
-- attachment SCOPE: "entity" (the Phase-5 default — attaches to a living creature) or "room"
-- (attaches to the ROOM entity, ticks over its occupants, lands on entrants — web/darkness/silence).
-- It is a first-class affect property exactly like stack_scope (00003), so it gets its own column
-- rather than riding the body JSONB — the loader reads it back the same way it reads stack_scope.
-- NOT NULL DEFAULT 'entity' keeps every pre-6.4a affect_defs row (and any insert that omits it) at
-- the unchanged entity-scoped behavior; a re-seed of a room affect (scope='room') sets it explicitly.
ALTER TABLE affect_defs ADD COLUMN scope TEXT NOT NULL DEFAULT 'entity';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE affect_defs DROP COLUMN scope;
-- +goose StatementEnd
