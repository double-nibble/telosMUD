-- +goose Up
-- +goose StatementBegin

-- Persist the terminal `color on/off` preference across sessions (#23, Round 17 Track 1). Color is an EDGE
-- concern (the gate's {{TOKEN}} -> SGR rendering), so the gate reads/writes this directly via telos-account —
-- it never routes through the world. NULLABLE on purpose: NULL = the player has never set a preference, so
-- the gate keeps its default (color ON). false/true are the explicitly-chosen states.
ALTER TABLE accounts ADD COLUMN IF NOT EXISTS color_enabled BOOLEAN;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE accounts DROP COLUMN IF EXISTS color_enabled;
-- +goose StatementEnd
