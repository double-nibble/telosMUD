# Persistence

Two data domains with opposite access patterns, plus an operational tier. The
[extensibility pillar](PRINCIPLES.md) drives the central choice: **no game concept gets a
hardcoded column.** Adding an attribute, resource, or affect is *content*, never a schema
migration.

Status: **proposal** — three choices flagged in §10.

| Domain        | Store              | Pattern                | Examples                                  |
|---------------|--------------------|------------------------|-------------------------------------------|
| Content/defs  | Postgres (cached)  | read-mostly, bulk-load | attributes, abilities, affects, prototypes, rooms, resets, Lua |
| Player state  | Postgres + Redis   | read/write, durable    | accounts, characters, persistent objects, mail |
| Operational   | Redis              | hot, ephemeral         | sessions, presence, directory, locks, cooldowns, write-back cache |

---

## 1. Content / definitions

Everything the engine treats as a "definition" — attributes, resources, damage types, affects,
abilities, classes, races, mob/item prototypes, rooms, zone resets — is **content**, loaded
into shard memory at boot and cached. The engine boots with *none* of it (the bare-engine
invariant from [ABILITIES.md](ABILITIES.md) §10).

Storage shape (D2): **one table per definition *kind*** the engine models. The table is the
*kind*; each **row is an instance** (content). This stays on-pillar — `ability_defs` never
hardcodes `fireball`; `fireball` is a row — while buying real FK integrity for the world graph.

Every definition table follows the same pattern: a `ref` primary key, a `pack` column (for
the strip-the-stdlib guarantee), relational columns for that kind's **stable** fields, and a
JSONB **tail** for the open-ended remainder so unusual content still needs no migration.

```sql
CREATE TABLE attribute_defs (
  ref          TEXT PRIMARY KEY,
  pack         TEXT NOT NULL,
  display_name TEXT NOT NULL,
  value_kind   TEXT NOT NULL,            -- 'int' | 'float' | 'derived'
  default_base JSONB,                    -- literal or derivation formula
  body         JSONB NOT NULL DEFAULT '{}'
);

CREATE TABLE ability_defs (
  ref            TEXT PRIMARY KEY,
  pack           TEXT NOT NULL,
  name           TEXT NOT NULL,
  invocation     TEXT NOT NULL,          -- 'command' | 'proc' | 'passive'
  targeting      JSONB NOT NULL,         -- mode / scope / range / disposition
  requires       JSONB NOT NULL DEFAULT '{}',
  costs          JSONB NOT NULL DEFAULT '{}',
  cast_time      INT  NOT NULL DEFAULT 0,
  lag            INT  NOT NULL DEFAULT 0,
  cooldown       INT  NOT NULL DEFAULT 0,
  on_resolve     JSONB,                  -- declarative op-list (the common case)
  on_resolve_lua TEXT,                   -- Lua escape hatch (complex skills)
  messages       JSONB
);

CREATE TABLE rooms (
  ref      TEXT PRIMARY KEY,
  pack     TEXT NOT NULL,
  zone_ref TEXT NOT NULL,
  name     TEXT NOT NULL,
  sector   TEXT,
  coord    JSONB,                        -- [x,y,z] for the minimap
  body     JSONB NOT NULL DEFAULT '{}'
);
CREATE TABLE exits (
  from_room TEXT NOT NULL REFERENCES rooms(ref),
  dir       TEXT NOT NULL,
  to_room   TEXT NOT NULL REFERENCES rooms(ref),   -- FK integrity on the world graph
  door      JSONB,
  PRIMARY KEY (from_room, dir)
);
-- ...and likewise: resource_defs, damage_type_defs, affect_defs, class_defs, race_defs,
--    mob_prototypes, item_prototypes, zone_resets — same ref/pack/columns+JSONB-tail shape.
```

- **Migrations are rare and meaningful:** a new *row* (any amount of new flavor) never needs
  one; a new *column* means the engine gained a capability — legitimately engine work. The
  schema thus documents the engine's actual feature surface.
- **`pack`** on every table preserves the bare-engine guarantee: stripping the stdlib is
  `DELETE ... WHERE pack='stdlib'` across the definition tables (one helper, one transaction).
- **`content-lint`** still runs for cross-references FKs can't express (a Lua `on_resolve`
  naming an affect that exists, an ability referencing a known attribute).

## 2. Player / mutable state — accounts & characters

The durable identity tables. These have a few **engine-universal** relational columns (for
lookups, ops, and integrity) and push all **content-defined** state into JSONB.

```sql
CREATE TABLE accounts (
  id          UUID PRIMARY KEY,
  status      TEXT NOT NULL DEFAULT 'active',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE account_identities (                 -- OAuth links (details in the account doc)
  account_id   UUID REFERENCES accounts(id),
  provider     TEXT NOT NULL,                     -- 'google' | 'discord' | 'github'
  provider_uid TEXT NOT NULL,
  email        CITEXT,
  PRIMARY KEY (provider, provider_uid)
);

CREATE TABLE characters (
  id            UUID PRIMARY KEY,
  account_id    UUID NOT NULL REFERENCES accounts(id),
  name          CITEXT UNIQUE NOT NULL,           -- engine-universal: one name, one char
  zone_ref      TEXT,                             -- where to rehydrate
  room_ref      TEXT,
  state_version BIGINT NOT NULL DEFAULT 0,        -- optimistic concurrency (§8)
  state         JSONB  NOT NULL,                  -- ALL content-defined state (§4)
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_saved_at TIMESTAMPTZ,
  last_login_at TIMESTAMPTZ,
  playtime_secs BIGINT NOT NULL DEFAULT 0,
  deleted_at    TIMESTAMPTZ                        -- soft delete
);
```

Relational columns are only things true of *every* character regardless of ruleset (identity,
location, save bookkeeping). Note there is **no `level`, `class`, `hp`, or `str` column** —
those are content concepts living in `state`.

## 3. The `state` JSONB

The character's content-defined state — a direct serialization of the in-memory authoritative
entity, and the same shape as the `PlayerSnapshot` in [PROTOCOL.md](PROTOCOL.md):

```json
{
  "templates": { "race": "elf", "class": ["mage"] },
  "attributes": { "strength": 12, "intellect": 18, "level": 23, "xp": 45123 },
  "resources":  { "hp": {"cur": 120}, "mana": {"cur": 40}, "stamina": {"cur": 90} },
  "skills":     { "fireball": {"prof": 71}, "dodge": {"prof": 55} },
  "affects":    [ {"id":"haste","dur":40,"mag":1,"stacks":1,"source":"self"} ],
  "flags":      { "pvp_opt_in": true },
  "inventory":  [ /* item instances, §5 */ ],
  "equipment":  { "wield": {"proto":"obj:longsword","delta":{"enchant":2}} },
  "quests":     { "...": {} }
}
```

Because we load and save a character as a *whole unit* (it lives in shard memory; the snapshot
is the transfer form), a single JSONB column round-trips cleanly. If save volume ever demands
it, the hot-churn subtree (`resources`) can be split into its own column so frequent
checkpoints don't rewrite the whole blob — a profiling-driven escape hatch, not a v1 concern.

## 4. Item instances & the flyweight model

Per [MUDLIB.md](MUDLIB.md) §5, an instance is a **prototype ref + copy-on-write delta**.
Persistence stores exactly that delta — never the prototype's immutable fields.

- **Carried items** live nested in the character's `state.inventory` / `equipment` (loaded and
  saved atomically with the character). Containers nest as nested JSON.
- **World-persistent objects** — housing contents, items in persistent rooms, mail
  attachments, auction lots — exist independent of any logged-in character and get their own
  table:

```sql
CREATE TABLE object_instances (
  id            UUID PRIMARY KEY,
  proto         TEXT NOT NULL,             -- prototype ref (flyweight source)
  delta         JSONB NOT NULL DEFAULT '{}',
  location_kind TEXT NOT NULL,             -- 'room' | 'container' | 'mailbox' | 'account_vault'
  location_ref  TEXT NOT NULL,
  state_version BIGINT NOT NULL DEFAULT 0
);
```

- **Most items are never persisted at all.** They spawn from zone resets (§6) and vanish on
  repop. Only instances flagged `persistent` hit `object_instances`. A room of 40 kobolds and
  their rusty daggers costs zero database rows.

## 5. Ephemeral world & zone resets

The living world is largely *reconstructed*, not stored:

- Zone **reset/repop** definitions are content (`definitions` kind `reset`). On zone boot and
  on the reset timer, the engine spawns prototype instances per the reset script.
- Spawned mobs/items are **ephemeral** — pure runtime, no DB footprint — unless flagged
  persistent.
- So a cold start = load content + run resets + load logged-in characters + load
  `object_instances`. The durable surface area is small, which is what makes the stated scale
  affordable.

## 6. Durability ladder & save strategy

Three tiers, each protecting against a different failure, with widening write cadence:

1. **Shard memory** — the authoritative live state. Mutated only in the zone goroutine.
2. **Redis checkpoint** — a frequent (~10s), cheap write-back mirror. Its job is to shrink the
   *crash data-loss window*: if a shard dies, recovery replays the last Redis checkpoint
   rather than the last Postgres flush.
3. **Postgres** — durable record. Flushed on a cadence (~60s), on **logout**, on **significant
   events** (level, major loot, quest completion), and on **shard drain** (rolling redeploy).

Cross-shard movement does **not** route through any store — it carries the fat snapshot
directly (PROTOCOL.md), so zone borders stay store-independent.

## 7. Concurrency: state_version + single-session

Two layers keep "one writer" honest:

- **Single-session lock** (Redis): one live session per character; a second login is rejected
  or takes over (configurable).
- **`state_version`** (optimistic concurrency): every save is
  `UPDATE ... SET state=$1, state_version=state_version+1 WHERE id=$2 AND state_version=$3`.
  Zero rows updated ⇒ a stale writer (e.g. a mis-fired handoff) tried to save; the write is
  rejected and the shard reconciles. The snapshot carries `state_version` so it survives a
  migration. This is the backstop behind the handoff `epoch`.

## 8. Content loading & hot reload

- Shards bulk-load `definitions` into memory at boot (filtered by enabled `pack`s).
- On content change (OLC edit or deploy), the writer publishes an invalidation keyed by
  `(kind, ref)` on **NATS**; shards reload just the affected rows (Lua included — ties to the
  per-zone VM hot-reload in ARCHITECTURE.md §3).
- Because game-design changes are JSONB/Lua, they need **no schema migration** — the whole
  point of the pillar showing up in ops.

## 9. Migrations

`goose`/`atlas` manage only the thin relational skeleton (`accounts`, `account_identities`,
`characters`, `object_instances`, `definitions`). New attributes/abilities/affects are content
writes, not migrations. Migrations stay rare and structural.
