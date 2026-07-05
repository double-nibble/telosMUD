-- +goose Up
-- +goose StatementBegin

-- Content-defined equipment vocabulary (#35, Round 11): the ordered set of WEAR SLOTS a pack declares
-- (head/body/hands/feet/wield/hold + any new slot like "waist"/"finger"), replacing the engine-fixed slot
-- enum. Like every other def table it is pure CONTENT: a (pack, ref) PK + a JSONB `body` tail (the slot's
-- display label, its display/selection order, and its equip-verb kind — worn/wield/hold). The world loads it
-- to build its runtime wear-slot vocab; an item's `wearable.locations` names these refs, and a worn item is
-- keyed by ref in the wearer's slot map. An empty pack ships no rows => the engine's DEFAULT slot set (the
-- classic Diku core), so the bare engine and any pack that declares none behave exactly as before #35.
CREATE TABLE wear_slot_defs (
  ref  TEXT NOT NULL,                -- the stable slot id ("head", "wield", a content "waist")
  pack TEXT NOT NULL,
  body JSONB NOT NULL DEFAULT '{}',  -- {"label": "...", "order": N, "kind": "worn"|"wield"|"hold"}
  PRIMARY KEY (pack, ref)
);
CREATE INDEX wear_slot_defs_pack_idx ON wear_slot_defs (pack);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS wear_slot_defs;
-- +goose StatementEnd
