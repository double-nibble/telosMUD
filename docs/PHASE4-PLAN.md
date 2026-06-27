# Phase 4 — Persistence & content pipeline (implementation plan)

The "everything is content" backbone. After Phase 4 the bare engine boots **empty**,
loads a content **pack** from Postgres, runs the demo world identically, and a
**character + world state survive a process restart**. Nothing in the game's flavor is
Go code anymore: `newDemoZone` becomes seeded rows.

Status: **plan / pre-implementation.** No production code, schema, or tests changed yet.
This document exists to surface the tech-choice decisions before slice 1.

Source of truth: [PERSISTENCE.md](PERSISTENCE.md). Boundary: [ROADMAP.md](ROADMAP.md)
Phase 4. Bare-engine guarantee: [PRINCIPLES.md](PRINCIPLES.md). Crash-failover
compatibility target: [PLACEMENT.md](PLACEMENT.md) §5–6. Identity model:
[MUDLIB.md](MUDLIB.md) §1, §5.

---

## 0. What exists today (the integration surface)

- **Prototype cache** (`internal/world/prototype.go`): `protoCache` is a per-shard,
  build-once-then-read-only `map[ProtoRef]*Prototype`. `Zone.spawn(ref)` makes a
  flyweight COW instance from it. **This is unchanged by Phase 4.** Phase 4 only
  changes *who fills the cache*.
- **`newDemoZone`** (`internal/world/world.go`): hand-authors midgaard + darkwood rooms,
  exits, torch/helmet/sword/chest prototypes, and spawns instances — all in Go. **This
  is what the content loader replaces.**
- **Entity identity** (`internal/world/identity.go`, `entity.go`): `ProtoRef` (live),
  `RuntimeID` (live), `PersistID` (plumbed but always nil). **Phase 4 makes `PersistID`
  real for player entities.**
- **Player session** (`internal/world/session.go`, `zone.go` `attach`): a player is a
  `prototype==nil` entity built by `newPlayerEntity`. Today it has **no durable state**;
  login spawns a blank entity, logout/reap just drops it. **Phase 4 adds load-on-login /
  save-on-logout + the checkpoint ladder.**
- **Snapshot** (`internal/world/handoff.go` `buildSnapshot`): carries only
  `character_id / name / applied_seq`. **Phase 4's `state` JSONB shares the same logical
  shape** (the snapshot is the in-flight form, the row is the at-rest form) — keep them
  convergent.
- **Directory** (`internal/directory/redis.go`): the working Redis pattern (Lua CAS
  scripts, TTL leases, `SetPlayerShard`/`PlayerEpoch`). **Redis checkpoints reuse this
  client + namespacing convention.**
- **Config** (`internal/config/config.go`): `Postgres.DSN`, `Redis.Addr`, `NATS.URL`
  already parsed; `TELOS_POSTGRES_DSN` already wired in compose. Postgres + NATS services
  are in `deploy/docker-compose.yml` but **completely unused** today.
- **Off-goroutine I/O pattern**: `server.go` already reads `PlayerEpoch` off the zone
  goroutine before posting `attachMsg`; `beginHandoff` does all directory I/O in a
  spawned goroutine and posts results back as inbox messages. **The character
  load/save path follows this exact pattern — never block the zone loop.**

---

## 1. Tech-choice decisions (DECIDE BEFORE SLICE 1)

| # | Decision | Recommendation | Trade-off / why |
|---|----------|----------------|-----------------|
| D1 | **Migration tool** | **goose** (`pressly/goose`) as a library + CLI | PERSISTENCE.md §9 already names goose/atlas. goose is a single Go dependency, embeds `.sql` migrations via `embed.FS`, runs both from a `make` target and programmatically on boot, and has no DSL to learn — migrations stay plain SQL, which suits the JSONB-tail schema where the SQL *is* the spec. atlas is more powerful (declarative diffing) but pulls a heavier toolchain and a HCL schema language we don't need for a "rare, structural" migration cadence. golang-migrate works but its file-pair convention and separate binary are clunkier than goose's embed story. Rejected: hand-rolled (re-inventing version tracking is a maintenance tax). |
| D2 | **DB access layer** | **pgx v5 directly** (`jackc/pgx`), thin hand-written query funcs in a `store` package | The schema is *narrow* (≈8 definition tables + 4 state tables) and every table is `ref/pack + few columns + one JSONB tail`. That shape is the worst case for an ORM and a poor fit for sqlc's column-by-column codegen (the open-ended JSONB tail isn't a typed column — sqlc would just hand back `[]byte` and we'd `json.Unmarshal` anyway). pgx gives `pgxpool`, native JSONB, `CopyFrom` for bulk content load, and `RETURNING`-based optimistic-concurrency updates with zero magic. sqlc is the runner-up and could be layered on later for the relational columns if query volume grows; not worth the codegen step in v1. Rejected: ORM (fights the pillar — hides SQL, tempts per-stat columns). |
| D3 | **Where migrations live / how they run** | `db/migrations/*.sql` embedded via `embed.FS`; a `make migrate` target (goose CLI) for dev/CI; **optional auto-migrate on world boot guarded by `TELOS_DB_AUTOMIGRATE`** (default off in prod, on in dev) | Keeps the single source of truth in one place, runnable from CI before tests and from a binary on a fresh dev box. Auto-migrate-on-boot is convenient for dev/compose but dangerous under multi-shard concurrency in prod (N shards racing migrations) — so it's opt-in and advisory-locked (goose takes a Postgres advisory lock, so concurrent boots serialize safely). |
| D4 | **Seed/content load vs migrations** | Seed the **stdlib demo pack as data**, NOT as a migration. A separate `content` package owns `import` (file→rows) and `export` (rows→file); `make seed` loads the demo pack | Migrations are structural (engine capability); content is rows (flavor). Mixing them violates PERSISTENCE.md §9. The demo pack ships as a checked-in content file (YAML or JSON) imported by `make seed`, so it's overridable/strippable per the bare-engine rule (`DELETE WHERE pack='stdlib'`). |
| D5 | **JSONB (de)serialization** | Hand-written **transfer structs** in the `store`/`content` layer (plain `encoding/json` tags), mapped to/from the `internal/world` component structs by explicit `loadX`/`dumpX` functions. Do **not** `json`-tag the `world` structs directly | The `world` component fields are unexported and tuned for the runtime (hot pointers, COW). Coupling the on-disk JSON shape to those structs would freeze the wire format to internal layout and leak persistence concerns into the simulation core. A thin DTO boundary keeps the `state` JSONB shape (which equals the `PlayerSnapshot` shape, PERSISTENCE.md §3) stable and independently testable, and is the natural place the loader maps `body`/`state` JSON onto components. |

**Open question for the user (D4a):** demo-pack file format — **YAML** (human-authored,
matches existing `config` yaml dep) vs **JSON** (no new dep, but noisier to hand-edit).
Recommendation: YAML for the authored pack, since builders read/write it; the DB column
is JSONB regardless.

---

## 2. Schema sketch

All definition tables follow PERSISTENCE.md §1: `ref` PK, `pack` (strip-the-stdlib),
stable relational columns, and a JSONB **tail** (`body`). Migrations create only the
relational skeleton; rows are content.

### Definition (content) tables

```sql
-- The zone is itself a definition (so a pack can ship a whole zone, and resets FK to it).
CREATE TABLE zones (
  ref        TEXT PRIMARY KEY,        -- "midgaard"
  pack       TEXT NOT NULL,
  name       TEXT NOT NULL,           -- "The City of Midgaard"
  reset_secs INT  NOT NULL DEFAULT 0, -- repop cadence; 0 = no timed reset
  body       JSONB NOT NULL DEFAULT '{}'
);

CREATE TABLE rooms (
  ref      TEXT PRIMARY KEY,                       -- "midgaard:room:temple" (STABLE; not the display name)
  pack     TEXT NOT NULL,
  zone_ref TEXT NOT NULL REFERENCES zones(ref),
  name     TEXT NOT NULL,                          -- display name "The Temple Square" (tool-minted later, decoupled from ref)
  sector   TEXT,
  coord    JSONB,                                  -- [x,y,z] minimap
  body     JSONB NOT NULL DEFAULT '{}'             -- flags, extra descs, etc.
);

CREATE TABLE exits (
  from_room TEXT NOT NULL REFERENCES rooms(ref),
  dir       TEXT NOT NULL,                         -- "north"
  to_room   TEXT NOT NULL REFERENCES rooms(ref),   -- FK integrity on the world graph (intra-pack);
                                                   --   cross-zone/cross-shard exits handled below
  door      JSONB,                                 -- closed/locked/key
  PRIMARY KEY (from_room, dir)
);

CREATE TABLE item_prototypes (
  ref      TEXT PRIMARY KEY,                       -- "midgaard:obj:torch"
  pack     TEXT NOT NULL,
  zone_ref TEXT REFERENCES zones(ref),
  short    TEXT NOT NULL,                          -- "a wooden torch"
  long     TEXT NOT NULL,                          -- ground line
  keywords TEXT[] NOT NULL DEFAULT '{}',           -- targeting tokens
  body     JSONB NOT NULL DEFAULT '{}'             -- component template: physical/wearable/weapon/container...
);

CREATE TABLE mob_prototypes (
  ref      TEXT PRIMARY KEY,
  pack     TEXT NOT NULL,
  zone_ref TEXT REFERENCES zones(ref),
  short    TEXT NOT NULL,
  long     TEXT NOT NULL,
  keywords TEXT[] NOT NULL DEFAULT '{}',
  body     JSONB NOT NULL DEFAULT '{}'             -- living/mob/AI/components (stats are content, Phase 5)
);

CREATE TABLE zone_resets (
  id       BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  pack     TEXT NOT NULL,
  zone_ref TEXT NOT NULL REFERENCES zones(ref),
  seq      INT  NOT NULL,                          -- ordering within the reset script
  body     JSONB NOT NULL                          -- {op:"spawn_mob", proto:..., room:..., max:..., ...}
);
```

**Room-identity decision (recorded):** `ref` is the stable PK and the exit target;
the display `name` is a separate column, free to change without breaking exits or saves
(matches `identity.go` ProtoRef comment). A future OLC tool mints refs.

**Exits & FK integrity:** the FK on `exits.to_room` enforces the world graph *within
what is loaded*. The demo's `market --north--> darkwood:room:grove` is a **cross-zone**
exit; if both zones live in the same pack/DB the FK holds. For genuinely cross-shard
exits where the target zone may not be in this DB, slice 1 will either (a) seed both
zones (they already coexist) or (b) drop the FK on cross-zone exits and keep
`content-lint` (PERSISTENCE.md §1) as the cross-reference check. **Recommend (a) for the
demo** (both zones seeded together); revisit when packs are split.

### Durable STATE tables (PERSISTENCE.md §2)

```sql
CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE accounts (                            -- minimal stub; full account model is Phase 14
  id         UUID PRIMARY KEY,
  status     TEXT NOT NULL DEFAULT 'active',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE characters (
  id            UUID PRIMARY KEY,                  -- the PersistID, now REAL (MUDLIB §1)
  account_id    UUID REFERENCES accounts(id),      -- nullable until Phase 14 auth
  name          CITEXT UNIQUE NOT NULL,            -- engine-universal: one name, one char
  zone_ref      TEXT,                              -- where to rehydrate
  room_ref      TEXT,
  state_version BIGINT NOT NULL DEFAULT 0,         -- optimistic concurrency (§4)
  state         JSONB  NOT NULL DEFAULT '{}',      -- ALL content-defined state == PlayerSnapshot shape
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_saved_at TIMESTAMPTZ,
  last_login_at TIMESTAMPTZ,
  playtime_secs BIGINT NOT NULL DEFAULT 0,
  deleted_at    TIMESTAMPTZ
);

CREATE TABLE object_instances (                    -- world-persistent items (housing, persistent rooms); flag-gated
  id            UUID PRIMARY KEY,
  proto         TEXT NOT NULL,
  delta         JSONB NOT NULL DEFAULT '{}',
  location_kind TEXT NOT NULL,                     -- 'room' | 'container' | 'mailbox' | ...
  location_ref  TEXT NOT NULL,
  state_version BIGINT NOT NULL DEFAULT 0
);
```

`object_instances` is **defined but unused in v1** (slice 4 only spawns ephemeral reset
content; no demo item is flagged `persistent`). It is created now so the schema is whole
and the flyweight-persistence path has a home, per PERSISTENCE.md §4.

---

## 3. The content loader (replaces `newDemoZone`)

The loader is a new `internal/content` (or `internal/world` sub-)package that reads
definition rows (filtered by enabled `pack`s) and **fills the existing `protoCache`** —
so `spawn()`, the COW model, and every Phase 3 test are untouched.

Flow at shard boot (`newShard` / `buildShard`):

1. `store.Open(dsn)` → `pgxpool`. If Postgres is unreachable, log a warning and **boot
   empty** (bare-engine invariant) — exactly as `buildShard` already degrades to
   single-shard when Redis is down.
2. `content.Load(ctx, store, enabledPacks)` returns a `*LoadedContent`:
   - `zones[ref]`, and for each zone the rooms/exits, item & mob prototypes, resets.
   - It calls `protoCache.define(ref, keywords, short, long, comps)` for every room/item/
     mob prototype — the loader is the new caller of `define`, replacing `defineRoom`/
     `defineTorch`/etc. Component templates are built by the **DTO→component mapper**
     (D5): `body` JSON → `*Physical`/`*Wearable`/`*Weapon`/`*Container`/`*Room`.
3. `newShard` then, per hosted zone, spawns room singletons + runs resets (slice 4) to
   populate instances — replacing `newDemoZone`'s `spawnRoom`/`Move(spawn(...))` calls.
   **`spawn` is byte-for-byte the same.**

**Bare-engine boot:** with zero enabled packs (or no DB), `Load` returns an empty
`LoadedContent`; `newShard` builds zones with no rooms. A zone with no `startRoom` must
not panic on login — slice 1 audits `resolveRoom`/`join` for the empty-world case (today
they assume a start room exists). This is the PRINCIPLES.md guarantee made executable.

**Demo content becomes a pack:** the exact midgaard/darkwood rooms, exits, and the
torch/helmet/sword/chest prototypes from `world.go` are transcribed into the seed pack
file (`pack='stdlib'` or `pack='demo'`). The Go authoring helpers (`defineRoom`,
`defineTorch`, ...) are **deleted** once the loader produces identical prototypes.

---

## 4. The durability ladder

Three tiers (PERSISTENCE.md §6), each off the zone goroutine:

```
shard memory (authoritative, zone goroutine)
   │  ~10s, cheap                     ── crash window shrink
   ▼
Redis checkpoint   (key: telos:ckpt:char:<name>, value: {state_version, state JSON})
   │  ~60s / logout / significant events / drain   ── durable record
   ▼
Postgres characters.state  (state_version optimistic CAS)
```

### Write path (NEVER blocks the zone goroutine)

- The zone goroutine **produces** a snapshot: a pure function `dumpCharacter(s *session)
  → CharSnapshot{name, zone, room, stateVersion, stateJSON}` run on-goroutine (it only
  reads zone-owned state — same safety as `buildSnapshot`).
- The snapshot is handed to an **async saver** (a per-shard goroutine + buffered channel,
  mirroring `beginHandoff`'s spawned-goroutine pattern). The saver does the blocking
  Redis/Postgres I/O. On a `state_version` conflict it posts a `saveConflictMsg` back to
  the zone inbox so the zone can reconcile (re-read + re-emit) — it never mutates entity
  state off-goroutine.
- **Cadence:** a per-zone pulse callback (the `pulseScheduler` already exists, fires on
  the zone goroutine) emits a checkpoint snapshot every ~10s (Redis) and a Postgres flush
  every ~60s; plus immediate flush on `leave`/clean-quit, and on shard drain.

### `state_version` optimistic concurrency

`UPDATE characters SET state=$1, state_version=state_version+1, ... WHERE id=$2 AND
state_version=$3` via pgx `RETURNING`. Zero rows ⇒ stale writer (mis-fired handoff,
duplicated owner) — reject + reconcile. The snapshot carries `state_version` (it already
carries `applied_seq`; add the field to `PlayerSnapshot` proto + `CharSnapshot`) so it
survives a handoff and a crash-rehydrate.

### Read path (login)

- `server.go` already reads `PlayerEpoch` off-goroutine before posting `attachMsg`. Add
  a sibling read: `store.LoadCharacter(name)` (Postgres) with a Redis-checkpoint
  freshness check (use whichever has the higher `state_version`). Thread the loaded
  `CharSnapshot` into `attachMsg.loaded`.
- `attach`'s **fresh-login** branch stops building a blank entity; it calls
  `loadCharacter(s, snapshot)` to rehydrate components from `state` JSON, or, for a
  brand-new name, creates + immediately inserts a `characters` row (mint the UUID =
  `PersistID`). The player entity's `pid` is set here — **PersistID becomes real.**

### Crash-failover compatibility (PLACEMENT.md §5–6)

The key requirement: a **different** shard can rehydrate a player it never saw, from the
last checkpoint. This plan satisfies it because:

- Load is keyed by **character name → `characters` row / Redis checkpoint**, not by any
  shard-local handle. Any shard can run `LoadCharacter`.
- `state_version` + the directory `epoch` together fence a stale original owner: after a
  crash, the new owner loads the checkpoint, and its first save bumps `state_version`; a
  zombie original's save fails the CAS.
- The same `CharSnapshot`/`state` shape is used for handoff (in-flight) and checkpoint
  (at-rest), so the Phase 10 "new-owner-rehydrates" flow is just "Prepare from the
  checkpoint instead of from a live source." **Phase 4 builds the load-from-checkpoint
  primitive; Phase 10 wires the trigger.** No Phase 4 code assumes the original owner is
  alive.

---

## 5. Hot reload — `(kind, ref)` invalidation over NATS

NATS is in compose, unused today. Add a minimal `internal/contentbus` (NATS client):

- The writer (OLC/deploy/`make seed`) publishes `content.invalidate` with `{kind, ref,
  pack}` after a content write.
- Each shard subscribes; on a message it **reloads just the affected definition row(s)**
  from Postgres and swaps the `*Prototype` in `protoCache` for that `ref`.

**The COW correctness argument (the load-bearing reasoning):** a live instance is a
flyweight that *references* its prototype's immutable fields until it COWs. If we mutate
the existing `*Prototype` in place, we'd race every zone goroutine reading it (the cache
is currently publish-then-immutable). Therefore hot reload must **build a NEW
`*Prototype` and atomically replace the map entry**, not mutate in place. But the
`protoCache.protos` map is read locklessly by `spawn` on every zone goroutine — so the
swap needs synchronization. Options:

1. **Replace the whole cache pointer** (`atomic.Pointer[protoCache]`): cheap, but copies
   the map per reload.
2. **Per-shard reload event onto each zone inbox**: the swap happens on the zone
   goroutine (single-writer), no atomics — fits the actor model best.

**Recommendation: option 2** — a `reloadProtoMsg{kind, ref, proto}` posted to every
hosted zone; each zone applies it on its own goroutine. This keeps the
"only-the-zone-goroutine-touches-its-state" invariant intact.

**Live instances are NOT retroactively changed** by a reload (they already COW'd or still
alias the *old* prototype, which stays alive as long as an instance references it — Go
GC handles the old prototype). New spawns use the new prototype. This matches MUD
expectations (an existing mob keeps its stats; the next repop uses the edit) and avoids
corrupting in-flight COW deltas. Documented explicitly as a design choice, not a bug.

NATS unavailable ⇒ hot reload is simply disabled (boot-load still works); never fatal.

---

## 6. Slicing (ordered, independently committable)

| Slice | Scope | Done when | Tests that MUST stay green |
|-------|-------|-----------|----------------------------|
| **4.1 — DB infra + loader replaces `newDemoZone`** | goose + `db/migrations` (definition + state tables); `internal/store` (pgx pool, content queries); `internal/content` loader filling `protoCache`; seed the demo pack as rows; delete `newDemoZone` Go authoring; `make migrate`/`make seed`; bare-empty-boot audit | Engine boots, loads the pack, demo world is **byte-identical**; **and** boots empty with no DB (bare-engine) | ALL Phase 1–3 tests, esp. the look/move/targeting tests that assert exact room text; `handoff_test.go`; `zone` test |
| **4.2 — Character persistence + durability ladder** | `characters` row CRUD; `dumpCharacter`/`loadCharacter` DTO mapping; async saver goroutine + buffered channel; Redis checkpoint; `state_version` CAS + `saveConflictMsg`; login-load / logout-save; pulse-driven cadence; `PersistID` set; add `state_version` to `PlayerSnapshot` proto | A character (name + location, and any state the demo carries) **survives a process restart**: log in, move, quit, restart world, log in → same room/state | All 4.1 tests; handoff still works (snapshot now carries `state_version`) |
| **4.3 — Hot reload over NATS** | `internal/contentbus` (NATS pub/sub); `content.invalidate` `{kind,ref,pack}`; per-zone `reloadProtoMsg`; new-prototype swap on the zone goroutine | Editing a prototype row + publishing invalidation makes the **next spawn** use the new data, with **no live-instance corruption** and no restart | 4.1/4.2 tests; a new test: reload a prototype, assert old live instance unchanged, new spawn changed |
| **4.4 — Zone resets / repop** | `zone_resets` interpreter; spawn ephemeral mobs/items on zone boot + on the reset timer (pulse); `persistent`-flag gate to `object_instances` (path exists, demo uses none) | A zone boots its reset script (demo mobs/items appear), and re-running the timer repops without leaks or duplicate persistent rows | 4.1–4.3 tests; resets must not change the demo room *text* tests (spawn onto the market floor, not the start room, exactly as today's torches do) |

**Justification for this order:** 4.1 is the keystone risk (loader ↔ `newDemoZone`
swap) and unblocks everything; it is independently valuable (content lives in the DB) and
provable by the existing exact-output tests. 4.2 delivers the headline "done-when"
(survive a restart). 4.3 and 4.4 are additive and lower-risk; either could ship after
4.2 in any order, but 4.3 before 4.4 lets resets be hot-tuned. This matches the ROADMAP
spine with resets pulled to last (they depend on prototypes from 4.1 and the pulse
cadence touched in 4.2).

---

## 7. Risks & out-of-scope

### Integration risks (flagged)

1. **Loader replacing `newDemoZone` without touching `spawn`/the entity model (HIGHEST).**
   The whole flyweight/COW contract lives in `prototype.go` and is asserted by Phase 3
   tests. The loader must produce `*Prototype`s *identical* to what the Go helpers
   produce — same keywords, same component pointers, same exits map. Mitigation: keep
   `protoCache.define` as the sole construction entry point; write the loader to call it;
   diff against the old `newDemoZone` output before deleting it (a temporary parity test).
2. **Demo tests assert exact room output.** Moving content to the DB must not shift one
   byte of what `lookRoom` prints. The exit display order (`dirOrder`), room name, and
   long description all come from the row now. Mitigation: 4.1's parity test + the seed
   pack transcribed verbatim from `world.go` strings.
3. **Not blocking the zone goroutine on DB I/O.** A synchronous `LoadCharacter`/save in a
   command handler would stall the whole zone (and every player on it). Mitigation: the
   established pattern — read off-goroutine in `server.go`, save via the async saver,
   post results back as inbox messages. Audited in 4.2.
4. **Hot reload vs live COW instances.** Mutating a shared `*Prototype` in place is a
   cross-goroutine data race and would corrupt live deltas. Mitigation: build-new +
   swap-on-zone-goroutine (§5); old prototype lives until GC; live instances unchanged.
5. **Empty-world boot.** `resolveRoom`/`join`/`attach` assume a start room exists. With
   zero content they must degrade gracefully (reject login with a clean message, not
   panic). Audited in 4.1.
6. **Multi-shard migration races.** N world processes booting against one Postgres.
   Mitigation: goose advisory lock; auto-migrate opt-in; or run `make migrate` as a CI/
   deploy step and keep boot read-only.

### Explicitly OUT of scope for Phase 4

- **Abilities / attributes / resources / affects runtime** — Phase 5. (The `state` JSONB
  *carries* these subtrees opaquely; Phase 4 round-trips them, it does not interpret them.)
- **Combat / death / corpses** — Phase 6.
- **Lua / `self.state` script persistence** — Phase 7 (the `Scripted` component's opaque
  state is just another JSONB subtree to Phase 4).
- **GMCP** — Phase 9.
- **The placement controller / director / drain** — Phase 10. Phase 4 only builds the
  *load-from-checkpoint primitive* the crash path needs; it wires no trigger.
- **OAuth / real accounts** — Phase 14; **chargen / progression** — Phase 11 (`accounts` is a
  nullable-FK stub).
- **Cross-shard inventory in the snapshot** — already deferred (handoff.go); Phase 4's
  `state.inventory`/`equipment` round-trip is for *save/load*, not handoff transfer.

---

## 8. Decisions needed from the user before slice 1

1. **D1 migration tool** — confirm **goose** (vs atlas / golang-migrate).
2. **D2 DB access** — confirm **pgx v5 direct** (vs sqlc / ORM).
3. **D3 migration run model** — confirm `make migrate` + opt-in `TELOS_DB_AUTOMIGRATE`
   for dev, advisory-locked.
4. **D4a demo-pack file format** — **YAML** (recommended) vs JSON.
5. **Pack name for the demo content** — `stdlib` (the strippable reference pack per
   PRINCIPLES.md §4) vs `demo`. Recommend `stdlib`, so the bare-engine strip story is
   exercised by the demo itself.
6. **Exit FK scope** — confirm seeding both demo zones together so the
   `exits.to_room` FK holds across the market↔grove cross-zone exit (vs dropping the FK
   for cross-zone exits + relying on content-lint).
7. **`PlayerSnapshot` proto change** — OK to add a `state_version` field (and later a
   `state` blob) to the handoff proto? It's a `make proto` regen (`*.pb.go` gitignored).
