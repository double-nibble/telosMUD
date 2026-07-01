# Completed work — archive

This is the historical record of DELIVERED work: the per-phase plan docs (Phases 3–16, all shipped and
CI-green) followed by every resolved follow-up. It exists for posterity — the live list of outstanding
work is [REMAINING.md](REMAINING.md). The original per-phase `PHASEXX-PLAN.md` files and `FOLLOW-UPS.md`
were consolidated here and removed; their raw form is preserved in git history.

> Note: some phase plans contain forward-looking "deferred" callouts. Any of that work that is still
> outstanding is tracked in REMAINING.md — treat those in-plan notes as historical context, not the TODO list.

## Contents
- [Phase 3](#phase-3)
- [Phase 4](#phase-4)
- [Phase 5](#phase-5)
- [Phase 6](#phase-6)
- [Phase 7](#phase-7)
- [Phase 8](#phase-8)
- [Phase 9](#phase-9)
- [Phase 10](#phase-10)
- [Phase 11](#phase-11)
- [Phase 12](#phase-12)
- [Phase 13](#phase-13)
- [Phase 14](#phase-14)
- [Phase 15](#phase-15)
- [Phase 16](#phase-16)

---

<a id="phase-3"></a>

# Phase 3

_(archived from docs/PHASE3-PLAN.md)_

# Phase 3 implementation plan — Mudlib core

Refactor the Phase-1/2 toy world (`Room`/`player`/`Zone` structs) into the settled
entity/component model ([MUDLIB.md](MUDLIB.md)) **without breaking the Phase-2
exactly-once handoff machinery**. This is the engine-mechanism layer; content stays
hardcoded in `newDemoZone` but is reshaped as prototypes so Phase 4 (content DB) drops
in cleanly.

**Done when** (ROADMAP Phase 3): a player can `get`, `wield`, `put`, `wear` items and
others see the correct `act()` messages.

The invariant that governs every line below: **a zone is owned by exactly one
goroutine; entities are data, never goroutines; cross-goroutine interaction is
message-passing only** (MUDLIB §4). Nothing in this plan adds a lock to game logic or
shares an entity across two zone goroutines.

---

## 1. Target file layout in `internal/world`

| New/changed file | Holds | Replaces |
|---|---|---|
| `identity.go` | `ProtoRef` (string), `RuntimeID` (uint64), `PersistID` (UUID), the per-zone RuntimeID allocator | room/player string ids as the *primary* handle |
| `entity.go` | `Entity` struct (§2 of MUDLIB), constructor, `Move`, containment helpers (`contentsByKeyword`, visibility filter hook) | `Room.occupants`, `player.room` (location now lives on the entity tree) |
| `component.go` | `Component` interface, `Kind`, `componentSet`, generic `Get[T]/Must[T]/Has[T]/Add[T]` | n/a (new) |
| `components.go` | the concrete core component structs used this phase: `Room`, `Living`, `PlayerControlled`, `Container`, `Wearable`, `Weapon`, `Physical`. Only the fields a slice actually needs are populated; the rest are stubs carrying the documented shape. | the old `Room` struct fields; the in-world half of `player` |
| `session.go` | `session` struct: the Phase-2 connection/handoff state, lifted verbatim out of `player` (see §2) | the connection half of `player` |
| `zone.go` | `Zone` keeps `inbox`, `Run`, `handle`, all handoff message types and handlers. Its maps change to `rooms map[ProtoRef]*Entity` and `players map[string]*session` (keyed by character id). | current `rooms map[string]*Room`, `players map[string]*player` |
| `commands.go` → `parser.go` + `commands/*` | `Command`, `Context`, the command registry/abbreviation, Diku targeting, `act()`. Verbs become registered `Command`s. | the hardcoded `switch` in `dispatch` |
| `prototype.go` (slice 3) | `Prototype`, the per-shard prototype cache, `spawn(ProtoRef) *Entity` (flyweight + COW) | nothing yet; `newDemoZone` becomes prototype authoring |
| `pulse.go` (slice 4) | heartbeat/pulse scheduler driven off the zone loop | nothing yet |

**Mapping the three current structs onto entities:**

- `Room` → an `Entity` with a `Room` component, `location == nil` (its container is the
  zone), `contents` = occupants + ground items. The zone keeps a
  `rooms map[ProtoRef]*Entity` index for O(1) lookup by ref (replaces `z.rooms[id]`).
- `player` → **split** into an in-world `Entity` (with `Living` + `PlayerControlled`
  components) and a `session` struct (§2). The `PlayerControlled` component points at
  the session; the session points back at the entity.
- `Zone` → unchanged in role (still the single-writer actor). Its internal indices
  change type; its inbox, message set, and handoff handlers are preserved.

`Move(e, dest)` is the single containment primitive: detach from
`e.location.contents`, set `e.location = dest`, append to `dest.contents`. All
intra-zone moves are plain slice ops on the zone goroutine — lock-free, exactly as the
current `occupants` map ops are.

---

## 2. The Entity vs Session split (the load-bearing decision)

The current `player` conflates the **in-world object** with the **connection/session**.
Phase 2's exactly-once substrate lives entirely on the connection side and **must keep
working unchanged**. We split as follows.

- **`Entity`** carries in-world identity + containment + components. A player entity has
  a `Living` component (hp/position later) and a `PlayerControlled` component.
- **`PlayerControlled` component** is the *bridge*: it holds `session *session` (and,
  later, account/aliases/prompt cfg/GMCP supports per MUDLIB §3). It is how the zone
  goes entity → output and how a command finds the actor's connection.
- **`session`** is the Phase-2 connection state, moved out of `player` byte-for-byte. It
  also keeps a back-pointer `entity *Entity`. The zone's `players` map is keyed by
  character id → `*session` (so attach/detach/reap/forwarding lookups are unchanged);
  `session.entity` reaches the in-world object.

### Field-mapping table (every current `player` field)

| Current `player` field | Lands on | Rationale |
|---|---|---|
| `id` | `session.character` **and** entity identity (character id; `pid` once persistence lands) | id is the routing key (session) and the entity's durable handle |
| `name` | `Entity` (short/proper name) | in-world display data |
| `room` | **removed** — replaced by `Entity.location` (the room entity) | containment is uniform; no more room-id string on the player |
| `out` | `session.out` | pure connection state |
| `currentZone` | `session.currentZone` | per-connection routing pointer (server.go owns it) |
| `appliedSeq` | `session.appliedSeq` | dedup high-water; connection/replay state |
| `detached`, `attachGen` | `session` | link-death/re-attach machinery |
| `quitting` | `session` | clean-quit flag |
| `frozen` | `session` | handoff freeze; gates input/detach |
| `epoch` | `session` | ownership epoch |
| `pending`, `token` | `session` | destination-side handoff bind state |
| `send(frame)` | `session.send` | stamps `appliedSeq`, non-blocking enqueue |

**Why the session stays a distinct struct and not "just a component":** the Phase-2
handlers (`attach`, `detach`, `reap`, `prepare`, `redirect`, `transferIn`, forwarding)
operate on *connection lifecycle* before/independent of the in-world object existing
(a `pending` player has session state but is "not yet in the room"). Keeping `session`
as the value in `z.players` means **those handlers change only in how they reach the
entity** (`s.entity` instead of `p.room`/room maps), not in their control flow. This is
the smallest possible blast radius on the riskiest code.

**look/say/move under the split:**
- `lookRoom(s)` → resolve `s.entity.location` (the room entity), read its `Room`
  component for exits/desc, iterate `location.contents` for occupants/items.
- `say` → `s.entity.location.contents` for the broadcast set.
- `move` → `Move(s.entity, destRoomEntity)` for the local case; the cross-zone /
  cross-shard cases still hand off via the **session** (transferOut moves the entity +
  session together; the snapshot is built from the entity, see §4).

---

## 3. Identity & the room-id cleanup (resolves the tabled room-identity concern)

- **`ProtoRef`** becomes the stable content key for rooms: `midgaard:room:temple`,
  `midgaard:room:market`, `darkwood:room:grove`, etc. The room's display name
  ("The Temple Square") is data on the entity, decoupled from its ref. This is exactly
  the room-identity separation that was tabled — a room has a *stable id* (ProtoRef) and
  a *display name* that can change without breaking exits or saves.
- **Exit refs** move from the current `"zone:room"` string to a `ProtoRef`
  (`zone:room` is already ProtoRef-shaped; we formalize the type and parse). `parseRef`
  in `handoff.go` splits a ProtoRef into `(zoneID, roomKey)` for routing; this stays
  but operates on the typed ref. Cross-zone routing logic in `move`/`beginHandoff` is
  unchanged in behavior.
- **`RuntimeID`** is a per-zone `uint64` counter on the entity, used for live target
  references (slice 2 targeting, future aggro). Never persisted, never crosses a shard.
- **`PersistID`** (UUID) is *plumbed but unused* in Phase 3 — `pid *PersistID` on the
  entity, nil for everything. It becomes real in Phase 4.
- `newDemoZone` is rewritten to **author room prototypes** (slice 1: inline; slice 3:
  through the prototype cache) keyed by ProtoRef, so the Phase-4 loader replaces the
  function body without touching callers.

---

## 4. Integration with Phase 2 (the part most likely to break)

### buildSnapshot / PlayerSnapshot proto

- `buildSnapshot(p *player)` → `buildSnapshot(e *Entity)` (or `(s *session)`, reading
  `s.entity`). **Slice 1 keeps it behavior-preserving**: it serializes the *same minimal
  fields* — `character_id`, `name`, `applied_seq` — now sourced from the entity/session
  instead of `player`. `applied_seq` still comes from the session (freeze-state).
- **No proto change in slice 1.** The `PlayerSnapshot` proto **already** carries
  `inventory`, `equipment`, `affects`, `skills`, `flags`, `state_version` (fields 6–11,
  currently unset). Populating those is deferred until the corresponding components carry
  real state (inventory in slice 4; stats/affects in Phase 5). Note for later: when
  inventory crosses a shard, `buildSnapshot` will walk `Container`/inventory `contents`
  and `prepare` will rehydrate them — that is a slice-4-or-later change and may need the
  `common.v1.Item` shape to reference ProtoRef + instance delta (flag, don't build now).

### Intra-shard transferOut / transferIn / forwarding

- These move the player between zone goroutines. After the split they move **the
  `session` (which references the `Entity`)** — `transferInMsg{ s *session, room ProtoRef }`.
  The destination zone takes ownership of both session and entity together; only one
  zone goroutine ever touches them (the message hand-off through the inbox preserves
  this, exactly as today).
- `currentZone`, `appliedSeq`, and the `forwarding map[string]*Zone` keep their current
  semantics verbatim — they are session-keyed by character id, which is unchanged. The
  destination `Move`s the entity into the destination room entity instead of setting
  `p.room` + occupant map.
- **Single-writer check:** `transferOut` still removes the session from `z.players`,
  records forwarding, and posts the session to dest; the source goroutine touches it no
  more. The stress test (`TestIntraShardWalkStress`, runs under `-race`) is the standing
  guard and must stay green.

### Locator / handoff / parseRef

- `Locator` interface is untouched (it speaks `zoneID`/`shardID`/`playerID` strings).
- `parseRef` operates on the typed `ProtoRef` but returns the same `(zone, room)`; the
  one behavioral subtlety: room keys in exits become the room **ProtoRef key**, so
  `prepare`'s "place in `m.room`, else start room" and `move`'s local lookup index by
  ProtoRef. Keep `startRoom` as a `ProtoRef`.
- `handoffToken`, `prepare`, `redirect`, `abortPending`, `pendingExpire` are **logic-
  unchanged**; they only swap `*player` for `*session` and resolve the entity through it.

---

## 5. Slicing — ordered, independently committable

Each slice is behavior-preserving where possible, builds + tests green before commit,
and is reviewed (owning engineer + cross-cutting expert) per the standing rule.

| # | Slice | Scope | Done when | Tests that must stay green |
|---|---|---|---|---|
| **1** | **Entity + identity + containment + the session split** | `identity.go`, `entity.go`, `component.go`, `components.go` (`Room`, `Living`, `PlayerControlled`), `session.go`; rewrite `zone.go`/`commands.go` internals to entities; `newDemoZone` authors room entities keyed by ProtoRef. Verbs stay the hardcoded `switch` for now. | `look`/`say`/`move`/`who` behave identically; **all Phase-1/2 tests pass unchanged** including cross-shard + intra-shard handoff and exactly-once. | `slice_test`, `zone_test`, `multizone_test`, `resume_test`, `handoff_test` |
| **2** | **Command parser + Diku targeting + act()** | `parser.go` (`Command`, registry, abbreviation, active-table stack), `Context`, `TargetSpec`/`Scope`/`Resolve` (`2.sword`, `all.coin`, `isname`), `act()` perspective messaging + per-entity `Sink`/`Send`. Port `look/say/who/move/quit` onto registered commands; `act()` replaces ad-hoc broadcast strings. | abbreviation resolves (`n`→north, not `nuke`); `act()` produces actor/observer/can't-see variants; targeting parses the Diku grammar. Same external behavior for existing verbs. | all of slice 1's, plus new parser/targeting/act unit tests |
| **3** | **Prototypes & instancing (flyweight + COW)** | `prototype.go`: immutable `Prototype` cache per shard, `spawn(ProtoRef)`, instance-as-delta with copy-on-write on first mutation of a shared field. `newDemoZone` authors *prototypes*; rooms/mobs/items spawn from them. **Flags the §8 D1 fork — needs user sign-off before building (see §6).** | spawning N identical entities shares immutable fields; mutating one instance COWs only that field; a room of identical items is cheap. | all prior; new prototype/COW unit tests |
| **4** | **Heartbeat scheduler + containers/inventory → the Phase-3 milestone** | `pulse.go` (pulse/heartbeat off the zone loop, per-zone timers); `Container`/`Wearable`/`Weapon` components made functional; commands `get`, `drop`, `put`, `wear`, `wield`, `remove`, `inventory`, `equipment` with correct scopes + `act()`. | **the ROADMAP "done when": `get`/`wield`/`put`/`wear` work and others see the right `act()` messages.** | everything green; new container/equipment command tests |

**Justification for the spine order:** slice 1 is the highest-risk refactor (it touches
the handoff) but is behavior-preserving, so the existing test suite is a tight safety
net — do it first while the surface is small. Slices 2–4 are additive engine depth on a
stable base. Containers/inventory (slice 4) depend on both targeting (slice 2) and
instancing-shaped items (slice 3), so they land last and carry the milestone.

---

## 6. Risks & decisions to approve before slice 1

1. **§8 D1 — instancing model (flyweight+COW vs deep-copy).** MUDLIB §5 proposes
   flyweight + copy-on-write; §8 records deep-copy-on-spawn as the fork. This is the one
   decision that shapes `entity.go` field access. **Recommendation: build slice 1 with an
   accessor-mediated entity (getters that *could* fall through to a prototype) but store
   fields locally for now; commit to COW in slice 3.** Decoupling lets slice 1 ship
   without resolving the fork. **Need: user confirms flyweight+COW (D1) is the target, or
   picks deep-copy.**

2. **`Living`/`Room` direct-pointer hot-path (MUDLIB §3 escape hatch).** MUDLIB holds
   `Living` and `Room` as typed pointers on `Entity` in addition to the component map.
   **Recommendation: do it in slice 1** — it is cheap, it is exactly the two components
   look/say/move/combat touch every tick, and retrofitting later churns every call site.
   **Need: confirm we add `room *Room` and `living *Living` fields day one.**

3. **Proto churn in slice 1.** **Recommendation: none.** `PlayerSnapshot` already has the
   fields we'll eventually need; slice 1 keeps `buildSnapshot` minimal. The only proto
   touch in all of Phase 3 is *possibly* slice 4 if cross-shard inventory is required —
   and ROADMAP scopes the milestone to a single zone, so **inventory-across-handoff is
   explicitly deferred.** Flag if the user wants it sooner.

4. **Anything touching the handoff.** The session split is the only handoff-adjacent
   change, and it is mechanical (swap `*player`→`*session`, reach the entity through it).
   **Risk is concentrated in slice 1**; the existing handoff tests (`handoff_test.go`,
   `resume_test.go`, `multizone_test.go`) are the gate. No handoff *protocol* change.

5. **Test rewrites.** `zone_test.go` and `multizone_test.go` construct `&player{...}`
   directly (white-box). The split means they construct a `session` + `Entity`.
   **Recommendation: provide a `newTestPlayerEntity` helper** so the test diffs are
   small and the assertions unchanged. These are tests I own; I'll surface the diff.

**Deferred within Phase 3:** cross-shard inventory in the snapshot (proto stays as-is);
promoting components beyond `Living`/`Room` to fields (profiling-driven, MUDLIB §3);
visibility filter beyond a trivial stub (dark/invis need flags that arrive with content).

---

## 7. Explicitly OUT of scope for Phase 3

- **Persistence / content DB / zone resets / hot-reload** — Phase 4. Rooms/items stay
  hardcoded in `newDemoZone` (reshaped as prototypes). `PersistID` is plumbed but nil.
- **Attributes / resources / affects / ability framework** — Phase 5. `Living` carries
  the *shape* (hp/mp/mv, `Position`, `CoreStats`) but no derivation/modifier stack.
  `Affected`, `Skilled` components are not built.
- **Combat** — Phase 6. `Weapon`/`Armor` exist as data; no round resolution, no
  `PULSE_VIOLENCE`. The pulse scheduler (slice 4) is the substrate only.
- **Lua scripting** — Phase 7. `Scripted` is not built.
- **Comms over NATS, GMCP, orchestration, loot, economy** — Phases 8+. Socials/channels
  (MUDLIB §6) are *not* implemented; the parser leaves the post-command-table hook but
  no social/channel data.
- **Backpressure / flow control** — Phase 15. `session.send` keeps its non-blocking
  drop-on-full behavior.


---

<a id="phase-4"></a>

# Phase 4

_(archived from docs/PHASE4-PLAN.md)_

# Phase 4 — Persistence & content pipeline (implementation plan)

The "everything is content" backbone. After Phase 4 the bare engine boots **empty**,
loads a content **pack** from Postgres, runs the demo world identically, and a
**character + world state survive a process restart**. Nothing in the game's flavor is
Go code anymore: `newDemoZone` becomes seeded rows.

Status: **plan / pre-implementation.** No production code, schema, or tests changed yet.
This document exists to surface the tech-choice decisions before slice 1.

Source of truth: [PERSISTENCE.md](PERSISTENCE.md). Boundary: [ROADMAP.md](#roadmap-overview)
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


---

<a id="phase-5"></a>

# Phase 5

_(archived from docs/PHASE5-PLAN.md)_

# Phase 5 — Attributes, resources, affects & the ability framework — IMPLEMENTATION PLAN

Status: **proposal / planning** — needs the four decisions in §1 confirmed before slice 1.

The generic substrate ([ABILITIES.md](ABILITIES.md) §1–2) + the effect-op vocabulary (§3) + the
ability lifecycle (§4) + the `Affected` runtime (§5) + tag CC (§6) + the automatic PvP gate
(§7, D2). **Done when** (ROADMAP Phase 5): a data-defined `fireball` casts, costs mana, deals
typed damage, and applies a content-defined affect — *all without engine code changes*.

This phase builds the **substrate** combat (Phase 6) consumes; it does **not** build combat
round resolution. Lua `on_resolve_lua` is Phase 7 — Phase 5 ships the **declarative** op-list and
**reserves** the Lua hook. GMCP is Phase 9.

---

## 0. Where Phase 5 sits on the existing code

| Existing (Phase 1–4) | Phase 5 change |
|---|---|
| `Living` (components.go) carries `hp/maxHP/mp/...` as **stub int fields** | Becomes **real**: vitals are content-defined *resources*; `CoreStats` become content-defined *attributes* with a modifier stack. |
| `Affected` / `Skilled` component **kinds reserved** (component.go, never instantiated) | `Affected` becomes a real component the runtime ticks; `Skilled` carries known abilities + prof. |
| `pulse.go` per-zone scheduler (single-writer, `every`/`after`, resolve-by-id contract) | Affect **ticks** + cast-time/cooldown timers hang off this — the contract in the `pulseFunc` doc comment is load-bearing. |
| `content` pkg: Pack → **Zones** → rooms/protos/resets; `Source.LoadPacks` | New **pack-scoped, zone-independent** definition kinds (attributes/resources/damage-types/affects/abilities). This is a *structural* extension — today every DTO hangs under a `ZoneDTO`. |
| `content_map.go` DTO→component mapper; `build.go defineContent` | Extends to register the new global defs into per-shard registries (not the proto cache). |
| `reset.go` data-op interpreter (switch on `r.Op`, single-writer, no-I/O) | The **template** for the new effect-op interpreter — same switch-on-kind, same single-writer/no-blocking discipline. |
| `character.go dump/loadCharacter`; `StateJSON` reserves `attributes/resources/affects/skills/flags` (round-tripped **opaquely** in Phase 4) | Those subtrees become **interpreted**: current resources + active affects (remaining dur/stacks) round-trip and re-attach. |
| migrations `00001_definition_tables.sql` | New migration adds the 5 (±2) global definition tables. |

The riskiest *structural* point is that today **all content is zone-scoped**; attributes/resources/
damage-types/affects/abilities are **global to a pack** (a `fireball` is not "owned" by Midgaard).
See §2.

---

## 1. Tech / design decisions (confirm before slice 1)

| # | Decision | Recommendation | Trade-off |
|---|----------|----------------|-----------|
| **P5-D1** | Derived-attribute **formula language** | A small **declarative postfix/AST expression** evaluated by an engine interpreter (no Lua). Operators `+ - * / min max`, refs to other attrs (`con`, `level`), literals. Stored as JSON in `default_base`. | Keeps formulas content-defined *now* (Lua is Phase 7). Cost: we build a tiny evaluator + must avoid cycles. Alt: defer all derived attrs to Phase 7 Lua — rejected (`max_hp` is needed for the milestone resource caps). |
| **P5-D2** | v1 **effect-op set** | Implement: `deal_damage`, `heal`, `restore`, `modify_resource`, `apply_affect`, `remove_affect`, `dispel`, `act`/`send`, and flow `if`/`chance`. Defer: `drain`, `move`/`teleport`/`pull`/`push`/`recall`, all `scan`/`reveal`/`detect`/`identify`, `spawn`/`summon`/`transform`, `gmcp`, `for_each`/`delay`. | The deferred set is exactly what combat (6), perception, world-manip, and GMCP (9) own; the v1 set is the minimum for "costs mana, typed damage, applies affect". Each op is a *registered handler*, so deferred ops are later additions, not rework. |
| **P5-D3** | Affect **stacking model** | Support all four §5 modes — `refresh` (default), `stack` (count up to `max_stacks`, magnitude scales), `extend` (sum durations), `ignore` (first wins) — as a `stacking` enum on the affect def. Keyed by `(affectRef, source)` or `(affectRef)` per a `stack_scope` field (default: per-source). | Poison needs `stack`; haste needs `refresh`. Implementing all four now is cheap (one switch in `apply`) and avoids a v2 migration of saved affects. Alt: ship only `refresh`+`stack` — rejected, the data field is the same effort. |
| **P5-D4** | PvP-gate **shape** (security boundary) | `pvp_allowed(actor, target) bool` is an **engine function** backed by a **content-defined policy** (a small declarative ruleset: consent flags, safe-room flag, level gap, arena zone flag). Enforced at **two** choke points: lifecycle step 4 (whole-ability) **and** inside *every harmful effect op* (a shared `guardHarmful` the op handlers funnel through). Mob targets ⇒ no-op. | Defense-in-depth, can't-forget. The in-op check is what makes even a future Lua `on_resolve` unable to bypass. Cost: every harmful op must route through the guard — enforced by making the damage/debuff path a single shared function, not copy-paste. **Security-auditor must review.** |

### 1.1 Derivation model (P5-D1)

`attr(e, "strength")` resolves through the stack (ABILITIES §1):

```
base            (literal, or a derived FORMULA evaluated against other attrs)
  → + flat mods   (Σ of additive modifiers from gear + active affects)
  → × multipliers  (Π of multiplicative modifiers)
  → computed       (clamped to the attr's min/max if declared)
```

- **base** comes from the attribute_def `default_base`: either a literal (`{"lit": 10}`) or a
  formula (`{"expr": ["*", ["attr","con"], 10]}` → `con*10`). Per-entity overrides
  (race/class/level/point-buy) live in the entity's `attributes` state and replace the default base.
- **mods** come from two sources: equipped gear (an `Armor`/affix component — coarse this phase)
  and **active affects** (`modifiers = [{attr, op:add|mul, value}]`, §5). The `Affected` runtime
  is the writer of the affect-sourced modifier list.
- **Formula form** (recommended): a nested-array prefix AST, JSON-serializable, no Lua:
  `["+", ["*", ["attr","con"], 10], ["*", ["attr","level"], 5]]` = `con*10 + level*5`.
  Allowed heads: `+ - * / min max clamp`, `["attr", name]`, `["lit", n]`. A reference to an attr
  pulls *its* resolved value (recursive `attr()`), so derived-of-derived works.
- **Cycle / cost guard:** derivation is **memoized per entity** with a dirty-bit invalidated when
  any base/mod/affect changes (`attr()` is hot). Cycle detection = a per-resolution visited set
  (a self/mutual reference errors at load via content-lint, and defensively at eval). Caching
  detail is in §5 (Risks) — it is the main perf concern.

### 1.2 Resources

A `resource_def` is `{name, max_attr (derived attr ref, e.g. "max_hp"), regen (formula or
per-tick literal), vital (bool), depleted_threshold, on_depleted (op-list)}`. The engine holds
`current`; `max` is a derived attribute (so gear/affects that raise `max_hp` flow through §1.1).
`vital` + `on_depleted` is how "hp at 0 = death" is **content**, not Go (§5 ABILITIES). Regen ticks
on the pulse scheduler alongside affect ticks.

### 1.3 Effect-op interpreter (mirrors reset.go)

`on_resolve` is a JSON op-list. The interpreter is a `switch` over `op.Kind` exactly like
`reset.go applyReset` — single-writer (runs in step 8 on the zone goroutine), never blocks, unknown
op logged+skipped (content-lint is the real gate). Each op is a registered handler
`func(*effectCtx, opArgs) error`. The `effectCtx` carries actor, target(s), source, magnitude, and
the **shared mitigation + PvP guard** helpers. Adding an op = registering a handler — no lifecycle
change. The `on_resolve_lua` column is **read but not executed** this phase (reserved for Phase 7).

### 1.4 Affected runtime (P5-D3)

- **Attach:** `apply_affect` resolves the `affect_def`, runs the stacking rule against any existing
  instance keyed by `(ref[, source])`, sets `remaining = duration` (literal or formula), records
  `magnitude`/`stacks`/`source`, fires `OnApplyAffect`, and **dirties the target's attribute cache**
  (so its `modifiers` take effect on the next `attr()`).
- **Tick:** the runtime registers ONE pulse callback per affected entity (not per affect) via
  `pulse.every`, following the `pulseFunc` resolve-by-id-or-cancel contract verbatim — re-resolve
  the entity by id each tick, stop if absent/frozen. The callback decrements `remaining`, runs each
  affect's `tick.on_tick` op-list at its interval (a DoT is just `tick → deal_damage`), and expires
  any at 0 (`OnAffectExpire`, clear modifiers, re-dirty cache). Resource regen rides the same
  callback.
- **Modifiers feed derivation:** the `Affected` component exposes `flatMods(attr)`/`mulMods(attr)`
  that §1.1 sums/multiplies. Affect changes dirty the attr cache; that is the whole coupling.
- **Stacking:** see P5-D3.

### 1.5 Tag-based CC (§6)

Tags are **strings** (open set, no enum — pillar). An ability def carries `tags: ["cast","verbal",
"fire"]`; an affect def carries `prevents: ["move","verbal"]`. At lifecycle **step 3** the engine
asks: *does any active affect on the actor `prevents` any tag this ability carries?* If so, blocked
with the affect's block message. `requires.not_prevented = "cast"` uses the same query. The engine
never names a CC type. Represent active prevents as a small per-entity set the `Affected` runtime
maintains (union of active affects' `prevents`), so the step-3 check is O(tags).

### 1.6 Ability lifecycle (§4) on the existing machinery

The 10 steps (§4) map onto the existing command/pulse spine:
- **Step 1 invoke**: a `command`-invocation ability **registers a `Command`** (MUDLIB §6 command
  table) whose `Run` enters the lifecycle. `proc`/`passive` invoke from events (events are mostly
  Phase 6/7; reserve the hooks).
- **Steps 2–5** (targets / requires / **gate** / costs): synchronous, in the command handler, on the
  zone goroutine. Targeting reuses `Zone.Resolve` + scopes (MUDLIB §7).
- **Step 6 cast_time**: a `pulse.after` lockout (interruptible per a flag); on interrupt → refund.
  `cast_time = 0` (the fireball milestone) skips straight to commit.
- **Step 7 commit**: pay costs, impose `lag` via `ctx.Lag` (existing WAIT_STATE), arm `cooldown` via
  `pulse.after`.
- **Step 8 on_resolve**: the effect-op interpreter (§1.3); every harmful op re-checks the gate +
  routes the shared mitigation pipeline.
- **Steps 9–10 emit/events**: `ctx.Act` messages now; GMCP deltas (Phase 9) and the event bus
  (Phase 6/7) are **reserved hooks** — fire-no-op or log this phase.

The **shared mitigation pipeline** (`deal_damage` → resist/vuln/immune matrix from
`damage_type_defs` → soak) is a single function so "a spell and a sword obey the same rules"; Phase 6
attaches weapon swings to the same entry point.

---

## 2. Schema + loader integration

### 2.1 New definition tables (migration `00003_ability_tables.sql`)

Pack-scoped, **zone-independent** (no `zone_ref`), same ref/pack + columns + JSONB-tail pattern:

```sql
CREATE TABLE attribute_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  display_name TEXT NOT NULL,
  value_kind   TEXT NOT NULL,        -- 'int' | 'float' | 'derived'
  default_base JSONB,                -- literal or formula AST (§1.1)
  body JSONB NOT NULL DEFAULT '{}'   -- min/max, clamp, display
);
CREATE TABLE resource_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  display_name TEXT NOT NULL,
  max_attr TEXT,                     -- derived-attr ref that caps it (e.g. "max_hp")
  vital BOOLEAN NOT NULL DEFAULT false,
  body JSONB NOT NULL DEFAULT '{}'   -- regen, depleted_threshold, on_depleted op-list
);
CREATE TABLE damage_type_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  display_name TEXT NOT NULL,
  body JSONB NOT NULL DEFAULT '{}'   -- resist/vuln/immune matrix, color
);
CREATE TABLE affect_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  name TEXT NOT NULL,
  category TEXT,                     -- dispel/cure targeting
  stacking TEXT NOT NULL DEFAULT 'refresh',
  max_stacks INT NOT NULL DEFAULT 1,
  dispellable BOOLEAN NOT NULL DEFAULT true,
  body JSONB NOT NULL DEFAULT '{}'   -- duration, modifiers, prevents[], tick{}, on_apply/expire, resist
);
CREATE TABLE ability_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  name TEXT NOT NULL,
  invocation TEXT NOT NULL,          -- 'command' | 'proc' | 'passive'
  targeting JSONB NOT NULL,          -- mode/scope/range/disposition
  tags TEXT[] NOT NULL DEFAULT '{}', -- §6 CC tags
  requires JSONB NOT NULL DEFAULT '{}',
  costs    JSONB NOT NULL DEFAULT '{}',
  cast_time INT NOT NULL DEFAULT 0, lag INT NOT NULL DEFAULT 0, cooldown INT NOT NULL DEFAULT 0,
  on_resolve     JSONB,             -- declarative op-list (this phase)
  on_resolve_lua TEXT,              -- RESERVED, read-not-run (Phase 7)
  messages JSONB
);
```

**`class_defs` / `race_defs`: DEFER.** They are PERSISTENCE §1's named tables but the milestone
needs neither (a character can carry attributes/skills directly). Real class/race progression
(point-buy, level-up grants) is a meaningful slice of its own; reserve the tables, build them when
chargen/progression lands. *Confirm this deferral.*

### 2.2 Content pipeline + mapper extension

- **DTO:** add `AttributeDTO`, `ResourceDTO`, `DamageTypeDTO`, `AffectDTO`, `AbilityDTO` to
  `internal/content/dto.go`. **Structural choice:** these are **pack-global**, not under a `ZoneDTO`.
  Recommendation: add them to the top-level `Pack` (`Pack.Attributes`, `Pack.Affects`, …) and extend
  `LoadedContent` with global slices/maps (`lc.Attributes`, `lc.Abilities`, …) alongside `Zones`.
  This is the cleanest fit for "global to a pack" and keeps `Source.LoadPacks` returning whole packs.
- **Store:** extend `internal/store/content.go LoadPacks` to also `SELECT` the five tables
  `WHERE pack = ANY($1)` and fill the new `Pack` fields; add single-row loaders for hot-reload
  (mirroring `loadRoomDefinition`).
- **Mapper / registries:** the world side gains **per-shard registries** (not the proto cache —
  these aren't prototypes): `attrRegistry`, `resourceRegistry`, `damageTypeRegistry`,
  `affectRegistry`, `abilityRegistry`, each an `atomic.Pointer`-swapped table like `protoCache`
  (PERSISTENCE §8 reload semantics). `build.go defineContent` registers them at boot; ability
  command-invocations register into the command table. `content_map.go` gains the DTO→runtime
  builders (formula AST parse, op-list parse).

### 2.3 Strippable stdlib pack (D3 / §10)

Extend the embedded demo pack (`internal/content/packs/demo.yaml`) — or a new `stdlib` pack — with
sample: attributes (`strength`, `intellect`, `constitution`, `level`, derived `max_hp`, `max_mana`),
resources (`hp`, `mana` — `hp` vital with `on_depleted`), damage types (`fire`, `slash` with a small
resist matrix), affects (`poison` — the §5 example, a DoT + `-2 strength`; `haste` — a `refresh`
buff), and abilities (`fireball` — costs 30 mana, `deal_damage fire`, `apply_affect poison`). The
**bare-engine invariant** holds: boot with no pack ⇒ zero attrs/resources/abilities, server still
runs (the empty-boot test from Phase 4 must stay green and gain an assertion that combat ops are
simply unavailable).

---

## 3. Persistence integration

`StateJSON` (character.go) already reserves `attributes/resources/affects/skills/flags`
(Phase 4 round-trips them opaquely). Phase 5 makes them **interpreted**:

- **dumpCharacter** adds: `attributes` (per-entity base overrides only — derived values are
  recomputed, never stored), `resources` (`{hp:{cur:N}, mana:{cur:N}}` — current only; max is
  derived), `affects` (`[{id, dur(remaining), mag, stacks, source}]` — the §3 PERSISTENCE shape),
  `skills` (known abilities + prof), `flags` (incl. `pvp_opt_in`).
- **loadCharacter** re-applies: install attribute bases, set resource currents (clamped to derived
  max), and **re-attach each affect** via the runtime's attach path with `remaining` from the
  snapshot — which **re-registers the per-entity tick callback** and re-seeds the prevents set and
  modifier contributions. Affects must not double-tick or reset duration on load.
- **Affect durations across save/restore AND handoff:** durations are stored as **remaining pulses**
  (already pulse-denominated). On a handoff (cross-shard, fat snapshot — not via the store), the
  same `affects` subtree rides the snapshot; the destination zone re-attaches them exactly like a
  load. Decrementing only happens on the owning zone's pulse, so a frozen/in-flight entity does not
  tick (the resolve-by-id/skip-frozen contract) — durations are conserved across the seam. Surface
  to persistence-owner: confirm the snapshot/Redis-checkpoint/handoff all share this one subtree.

---

## 4. Slicing (ordered, independently committable)

The natural spine is **substrate → affected runtime → ability lifecycle/gate**. Each slice is a
commit with the prior phase's tests green.

| Slice | Scope | Done when | Tests that stay green / added |
|-------|-------|-----------|-------------------------------|
| **5.1 — Generic substrate** | Content-defined attribute/resource/damage-type/flag defs + tables + DTO/loader/registry wiring; the **modifier stack + derivation** (formula evaluator, memoized cache, invalidation); `Living` made real (vitals = resources, stats = attributes) without breaking call sites. No abilities yet. | A demo character loads with content-defined `strength`/`max_hp`/`hp`; `attr(e,"max_hp")` evaluates a formula through the stack; bare-engine boot (no pack) still runs. | All Phase 1–4 green (esp. handoff/character/zone tests as `Living` fields change behind accessors). New: derivation unit tests, formula-eval tests, empty-boot assertion. |
| **5.2 — Affected runtime** | Affects as content (`affect_defs` + DTO/loader); the real `Affected` component; attach / stacking (4 modes) / duration / **tick on the pulse scheduler** / expire; modifiers feed §1.1 derivation; **tag-based CC** (prevents set + step-3-style query, even before full lifecycle); persistence round-trip of active affects. | `apply` a `poison` affect → it ticks damage on the pulse, decrements, expires; a `-2 strength` affect changes `attr(e,"strength")`; a `prevents:["move"]` affect blocks a tagged action; an affect survives save/load with correct remaining duration. | 5.1 green. New: affect attach/stack/tick/expire tests, derivation-with-modifiers tests, CC query test, affect persistence round-trip test, **pulse resolve-by-id-or-cancel test for affect ticks**. |
| **5.3 — Ability lifecycle + effect ops + PvP gate (the milestone)** | The 10-step lifecycle on the command/pulse spine; the effect-op interpreter (§1.3, P5-D2 op set) modeled on reset.go; the **automatic PvP/hostility gate** at step 4 + inside every harmful op (P5-D4); the shared mitigation pipeline; `ability_defs` + DTO/loader; `fireball` in the stdlib pack. | **`fireball` casts**: a granted command, costs 30 mana (reserved/paid), resolves a typed `fire` damage op through resist/soak, and `apply_affect poison` — all from data, **zero engine changes** to add it. PvP gate blocks a harmful op vs a non-consenting player at *both* choke points. | 5.1+5.2 green. New: lifecycle-step tests, each op-handler test, **PvP-gate defense-in-depth tests (lifecycle AND in-op, incl. a hostile op that tries to skip step 4)**, mitigation-pipeline test, end-to-end fireball test. |

**Adjustment / justification.** I keep the three-slice spine from the brief but flag two things:
(a) tag-CC's *query* lands in 5.2 (it is an `Affected` capability) while its *enforcement at
lifecycle step 3* lands in 5.3 (no lifecycle exists until then) — the query is testable standalone in
5.2; (b) if 5.3 proves large, split off a **5.3a costs/timing/targeting lifecycle** (no harmful ops)
from **5.3b effect ops + PvP gate + fireball** so the security-sensitive gate lands as its own
reviewable commit. Recommend planning for the split, executing as one if it stays small.

---

## 5. Risks & out-of-scope

### Explicitly OUT of scope
- **Combat round resolution = Phase 6.** Phase 5 builds the substrate it uses (the shared mitigation
  pipeline, `deal_damage`, lag/cooldown, vitals/resources, `on_depleted`/death-as-content). No
  `PULSE_VIOLENCE`, attacks-per-round, avoidance ladder, threat, or corpses here.
- **Lua `on_resolve_lua` / affect-hook Lua = Phase 7.** Ship the declarative op-list; **reserve** the
  `on_resolve_lua` column and the `on_apply`/`on_tick`/`on_expire` Lua hooks (read-not-run, the AST
  form covers the milestone).
- **GMCP vitals/affliction deltas = Phase 9.** Emit step-9 messages via `act`; reserve the GMCP
  emit points.
- **class_defs / race_defs progression = deferred** (reserve tables; §2.1) — confirm.
- **Event bus (`OnHit`/`OnAffectTick`/proc dispatch) = mostly Phase 6/7.** Reserve the hook points so
  passives/procs wire in later without lifecycle change.

### Integration risks
1. **Don't block the zone goroutine.** Every lifecycle step, op, affect tick, and regen runs
   single-writer in the zone loop; any I/O (none expected this phase) must go async + post back,
   exactly as reset.go / the saver already do.
2. **Affect ticks must obey the pulse contract.** The `pulseFunc` doc comment (pulse.go) is binding:
   re-resolve the player by id each tick, **stop if absent or `s.frozen`**, never close over a stale
   `*Entity`. This is the first real registrant of that contract (the comment anticipated Phase 5);
   **distributed-systems-architect must review** the affect-tick registration + the
   duration-across-handoff conservation.
3. **Derivation performance — `attr()` is hot.** Memoize per entity with a dirty-bit invalidated on
   base/mod/affect change; a naive recompute-every-call through a formula tree will dominate combat.
   Cycle detection on derived-of-derived formulas (visited set + content-lint).
4. **The PvP gate must be truly can't-bypass.** Funnel *every* harmful op through one shared
   `guardHarmful` so a new op author can't forget it, and keep the lifecycle step-4 check as the
   outer layer (defense-in-depth). **Security-auditor must review** — this is the §7/D2 security
   boundary, and the in-op layer is what survives a future Lua escape hatch.
5. **Keeping Phase 1–4 green as `Living`/`Affected` become real.** `Living.hp/maxHP/...` change from
   raw fields to resource/derived-attr lookups; route reads through accessors so handoff/character/
   zone/COW tests don't churn. `cloneComponent` must learn the new `Affected` shape (its
   reference-typed slices/maps) or the COW `default` panic fires — handle it explicitly.

### Cross-cutting reviewers
- **security-auditor:** slice **5.3** (the PvP/hostility gate — both choke points, the in-op guard,
  the "Lua can't bypass" property even though Lua is Phase 7).
- **distributed-systems-architect:** slices **5.2 & 5.3** (affect ticks + cast/cooldown timers on the
  pulse scheduler; the resolve-by-id/skip-frozen contract; affect-duration conservation across a
  handoff and across the durability ladder).
- **persistence-owner (cross-component, surface-don't-edit):** the `StateJSON` interpreted subtrees +
  the shared affects/resources subtree across snapshot/checkpoint/handoff (§3).


---

<a id="phase-6"></a>

# Phase 6

_(archived from docs/PHASE6-PLAN.md)_

# Phase 6 — Combat (+ the check primitive, the event bus, AoE & room affects) — IMPLEMENTATION PLAN

Status: **proposal / planning** — the six gap-analysis forks are already settled (see
[GAME-SYSTEMS-GAP-ANALYSIS.md](#gap-analysis) §18); the §1 decisions below are
the *combat-specific consequences* + a few new ones, to confirm before slice 6.1.

Combat is the round-based, ROM-derived, layered-avoidance + soak model ([COMBAT.md](COMBAT.md)) —
but the gap analysis ([GAME-SYSTEMS-GAP-ANALYSIS.md](#gap-analysis) §2, §9, §17)
reframes Phase 6 as **the phase that builds the primitives combat is assembled *from*** before it
builds the fight loop. **Done when** (ROADMAP Phase 6): you fight a mob through the full pipeline
(to-hit check → miss/dodge/parry/block → soak), a fireball's save halves its damage across everyone
in the room, a rage bar builds on hit via an `OnHit` handler, and you kill the mob and loot its
corpse — **all from content, no engine changes**.

This phase builds the **check primitive [G2]**, the **in-zone event bus [G3]**, **AoE [G12]**,
**room-scoped affects [G13]**, **cooldown completion [G8]**, and **reaction checkpoints [G9]** on
top of the Phase 5 substrate (the shared mitigation pipeline, `dealDamage`, resources/affects, the
pulse scheduler, the PvP gate). It does **not** build: Lua handlers / result-altering reactions /
concentration (Phase 7), the cross-zone scoped + durable event bus (Phase 10), GMCP combat deltas
(Phase 9), or progression/chargen (Phase 11).

---

## 0. Where Phase 6 sits on the existing code

| Existing (Phase 3–5) | Phase 6 change |
|---|---|
| `effect_op.go` flow ops `if`/`chance` recurse into `runOps`; each op is a registered handler | The **`check` flow op [G2]** is the next flow op — same shape (resolve → pick a branch → recurse into `runOps`), but classifies a rolled total into an **ordered band list** instead of a bool. |
| `effect_op_handlers.go rollDice(c, num, size)` + `effectCtx.rng` (seeded per zone) | Extended to parse **content dice notation** beyond `NdS`: keep-high/low (`2d20kh1`), Fudge (`4dF`), pool-count-successes (`5d6>4`). The RNG seam is unchanged. |
| `formula.go` heads `+ - * / min max clamp lit attr` (prefix AST) | Add `floor`/`ceil`/`round`/`mod` + conditional heads [G1] (combat needs exact integer to-hit/soak); add `$actor`/`$target`/`$source` **scoped refs** for check `bonus`/`vs` [G2c]. |
| `ability.go` step 10 fires the **reserved** `OnAbilityResolved`/`OnHit` events as *log-only* (Phase 6/7 reserved) | Those reservations become a **real in-zone event dispatch [G3]** — synchronous, single-writer, content-subscribable. |
| `Affected` runtime (`affected.go`/`affect_runtime.go`) ticks per-entity on the pulse, resolve-by-id contract | Gains **room-scoped affects [G13]** (an affect attached to the *room* entity, ticking over occupants) and is the substrate AoE/DoT route through. |
| `pulse.go` per-zone scheduler (`every`/`after`, resolve-by-id-or-cancel) | Hosts **`PULSE_VIOLENCE`** (a fixed multiple of base pulse) — the round driver; plus cooldown timers [G8]. The `pulseFunc` contract is binding for the round loop. |
| `pvp.go guardHarmful` + `affectIsDetrimental` (the single harm funnel, Phase 5) | **Every new harm vector** — swing damage, AoE per-target, event-handler ops, reaction ops — routes through the *same* `guardHarmful`. No second gate. **security-auditor boundary.** |
| `Living`/resources (Phase 5): vitals are content resources, `vital` flag, `on_depleted` op-list | Combat reads the **`vital` resource by flag**, never a hardcoded `hp`; death = `on_depleted` firing (content) → the engine's corpse/threat machinery. |
| `commands.go` command table; abilities register commands (Phase 5) | Combat skills are abilities-as-commands with `lag` (WAIT_STATE, exists) + the new **cooldown gate [G8]**; `kill`/`flee`/`assist`/`consider` are engine commands driving the `Fighting` state. |
| Cross-shard handoff (Phase 2/4): fat snapshot, epoch CAS, freeze-during-transfer | Combat/threat is **transient** (dropped on quit/handoff); cooldowns + active affects **persist**. AoE "room + adjacent" must respect that adjacent rooms may live on **another shard** (message-pass, never reach across — see §5). |

The riskiest *structural* points: (a) the **event bus re-entrancy** (an `OnHit` handler that deals
damage that fires `OnDamageTaken` …) needs a depth/loop guard and a clear single-writer story; and
(b) the **Phase-6-vs-Phase-10 bus boundary** — Phase 6 is the *in-zone, synchronous, transient*
dispatch; Phase 10 ([WORLD-EVENTS.md](WORLD-EVENTS.md)) adds *cross-zone scopes + durable JetStream*.
See §1.2 and §5.

---

## 1. Tech / design decisions (confirm before slice 6.1)

Settled-by-gap-analysis decisions are marked **(settled)** — restated here as the binding input;
the rest are new combat-specific calls.

| # | Decision | Recommendation | Trade-off |
|---|----------|----------------|-----------|
| **P6-D1** | **Check primitive home & arity** (settled — gap §18.2, §18.4) | A `check` **flow op** in the effect-op interpreter (invokable from exits/objects/affect-ticks/abilities), classifying a content dice roll into an **ordered band list** (binary = the 2-band case). Engine stays ignorant of dice *shape* AND outcome *arity*. | Builds the most load-bearing abstraction first, standalone-testable (a climb check needn't wait for the fight loop). Cost: a dice-notation parser + band classifier + dual-scope formula context up front. Already the chosen ordering. |
| **P6-D2** | **Band classifier model** (new) | Bands are an **ordered list of `(test → label, ops)`** where `test` is a **threshold** (`≥N` against the rolled total, or `vs` a target's roll for contested) OR a **margin/degree** (`total − dc ≥ k`); first matching band (top-down) wins; the last band is the default. Supports binary `{success,failure}`, half-on-save `{success→half, failure→full}`, PbtA `{≥10, 7..9, ≤6}`, BRP degrees, Blades pools (count successes → band). | The union abstraction across the whole catalog (gap §2 design note). Cost: the test grammar must express both threshold and margin without becoming a mini-language — keep it to `{min, max, margin_min}` per band. Alt: hardcode bands per dice kind — rejected (breaks the pillar). |
| **P6-D3** | **In-zone event bus scope** (new — the keystone) | Phase 6 ships a **synchronous, single-writer, in-zone** dispatch: content subscribes op-lists to event *kinds* (`OnHit`/`OnDamageTaken`/`OnKill`/`OnAbilityResolved`/`OnCheck`/`OnLeaveRoom`/`OnRest`) via `on_<event>` fields on `resource_def`/`affect_def`/`ability_def`/items; handlers run **inline** on the zone goroutine at the fire point, **gated by a recursion-depth budget**. **No NATS, no durability, no cross-zone** — that is Phase 10 ([WORLD-EVENTS.md](WORLD-EVENTS.md)). | This is binding-constraint #3 (events-as-glue) and the highest-leverage unbuilt thing (gap §19). Synchronous-in-zone matches the actor model and gives content a consistent single-threaded view. Cost: re-entrancy/ordering discipline + the depth guard (§5). **distributed-systems-architect must review** the boundary with the durable bus. |
| **P6-D4** | **Reaction interrupt depth (v1)** (settled — gap §18.6) | The swing/cast pipeline exposes **named interruptible checkpoints** (`before_swing`, `on_to_hit`, `before_damage`, `on_damaged`, `on_leave_room`, `before_cast_commit`) that fire events. v1 reactions may **fire a granted op-list + consume a per-round reaction resource**; they may **NOT alter the in-flight result** (cancel a cast, add to AC after the roll). Result-altering = Lua (Phase 7). | Opportunity attack (a granted attack on `OnLeaveRoom`) and Hellish-Rebuke-style (an op-list on `OnDamaged`) work declaratively now; Counterspell/Shield wait for the Lua hatch. Cost: the checkpoint set must be designed into the pipeline *now* even though the alter-path lands later. |
| **P6-D5** | **Roll visibility** (settled — gap §18.1) | **Hidden by default**, opt-in `show` (and `summary`), overridable **per pack / per ability / per check**. Phase 6 emits via `act()`/`send` text (`"You scramble up the wall."` vs `"You rolled 14+6 vs 15 — success."`); the **GMCP** structured emit is a reserved hook (Phase 9). | A 5e pack shows the math; a WoW/IRE pack hides it. Cost: a visibility resolution order (check → ability → pack → engine default) + two render paths. |
| **P6-D6** | **Combat numbers are content attributes, not a ruleset table** (new) | The swing pipeline is **engine shape**; the *numbers* (accuracy, evasion, dodge/parry/block, soak-by-type, `attacks`, crit chance/mult, to-hit/soak curves) are **content-defined derived attributes** (Phase 5 substrate) the pipeline reads + feeds to `check`. **No `combat_ruleset` table** — the attribute_defs *are* the tunable ruleset; a pack swaps the curve by redefining the formula. | Keeps "ROM, refined" tunable without recompile (COMBAT §9) and avoids a parallel config system. Cost: a handful of *conventionally-named* attributes a combat pack must define (`accuracy`, `evasion`, `soak_<type>`, `attacks`) — conventional, not engine-hardcoded; a contentless world simply has no combat. |
| **P6-D7** | **Avoidance ladder = a sequence of checks** (new) | To-hit is `check` #1 (`accuracy` vs `evasion`/AC → bands `{crit, hit, miss}` by margin/nat-max). A *would-be hit* then runs **independent** dodge → parry → block checks (each gated by content requirements: parry needs a wielded weapon, block needs a shield); first success negates the swing. 5e's single-AC model is the degenerate case (no dodge/parry/block defs → straight to soak). | COMBAT §3's layered ladder falls straight out of [G2]; each stage emits its own message + (Phase 9) its own GMCP event. Cost: 1–4 checks per swing — bounded, on the zone goroutine; the RNG is cheap. |
| **P6-D8** | **What persists vs is transient** (new) | **Persist:** per-ability **cooldowns** [G8] (into `state` JSONB), active affects + resource currents (Phase 5, unchanged). **Transient:** the `Fighting` state, threat lists, the per-round reaction budget, room-scoped affects (re-applied by content/reset). A logout/handoff drops combat but not cooldowns or buffs. | Matches player expectation (you don't resume a fight after a crash) and keeps the snapshot small. **persistence-engineer must confirm** the cooldown shape + that combat-transient state is correctly excluded from the fat snapshot. |

### 1.1 The check primitive (P6-D1/D2) — the prefix

A **check spec** (inline JSONB on an op, or a named `check_def` for reuse) carries (gap §2b):

```
{
  dice:       "1d20" | "2d20kh1" | "1d100" | "2d6" | "4dF" | "5d6>4",   // content notation
  bonus:      <formula over $actor attrs>,        // e.g. ["+", ["attr","$actor.dex_mod"], ["attr","$actor.athletics"]]
  vs:         <formula DC>  |  {contested: <defender check spec>},      // literal/formula or opposed roll
  bands:      [ {min: 10,           label:"strong", ops:[...]},          // ordered, first match wins
                {min: 7, max: 9,    label:"weak",   ops:[...]},
                {            label:"miss",   ops:[...]} ],                // default (no test) = last
  visibility: "hide" | "show" | "summary"                               // optional; resolves up the chain
}
```

The engine: rolls `dice` (extended notation, `rollDice` + new parsers), evaluates `bonus`/`vs` in a
**dual-scope formula context** (`$actor`/`$target`/`$source` — P6-D1/§1.4), computes the total (or
the contested comparison / pool success count), classifies into the first matching `bands` entry,
fires **`OnCheck`** (so content can react — a reroll, a lucky-halfling, bardic inspiration), runs
that band's `ops` via `runOps`, and emits per the visibility config. The `check` op is **structurally
identical to `if`/`chance`** (a flow op that recurses into `runOps`), so it adds no lifecycle change.

This makes the canonical examples one shape:
- **Climb a wall** — a room exit / object carries `check = {dice:"1d20", bonus:"$actor.dex_mod + $actor.athletics", vs:15, bands:{success→[move ...], failure→[deal_damage fall ...]}}`.
- **Saving throw** — invoked from a spell's op-list: `{dice:"1d20", bonus:"$target.dex_save", vs:"$source.spell_dc", bands:{success→half, failure→full}}`.
- **Attack roll** — `{dice:"1d20", bonus:"$actor.accuracy", vs:"$target.ac", bands:{faceEq:20→crit, faceEq:1→miss, marginMin:0→hit, default→miss}}`.
- **Save-ends condition** — the *same* spec fired on an affect tick (Phase 5 hook).

**Band-test axes (built in 6.1, after the rpg-systems acceptance review).** A band tests any of: the
**total** (`min`/`max`), the **margin** over the DC (`marginMin`/`marginMax`), or the **natural faces**
(`faceEq` + `faceCount` — "≥ N dice showing exactly V"). Every numeric edge is a **formula** scoped
like `bonus`/`vs`, so an edge can be a *derived* value (WoW crit/miss boundaries; BRP roll-under crit
= `max: ["/", skill, 20]`). This is what makes the union abstraction actually cover the catalog: 5e
nat-20 auto-crit / nat-1 auto-miss (face tests, ordered before the margin band), d100/BRP roll-under
+ degrees (formula `max` edges), Fate shift windows + contested ties (`marginMin`+`marginMax`), Blades
6-6 (`faceEq:6, faceCount:2`).

**Deferred check refinements (from the 6.1 review — not schema-breaking, so safe to land later):**
- *Outcome magnitude bindable into band ops* — exposing `margin`/`total`/success-count as a
  `$check.*` ref a band's ops can read (Fate "boost = shifts", BRP "damage scales with degree"). Lands
  with the **event-bus value-binding** work in **slice 6.2**, not as a one-off.
- *Blades' highest-single-die reading* — a `dicePoolHigh` kind banding on the single highest face
  (1-3/4-5/6 position). Count-pools (`Nd S>T`) already serve Year Zero / generic; the Blades
  position/effect read is a later dice-kind addition (or Lua).

**Builder-guide authoring notes to carry into the eventual docs (from the 6.1 review):**
- Teach **roll-under** (d100/BRP, Cairn) as `max`-on-**total** (the bare die as `total`, no `vs.dc`,
  ceiling bands), *not* a DC + margin — BRP's degree sub-thresholds (`dc/5`, `dc/20`) are formula
  `max` edges, so the `max`-on-total idiom is both correct and the only one that composes.
- `faceEq` counts **all rolled faces**, not the kept die — so an advantage (`2d20kh1`) nat-20 crit is
  right (either die), but pairing a `faceEq:1` auto-miss band *with* advantage is a known wrong-ish
  edge (documented in `check.go`); scope that guidance.
- Scope the Blades coverage claim to **crit-only** (the 6-6 `faceEq` band) until `dicePoolHigh` lands;
  the highest-die *partial* (4-5) tier isn't expressible by count-pools.

### 1.2 The in-zone event bus (P6-D3) — the keystone

The Phase 5 reservation (`ability.go` step 10 fires `OnAbilityResolved`/`OnHit` as log-only)
becomes a real dispatch. The model:

- **Subscription is content, collected per-entity.** A `resource_def` carries
  `on_event: {OnHit: [modify_resource $actor rage +5]}`; an `affect_def`, `ability_def`, or item may
  too. When an entity is built/loaded, the engine **collects its active subscriptions** (from its
  resources + active affects + known abilities + equipped items) into a per-entity handler map keyed
  by event kind — so a fire is an O(handlers) lookup, **not** a global scan.
- **Fire points** are engine-named and synchronous: `OnAbilityResolved`/`OnCheck` (abilities/checks),
  `OnHit`/`OnDamageTaken`/`OnKill` (the swing pipeline), `OnLeaveRoom` (movement), `OnRest` (the rest
  command, [G5]), `OnAffectApply/Tick/Expire` (Phase 5, now dispatched not just logged).
- **Single-writer, inline, depth-guarded.** Handlers run on the zone goroutine at the fire point.
  An `effectCtx` carries a **remaining-depth budget**; a handler that fires another event decrements
  it; at 0 further fires are dropped + logged (kills the `OnHit`→damage→`OnDamageTaken`→damage… loop).
  Ordering within a kind is stable (subscription order).
- **Harm still gates.** A handler op-list that deals damage / applies a detrimental affect routes the
  *same* `guardHarmful` (P5-D4) — an event handler is **not** a PvP-gate bypass (§5, security).
- **Boundary with Phase 10.** This bus is *zone-local, transient, synchronous*. The **scoped +
  durable (JetStream, ordered, idempotent) cross-zone bus** ([WORLD-EVENTS.md](WORLD-EVENTS.md)) is
  Phase 10. Phase 6 fires only entity/room-scoped events handled *within the firing zone*; a handler
  that needs a cross-zone consequence enqueues it for the (Phase-10) director — reserved, no-op now.

This single mechanism delivers [G3] **and** unblocks the WoW resource zoo (rage/runic/combo builders
are `on_event → modify_resource`), conditional regen [G4] (`on_event OnEnterCombat/OnLeaveCombat`),
the rest event [G5], and the declarative reactions [G9].

**Built in 6.2 + carried from its reviews:** the dispatch is gather-at-fire-time (no cached map, no
invalidation surface); subscriptions come from the resources an entity HAS + its active affects
(ability/item subscriptions await the Skilled/equipment components). Two guards bound the cascade — a
**depth** cap (re-entrancy) AND a shared **width** budget (`maxEventHandlers`, total handler runs per
root action) so a wide non-recursive fan-out can't starve the heartbeat. The harm gate is structural:
`guardHarmful` fails **closed** on a detached actor/target (the `fireOnTick` lesson, now covering every
fire point), and the three non-damage cross-player writes funnel one `guardCrossPlayerWrite`.
*Deferred (non-blocking):* the per-entity subscription **index** (the plan's "built at load/affect-
apply time" optimization — recompute-at-fire is correct and cheap at current scale; revisit if 6.3's
per-swing fire points profile hot); and a **Phase-7 threat-model note** — a Lua handler receives a live
`other` pointer with attr/has-affect read access, so the sandbox must decide whether Lua may read a
counterpart's hidden state or must see a capability-narrowed view.

### 1.3 The combat round (COMBAT.md, on the substrate)

- **Round driver:** `PULSE_VIOLENCE` = a fixed multiple of the base zone pulse (≈2.4s/round,
  tunable), registered like any pulse callback (resolve-by-id contract). Every entity in the
  `Fighting` state resolves its swings for the round on that pulse — per-zone, no global lockstep.
- **Swings/round:** `attacks` (a content attribute — weapon speed, haste affect, ROM second/third-
  attack, dual-wield). A round loops `attacks` swings; each runs the pipeline.
- **Swing pipeline (P6-D7):** gates (position/visibility/safe-room/immunity) → **to-hit check** →
  **avoidance ladder** (dodge/parry/block checks) → **damage roll** (weapon dice + `damroll` + crit
  band scale) → **soak/mitigation** (the *built* Phase 5 `dealDamage`: resist/vuln/immune matrix +
  soak-by-type) → **apply** (subtract the `vital` resource; `on_depleted` = death) → **on-hit
  procs** (`OnHit` event → content). Each stage emits its own message (visibility-aware) + reserves a
  GMCP event (Phase 9).
- **Cooldowns [G8]:** an ability commit arms `lag` (WAIT_STATE, exists) + a **per-ability cooldown**
  (new `pulse.after` + a cooldown map). Lifecycle **step 3** gains a "still cooling down?" gate
  (today it fires-and-logs). The map serializes into `state` (P6-D8). The **GCD** is just a shared-
  tag lag affect (apply a `gcd` affect that `prevents` the `ability` tag for N pulses — pure content
  on the Phase 5 tag-CC model, no engine change). Charges = a small resource that regens one per cd.
- **Death / corpse / threat:** `on_depleted` on the `vital` resource triggers the engine death path
  — create a **corpse** container entity holding inventory+equipment+coins, fire `OnKill` (XP award
  is a content handler — the [G6] hook, not built here), drop `Fighting`, mob corpse takes a loot-
  roll reservation (Phase 11/12). A single primary `Fighting` target; `assist`/threat list (damage +
  heal weighted) chooses mob targets; aggressive mobs initiate on entry.

#### 1.3.1 Acceptance-validated design inputs (rpg-systems-designer, before 6.3)

The §16 full-spectrum acceptance confirmed **DikuMUD/ROM, 5e attack-vs-AC, and the text-WoW hit/crit
table all express as PURE CONTENT** on this pipeline (no engine flavor-hardcoding; 5e degenerates
cleanly with no avoidance defs; WoW's single-roll combined table is just the to-hit check with a
richer band list). It surfaced **one genuine new mechanism** plus three pipeline-SHAPE constraints
6.3 must honor (cheap now, expensive to retrofit):

- **[G-A] Damage as a FORMULA over attributes — the one new mechanism (highest priority, blocks all
  three models).** `opDealDamage` takes a flat `amount` + literal `NdS` today. ROM `weapon + damroll
  + str_bonus`, 5e crit-doubled dice + level-scaled riders (sneak attack `ceil(level/2)d6`), and WoW
  combo-point finishers ALL need the damage amount/dice-count to read `$actor`/`$target`/`$source`
  attributes. Extend the damage op to accept a **scoped formula** (reusing the 6.1 formula evaluator +
  check scoping) for the bonus and for a dice-count formula; the result still flows through
  `dealDamage → guardHarmful → mitigate` (no security change). *Without it a sword that adds STR falls
  to Lua — a pillar regression for the most basic case.* **Owned by abilities-engineer; landed FIRST
  as a standalone primitive (it's reusable by any ability), then the swing pipeline consumes it.**
- **[G-F] The avoidance ladder is OPTIONAL (0+ content-declared checks); the to-hit `check` may be the
  SOLE classifier.** ROM authors dodge/parry/block; 5e authors none (straight to soak); WoW authors
  none and puts the whole hit/dodge/parry/block/crit table in the to-hit check's bands. The pipeline
  must NOT hardcode "always run dodge then parry then block" — it runs the to-hit check, then
  zero-or-more content-declared avoidance checks. (Avoidance gating — parry needs a weapon, block a
  shield — is content via gear zeroing the `parry`/`block` attribute → roll-under-0 auto-fails; no new
  predicate. [G-C])
- **[G-G] The round driver defaults to SIMULTANEOUS (no initiative).** Per-pulse iteration over
  `Fighting` entities is a stable/arbitrary order by default (ROM/WoW); 5e initiative is an OPTIONAL
  content `check` at `OnEnterCombat` that writes an order attribute the driver sorts by. Do not bake
  in "roll 1d20+dex" or "no order" — make the iteration order a content-overridable sort key.
- **[G-H] Expose the per-swing INDEX to the swing ctx** (a `$swing.index` scoped ref) so the to-hit
  `bonus` formula can vary by swing — PF iterative attacks (−5/−10/−15) are unauthorable without it.
  Cheap while the round loop is being written; 5e/ROM/WoW don't need it.

Already in 6.3 scope, confirmed no shape problem: **[G-B]** wire `soak()` to read `soak_<type>`
attributes (5e = the no-op case, defines none); **[G-D]** `on_depleted` → death → corpse → `OnKill`
(carry `subject`=killer, `other`=victim). Content-expressible via built conventions: **[G-E]** a
once-per-round rider (sneak attack "once per turn") gates on a per-round resource readable from an
`OnHit` handler — confirm the convention reaches `on_event` handlers. Crit covers all idioms
(nat-face `faceEq` / %-chance `max` band / margin) — the crit consequence (double dice / ×mult)
routes through [G-A].

**Slicing (the plan's split, with the acceptance inputs folded in):**
- **6.3a — round driver + swing pipeline + formula damage.** [G-A] formula damage (first, standalone);
  `PULSE_VIOLENCE` + `Fighting` (simultaneous default [G-G]); the swing pipeline (to-hit check →
  optional avoidance ladder [G-F] → formula damage + crit bands → soak [G-B] → apply → `OnHit`/
  `OnDamageTaken`); cooldown completion [G8] + GCD; swing index [G-H]; a ROM-style combat content
  pack. **Done when:** you fight a mob through the full pipeline (miss/dodge/parry/block/soak) with a
  STR-bonus weapon, all from content.
- **6.3b — death & threat.** [G-D] `on_depleted` → death → corpse (gear+coins) → `OnKill`; threat list,
  `assist`/`flee`/`consider`. **Done when:** the mob dies, drops a lootable corpse, and threat/assist work.

### 1.4 Formula context scoping & new heads (P6-D1/§G1)

`formula.go` gains: arithmetic heads `floor`/`ceil`/`round`/`mod` and a conditional head
(`["if", cond, then, else]`) for exact integer derivation (5e mods, PF BAB, WoW ratings); and a
**scoped `attr` ref** — `["attr", "$target.dex_save"]` resolves against whichever entity the check
binds to `$target`. The `effectCtx` already tracks actor/target/source; the check evaluator threads
that binding into `evalFormula`. Cycle detection (Phase 5 visited-set) is unchanged.

---

## 2. Schema + loader integration

Phase 6 is **light on new tables** — most of it is new *op handlers*, new *formula heads*, new
*pulse callbacks*, and new `on_<event>` JSONB fields on **existing** def-tables (additive, JSONB-tail
pattern — no migration strain; **persistence-engineer to confirm**).

### 2.1 Migration `00004_combat.sql` (small)

```sql
-- Named, reusable check specs (inline specs need no table; this is for shared checks).
CREATE TABLE check_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  body JSONB NOT NULL DEFAULT '{}'   -- {dice, bonus, vs, bands[], visibility}
);
```

Everything else rides existing tables as **new JSONB fields** (no `ALTER` needed — they live in the
existing `body`/tail JSONB and are parsed by the extended mapper):
- `resource_def.body.on_event`, `.regen` extended to a **conditional formula** [G4] (regen `when` a
  content predicate — e.g. `-N when not in_combat`).
- `affect_def.body.on_event`, `.scope` ∈ `entity|room` [G13] (a `room`-scoped affect attaches to the
  room entity and ticks over occupants).
- `ability_def.on_resolve` gains the `check` op + AoE targeting (`targeting.area` ∈
  `self|target|room|room_and_adjacent`); `ability_def` gains `cooldown` *completion* (the column
  exists from Phase 5; now enforced) + `reaction` metadata (`checkpoint`, `reaction_cost`).
- item/gear modifiers into the attr mod-stack [G14] (the `modSource` seam is built-and-waiting from
  Phase 5 — wire equipped gear as a modifier source; affixes are Phase 12).

### 2.2 Loader / mapper / registries

- `internal/content/dto.go`: add `CheckDTO` + the `on_event`/`scope`/`area`/`reaction` fields to the
  existing ability/affect/resource DTOs; add `Pack.Checks`.
- `internal/store/content.go LoadPacks`: `SELECT` `check_defs WHERE pack = ANY($1)`; single-row
  loader for hot-reload (mirror Phase 5).
- World side: a `checkRegistry` (atomic.Pointer-swapped, like the Phase 5 registries); the
  DTO→runtime builders parse the check spec (band list, dice notation, scoped formula). The **event
  dispatcher** is per-shard runtime state (the per-entity handler maps are built at entity
  load/equip/affect-apply time — not a registry, derived from active content).

### 2.3 Stdlib combat pack (the acceptance content)

Extend the demo/stdlib pack with the **conventionally-named combat attributes** (`accuracy`,
`evasion`, `dodge`, `parry`, `block`, `soak_slash`/`soak_fire`/…, `attacks`, `crit_chance`,
`crit_mult`, `ac`, the save attrs), a **weapon** item (dice + damage type + `attacks`), an **armor**
item (soak mods via the `modSource` seam), a **mob** with those attributes + a loot reservation, and
two abilities exercising the new surface: `bash` (a melee skill: to-hit check, lag+cooldown, a stun
affect on hit) and a `fireball` upgrade (now an **AoE** over the room with a per-target DEX save
[G12], and a rage resource that builds via an `OnHit` handler [G3]). The **bare-engine invariant**
holds: no combat pack ⇒ no combat attributes ⇒ `kill` simply reports nothing to fight (the empty-boot
test stays green; combat ops are unavailable, not erroring).

---

## 3. Persistence integration (P6-D8)

- **Cooldowns [G8]:** `dumpCharacter` adds `cooldowns: {abilityRef: remainingPulses}`; `loadCharacter`
  re-arms each via `pulse.after(remaining)`. A logout mid-cooldown does **not** refresh it. Affect
  durations + resource currents are the Phase 5 shape, unchanged.
- **Transient (NOT in the snapshot):** `Fighting`/target, threat lists, the per-round reaction budget,
  the per-entity event-handler maps (derived, rebuilt on load), room-scoped affects (re-applied by
  content/reset). On a crash or handoff, combat drops cleanly; the durability ladder carries only the
  cooldown map + the already-persisted affects/resources.
- **Handoff:** the cooldown map rides the fat snapshot (same subtree mechanics as Phase 5 affects);
  the destination re-arms on `transferIn` on the destination goroutine (the Phase 5.2 lesson — never
  a cross-goroutine timer write). **distributed-systems-architect + persistence-engineer** confirm.

---

## 4. Slicing (ordered, independently committable)

The spine is **primitive → glue → combat → area/reactions**. Each slice is a commit with the prior
phase's tests green and its owning + cross-cutting reviewers signing off
([subagent-review-after-every-step]).

| Slice | Scope | Done when | Tests added |
|-------|-------|-----------|-------------|
| **6.1 — The check primitive [G2]** (the prefix) | Dice-notation extension (kh/kl, `dF`, pool `>N`); the `check` flow op + ordered-band classifier (P6-D2); the `$actor`/`$target`/`$source` scoped formula context + the new heads `floor`/`ceil`/`round`/`mod`/`if` [G1]; visibility resolution + text emission (P6-D5); the `OnCheck` fire point reserved. **No combat, no event handlers yet.** | A room exit with a climb `check` resolves deterministically (seeded) and branches to `move`/`deal_damage`; a DEX save inside `fireball`'s op-list halves damage on success; a PbtA 3-band and a contested check both classify correctly; visibility `hide`/`show` render the two paths. | check-op + band-classifier unit tests (binary / half / 3-tier / degrees / contested / pool); dice-notation parser tests; scoped-formula + new-head tests; visibility render test. All Phase 1–5 green. |
| **6.2 — The in-zone event bus [G3]** (the keystone) | Synchronous single-writer dispatch (P6-D3): per-entity handler collection from `on_event` on resources/affects/abilities/items; the fire points (`OnAbilityResolved`/`OnCheck` live; `OnHit`/`OnKill`/`OnLeaveRoom`/`OnRest` reserved for 6.3/6.4); the **recursion-depth budget**; `guardHarmful` on handler ops. Conditional resource regen [G4] + the `rest` command/`OnRest` event [G5] ride here. | A `resource_def` with `on_event OnCheck → modify_resource $actor rage +N` builds rage when its owner makes a check; the depth guard halts a deliberately self-firing handler; a harmful handler op vs a protected player is **blocked** by the gate; `rest` refills a per-rest pool; rage decays out of combat via a conditional regen. | dispatch + subscription-collection tests; **depth-guard / re-entrancy test**; **gate-applies-to-handler-ops test (security)**; conditional-regen + rest-event tests. |
| **6.3 — Combat round resolution** (the milestone) | `PULSE_VIOLENCE` round driver + `Fighting` state; `attacks`/round; the swing pipeline (gates → to-hit check → dodge/parry/block ladder → damage+crit → `dealDamage` soak → apply → `OnHit`); **cooldown completion + step-3 gate + persistence [G8]**; the GCD-as-tag-affect; gear modifiers into the mod-stack [G14]; death → corpse; threat/`assist`/`consider`/`flee`. | The ROADMAP done-when: `kill mob` → fight through the full pipeline (miss/dodge/parry/block/soak visible in the log), it dies, a corpse holds its gear+coins, you loot it; a cooldown survives logout; the GCD blocks back-to-back skills — all content. | swing-pipeline stage tests (each avoidance layer); to-hit-as-check test; crit-band test; death→corpse test; **cooldown persistence round-trip**; threat-selection test; round-driver pulse test (resolve-by-id). |
| **6.4 — AoE, room-affects & reaction checkpoints** | AoE targeting [G12] (loop the *built* `dealDamage`/harm-gate per target over `room`/`room_and_adjacent`, **same-zone-guarded** — §5); room-scoped affects [G13] (attach to the room entity, tick over occupants); named interruptible **checkpoints** [G9] firing events, with declarative reactions (opportunity attack on `OnLeaveRoom`; `OnDamaged` rebuke) + the per-round reaction budget. Result-altering reactions remain Phase 7. | `fireball` over a room rolls a **per-target** save and gates **each** target independently; a `web` room-affect roots entrants and ticks; leaving a room with an engaged enemy provokes a declarative opportunity attack that consumes the reaction budget; an AoE never reaches a same-named room on another shard. | per-target AoE + **per-target gate** test; **cross-zone AoE-containment test**; room-affect attach/tick/occupant test; opportunity-attack + reaction-budget test. |

**Adjustment / justification.** 6.1 lands the check primitive *standalone* exactly as decided (gap
§18.2) — its biggest payoff is being usable by non-combat content (exits/objects) before combat
exists. 6.2 lands the bus next because 6.3's `OnHit`/`OnKill` and the resource builders depend on it.
If **6.3 proves large**, split **6.3a round driver + single-target swing pipeline + cooldowns** from
**6.3b death/corpse/threat/assist** (the death path touches loot reservations + groups). If **6.4
proves large**, split AoE+room-affects from reaction checkpoints. Recommend planning for the splits,
executing as one each if they stay small (the Phase 5.3 pattern).

---

## 5. Risks & out-of-scope

### Explicitly OUT of scope
- **Lua handlers / result-altering reactions [G9] / concentration [G11] / multiclass slot math [G7]
  = Phase 7.** Phase 6 ships the *declarative* event handlers + the *checkpoint events*; the alter-
  the-in-flight-result path and the complex-20% are the Lua hatch. Design the checkpoints **now** so
  Phase 7 only adds handlers, not pipeline surgery.
- **The cross-zone scoped + durable event bus = Phase 10** ([WORLD-EVENTS.md](WORLD-EVENTS.md)). Phase
  6's bus is in-zone, synchronous, transient. A handler needing a cross-zone consequence enqueues for
  the (Phase-10) director — reserved no-op.
- **Progression / chargen / XP-to-levels [G6] = Phase 11.** Phase 6 *fires* `OnKill` and awards no XP
  itself — the XP-on-kill handler is content that lands with the progression tracks. Classes/races as
  bundles, advancement modes: Phase 11.
- **GMCP combat deltas = Phase 9.** Emit `act()`/`send` text now (visibility-aware); reserve the
  `Char.Vitals`/`Char.Status`/`Mud.Target`/`Mud.Cooldowns`/`Mud.Afflictions` emit points (COMBAT §8).
- **Loot tables / affixes / scheduled boss spawns = Phase 11/12.** Death creates a corpse + reserves
  a loot roll; the resolver is later. Gear `modSource` wiring [G14] is here; affix *rolls* are Phase 12.
- **Tactical-grid spatial fidelity = never** (settled gap §18.3) — no intra-room coords; range =
  same-room vs adjacent-exit; positioning = abstract engaged/disengaged tags.

### Integration risks
1. **Event-bus re-entrancy & ordering (the keystone risk).** Synchronous in-zone dispatch means a
   handler runs *inside* the action that fired it. An `OnHit` handler that deals damage fires
   `OnDamageTaken`, whose handler might heal/reflect, etc. Mandatory: a **depth budget** in the
   `effectCtx`, stable within-kind ordering, and a rule that handlers see a **consistent single-
   threaded** zone (no I/O, no cross-entity timer writes). **distributed-systems-architect must
   review** — and confirm the clean seam to the Phase-10 durable/ordered/idempotent cross-zone bus so
   we don't build something Phase 10 must rip out.
2. **AoE must not reach across a shard.** "room + adjacent rooms" can name an exit whose destination
   is a zone on **another shard**. The AoE loop must enumerate **same-zone** targets only (or message-
   pass to the neighbor zone — reserved); never dereference a cross-zone `*Entity` (the Phase 5.2/5.3
   single-writer bug class). **distributed-systems-architect reviews** the containment.
3. **The PvP gate over the whole new harm surface (security).** Every new harm vector — swing damage,
   AoE per-target, event-handler ops, reaction op-lists, room-affect ticks — must funnel the **same**
   `guardHarmful` (P5-D4), gated **per target** (an AoE re-gates each occupant; an `OnHit` proc that
   damages re-gates). A check that *branches* into a harmful op does not bypass the gate (the gate is
   at the op, not the ability). **security-auditor must review 6.2, 6.3, 6.4** — this is the largest
   harm-injection surface added since the gate was built, and the in-op funnel is what makes it
   can't-forget. Also: a content `check` must not let a pack read/leak another player's hidden state
   via `$target` scoping beyond what targeting already permits.
4. **Don't block the zone goroutine.** Round driver, swings, checks, event handlers, cooldown timers
   all run single-writer on the pulse — any I/O (none expected) goes async + posts back (reset.go /
   saver pattern). The round loop must obey the `pulseFunc` resolve-by-id/skip-frozen contract.
5. **Combat numbers stay content (the pillar).** No hardcoded `hp` (read the `vital` resource by
   flag), `d20` (content dice notation), or `success/failure` (content bands). The conventionally-
   named combat attributes (`accuracy`/`evasion`/`soak_*`/`attacks`) are a *pack convention*, not
   engine constants — a non-d20 pack redefines them. The **rpg-systems-designer** validates that a
   plain Diku/ROM fight, a 5e attack-vs-AC, and a WoW hit/crit table all express in this pipeline
   (the §16 acceptance question) before 6.3 is cut.
6. **Cooldown/affect conservation across save & handoff.** Cooldowns (new) join affects/resources in
   the persisted subtree; re-arm on the destination goroutine on `transferIn`, never reset on load.

### Cross-cutting reviewers (per [subagent-review-after-every-step])
- **combat-engineer (owning):** 6.3/6.4 — the round driver, swing pipeline, avoidance ladder,
  cooldowns, death/corpse/threat; confirm the §20 validation (checks host to-hit/saves; checkpoints;
  AoE over the built `dealDamage`; `attacks`/avoidance over content formulas; `soak()`/`modSource`).
- **abilities-engineer (owning):** 6.1/6.2/6.4 — the `check` flow op fits beside `if`/`chance`
  additively; `on_event` subscriptions + AoE + room-affects fit the effect-op/Affected runtime with
  no lifecycle change; the resource→scaling read (combo finishers).
- **distributed-systems-architect:** 6.2/6.3/6.4 — the in-zone event-bus re-entrancy/ordering/depth
  guard + the Phase-10 boundary; combat on the pulse (single-writer); AoE same-zone containment;
  cooldown/affect conservation across the handoff + durability ladder.
- **security-auditor:** 6.2/6.3/6.4 — the PvP/hostility gate over the entire new harm surface
  (per-target AoE, event-handler ops, reaction ops, room-affect ticks); check `$target` scoping can't
  leak state; the in-op funnel is can't-bypass even ahead of the Phase 7 Lua hatch.
- **persistence-engineer:** 6.3 — cooldown serialization into `state`; combat-transient state excluded
  from the snapshot; `check_defs` follows the per-kind-table + JSONB-tail + `pack` pattern.
- **rpg-systems-designer (acceptance):** before 6.3 — confirm Diku/ROM, 5e attack-vs-AC, and a WoW
  hit/crit table all express in the pipeline + the check bands (the §16 full-spectrum acceptance).
- **scripting-engineer (forward-looking):** review the 6.4 checkpoint set so the Phase 7 result-
  altering reactions (Counterspell/Shield) + concentration have the hook points they need.


---

<a id="phase-7"></a>

# Phase 7

_(archived from docs/PHASE7-PLAN.md)_

# Phase 7 — Lua scripting (the curated escape hatch + sandbox) — IMPLEMENTATION PLAN

Status: **proposal / planning** — slices the existing [LUA.md](LUA.md) design (one VM/zone,
handle-not-pointer API, restricted-globals sandbox + instruction-budget + circuit-breaker,
`self.state` persistence, hot reload, per-zone-RNG determinism). The design is the baseline; this
plan **orders** it into shippable slices, foregrounds the **sandbox threat model** (the sharpest
trust boundary in the engine), and **resolves the three open design forks** LUA.md §10 flags but
never wrote. Confirm §1 + §3 before slice 7.1.

Lua is content's escape hatch for the complex ~20% the declarative op-list can't express
([PRINCIPLES.md](PRINCIPLES.md): engine = mechanism, content = flavor; and the second pillar — every
action is hookable, Phase 7 makes the hook *bodies* arbitrary). **Done when** (ROADMAP Phase 7): a
room script greets on entry, a scripted mob reacts to speech, a Lua Counterspell cancels an in-flight
cast, and a pack defines/fires/handles a custom event the engine never heard of — **all edited live,
none able to crash, stall, or cross a zone.**

This phase builds the **Lua runtime** on the Phase 6 substrate (the in-zone event bus, the effect-op
interpreter + the shared `guardHarmful`/`dealDamage`/`applyDebuff` harm funnels, the per-zone pulse
scheduler, the per-zone seeded RNG, the `state` JSONB ladder, the hot-reload applier). It does **not**
build: the cross-zone scoped + durable event bus (Phase 10), GMCP structured emit (Phase 9),
progression/chargen/the track grants Lua will compose (Phase 11), or any new concurrency.

---

## 0. Where Phase 7 sits on the existing code

| Existing (Phase 3–6) | Phase 7 change |
|---|---|
| `zone.go` — the single-goroutine actor (`Run` serial inbox loop + pulse ticker; `dispatchSafe`/`handle` panic nets) | Each zone gains **one `*lua.LState`**, constructed at zone build, called **only** from `Run`'s goroutine. No new goroutine, no lock — the VM rides the existing single-writer invariant. |
| `effect_op.go runOps` + the registered op table; the **one** `guardHarmful`/`guardCrossPlayerWrite` chokepoint; `dealDamage`/`applyDebuff` shared funnels | Lua effect-op handles (`h:damage{}`, `h:apply_affect{}`, …) **call the same Go funnels** — no parallel harm path. A Lua op physically cannot reach a protected player except through `guardHarmful` (the can't-bypass property extended to the Lua surface). |
| `event.go` — the in-zone bus, `knownEventKinds` closed set, `gatherEventHandlers`, `fireEvent` with `maxEventDepth`/`maxEventHandlers` guards | Handlers may now be **Lua bodies** (not just op-lists); the bus grows a **`pack:event` lane** (builder-defined kinds) **and** lights the reserved engine kinds (`OnApplyAffect`/`OnAffectTick`/`OnAffectExpire`, a new `OnEnter`). Lua handlers run under the **same** depth/width budget. |
| `defs.go` reserved Lua fields — `affectDef.onApply`/`onExpire` (read-not-run), `abilityDef.onResolveLua` (read-not-run); `ability_build.go` carries `OnResolveLua` but never executes | Those reserved columns become **live**: compiled to a Lua chunk at content build, invoked through the sandbox + `pcall` + budget. |
| `check.go` — the check primitive + `OnCheck` fire; `formula.go` — the prefix-AST evaluator | The `pvp_allowed` policy hook + ruleset formulas (`to_hit`/`soak`/`regen`/`xp_for`) gain a **Lua alternative** to the prefix-AST (a pack picks data formulas OR a Lua function — never both for one ref). |
| `character.go` `StateJSON` + the save cadence (`dumpCharacter`/`loadCharacter`); the durability ladder | `self.state` is a **data-only** subtree mirrored into `StateJSON.Script` (new field), serialized on the same cadence, size-guarded. No code, no handles, no closures persist. |
| `reload.go` — the hot-reload applier (atomic prototype swap on a `(kind, ref)` content-bus invalidation) | Hot reload also **recompiles the Lua chunk** and **swaps the registered handlers**; `self.state` data survives (it's not code); a generation tag drops stale `mud.after` callbacks. Rides the **existing** invalidation path. |
| `pulse.go` — the per-zone timer wheel (`after`/`every`, resolve-by-id-or-cancel) | `mud.after(pulses, fn)` schedules on **this** wheel — never a real sleep, never a goroutine. The callback runs on the zone goroutine; a generation tag drops callbacks bound to a reloaded chunk. |
| `identity.go` — `RuntimeID` (per-zone uint64) + the target-resolution by RID | Lua **handles** wrap `(RuntimeID, zone)` as validated userdata with a `__tostring` metamethod (never the raw Go pointer — T15); every method re-resolves the entity **still exists and is in this zone** before acting (LUA.md §4). No `*Entity` ever reaches Lua. |

> **Doc-correction note:** [LUA.md](LUA.md) §5 cites `SetMaxStackSize` and "instruction budget via the
> LState context" — **both are wrong for gopher-lua v1.1.1** (verified by the security-auditor's probes
> against the real runtime, 2026-06). `SetMaxStackSize` does not exist (the control is the constructor
> `lua.Options{CallStackSize, RegistrySize/RegistryMaxSize}` — §1.1/T4); the `LState` context bounds
> wall-clock **between** ops only, never instruction count and never inside a Go builtin (P7-D6/T3/T13).
> This plan supersedes LUA.md §5 on both points; LUA.md should be amended when this plan is signed off.

The riskiest *structural* points: (a) **the sandbox is the sharpest trust boundary in the engine** —
builders run arbitrary Lua **in-process, on the zone goroutine** (§ Sandbox threat model — written to
be reviewed by the security-auditor); (b) **the harm-gate must funnel** — a Lua effect op is the
newest harm-injection surface since the gate was built, and like Phase 6's event handlers it must
route the same `guardHarmful` with no second path (§4); (c) **memory** — gopher-lua has no hard
per-VM cap, an acknowledged limitation we bound indirectly, not silently (§ threat model, M-row).

---

## 1. Tech / design decisions (confirm before slice 7.1)

| # | Decision | Recommendation | Trade-off |
|---|----------|----------------|-----------|
| **P7-D1** | **Runtime + VM granularity** (settled — LUA.md §1) | `github.com/yuin/gopher-lua` (already a transitive dep — promote to direct), **one `*lua.LState` per zone**, called only from `Zone.Run`'s goroutine. Per-script sandboxed `_ENV`. | Memory/perf amortized per zone; isolation between zones is automatic (a script can only reach its own zone). Cost: a zone-scoped VM lifecycle wired into build/teardown. The actor model already gives us lock-free single-writer — the VM rides it. |
| **P7-D2** | **API shape: curated handles, not reflection** (settled — LUA.md §4) | A **curated, hand-written binding surface** (handle userdata + a `mud` table) — **never** `gopher-luar`, never a raw `*Entity`. A handle wraps `(RuntimeID, zone-generation)`; every method re-validates in Go. | Content depends on the API, not Go struct layout (refactor-safe); no dangling pointers; no cross-zone reach. Cost: every exposed capability is hand-bound (a feature, not a bug — the API surface *is* the audit surface). |
| **P7-D3** | **Harm funnels reuse, never duplicate** (settled — the Phase 6 boundary) | Every Lua effect op routes the **existing** `dealDamage`/`applyDebuff`/`guardCrossPlayerWrite` — which call `guardHarmful` first. No Lua-specific harm path. A Lua handler on the bus is **not** a gate bypass, exactly like a declarative op-list handler. **Five `effectCtx`-binding invariants (the binding's single most security-sensitive code; refs effect_op.go — effectCtx:38, guardHarmful:252, guardCrossPlayerWrite:293, dealDamage:340, applyDebuff:448):** (1) **actor/source/target are ENGINE-resolved** from the handle's `(rid,zone,gen)` + the invocation context — **never script-supplied** (no `h:apply_affect{source=arbitrary}` attribution-spoofing); (2) **`disp` is engine-set** from the op/def — a script cannot set it helpful to skip the gate; (3) the funnels are the **ONLY write path** — the T8 audit (below) is a **build-failing lint**, not a grep; (4) **`rng` is always** the ctx/zone RNG (P7-D4); (5) **`depth`/`eventBudget` are threaded** from the invoking cascade, **never reset**. | The can't-forget property the security-auditor already trusts extends to Lua for free — **provided the five invariants hold**: each is a way the binding could *silently* re-open the gate the funnel closes. |
| **P7-D4** | **Determinism: the per-zone engine RNG only** (settled — LUA.md §9) | `math.random` is **rebound** to the per-zone seeded RNG; `mud.random`/`mud.roll` draw the same source. **No** `os.time`, `os.clock`, no Lua RNG state, no other entropy. `mud.now()` returns the zone pulse counter (deterministic), not wall-clock. | Combat/loot/procs stay reproducible in tests + replays; a script cannot be a non-determinism injection vector. Cost: the binding must thread the ctx RNG into every Lua-reachable random draw (the `effectCtx.rng` seam already exists). |
| **P7-D5** | **`self.state` is data-only, size-guarded** (settled — LUA.md §7, + the new guard) | `self.state` is a plain Lua table mirrored to/from `StateJSON.Script` JSONB: numbers/strings/booleans/nested tables of those **only**. No functions/closures/userdata/handles (store `h:id()`, re-resolve). A **byte-size + depth + key-count cap** on the marshalled subtree (state-injection bound — § threat model). | Script memory rides the normal durability ladder; a runaway `self.state` can't balloon the snapshot or the VM. Cost: a Lua-table↔JSON marshaller with the type allowlist + the caps, run at save time on the zone goroutine. |
| **P7-D6 (RESOLVED — LUA.md §10 fork 1; USER DECISION 2026-06)** | **Per-call budget: how is the instruction/wall-clock limit enforced, and what are the defaults?** | **THREE layers (user-decided): (1) a vendored gopher-lua fork** adding an instruction-count abort in `mainLoopWithContext` beside the existing `ctx.Done()` select (gopher-lua v1.1.1 has **no** `SetHook`/`MaskCount`/debug-hook — the count must come from the fork); **(2) the `LState` context wall-clock deadline** (`SetContext`+`context.WithTimeout`, armed fresh per call — §4 chokepoint invariant); **(3) capped amplifier builtins** (T13 — the deadline checks between ops, never inside a Go builtin, so `string.rep`/`format`/`gsub`/`table.concat` ship as size-capped wrappers). Default: **deadline = 5ms wall-clock, budget = 100k VM instructions per entry-point call**, both tunable per pack and overridable per def; the fork is pinned + documented. *(7.1 review note: 100k is TIGHT — a plain 50k-iteration arithmetic loop hits it; the rpg-systems-designer acceptance pass must validate the default against real content loops/formula tables and raise it if surprisingly low — it's tunable, not a safety knob.)* | The three layers are complementary, not redundant: the **count** is deterministic + test-reproducible (a tight pure-CPU loop trips it identically every run, unlike wall-clock); the **clock** catches a low-instruction stall (a GC pause, a slow C-side call); the **builtin caps** catch a single-op bomb (`string.rep("A", 2e9)` — one instruction, GB allocated — that neither the count nor the clock can stop, T13). Cost: vendoring + a pinned fork to maintain; the count check adds per-N-instruction overhead (granularity ~1k ops, <2%). **Resolved below (§ open forks); vendoring is slice 7.1's first work item.** **security-auditor must review the fork's abort path + the builtin caps.** |
| **P7-D7 (OPEN — LUA.md §10 fork 2)** | **Hot-reload of in-flight `mud.after` callbacks bound to a now-swapped chunk: complete or drop?** | **DROP by generation tag** (the configurable LUA.md §8 default made concrete). Each compiled chunk carries a monotonic `gen`; `mud.after` captures the gen at schedule time; on fire the wheel skips a callback whose gen != the def's current gen. A pack may opt a specific timer into *complete-anyway* (`mud.after{durable=true}`) for a state-cleanup finalizer. | A live edit must not run **old code** against **new state** (the subtle reload-corruption class); dropping is the safe default. Cost: a gen field on the chunk + the timer; the rare legitimate finalizer gets the opt-in. **Resolved below.** |
| **P7-D8 (OPEN — LUA.md §10 fork 3)** | **Result-altering reactions (Counterspell/Shield/concentration): how does a Lua hook reach INTO an in-flight ability to alter/veto it, within the single-writer model?** | A **reaction context object** passed to the `BeforeCastCommit`/`OnDamageTaken`/check checkpoint hooks (the Phase 6 named checkpoints, already designed-in): `rx:cancel()`, `rx:modify(field, delta)`, `rx:replace_target(h)`, `rx:consume_resource(ref, n)`. **Three hardening invariants (security):** (1) `field` is a **closed per-checkpoint enum resolved by a Go switch** — to-hit allows only `{"ac"}`, `OnDamageTaken` only `{"amount"}` — **never a string indexing an attribute map**; (2) `rx:replace_target(h)` **re-runs `guardHarmful` against the new target** (the original gate ran against the original target — replacing onto a non-consenting player otherwise bypasses it); (3) the reaction path threads the **same `eventBudget` pointer** (effect_op.go:56) so a reaction→checkpoint→reaction loop is bounded by the shared width cap + the depth cap. The engine fires the checkpoint, runs the Lua hook **synchronously inline**, then **re-reads** the reaction object's recorded mutations and applies them at the seam — the **observe-then-recheck** shape the death checkpoint already implements (PRINCIPLES.md). The hook cannot reach past the fields the checkpoint exposes. | Counterspell (`rx:cancel()` on `BeforeCastCommit` if the caster spends a slot + wins a check), Shield (`rx:modify("ac", +5)` on the to-hit checkpoint), concentration (`rx:cancel()` of the concentration affect on a failed `OnDamageTaken` save) all express **without pipeline surgery** — Phase 6 designed the checkpoints, Phase 7 only adds the alter-capable hook bodies. Cost: each checkpoint must publish a typed, bounded reaction object (not a raw pipeline pointer) + the per-checkpoint field enum. **Resolved below.** **security-auditor reviews the mutation allowlist + the re-gate.** |
| **P7-D9** | **Gating: who may author Lua?** (settled — LUA.md preamble) | Lua is **gated to reviewed authors** (a pack-level `lua_trusted` flag content-side); the **sandbox is defense-in-depth regardless** — it must hold even against a hostile author, because the gate is policy and the sandbox is mechanism. | The threat model assumes a hostile author (§) even though policy restricts authoring — the engine never relies on the gate for safety. Cost: none structural; it shapes the threat model's adversary. |
| **P7-D10** | **`pcall` isolation + the circuit breaker** (settled — LUA.md §6, + two hardening calls) | Every entry point is invoked through Go-side `pcall`. A failure **fails just that action**, logs `(zone, kind, ref, stack)`, and increments the script's **error budget**; repeated failures **trip a breaker** that disables *that script* (not the zone), alerts ops, and re-enables on the next successful hot-reload. The player-facing fizzle message is **generic — never the raw Lua error/stack** (that goes to ops logs only — T11/T15). **Two hardening calls:** (a) **breaker scope** — a breaker keyed per-`(kind, ref)` over a **SHARED def** (one ability/affect used by many entities) trips **content-wide**, so a hostile shared def is a content-wide DoS; **recommendation: per-instance breaker for entity-scoped scripts (triggers/`self.state`-bearing defs), per-`(kind,ref)` for genuinely shared defs, and the shared-def blast radius documented**; (b) **separate accounting** — **wall-clock-deadline aborts are weighted/rate-limited DIFFERENTLY from deterministic logic errors**, so a deadline trip under load (a GC pause) doesn't quarantine a correct script, and an attacker can't drive a victim's breaker by inducing latency. | No script can crash a zone; a chronically-broken script is quarantined, not the world — **without** a latency-induced false quarantine or a shared-def cross-content trip being a silent surprise. Cost: a two-mode breaker + the deadline-vs-error split. |

### 1.1 The compiled-chunk lifecycle (P7-D1/D7, the spine)

A scripted def (`ability_def.on_resolve_lua`, an `affect_def.on_apply`/`on_tick`/`on_expire`, a
room/mob/item trigger block, a custom command, a formula, the `pvp_allowed` policy) carries a **Lua
source string** in content. At content build (and hot reload) the source is **compiled once** into a
`*lua.FunctionProto` (the reusable bytecode) tagged with a monotonic **generation**. At invocation the
engine instantiates the proto into the zone's `LState` under a **fresh sandboxed `_ENV`** (so one
script can't clobber another's globals in the shared VM), binds `self`/`ctx`/`ev`/`rx` as appropriate,
and calls it through `pcall` + the budget. The proto + gen live in the per-shard registry beside the
prototype it belongs to; the reloader recompiles + bumps the gen and swaps it via the **existing**
atomic registry swap (`reload.go`). Compilation failures are non-fatal (the def keeps its last-good
proto, like the prototype reloader keeps the last-known on a re-read error).

**The fresh-`_ENV`-per-call claim is INCOMPLETE on its own (T14).** A per-call `_ENV` isolates a
script's **globals**, but it does **not** cover **`L.G`-scoped** (VM-global) state shared across every
`_ENV` in the zone. The probed escape: `getmetatable("")` returns the **string library module itself**
(it lives on `L.G.builtinMts`, is VM-global, and is **writable**); a script doing
`getmetatable("").rep = evil` poisons `("x"):rep()` for **every** other script in the zone — including
trusted policy/formula chunks — and the poison **survives the per-call `_ENV` reset**. So isolation
requires, additionally (slice 7.1, T14): **(a)** at VM build, point `L.G.builtinMts[LTString]` at a
**private, engine-owned table holding the T13 capped wrappers, never exposed as a script-reachable
global** — so no Lua value references a mutable shared table at all (a script needing the
`string.`-namespaced form gets a separate **read-only proxy**, not the live table); and **(b) never
register `getmetatable`/`setmetatable`** (closing the `getmetatable("")` reach). NOTE a plain
read-only-`__index` / write-block on the metatable is **insufficient**: in Lua 5.1 `__newindex` fires
only for *absent* keys, so overwriting an existing `rep`/`gsub` is a raw set it never intercepts, and
method syntax `("x"):rep()` resolves through the shared `builtinMts[LTString]` table regardless of
`_ENV`. The robust fix is the unreachable engine-owned table, not a guard on a still-reachable one.
Fresh-`_ENV` + an unreachable-immutable `L.G` shared table together give the isolation §1.1 needs;
fresh-`_ENV` alone does not.

### 1.2 The handle userdata layer (P7-D2, the no-dangling/no-cross-zone guarantee)

A handle is a `*lua.LUserData` wrapping a small Go struct `{rid RuntimeID, zone *Zone, zoneGen uint64}`
with a metatable of curated methods. **Every method first re-resolves** `rid` → `*Entity` in `zone`
(the existing per-zone RID lookup): if the entity no longer exists, left the zone, or the zone changed
generation, the method is a **safe no-op** returning `nil`/`false` — never a panic, never a stale
pointer (LUA.md §4). The `*Entity` is fetched, used, and dropped **within the single Go method call**;
it never lives in a Lua value across calls. A handle for an entity in **another** zone is invalid here
(the zone pointer mismatch / RID-not-found) — cross-zone interaction must go through engine-mediated
events, preserving the single-writer invariant. This is the structural enforcement of "no script can
reach another zone."

---

## 2. The sandbox threat model (the sharpest trust boundary — written for security-auditor review)

Builders/content authors run **arbitrary Lua in-process, on the zone goroutine**. Even with authoring
gated to reviewed authors (P7-D9), the sandbox is **defense-in-depth that must hold against a hostile
author** — the engine never relies on the gate for safety. This section enumerates the attack surface,
the invariant each carries, how it is enforced, and how it is tested. **Every slice that adds surface
must carry its row's mitigation; no slice ships a capability without its gate.**

| # | Attack surface | Invariant | Enforced by | Tested by |
|---|----------------|-----------|-------------|-----------|
| **T1** | **Code loading / FFI / dynamic eval** — `load`/`loadstring`/`dofile`/`loadfile`/`require`/`package`, any FFI. | A script cannot load or eval new code, link a C library, or escape the bytecode it was compiled from. | The VM is built with a **restricted global table** (LUA.md §5): these globals are **never registered** (we build `_ENV` from an allowlist, not by deleting from the stdlib — deletion can be defeated by `_G` aliasing; allowlisting is the safe construction). `package`/`require` never exist. (slice 7.1) | A unit test asserts each forbidden global is `nil` in a fresh script `_ENV`; a test that `load("return 1")` errors `attempt to call a nil value`. |
| **T2** | **Filesystem / network / process reach** — `os` (`execute`/`getenv`/`remove`/`exit`), `io` (`open`/`popen`/`lines`), any socket. | A script has **zero** filesystem, network, or process reach; cannot read env, spawn a process, or exit the host. | `os`/`io` are **not in the allowlist** (T1's construction). `os.time`/`os.clock` (entropy/timing) are also gone (P7-D4); `mud.now()` returns the deterministic pulse counter. (slice 7.1) | A test asserts `os == nil and io == nil`; a test that `mud.now()` is the pulse counter, monotonic, not wall-clock. |
| **T3** | **CPU exhaustion (multi-op)** — a tight loop, a pathological pattern over many ops. | A single entry-point call **cannot stall the zone goroutine**; it is bounded in both VM instructions and wall-clock. | The **dual budget** (P7-D6): the **vendored-fork instruction-count abort** (in `mainLoopWithContext`, ~1k granularity — gopher-lua v1.1.1 has **no** `MaskCount`/`SetHook`, so the count comes from the fork, not a debug hook) aborts past the per-call instruction budget; the `LState` **context deadline** aborts past the wall-clock limit. Both raise a Lua error caught by the engine `pcall`, log, and count against the error budget (T11, weighted per P7-D10). **Single-op bombs are NOT covered here — see T13.** (slice 7.1 fork + 7.5) | A test that a `while true do end` body aborts within the deadline and the **zone keeps serving** (a second command on the same zone succeeds after); a test that an instruction-heavy-but-fast loop trips the **count** budget (not the clock); a benchmark asserting fork overhead <2%. |
| **T4** | **Stack / recursion blowout** — deep or infinite Lua recursion overflowing the Go goroutine stack. | Deep recursion **errors out** (a catchable Lua error), never overflows the host goroutine stack (which would crash the process). | The constructor option **`lua.Options{CallStackSize: N}`** caps the Lua call stack (gopher-lua v1.1.1 has **no** `SetMaxStackSize` — LUA.md §5 is wrong; the cap is a build-time `Options` field, paired with `RegistrySize`/`RegistryMaxSize` for the value stack); the overflow is a Lua error caught by `pcall`. (slice 7.1, set at VM build) | A test that a self-recursive Lua function errors cleanly and the zone survives; the recursion does **not** SIGSEGV the test binary; a test that `CallStackSize` is set on the constructed VM. |
| **T5** | **Memory exhaustion (gradual)** — allocating many tables/strings over a call; gopher-lua has **no hard per-VM memory cap** (the acknowledged limitation). | Gradual growth is **bounded indirectly and DETECTED observably** — never a silent gap; a single-op allocation bomb is T13. | Layered: the **instruction budget** (T3) caps total work for **multi-op** growth; **`self.state` size/depth/key caps** (P7-D5) bound persisted growth; the **per-zone VM memory metric is DETECTION-ONLY** (it fires **after** the fact — it cannot prevent an allocation, only alert + inform a kill decision); the **single-op bomb is prevented by the capped builtins (T13)**, not by this row. We document the soft-cap explicitly, not silently. (slice 7.1 caps + 7.5 metric + 7.6 state caps) | A test that a `state` table over the byte/depth/key cap is rejected at save (clean error, no balloon); a metric assertion that VM memory is reported per zone; (the prevention test lives in T13). |
| **T6** | **Zone-goroutine starvation** — blocking, sleeping, or spawning. | A script **never blocks** the single-writer loop: no real sleep, no I/O wait, no goroutine, no channel op. | No blocking primitive is in the allowlist (T1); `mud.after(pulses, fn)` schedules on the **zone timer wheel** (pulse.go), not the OS scheduler, and returns control immediately; there is no `mud.spawn_goroutine`, no `mud.sleep`. A script needing zone-absent data **returns**; the engine fetches async and re-invokes. (slice 7.3 for `mud.after`) | A test that `mud.after` schedules on the wheel and the callback runs on the zone goroutine (not a new goroutine — assert via a goroutine-id capture / no race under `-race`); a test that no sleep primitive exists. |
| **T7** | **Cross-zone reach** — a handle smuggled to act on an entity in another zone, or a handle held across the entity's zone change. | A handle is **invalid outside its zone**; any method on a cross-zone or departed entity is a safe no-op. No `*Entity` ever crosses a goroutine. | The handle re-resolves `(rid, zone, zoneGen)` **in Go on every method** (P7-D2/§1.2); a mismatch (entity gone / moved / zone-gen changed) returns `nil`/`false`. The `*Entity` lives only within the Go method call. (slice 7.2) | A test that a handle for a moved-away entity no-ops; a test that a handle captured in `self.state`-adjacent Lua and reused after the entity left the zone does not act and does not panic; a `-race` test that no method dereferences a foreign-zone entity. |
| **T8** | **Hostility / PvP gate bypass** — a Lua effect op harming a protected player without the gate. | **Every** Lua harm vector funnels the **same** `guardHarmful` — a Lua op is not a gate bypass. The gate is at the op, not the API call site. | Lua effect handles call the **existing** `dealDamage`/`applyDebuff`/`guardCrossPlayerWrite` (P7-D3) — there is no Lua-specific harm path; the binding constructs the `effectCtx` (the **five binding invariants**, P7-D3) and the funnel does the gate, fail-closed on a detached actor/target (effect_op.go guardHarmful:252). The "funnels are the only write path" check is a **build-failing CI lint**: any Lua handle method touching `*Entity.living`/affects/flags outside the funnels (incl. `h:set_flag` + any future direct-mutator on a deny-list) **fails the build** — not a grep that can rot. (slice 7.3c) | A test that a Lua `h:damage{}` against a protected player in a safe room is a **clean no-op** (the existing combat-test pattern, now driven from Lua); a test that a Lua bus handler's harmful op is gated **per target**; a Lua `h:apply_affect{source=arbitrary}` cannot spoof attribution; the **build-failing lint** flags a direct-mutator. |
| **T9** | **Determinism / entropy injection** — a script seeding non-reproducibility (wall-clock, Lua RNG state, goroutine timing). | A script's only randomness is the **per-zone seeded engine RNG**; no other entropy source. | `math.random` rebound + `mud.random`/`mud.roll` draw the ctx RNG; `os.time`/`os.clock` absent (T2); `mud.now()` is the pulse counter (P7-D4). (slice 7.3) | A seeded-zone test that two runs of the same scripted ability produce identical rolls; a test that no wall-clock/entropy primitive is reachable. |
| **T10** | **State injection via `self.state`** — persisting code, handles, or an unbounded blob to corrupt load or balloon the snapshot. | `self.state` is **data-only** and **size-bounded**; loading it can never execute code or resurrect a stale pointer. | The marshaller (P7-D5) allowlists number/string/bool/nested-table **only** (functions/closures/userdata/handles rejected at save) and enforces the byte/depth/key caps; load reconstructs a plain table — never a handle (content stores `h:id()` and re-resolves). (slice 7.6) | A test that a `state` carrying a function/handle is rejected at save with a clean error (not a panic, not a silent drop of the rest); a round-trip test that a nested data table survives save/load identically; a cap-exceeded test. |
| **T11** | **Buggy-script blast radius** — a script that errors (or trips a budget) repeatedly. | One bad script **fails just its action** and, if chronic, **disables itself** — never the zone, never the world; the player **never sees the raw error/stack**. | `pcall` isolation + the **error-budget circuit breaker** (P7-D10): an error/abort fails that action with a **generic player-facing message** (the raw Lua error + stack go to **ops logs only** — T15), logs `(zone,kind,ref,stack)`, increments the budget (deadline aborts weighted separately, P7-D10); tripping it disables the script (per-instance or per-`(kind,ref)`, P7-D10) and alerts ops; reset on the next successful reload. (slice 7.5) | A test that an always-erroring trigger fizzles with a **generic** message (no Lua error text leaked to the player), the zone serves the next command, and after N failures the breaker disables it (and a reload re-enables it); a test that a deadline abort doesn't quarantine as fast as a logic error. |
| **T12** | **Reaction-hook over-reach** — a Lua result-altering reaction reaching past the checkpoint's exposed fields (P7-D8). | A reaction hook can mutate **only** the checkpoint's published, typed fields, **re-gated**, under the **shared cascade budget** — not arbitrary pipeline/engine state. | The reaction context object (P7-D8): `modify(field,…)`'s `field` is a **closed per-checkpoint enum resolved by a Go switch** (never a string indexing an attr map); `replace_target(h)` **re-runs `guardHarmful` against the new target**; the path threads the **same `eventBudget`** (effect_op.go:56) so a reaction loop is width+depth bounded. The engine applies only recorded mutations at the seam (observe-then-recheck); no raw pipeline pointer reaches Lua. (slice 7.9) | A test that a non-allowlisted `modify` field is a no-op; a test that `replace_target` onto a non-consenting player is **gate-blocked**; a test that Counterspell `rx:cancel()` cancels exactly the cast and nothing else; a reaction-loop budget-exhaustion test. |
| **T13** | **Single-builtin alloc/CPU bomb** — `string.rep("A", 2e9)` (GB in ONE instruction, probed), `string.format`/`gsub`/`table.concat` width blowups, pathological `string.find`/`match` backtracking. | A single Go-builtin call **cannot allocate unbounded memory or burn unbounded CPU** in one op. | **The deadline checks BETWEEN bytecode ops, never INSIDE a Go builtin, and the instruction count sees it as ONE op — so neither the clock nor the count catches it (probed false on T5's old claim).** Mitigation: **never expose the raw stdlib amplifiers** — ship **length/width/size-capped wrappers** for `string.rep`/`string.format`/`string.gsub`/`table.concat`, and **guard pathological backtracking** in `string.find`/`match` (cap pattern complexity / input length). (slice 7.1 — the capped builtins ARE part of the allowlist construction) | A test that `string.rep("A", 2e9)` is **rejected at the cap** (clean error, no GB allocation); per-wrapper cap tests (`format`/`gsub`/`concat`); a backtracking-pattern test that a known-pathological `match` is bounded. |
| **T14** | **Shared-`L.G` writable-metatable poison** — `string.rep = evil` (the kept `string` global == `L.G.builtinMts[LTString]`) poisons `("x"):rep()` for **every** script in the zone (incl. trusted policy/formula) via method syntax, regardless of `_ENV`, and **survives the per-call `_ENV` reset** (the §1.1 fresh-`_ENV` claim is incomplete — it doesn't cover `L.G`-scoped state). | The shared `L.G.builtinMts[LTString]` table (what method syntax `("x"):m()` dispatches through) is **engine-owned and unreachable** by any script — no Lua value references a mutable copy of it. | At VM build, set `L.G.builtinMts[LTString]` to a **private engine-owned table** holding the **T13 capped wrappers**, **never exposed as a script-reachable global** (scripts get a separate **read-only proxy** for the `string.` form). **Drop `getmetatable`/`setmetatable`** (closes `getmetatable("")`). A read-only-`__index`/`__newindex` guard ALONE is **insufficient** (Lua-5.1 `__newindex` skips existing-key overwrites; method syntax bypasses `_ENV`) — the table must be *unreachable*, not merely guarded. (slice 7.1) | A test that `getmetatable` is `nil`; the **load-bearing test**: a script doing `string.rep = evil` (and any other reach attempt) **cannot change** what `("x"):rep(2)` returns in a **sibling** script — **cross-script method-syntax invariance**, the path that actually bites. |
| **T15** | **Info leak via `tostring`** — `tostring(userdata)` returns the live Go pointer `0x…` (ASLR defeat); raw Lua errors echoing internals to players. | A script **cannot read a Go pointer** or any host-internal address through a handle; players never see engine internals. | **Every handle metatable defines `__tostring`** returning a safe `<entity #rid>` — **no userdata is ever exposed without one** (the default gopher-lua `tostring(ud)` leaks the pointer). Player-facing fizzle messages are generic (T11); raw errors/stacks go to ops logs only. *(7.1 review: bare `tostring(function)`/`tostring(table)` ALSO leak Go pointers — same ASLR-leak class as handles. 7.5's player-facing value-render path must sanitize any `tostring` output reaching a player, not just handle `__tostring` and error strings.)* (slice 7.2 for handles; 7.5 for messages) | A test that `tostring(self)` is `<entity #rid>`, **never** `0x…`; a test that no handle type lacks `__tostring`; a test that a player-facing error/value-render carries no `0x…` pointer. |

**Construction note (the load-bearing detail for T1/T2/T13/T14):** the sandbox `_ENV` is **built from
an allowlist by registering the kept base functions individually** — **NOT** `lua.OpenBase` and **NOT**
`NewState()`-then-delete. `OpenBase` registers `load`/`loadstring`/`dofile`/`loadfile`/`require`/
`module`/`collectgarbage`/`getmetatable`/`setmetatable`/`rawget`/`rawset`/`rawequal`/`next`/`_G`/
`newproxy` **all at once** — exactly the set we must withhold (T14's `getmetatable`/`setmetatable`
write path among them). Deleting after the fact is defeatable (`_G`/`_ENV` aliasing, a kept function
re-exposing a removed one); registering individually means an unsafe capability is *absent*, not
*hidden*. The amplifier builtins (`string.rep`/`format`/`gsub`/`concat`, `find`/`match`) are registered
as **capped wrappers** (T13), never the raw stdlib versions. The kept/dropped sets are enumerated in
**slice 7.1's absence test (§ Allowlist)**. **security-auditor signs off on the allowlist + the capped
wrappers + the frozen string metatable before 7.1 lands.**

### 2.1 Allowlist — keep / drop (slice 7.1's absence test asserts the full DROP set)

**KEEP (register individually, not via `OpenBase`):** `assert`, `error`, `pcall`, `xpcall`, `select`,
`type`, `tostring` (handles supply `__tostring`, T15), `tonumber`, `pairs`, `ipairs`, `unpack`/
`table.unpack`, `print`→`mud.log`; **tables:** `string` (`rep`/`format`/`gsub`/`find`/`match` → **capped
wrappers**, T13), `table` (`concat` **capped**), `math` (`random`/`randomseed` **rebound to the zone
RNG**; `randomseed` ideally a **no-op**, T9).

**DROP / never-register (the absence test must assert ALL):** `load`, `loadstring`, `dofile`,
`loadfile`, `require`, `module`, `package`, `collectgarbage`, `getmetatable`, `setmetatable`, `rawget`,
`rawset`, `rawequal`, `rawlen`, `next`, `_G`, `setfenv`, `getfenv`, `newproxy`, `os`, `io`, `debug`,
`coroutine`, `channel` (gopher-lua's goroutine primitive — T6), `string.dump`, and `math.randomseed`
as an entropy reset (no-op it). This corrects the earlier draft's list, which omitted
`getmetatable`/`setmetatable`/`rawset`/`setfenv`/`getfenv`/`newproxy`/`collectgarbage`/`module`/`next`/
`_G`/`coroutine`/`channel`/`string.dump`.

---

## 3. Resolving the three open design choices (LUA.md §10)

LUA.md line 8 promises "three choices flagged in §10," but the doc ends at §9 — the forks were never
written. The three real open forks, and the resolution for each:

**Fork 1 — Budget enforcement mechanism & defaults (P7-D6) — RESOLVED (user decision, 2026-06).**
*Decision:* a **three-layer budget**, because the security-auditor's probes against the real
gopher-lua v1.1.1 falsified the simpler designs:
1. **A vendored gopher-lua fork** adds the **instruction-count abort** — gopher-lua v1.1.1 has **no**
   `SetHook`/`MaskCount`/debug-hook at all (the earlier draft's `lua.MaskCount` does not exist). The
   fork adds the count check **in `mainLoopWithContext`, beside the existing `ctx.Done()` select** —
   deterministic and test-reproducible (a pure-CPU loop trips it identically every run). Vendor
   `github.com/yuin/gopher-lua`, **pin + document the fork**; this is **slice 7.1's first work item**.
2. **The `LState` context wall-clock deadline** (`SetContext` + `context.WithTimeout`) catches a
   low-instruction stall (a GC pause, a slow C-side call). It checks **between** bytecode ops only.
3. **Capped amplifier builtins** (T13) catch the **single-op bomb** — `string.rep("A", 2e9)`
   allocates GB in **one** instruction (probed), which neither the count (one op) nor the clock
   (no between-op check inside a builtin) can stop. So `string.rep`/`format`/`gsub`/`table.concat`
   ship as size-capped wrappers; the raw stdlib versions are never exposed.

*Defaults:* **5ms wall-clock, 100k VM instructions** per entry-point call, tunable per pack and per def;
fork overhead <2% at ~1k-op granularity. All three abort *that call only*, are caught by `pcall`, and
feed the error budget (deadline aborts weighted separately, P7-D10). *Rejected alternatives:* a debug
hook (does not exist in v1.1.1); wall-clock alone (non-reproducible in tests; misses both the
single-op bomb and a tight loop that re-burns the deadline every call); a goroutine-watchdog
preempting the VM (cross-goroutine `LState` access — violates the single-writer invariant, gopher-lua
is not goroutine-safe).

**Fork 2 — In-flight `mud.after` callbacks across a hot reload (P7-D7).**
*Recommendation:* **drop by generation tag** as the default; an explicit `mud.after{durable=true}`
opt-in for a state-cleanup finalizer. *Why:* the reload hazard is running **old code against new
state**; a timer closure compiled against the pre-edit chunk may assume a `self.state` shape the new
chunk changed. Dropping is the safe default (the edit "starts fresh"); the rare legitimate
finalizer (release a held resource, clear a flag) gets the opt-in. Mirrors the prototype reloader's
"live instances keep the old, next spawn uses the new" semantics — here, "in-flight old-gen timers
drop, new invocations use the new chunk." *Rejected:* always-complete (runs stale code against new
state — the corruption class); always-drop with no opt-in (loses a legitimate finalizer use).

**Fork 3 — Result-altering reactions reaching into an in-flight action (P7-D8).**
*Recommendation:* a **typed, bounded reaction context object** passed to the Phase-6 named checkpoints
(`BeforeCastCommit`, the to-hit checkpoint, `OnDamageTaken`), exposing a small mutation allowlist
(`cancel`/`modify(field,delta)`/`replace_target`/`consume_resource`); the engine fires the checkpoint,
runs the Lua hook **synchronously inline** on the zone goroutine, then **re-reads** the recorded
mutations and applies them at the seam — the **observe-then-recheck** shape the `on_depleted` death
checkpoint already implements (PRINCIPLES.md: the reference before-checkpoint). Three hardening
invariants make the surface auditable (T12): `modify`'s `field` is a **closed per-checkpoint enum
resolved by a Go switch** (to-hit `{"ac"}`, `OnDamageTaken` `{"amount"}` — never a string indexing an
attr map); `replace_target` **re-runs `guardHarmful` against the new target** (the original gate ran
against the original — replacing onto a non-consenting player would otherwise bypass it); the path
threads the **same `eventBudget`** so a reaction→checkpoint→reaction loop is width+depth bounded.
*Why:* Phase 6 deliberately built the checkpoints so Phase 7 adds **hook bodies, not pipeline
surgery**; a typed reaction object (not a raw pipeline pointer) keeps the alter-surface auditable and
single-writer. Counterspell/Shield/concentration all express on this one shape. *Rejected:* handing Lua a raw mutable
pipeline struct (unbounded reach, un-auditable, T12 violation); a post-hoc "undo" model (the action
already had side effects — can't cleanly rewind).

---

## 4. Integration constraints (binding)

- **No new concurrency.** The `LState` is constructed at zone build and called **only** from
  `Zone.Run`'s goroutine (the existing single-writer loop). No goroutine touches it; no lock guards it
  (gopher-lua is not goroutine-safe — and we never need it to be). Lua callbacks (`mud.after`,
  bus handlers, reactions) all run inline on that goroutine. This is the Phase 6 actor-model contract,
  unchanged.
- **Handles never hold `*Entity`.** A Lua value wraps `(RuntimeID, zone, zoneGen)`; the `*Entity` is
  resolved, used, and dropped inside each Go method call (§1.2). This is what makes "no dangling, no
  cross-zone" structural rather than disciplinary.
- **Harm reuses the funnels — no parallel path.** Lua effect ops call `dealDamage`/`applyDebuff`/
  `guardCrossPlayerWrite` (effect_op.go), which call `guardHarmful` first. The binding's job is to
  build a correct `effectCtx` **holding the five binding invariants (P7-D3)** — actor/source/target
  engine-resolved (never script-supplied), `disp` engine-set, the funnels the only write path
  (build-failing lint), `rng` always the ctx/zone RNG, `depth`/`eventBudget` threaded never reset.
  The funnel owns the gate. No Lua-specific damage/affect write exists.
- **One budget chokepoint arms a fresh deadline per call — for EVERY Lua-invoking path.** The
  `LState` context deadline survives inner `pcall` **only if a fresh `context.WithTimeout` is set
  before every Lua entry and cleared after** — a stale/cancelled context makes the **next** call fail
  instantly. The binding invariant: **there is no Lua-invoking path that does not pass through the one
  chokepoint that does `SetContext(fresh) → run → RemoveContext`.** This explicitly includes
  **`mud.after` timer callbacks, reaction hooks, and bus handlers** — not just top-level triggers
  (each is a fresh entry needing its own fresh deadline). The vendored instruction-count budget is
  re-armed at the same chokepoint. **DOUBLY load-bearing (7.1 security review):** the default gopher-lua
  loop is the plain `mainLoop` (no count, no deadline); only `SetContext` swaps to `mainLoopWithContext`
  where **both** layers live — so a path that forgets `SetContext` silently loses the budget *and* the
  deadline (a runaway runs unbounded), not just the deadline. Therefore 7.5 must make a `runChunk`-style
  private method the **SOLE** way to enter Lua (no raw `L.PCall`/`L.Call` reachable from engine code
  outside it — enforce with a build-failing lint like the T8 funnel check), and add a test that a
  budget-armed call with no context is impossible by construction. (7.1's single `runChunk` already does
  this correctly for its one caller — 7.5 generalizes + locks it.)
- **Determinism via the per-zone engine RNG.** The binding threads `effectCtx.rng` into every
  Lua-reachable random draw; `math.random` is rebound; no other entropy is exposed (P7-D4).
- **Hot reload rides the existing content-invalidation path.** The reloader (reload.go) recompiles the
  Lua chunk on the same `(kind, ref)` bus invalidation it already handles for prototypes, bumps the
  gen, and swaps via the existing atomic registry swap. `self.state` data survives; old-gen timers
  drop (P7-D7).
- **The bus budget is shared.** A Lua bus handler runs under the **same** `maxEventDepth`/
  `maxEventHandlers` budget (event.go) as a declarative op-list handler; a Lua reaction increments the
  same depth and decrements the same width budget. Lua adds no new cascade-bounding surface — it reuses
  Phase 6's.
- **Cross-zone consequences are reserved (Phase 10).** A Lua handler needing a cross-zone effect
  enqueues for the (Phase-10) director — a no-op reservation now, exactly like the declarative path.

---

## 5. Slicing (ordered, independently committable)

The spine is **VM + sandbox → handles → API surface → entry points → safety → state → hot reload →
hookability obligations → escape-hatch cases**. Smallest-first, each a commit with the prior phase's
tests green and its owning + cross-cutting reviewers signing off ([subagent-review-after-every-step]).
The **security-auditor reviews every slice that adds sandbox surface** (7.1 — the vendored-fork abort,
the allowlist construction, the capped builtins, the frozen string metatable; 7.2 — `__tostring`; 7.3c
— the harm funnels; 7.5 — the budget chokepoint + breaker; 7.6 — the `state` marshaller; 7.9 — the
reaction mutation allowlist + re-gate) — the threat-model row each slice carries is the review checklist.

| Slice | Scope | Done when | Tests added |
|-------|-------|-----------|-------------|
| **7.1 — Vendor the fork + VM lifecycle + the restricted-globals sandbox** | **First work item: vendor `github.com/yuin/gopher-lua`** — add the instruction-count abort in `mainLoopWithContext` beside the existing `ctx.Done()` select; pin + document the fork (P7-D6 layer 1). Then: construct one `*lua.LState` per zone via **`lua.Options{CallStackSize, RegistrySize/RegistryMaxSize}`** (T4 — the recursion/value-stack caps are build-time options, **not** `SetMaxStackSize`), torn down on stop, called only from `Run`. The **allowlist-built `_ENV` by registering kept base functions individually — NOT `lua.OpenBase`** (which bundles `load`/`require`/`getmetatable`/`setmetatable`/… — T1/T14); the **capped amplifier builtins** (`string.rep`/`format`/`gsub`/`find`/`match`, `table.concat` — T13) instead of the raw stdlib; **drop `get/setmetatable`** (T14); `math.random` rebound, `randomseed` no-op (T9); `print`→`mud.log`. **The shared `L.G.builtinMts[LTString]` points at the private capped-wrapper table, unreachable as a script global — NOT a `__newindex`-guarded still-reachable table (T14).** **No handles, effect ops, or entry points yet.** | A zone boots with a live VM (`CallStackSize` set); a sandboxed `print("hi")` reaches the log; the **§2.1 DROP set is asserted absent in full** (incl. `getmetatable`/`setmetatable`/`rawset`/`setfenv`/`getfenv`/`newproxy`/`collectgarbage`/`module`/`next`/`_G`/`coroutine`/`channel`/`string.dump`); `load(...)` errors; `string.rep("A",2e9)` is capped (T13); `getmetatable("")` is `nil`, and **a script setting `string.rep = evil` does NOT change a sibling script's `("x"):rep(2)`** (T14 cross-script method-syntax invariance). Bare-zone-unchanged. | **§2.1 DROP-set absence test (T1/T2/T14, security)**; **capped-builtin tests (T13)**; **cross-script-method-syntax-invariance test (T14, the load-bearing one)**; `CallStackSize`-set test (T4); allowlist-present + `randomseed`-no-op test; `print`→log + bare-zone-unchanged tests; vendored-fork-pinned check. All Phase 1–6 green. |
| **7.2 — The handle userdata layer** | The `(rid, zone, zoneGen)` userdata + metatable **with a `__tostring` returning `<entity #rid>` — never the raw Go pointer** (T15: default `tostring(ud)` leaks `0x…`, an ASLR defeat); the **re-validate-every-method** Go path (§1.2); the identity/query read methods (`h:id`/`h:name`/`h:short`/`h:attr`/`h:resource`/`h:level`/`h:has_affect`/`h:affect_magnitude`/`h:has_flag`/`h:room`); `self` bound in a trivial trigger context. **Read-only — no effect ops, no harm surface yet.** | A trigger script reads `self:name()`/`self:attr("str")`; `tostring(self)` is `<entity #rid>`, **never** `0x…` (T15); a handle for a **moved-away** entity no-ops (returns `nil`); a handle for an entity in **another zone** is invalid here; no method holds an `*Entity` across the call. | handle-resolve + no-dangling test (**T7**); cross-zone-invalid test (**T7**); **`__tostring`-no-pointer test (T15, security)**; each read-method test; `-race` test that no foreign-zone deref occurs. |
| **7.3 — The curated API surface (3 sub-slices, incremental)** | **7.3a identity/query + traversal:** `h:contents`/`h:equipment`/`h:group`/`h:is_enemy`/`h:distance`/`h:can_see` (handle-returning traversal); the comms ops `h:send`/`h:act`/`h:say`/`h:emote` (no harm). **7.3b the `mud.*` world/util table:** `mud.random`/`mud.roll` (ctx RNG — **T9**), `mud.now` (pulse counter — **T2/T9**), `mud.log`, `mud.scan`/`mud.broadcast`, `mud.spawn`/`mud.transform`/`mud.summon`, `mud.after`/`mud.cancel` (zone-wheel scheduling — **T6**). *(7.2 review: the T15 "no bare userdata without `__tostring`" invariant extends to ANY new userdata 7.3 exposes — esp. `mud.after`'s timer handles — re-run the no-pointer-leak check on them.)* `mud.pvp_allowed`. **7.3c the effect-op handles (the harm surface):** `h:damage{}`/`h:heal`/`h:modify_resource`/`h:drain`/`h:apply_affect`/`h:remove_affect`/`h:dispel`/`h:move`/`h:teleport`/`h:recall` — **each routing the existing `dealDamage`/`applyDebuff`/`guardCrossPlayerWrite` funnels** (P7-D3, **T8**). | 7.3a: a script greets a room (`h:act`) and walks its `h:contents()`. 7.3b: `mud.after(2, fn)` fires on the zone wheel on the zone goroutine (not a new goroutine); two seeded runs roll identically. 7.3c: a Lua `h:damage{}` against a **protected player in a safe room is a clean no-op** (gate held); a Lua buff on self attaches; harm funnels the same gate as a declarative op. | 7.3a traversal/comms tests; 7.3b `mud.after`-on-wheel + goroutine-id test (**T6**), seeded-RNG determinism test (**T9**), `mud.now`-pulse test (**T2**); **7.3c gate-held-from-Lua test (T8, security)**, funnel-reuse audit-grep test, per-target gate test. |
| **7.4 — Entry points (Lua handler bodies)** | Wire the reserved Lua columns to **run**: ability `on_resolve` in Lua (defs.go `onResolveLua`, ability_build.go — now executed, not read-not-run); affect `on_apply`/`on_tick`/`on_expire`/`on_dispel` (defs.go reserved hooks); **triggers** `on(event, fn)` (room/mob/item `enter`/`leave`/`speech`/`get`/`give`/`attack`/`death`/`tick`/`reset`/`greet`); **custom commands** (content registers a verb implemented in Lua, into the command table); **formulas** (`to_hit`/`soak`/`regen`/`xp_for` as a Lua function alternative to the prefix-AST); the **`pvp_allowed(actor, target)` policy hook** in Lua. Each invoked through `pcall` + the (still-default, un-budgeted) sandbox. **Bind `self`/`ctx`/`ev`/`rx` in the per-call FRESH `_ENV` (§1.1), NOT a shared global** *(7.2 review: 7.2's standalone `runChunkWithSelf` set `self` as a `defer`-cleared global — correct for one call, but a real entry point firing a reaction mid-handler could observe a stale `self`; the entry binding must ride the fresh `_ENV`).* **Lua bus handlers** ride the Phase-6 bus (a Lua body where an op-list sat). | A mob's `on("greet", …)` greets a player by name and remembers via `self.state`; a mob's `on("speech", …)` reacts to "amulet"; a Lua `on_resolve` composes effect ops; a custom `dance` verb runs; a Lua `pvp_allowed` policy decides a fight; a Lua handler on `OnHit` builds a resource. | per-entry-point invocation tests (trigger/on_resolve/affect-hook/custom-command/formula/pvp-policy); Lua-bus-handler test (rides the depth/width budget); `pcall`-isolation smoke (a bad body fizzles, zone serves on). |
| **7.5 — The budget chokepoint + circuit breaker + error isolation** | The **one chokepoint** (§4): `SetContext(fresh deadline) → re-arm the vendored instruction count → run → RemoveContext`, wrapping **EVERY** Lua-invoking path — top-level triggers, **`mud.after` callbacks, reaction hooks, AND bus handlers** (a stale cancelled context fails the next call). The vendored count (7.1) + the deadline together are P7-D6 layers 1–2 (layer 3, the builtin caps, landed in 7.1). The **error-budget circuit breaker** (P7-D10, **T11/T15**): **per-instance for entity-scoped scripts, per-`(kind,ref)` for shared defs** (shared-def blast radius documented); **deadline aborts weighted/rate-limited separately from logic errors** (no latency-induced false quarantine); a **generic player-facing message**, raw error/stack to ops logs only; reset-on-reload. The per-zone VM memory **metric (detection-only, T5)**. *(7.3b reviews: also add a **per-zone live-`mud.after`-timer cap** — a callback that schedules ≥1 timer each fire grows the wheel unboundedly across ticks, bounded by neither the per-call budget nor the spawn cap; and revisit the **per-zone spawn cap**, currently monotonic-since-build — give it a despawn-census or reset-on-zone-reset so legitimate long-lived spawn content can't permanently wedge `mud.spawn`.)* | A `while true do end` trigger **aborts within the deadline and the zone keeps serving**; **an `mud.after` callback AND a bus handler are each deadline-bounded** (not just a top-level trigger); deep recursion errors cleanly (T4's `CallStackSize`, set in 7.1); a deadline trip doesn't quarantine as fast as a logic error; after N failures a script is **disabled** and a reload re-enables it; no raw Lua error leaks to a player; VM memory is reported. | **chokepoint-arms-fresh-deadline-per-call test incl. `mud.after` + bus handler (T3, security)**; count-vs-clock test; **breaker trip + reload-reset test, per-instance vs shared-def (T11)**; **deadline-vs-error weighting test**; generic-message test (T15); overhead benchmark (<2%); memory-metric test. |
| **7.6 — `self.state` ↔ persisted JSONB** | The data-only Lua-table↔JSON marshaller (P7-D5, **T10**): the type allowlist (number/string/bool/nested-table), the **byte/depth/key-count caps**, rejection of functions/handles/userdata at save; `self.state` mirrored into a new `StateJSON.Script` field (character.go), serialized on the **existing** save cadence, re-hydrated by `loadCharacter` into a plain table. Mob/item script state rides the same path where those entities persist. | A scripted mob's quest counter in `self.state` **survives logout/login** (and a crash-rehydrate); a `state` carrying a function/handle is **rejected cleanly at save** (no panic, no silent partial drop); an over-cap `state` is rejected; a nested data table round-trips identically. | **state round-trip test (T10)**; **reject-code/handle test (T10, security)**; cap-exceeded test; crash-rehydrate test; cadence-integration test (rides the existing ladder). |
| **7.7 — Hot reload (recompile + swap handlers)** | **MUST-FIX from the 7.4 review (load-bearing):** today `chunkFor` (luaentry.go) caches a compiled chunk **by key only, ignoring the source/gen** — so a source edit silently REUSES the stale chunk and hot-reload is a NO-OP (and for `pvp_allowed` specifically, a permissive→restrictive policy edit keeps using the permissive one — a security hazard). 7.7's first job: make `chunkFor` **source/gen-aware** (key on a source hash, or honor a `chunkGen` bump the reloader sets) so the swap actually takes. The reloader (reload.go) recompiles the Lua chunk on the **existing** `(kind, ref)` content-bus invalidation, bumps the chunk **generation**, and swaps the proto via the existing atomic registry swap; `self.state` data survives (it's not code); **old-gen `mud.after` callbacks drop** (P7-D7), with the `durable=true` opt-in honored; a compile error keeps the last-good proto (non-fatal, like the prototype reloader); the circuit breaker resets on a successful reload. | Editing a mob's Lua greeting **reloads live** (no restart) and the next greet uses the new text while `self.state` (who's been greeted) persists; an in-flight old-gen `mud.after` timer **drops**; a `durable=true` finalizer **completes**; a syntactically-broken edit keeps the old behavior + logs. | live-reload-swaps-handler test; **state-survives-reload test**; **old-gen-timer-drops test (P7-D7)**; durable-opt-in test; compile-error-keeps-last-good test; breaker-reset-on-reload test. |
| **7.8 — The hookability obligations (custom-event lane + reserved-kind lighting)** | **(a) The content-namespaced custom-event lane** (PRINCIPLES.md pillar 2, ROADMAP): the closed `knownEventKinds` map (event.go) grows a **`pack:event` lane** — builders `mud.fire("pack:OnShipDock", subject, data)` and subscribe `on("pack:OnShipDock", fn)`; still **depth/width-budgeted and gate-funneled** like an engine event, **no privileged status**; namespaced by pack to avoid collision. **(b) Light the reserved engine kinds** whose owners exist by now (event.go consts already named, reserved): `OnApplyAffect`/`OnAffectTick`/`OnAffectExpire` fire from the affect runtime (affect_runtime.go), and a **new `OnEnter`** movement hook fires from the move path — so "a missing hook is an engine bug" holds. (Cross-phase kinds — `OnRest`/`OnLevelUp`/`OnLogin` — stay owned by their phase.) | A pack **defines, fires, and handles** a `pack:OnShipDock` event the engine has **never heard of** — a sailing system's quest hooks it, all in content; the custom fire obeys the depth/width budget and any harmful op in its handler funnels the gate. `OnApplyAffect`/`OnAffectTick`/`OnAffectExpire` and `OnEnter` fire to content/Lua handlers. | custom-event fire+subscribe test; **custom-event budget + gate test (security)**; pack-namespacing/collision test; reserved-kind lighting tests (apply/tick/expire/enter); unknown-kind-still-lints test. |
| **7.9 — The documented escape-hatch cases** | The result-altering reaction model (P7-D8, **T12**): the typed **reaction context object** at the Phase-6 checkpoints — `rx:cancel`/`rx:modify(field,delta)`/`rx:replace_target(h)`/`rx:consume_resource`, with the **three hardening invariants**: `field` a **closed per-checkpoint enum resolved by a Go switch** (to-hit `{"ac"}`, `OnDamageTaken` `{"amount"}`), `rx:replace_target` **re-runs `guardHarmful` against the new target**, and the path threads the **same `eventBudget`** (effect_op.go:56). **Counterspell** (`rx:cancel()` on `BeforeCastCommit`), **Shield** (`rx:modify("ac", +5)` on to-hit), **concentration** [G11] (a concentration affect `rx:cancel()`s itself on a failed `OnDamageTaken` save), the 5e **multiclass spell-slot table** [G7] (a Lua formula over multiple class levels). | A Lua **Counterspell cancels an in-flight cast** (observe-then-recheck); Shield raises AC for the triggering swing only; concentration drops on a failed save; multiclass slots compute correctly. A **non-allowlisted `modify` field is a no-op**; `replace_target` onto a **non-consenting player is gate-blocked**; a reaction loop is **budget-bounded**. | Counterspell/Shield/concentration/multiclass tests; **non-allowlisted-field no-op test (T12, security)**; **`replace_target` re-gate test (T12, security)**; **reaction-loop budget-exhaustion test**. |

**Adjustment / justification.** 7.1–7.2 land the **smallest, riskiest** thing first (the VM + the
sandbox skeleton + the no-pointer handle layer) so the security-auditor reviews the trust boundary
**before** any capability hangs off it — the allowlist and the re-validate-every-method path are the
foundation everything else trusts. 7.3 adds capability **incrementally**, with the **harm surface
(7.3c) last in its trio** and explicitly gated. 7.5 (budgets/breaker) lands **after** the entry points
(7.4) so there is real script work to bound, but **before** the phase is considered safe — every entry
point then runs under the full budget. 7.8 (the hookability obligations) and 7.9 (the escape-hatch
cases) are last because they depend on the full API + the bus integration. **If 7.3 proves large**,
its three sub-slices ship as three commits (recommended). **If 7.9 proves large**, split the
reaction-context mechanism (Counterspell/Shield/concentration) from the multiclass-slot formula (it
depends only on the Lua-formula entry point from 7.4, not the reaction model).

---

## 6. Schema + loader integration

Phase 7 is **light on new tables** — most of it is wiring **existing reserved columns** to run and
adding a **Lua-source tail** to def bodies (additive JSONB, the established pattern — **persistence-
engineer to confirm**).

- **The Lua runtime dependency is a VENDORED fork of `github.com/yuin/gopher-lua`** (P7-D6 / slice
  7.1), not the upstream module: the instruction-count abort (in `mainLoopWithContext`) does not exist
  upstream and must be carried in-tree, pinned + documented. The build references the vendored path;
  the fork's delta is kept minimal (one abort beside the existing `ctx.Done()` select) so rebasing on
  an upstream release stays cheap.
- **Reserved columns become live (no migration):** `ability_def.on_resolve_lua` (already carried,
  ability_build.go `onResolveLua`), `affect_def`'s `on_apply`/`on_tick`/`on_expire` Lua hooks (defs.go
  reserved `onApply`/`onExpire`). These exist; 7.4 compiles + runs them.
- **New JSONB tails (additive, no `ALTER`):** a `lua` / `triggers` block on room/mob/item def bodies
  (the `on(event, fn)` source); a `lua` formula alternative on the ruleset formula refs; a `pvp_lua`
  policy source; a `commands` block registering custom verbs. Parsed by the extended mapper into a
  compiled-proto registry (atomic-swap, like the Phase 5/6 registries).
- **`StateJSON.Script`** (character.go) — the new data-only `self.state` subtree (7.6), serialized on
  the existing cadence; pre-7.6 saves load with none (the established backward-compat default).
- **The compiled-proto registry** is per-shard runtime state (atomic.Pointer-swapped like the
  prototype cache); the reloader recompiles a single proto on a `(kind, ref)` invalidation (7.7).
- **The stdlib pack** (the acceptance content) gains: a **scripted greeter mob** (`on("greet"/
  "speech")` + `self.state`), a **Lua `on_resolve` ability**, a **Lua Counterspell + Shield + a
  concentration spell**, a **multiclass-slot Lua formula**, and a **sailing demo** (a `pack:OnShipDock`
  custom event a dock room fires and a quest handler subscribes to) — the §5 done-when content. The
  bare-engine invariant holds: **no Lua content ⇒ no scripts compiled ⇒ the VM runs nothing** (the
  empty-boot test stays green; Lua is unavailable, not erroring).

---

## 7. Risks & out-of-scope

### Explicitly OUT of scope
- **The cross-zone scoped + durable event bus = Phase 10** ([WORLD-EVENTS.md](WORLD-EVENTS.md)). A Lua
  handler needing a cross-zone consequence enqueues for the (Phase-10) director — reserved no-op. The
  custom-event lane (7.8) is **in-zone** like the rest of the Phase-6 bus.
- **GMCP structured emit = Phase 9.** `mud.gmcp(h, package, data)` is the binding's shape but the
  emit lands with the GMCP negotiation phase; Lua emits `act`/`send` text now.
- **Progression / chargen / the track grants Lua composes = Phase 11.** Phase 7 ships the Lua
  multiclass-slot **formula** (the table math) and the reaction model; the `grant_*` ops and
  `track_defs` Lua composes are Phase 11.
- **`gopher-luar` / reflection-based binding = never** (P7-D2). The curated surface is the audit
  surface; reflection would expose Go struct layout and defeat the API/engine decoupling.
- **A hard per-VM memory cap = not available** (T5, the acknowledged gopher-lua limitation). We bound
  memory indirectly (capped builtins T13, `state` caps) and observably (per-zone metric, **detection-
  only** — fires after the fact) — documented, not silent.

### Integration risks
1. **The sandbox is the sharpest trust boundary in the engine (security) — and the security-auditor's
   probes against real gopher-lua v1.1.1 falsified five mitigations as first drafted.** The corrected
   set: the allowlist-built `_ENV` **by registering kept functions individually, NOT `lua.OpenBase`**
   (T1/T14); the **vendored-fork instruction count** (v1.1.1 has no debug hook — `MaskCount` does not
   exist) + the wall-clock deadline + the **capped amplifier builtins** (single-op bombs the count/clock
   miss — T13); **`lua.Options{CallStackSize}`** for recursion (no `SetMaxStackSize` — T4); the
   **frozen string metatable + dropped `get/setmetatable`** (the shared-`L.G` poison the per-call `_ENV`
   reset doesn't cover — T14); `__tostring` on every handle (the pointer leak — T15); the harm-funnel
   reuse + handle re-validation. The threat model (§2, now **T1–T15**) is the review checklist.
   **security-auditor reviews 7.1, 7.2, 7.3c, 7.5, 7.6, 7.9** — the largest new attack surface since the
   engine began.
2. **The harm gate over the Lua surface (security).** A Lua effect op is the newest harm-injection
   vector; it must funnel the **same** `guardHarmful` with **no** Lua-specific path (T8/P7-D3). The
   binding builds the `effectCtx`; the funnel owns the gate, fail-closed on a detached actor/target.
   **security-auditor reviews 7.3c** — the in-op funnel is what makes it can't-bypass.
3. **No new concurrency (distributed-systems).** The `LState` is single-writer on the zone goroutine;
   no goroutine touches it, no lock guards it. `mud.after` schedules on the zone wheel, not the OS
   scheduler. **distributed-systems-architect confirms** the VM lifecycle adds no cross-goroutine
   access and the reload swap stays on the subscription goroutine (the existing reload.go contract).
4. **Hot reload must not run old code against new state (correctness).** Old-gen `mud.after` timers
   drop by default (P7-D7); `self.state` data survives but the chunk is swapped atomically; a compile
   error keeps the last-good proto. **The reload-corruption class is the subtle risk** the gen tag
   guards.
5. **Memory is a soft cap (security/ops).** gopher-lua has no hard per-VM limit. **A single-op
   allocation bomb (`string.rep("A", 2e9)`) is NOT caught by the instruction count (one op) or the
   wall-clock (no between-op check inside a builtin) — it is prevented by the capped builtins (T13)**,
   not by a budget. Gradual multi-op growth is bounded by the instruction count + `state` caps; the
   per-zone memory metric is **detection-only** (fires after the fact). Documented as a known
   limitation (T5/T13), not silent.
6. **`self.state` is a persistence + injection surface (persistence/security).** Data-only,
   size-bounded, no code/handles (T10). **persistence-engineer confirms** the `StateJSON.Script`
   subtree follows the JSONB-tail + cadence pattern and excludes nothing that should persist; the
   marshaller's allowlist is **security-auditor**'s review (7.6).

### Cross-cutting reviewers (per [subagent-review-after-every-step])
- **scripting-engineer (owning):** every slice — the VM lifecycle, the handle layer, the API surface,
  the entry points, the budgets/breaker, `self.state`, hot reload, the hookability obligations, the
  reaction model.
- **security-auditor:** **7.1** (the vendored-fork abort path + the individually-registered allowlist
  + the capped builtins + the frozen string metatable — the load-bearing construction), **7.2** (handle
  `__tostring`, no pointer leak), **7.3c** (the harm-funnel reuse + the five binding invariants),
  **7.5** (the budget chokepoint over EVERY Lua path + the two-mode breaker + deadline-vs-error
  weighting), **7.6** (the `state` marshaller allowlist + caps), **7.9** (the reaction field enum +
  `replace_target` re-gate + shared budget) — the §2 threat model (T1–T15) is the checklist; each slice
  carries its row's mitigation. **Re-confirm sign-off after this revision** (the GAPS-FOUND fold).
- **distributed-systems-architect:** 7.1 (no cross-goroutine VM access), 7.3b (`mud.after` on the zone
  wheel, not the OS scheduler), 7.7 (the reload swap stays on the subscription goroutine), 7.8 (the
  custom-event lane stays in-zone — the Phase-10 boundary).
- **persistence-engineer:** 7.6 (`StateJSON.Script` JSONB-tail + cadence + size caps; nothing that
  should persist is excluded), 7.7 (state survives the reload).
- **abilities-engineer:** 7.4 (Lua `on_resolve`/affect hooks fit beside the op-list interpreter with no
  lifecycle change), 7.9 (the reaction context object fits the Phase-6 checkpoints additively).
- **combat-engineer:** 7.9 (Counterspell/Shield/concentration reach the checkpoints the Phase-6 swing/
  cast pipeline published — confirm the seam carries what the reactions need).
- **rpg-systems-designer (acceptance):** 7.9 — confirm the escape-hatch cases (result-altering
  reactions, concentration, the multiclass-slot table) are the right complex-20% set and express
  cleanly in the reaction + Lua-formula model.

---

## 8. Done-when (the phase capstone)

The ROADMAP Phase 7 done-when, made concrete on this plan — **all four, all edited live, none able to
crash, stall, or cross a zone:**

1. **A room script greets on entry** — a room/mob `on("enter"/"greet", …)` greets an arriving player
   by name and remembers them via `self.state` (which survives logout/login) — and the greeting text
   is **edited live** (hot reload) without a restart.
2. **A scripted mob reacts to speech** — `on("speech", …)` makes a mob respond to a keyword.
3. **A Lua Counterspell cancels an in-flight cast** — a `BeforeCastCommit` reaction hook (the typed
   reaction context, `rx:cancel()`) reaches into an in-flight ability and **vetoes** it
   (observe-then-recheck), within the single-writer model, no pipeline surgery.
4. **A pack fires and handles a custom event the engine never heard of** — a sailing system defines,
   `mud.fire`s, and `on`-subscribes a `pack:OnShipDock` event entirely in content (the custom-event
   lane), depth/width-budgeted and gate-funneled like an engine event.

And the safety capstone, demonstrated under test: a deliberately runaway script (`while true do end`),
a **single-op allocation bomb** (`string.rep("A", 2e9)` — capped, T13), a deeply recursive one
(`CallStackSize`-bounded, T4), a **string-metatable poison attempt** (`getmetatable("")` is `nil`, the
mt frozen — T14), a chronically-erroring one, and a harm-injecting one **each fail just their own
action** — the zone keeps serving every other player, the breaker quarantines the chronic offender (a
latency-induced deadline trip does NOT), the harm funnels the gate, no Lua error leaks to a player, and
**no script crashes, stalls, or reaches out of its zone.**


---

<a id="phase-8"></a>

# Phase 8

_(archived from docs/PHASE8-PLAN.md)_

# Phase 8 — Comms over NATS (cross-shard channels, tells, who, presence, mail) — IMPLEMENTATION PLAN

Status: **proposal / planning** — the design + sliced plan for ROADMAP Phase 8. This is a sign-off
doc for the human owner; it implements **nothing**. Confirm §1 (the topology decision) and §2 (the
subject taxonomy) before slice 8.1.

Phase 8 is the first phase whose primary state is **player-scoped and cross-shard**, not zone-scoped.
Every system before it (rooms, combat, affects, Lua) lives *inside* a zone goroutine and never reaches
past it; cross-zone interaction is the handoff (PROTOCOL.md) or a reserved Phase-10 hook. Comms breaks
that frame on purpose: a `gossip` line typed by a player on shard A must reach the socket of a player
on shard B **regardless of which zone either is in**. So the central question is not "what does a
channel do" (that is easy) but **where the cross-shard fan-out terminates** — which process holds the
subscription that turns a NATS message into bytes on a particular terminal. §1 answers that first; the
rest hangs off it.

**Done when** (ROADMAP Phase 8): two players on **different shards** chat on a channel and see each
other in `who`. The capstone (§8) makes that concrete and adds the failure-mode demonstrations (a
crashed shard's players age out of `who`; an offline tell is delivered on next login exactly once).

This phase builds on, and must not re-invent:
- **The NATS wiring + mem-fallback pattern** already proven by the content bus
  (`internal/contentbus/nats.go`, `membus.go`, `contentbus.go`): a small `Bus` interface, a NATS impl,
  an in-process `MemBus` that mirrors the NATS observable semantics so the whole feature is
  unit-testable with **no live broker** (the cross-shard tests run many shards in one process against
  one `MemBus`). Phase 8 ships a **parallel comms bus** with the same shape. It does **not** widen the
  content bus — that bus carries `(kind,ref,pack)` invalidations and nothing else.
- **The directory** (`internal/directory/`): `PlayerPlacement`/`SetPlayerShard` already track which
  shard a player lives on, with a monotonic epoch. Tell-routing and presence reuse this — the
  directory is the existing player→shard map.
- **The gate↔world Play stream** (`internal/gate/`, `internal/world/server.go`): the per-player socket
  and the `out chan *playv1.ServerFrame` that already carries every line to a terminal. Comms output
  is a new producer into that **existing** channel.
- **The durability ladder** (PERSISTENCE.md): Redis for operational/ephemeral (`presence` is already
  listed there), Postgres+Redis for durable player state (`mail` is **already listed there**). Phase 8
  does not invent a new store tier; it uses the ones the ladder already names.

It does **not** build: GMCP structured comm emit (`Comm.Channel.Text` — Phase 9; Phase 8 emits plain
text frames), the **cross-zone scoped world-event bus** (Phase 10, WORLD-EVENTS.md), auth/accounts
(Phase 14 — Phase 8 keeps the stub login, and a "player" is the login name as today). The Phase-8 ↔
Phase-10 boundary is load-bearing and is drawn explicitly in §7.

---

## 0. Where Phase 8 sits on the existing code

| Existing | Phase 8 change |
|---|---|
| `internal/contentbus/` — `Bus` interface + NATS impl + `MemBus`, one subject, JSON payload, optional/never-fatal | A **new sibling package `internal/commbus/`** with the **same shape** (interface + NATS impl + `MemBus`), but a **subject taxonomy** (not one subject) and **JetStream** for durable tells/mail. Reuses the connection/degradation discipline verbatim; does not import or extend contentbus. |
| `internal/directory/redis.go` — `PlayerPlacement`/`SetPlayerShard`/`PlayerEpoch` (player→shard, epoch-monotonic) | Tell routing reads `PlayerPlacement` to find a target's shard subject. Presence **may** reuse the same Redis (a `who` roster) or a NATS mechanism — §1 P8-D4 decides. No change to the placement CAS. |
| `internal/gate/` — `Server.handle` runs the per-connection bridge; `session` owns the input buffer; the writer goroutine drains `out chan ServerFrame` to the socket | **P8-D1 decides whether the gate or the world subscribes** for a player's channels. If the gate subscribes (the recommendation), the gate gains a **comms client** per connection and a new producer into the writer path. |
| `internal/world/commands.go` — `cmdSay`/`cmdWho` (zone-local), `cmdQuit`; `zone.who` lists only `z.players` | `say` stays zone-local (it is a room verb). **New commands** `gossip`/`newbie`/channel verbs, `tell`, a **cross-shard `who`**, `reply`, `mail`, channel toggles, `ignore`, `afk`. Whether these live in the world command table or a comms tier depends on P8-D1. |
| `internal/world/zone.go` — `presenceMsg` (zone-local presence query), `linkDeadGrace` (a player's in-zone presence survives a stream drop briefly) | A **presence heartbeat** publisher (per shard, listing its live players) feeds the cross-shard roster; the link-dead/quit lifecycle is the disconnect signal that ages a player out. |
| `internal/world/character.go` — `StateJSON` + `state` JSONB, saved on the existing cadence/ladder | **Player comms state** (channel on/off, ignore list, AFK) is a new **data-only subtree** of `StateJSON` (the established additive-JSONB pattern), saved on the existing cadence. |
| `cmd/telos-world/main.go` / `cmd/telos-gate/main.go` — wire NATS/Redis/directory, optional/never-fatal | The comms bus is wired the same way: `TELOS_NATS_URL` (the existing `cfg.NATS.URL`), optional, never fatal — **NATS down ⇒ comms degraded (local-only), never a boot failure**, exactly like hot reload degrades. |
| The empty-boot invariant (`NewShardFromContent` with no content ⇒ empty zones; `empty_world_test.go`) | **Channels are content** (`channel_defs`): a pack with no `channel_defs` ⇒ **no channels** and the empty-boot test stays green. Tells/who/presence/mail are engine mechanism (no content needed) but are inert with no players. |

The riskiest *structural* point is **the trust/ordering boundary the comms bus introduces** — a
message authored on one shard and rendered on another crosses a process boundary with no single-writer
to serialize it. So the plan leads with the **transport + topology** (8.1) and gets the
attribution/ordering/abuse boundary reviewed **before** any feature (channels, tells, who, mail) hangs
off it. This mirrors PHASE7-PLAN leading with the sandbox: review the boundary before the capabilities.

---

## 1. Design decisions (confirm before slice 8.1)

### P8-D1 — The comms topology: **gate-subscribes** (the central decision)

**The question.** A channel/tell line is *player-scoped*: its output must reach a player's terminal no
matter which zone or shard that player is currently on. Three processes could hold the NATS
subscription that turns a comms message into a frame on that terminal:

- **(A) World subscribes (per shard).** Each world shard subscribes to the channels its resident
  players are on, and pushes the rendered line into each player's `out chan ServerFrame`.
- **(B) Gate subscribes (per connection).** The gate — which already owns the socket and the
  `out`-equivalent writer path, and is *stable across the cross-shard handoff* — subscribes on behalf
  of each connected player and writes comms lines straight to the terminal.
- **(C) A new comms tier.** A dedicated `telos-comms` service holds all subscriptions and fans out to
  gates.

**Decision: (B) gate-subscribes, with the world as the message *source*.** The gate is the natural
home for player-scoped cross-shard output because:
1. **The gate already survives the handoff.** When a player walks A→B, the gate keeps the same
   `session` and socket and merely re-dials the Play stream (PROTOCOL.md §5; `gate.go` re-dial loop).
   A *world-held* subscription (option A) would have to be **torn down on shard A and re-established on
   shard B mid-handoff** — a new neither/both-subscribed window layered on top of the existing
   neither/both-*own* handoff window. The gate subscription does not move when the player's zone moves.
   This is the decisive argument: comms subscription lifetime should track the **connection**, which is
   the gate's unit, not the **zone ownership**, which moves.
2. **The gate already multiplexes one socket.** Channel text, tell text, and room output all become
   `ServerFrame_Output` on the same writer; the gate is where they converge to bytes anyway.
3. **No new tier to deploy/operate** (rejecting C for v1). C is the right answer at extreme scale (it
   decouples channel fan-out from gate count and lets channels shard independently) — but it is a
   premature tier now and is called out as a **scale escape hatch** in §3, not built.

**The gate↔world responsibility split that follows:**
- **World is the SOURCE.** A player types `gossip hi` → it reaches the zone goroutine as input (the
  existing path) → the world command handler **publishes** a channel message to NATS (author identity,
  channel ref, text, a monotonic-per-author sequence, an idempotency key). The world does **not**
  deliver it to anyone's socket directly (not even co-located players — they receive it via the bus
  too, so there is exactly one delivery path and no double-render). The world publishes because **it
  holds the authoritative author identity** (the `*Entity`, its name, its flags) and the
  access/format rules (channel is content — P8-D3); the gate must never be trusted to attribute a
  message (P8-A2, impersonation).
- **Gate is the SINK.** Each connection's gate-side comms client is subscribed to the channels the
  player has on (and to that player's personal tell/notify subject). On a message it renders per the
  channel format and writes a frame. The gate applies **receiver-side** policy that is cheap and
  socket-local: the receiver's channel-off toggle and ignore list (so a blocked sender's line is
  dropped at the receiver — defense in depth beside the sender-side checks; P8-A6).
- **Commands** that *emit* comms (`gossip`, `tell`, who-as-broadcast) are **world commands** (they need
  the author entity + content rules), registered in the world command table beside `say`. Commands
  that are **pure local toggles** (`channels on/off`, `ignore`, `afk`) are also world commands because
  they mutate persisted character state (the JSONB subtree, P8-D7) — but the gate caches the resulting
  filter set so receiver-side filtering needs no per-message world round-trip.

> **The trust line (write it down):** the **world authors** (it owns identity + content rules); the
> **gate renders + receiver-filters** (it owns the socket). A message's attribution is set by the
> source world from the live `*Entity`, **never** by the gate and **never** carried as a
> client-supplied field. This is the impersonation gate (P8-A2) and is the single most security-
> sensitive invariant of the phase.

*Rejected:* (A) world-subscribes — the subscription would migrate on every handoff, multiplying the
handoff's neither/both window onto comms, and a shard hosting 0 of a channel's listeners would still
carry the subscription churn. (C) comms tier — correct at scale, premature now (§3 escape hatch).

### P8-D2 — NATS subject taxonomy + JetStream streams

A **subject hierarchy** (not contentbus's single subject), namespaced under a `telos.comms.` root so it
never collides with `content.invalidate`. Proposed taxonomy (confirm the exact tokens at sign-off):

| Purpose | Subject | Delivery | Notes |
|---|---|---|---|
| Channel message | `telos.comms.chan.<channelRef>` | **NATS core** (transient, at-most-once) | Channels are ephemeral chat: a missed line during a NATS blip is acceptable (you were not listening). Every gate subscribed to that channel ref receives it. `<channelRef>` is the content channel id (`gossip`/`newbie`), so a gate subscribes per-channel-the-player-has-on. |
| Online tell (notify) | `telos.comms.tell.<targetPlayerId>` | **NATS core** when the target is online | The sender's world looks up the target via `directory.PlayerPlacement`; if present/online it publishes to the target's personal subject, which the target's gate is subscribed to. |
| Presence heartbeat | `telos.comms.presence` (or a NATS **KV** bucket — P8-D4) | **NATS core** fan-in to the roster owner, OR KV | Per-shard heartbeats listing live players; feeds the `who` roster. P8-D4 picks the mechanism. |
| Durable tell (offline) | JetStream stream `COMMS_TELL`, subject `telos.comms.dtell.<targetPlayerId>` | **JetStream** (at-least-once + dedup) | An offline target's tell is persisted; delivered on next login. Idempotency via `Nats-Msg-Id` (publish-side dedup window) + a consumer-side delivered-cursor (P8-D5). |
| Mail | JetStream stream `COMMS_MAIL` **or Postgres** (P8-D6) | durable | Persistent inbox; read/send model. P8-D6 chooses the store. |

**Wildcard subscription.** A gate subscribes to `telos.comms.chan.gossip`, `telos.comms.chan.newbie`,
… per the player's enabled channels (re-subscribing on a toggle), plus its own
`telos.comms.tell.<self>` and `telos.comms.dtell.<self>`. It does **not** use a `telos.comms.chan.>`
wildcard (that would receive every channel including ones the player has off, pushing the filter to the
gate for *every* line — the per-channel subscribe keeps the broker doing the fan-out cut). *(Open
question OQ-3: per-channel subscribe vs one wildcard + gate-filter — a fan-out-vs-subscription-churn
trade; recommended per-channel, revisit under load.)*

**Mem fallback.** `internal/commbus/membus.go` mirrors these semantics in-process (a subject→subscriber
map with per-sub ordered delivery, exactly like contentbus's `MemBus`), and a **mem JetStream stand-in**
(an in-memory append log with a delivered-cursor) so the durable-tell/mail slices are testable without a
broker. The cross-shard tests run N shards + N gates in one process against one `MemBus`.

### P8-D3 — Channels are CONTENT (`channel_defs`), not hardcoded

Per the engine pillar (PRINCIPLES.md: nothing hardcoded; MEMORY: extensibility across game systems), a
channel is a **content definition**, not an engine enum. A `channel_def` carries:

- `ref` (stable id: `gossip`, `newbie`, `auction`, an OOC channel, a guild channel later)
- display `name` and the **command verb(s)** that emit on it (so `gossip hi` works because the pack
  defined a `gossip` channel with that verb; an empty pack ⇒ no such verb)
- `color`/markup template and the **format** strings (the speaker-perspective and listener-perspective
  templates, e.g. `"[$channel] $name: $t"`), rendered with the existing `act()`-style `$`-substitution
  so a `%`/`$` in user text is data, never a template (the `cmdSay` precedent, commands.go)
- `access` predicate (who may listen/speak — by flag/attribute/level; later by guild membership). A
  content predicate, evaluated engine-side against the author `*Entity` — **never** trusting the client.
- `default_on` (is a new character subscribed by default), `history` size (recent-lines buffer for a
  late joiner — optional, deferred shape).

This is a **new definition table + loader mapping + content-bus invalidation kind** (`channel`), so
editing a channel's color/access hot-reloads like any other def (the Phase-4 pattern). The empty-boot
invariant: **no `channel_defs` ⇒ no channels ⇒ no channel verbs ⇒ `empty_world_test.go` stays green.**
`tell`/`who`/`mail`/presence are **engine mechanism** (not content — they exist with zero packs), but a
*channel* is content.

> Tells, who, mail, presence: **engine**. Channels: **content**. This split is deliberate — directed
> player↔player messaging and the online roster are universal mechanism; named broadcast channels with
> colors/access/format are world-flavor.

### P8-D4 — Cross-shard `who` / presence: **Redis roster + heartbeat, with a NATS-core liveness ping**

`who` must list players across **all** shards, and a crashed shard's players must **age out** (the
stated failure mode). Options:

- **(A) Redis presence roster** (recommended). Each shard maintains, in Redis, the set of its live
  players with a **per-player TTL** refreshed by a heartbeat (the same pattern as the directory's
  shard/zone leases, `directory/redis.go`). `who` is a single Redis scan of the roster (filtered by
  visibility). **Staleness handling is automatic**: a crashed shard stops refreshing, and its players'
  entries **expire** (TTL ≈ 2–3× the heartbeat interval) — they age out of `who` without any explicit
  cleanup. PERSISTENCE.md **already lists `presence` under the Redis/operational tier**, so this is the
  ladder's intended home.
- **(B) NATS KV presence bucket** — equivalent semantics (per-key TTL, watchable), but introduces a
  second store for a job Redis already does, and Redis is already wired (`cmd/telos-world/main.go`).
- **(C) Scatter-gather query** — on `who`, broadcast a request and aggregate replies with a timeout.
  Rejected: adds tail-latency to a common command and a partition makes `who` flaky; the roster is
  strictly better for a read-mostly online list.

**Recommendation: (A) Redis roster.** Each shard writes `presence:<playerId> = {name, shardId, flags,
lastSeen}` with a TTL on join and refreshes it on a heartbeat (and on the existing link-dead/quit
lifecycle removes it eagerly on a clean quit — `cmdQuit`/`leave` in zone.go). `who` reads the roster.

**Crash age-out invariant (the failure the phase must demonstrate):** a shard that crashes leaves its
players in the roster only until their TTL lapses; after that they are gone from `who`. The TTL is the
**only** mechanism that recovers a crashed shard's presence — never an explicit "shard died, clean its
players" step (which itself could be lost). This is the same lease-expiry recovery the directory uses
for zones (`DefaultZoneLease`). **Tune the heartbeat/TTL** so the age-out window is bounded (target:
≤ ~30s, matching the directory leases) — OQ-2.

**Presence is operational/ephemeral, never authoritative.** A stale presence entry must never let a
**tell** route to a dead shard and be lost — tell routing reads the **directory** (`PlayerPlacement`,
which is the placement-epoch-authoritative map), not the presence roster, and falls back to the durable
offline path if the publish has no live consumer (P8-D5). Presence answers *"is this name in `who`"*;
the directory answers *"which shard owns this player right now"*. Conflating them is a bug (P8-A4).

### P8-D5 — Tell routing: online via directory, offline via JetStream (at-least-once + idempotent)

A `tell <name> <msg>` resolves in the **source** world (it owns the sender identity + the gate/ignore
checks the sender side can do):

1. **Resolve the target** via `directory.PlayerPlacement(targetId)` (the authoritative player→shard
   map). If the target has a live placement → publish to `telos.comms.tell.<targetId>` (NATS core); the
   target's gate is subscribed and renders it. Echo "You tell X" to the sender.
2. **Offline / no live placement** → publish to the **JetStream** durable stream
   (`telos.comms.dtell.<targetId>`), persisted until the target logs in. On login the target's gate (or
   world — OQ-4) **consumes** the durable backlog and renders "While you were away, X told you …".

**At-least-once + idempotent + ordered:**
- JetStream is **at-least-once**; a redelivery (consumer ack lost, a reconnect) must not double-deliver
  a tell. Each durable tell carries an **idempotency key** (`<senderId>:<senderSeq>` — the sender's
  monotonic per-author sequence) set as the `Nats-Msg-Id` header → **publish-side dedup** within the
  stream's dedup window, **plus** a **consumer-side delivered-cursor** (the last-delivered sequence,
  persisted per player in their character state or Redis) so a redelivery **after** the dedup window
  (minutes later) is still suppressed at render time. Belt and suspenders: the broker dedups recent
  duplicates; the cursor dedups old ones (P8-A5, redelivery storms).
- **Ordering** is per-target, single durable consumer per player, acked in order; the sender's
  monotonic sequence gives a total order *per sender* and the consumer renders in stream order. We do
  **not** promise a global order across different senders (no shared clock; not worth a sequencer) —
  only "messages from one sender arrive in send order," which is what users perceive.
- **The online→offline race** (the target logs out between the directory read and the publish): if a
  core-NATS tell is published to a target's subject with **no subscriber**, it is silently dropped
  (NATS core is at-most-once). Mitigation: route ambiguity favors **durable**. Recommended:
  resolve-then-publish-core, **and** if the directory read showed the target only *recently* present or
  the publish cannot confirm a subscriber, **fall through to the durable stream** so a logging-out
  target still gets the tell on next login. *(OQ-1: do we want a JetStream-with-immediate-consumer
  model for ALL tells — durable always, online delivery is just a fast consumer — to eliminate the
  core/durable split and the race entirely? Simpler correctness, slightly more JetStream load. Strong
  candidate; flagged for the owner.)*

### P8-D6 — Mail: **Postgres** (recommended) vs JetStream

Mail is durable, queryable (list inbox, read item N, delete, mark-read), and long-lived — a
relational/document store fits better than a log:

- **(A) Postgres** (recommended). A `mail` table (`to_player`, `from_player`, `subject`, `body`,
  `sent_at`, `read_at`). PERSISTENCE.md **already lists `mail` under the Postgres+Redis durable player
  tier** — this is the documented home. Read/send is ordinary CRUD on the existing pool
  (`internal/store`), with the same optional/never-fatal degradation (no Postgres ⇒ mail disabled,
  not a crash).
- **(B) JetStream** stream per recipient. Works for delivery but is awkward for the **read model** (mark-
  read, delete a single item, list with subjects) — JetStream is a log, not an inbox. Rejected for mail
  as the primary store, though the *send notification* ("you have new mail") can ride the comms bus.

**Recommendation: (A) Postgres for the mail store; the comms bus only carries the "new mail" ping.**
Mail send = INSERT; mail read = SELECT; the recipient gets a transient `telos.comms.tell.<self>`-style
notify if online. This keeps the **offline tell** (transient, fire-and-forget, JetStream) and **mail**
(a durable inbox, Postgres) as distinct models rather than forcing both into one.

### P8-D7 — Player comms state: a data-only `StateJSON` subtree

Channel on/off toggles, the ignore/block list, and the AFK flag/message are **per-character, durable,
and small** — they ride the **existing character `state` JSONB** as a new data-only subtree (the
`StateJSON.Script` precedent in `character.go`, the established additive-JSONB pattern). Saved on the
existing cadence/ladder; loaded on login. The gate **caches** the receiver-side filter set (channels-on,
ignore list) at attach and on change, so per-message receiver filtering is socket-local and needs no
world round-trip.

- **Channel toggles**: which `channel_def` refs the player is subscribed to (default from
  `channel_def.default_on`). Drives the gate's per-channel NATS subscriptions.
- **Ignore list**: sender ids the player blocks — applied **both** sender-side (the source world drops a
  channel/tell from an author the *target* ignores — but the source only knows the *sender's* ignores,
  so the authoritative block is **receiver-side at the gate**) and as defense in depth. The blocked-
  sender-bypass threat (P8-A6) is why the **gate (receiver) is the authoritative ignore enforcement
  point** — it sees every inbound line and the receiver's own list.
- **AFK**: a flag + optional message; an AFK target's tell still delivers (or auto-replies "X is AFK")
  and the sender is told. Presence carries the AFK flag so `who` can mark it.

This subtree is **data-only** (strings/bools/lists of ids) — no code, no handles — and is size-guarded
like the Lua state subtree. Nothing here is content; it is per-player runtime state.

---

## 2. Threat / abuse model (the security-auditor's checklist for the phase)

Comms is the first **player-authored, cross-shard, fan-out** surface. Unlike the Lua sandbox (a trust
boundary against a *content author*), this is a trust boundary against **every connected player** — the
adversary is an ordinary hostile user with a telnet client. Every slice that adds a comms surface
carries its row's mitigation.

| # | Attack surface | Invariant | Enforced by | Tested by |
|---|---|---|---|---|
| **P8-A1** | **Spam / flood** — a player floods a channel or tells, drowning others / loading the broker. | Per-author comms is **rate-limited**; a flood throttles the **sender**, never degrades other players' delivery. | A token-bucket per author per channel/tell, enforced in the **source world** (it holds the author identity) **before** publish; over-limit lines are dropped with a "you are doing that too much" to the sender only. The broker never sees the dropped line. Slow-consumer protection: the gate's writer path already drops/backpressures a slow socket (the existing `out chan` discipline) so one slow terminal can't stall fan-out. | a flood test: author rate-limited, other players unaffected; a slow-socket test: fan-out not stalled by one slow gate. |
| **P8-A2** | **Impersonation** — a player forging another's name as the channel/tell author. | A message's **author identity is set by the source world from the live `*Entity`**, never from a client field. | The world publishes the author id/name; the wire payload's author field is **engine-set**; the gate **renders** but never **authors** (P8-D1). No client frame carries an author name. A receiver gate trusts the bus author field **only because the source world is the only publisher** — the comms bus subjects are published to **by world shards only**, never by gates (gates are subscribe-only on channel/tell subjects). | a test that a crafted client input cannot change the rendered author; a test that a gate cannot publish to a `chan.*`/`tell.*` subject (publish ACL / it simply has no publish path). |
| **P8-A3** | **Cross-shard message ordering** — two shards interleave a channel such that one player sees A-before-B and another B-before-A; or a sender's own lines reorder. | **Per-sender order** is preserved for every receiver; no global order is promised (and none is needed). | Each author stamps a **monotonic per-author sequence**; a channel is a single NATS subject so the **broker imposes one publish order** per channel (all subscribers see the same order for that subject). For tells, the per-player durable consumer acks in stream order. We do **not** claim cross-sender global order (no shared clock). | a two-sender interleave test: each sender's lines are in-order for every receiver; a tell-order test per sender. |
| **P8-A4** | **Presence spoofing / staleness** — a player appears online when crashed/gone, or a tell routes to a dead shard and is lost. | `who` is **best-effort and self-healing** (TTL age-out); **tell routing never trusts presence** — it uses the authoritative directory and falls back to durable. | Presence is TTL-leased (P8-D4): a crashed shard's players expire. A player cannot write another's presence (the shard writes only its own residents' keys, keyed by the player it actually hosts). Tell routing reads `directory.PlayerPlacement` (epoch-authoritative), not the roster; a publish with no live consumer falls to the durable stream (P8-D5) so it is not lost. | a crashed-shard age-out test (players gone from `who` after TTL); a tell-to-logging-out-target test (delivered durably, not lost); a test that a shard cannot write a non-resident's presence. |
| **P8-A5** | **JetStream redelivery storm** — a consumer reconnect / ack-loss redelivers a backlog, double-rendering tells or hammering a just-logged-in player. | A redelivered tell is **suppressed idempotently**; redelivery is **bounded** and never amplifies. | The `Nats-Msg-Id` idempotency key + the per-player **delivered-cursor** (P8-D5) suppress duplicates at render even past the dedup window; the durable consumer has **bounded redelivery** (max-deliver + backoff) so a poison message is parked, not infinitely redelivered; the login-time backlog drain is **paced** (cap lines/sec to the freshly-joined gate). | a redelivery test: the same tell delivered twice renders once; a max-deliver test: a failing message parks; a backlog-pacing test. |
| **P8-A6** | **Blocked-sender bypass** — an ignored sender's channel line / tell still reaches the receiver via a path that skips the block. | A receiver's **ignore list is enforced at the receiver gate** — the one place that sees **every** inbound comms line for that player — so no source path can bypass it. | The gate (P8-D1 sink) applies the receiver's ignore list to **every** channel/tell frame before rendering; sender-side checks are defense-in-depth, not the authority. A new comms path (mail notify, a future channel type) inherits the gate filter automatically because it funnels the same render point. | a test that an ignored sender's gossip is dropped at the receiver; a test that a *new* comms frame type is also ignore-filtered (the funnel, not per-path). |
| **P8-A7** | **Text injection** — a player embeds telnet control bytes, ANSI, or `$`/`%` format tokens to corrupt another's terminal or spoof channel markup. | User-supplied comms text is **data**, never markup/template; control bytes are sanitized. | Channel/tell text is substituted as a `$t` **data argument** (the `cmdSay` precedent — a `$`/`%` in it is literal), and run through the existing terminal sanitizer (the gate's `textsan` path used for speech) so a player can't inject ANSI/telnet IAC or fake a `[gossip] Admin:` prefix. The channel **format template** comes from content (trusted), the **text** from the user (sanitized). | a test that a `$n`/ANSI/IAC in tell text renders literally and cannot forge a channel prefix; reuse the existing speech-sanitization test pattern. |
| **P8-A8** | **Subject-injection / channel access bypass** — a player speaks on a channel they lack access to, or names a `<channelRef>` that injects into the subject space. | A player can only emit on a channel whose **content `access` predicate** they satisfy; `<channelRef>` is validated against the loaded `channel_defs`, never free-form into a subject. | The source world checks `channel_def.access` against the author `*Entity` before publish; the subject is built from a **validated, known** channel ref (a ref not in the loaded defs is rejected — no arbitrary subject). The gate subscribes only to channels the player has access to + on. | an access-denied test (speak on a no-access channel rejected); a bogus-channel-ref test (rejected, no subject injection). |

**Construction note (the load-bearing trust line):** the comms bus subjects under `telos.comms.chan.*`
and `telos.comms.tell.*` are **published to by world shards only**; **gates are subscribe-only** on
them. This is what makes P8-A2 (impersonation) hold — a receiver trusts the author field *because the
only writers are source worlds that set it from the live entity*. If a deployment ever lets a gate
publish (it must not), the impersonation gate is gone. The security-auditor signs off on this
publish/subscribe asymmetry before 8.1 lands, and on the rate-limit (P8-A1), the receiver-side ignore
funnel (P8-A6), and the text sanitization (P8-A7).

---

## 3. Risks & out-of-scope

### Explicitly OUT of scope
- **The cross-zone scoped world-event bus = Phase 10** (WORLD-EVENTS.md). Phase 8's comms bus is a
  **player-presence/channel layer**: it carries *player-authored chat and player-directed messages*
  between **gates and worlds**. The Phase-10 bus carries *world-state consequences* (a boss death
  rippling a region) between **zones/directors** with `transient`+`durable` scopes and leader-elected
  directors. **They are different buses with different payloads, owners, and lifecycles** — do not
  conflate them, do not build one on the other. A Phase-8 channel message is not a world event; a
  Phase-10 region event is not a chat line. (They may *share the NATS connection and the JetStream
  server*, but not the subject space or the code.) This boundary is the §7 note's whole point.
- **GMCP `Comm.Channel.Text` structured emit = Phase 9.** Phase 8 emits **plain `ServerFrame_Output`
  text** (the existing frame). When Phase 9 lands GMCP negotiation, a channel message *also* emits a
  structured `Comm.Channel.Text` package — but the binding shape is reserved, not built here. Phase 8
  channels render as text now.
- **Auth / accounts / real identity = Phase 14.** A "player" in Phase 8 is the **login name** (the
  current stub), and `playerId` is that name (as the directory already keys it). Single-session lock
  (PERSISTENCE.md) already prevents one name being two live sessions; cross-account block lists, real
  account identity, and friend lists wait for Phase 14.
- **A dedicated `telos-comms` tier (topology option C) = a scale escape hatch, not v1.** At millions of
  concurrent players, channel fan-out (every gossip line × every subscriber) can dominate. The escape
  hatch (documented, not built): introduce a comms tier that (a) terminates channel subscriptions so
  the gate count and channel count scale independently, (b) shards a hot channel across subjects, and
  (c) coalesces presence. Because Phase 8 keeps the **world=source / gate=sink** split behind a `Bus`
  interface, inserting a comms tier later is a wiring change, not a redesign. Capacity note: with the
  gate-subscribes model, a channel with N subscribers across M gates costs **M subscriptions and one
  broker fan-out of N**; the broker fan-out is the ceiling to watch (§ capacity, below).
- **Channel history / scrollback** beyond a small recent-lines buffer — deferred shape (P8-D3 notes the
  optional `history` size).

### Capacity / scale notes (quantify where we can)
- **Channel fan-out is the dominant write-amplification.** One gossip line to a channel with `S`
  subscribers is **one publish, S deliveries** by the broker. At `S` = tens of thousands on a global
  channel, a chatty channel is a fan-out hotspot — this is the metric to watch and the reason the
  comms-tier escape hatch exists. **Mitigation levers (documented, mostly deferred):** per-author rate
  limits (P8-A1, built), channel access narrowing the audience (P8-D3), and channel sharding (escape
  hatch). The *who* roster read is a Redis scan — bounded by online player count, cache/paginate at
  scale.
- **Presence write rate.** Each shard heartbeats its residents' presence TTLs. Batch the refresh (one
  pipelined Redis write per shard per heartbeat, not one per player per beat) so presence write rate is
  **O(shards / heartbeat-interval)**, not O(players). OQ-2 tunes the interval against the age-out window.
- **Tell volume** is point-to-point (one publish, one delivery) — not a fan-out concern; the durable
  stream's storage is the bound (offline tells accumulate until login; cap per-player backlog depth +
  TTL old durable tells).

### Integration risks
1. **The comms bus is a new cross-process trust + ordering boundary (security + distsys).** No single-
   writer serializes a cross-shard channel; correctness rests on the per-author sequence + per-subject
   broker order (P8-A3) and the world=source publish ACL (P8-A2). **security-auditor + distributed-
   systems-architect review 8.1** before any feature hangs off it.
2. **The gate subscription lifecycle must track the connection, not the handoff (distsys).** A
   subscription leak (a gate that doesn't unsubscribe on disconnect) is a slow resource leak and a
   ghost-presence bug; the subscription must be torn down on the **same** disconnect signal that drops
   the session. The handoff must **not** touch comms subscriptions (the whole reason for P8-D1-B). The
   distsys reviewer confirms the subscribe/unsubscribe pairs with the connection lifecycle in `gate.go`,
   and that a handoff is comms-transparent.
3. **Presence vs directory must not be conflated (distsys).** Tell routing uses the **directory**
   (authoritative, epoch); `who` uses **presence** (best-effort, TTL). Routing on presence would lose
   tells to a crashed shard (P8-A4). distsys reviewer confirms the two are never crossed.
4. **JetStream durability window vs Postgres (persistence).** Offline tells live in JetStream; mail
   lives in Postgres (P8-D6). The persistence-engineer confirms the durable-tell stream's
   retention/dedup window and the per-player delivered-cursor placement (character state vs Redis), and
   that the mail table follows the existing store patterns + optional/never-fatal degradation.
5. **NATS-down degradation (orchestration).** With NATS down, the comms bus is unreachable: channels and
   cross-shard tells **degrade to unavailable** (a clear "comms are temporarily offline" to the player),
   **never a crash** — exactly as hot reload degrades (`openContentBus`). `say`/`who`-within-shard still
   work (zone-local). orchestration reviewer confirms the never-fatal wiring and the player-facing
   degradation message.

### Cross-cutting reviewers (per the subagent-review-after-every-step rule)
- **comms/world-engineer (owning):** every slice — the comms bus, the topology wiring, the commands,
  presence, mail, the state subtree.
- **distributed-systems-architect:** **8.1** (the transport + the trust/ordering boundary, the
  publish/subscribe asymmetry), **8.2** (the gate subscription lifecycle vs the handoff —
  comms-transparency), **8.4** (presence TTL age-out + the directory/presence non-conflation),
  **8.5** (tell online/offline routing race + ordering), the Phase-8/Phase-10 boundary (every slice).
- **security-auditor:** **8.1** (publish ACL / impersonation gate), **8.3** (channel access predicate +
  text sanitization + rate limit), **8.5** (redelivery idempotency + bounded redelivery), **8.6**
  (receiver-side ignore funnel), the §2 threat model is the checklist; each slice carries its row.
- **edge/transport-engineer:** **8.2** (the gate-side comms client + the new producer into the writer
  path, slow-consumer backpressure), **8.3** (rendering channel frames on the existing socket path).
- **persistence-engineer:** **8.5** (durable-tell stream + delivered-cursor), **8.6** (mail store),
  **8.7** (the comms-state JSONB subtree + cadence).
- **orchestration-engineer:** **8.1** (NATS wiring, optional/never-fatal), the degradation behavior.
- **rpg-systems-designer (acceptance):** **8.3** (channel_defs are the right content shape — color/
  access/format/verb — and express a real channel set: gossip/newbie/auction/OOC).

---

## 4. Sliced implementation plan (ordered, independently committable)

The spine is **transport + topology → gate wiring → channels (content) → presence/who → tells (online
+ offline durable) → ignore/toggles → mail → comms state persistence**. Smallest, riskiest-first: 8.1
lands the bus + the trust/ordering boundary so the security + distsys reviewers sign off the boundary
**before** any feature hangs off it (the PHASE7 sandbox-first discipline). Each slice is a commit with
prior tests green and its owning + cross-cutting reviewers signing off. The **bare-engine invariant**
(a pack with no `channel_defs` ⇒ no channels; `empty_world_test.go` stays green) is a done-when on every
content-touching slice.

| Slice | Scope | Done when | Tests added |
|---|---|---|---|
| **8.1 — The comms bus transport + topology skeleton (the trust/ordering boundary)** | New `internal/commbus/`: the `Bus` interface (publish to a subject, subscribe to a subject/wildcard, close) + a **NATS impl** (subject taxonomy P8-D2, `telos.comms.*` root, **publish ACL: worlds publish, gates subscribe**) + a **`MemBus`** mirroring the semantics (per-sub ordered delivery) for hermetic tests + a **mem-JetStream stand-in** (append log + delivered-cursor). The **author-identity-is-engine-set** wire shape (P8-A2): the message payload carries an engine-set author id/name + a **monotonic per-author sequence** (P8-A3) + an idempotency key. **No channels/tells/who yet** — just the transport, the payload shape, and a round-trip across two in-process shards+gates over a `MemBus`. Optional/never-fatal wiring (NATS down ⇒ comms disabled), mirroring `openContentBus`. | A message published by shard-A's "world" is received by shard-B's "gate" subscriber over the `MemBus`; the author field is set by the publisher and a gate **cannot** publish on a `chan.*`/`tell.*` subject; per-author sequence is monotonic and a single subject preserves order; NATS-down ⇒ the bus is a nil/disabled no-op (no crash). Bare-zone unchanged. | **publish-ACL test (P8-A2, security)**; **per-subject-order + per-author-sequence test (P8-A3)**; mem-vs-(gated)NATS parity test; disabled-bus no-op test; round-trip-across-shards test. |
| **8.2 — Gate-side comms client + the writer-path producer (the sink)** | Wire P8-D1-B: each gate connection gets a **comms client** (subscribe-on-attach, **unsubscribe-on-disconnect** — the lifecycle tracks the **connection**, not the handoff), and a new producer that renders a comms message into a `ServerFrame_Output` on the **existing** writer path (`gate.go` writer). **The handoff must be comms-transparent**: a re-dial (A→B) does **not** touch the comms subscription. Slow-consumer: the comms producer respects the existing `out`-channel backpressure (one slow socket never stalls fan-out, P8-A1). **No channels defined yet** — drive it with a synthetic test message. | A comms message reaches a connected gate's socket; the subscription is **torn down on disconnect** (no leak); a **cross-shard handoff leaves the comms subscription untouched** (the player keeps receiving channel lines across an A→B walk — the load-bearing P8-D1 proof); a slow socket doesn't stall a sibling. | **subscription-lifecycle test (subscribe/unsubscribe paired with connect/disconnect, distsys)**; **handoff-comms-transparency test (distsys — the central topology proof)**; slow-consumer-no-stall test (edge). |
| **8.3 — Channels as content (`channel_defs`) + the channel verbs** | The `channel_def` table + loader mapping + a `channel` content-bus invalidation kind (P8-D3): ref, name, verb(s), color/format template, `access` predicate, `default_on`, `history` size. The **source-world publish path**: a channel verb (`gossip`) → world handler → **access check** (P8-A8) → **rate-limit** (P8-A1) → **sanitize text** as `$t` data (P8-A7) → publish to `telos.comms.chan.<ref>` with the engine-set author. The gate subscribes per the player's enabled channels and renders per the content format + **receiver access filter**. **Channels are CONTENT** — empty pack ⇒ no channel verbs. | A pack defines `gossip`; a player types `gossip hi` and a co-located AND a cross-shard player both see it rendered with the channel's color/format; speaking on a no-access channel is refused; flooding rate-limits the sender only; a `$`/ANSI/IAC in the text renders literally and can't forge a prefix; **no `channel_defs` ⇒ no `gossip` verb, `empty_world_test.go` green**. | **cross-shard channel delivery test (the Phase-8 done-when half)**; channel-access-denied test (P8-A8, security); rate-limit test (P8-A1, security); text-sanitization test (P8-A7, security); content hot-reload-of-a-channel test; **empty-boot-no-channels test**. |
| **8.4 — Cross-shard presence + `who`** | The Redis presence roster (P8-D4): each shard writes `presence:<playerId>={name,shardId,flags,lastSeen}` with a TTL on join, **batched heartbeat refresh**, eager removal on clean quit/leave (`zone.go` lifecycle). `cmdWho` becomes a **cross-shard roster read** (filtered by visibility), replacing the zone-local `z.who`. **TTL age-out** is the crashed-shard recovery (no explicit cleanup). Presence carries the AFK flag. | Two players on **different shards** both appear in `who` (the Phase-8 done-when, completed); a **crashed shard's players age out of `who`** after the TTL (the failure-mode demonstration); a clean quit removes the player from `who` immediately; a shard cannot write a non-resident's presence (P8-A4). | **cross-shard `who` test (done-when)**; **crashed-shard age-out test (P8-A4, distsys)**; clean-quit-eager-removal test; presence-write-authority test (P8-A4, security); batched-heartbeat-write-rate test. |
| **8.5 — Tells: online routing + offline durable (JetStream)** | `tell <name> <msg>` / `reply`: resolve target via **`directory.PlayerPlacement`** (P8-D5) → online: publish `telos.comms.tell.<target>` (core); offline: publish the **JetStream** durable stream with `Nats-Msg-Id` idempotency key + per-author sequence. Login-time **durable backlog drain** (paced) renders "while you were away…". **Idempotency**: the `Nats-Msg-Id` dedup window + the per-player **delivered-cursor** (P8-A5); **bounded redelivery** (max-deliver + backoff). The online→offline race favors durable (OQ-1). | A tell to an **online cross-shard** target arrives; a tell to an **offline** target is **delivered on next login, never lost, rendered exactly once in steady state** (the JetStream done-when — see OQ-4 for the precise at-least-once / usually-exactly-once / never-lost guarantee and its bounded crash-window exception); a **redelivery renders once** (the cursor gate); a target logging out mid-tell still gets it (durable fallback, not lost, P8-A4); per-sender order holds; a poison message parks (max-deliver). | **cross-shard online tell test**; **offline-tell-delivered-on-login test (done-when)**; **redelivery-idempotency test (P8-A5, security)**; logging-out-race test (P8-A4); per-sender-order test (P8-A3); bounded-redelivery test. |
| **8.6 — Channel toggles + ignore/AFK (receiver-side enforcement)** | `channels on/off <ref>`, `ignore <name>`, `afk [msg]` — world commands mutating the persisted comms-state subtree (P8-D7); the gate **caches** the filter set and re-subscribes channels on toggle. The **receiver gate** is the **authoritative ignore enforcement point** (P8-A6): every inbound channel/tell frame passes the receiver's ignore list before render — a **single funnel** so a new comms path inherits it. AFK auto-reply/marker. | Toggling `gossip` off stops its lines (gate unsubscribes); an **ignored sender's channel line AND tell are both dropped at the receiver** (P8-A6); a **new comms frame type is also ignore-filtered** (the funnel, not per-path); AFK marks the player in `who` and auto-replies a tell. | **toggle-unsubscribe test**; **receiver-side-ignore funnel test (channel + tell + a synthetic new type) (P8-A6, security)**; AFK test. |
| **8.7 — Mail (Postgres durable inbox) + comms-state persistence — DONE (2026-06-28)** | The `mail` table (P8-D6, `00007_mail.sql`) + send/list/read/delete CRUD on the existing store pool (optional/never-fatal: no Postgres ⇒ mail disabled); a "new mail" notify over the comms bus when online. The **comms-state subtree** (P8-D7) round-trips through `StateJSON` on the existing save cadence/ladder (channels-on, ignore list, AFK) — survives logout/login and a crash-rehydrate (landed in 8.6, `StateJSON.Comms`). | **DONE** — Sending mail to an offline player, who reads it on login (`mail`); mail list/read/delete works; a "new mail" ping reaches an online recipient; the comms-state subtree (channel toggles + ignore list) **survives logout/login and a crash-rehydrate**; no Postgres ⇒ mail cleanly disabled (not a crash). | mail send/read/delete round-trip test (hermetic MemStore + gated real-PG `TestMailCRUD`); offline-mail-on-login test; **comms-state-survives-restart test (persistence, 8.6)**; mail-disabled-without-postgres test; new-mail-notify test; **read/delete access-control test (security)**; **from-player engine-set test (security)**; recipient-refusal test. |

### 8.7 implementation notes (mail)

- **Table/migration:** `db/migrations/00007_mail.sql` — `mail(id, to_player CITEXT, from_player CITEXT,
  subject, body, sent_at, read_at NULL)`, index `mail (to_player, sent_at DESC)` for the inbox read.
  Mail is MUTABLE player state (engine mechanism), not a `*_def` content table. Down migration verified
  clean (down + re-up).
- **Store CRUD:** `internal/store/mail.go` (`*Pool`) — `SendMail`/`ListMail`/`ReadMail`/`DeleteMail`.
  Read/delete address a message by its 1-based **inbox position** (`OFFSET` within the player-scoped
  newest-first inbox), never a guessable id; every read/delete query is double-scoped by
  `WHERE to_player = $player` (the access control lives in the SQL). A hermetic `MemStore` mail impl
  (`internal/world/memstore.go`) mirrors the same semantics so the world-level journey is hermetic.
- **World commands:** `internal/world/mailcmds.go` — `mail` (list), `mail read <n>`, `mail delete <n>`,
  `mail send <name> <subject> | <body>` (one-line compose, `|` separates subject/body). All store I/O
  runs off the zone goroutine (the `cmdWho`/`sendTell` discipline). Registered with the comms commands
  (lowest priority).
- **Security:** `from_player` is engine-set from `s.character` (P8-A2); subject/body sanitized via
  `textsan.CleanLine` + a subject rune cap (P8-A7); recipient resolved via `directory.PlayerShard` — a
  never-seen name is refused (no directory ⇒ accept-and-store, durable-always); per-author rate limit
  shared with channels/tells (`commRateOK`, P8-A1). The new-mail notify rides
  `commbus.TellSubject(recipient)` (the gate sink) and carries **no body** (a pure presence ping, so no
  mail text leaks past the recipient's own inbox scoping).

**Adjustment / justification.** 8.1–8.2 land the **transport + the topology** first so the trust/
ordering boundary (the publish ACL, the per-author sequence, the gate-subscription-vs-handoff
lifecycle) is reviewed **before** channels/tells/who hang off it — the central P8-D1 proof (handoff
comms-transparency) is a 8.2 done-when, not an afterthought. 8.3 (channels) is the first user-visible
feature and completes **half** the phase done-when (cross-shard channel chat). 8.4 (presence/who)
completes the **other half** (cross-shard `who`) and demonstrates the crashed-shard age-out failure
mode. 8.5 (tells incl. offline JetStream) is the at-least-once/idempotency-heaviest slice and lands
after the bus is proven. 8.6 (toggles/ignore) layers the receiver-side enforcement funnel. 8.7 (mail +
state persistence) is last (it depends on the comms paths + the store). **If 8.5 proves large**, split
online-tell (core) from offline-durable-tell (JetStream) into two commits — the JetStream
idempotency/redelivery machinery is the heavier, security-reviewed half.

### 8.1 review — carried-forward obligations (security-auditor, sign-off 2026-06-28)

8.1's trust boundary cleared SOUND (publish ACL enforced identically across NATS/Mem/disabled, role
immutable, author-is-engine-set shape has no client path). The auditor flagged obligations the later
slices MUST satisfy for the boundary to hold end-to-end — fold each into the named slice + its review:
- **8.2 (wiring):** grep that `cmd/telos-gate` opens comms via `commbus.OpenGate` ONLY (never `OpenWorld`
  / the test-only `MemBus.WorldHandle()`), and `cmd/telos-world` via `OpenWorld`. A gate handed a world
  handle defeats the whole ACL.
- **8.3 (author stamp + channel access):** stamp `AuthorID`/`AuthorName` from the server-resolved live
  `*Entity`, NEVER from a client frame field; `Seq` from a server-held monotonic counter (the ACL stops a
  gate publishing, NOT a world publishing a badly-sourced author — that's 8.3's job). Render `Body` through
  the terminal sanitizer (P8-A7). Reject a channel ref not in loaded `channel_defs` BEFORE `ChanSubject`
  (it does no validation — P8-A8). Check the channel `access` predicate against the author entity.
- **8.4 (presence):** presence is deliberately NOT ACL-guarded (a gate CAN publish it today). Consumers
  MUST validate/key each presence frame by the player the publishing shard actually hosts, so a forged
  frame can't mark an arbitrary player online or evict a real one; tell routing reads the epoch-
  authoritative directory, NEVER presence (P8-A4).
- **8.5 (per-player subjects):** a gate subscribes ONLY to concrete `telos.comms.tell.<hostedPlayerId>`
  subjects for players it currently hosts — NEVER `telos.comms.tell.*` (subscribe is not ACL'd, so the
  concrete-subject choice is the only thing preventing a cross-player tell leak). Always set the
  `IdempotencyKey` (the `MemJetStream` stand-in does not dedup an empty key).

### 8.3 review — carried-forward obligations (security/edge) — RESOLVED in 8.6 (2026-06-28)

8.3 cleared SOUND (the 5 publish obligations enforced in order; prefix-forge/ANSI defense holds; the
`chan.*` wildcard can't match `tell.*`). The deferred **receiver HEAR-filter** was the carry-forward, and
**slice 8.6 has now closed it**:
- **DONE — the receiver HEAR-filter.** The gate **no longer subscribes the `chan.*` wildcard**. The
  SOURCE world computes each player's effective **{enabled ∩ hearable}** channel-ref set (it has the live
  `*Entity` + the `channel_defs`; `effectiveHearSet` in `internal/world/commsstate.go`) and publishes it,
  with the player's ignore list, to a **per-player config subject** (`telos.comms.config.<playerId>`,
  `commbus.ConfigSubject` / `ConfigPayload`). The gate subscribes its OWN concrete config subject (never a
  `config.*` wildcard) and re-subscribes exactly the named **concrete `ChanSubject(ref)`** channels —
  added/dropped on every toggle — under the same connection-scoped `commsClient.close()` teardown
  (`internal/gate/comms.go`). Recomputed + re-published on login, on a handoff arrival, and on every
  `channels`/`ignore` mutation. Handoff-transparent: the hear-set + ignore set live on the CONNECTION
  (survive a re-dial), and the comms-state subtree rides the handoff snapshot (`comms_state`), so a
  cross-shard walk does not reset toggles.
- **DONE — the CONTENT GUARDRAIL is now CLOSED.** A RESTRICTED-access channel
  (`access.require_flag`/`min_attr`) is now HEARD only by a player who passes its predicate (the world
  omits it from a non-hearer's hear-set, so the gate never subscribes it for them). **Restricted
  channels (admin/guild) MAY now ship.** Tested both speak-side (the publish access predicate, 8.3) and
  hear-side (`TestEffectiveHearSetEnabledIntersectHearable`, `TestHearFilterSubscribesOnlyHearSet`).
- **DONE — the receiver IGNORE funnel (P8-A6).** The gate caches the receiver's ignore list and drops
  EVERY inbound comms frame whose `AuthorID` is in it at a SINGLE chokepoint (`commsClient.ignored`),
  shared by the channel AND tell paths, so a synthetic NEW comms frame type inherits it
  (`TestIgnoreFunnelIsSingleChokepointForNewFrameType`).
- **Still open (non-blocking, 8.2 note):** surfacing a wholly-failed `openComms` as a one-line "comms
  unavailable" notice. DEFERRED — a disabled bus is byte-identical to pre-Phase-8 and detecting it from
  the `Bus` interface would couple the gate to bus internals; the gate stays a content-free sink.

---

## 5. Schema / loader / proto touchpoints

- **`internal/commbus/` (new package)** — the comms `Bus` interface + NATS impl + `MemBus` + mem-
  JetStream stand-in (8.1). Mirrors `internal/contentbus/` structurally; **does not** import or widen
  it. Subject root `telos.comms.*`; **gates subscribe-only, worlds publish-only** on chan/tell subjects.
- **`channel_def` content table + loader mapping (8.3)** — a new definition kind (ref, name, verb(s),
  color/format template, `access` predicate, `default_on`, `history`), parsed by the content mapper
  like the other def tables, and a new **`channel` content-bus invalidation kind** so channel edits
  hot-reload (the Phase-4 pattern). Empty packs ⇒ no channels.
- **`mail` table (Postgres, 8.7)** — `to_player`, `from_player`, `subject`, `body`, `sent_at`,
  `read_at`; CRUD on the existing `internal/store` pool; optional/never-fatal (PERSISTENCE.md already
  lists `mail` under the durable tier).
- **`StateJSON` comms subtree (8.7, P8-D7)** — a new **data-only** field on `character.go`'s
  `StateJSON` (the `Script`-subtree precedent): channel toggles, ignore list, AFK. Saved on the
  existing cadence; loaded on login; size-guarded. Pre-8.7 saves load with none (the established
  backward-compat default).
- **Redis presence keys (8.4)** — `presence:<playerId>` with a TTL, written by the resident shard,
  read by `who`. Operational/ephemeral (PERSISTENCE.md's Redis tier already names `presence`).
- **JetStream streams (8.5)** — `COMMS_TELL` (subject `telos.comms.dtell.<target>`), with a per-player
  durable consumer, `Nats-Msg-Id` dedup, bounded redelivery; the delivered-cursor (per-player; placement
  in character state vs Redis is a persistence-reviewer call — OQ-4).
- **New gate↔world Play frames?** — **none required for v1.** Comms output is the existing
  `ServerFrame_Output` text frame (the gate renders the channel format). Comms *input* is ordinary
  player input (the existing `ClientFrame_Input`) parsed by world command handlers. (Phase 9 adds the
  structured `Comm.Channel.Text` GMCP package — a *new* emit, reserved, not Phase 8.) The only new wire
  is the **comms bus** (NATS), not the Play stream.
- **`cmd/telos-world` / `cmd/telos-gate` wiring** — connect the comms bus from `cfg.NATS.URL` (the
  existing config), optional/never-fatal (mirror `openContentBus`). The gate gains a comms-bus
  connection (it is now a comms subscriber, P8-D1-B) — a new dependency the gate did not have (today
  the gate only knows the directory + the Play pool).

---

## 6. Open questions for sign-off — RESOLVED (owner sign-off 2026-06-28)

1. **OQ-1 — Tells: DECIDED → durable-always.** Every tell is a JetStream message; online delivery is
   just a fast durable consumer. This eliminates the online→offline logout race and the dual code path
   (correctness over the lighter-JetStream split). **P8-D5 / slice 8.5 adopt durable-always** — there is
   no separate NATS-core online-tell path; "online" is simply the durable consumer being live. The 8.5
   split-if-large note (core vs durable) is therefore moot; if 8.5 is large, split the
   send/publish path from the login-drain/cursor path instead.
2. **OQ-2 — Presence TTL: ACCEPTED rec.** Heartbeat well within the directory's ~15s lease cadence;
   crashed-shard age-out ≤ ~30s.
3. **OQ-3 — ACCEPTED rec:** per-channel subscribe (the broker does the fan-out cut); revisit only if
   toggle-churn subscription cost dominates under load.
4. **OQ-4 — ACCEPTED rec:** the **world** drains the offline backlog on login and emits via the same
   source path (the gate stays a pure sink); the per-player delivered-cursor lives in **character `state`
   JSONB** (durable with the character, rides the ladder). Persistence-reviewer to confirm at slice 8.5/8.7.
   **Precise durability guarantee (8.5 review wording):** a durable tell is **render-at-least-once /
   usually-exactly-once / never-lost** — NOT a literal exactly-once durable guarantee. It is never lost
   (durable from publish until acked); it renders exactly once in steady state (the strictly-greater
   cursor gate suppresses any redelivery whose Seq ≤ the stored cursor — the Nats-Msg-Id dedup window
   covers recent redeliveries, the persisted cursor covers later ones). The **one exception** is a crash
   in the narrow window between the gate emit and the next persistence of the advanced cursor (the cursor
   advances in memory immediately but rides the save cadence to durable storage): on restart that single
   tell can re-render **once** before the re-advanced cursor re-suppresses it — **bounded** (never a loop,
   never a storm).
5. **OQ-5 — ACCEPTED rec:** key comms identity on the directory's player id today (the stub login name
   until Phase 14 auth); accept the Phase-14 migration for block lists / friends.

---

## 7. The Phase-8 ↔ Phase-10 boundary (read this before conflating the two buses)

Phase 8 and Phase 10 both put a bus over NATS. They are **deliberately separate**:

| | Phase 8 comms bus (`internal/commbus`) | Phase 10 world-event bus (WORLD-EVENTS.md) |
|---|---|---|
| **Carries** | player-authored chat, directed tells, presence, mail notifies | world-state consequences (a boss death → region change), `signal_*`, remote effect commands |
| **Endpoints** | **gate ↔ world** (gate=sink, world=source) | **zone ↔ zone / director** (single-writer scopes) |
| **Owner** | the connection (gate) + the source world | the **director** tier (leader-elected) + zone actors |
| **Scopes** | per-player / per-channel | region / world (supra-zone) |
| **Durability** | core for channels, JetStream for offline tells, Postgres for mail | `transient` (core) + `durable` (JetStream, idempotent, ordered) per the scoped design |
| **Ordering** | per-author / per-subject | per-scope (single-writer per scope) |

They **may share the NATS server and JetStream**, but **not the subject space, the payloads, or the
code**. A Phase-8 channel line is *not* a world event; a Phase-10 region event is *not* chat. Building
comms on the (not-yet-existing) Phase-10 bus, or vice versa, would entangle a player-facing chat layer
with a single-writer world-state layer — different invariants, different failure modes, different
owners. Phase 8 ships the **player-presence/channel layer only**; Phase 10 extends the Phase-6 in-zone
event bus to cross-zone scopes. The `Bus`-interface boundary in 8.1 is exactly what keeps them
swappable and separable.

---

## 8. Done-when (the phase capstone) — **PHASE 8 COMPLETE (2026-06-28)**

All four ROADMAP done-when items below are met (8.1–8.7 landed), plus the mail capstone from 8.7
(durable inbox: send/list/read/delete, offline-mail-on-login, online new-mail notify, player-scoped
read/delete access control, from-player engine-set, storeless degrade) and the abuse/safety capstone.

The ROADMAP Phase 8 done-when, made concrete on this plan:

1. **Cross-shard channel chat** — two players on **different shards** (A and B) both `gossip` and each
   sees the other's lines, rendered with the content channel's color/format (8.3). A pack with **no**
   `channel_defs` has **no** channels and `empty_world_test.go` stays green.
2. **Cross-shard `who`** — those two players, on different shards, **see each other in `who`** (8.4).
3. **Crashed-shard presence age-out** — when shard B crashes, its players **disappear from `who`** after
   the presence TTL, with no explicit cleanup (the self-healing failure-mode demonstration, 8.4).
4. **Offline tell, never lost, rendered exactly once in steady state** — a `tell` to an offline player
   is **persisted (JetStream) and delivered on next login**, and a redelivery (reconnect/ack-loss)
   **renders once** via the strictly-greater cursor gate (8.5). The guarantee is precisely
   **render-at-least-once / usually-exactly-once / never-lost** (OQ-4): the sole re-render is the bounded
   crash window between the gate emit and the next cursor persistence — never a literal exactly-once
   durable guarantee.

And the abuse/safety capstone, demonstrated under test: a **flooding** player is rate-limited without
degrading others (P8-A1); an **impersonation** attempt cannot change a rendered author (P8-A2, the
world-is-source publish ACL); an **ignored** sender's channel line and tell are both dropped at the
receiver gate (P8-A6); **text injection** (ANSI/IAC/`$`) renders literally and cannot forge a channel
prefix (P8-A7); a **handoff is comms-transparent** (a player keeps receiving channel lines across an
A→B walk, P8-D1); and **NATS down** degrades comms to a clean "temporarily offline," never a crash.


---

<a id="phase-9"></a>

# Phase 9

_(archived from docs/PHASE9-PLAN.md)_

# Phase 9 — GMCP (rich-client data) — plan

Lights up the GMCP plumbing already reserved in the wire: `api/proto/.../play.proto` carries
`GmcpIn{pkg,json}` (client→world), `ServerFrame.gmcp = GmcpOut{pkg,json}` (world→gate), and the
client-capabilities fields (`gmcp`, `gmcp_supports`, `mccp`, width/height/charset/mtts). The telnet
layer currently REFUSES all option negotiation (`handleIAC`), so the work is: negotiate option 201,
parse/encode the `IAC SB 201 … IAC SE` subnegotiation, track per-connection `Core.Supports`, and emit
the package payloads from the engine's existing event/output path.

**Done-when (roadmap):** Mudlet shows a live vitals gauge + a minimap that updates as you walk
(= 9.1 + 9.2 + 9.3). v1 scope (user decision 2026-06-29): the **full client set, 9.1–9.5**. `Mud.*`
(quest/group/cooldowns) defers to its dependent phases (progression/party); `Mud.Map` may ride 9.3.

## Testing mandate (binding — see memory [[testing-standard]])
Every slice ships tests across ALL applicable tiers as it lands: unit + boundary/error matrices,
**property/fuzz** for the subneg parser and any payload (de)serializer, integration (gate↔world),
**e2e** black-box (a client negotiates GMCP and asserts the messages), and **chaos** (malformed/
oversized subneg, a client lying about supports, GMCP under handoff). Per-slice subagent reviews
(owning edge-engineer + a cross-cutting expert) before each commit. No deferring a slice's own tests
to the end-of-roadmap hardening sweep.

## Slices

### 9.1 — Transport foundation + Core.*
- Telnet: answer `IAC WILL GMCP` / handle `IAC DO/DONT GMCP`; parse `IAC SB 201 <pkg> SP <json> IAC SE`
  → `GmcpIn`; encode `GmcpOut` → `IAC SB 201 <pkg> SP <json> IAC SE`. Bounded + fail-closed on malformed.
- Gate: forward `GmcpIn` to the world; maintain the per-connection `Core.Supports` set; a
  per-connection **filtered encoder** drops any `GmcpOut` whose package the client didn't advertise, so
  the engine can always emit without checking support.
- `Core.Hello` (client name/version), `Core.Supports.Set/Add/Remove`, `Core.Ping`→`Core.Ping`,
  `Core.Goodbye` on disconnect.
- Tests: **FuzzGmcpSubneg** (hostile IAC/SB framing → no panic, bounded, fail-closed); supports-filter
  unit matrix; integration (gate negotiates + forwards a round-trip); e2e handshake; chaos (truncated/
  oversized subneg, unadvertised-package drop, supports survives a cross-shard handoff).

### 9.2 — Char.* HUD
- `Char.Vitals` / `Char.Stats` / `Char.Status` built from the entity's resources/attributes/combat
  state, emitted from the SAME event that drives the text prompt (one change → prompt + GMCP), so they
  never drift. `Char.StatusVars` declares Status labels for generic client rendering.
- Tests: payload builders (unit, content-driven resource set — no hardcoded hp/mp); integration (a
  vitals change emits both the prompt and `Char.Vitals`); e2e (gauge updates on damage).

### 9.3 — Room.Info (minimap) + the room-identity prereq
- A stable **ProtoRef↔integer-id** room table (the GMCP `num`/`exits` ids) and room **coord** (x,y,z)
  storage + authoring. `Room.Info` on every move: num/name/zone/environment/coord/exits/doors.
  `Mud.Map` (zone blob) optional here.
- Tests: id-table unit (stable, collision-free, survives reload); coord round-trip; move→`Room.Info`
  exits/coord correctness; e2e (walking updates the map data).

### 9.4 — Char.Items.* panels + deltas
- `Char.Items.Inv/Contents/Room` full lists + `Char.Items.Add/Remove/Update` incremental deltas as
  inventory/equipment/room items change. Item entry `{id,name,icon,attrib}`.
- Tests: item-entry builder + delta computation (unit); get/drop/wear/loot → correct deltas
  (integration); chaos (delta correctness under churn / rapid mutation).

### 9.5 — Comm.Channel.Text routing
- Route Phase-8 channel lines to `Comm.Channel.Text {channel,talker,text}` for client tab routing;
  `Comm.Channel.List/Players`. Reuses the Phase-8 bus + the receiver hear-filter (a muted/ignored
  channel emits no GMCP either).
- Tests: payload (unit); gossip → `Comm.Channel.Text` to a supporting client only (integration); the
  hear-filter/ignore funnel suppresses the GMCP too (chaos-adjacent); e2e.


---

<a id="phase-10"></a>

# Phase 10

_(archived from docs/PHASE10-PLAN.md)_

# Phase 10 — Orchestration (directors, scopes, cross-zone events) — plan

Supra-zone state + cross-zone consequences, plus dynamic zone placement. Designs:
[WORLD-EVENTS.md](WORLD-EVENTS.md) (scopes/directors/event bus) + [PLACEMENT.md](PLACEMENT.md) (placement).

**Done-when:** a boss death in one zone ripples a region-wide change across zones on different shards,
and survives a director restart.

**Scope (user decision 2026-06-29): the FULL phase — orchestration (10.1–10.5) PLUS dynamic zone
placement (10.6).** Replaces static `TELOS_ZONES` with claim-from-pool + director rebalancing.

**The golden rule (load-bearing):** cross-scope effects are MESSAGE-PASSING, never shared mutation. A
script never reaches into another zone — it SIGNALS; the engine routes; each affected zone applies the
consequence LOCALLY in its own goroutine. The single-writer / no-cross-zone-handle / actor-per-zone
invariants are intact; cross-scope power comes entirely from messages, directors, and local application.

The actor pattern now spans FOUR tiers: gate (edge), zone (simulation), **director (orchestration)**,
account (auth, Phase 14). A director is an actor internally (inbox + tick + sandboxed Lua VM, same model
as a zone) but hosted out-of-band from the simulation shards in the new `telos-director` service.

## Testing mandate (binding — see memory [[testing-standard]])
Every slice ships tests across ALL tiers as it lands: unit + boundary, property/fuzz where there's a
parser/serializer, **gated real-infra** (Redis leader election, NATS/JetStream routing, PG scope state —
the W7 pattern), **chaos/failure-injection** (director crash → standby promotion; shard-down → durable
redelivery; lease-expiry → orphan re-claim; split-brain → CAS arbitrates), and the e2e milestone. Per-slice
subagent reviews. New persistence fields are round-trip-verified through real Postgres (the field-drop class).

## Slices

### 10.1 — Director tier + scope state + leader election
- `cmd/telos-director` + the director ACTOR (inbox + tick + sandboxed Lua VM); leader election (a Redis
  lease — exactly one live owner per region/world scope, standby failover on crash).
- `world_state(key, value JSONB, version)` + `region_state(region_id, key, value JSONB, version)` tables
  (goose migration), versioned for the same optimistic-concurrency backstop as characters. The director
  owns + persists them. No event routing yet.

### 10.2 — Scoped event bus (cross-zone transport) — **COMPLETE**
- Scope channels `world.<event>` / `region.<id>.<event>` / `zone.<id>.<event>` over NATS core
  (`transient`) + JetStream (`durable`: at-least-once, idempotency keys, per-scope ordering, director =
  sequencer). The signal/broadcast plumbing, extending the Phase-8 commbus pattern.
- **DONE:** `internal/scopebus` — ONE subject per scope (`telos.scope.world`/`.region.<id>`/`.zone.<id>`),
  the event name + payload in the body (channel-subject pattern, no wildcard gymnastics), a subject-
  injection guard on the id. **10.2a transient** (`Signal`/`Subscribe` over `commbus.Bus`). **10.2b
  durable** (`SignalDurable`/`SubscribeDurable` over JetStream): a new `WORLD_EVENTS` stream binds
  `telos.scope.>` (`commbus.NewScopeJetStream`/`OpenScopeJetStream`, sharing the tell stream's
  Publish/Consume machinery via `dialJetStream`); per-process `<source>:<seq>` idempotency key, a
  `DurableEvent{Backlog,Key,...}` for consumer-side apply-once dedup + restart catch-up, NAK-redelivery.
  `scopebus.SubjectRoot == commbus.ScopeSubjectPrefix` (one source of truth, no binding/publisher drift).
- Tests: hermetic (MemBus + MemJetStream — round-trip, region isolation, survives-late-subscriber,
  NAK-to-success, monotonic keys, requires-config) + gated real-NATS (`WORLD_EVENTS` offline→online
  backlog replay, now in the comms CI job).

### 10.3 — region_defs + zone read-replica + signal-up
- `region_defs` content (id + member zones) in the definition tables. The zone-side CACHED read replica
  of region/world state (eventually consistent, lock-free) → Lua `world.flag("x")` / `region:get("k")`.
  `signal_region`/`signal_world` (a zone commands UP to the director; never mutates cross-scope).
- **10.3a DONE (regions as content):** migration 00009 `region_defs(ref,pack,body)` + `RegionDTO` +
  `Pack.Regions`/`LoadedContent.Regions` (last-write-wins merge-by-ref) + store read/write/strip +
  demo "heartlands" region (midgaard+darkwood). Tests: demo-load + merge-override (hermetic) + gated PG
  round-trip & idempotency. Modeled on the 8.3 channel_defs precedent.
- **10.3b DONE (zone read-replica + Lua reads):** per-zone `scopeReplica` (world+region maps + regionID),
  written only by `applyScopeDelta` (posted `scopeDeltaMsg`, applied on the zone goroutine — golden rule).
  Shard-side `scopeReplication` (`WithScopeBus`) derives zone→region from region_defs, subscribes to the
  world + hosted-region scopes, routes a director broadcast DOWN (world→all zones, region→members). Lua
  read-only `world`/`region` globals: `world.flag`/`world.get`/`region:get`/`region.id` (luascope.go).
  Wired in cmd/telos-world. Tested hermetic (-race): Lua reads, region isolation, delete, shard routing.
- **10.3c DONE (signal-up):** `signal_region`/`signal_world` Lua globals → enqueue (zone goroutine) → a
  shard `signalLoop` publishes DURABLE (`SignalDurable`) off-goroutine. Region-less / busless = clean
  no-op. cmd/telos-world wires the durable tier (OpenScopeJetStream, run-unique source). Tested: signal
  up to region+world with payload via a durable consumer; region-less no-op. The director-side apply is 10.4.

### 10.4 — Director writes + broadcast + remote effects — **COMPLETE**
- The director's authoritative write API + broadcast + remote effects.
- **10.4a DONE:** `director/signals.go` — `WithScopeBus` + `WithSignalHandler`. A `SignalHandler` (the
  "director script") runs on the actor goroutine with an `API`: `Get`, `Set` (persist via the 10.1 CAS +
  broadcast the delta DOWN as `scopebus.EventStateSet`), `Broadcast` (a custom remote-effect event). The
  durable signal-up consumer is bound to LEADERSHIP (only the leader consumes+applies; stable consumerID
  so a restart resumes), applied apply-once via a per-source idempotency-key high-water. The DOWN vocab
  (`EventStateSet`/`StatePayload`) moved to scopebus as the one shared contract. cmd/telos-director wires
  the bus (nil handler for now — a content-defined director script is future). Tested: boss-ripple apply +
  broadcast, remote-effect, apply-once.
- **10.4b DONE:** `on_world`/`on_region` Lua handlers (luaentry_triggers registration binds → namespaced
  es.handlers keys) fire on a director's custom (non-state) broadcast — the shard splits EventStateSet
  (read-replica) from any other event (a `scopeEventMsg` → `fireScopeEvent` runs every registered handler
  with the payload as `ev`). Tested: on_world fires with payload; remote-effect routing.

### 10.5 — Capstone (the done-when) — **COMPLETE**
- `internal/world/orchestration_capstone_test.go`: the full boss ripple, hermetic + `-race` (8x stable). A
  zone signals a kill UP (durable); director #1 counts 2 (persisted); director #1 is STOPPED and director
  #2 brought up on the SAME store + transports — it RELOADS the count + RESUMES the durable stream; the 3rd
  kill crosses the threshold and a scripted mob reacts to the director's remote effect (race-free via a Go
  callback). Proves durable signal survives the restart, persisted state reloads, apply-once across failover
  (exactly 3, never 4), the gate persists.

### 10.6 — Dynamic zone placement (director as zone coordinator) — **COMPLETE (core; drain executor deferred)**
- `internal/placement`: `ClaimFromPool` (decentralized liveness — a world server claims free zones via the
  directory CAS at boot, standby if none) + `Plan` (the coordinator decision: assign unclaimed/orphaned to
  the least-loaded, rebalance busiest→idlest past a hysteresis threshold; count-based v1) + `Observe`.
  `directory.ListShards` (the live-fleet view). cmd/telos-world boots via claim-from-pool (single server
  wins all — smoke/e2e unchanged); cmd/telos-director runs a leader-only observe→plan→log coordinator.
- **Deferred (documented next mile, PLACEMENT.md):** EXECUTING a rebalance move — draining a zone's live
  players to the new owner via the cross-shard handoff fanned over the zone (`Shard.Drain`) — and runtime
  zone-add for a standby reclaiming an orphan live. The boot-time claim + the optimizer brain are done.

## Open decisions (WORLD-EVENTS §10 flagged three; surface + settle with the owning engineer as hit)
- D1/D2/D3 (the doc references a §10 that isn't written): the signal coalescing/debounce shape; the
  read-replica delivery guarantee; the reliability-tier default. Settle per-slice with the
  distributed-systems-architect, then lock in a test.


---

<a id="phase-11"></a>

# Phase 11

_(archived from docs/PHASE11-PLAN.md)_

# Phase 11 — Character progression & chargen

The opener of **Track D** (progression & economy) and the largest single content area the gap analysis
surfaced ([GAME-SYSTEMS-GAP-ANALYSIS.md](#gap-analysis) §5, gap **[G6]**). It depends only
on the Phase-6 event bus + Phase-7 Lua (both done) — nothing in Track C blocks it.

Status: **COMPLETE (11.1–11.5 + capstone landed, CI green).** Chargen (11.6) deferred to Phase 14 as planned.

**Settled scope (the 5 forks, confirmed):**
1. **Core progression: 11.1–11.5 + the capstone. Chargen (11.6) is DEFERRED to pair with Phase 14**
   (account/login) — its interactive front end wants the auth flow; the grant/bundle machinery does not.
2. **Spell slots / per-class casting is DEFERRED** to a dedicated casting slice (and the hairy multiclass
   fractional-caster-level slot table regardless). Phase 11 stays on the track/grant/bundle core.
3. **One GENERIC `bundle_defs(kind, …)` table** (a kind discriminator), one loader path — not per-kind tables.
4. **`OnLevel`/`OnTrackStep`/`OnSkillUse` are new engine `eventKind`s** (the engine fires them).
5. Chargen depth — N/A this phase (deferred per #1).

## The binding shape (settled — do not re-litigate)

From the gap-analysis decisions (memory `gap-analysis-decisions`, settled 2026-06-26):

- **N independent advancement TRACKS.** Character level, guild/class levels, use-based skills — each a
  separate track. There is no single "level" the engine privileges.
- **`level` is an ORDINARY ATTRIBUTE.** Some tracks happen to raise a `level` attribute; a use-based MUD
  has tracks with no `level` at all. The engine must never grow a `level` concept. (The risk to watch.)
- **A level-up is an OP-LIST.** Grants reuse the whole effect-op interpreter + the event bus — they are
  not a new code path. "Which event feeds the track" is the only thing that differs between XP-auto,
  train-at-trainer, point-buy, and use-based — all four are content, not four engine paths.
- **Bundles are content.** `class_def`/`race_def`/`background_def`/`feat_def`/`talent_def` are content
  bundles of grants (+ track definitions); the engine knows only the KIND "bundle", never "fighter".
- **Acceptance target: the 5e SRD.** A 5e class+race expressed as pure content is the design proof
  (memory `srd-5e-as-design-target`), while staying expressible for Pathfinder / use-based / WoW-like
  (memory `extensibility-across-game-systems`).

## What already exists (the foundation Phase 11 builds on)

- **Effect-op interpreter** (`internal/world/effect_op*.go`): a registry of ops (`deal_damage`, `heal`,
  `modify_resource`, `apply_affect`, `if`, `chance`, `check`, …) run as op-lists. Phase 11 ADDS grant ops.
- **Per-entity attribute base override** (`setAttrBase`): chargen/point-buy/level-up write a stat's base
  here. `modify_attribute_base` is a thin op over it.
- **The in-zone event bus** (`event.go`): `OnHit`/`OnDamageTaken`/`OnKill` are LIVE; the closed `eventKind`
  set + the custom-event lane (`mud.fire`/`on(name,fn)`) exist. Phase 11 ADDS the progression events.
- **Abilities** granted as command verbs (per-shard ability registry + command table). `grant_ability`
  adds to an entity's granted set.
- **Character `state` persistence** already carries `templates`/`attributes`/`skills` subtrees — the
  persisted shape for tracks/grants largely exists; verify + extend in 11.2.

## Testing mandate (binding — memory `testing-standard`)

Every slice ships tests across all tiers (unit + table-driven + fuzz where a parser/threshold is involved
+ gated PG round-trip for any new def-table + the e2e/journey for the capstone) and per-slice reviews
(owning engineer = `progression-engineer`; cross-cutting = `persistence-engineer` for def-tables/state,
`rpg-systems-designer` for the system-fidelity check, `scripting-engineer` for any Lua surface). New
features mean new tests of every tier.

## Slices

### 11.1 — Grant ops [G6b] (the foundation)
The additive effect ops a level-up / bundle / chargen runs, reusing the effect-op interpreter + op-list
machinery. These two wrap EXISTING persisted seams (`setAttrBase`, `setFlag`) so they survive a reload by
construction (the state subtree is restored, not the grant re-run):
- `modify_attribute_base` (raise/lower a stat's per-entity base — the constraint explicitly names this),
  `set_flag`/`clear_flag` (the open-set named-flag grant).
- The other grant ops are coupled to mechanisms built in later slices and land there: **`grant_track`** with
  the track machinery (11.2); **`grant_ability`** with the per-entity ability-ownership model + the
  invocation gate (11.4 — content abilities currently dispatch globally, so the ownership gate is a real
  mechanism, not a thin op); **`grant_resource`** (= `modify_attribute_base` on the pool's max attr +
  set-current) folded into 11.2/11.4.
- **Done when:** an op-list raises an entity's `strength` base and sets a named flag, and both survive a
  save/reload.

### 11.2 — `track_defs` + the track machinery [G6a]
A track = `{ progress_attr, thresholds[], grants_per_step }` (`progress_attr` is just an attribute —
`xp`, `mining_skill`, `warrior_xp`). The engine watches the progress attribute, and on a threshold
CROSSING fires `OnTrackStep` (and `OnLevel` when the step grants a `level`-attr bump) → runs the step's
grant op-list (11.1). A per-entity track SET in `state`, persisted; the threshold check is the new
mechanism, not a new code path for grants.
- new def-table `track_defs` (migration + DTO + loader + store round-trip, mirroring `channel_defs`).
- **Done when:** an entity with an XP track crosses a threshold (progress attr raised by any op), the
  engine fires the step, applies the grants, advances to the next threshold, and the new level/grants
  survive a restart — exactly once across the reload (no double-grant).

### 11.3 — Progression events [G6d] (the "which event feeds the track" glue)
Wire the events that DRIVE tracks, so every advancement mode is just an event→op binding:
- `OnKill → modify_attribute(xp, +reward)` (XP-auto — Diku/5e). `OnKill` already exists; wire an XP grant.
- `OnSkillUse → chance(p, modify_attribute(skill, +1))` (use-based — LP/Discworld/BRP). `OnSkillUse` is a
  NEW engine event fired when a skill-tagged ability/check is used.
- `OnLevel`/`OnTrackStep` as the engine events 11.2 fires (content subscribes to run flavor/unlocks).
- **Done when:** killing a mob raises XP and auto-levels one track; using a skill has a chance to improve
  it (a second, level-less track) — both content-defined, no engine code per mode.

### 11.4 — Template/bundle def-tables [G6c]
`class_def`/`race_def`/`background_def`/`feat_def`/`talent_def`: content bundles of grants (attrs/
resources/abilities/flags/affects) + track definitions, applied when chosen or when a track step is
reached. `grant_track` (11.1) adds a class track at runtime; entry prerequisites are a `check` against
attributes (prestige class / multiclass requirement).
- one generic `bundle_defs` table with a `kind` discriminator (class/race/…) OR per-kind tables — a fork
  (see §Decisions). Bundles are pure content; the engine knows only "apply this bundle's grants".
- **Done when:** applying a `race_def` + a `class_def` bundle to a fresh entity grants the right
  attrs/abilities/track, a "join guild at 5" content action adds a second class track mid-life, and a
  prestige track's entry check gates it — all content, surviving a restart.

### 11.5 — The four advancement modes, demonstrated as content
Prove the union abstraction: the same track machinery expresses all four modes with no engine branch.
- XP-auto (11.3) ✓; **train-at-trainer** = an NPC ability that spends a currency resource and runs the
  step grant op-list directly (no auto-threshold); **point-buy** = a level grants a `points` resource +
  a `spend_points` ability that `modify_attribute_base(+1)` per point (WoW talents / 5e ASIs).
- **Done when:** the demo pack ships one track per mode and a journey test drives each (kill→auto-level,
  visit-trainer→train, spend-points→stat bump, use-skill→improve).

### 11.6 — Chargen flow [G6e] — **DEFERRED to Phase 14** (pairs with account/login)
The creation-time grant flow: choose race/class/background, point-buy stats, apply the bundles. Its
interactive front end wants the Phase-14 account/login flow, so it lands there. The grant/bundle machinery
it drives is built here (11.1/11.4), so Phase 14 chargen is "wire the existing bundles into a creation UI".

### 11.x — Capstone (the done-when)
A character is **created from a class+race bundle**, **gains XP on kills** (auto-leveling one track),
**trains a skill through use** on another track, and **the build survives a restart** — all content.
Demo content + an e2e milestone journey + a restart-survival test (mirrors the Phase-10 capstone rigor).

## Builds on / relates to

Phase 6 (check primitive + event bus) · Phase 7 (Lua + the effect-op interpreter) · Phase 4 (persistence —
the track/grant state must survive a restart, the capstone's done-when). Acceptance target: the 5e SRD
(memory `srd-5e-as-design-target`). The loot/spawns that hang off `OnKill` are **Phase 12**; auth/website
+ the real chargen front end are **Phase 14**.


---

<a id="phase-12"></a>

# Phase 12

_(archived from docs/PHASE12-PLAN.md)_

# Phase 12 — Loot & scheduled spawns

The second slice of **Track D** (progression & economy). Two features that combine into the target "key
boss" experience — *kill the weekly boss for months chasing the legendary sword*: a **modern-MMO loot
system** (always-good drops with a rare legendary + bad-luck protection) and **long-timer scheduled
spawns** (a weekly world boss). Design: [LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md).

Status: **COMPLETE (12.1–12.4 + capstone landed, CI green).**

**Settled forks (confirmed):** (1) FULL scope (12.1–12.4 + capstone), 12.3 deliberately coarse (item level
+ a few affixes). (2) Eligibility v1 = "dealt any damage" via the existing damage/threat record + a content
tag hook. (3) Delivery = personal-direct to each looter (corpse holds only the body). (4) Scheduled-spawn
command path = a 10.4 remote-effect broadcast (zone reacts by spawning), no new transport. (5) Rare+ drops
persist, common ages out (ephemeral). (6) The `on_roll(ctx)` Lua escape hatch is DEFERRED — ship the
declarative resolver first.

## Why now (what it builds on — all in place)

- **The death path + `OnKill`** (Phase 6/11) and `makeCorpse` (death.go) — which already records the
  killer. The loot resolver hooks here; the killer/threat record is the loot-ownership seam.
- **Flyweight item instances + per-instance deltas** (Phase 4) — a rolled item is the shared prototype +
  a per-instance quality/affix delta; the prototype stays shared, only the delta varies.
- **The director tier + durable scope state** (Phase 10) — `world_state`/`region_state` (versioned,
  restart-safe) is exactly where `next_spawn_at` lives; the director heartbeat checks due schedules and
  commands the owning zone (the remote-effect path from 10.4).
- **The def-table precedent** (channel→region→track→bundle) — every new table (`loot_table_defs`,
  `rarity_tier_defs`, `affix_defs`, `spawn_schedule_defs`) is the same ref/pack/JSONB-body shape.
- **The per-zone deterministic RNG** (Phase 7) — the resolver rolls on the dying mob's zone goroutine.

On-pillar throughout: every table, tier, affix, and schedule is CONTENT; the engine runs the resolver +
the scheduler and names no boss, item, or tier.

## Testing mandate (binding — memory `testing-standard`)

Every slice ships tests across all tiers: unit + table-driven for the resolver math (weights, pity curve,
quality rolls — seeded RNG makes these deterministic), gated PG round-trip for each new def-table + the
pity/schedule persistence, and a milestone/e2e for the capstone. Per-slice reviews: owning engineer =
`progression-engineer`; cross-cutting = `persistence-engineer` (def-tables + pity/schedule state),
`distributed-systems-architect` (the director scheduler's restart-safety + idempotency),
`security-auditor` (loot can't be duped/forced; eligibility can't be gamed).

## Slices

### 12.1 — Rarity tiers + loot tables + the resolver core
The loot system's spine: content tables + the on-death resolver (no quality variance or pity yet).
- `rarity_tier_defs` (ordered named tiers: common→…→legendary, each with weight/color) + `loot_table_defs`
  (a list of independent `roll`s: `guaranteed` / `chance` / `weighted_one` / `weighted_n`, with an optional
  `quality_floor`). A mob prototype references a loot table by ref.
- The resolver runs on death (the dying mob's zone goroutine, seeded RNG): eligibility → per-looter
  (personal loot, each eligible player rolls independently) → resolve each roll → deliver to that player
  (the corpse holds only the body — no contested pickups, §5 step 6).
- **Done when:** a mob with a loot table dies and each eligible player independently receives their own
  rolled drop (a guaranteed rare+ item), delivered to them — content-defined, deterministic under a seed.

### 12.2 — Pity (bad-luck protection)
The bounded "grind for months": a `chance` roll may carry a `pity` spec `{key, step, cap}` — each kill
that misses nudges the chance up by `step` (to `cap`); a hit resets it. Counters are per-character
(`state.loot_pity` JSONB, riding the durability ladder), read+updated by the resolver.
- **Done when:** repeated kills without the drop raise the effective chance along the pity curve, a hit
  resets the counter, and the counter survives a save/reload — proven deterministically under a seed.

### 12.3 — Item quality variance (affixes)
The within-tier "always good, but it varies": on drop, the resolver rolls an instance quality — an item
level + a set of affixes from `affix_defs` — written into the item's per-instance delta (the prototype
stays shared). A legendary rolls from a richer pool. Optional per item (`quality_spec`). Coarse v1 (item
level + a small affix set); deep affix systems deferred.
- **Done when:** two drops of the same prototype carry DIFFERENT rolled affixes/level in their instance
  deltas, the delta round-trips through persistence, and a legendary rolls a richer set — under a seed.

### 12.4 — Director-owned scheduled spawns
The long-timer boss: `spawn_schedule_defs` content (`interval_after_death`/wall-clock, `on_missed`,
announce). The director persists `next_spawn_at` in world/region scope state, checks due schedules on its
heartbeat, commands the owning zone to spawn the boss (the 10.4 remote-effect path → a zone spawn), and on
the boss's death event computes the next time. Restart-safe: on startup the director loads schedules and
applies `on_missed` (`spawn_if_overdue` vs `skip_to_next`).
- **Done when:** a scheduled boss spawns when due, its death schedules the next spawn, and a director
  restart mid-interval resumes the schedule correctly (overdue → spawns; not-yet-due → waits) — no double
  spawn, no lost schedule.

### 12.x — Capstone (the done-when)
The combined scenario (§7): a weekly boss spawns on schedule, a raid kills it, and each eligible player
receives personal loot — a guaranteed rare+ item with rolled affixes — while independently rolling the
rare legendary with a working pity timer that survives a restart. Demo content + an e2e milestone + the
director-restart-mid-schedule + pity-survives-reload tests (Phase-10/11 capstone rigor).

## Decisions to settle before building (the open forks)

1. **Scope.** Full (12.1–12.4 + capstone), or core-loot-first (12.1 + 12.2 + 12.4 + capstone, deferring
   the affix/quality layer 12.3)? The roadmap calls quality variance "coarse v1"; the doc marks it optional
   (the old §8 D3). *Recommend:* full, but keep 12.3 deliberately coarse (item level + a few affixes).
2. **Eligibility model (v1).** Who may loot — *dealt damage to the mob* (simplest fair rule), tagged-the-mob
   (first/last hit), or group membership? *Recommend:* "dealt any damage" via the existing threat/damage
   record, with a content tag hook for later refinement.
3. **Delivery model.** Personal loot delivered DIRECTLY to each eligible player (no contested corpse
   pickup, per §5 step 6), vs. dropping into the corpse container. *Recommend:* personal-direct (matches the
   doc + the modern-MMO feel; the corpse holds only the body).
4. **Scheduled-spawn command path.** The director commands the owning zone to spawn via a 10.4 remote-effect
   broadcast (a zone `on_world`/`on_region` handler runs the spawn), vs. a new engine `spawn_in` op/gRPC.
   *Recommend:* the remote-effect broadcast — reuses Phase 10, no new transport.
5. **Persistent vs ephemeral drops.** A rolled legendary is a PERSISTENT item instance; a common drop left
   on the ground ages out (ephemeral). *Recommend:* persist by a rarity/`bind`-driven flag (rare+ persists),
   common stays ephemeral — confirm the threshold.
6. **The `on_roll(ctx)` Lua escape hatch** (conditional drops the declarative form can't express — "only
   while the realm is at war"). In scope for 12.1, or deferred? *Recommend:* defer to a follow-up; ship the
   declarative resolver first (declarative for the 80%, Lua hatch later).

## Builds on / relates to

Phase 6 (death + `OnKill`) · Phase 4 (item instances + the durability ladder) · Phase 10 (director +
durable scope state + remote effects) · Phase 11 (`OnKill` already drives XP; loot hangs off the same
hook). Crafting/economy (binding, salvage, the material economy) is the NEXT phase (**Phase 13**); auth +
the website are **Phase 14**.


---

<a id="phase-13"></a>

# Phase 13

_(archived from docs/PHASE13-PLAN.md)_

# Phase 13 — Crafting & economy

The close of **Track D**: the item-economy layer — rarity binding + professions that **deconstruct** items
into components and **craft/augment** new ones, the modern-MMO material loop. Design:
[CRAFTING.md](CRAFTING.md). It answers "does personal loot (Phase 12) kill the economy?" — *no, because
bound gear re-enters the economy as components.*

Status: **COMPLETE (2026-06-30).** All slices landed + CI-green: 13.1 binding/transfer gate · 13.2 stackable
materials · 13.3 crafting ops + professions · 13.4 deconstruction (salvage/disenchant) · 13.5 crafting &
recipes · capstone (disenchant a bound epic → mats → craft at a station, survives restart). Deferred items
in docs/FOLLOW-UPS.md (generic salvage/craft verbs; profession-cap content-config; augment affix depth).

**Settled forks (confirmed):** (1) FULL scope (13.1–13.5 + capstone), `augment_item` kept to a flat-stat-
bump stub (the rich affix/socket catalog stays §10-deferred). (2) Professions reuse the Phase-11.2 track
system for skill levels + a thin `state.professions` set for membership/the D2 cap. (3) The `bound` state +
stack count ride the item-instance delta (Phase 12.3 round-trip). (4) Stations = a ROOM flag (`forge`) for
v1, with a furniture-upgrade hook. (5) Stacking = coarse v1 (merge identical materials on pickup + `split`);
broader auto-stacking is a follow-up.

## The binding shape (settled — do not re-litigate)

CRAFTING.md §11 settled the three core decisions; they are binding inputs:

- **D1 — Component binding is TIER-DEPENDENT.** Low/mid-tier salvage components are `unbound` (tradeable —
  this feeds the market); top-tier (legendary) essence is `bound` (sinks the top end so legendaries can't
  be bought). The threshold sits on `rarity_tier_defs`.
- **D2 — A CAP on crafting professions** (e.g. 2), gathering/utility unlimited; content-configurable.
- **D3 — Stations are PER-RECIPE.** A recipe may require a `forge`/`tanning_rack`; many craft anywhere.

And the architecture the doc fixes:
- **Crafting actions are ABILITIES** (disenchant/salvage/craft/augment) — the existing lifecycle (requires
  gates + costs + an `on_resolve` op-list), no separate crafting engine. Adds new **item effect ops**.
- **Binding is a TRADE restriction, not a use/destroy one** — a bound item can still be equipped,
  destroyed, and (crucially) **deconstructed by its owner**. One engine gate guards every transfer.
- **Salvage yield = a weighted roll** reusing the Phase-12 loot resolver (a `salvage_table` keyed on the item).
- **Deep affixes/sockets/weapon-altering are DEFERRED** (§10) — Phase 13 scaffolds the `augment_item` hook
  + coarse quality bands; the rich modification catalog is its own later subsystem.

## What already exists (the foundation)

- **The effect-op interpreter** — the new ops (`consume_item`/`produce_item`/`augment_item`) register
  exactly like the Phase-11 grant ops (effect_op.go).
- **The ability lifecycle** — requires (reqAttr, `requires_grant`, cooldown, tags) + costs + on_resolve.
  Phase 13 adds a `requires.profession` + `requires.station` gate (checkRequires).
- **Item-instance deltas** (Phase 12.3) — `ItemJSON.Delta` + the round-trip; the `bound` state + an
  augment delta ride it, exactly like rolled quality.
- **The loot resolver** (Phase 12.1) — `salvage_table` reuses it (the same weighted-roll machinery).
- **`rarity_tier_defs`** (Phase 12.1) — the binding threshold (D1) is a field on a tier.
- **The track system** (Phase 11.2) — a profession's skill is a use-based track (`OnSkillUse`→advance);
  professions reuse it rather than inventing a new progression mechanism.
- **`grant_ability` + ownership** (Phase 11.4) — a profession grants its craft/deconstruct verbs.

On-pillar: every tier, binding rule, profession, recipe, and salvage table is CONTENT; the engine runs the
ops + the transfer gate and names no profession or item.

## Testing mandate (binding — memory `testing-standard`)

Every slice ships tests across all tiers: unit + table-driven for the ops + the binding gate + the salvage
roll (seeded RNG → deterministic), gated PG round-trip for each new def-table + the bound/stack/profession
state, and the capstone milestone. Per-slice reviews: owning engineer = `progression-engineer`;
cross-cutting = `persistence-engineer` (def-tables + item/profession state), `security-auditor` (the
binding gate can't be bypassed; no item dupe via craft/salvage/stack-split), `mudlib-engineer` (the
item-model stacking piece).

## Slices

### 13.1 — Binding + the transfer gate
The hinge the economy turns on. A per-item `bound` state in the instance delta; a bind rule on the item
prototype (`bind_on_pickup` / `bind_on_equip` / `unbound`), applied on loot (BoP) / equip (BoE). One engine
gate guards every TRANSFER (give / trade / drop-to-other / vendor-sell) and refuses a bound item — while
equip, destroy, and owner-deconstruct stay allowed.
- **Done when:** a bind-on-pickup item binds when looted, cannot be given/dropped-for-another, but can
  still be equipped + (later) deconstructed by its owner; the bound state survives a reload.

### 13.2 — Stackable materials
The item-model piece materials need. A `Stack` component (count + max) on a prototype flagged a material;
identical material instances merge on pickup; a `split` command divides a stack. Which items stack is
content (a `material` flag + tier/type on the prototype); stacking/merging/splitting is engine mechanic.
- **Done when:** picking up two stacks of the same material merges them (bounded by max), splitting yields
  two stacks, and stack counts round-trip through persistence.

### 13.3 — Crafting ops + professions
The op vocabulary + the trade-skill model. New ops: `consume_item(item, qty)` (destroy/decrement an input),
`produce_item(proto, qty, {quality, bind, owner})` (create into inventory), `augment_item(item, mod)` (the
deferred-depth hook + a minimal flat-stat-bump v1). Professions: `profession_defs` content (a trade kind +
the abilities/recipe-tags it grants); a profession is LEARNED (grant) and its skill is a use-based track
(Phase 11.2); the ability `requires` gains a `profession`+`skill` gate. `state.professions` (the cap, D2).
- **Done when:** learning a profession grants its verbs + a skill track; a craft-style ability gated on
  `requires.profession` is refused without it and runs the consume/produce ops with it; the profession +
  skill survive a reload.

### 13.4 — Deconstruction (salvage / disenchant)
The economy's source of components. `salvage` / `disenchant` abilities: gated on profession+skill+item-tag,
they roll a `salvage_table` (reusing the Phase-12 loot resolver), consume the item, and produce the
components — **owner can deconstruct a BOUND item** (§1), and **component binding is tier-dependent** (D1:
low/mid components unbound, top-tier essence bound).
- **Done when:** an owner disenchants a bound epic into tradeable mid-tier components (unbound) + a bound
  top-tier essence, the source item is consumed, and the yield is a deterministic weighted roll under a seed.

### 13.5 — Crafting & recipes
Closing the loop. `recipe_defs` (profession+skill, optional station, inputs, output + a coarse quality
band); a `craft` ability validates profession+skill (+ station presence, D3 — a station = a room flag),
consumes the inputs, and produces the output. Output quality scales with skill/crit (coarse band; the rich
roll is the deferred affix system).
- **Done when:** a character with the profession crafts an item at the required station from its component
  inputs (consumed), the output lands in inventory, and crafting without the station/skill is refused.

### 13.x — Capstone (the done-when)
The §9 material-economy loop: **disenchant a bound epic into tradeable mats, then craft a new item at a
station** — proving bound gear re-enters the economy as components, the transfer gate holds (the bound epic
can't be sold but its mid-tier components can be traded), and the whole flow survives a restart. Demo
content (a profession + a salvage table + a recipe + a station room) + the milestone + persistence tests.

## Decisions to settle before building (the open forks — §11 is already settled)

1. **Scope.** Full (13.1–13.5 + the minimal augment + capstone), or defer 13.5 crafting/recipes to keep it
   to the binding+salvage+economy-source half? *Recommend:* full — the capstone (disenchant → craft) needs
   both halves to demonstrate the loop; keep `augment_item` to a flat-stat-bump stub (the rich catalog is
   §10-deferred regardless).
2. **Professions: reuse the Phase-11.2 track system, or a dedicated `state.professions` subtree?**
   *Recommend:* a profession's SKILL is a use-based track (no new progression mechanism); a thin
   `state.professions` set records which professions are learned (for the D2 cap + recipe gating). Hybrid:
   tracks for levels, a small set for membership.
3. **The `bound` state + stack count: ride the item-instance delta (Phase 12.3), or a new per-item table?**
   *Recommend:* the instance delta — `bound` and stack count join rolled quality in `ItemJSON.Delta`, reusing
   the round-trip just built.
4. **Station model: a ROOM flag (a room tagged `forge`) vs a nearby furniture item?** *Recommend:* a room
   flag for v1 (simplest, "places matter"), with the content hook to upgrade to a furniture item later.
5. **Stacking depth: merge-on-pickup + a `split` command (coarse v1), or full auto-stacking everywhere
   (inventory, ground, containers, trade)?** *Recommend:* coarse v1 — merge identical materials on pickup +
   `split`; the broader auto-stack surface is a follow-up.

## Builds on / relates to

Phase 11 (the effect-op grant precedent, the track system, ability ownership) · Phase 12 (item-instance
deltas, the loot resolver reused for salvage, rarity tiers for the binding threshold). The deferred affix/
socket/weapon-altering subsystem (§10) is a FUTURE phase the `augment_item` hook + quality bands scaffold
for. Auth + the website (the real chargen front end the Phase-11 bundles feed) are **Phase 14**; hardening
+ scale are **Phase 15** — Phase 13 closes the engine/content arc before the services track.


---

<a id="phase-14"></a>

# Phase 14

_(archived from docs/PHASE14-PLAN.md)_

# Phase 14 — Auth & website

The close of **Track E's first phase**: replace the credential-less stub login (`gate.go` — "By what name
shall you be known?", `account_id` nullable since Phase 1) with real accounts, OAuth, encrypted transports,
and a content-driven chargen website. Design: [ACCOUNT.md](ACCOUNT.md). Owned by a new `telos-account`
service; consumed by `telos-gate`; the world trusts it via signed assertions, never a hot-path RPC.

Status: **LOCKED (2026-06-30).** Building 14.1 → 14.8 + capstone. All four sub-forks (D-A server-rendered Go
templates + htmx · D-B Ed25519 assertions · D-C GitHub OAuth first · D-D OWASP Argon2id) confirmed. OAuth
provider credentials are configured by the user at 14.7 (a GitHub OAuth app — client id/secret + redirect
URI); 14.1–14.6 build + CI-verify against stubs with no external setup.

---

## Settled forks (from review)

1. **FULL website** — OAuth sign-in + account dashboard + character management + a polished content-driven
   chargen UI (not a minimal bridge). This makes the web layer 2 slices (14.7 core + 14.8 chargen UI).
2. **Transports: TLS + SSH default-ON; plain telnet OPT-IN.** The gate listens on TLS telnet + SSH by
   default; **unencrypted plain telnet is disabled unless explicitly enabled** at gate startup (a
   `--allow-plaintext` flag / `TELOS_GATE_ALLOW_PLAINTEXT=1`). Credentials never cross an unencrypted wire
   by default.
3. **Single-session conflict: TAKEOVER** — a new login drops the old connection ("you have been
   disconnected by a new login"), the standard MUD UX.

## Open sub-forks (recommendations — confirm or override at lock)

- **D-A — Frontend stack.** *Recommend:* `telos-account` serves **server-rendered Go `html/template` +
  htmx** for the interactive chargen — in-repo, no separate build pipeline, server-testable end to end.
  Alternative: a separate SPA (heavier; its own toolchain). 
- **D-B — Assertion signing.** *Recommend:* **Ed25519** (small, fast, modern) for the session assertion;
  account publishes its public key (a `/.well-known`-style endpoint or a Redis-cached key the shard reads),
  rotated periodically with a key-id in the assertion header.
- **D-C — OAuth provider first.** *Recommend:* wire **GitHub first** (the repo lives there; easiest to
  test), with Google/Discord as config-parallel providers added behind the same identity flow.
- **D-D — Argon2id parameters.** *Recommend:* the OWASP baseline (m=19 MiB, t=2, p=1), tunable via config.

---

## Slice breakdown

### 14.1 — `telos-account` service skeleton + schema + gate seam
The spine everything hangs off. New `cmd/telos-account` (gRPC server) + its internal API surface, the schema,
and the gate's account-client seam (an interface + a test stub, so 14.2+ can be tested before the real
service exists).
- Schema (migration): extend `accounts` (the stub from 00002 — fix the stale "Phase 13" comment) +
  `account_identities` (provider, provider_uid UNIQUE); add `account_auth` (Argon2id passphrase) + `ssh_keys`
  (fingerprint PK). (PERSISTENCE.md §2, ACCOUNT.md §11.)
- gRPC API: `Authenticate`, `RedeemLinkCode`, `VerifyPassphrase`, `ResolveSSHKey`, `ListCharacters`,
  `CreateCharacter`, `ReserveName`. The proto + a `buf generate` pass.
- Gate seam: an `AccountClient` interface in the gate; a `stubAccount` for hermetic tests; the real gRPC
  client behind a flag.
- **Done when:** `telos-account` boots, the schema migrates, and the gate can call a stubbed `ListCharacters`
  over the seam (no real auth yet).

### 14.2 — Link codes (the primary telnet bridge)
The credential-less happy path. Account mints a single-use, ~40-bit, ~5-min-TTL code in Redis keyed to
`(account_id[, character_id])`; `connect <code>` at the gate redeems it.
- `MintLinkCode` (account, for the website's "Play" button) + `RedeemLinkCode` (gate → account: atomic
  consume, returns account + character list + the session assertion stub).
- Gate login flow: replace the bare name prompt with a menu — `connect <code>` → character select/enter world.
- **Done when:** a (test-minted) link code redeems once, returns the character list, enters the world; a
  second redeem of the same code fails.

### 14.3 — Signed session assertions (gate↔world trust)
Decouple the world from account at connect time. Account issues a short-lived signed
`{account_id, character_id, session_id, exp}` (D-B: Ed25519); the gate carries it in the `Attach` frame;
shards verify against account's published public key (cached, offline — no per-connect RPC).
- Assertion mint (account) + verify (world); key publication + rotation (key-id + cached public key).
- Wire it into `Attach` (PROTOCOL.md §1) beside the existing handoff token; the world rejects an
  unverifiable/expired assertion. Distinct from the intra-cluster handoff token.
- **Done when:** a forged/expired assertion is refused by the shard; a valid one enters the world with the
  asserted identity, verified with no call to account.

### 14.4 — Single-session lock (takeover)
One live session per character. A Redis per-character lock with a TTL, heartbeated by the session; a crashed
connection's lock self-expires. **Takeover policy:** a second login drops the old connection.
- Acquire-on-enter / release-on-exit / heartbeat; the takeover signal to the old gate session ("disconnected
  by a new login"). Complements the epoch/state_version single-WRITER guard (this prevents two CONNECTIONS).
- **Done when:** two logins for one character → the first is cleanly dropped, the second plays; a crash
  releases the lock within the TTL.

### 14.5 — Passphrase auth + rate limiting
The website-less convenience path (discouraged on plain telnet). Argon2id (D-D) in `account_auth`;
`connect <name> <passphrase>`; per-account + per-IP rate limits with lockout/backoff; a cleartext warning
when sent over an unencrypted transport.
- `SetPassphrase` (web/account) + `VerifyPassphrase` (gate→account) with the rate-limit/lockout state.
- **Done when:** a correct passphrase logs in; repeated failures lock out with backoff; a plain-telnet
  passphrase entry emits the cleartext warning.

### 14.6 — Encrypted transports (TLS + SSH; plain opt-in)
The transport posture (settled fork #2). Gate listens on TLS telnet + an SSH server by default; plain telnet
only when explicitly enabled.
- TLS telnet (TELNETS) listener; cert config. SSH server (pubkey auth → `ResolveSSHKey` → account); unknown
  key falls back to interactive link-code entry (+ optional key registration).
- Plain telnet gated behind `--allow-plaintext` / `TELOS_GATE_ALLOW_PLAINTEXT`; OFF by default. GMCP/MCCP
  negotiation identical across transports.
- **Done when:** a client connects over TLS and over SSH (pubkey); plain telnet is refused unless the flag is
  set; the same GMCP/MCCP handshake runs on all three.

### 14.7 — OAuth + account website core ✅ DONE (2026-06-30)
The browser front door (D-A: server-rendered Go templates + htmx; D-C: GitHub first). OAuth 2.0 / OIDC
Authorization Code + PKCE: provider sign-in → `account_identities` lookup (found→login, not-found→create) →
session cookie. Account dashboard + character management; the **Play** button mints a link code (→ 14.2).
- PKCE + `state` CSRF + strict redirect-URI allowlist; account linking only while authenticated (never
  auto-merge by email). Dashboard: characters, linked providers, passphrase set, SSH keys.
- **Done when:** sign in with GitHub on the website, land on a dashboard, click Play → get a link code that
  redeems at the gate.
- **Landed:** `internal/web` (oauth.go PKCE+exchange+identity, session.go signed cookies, server.go routes,
  templates.go pages); store `FindIdentity`/`CreateAccountWithIdentity`/`AccountDisplayName`; config
  `Web*`/`Github*`/`OAuthRedirectURL` + env; wired into `cmd/telos-account` (serves :8080 when `WEB_LISTEN`
  set); `account` service in docker-compose (`env_file: auth.local.env` required:false; GITHUB creds gitignored).
  Hermetic flow test stubs the provider via httptest; gated PG round-trip for the identity methods.
  **Deferred to the capstone:** pointing the gate at telos-account (`ACCOUNT_TARGET`) so a web-minted code
  redeems end-to-end over telnet — held back here so stub-login smoke/e2e stays green.

### 14.8 — Content-driven chargen front-end ✅ DONE (2026-06-30)
<!-- 14.8a (chargen_defs + world first-spawn apply) + 14.8b (validator + account BuildCharacter + web form) all landed. -->

Chargen is content, not hardcoded (PRINCIPLES.md), and — per the user (2026-06-30) — **not boxed into one
system**: content drives *how* generation works (roll-and-assign, point-buy, standard array, 1-stat-then-
spend-XP, …). The abstraction is a **content `chargen` flow = ordered STEPS**, each a `kind`:
- `bundle_choice` — pick N bundles of a `bundle_kind` (race, class, background) → resolves to the chosen refs.
- `point_buy` — allocate a `points` budget across `attributes` under a per-target `cost` curve + min/max →
  resolves to the chosen attribute values. (Implemented now.)
- (future kinds — `array_assign`, `roll` — are a content write + a small step-kind handler, not a rewrite.)

Storage/flow:
- **`chargen_defs`** — the 8th def-table (ref+pack+JSONB body, the full def-table precedent: DTO + loader +
  store read/write/strip/migration + gated round-trip + world/registry). One flow per pack by convention.
- **Apply on FIRST SPAWN (Model A).** Chargen's OUTPUT (chosen bundle refs + chosen attribute values) is
  recorded into the new character's INITIAL STATE as a pending-chargen marker. `CreateAccountCharacter`
  already takes the `state []byte`. The **world**, on first spawn of a brand-new character, reads the marker,
  SETS the point-buy attribute bases, then runs the existing `apply_bundle` grant path for each chosen bundle
  (single-writer, authoritative), clears the marker, and persists (restart-safe). No grant interpreter in
  telos-account.
- **telos-account** only READS content rows (the pack's `chargen` flow + `bundle_defs`, from Postgres) to
  render the form + VALIDATE the submission server-side (bundle kinds match, point-buy within budget/bounds),
  then writes the marker. Adding a race is a content write — the form updates with no code change.

Sub-slices: **14.8a** content schema (`chargen_defs`) + loader + store + migration + world first-spawn
application + account-side validation/create; **14.8b** the web chargen flow (read flow → render steps →
POST validate + create).
- **Done when:** create a character on the web from the demo class+race + a point-buy allocation; on first
  connect the bundle grants + bought attributes are applied (and survive a restart); a newly-content-added
  race appears in the form with no code change.

### 14.x — Capstone (the done-when) ✅ DONE (2026-06-30)
The full front door, end to end: **create an account on the web (GitHub OAuth), build a character from
content-driven chargen, get a link code, `connect` over TLS (or SSH), enter the world** — the session
assertion verifies offline at the shard, the single-session lock holds (a second login takes over), and the
account+character survive a restart. Tests (hermetic stubs for OAuth/providers) + the milestone.

**Landed (additive, smoke/e2e stay green):**
- **`gate-auth`** compose service — the account-backed front door (`TELOS_ACCOUNT_TARGET`, port 4001) that
  completes the web→telnet loop (sign in → Play → `connect <code>`). The plain `gate` stays name-only so the
  `connect <Name>` smoke/e2e suite is unaffected; flipping the primary gate + migrating those tests is a
  Phase-15 hardening call.
- **Capstone tests:** `TestChargenBuildSurvivesReloadNoReapply` (hermetic, internal/world) — a chargen-built
  character dumped + reloaded keeps its stats EXACTLY, no double-application; `TestChargenAccountJourneyCapstone`
  (gated, real PG) — the account Service validates a submission against the REAL demo flow and the world's load
  path reads back the exact first-spawn marker. The connect→spawn path is the existing `linkcode_journey`
  tests; OAuth sign-in is the `internal/web` flow test. The loop is covered across the suite.
- Deferred to Phase 15: the §F2/F4/F8 web-auth hardening (docs/FOLLOW-UPS.md); pointing the PRIMARY gate at
  account + migrating smoke to an account login; a content-supplied bundle display name (the form labels off
  the ref today).

---

## Notes / dependencies
- Cashes in two long-deferred threads: the **chargen front-end** (deferred from Phase 11) and the **auth
  bridge** (stubbed since Phase 1).
- The `auth-engineer` agent owns this domain; `security-auditor` should review 14.2/14.3/14.5/14.6/14.7
  (link codes, assertions, passphrases, transports, OAuth) — the highest-value attack surface in the project.
- After Phase 14: **Phase 15** (hardening & scale) + the end-of-roadmap **GitHub wiki** push.


---

<a id="phase-15"></a>

# Phase 15

_(archived from docs/PHASE15-PLAN.md)_

# Phase 15 — Terminal-native OAuth (login rework)

Status: **COMPLETE (2026-06-30).** 15.1 → 15.6 all landed CI-green. Reworked Phase 14's front-end: the website-
centric link-code/passphrase/SSH login is replaced by a single **terminal-native OAuth device flow**. The
former "Phase 15 — Hardening & scale" shifted to **Phase 16**.

**Landed:** 15.1 device-auth backend (Redis device sessions + StartDeviceAuth/PollDeviceAuth) · 15.2 the
one-click broker (`internal/web` stripped to `/login/{device_code}` + the OAuth callback) · 15.3 gate OAuth
device login (replaces the code/passphrase prompt; the dead login paths removed) · 15.4 prompt-driven char
select + create (GetChargenFlow/CreateChargenCharacter, char cap; the at-cap fix keeps a full account on the
selection menu) · 15.5a SSH transport removed · 15.5b passphrase + link codes + the dead RPCs/packages removed
(OAuth-only) · 15.6 the `TELOS_DEV_AUTOAUTH` bypass + the primary `:4000` gate flipped account-backed (dev sets
the bypass so smoke/e2e stay headless) + the success page auto-closes. The flow is proven by the gate journey
suite (device login, prompt-chargen create, dev-autoauth, at-capacity) + the gated account→world chargen
journey + the hermetic world build-survives-reload test; the live TLS + browser-OAuth + reconnect capstone is
exercised manually on `:4001` + the `:8080` broker.

## Goal

`connect localhost:4000` → the gate shows a one-click link → the browser does OAuth → the telnet session is
authed → pick or create a character (prompt-driven chargen) → play. **No passwords, ever; auth fully
externalized to the OAuth provider.**

## Decided (user, 2026-06-30)

- **One-click broker** (not the native RFC-8628 device flow): the terminal shows a clickable link to OUR tiny
  auth endpoint, which 302-redirects into the provider's OAuth; the callback marks the waiting session authed.
  No code to type. The web surface shrinks to a bare **auth bridge** — no dashboard, no chargen form, no Play.
- **OAuth-only.** Remove passphrase auth (14.5) AND SSH pubkey auth + the SSH transport (14.6b) entirely. No
  key-based auth. Keep **plain telnet + TLS telnet (`telnets://`)** as the only transports.
- **Prompt-driven onboarding.** Character select + chargen happen as telnet prompts (the content-driven chargen
  ENGINE is reused unchanged — only the renderer moves from web form to prompts).

## Architecture — the brokered device flow

The Redis link-code store (14.2) is repurposed as the **device session** (the inverse of 14.2: the telnet
side now MINTS the pending code, the browser side FULFILLS it).

1. **connect** → gate calls account `StartDeviceAuth(connInfo)` → a random `device_code` (Redis, ~10 min TTL,
   status=pending) + a `verification_uri` (`http://<broker>/login/<device_code>`).
2. Gate prints the link (OSC-8 hyperlink + a plaintext fallback) and **polls** account `PollDeviceAuth(device_code)`
   (a few-second interval, until authed / expired / the player disconnects).
3. Player opens the link → broker `/login/{device_code}`: validate pending → stash the device_code in a signed
   flow cookie → start OAuth (PKCE) → 302 to the provider.
4. Provider → broker `/auth/<provider>/callback`: exchange code → fetch identity → resolve-or-create account →
   **mark the device session authed** (device_code → {authed, account_id}) → render "✓ Logged in — return to
   your terminal."
5. Gate's poll sees `authed` → gets `account_id` (+ characters) → issues the signed session assertion → enters
   **character select**.
6. **Character select / chargen (prompts):** list the account's characters numbered (pick one) or `new` (up to a
   configurable cap); no characters → must create. `new` walks the content chargen flow's steps as prompts
   (reusing `content.ValidateChargen` + the account `BuildCharacter`), then spawns — the world applies the
   build on first spawn (Phase 14.8a, unchanged).

## Reused vs removed

**Reused:** account/character store, `oauth.go` (PKCE/exchange/identity), signed-cookie helpers (for the broker's
OAuth state), signed session assertions, the single-session lock, the **entire chargen engine** (`chargen_defs`,
`ValidateChargen`, `BuildCharacter`, world first-spawn apply), TLS telnet transport.

**Removed:** the website dashboard + chargen form + Play page + the website-minted-link-code direction; passphrase
auth (store methods + gate path + `account_auth` usage); the SSH transport + pubkey auth + `ResolveSSHKey` +
`ssh_keys`; the name-only stub login as a *prod* path (see 15.6 for the dev/test seam).

## Slice breakdown

- **15.1 — Device-auth backend.** Account gRPC `StartDeviceAuth` / `PollDeviceAuth` over a Redis device-session
  store (repurpose `linkcodes`). The proto grows the two RPCs. Hermetic + gated tests.
- **15.2 — The one-click broker.** Strip `internal/web` to `/login/{device_code}` + `/auth/<provider>/callback`
  + a confirmation page; reuse oauth.go + session signing; the callback marks the device session authed. Delete
  the dashboard/form/play handlers + templates. Hermetic flow test (stub provider).
- **15.3 — Gate OAuth login.** Replace the gate's code/passphrase prompt with: `StartDeviceAuth` → print the
  OSC-8 link (+ plaintext) → poll → authed. The primary gate (:4000) is OAuth-only.
- **15.4 — Prompt-driven char select + chargen.** List/pick/new (configurable cap); walk the content chargen
  flow as prompts; reuse `BuildCharacter`. The world applies the build on first spawn (unchanged).
- **15.5 — Transport + dead-code cleanup.** Keep plain + TLS telnet; REMOVE the SSH transport/pubkey path and
  the passphrase path (gate + account + store + the `account_auth`/`ssh_keys` migrations stay as historical
  migrations but the code paths go). Update docs/ACCOUNT.md.
- **15.6 — Test migration + capstone.** A **dev/test auth seam** (`TELOS_DEV_AUTOAUTH=1`, default OFF) lets the
  gate auto-auth `connect <name>` to a seeded account WITHOUT a browser — so smoke/e2e stay green headlessly;
  prod stays OAuth-only. e2e additionally drives the real broker (stub provider). Capstone milestone: connect
  over TLS → click link → OAuth → authed → create a character via prompts → play → reconnect (survives restart).

## Open design points (recommendations — confirm or adjust at approval)

1. **Phase number / roadmap.** Call this **Phase 15**; the old "Hardening & scale" becomes **Phase 16**. (Update
   ROADMAP.md.) — *Recommend yes.*
2. **Test/dev auth seam.** A config-gated `TELOS_DEV_AUTOAUTH` that restores a name-only auto-auth for headless
   smoke/e2e (insecure, dev-only, default OFF). The alternative — driving the full browser OAuth in smoke — isn't
   feasible against real GitHub in CI. — *Recommend the dev seam.*
3. **Character cap.** Default **3** per account, `TELOS_MAX_CHARACTERS` configurable. — *Recommend 3.*
4. **Provider scope.** GitHub first (already configured); the broker stays provider-generic so Google/Discord
   are a config add later. — *Recommend GitHub-first.*
5. **Broker still served by telos-account** (in-process, as today) on `WEB_LISTEN`; it's now an auth bridge, not
   a site. — *Recommend keep in telos-account.*


---

<a id="phase-16"></a>

# Phase 16

_(archived from docs/PHASE16-PLAN.md)_

# Phase 16 — Hardening & scale

Status: **COMPLETE (2026-07-01).** All slices (16.1 metrics · 16.2 bot-swarm · 16.3 backpressure · 16.4 FULL
zero-drop drain) landed CI-green. The user chose the FULL drain (over the narrow clean-save fallback), which
un-deferred runtime zone-add + the rebalance executor: runtime `HostZone`, the fenced `HandoverZone` lease
flip, zone-lease renewal moved into the shard, the `AdoptZone` RPC, `BeginDrain` + the SIGTERM wiring, and the
hermetic zero-drop capstone. Deferred (docs/FOLLOW-UPS.md): bounded drain fan-out concurrency, drain metrics +
clean-disconnect, director-owned/serialized target selection, runtime-zone scope-replica registration.
The last roadmap phase — next is the end-of-roadmap wiki.

Original plan below (kept for the decision record).

**Decisions (approved):** metrics via **OpenTelemetry (OTLP)** — the OTel metric SDK in-process + an OTLP
exporter, an `otel-collector` in the dev stack; traces are available from the same SDK (optional, hot-path
spans later). **Instanced zones are DEFERRED** to a later content phase (kept Phase 16 on scale + ops).
**Handoff-based zero-drop drain.** **Scale bar: ~1–2k concurrent synthetic players on one box**, heartbeat
250 ms, p99 tick-lag under budget (~50 ms) — runnable locally/CI as the gate; the harness scales higher on
real hardware.

## Goal / done-when

- **N-thousand synthetic players sustain the target tick rate** — a bot swarm drives a realistic traffic mix
  through the gate while the heartbeat stays on cadence (p99 tick-lag under budget), visible in metrics.
- **A shard drains + redeploys with zero dropped connections** — on SIGTERM the shard hands its live players
  off to peers, flushes, and exits; players keep playing throughout.

## What already exists (so we build the gaps, not re-do)

- **Metrics: none.** `internal/obs` is slog-only with a "tracing/metrics to attach later" placeholder. The
  metrics layer is new.
- **Soak: state-churn only.** `make soak` (TELOS_SOAK) hammers the save/concurrency path under `-race`; it is
  NOT a synthetic-client load test. The bot swarm is new.
- **The zone never blocks on a slow client.** `session.send` is already non-blocking (`select … default` →
  DROP on a full `out` channel) — the golden rule holds today. What's missing is *measuring* the drops and a
  policy for a persistently-slow client (it currently holds its slot + stream forever).
- **`Shard.Drain()` exists** (bulk-flush every live player, PERSISTENCE.md §6) but its **SIGTERM trigger was
  deferred to "Phase 10 / the placement controller"** — never wired. A true zero-drop drain (hand players off
  to peers before exit) is new.
- The **heartbeat is 250 ms** (`pulseInterval`, Diku-style quarter-second). The cross-shard **handoff** (Phase
  2/10) is the migration primitive a drain reuses. **Dynamic placement** (Phase 10.6) is the substrate for
  instanced zones.

## Slice breakdown

- **16.1 — Metrics foundation (OpenTelemetry).** Wire the OTel meter provider in `obs.Init` (replacing the
  slog-only placeholder) with an OTLP exporter (no-op/stdout default; OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT`
  is set; an in-memory reader for tests). Instrument the load-bearing signals: zone **tick-lag** (heartbeat
  overrun vs the 250 ms budget, a histogram) + **occupancy** (players/entities per zone, a gauge) + the save
  cadence (checkpoint/flush durations, CAS-loss rate) + gate connections + **scoped-event-bus / NATS lag**
  (publish→deliver latency, JetStream backlog) + dropped frames. Add an `otel-collector` to the dev compose.
  Nothing else can be tuned without this.
- **16.2 — Bot-swarm load tester.** A synthetic-client harness (`cmd/telos-botswarm` + a `make loadtest`
  target, gated like soak) that opens N telnet (+ optional GMCP) sessions through the gate and drives a
  realistic mix (login → move/look/say/who/combat → quit), reporting throughput + client-side latency. With
  16.1 it makes the tick rate under load VISIBLE.
- **16.3 — Slow-client backpressure.** Refine the existing drop-on-full into a MEASURED + bounded policy:
  count drops per connection, and DISCONNECT a persistently-slow client (sustained overflow) so it can't hold
  a slot/stream; a bounded gate-side writer mirrors it. A chaos test proves one wedged client never stalls the
  others or the heartbeat.
- **16.4 — Graceful shard drain (FULL zero-drop, approved 2026-06-30).** The user chose the full version over
  the narrower "clean-save + reconnect" fallback, which un-defers two Phase-10.6 pieces (runtime zone-add +
  the rebalance drain executor). Players keep playing the SAME zone across a rolling redeploy. Built as four
  sub-slices, each verify+review+green:
  - **16.4a — Runtime zone-add (world).** `Shard.HostZone(id)`: the standby already has every zone's room
    prototypes (`defineContent` fills the cache from ALL loaded zones, not just the won set), so hosting a new
    zone at runtime is: build the zone from the retained `LoadedContent`, `adopt` it, launch `z.Run` on the
    shard's run ctx, and claim its directory lease + start renewal. Makes `s.zones` runtime-mutable — guard it
    with the existing `s.mu` behind `zoneByID`/`zonesList` accessors (per-attach/move reads, not per-tick, so a
    mutex is fine). The standby model already exists (a shard that wins no zones from its pool runs as a
    standby, registered + heartbeating); this gives it the "live re-claim" its own boot comment promised.
  - **16.4b — Drain choreography (world + director).** `Shard.BeginDrain(ctx)`: (1) set a `draining` flag that
    REJECTS new fresh-login attaches (a re-dial/handoff bind is still accepted so an in-flight move completes);
    (2) resolve a target shard for each hosted zone — the director assigns the drained zones to a standby via
    `HostZone` + a lease handover (release-then-claim so `ShardForZone` flips to the standby); (3) fan
    `beginHandoff` over every live player to the new owner (reusing the exact cross-shard handoff — the gate
    holds the socket open across the Redirect); (4) wait until every zone is empty or a deadline, then `Drain`
    (flush) + return. Correctness: single-owner lease handover, the both-serve window, epoch monotonicity, and
    in-flight handoffs started before the drain flag — reviewed by the distributed-systems architect before it
    lands.
  - **16.4c — SIGTERM wiring (cmd/telos-world).** Replace the post-cancel `GracefulStop` (best-effort flush
    only) with: on signal → `BeginDrain(ctx-with-timeout)` while the zone+saver goroutines stay ALIVE → wait
    for drain-complete or timeout → then cancel + `GracefulStop` + exit. The drain runs BEFORE the zone loops
    stop, which is the whole point (§6 said the flush must precede ctx cancel).
  - **16.4d — Capstone test.** A hermetic 2-process (+standby) topology: players on shard S under load,
    SIGTERM S, players migrate to the standby now hosting the same zone and keep issuing commands with zero
    dropped connections; S exits clean. Honest scope (per the 16.3 review): "zero-drop" covers HEALTHY
    connections — a client wedged mid-drain is deadline-reclaimed and counted separately.
- **Capstone.** The bot swarm sustains the target tick rate at the agreed scale (16.1 confirms p99 tick-lag),
  AND a rolling redeploy drains a shard with zero dropped connections (the load test keeps running across it).

## Deferred (not Phase 16)

- **Instanced zones** — party dungeon copies on the Phase-10.6 dynamic-placement substrate (director mints/
  reaps instances + routes a party to one). A world/content feature, folded into a later content phase.
  Recorded in docs/FOLLOW-UPS.md.


---

<a id="resolved-follow-ups"></a>

# Resolved follow-ups

These were tracked in the (now-removed) `FOLLOW-UPS.md` and are DONE. Kept for the record; the open backlog is
in [REMAINING.md](REMAINING.md).

**Reviews & lint**
- Phase 7.7 hot-reload formal review trio (security / distributed-systems / persistence) — discharged CLEAR/SOUND.
- The whole `TODO(owner)` nolint backlog — burned down (mechanical conversions bounded; deliberate suppressions reclassified).

**Engine / combat / scripting**
- `rx:replace_target` redirect wired (re-mitigated against the new target through the shared `dealDamage` pipeline, loop-bounded).
- `pendingFinalFlush` stash active eviction (one-shot `createFailedMsg` on permanent create failure).
- pgx-gated + cross-session create-window logout-race coverage.
- Room-affect tick cadence (per-occupant re-lease only at the affect's `tickInterval`).
- Lua relocation combat-fidelity (per-method OA semantics, liveness re-check).
- A content op grants a flag — `set_flag`/`clear_flag` effect-ops (Phase 11.1).

**Protocol / handoff / edge**
- Retired the redundant `Redirect.resume_input_seq` wire field (gate replays from the destination's ack).
- `gate.go` `resumeSeq` investigation — resume is wired via the Attached ack; dead param dropped.
- Single-session clean-kick contract (a second login cleanly kicks the old connection + resets the dedup fence).

**Security hardening (this burn-down, Cluster 1)**
- Mail per-recipient inbox cap + `ListMail` LIMIT (atomic conditional insert, `ErrMailboxFull`).
- Cross-shard handoff destination pack-set validation (reject-before-drop, no silent item loss).
- Cross-shard handoff payload caps (`maxCarryStateBytes` 256 KiB + `maxCarryItemNodes` 512, anti-spawn-bomb).
- Corpse loot-ownership window (`CorpseOwner`, killer-owned 60s, anti-ninja-loot).

**Scale / orchestration hardening (this burn-down, Clusters 2–3)**
- `who` read-cache (~1s snapshot, collapses a `who` flood to one Redis SCAN/window).
- `notifyZones` non-blocking hot-reload fan-out (`postOrDrop`, no head-of-line stall).
- Bounded drain fan-out concurrency (per-shard `handoffSem` of 32).
- Weighted placement planner (`placement.PlanWeighted`, load-aware with an indivisible-heavy-zone guard).
- Runtime-hosted-zone scope-replica registration (`scopeReplication.registerZone`).
- Rebalance drain executor + runtime zone-add — delivered as Phase 16.4 (`HostZone`, fenced `HandoverZone`,
  `BeginDrain`, the zero-drop drain).

**Tests**
- Durable-tell redelivery (Consume-driven) end-to-end test.
- `combat_test` empty-`if` — asserts the move outcome + exclusion message.
<a id="roadmap-overview"></a>

# Roadmap overview (all phases delivered)

_(archived from docs/ROADMAP.md — the whole roadmap, Phases 0–16, shipped and CI-green.)_

# Roadmap

Strategy: stand up the real distributed topology around a trivial world early, prove
scale-out, then deepen the engine *inside* the working system. Each phase ends with a
demoable, testable milestone. Phases are grouped into tracks; within the engine track,
content-driven systems land only after the content pipeline (Phase 4) exists.

⭐ = highest-risk integration work — get these right early; everything rides on them.

---

## Track A — Spine (prove the topology)

### Phase 0 — Foundations
Monorepo + `go.work`; protobuf toolchain (`buf`); CI (build/test/lint/vet); `docker-compose`
with Postgres + Redis + NATS; config (env+yaml), `slog` logging, OpenTelemetry wiring.
**Done when:** `make up` brings up deps; `make test` is green on an empty engine.

### Phase 1 — Vertical slice skeleton ⭐
The whole pipe end-to-end with the minimum world. ([PROTOCOL.md](PROTOCOL.md))
- `telos-gate`: telnet accept, basic option negotiation, line I/O.
- `telos-world`: one shard, one hardcoded zone, two rooms; `look`, `say`, `move`.
- `transport`: gate ↔ world gRPC bidi `Play` stream; route `ClientFrame`/`ServerFrame`.
- `directory`: real interface, single-shard impl.

**Done when:** telnet in → see a room → `north` → see the next room → `say hi` echoes.

### Phase 2 — Multi-shard + handoff ⭐
The scale-out proof. ([PROTOCOL.md](PROTOCOL.md) §2–3)
- Two world shards; `zone→shard` directory in Redis.
- Two-phase cross-shard handoff (Prepare/Commit + epoch; gate re-dial; input replay).

**Done when:** a player walks from a zone on shard A into a zone on shard B with no visible
seam, and no input is lost across the handoff.

## Track B — Engine

### Phase 3 — Mudlib core
Turn the toy world into a real object model. ([MUDLIB.md](MUDLIB.md))
- Entity/component model (ECS-lite, flyweight + COW), uniform containment.
- Command parser with abbreviation + Diku targeting (`2.sword`, `all.coin`).
- `act()` perspective messaging, heartbeat scheduler, containers/inventory.

**Done when:** you can `get`, `wield`, `put`, and `wear` items, and others see the right
act() messages.

### Phase 4 — Persistence & content pipeline
The "everything is content" backbone — needed before any content-driven system.
([PERSISTENCE.md](PERSISTENCE.md), [PRINCIPLES.md](PRINCIPLES.md))
- Per-type definition tables + `state` JSONB; migrations; `pack` namespacing.
- Content loader (boot-load definitions into shards) and `(kind,ref)` hot-reload over NATS.
- Save strategy: durability ladder (memory → Redis checkpoint → Postgres), `state_version`.
- Zone resets/repop.

**Done when:** the bare engine boots empty, loads a content pack, and a character + world
state survive a restart.

### Phase 5 — Attributes, resources, affects & ability framework
The generic substrate + the effect-op vocabulary. ([ABILITIES.md](ABILITIES.md))
- Content-defined attributes/resources/damage-types/flags; modifier stack + derivation.
- `Affected` runtime (durations, stacking, ticks); tag-based CC.
- Ability lifecycle (declarative `on_resolve`), effect ops, automatic PvP/hostility gate.

**Done when:** a data-defined `fireball` casts, costs mana, deals typed damage, and applies a
content-defined affect — all without engine code changes.

### Phase 6 — Combat (+ the check primitive, the event bus, AoE & room affects) ✅
**Status: complete (slices 6.1–6.5).** Round-based resolution on top of the substrate — and the phase
that builds the load-bearing primitives combat is assembled *from*. ([COMBAT.md](COMBAT.md),
[PHASE6-PLAN.md](COMPLETED.md#phase-6), [GAME-SYSTEMS-GAP-ANALYSIS.md](#gap-analysis))

Foundational primitives (built *with/before* the fight loop — to-hit and saves *are* these):
- **The check/save/contested primitive** [G2] — a `check` flow op beside `if`/`chance`: a
  content-named dice expression (keep-high/low for advantage, `dF`, pools) classified into an
  *ordered list of outcome bands* (binary 5e = the 2-band case; PbtA 3-tier, BRP degrees all fit).
  Roll visibility is config (hidden default, opt-in `show`). Invokable from exits/objects/affect-
  ticks too — a climb check needn't wait for combat.
- **The event-bus origin** [G3] — engine events (`OnHit`/`OnDamageTaken`/`OnKill`/
  `OnAbilityResolved`/`OnCheck`/`OnLeaveRoom`) fire to content op-lists. The universal glue:
  rage/combo builders, XP-on-kill, procs, and the reaction checkpoints all hang off it (declarative
  handlers here; Lua handlers in Phase 7). The first realization of the
  [universal-hookability pillar](PRINCIPLES.md#pillar-every-action-and-event-is-hookable) — the
  taxonomy ships *partial* (several kinds reserved-but-unlit, no builder-defined events yet); Phase 7
  completes it.
- **AoE / area targeting** [G12] — loop the *built* `dealDamage` harm-gate per target (the room, or
  the room + adjacent rooms) with a per-target save.
- **Room-scoped affects** [G13] — affects attached to the room entity, ticking over its occupants
  (wall/web/darkness/silence-field/lair actions).

Combat resolution on those primitives:
- `PULSE_VIOLENCE`, attacks/round, the avoidance ladder + soak pipeline; to-hit is a check vs AC.
- **Cooldown completion + persistence** [G8] — per-ability cooldown map, a step-3 "still cooling
  down" gate, serialized into `state` (logout doesn't refresh cooldowns); the GCD is a shared-tag
  lag affect.
- **Named interruptible checkpoints** [G9] in the swing/cast pipeline that fire events — easy
  reactions (opportunity attack on `OnLeaveRoom`) are declarative; result-altering ones
  (Counterspell/Shield) are Lua (Phase 7).
- Conditional/formula resource regen [G4] + a `rest`/`recover` event [G5]; `floor`/`ceil`/`round`/
  `mod` formula heads [G1]; gear modifiers into the attr mod-stack [G14].
- Skills-as-commands with lag/cooldowns; threat/assist; **uniform, cancellable death** (6.5) — the
  depletion→death seam lives in the shared `dealDamage` funnel so *any* damage (swing/spell/AoE/DoT/OA)
  kills, and an `on_depleted` hook can cancel death (a death-ward: hp→1 + rooted) — the reference
  before-checkpoint for the hookability pillar.

**Done when:** you fight a mob through the full pipeline (to-hit check → miss/dodge/parry/block →
soak), a fireball's save halves its damage across everyone in the room, a rage bar builds on hit via
an `OnHit` handler, you kill the mob and loot its corpse — all from content, no engine changes.

### Phase 7 — Lua scripting ✅
The curated escape hatch + sandbox — and the home of the complex ~20% the declarative op-list can't
express. ([LUA.md](LUA.md))
- `gopher-lua`, one VM per zone, curated handle API, strict budget + circuit breaker.
- Triggers, ability `on_resolve` in Lua, affect hooks, hot reload; `self.state` persistence.
- Lua **event handlers** on the Phase 6 bus; **result-altering reactions** [G9] (Counterspell/Shield
  reaching into an in-flight ability), **concentration** [G11], and the 5e **multiclass spell-slot
  table** [G7] — the documented escape-hatch cases.
- **Builder-defined events + taxonomy completion** — the
  [universal-hookability pillar](PRINCIPLES.md#pillar-every-action-and-event-is-hookable) made
  concrete on the bus:
  - A **content-namespaced custom-event lane**: builders *fire* and *subscribe to* their own named
    events (a sailing system's `OnShipDock`), not just the engine's enumerated kinds — today's closed
    `eventKinds` validation map grows a `pack:event` lane (still depth/width-budgeted and gate-funneled
    like an engine event; no privileged status).
  - **Light the reserved engine kinds** whose owners exist by now — `OnApplyAffect`/`OnAffectTick`/
    `OnAffectExpire` and an `OnEnter` movement hook — so "a missing hook is an engine bug" holds. The
    cross-phase kinds get lit by their owning phase (`OnRest` with regen [G5], `OnLevelUp` with
    progression Phase 11, `OnLogin` with auth Phase 14).

**Done when:** a room script fires on entry and a scripted mob greets you — edited live — and a pack
defines, fires, and handles an event the engine has never heard of.

Phase 6 (the event bus) + Phase 7 (Lua) are the prerequisites for the progression phase (Phase 11).

## Track C — World & clients

### Phase 8 — Comms over NATS ✅
- Channels (`gossip`, `newbie`), tells, `who`, presence — all cross-shard via NATS.
- JetStream for offline tells/mail.

**Done when:** two players on different shards chat and see each other in `who`.

### Phase 9 — GMCP ✅
Rich-client data. ([GMCP.md](GMCP.md)) (`Room.Info` can be pulled forward to Phase 3.)
- Negotiation (option 201), `Core.Supports` filtering, MCCP2, NAWS.
- `Char.Vitals/Stats/Status`, `Room.Info`, `Char.Items.*`, `Comm.Channel.Text`, `Mud.*`.

**Done when:** Mudlet shows a live vitals gauge and a minimap that updates as you walk.

### Phase 10 — Orchestration (directors, scopes, event bus) ✅
Supra-zone state and cross-zone consequences. ([WORLD-EVENTS.md](WORLD-EVENTS.md))
> Done. Dynamic-placement core landed (claim-from-pool + the coordinator's decision engine); the live
> rebalance-drain executor + runtime zone-add are documented follow-ups (REMAINING.md).
- `telos-director` tier with leader election; region/world state (single-writer).
- **Cross-zone** scoped event bus: `transient` (NATS core) + `durable` (JetStream, idempotent,
  ordered) — extends the Phase 6 *in-zone, synchronous* bus to region/world scopes across shards.
- Remote effect commands into zones; the Lua `world.*` / `region:*` / `signal_*` API.
- **Dynamic zone placement** (the director hosts the zone coordinator): world servers
  *claim* zones from a pool instead of declaring them, with balancing, standbys, and
  failover/rebalance. Builds on Phase 4 (crash-failover rehydrates from the durability
  ladder). Design: [PLACEMENT.md](PLACEMENT.md). Replaces static `TELOS_ZONES`.

**Done when:** a boss death in one zone ripples a region-wide change across zones on different
shards, and survives a director restart.

## Track D — Progression & economy

### Phase 11 — Character progression & chargen ✅
> Done (11.1–11.5 + capstone). The grant/track/bundle machinery + all four advancement modes landed; the
> interactive **chargen front end is deferred to Phase 14** (account/login), which the bundles feed.
The largest content area the gap analysis surfaced ([G6]); needs the event bus (Phase 6) + Lua
(Phase 7), which is why it lands here — though it depends on *nothing* in Track C and may be pulled
forward. ([GAME-SYSTEMS-GAP-ANALYSIS.md](#gap-analysis) §5)
- **N independent advancement tracks** (`track_defs`) — character level, guild/class levels, use-
  based skills — each with content-defined XP sources, thresholds, and per-level grants. `level` is
  an ordinary attribute, not an engine concept.
- **Grant ops** — `modify_attribute_base`, `grant_ability`/`grant_track`/`grant_resource`/flags.
- **Content bundles** — `class_def`/`race_def`/`background_def`/`feat_def`/`talent_def`: pure content
  that grants resources/attrs/abilities/flags and defines tracks (multiclass, "join a guild at 5").
- **All four advancement modes** as "which event feeds the track": XP-threshold auto-level (Diku),
  train-at-a-trainer, point-buy per level, and use-based (LP/Discworld — `OnSkillUse` → chance-to-
  improve). Plus chargen (the creation-time grant flow; the account/login flow is Phase 14).

**Done when:** a character is created from a class+race bundle, gains XP on kills (auto-leveling one
track), trains a skill through use on another, and the build survives a restart — all content.

### Phase 12 — Loot & scheduled spawns ✅
> Done (12.1–12.4 + capstone). Loot resolver + pity + per-instance quality + director-owned weekly
> spawns. Deferred: a worn affix's stat effect (the gear-modifier seam), an `on_roll` Lua hatch, a
> normalized `affix_defs` table, per-mob xp-value cap (see REMAINING.md).
([LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md))
- Loot resolver on death: roll kinds, rarity tiers, personal loot, pity counters.
- Item quality/affix rolls into instance deltas (coarse v1; deep affixes deferred).
- Director-owned durable scheduled spawns (weekly boss; wall-clock, restart-safe).

**Done when:** a weekly boss spawns on schedule and drops personal loot with a working pity
timer.

### Phase 13 — Crafting & economy ✅
> Done (13.1–13.5 + capstone). Binding/transfer gate, stackable materials, professions (= a bundle + a
> track + a membership set, no new def table), salvage/disenchant (tier-bound components, owner may
> deconstruct bound gear), `recipe_defs` (station = a room flag) + the §9 material loop. Deferred: generic
> `disenchant <item>`/`craft <recipe>` verbs, profession-cap content-config, augment affix depth
> (REMAINING.md).
([CRAFTING.md](CRAFTING.md))
- Rarity/binding (BoP rules, tier-dependent component binding) + the transfer/bind gate.
- Professions, recipes, stackable items, deconstruction (salvage yields = weighted rolls).
- `consume_item`/`produce_item`/`augment_item` ops; crafting stations.

**Done when:** you disenchant a bound epic into tradeable mats and craft a new item at a
station.

## Track E — Services & scale

### Phase 14 — Auth & website
([ACCOUNT.md](ACCOUNT.md)) (replaces the stub login used since Phase 1.)
- `telos-account`: OAuth (Google/Discord/GitHub), accounts, and the chargen *flow* (the progression-
  track grants/bundles it drives are Phase 11).
- Link-code + passphrase + SSH pubkey; TLS/SSH transports; signed session assertions;
  single-session lock.

**Done when:** create an account on the web, get a link code, `connect` over TLS/SSH.

### Phase 15 — Terminal-native OAuth (login rework) ✅
([PHASE15-PLAN.md](COMPLETED.md#phase-15)) Reworks Phase 14's front-end: the website + passphrase + SSH logins
are replaced by a single **terminal-native OAuth device flow** — no passwords, auth externalized.
- `connect` → a one-click link → the browser does OAuth (brokered, PKCE) → the telnet session is authed.
- Prompt-driven character select + chargen (the content-driven chargen *engine* from 14.8 is reused).
- OAuth-only: passphrase + SSH pubkey auth removed; plain telnet + TLS `telnets://` are the only transports.

**Done when:** `connect` over TLS → click the link → OAuth in the browser → create a character via prompts →
play → reconnect (survives restart). No password/key path exists in prod.

### Phase 16 — Hardening & scale ✅ COMPLETE
- ✅ OTel(OTLP) metrics (tick-lag/occupancy/gate-conns/frames-dropped) + `otel-collector`.
- ✅ Bot-swarm load tester (`cmd/telos-botswarm`, `make loadtest`).
- ✅ Slow-client backpressure (world measures drops; the gate's per-write deadline reclaims a wedged client).
- ✅ FULL handoff-based **zero-drop graceful drain** for rolling redeploys: runtime zone-add (`HostZone`),
  fenced atomic lease flip (`HandoverZone`), lease renewal moved into the shard, `AdoptZone` RPC, and
  `BeginDrain` wired on SIGTERM — a draining shard hands every zone + its live players to a peer with the
  socket held open. (Instanced zones DEFERRED to a later content phase.)

**Done when:** ✅ a shard drains + redeploys with zero dropped connections (hermetic capstone
`TestGracefulDrainZeroDrop`). The N-thousand-players-sustain-tick-rate check is an operational load-test run
(`make up` + `make loadtest BOTS=1500`, watch `:8889` tick-lag) — tooling ready.

**This is the last roadmap phase.** Next: the end-of-roadmap GitHub wiki (mudlib-dev / sysadmin / builder).

---

## Starting point
Build **Phase 0**, then the **Phase 1–2 spine** before any engine depth — they carry the
riskiest integration (the gate↔world stream and the cross-shard handoff), and every later
phase assumes them. After that the engine track (3–7) is mostly linear; tracks C–E layer on
top once the content pipeline (Phase 4) is in place.


---

<a id="test-coverage-program"></a>

# Test-coverage program (archived)

_(archived from docs/TEST-COVERAGE.md — the coverage gap-map + wave program; the waves shipped.
Any still-open test items are in [REMAINING.md](REMAINING.md) §7.)_

# Test coverage gap-map + the maximal-coverage program

This is the honest, unflinching map of TelosMUD's test coverage as of **HEAD (Phases 1–8
complete)** and the **prioritized wave plan** for the sustained "maximal coverage" push the
owner has asked for ("as much test coverage as imaginable… confidence that everything works";
slow tests are explicitly fine).

It supersedes the earlier four-wave **black-box** gap-map (that push is DONE — see the
"Black-box push (Waves 1–4, DONE)" section at the bottom). This refresh widens the lens from
"player-visible journeys" to **every system × every test TYPE**: deep functional/unit edges,
gated-infra integration, e2e journeys, chaos/failure-injection, property/fuzz, and stress/soak.

> **The class of bug we exist to kill** stays the same: stateful, real-dependency, multi-run,
> multi-service, failure-path bugs that hermetic single-shot unit tests structurally cannot see.
> Two have already shipped: the seed/`deletePack` idempotency bug (only failed on the *second*
> run against *real* Postgres) and the **cross-shard gear-carry** drop (`buildSnapshot` carried
> an empty state subtree, so a player walking midgaard→darkwood lost worn/carried gear, stats,
> affects, and cooldowns — fixed in `b8d764f`, pinned at the *world seam* by
> `handoff_carry_test.go` but **never as a player-visible e2e walk-with-gear journey**).

---

## Deliverable 1 — where we actually are (measured at HEAD)

### Hermetic `go test ./... -cover` (the per-commit surface)

| Package | Coverage | Read |
| --- | --- | --- |
| `internal/textsan` | **94.7%** | strong; the obvious fuzz target |
| `internal/presence` | **89.5%** | `mem` 96.7 / `redis` 89.0 (mem path; real-Redis edges gated) |
| `internal/gate` | **82.8%** | the in-process harness carries this; high for an edge |
| `internal/world` | **80.8%** | the engine; **but** the average hides thin files (below) |
| `internal/telnet` | **78.1%** | IAC/negotiation; decent |
| `internal/config` | **69.0%** | |
| `internal/directory` | **63.6%** | `redis.go` 76.5, `directory.go` **0.0** (interface/ctor only exercised via redis) |
| `internal/commbus` | **59.0%** | mem paths strong (`memjs` 91.5, `membus` 89.6); **NATS paths 12–38%** (gated) |
| `internal/contentbus` | **43.3%** | `membus` 97.4; `nats.go` **15.0**, `contentbus.go`/`publish.go` **0.0** (gated) |
| `internal/content` | **40.3%** | `loader.go` 82.8; `demo.go` 45.8; `definition.go` **0.0** |
| `internal/store` | **0.0%** | **entirely DSN-gated** — `t.Skip` without `TELOS_TEST_DSN`; the real number only shows in the integration tier |
| `internal/checkpoint` | **0.0%** | no hermetic test; exercised only via the gated store/integration tier |
| `internal/obs` | **0.0%** | observability shims, untested |
| `cmd/telos-*`, `db`, `api/gen` | **0.0%** | mains/generated; smoke + integration cover the behavior |

**The reported numbers UNDERCOUNT.** Three tiers are invisible to a bare `-cover` run and must
be accounted for separately:
- **Gated Postgres integration** (`TELOS_TEST_DSN` set): `internal/store` + `tests/integration`
  jump from 0% to real coverage; this is the only tier that exercises `checkpoint`, the CAS, the
  durability flush, and `import.go` against real Postgres. CI runs it in the `integration` job.
- **e2e** (`-tags e2e`, live gate): one journey today (`combat_death_test.go`). Not in `-cover`.
- **smoke** (`tests/smoke/smoke.sh`, `--twice`): the Docker/seed/compose surface. No Go coverage.
- **`-race -count=100`** (`make test-race`): a *different* signal than coverage — it's the only
  thing that exercises the concurrent zone-goroutine/saver/session interleavings.

### Test-type census (the structural gap)

- **641** `func Test*` · **11** `func Benchmark*` · **0** `func Fuzz*`.
- **No property-based or fuzz tests exist at all.** The four highest-value fuzz targets
  (`textsan`, the parser, the durability StateJSON round-trip, the handoff snapshot round-trip)
  have zero adversarial-input coverage.
- **No chaos harness** beyond the two in-process gate chaos tests (`shard_drop_chaos_test.go`,
  `shard_restart_test.go`). NATS-down / Redis-down / Postgres-down degradation is untested.
- **Benchmarks** exist but there is **no stress/soak tier** (many-player/many-zone, long pulse
  loops, comms fan-out under load).

### Thin files inside the well-covered packages (the deep-unit backlog)

`internal/world` averages 80.8% but the *seams and failure paths* are the thin part:

| File | ~Cov | What's unverified |
| --- | --- | --- |
| `handoff_server.go` | **42%** | the failure legs: Prepare timeout, Attach with a bad/stale token, redirect-target-unreachable, abort-and-thaw |
| `zone.go` | **43%** | freeze/thaw reaper edges, panic-recovery branches, shutdown drain |
| `components.go` | **43%** | component (de)serialization branches |
| `scripted.go` | **50%** | scripted-mob/room trigger error paths |
| `living.go` | **56%** | position transitions, resource clamps at boundaries |
| `tell.go` / `commscmds.go` | **60%** | offline tells, ignore-filter, toggle interactions |
| `luaharm.go` / `reload.go` | **67–68%** | sandbox-escape denials, reload error paths |
| `effect_op_handlers.go` | **71%** | per-op edges across the 13-op registry (below) |
| `formula.go` | **73%** | formula eval error/NaN/div-by-zero branches |
| `combat_commands.go` | **73%** | combat command error paths |

The **13 effect ops** (`deal_damage, heal, restore, modify_resource, apply_affect,
remove_affect, dispel, act, send, if, chance, check` + the `self/other/actor/victim` scope
resolvers) each have a happy-path test; the **per-op boundary/error matrix** (negative amounts,
missing target, resource over/underflow, affect-stacking-vs-refresh, nested `if/chance/check`)
is thin.

---

## Deliverable 2 — the maximal-coverage gap-map, by SYSTEM × TIER

Status legend: **DONE** = a test pins it at the right tier · **THIN** = partial/indirect ·
**GAP** = unverified. Tier abbreviations: U=unit, H=in-process harness, I=gated integration,
E=e2e, C=chaos, F=fuzz/property, S=stress/soak.

### S1 — Combat (attack/avoidance/soak/damage/death)
| Behavior | Tier | Status |
| --- | --- | --- |
| Attack roll → avoidance → soak → typed damage | U | **DONE** (`combat_test.go`, `formula_damage_test.go`) |
| Death → corpse → loot → repop (COW cycle) | U+E | **DONE** (`cow_repop_cycle_test.go`, `combat_death_test.go`) |
| Avoidance/soak pipeline EDGES (0/negative/overflow, immunity, resist/vuln) | U | **THIN** |
| Multi-attacker / threat / target-switch | U | **GAP** |
| Death-as-cancellable-checkpoint (`on_depleted` cancels: death-ward) | U | **THIN** |
| Full fight pipeline through the gate (init→swing→death→loot) | E | **THIN** |
| Combat under `-race` with concurrent attackers | S | **GAP** |

### S2 — Abilities / affects / effect-ops
| Behavior | Tier | Status |
| --- | --- | --- |
| Ability cast: cost/cooldown/target/resource gate | U | **DONE** (`ability_test.go`) |
| Each of the 13 effect-ops, happy path | U | **DONE** |
| Effect-op boundary/error matrix (per op) | U | **THIN** — the deep-unit backlog |
| Affect stacking vs refresh vs replace; expiry tick; remaining-conserved | U | **THIN** (`affect_test.go` partial) |
| Data-defined `fireball`: cast→mana→typed-dmg→affect, no engine change | H/E | **GAP** (unit-only) |
| AoE save-halves; room affects | U | **DONE** (`aoe_test.go`, `affect_room_test.go`) |

### S3 — Event bus + reactions + check primitive
| Behavior | Tier | Status |
| --- | --- | --- |
| In-zone event publish/subscribe; reserved events fire | U | **DONE** (`event_test.go`) |
| Check primitive: band classification, margin, DC, advantage | U | **DONE** (`check_test.go`) |
| Every check BAND edge (crit/fumble/degrees/PbtA-tiers/derived edges) | U | **THIN** |
| Reaction fires → handler → effect, through a running zone | H | **THIN** |
| Custom (engine-unknown) event round-trip | U | **DONE** (`luahook_test.go`) |

### S4 — Lua sandbox / scripting / hot-reload / self.state
| Behavior | Tier | Status |
| --- | --- | --- |
| Sandbox escape denied (os/io/ffi/loadstring) | U | **DONE-ish** (`luaharm*_test.go`) — harden as handle API grows |
| Budget/circuit-breaker: runaway script killed, zone survives | H | **DONE** (`lua_sandbox_journey_test.go`) |
| Whole-zone panic recovery | H | **DONE** |
| self.state survives logout/relogin ladder | H | **DONE** (`lua_state_journey_test.go`) |
| Hot-reload live script, players intact, state preserved | U/H | **DONE** (`luareload_test.go`) |
| **Sandbox holds under ARBITRARY content (no escape/no crash, fuzzed)** | F | **GAP** |

### S5 — Persistence / durability ladder / CAS
| Behavior | Tier | Status |
| --- | --- | --- |
| Save/load round-trip (in-memory store) | U | **DONE** (`store_test.go` mem) |
| Durability ladder: checkpoint(Redis)→flush(PG)→final, reason selection | U | **THIN** (`saver` logic unit-only) |
| `state_version` CAS contention, exactly one wins | I | **DONE** (`store_test.go::TestSaveCharacterConcurrentCAS`) |
| Zombie-fence / input-seq fence | H | **DONE** (`reconnect_roundtrip_test.go`) |
| **Durability ladder END-TO-END against real Redis+Postgres** | I | **THIN** — CAS is pinned; the full ckpt→flush→reload ladder isn't |
| **CAS/zombie-fence under real contention (N savers, real PG)** | I | **THIN** |
| **Arbitrary StateJSON save→load identity** | F | **GAP** |
| Checkpoint (`internal/checkpoint`) against real Redis | I | **GAP** (0% hermetic) |

### S6 — Cross-shard handoff (a real bug just shipped here: gear-carry)
| Behavior | Tier | Status |
| --- | --- | --- |
| Cross-shard walk, no seam, no lost input | H+E | **DONE** |
| Full-state CARRY at the world seam (gear/stats/affects/cooldowns) | U | **DONE** (`handoff_carry_test.go`, the regression for `b8d764f`) |
| **Cross-shard walk WITH GEAR, player-visible (the gear-carry e2e)** | H/E | **GAP** — the regression has no journey-level pin |
| Handoff interrupted, destination unreachable → thaw+restore | H | **DONE** (`handoff_state_test.go`) |
| Redirect-target-unreachable crash-failover window | C | **GAP** (documented open contract) |
| Prepare timeout / abort-and-thaw | C | **THIN** |
| Exactly-once dedup on replay (mid-transfer) | U | **DONE** (unit), **GAP** at H |
| **Handoff snapshot arbitrary-state round-trip parity** | F | **GAP** |

### S7 — Comms (channels/tells/who/presence/mail/toggles/ignore/hear-filter)
| Behavior | Tier | Status |
| --- | --- | --- |
| Channel send/recv; tells; who; presence (in-memory bus) | U+H | **DONE** (`channel_test.go`, `tell_test.go`, `comms_*_journey_test.go`) |
| Toggles / ignore / hear-filter | U+H | **DONE-ish** (`comms_toggle_journey_test.go`, `commsstate_test.go`) |
| Mail (in-memory store) | U | **DONE** (`mail_test.go`) |
| **Mail/tells/presence against REAL NATS + REAL Redis/PG** | I | **GAP** — only `mem`/`memjs` paths run hermetically |
| Offline tell → mail fallback; cross-shard tell routing | H/I | **THIN** |
| **NATS down → comms degrade gracefully** | C | **GAP** |
| **JetStream redelivery storm / dedup** | C | **GAP** — `jetstream_nats.go` 12.5% covered |
| Comms fan-out under load (N players, N channels) | S | **GAP** |

### S8 — Parser / commands
| Behavior | Tier | Status |
| --- | --- | --- |
| Verb dispatch, abbreviation, args | U | **DONE** (`parser_test.go`, `commands_test.go`) |
| Every command's error/edge path (no-target, bad-arg, posn-gate) | U | **THIN** |
| **Arbitrary bytes → no panic (parser fuzz)** | F | **GAP** |

### S9 — textsan
| Behavior | Tier | Status |
| --- | --- | --- |
| CleanLine/CleanName/CleanMarkup happy paths | U | **DONE** (94.7%) |
| **Arbitrary input → control-free + idempotent (fuzz)** | F | **GAP** |

### S10 — Content loader / hot-reload
| Behavior | Tier | Status |
| --- | --- | --- |
| Pack load/validate (in-memory) | U | **DONE** (`loader_test.go`) |
| `definition.go` validation branches | U | **GAP** (0% covered) |
| Seed idempotency (import twice, real PG) | I+smoke | **DONE** (`store_pack_test.go`, `smoke-twice`) |
| Content hot-reload over REAL NATS (`(kind,ref)`) | I/C | **GAP** |
| **Content-bus invalidation flood** | C | **GAP** |

### S11 — Gate / edge / telnet
| Behavior | Tier | Status |
| --- | --- | --- |
| Connect/look/move/say journeys | H+E | **DONE** |
| Telnet IAC negotiation | U | **DONE** (`telnet_test.go`, 78%) |
| **Slow/blocked socket → non-blocking drop-on-full holds, zone pulses** | C | **GAP** (the named backpressure gap) |
| Shard drop while connected; blast radius = 1 connection | H | **DONE** (`shard_drop_chaos_test.go`) |

### S12 — Auth-adjacent / session / reconnect
| Behavior | Tier | Status |
| --- | --- | --- |
| Single-session takeover (newest wins) | H | **DONE** (`session_test.go`, harness) |
| Reconnect to saved room; input-seq fence reset | H | **DONE** (`reconnect_roundtrip_test.go`) |
| Bad-login re-prompt / name validation | H+U | **DONE** (`onboarding_journey_test.go`, `name_test.go`) |
| Directory double-registration race (real Redis) | I | **GAP** (debugged by hand, never pinned) |
| Directory lease loss/expiry | I | **GAP** |

---

## Program status (executed)

All ten waves' high-value coverage landed. Highlights and the THREE real bugs the program caught:

- **W5 ✅** gear-carry cross-shard regression (controlled-break-verified) + nested/inventory carry; the
  effect-op core + boundary/error matrix (the uncovered restore/send/act ops; missing-arg guards; the
  negative-heal anti-weaponization clamp; modify_resource floor-at-0).
- **W6 ✅** 6 fuzz targets (textsan, parseTargetSpec, dispatch, luaCompile, StateJSON round-trip,
  formulaEval) + a scheduled **nightly** active-fuzz tier (`make fuzz`). 🐛 **FuzzTextsan found a real
  bug** in 15s: CleanLine returned ~3× MaxLineBytes on invalid-UTF-8+control input (cap applied before
  the U+FFFD-expanding strip) — fixed (cap-after-strip), security-auditor-reviewed.
- **W7 ✅** the gated real-NATS suite now RUNS in CI (a new `comms` job, NATS with `-js`). 🐛 Activating
  it caught `TestJetStreamRealOfflineThenOnline` **latently broken against real NATS** (an invalid dotted
  consumer name + a constant idempotency key the mem stand-in never validated) — fixed + rerun-robust.
- **W8 ✅** comms/tell/presence failure-injection (flakyBus/failingRoster): zone-survives-bus-failure +
  recovery, durable-tell NAK-before-cursor-advance ordering AND end-to-end recover-within-maxDeliver,
  who-degrades-to-local on a roster read error. (Shard-drop, crash-restart-restore, handoff-interrupted,
  CAS/zombie-writer were already covered.)
- **W9 ✅** the stress/soak tier (`make soak`, nightly): churn + concurrent-load under `-race`, asserting
  no wedge/panic/leak (residents + goroutines) over 100k cycles.
- **W10 ✅** the formula-eval fuzz (the thin formula.go failure surface) + the capstone kill→loot→equip
  milestone journey. The other thin-file failure paths (effect-ops, StateJSON, the comms/handoff failure
  legs) are now covered by the W5/W6/W8 work above.

🐛 **Also W6-adjacent:** the richer demo content surfaced a real persistence bug — the store dropped a
prototype's Lua through the Postgres `protoBody` JSONB (the same class as the earlier dropped `Living`).

Remaining work is incremental and tracked in docs/REMAINING.md (the comms-chaos deepenings —
MemJetStream park-at-maxDeliver vs NATS, the subscribe-side partition double, the AFK best-effort path —
plus the per-file coverage-% long tail). The 3-tier CI is in place: per-commit hermetic + gated
(Postgres `integration`, NATS `comms`), and the `nightly` workflow (active fuzz + deep soak).

## The wave plan (the program to execute — prioritized highest-confidence-per-effort first)

Each wave is a coherent committable unit. **gated** = needs `make deps`/`make up` infra and runs
only in CI's gated jobs (or `make test-integration`/`test-e2e`); **slow** = belongs in a nightly
tier, not per-commit. Reviewers named per the subagent-review rule (owning engineer + cross-cutting
expert). Default `go test ./...` MUST stay hermetic and fast — every gated/fuzz/stress test is
behind a build tag, an env guard, or a separate make target.

### Wave 5 — the gear-carry e2e + deep-unit effect-op/affect/check matrices (FAST, hermetic)
Highest confidence per effort: closes the just-shipped regression at journey level and fills the
thinnest hermetic gaps. No infra.
- **Gear-carry journey** (H, then E): a player wears+carries gear, walks cross-shard, and SEES the
  gear intact on the far side (the `b8d764f` regression as a player-visible walk). *This is the
  single highest-value missing test.*
- Effect-op boundary/error matrix: each of the 13 ops × {negative/zero amount, missing target,
  resource over/underflow, nested if/chance/check}.
- Affect stacking/refresh/replace/expiry matrix; remaining-conserved on refresh.
- Check-band edge matrix: crit/fumble/degrees/PbtA-tiers/derived-edge bands.
- Avoidance/soak pipeline edges (immunity/resist/vuln, 0/negative/overflow).
- Reviewers: combat/abilities engineer + test-engineer.
- CI: per-commit (hermetic).

### Wave 6 — property/fuzz seed corpora (FAST hermetic seeds; long fuzz in nightly)
The structural gap: 0 fuzz tests today. Each `Fuzz*` runs its seed corpus hermetically in
`go test` (fast); the long `-fuzz` runs go nightly.
- `FuzzTextsan` — arbitrary input → control-char-free + idempotent (`Clean(Clean(x))==Clean(x)`).
- `FuzzParser` — arbitrary bytes → no panic, bounded output.
- `FuzzStateJSONRoundTrip` — arbitrary StateJSON → save→load identity (durability round-trip).
- `FuzzHandoffSnapshot` — arbitrary carry state → buildSnapshot→prepare parity.
- `FuzzLuaSandbox` — arbitrary content script → no escape, no Go panic, budget-bounded.
- Infra needed: a `tests/fuzz` corpus dir + a `make fuzz` target + a nightly CI job with a
  time budget per target.
- Reviewers: per-target owner (textsan→edge, parser/handoff/lua→world, StateJSON→persistence) +
  test-engineer.
- CI: seed corpus per-commit; `-fuzz` nightly (slow).

### Wave 7 — gated-infra integration: durability ladder + comms on real backing services (gated)
The tier that let the seed bug ship. Behind `TELOS_TEST_DSN` / real-NATS-URL guards; CI gated job.
- Durability ladder end-to-end against real Redis+Postgres: checkpoint→flush→final, reason
  selection, reload-from-each-tier, the finalize-flush zombie-fence probe.
- CAS/zombie-fence under real contention (N concurrent savers, real PG) — deepen the existing CAS
  test into the full fence matrix.
- `internal/checkpoint` against real Redis (0% today).
- Comms against real NATS + real Redis/PG: mail durability, offline-tell→mail, presence, who.
- Content loader + idempotent re-import (deepen beyond the seed-twice case: partial/overlapping
  packs, `deletePack` table coverage for every def table).
- Content hot-reload over real NATS (`(kind,ref)` invalidation journey).
- Infra needed: real-NATS env guard + helper (mirror `OpenTestPool`); CI gated job already has PG,
  add NATS+Redis services.
- Reviewers: persistence engineer + comms/edge engineer + test-engineer.
- CI: gated `integration` job (extend with NATS+Redis). Slow-ish, not per-commit hermetic.

### Wave 8 — chaos / failure-injection (the owner's headline want)
Inject the failure, confirm it reproduces the bad behavior, THEN assert graceful degradation + no
data loss. Mix of in-process (H/C, hermetic) and gated-infra (real services killed mid-test).
- Shard DROP mid-session (deepen: assert the locked-in contract, not just today's behavior).
- Handoff FAILS mid-flight: destination unreachable (DONE), **Prepare timeout / abort-and-thaw**,
  **redirect-target-unreachable crash-failover window** (the open-contract gap).
- NATS down → comms degrade (send fails soft, no zone stall, recovery on reconnect).
- Redis down → presence/checkpoint degrade (the durable PG tier still saves; player not lost).
- Postgres down → mail/save degrade (checkpoint still mirrors; reconnect recovers on PG return).
- Slow/blocked socket → `session.send` drop-on-full holds, zone goroutine never stalls (named gap).
- JetStream redelivery storm → dedup holds (`jetstream_nats.go` is 12.5% covered).
- Content-bus invalidation flood → reloader debounces / doesn't wedge.
- Both world servers race to register (real-Redis directory race — the docker double-reg).
- Directory lease loss/expiry → another shard claims, no split-brain.
- Infra needed: a reusable **chaos harness** (kill/restart a backing service mid-test; a
  pauseable/blockable socket; a fault-injecting bus wrapper). Some legs hermetic (in-process,
  fault-injected fakes), some gated (real service killed).
- Reviewers: distributed-systems-architect + edge engineer + test-engineer.
- CI: hermetic legs per-commit; real-service-kill legs in the gated/nightly tier. Slow.

### Wave 9 — stress / soak (slow, nightly)
Confidence under load + the concurrency signal coverage can't give.
- `make test-race` already runs `-count=100`; add targeted high-`-count` race suites for the
  zone-goroutine/saver/session interleavings and concurrent combat.
- Many-player / many-zone harness: N players across M shards, sustained movement + handoff churn.
- Long-running pulse/affect/combat loops (advance the zone goroutine for K pulses; assert no leak,
  no drift, affects expire correctly over time).
- Comms fan-out under load (N players × N channels; assert no drop beyond the documented
  drop-on-full, no goroutine leak).
- Infra needed: a spin-up-N-shards/N-players helper (extend the gate harness); benchmark→soak
  bridges; a leak detector (goroutine count delta).
- Reviewers: distributed-systems-architect + test-engineer.
- CI: nightly only. Slow by design (owner is fine with slow).

### Wave 10 — remaining functional/milestone journeys + thin-file sweep
Cleanup wave: the deferred milestone journeys + the remaining low-coverage files.
- Black-box `fireball` cast journey (P5); AoE-save + OnHit-rage-bar legs through the gate (P6).
- Check/event-handler black-box journey (P6).
- Scripted-room-on-entry full journey (P7, beyond the unit trigger).
- Thin-file sweep: `handoff_server.go`, `zone.go`, `components.go`, `scripted.go`, `formula.go`
  error/edge branches to lift them off the floor.
- Reviewers: owning engineer per journey + test-engineer.
- CI: per-commit (hermetic) where possible; the few that need the stack go to the e2e job.

### CI structure for the program
- **Per-commit hermetic** (`go` job, today's `go test ./... -race`): all unit/H tests, all fuzz
  SEED corpora, the hermetic chaos legs. Stays fast; never needs infra.
- **Gated per-commit** (`integration` + `smoke` + `e2e` jobs, extended): the Wave-7 real-infra
  integration + the real-service-kill chaos legs. Extend the `integration` job with NATS+Redis
  services (it has PG today).
- **Nightly slow tier** (NEW scheduled workflow): `-fuzz` runs with a time budget, the Wave-9
  stress/soak suites, high-`-count` race soaks, the long chaos scenarios. Gated behind a `cron`
  schedule + `workflow_dispatch`; failures page but don't block commits.

### New test infrastructure / fixtures the program needs
1. **Chaos harness** (`tests/helpers` or `internal/.../chaos_test` support): kill/restart a backing
   service mid-test; a fault-injecting `commbus.Bus`/`contentbus` wrapper; a pauseable/blockable
   socket (the harness already has `pauseReader`/`resumeReader` — extend to a writer-block).
2. **Fuzz corpus** (`tests/fuzz` seed dirs) + `make fuzz` + the nightly job.
3. **Real-NATS test guard + helper** mirroring `OpenTestPool` (skip-without-URL, with cleanup).
4. **N-shard / N-player spin-up helper** (factor out of the gate harness for stress/soak).
5. **Goroutine-leak detector helper** for the soak tier.
6. **Richer content fixtures** — a richer demo pack is being authored in parallel; the
   gear-carry journey, the `fireball` journey, and the scripted-room journey all need it. Coordinate
   so the journeys land against the new pack rather than ad-hoc test content.

---

## Black-box push (Waves 1–4, DONE — historical record)

The earlier four-wave push closed every **P0** black-box GAP and the named P1
onboarding/sandbox/distributed journeys via the in-process gate harness + running-zone journeys.
Summary (full per-row detail is in the git history of this file at the four-wave commits):
- **Wave 1 — regression-proofing:** COW kill→repop→re-kill; look renders all room contents;
  persistence round-trip; single-session takeover.
- **Wave 2 — distributed correctness:** cross-shard input continuity; handoff interrupted
  (destination unreachable); true shard-restart persistence; `state_version` CAS contention.
- **Wave 3 — Phase 7 sandbox:** runaway script doesn't wedge the running zone; whole-zone Go-panic
  recovery; self.state through the real persistence ladder.
- **Wave 4 — onboarding journeys:** first-time onboarding; get/wield/wear `act()`; bad-login
  re-prompt; scripted-mob greet milestone.

### Open contract questions still surfaced (decide with the owning engineer, then lock in a test)
- **Shard drop** — today the socket simply closes (no notice, no auto-reconnect). notice-then-close,
  or directory-retry-and-failover? (edge / distributed-systems-architect)
- **Single-session takeover** — the displaced first connection is left silently mute, not cleanly
  dropped with a message. (edge / persistence)
- **Redirect-target-unreachable** — Prepare SUCCEEDED but the gate can't re-dial for the Redirect:
  the gate writes "The world is unreachable. Goodbye." and drops, while the directory already moved
  ownership to the unreachable destination. The crash-failover window of PLACEMENT.md §6 — NOT yet
  covered; needs the gate to retry the directory / re-resolve a healthy shard.
  (distributed-systems-architect) — scheduled in Wave 8.

---

<a id="gap-analysis"></a>

# Game-systems gap analysis (archived design input)

_(archived from docs/GAME-SYSTEMS-GAP-ANALYSIS.md — a design ANALYSIS, not a current-state description:
the reasoning for the engine's game-system generality + the gap forks that drove Phases 6/7/11/12/chargen
(all shipped). Kept for the design record; the current system is described in the live design docs
(ABILITIES / COMBAT / PERSISTENCE / etc.). Its one forward-looking goal — the 5e-SRD sample MUD — is in
[REMAINING.md](REMAINING.md).)_

# Game-systems content gap analysis

**Owner:** `rpg-systems-designer`. **Status:** design input for Phases 6 / 7 / a chargen-progression
phase / 11 / 12. **Companion docs:** [PRINCIPLES.md](PRINCIPLES.md), [ABILITIES.md](ABILITIES.md),
[COMBAT.md](COMBAT.md), [PERSISTENCE.md](PERSISTENCE.md), [OPEN-GAME-SYSTEMS.md](OPEN-GAME-SYSTEMS.md),
[the roadmap in COMPLETED.md](COMPLETED.md#roadmap-overview).

This document does **not** modify code or schema. It answers one question exhaustively: *can the
TelosMUD engine express our target game systems as pure content (def-table rows + JSONB + Lua), with
zero engine changes for flavor — and where it can't, exactly what new mechanism is needed and which
roadmap phase should own it?*

---

## 0. Thesis, targets, methodology

### 0.1 The pillar restated
The engine is **mechanism**; content is **flavor** ([PRINCIPLES.md](PRINCIPLES.md)). The engine knows
*kinds* ("an attribute exists", "a resource exists", "an affect exists", "an effect op exists"); every
*instance* (`strength`, `rage`, `stunned`, `fireball`) is a content row. A proposed engine change that
can only be expressed by naming a class / spell / resource / condition in Go is a design smell. The
test the whole analysis applies: **design to the union abstraction; test against the spread.** A
mechanism that fits only 5e is wrong.

### 0.2 The targets
- **Three capstones** (chosen because they diverge sharply — they triangulate the abstraction):
  - **D&D 5e SRD 5.2** (CC-BY): Vancian spell *slots*, six ability scores → modifiers + proficiency
    bonus, advantage/disadvantage, class+subclass+background+feat at fixed levels, short/long rest.
  - **Pathfinder SRD (1e)** (OGL): the d20 3.x lineage 5e simplified — *much* more granular: BAB,
    separate Fort/Ref/Will saves, iterative attacks, skill *ranks*, feat *chains*, prepared *and*
    spontaneous casting, prestige classes, CMB/CMD for combat maneuvers, size modifiers.
  - **Text World of Warcraft** (the WoW d20 RPG, OGL, used as the rules skeleton, plus the live-MMO
    feel as the experience target): class **resource diversity** (rage that *builds* from combat,
    energy that *regens fast*, focus, mana, runic power, combo points, soul shards), **talent trees**,
    **cooldowns** as the primary pacing mechanism (not slots), **threat/aggro**, **dual-spec**, and a
    raid/loot economy (the [LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md) / [CRAFTING.md](CRAFTING.md) target).
- **MUD heritage as a first-class baseline** (NOT an afterthought). The full spectrum the engine spans:
  - **TinyMUD / MUSH** — contentless social/building; little-to-no stats; bare engine + in-game OLC.
  - **DikuMUD / Merc / ROM** — fixed HP/SP/move pools, level growth, recovery **ticks** (the pulse
    scheduler *is* the tick), an optional class pool. The *simplest* subset of the generic model.
  - **LPMud / Discworld mudlib** — **skill-based**, advance-through-**use** (no levels;
    `OnSkillUse → chance-to-improve`), heavy soft-coded objects → Lua.
  - **Rich tabletop** — the three capstones.
  An explicit acceptance question, asked in §16: *can it still be a plain Diku / LP / Tiny MUD?*
- **Catalog breadth** ([OPEN-GAME-SYSTEMS.md](OPEN-GAME-SYSTEMS.md), ~36 systems) used to pressure-test
  generality — d100/BRP (Delta Green, OpenQuest, Legend), dice-pool/narrative (FATE, PbtA/Dungeon
  World, Blades, Year Zero, Cypher, Gumshoe), rules-light (Cairn, Tunnel Goons, Lumen), supers
  (FASERIP/4C). These surface gaps the three capstones hide (a dice *pool*, a *clock*, a *stress* track,
  a *push/devil's-bargain*).

### 0.3 The two translation theses (applied to every concept — this is the analysis, not a footnote)

**Thesis 1 — DM-adjudication → deterministic builder-content + engine roll.** Tabletop assumes a human
referee making non-deterministic calls. We have none. Every check / save / attack / contested roll
becomes one shape: a **check-gated branch**. A *builder* authors the parameters (the climbable wall =
"DEX check vs DC 15", or "unclimbable: too slick"); the *engine* resolves it deterministically (seeded
per-zone RNG: `roll + modifier vs DC`); the outcome runs a branch (`on success X / on fail Y | half |
none`). Saving throws, attack rolls, ability checks, contested rolls, opposed skill rolls — all one
shape. The check's RNG is engine; the DC / stat / consequence is content. **Roll visibility** ("you
rolled 14+6 vs 15 — success" vs "you scramble up the wall") is a **system-level default, overridable
per action** — never baked into the engine.

**Thesis 2 — the room-graph reframes space.** Tabletop assumes continuous space (feet, grids, templates,
line-of-sight, facing). Ours is a room/exit graph. Every spatial mechanic must be reframed and called
out: teleport → move-to-a-room-ref (named dest / marked anchor / random adjacent); AoE → "the room" or
"room + adjacent rooms" (loop the harm-gate per target); range → exit distance / line-of-sight along
exits; movement-in-combat → between rooms or an abstract intra-room position. Where a spell genuinely
can't fit (precise teleport coordinates, true flight, grappling positioning), say so and propose the
content-expressible substitute.

### 0.4 The binding constraints (non-negotiable design inputs)
1. **Generic resources** — content pools + content-defined dynamics (passive regen, event-driven gain,
   decay, cost). No hardcoded hp/mana; `vital` is a flag.
2. **N independent advancement tracks** — char level / guild level / class level, content grants, with
   *all* modes expressible: XP-threshold auto, train-at-trainer, point-buy, and **use-based**.
3. **Events as universal glue** — `OnHit / OnKill / OnLevel / OnSkillUse / OnCheck → content ops`.
4. **The full spectrum stays first-class** — contentless → fixed-pool-leveled-ticks → skill-use → rich
   tabletop.
5. **Presentation is driver-emits-data** — GMCP + optional server-side UTF-8/color map; rich client out
   of scope; honor the room graph.

### 0.5 What is actually built (Phases 0–5), grounded in the code
This analysis grounds every "maps onto our model" verdict in the read source, not assumption:
- **Attributes** (`internal/world/attributes.go`, `defs.go`): content `attribute_defs` with a
  modifier stack `base → +flat → ×mul → clamp`, derived attributes are formulas over other attributes
  (recursive, cycle-linted), per-entity base overrides (`setAttrBase`), cache+dirty. No `level`/`str`
  is hardcoded — all are rows.
- **Resources** (`resources.go`): named pools, `max` is a derived attribute, per-entity `current`,
  `vital` flag, flat `regen` per tick driven by the pulse. `vitalResource()` finds the first `vital`
  pool — combat subtracts from *that*, never a hardcoded "hp".
- **Damage types** (`defs.go`): named, with a resist/vuln/immune multiplier matrix.
- **Affects** (`affected.go`, `affect_runtime.go`, `defs.go`): content `affect_defs` with modifiers,
  `prevents` tags (the tag-CC model), four stacking modes, durations in pulses, per-entity tick,
  `on_tick` op-list (DoTs), reserved `on_apply`/`on_expire` (Phase 7 Lua), derived-harm gate
  (`affectIsDetrimental`).
- **Abilities** (`ability.go`, `defs.go`): the fixed 10-step lifecycle; `ability_defs` with targeting
  (self/none/enemy/ally), disposition, requires (attr thresholds + `not_prevented` tag-CC), resource
  costs, cast-time/lag/cooldown timers on the pulse, an `on_resolve` op-list, reserved `on_resolve_lua`.
- **Effect ops** (`effect_op.go`, `effect_op_handlers.go`): `deal_damage` (flat or `NdS` dice, scaled),
  `heal`, `restore`, `modify_resource`, `apply_affect`, `remove_affect`, `dispel`, `act`, `send`, `if`
  (only "has affect?" today), `chance`. A **seeded RNG** (`effectCtx.rng`, `rollDice`, `rollChance`) is
  already in the resolution context — the deterministic-roll substrate exists.
- **The PvP / hostility gate** (`pvp.go`, `guardHarmful`): every harmful op funnels through *one*
  chokepoint before touching a player's state; harm is **derived**, not trusted from a label.
- **Persistence** (`PERSISTENCE.md`, `character.go`): one def-table per kind + JSONB tail; character
  `state` JSONB holds `templates / attributes / resources / skills / affects / flags / inventory /
  equipment`. New flavor = a row, never a migration.
- **Pulse scheduler** (`pulse.go`): `every(n)` / `after(n)` timers with the resolve-by-id / skip-frozen
  contract. The recovery **tick** and combat **round** both ride this.
- **Not yet built (the gap surface):** combat round/swing pipeline (Phase 6), Lua (Phase 7), the event
  bus that fires `OnHit/OnKill/OnLevel` to content (Phase 6/7), progression / chargen, GMCP (Phase 9),
  loot (Phase 11), crafting/economy (Phase 12).

### 0.6 How to read a section
Each concept area gives: **(a)** the cross-system comparison (where 5e / PF / WoW / MUD-heritage
*diverge* — divergence is where the abstraction earns its keep); **(b)** the deterministic/content
translation (+ roll-visibility); **(c)** the room-graph translation where spatial; **(d)** *maps onto
our model as* — concrete def-table / component / op / affect / event / Lua hook / track; **(e)** a
**verdict**: `expressible-today` / `needs-new-mechanism` / `needs-Lua`, and for a new mechanism, *what*
and *which phase owns it*. New mechanisms are tagged **[G#]** and collected in §17.

---

## 1. Attributes, ability scores & modifiers

**(a) Cross-system.** 5e: six scores (STR/DEX/CON/INT/WIS/CHA, 1–20+), the modifier `(score−10)/2`
floored, plus a single **proficiency bonus** scaling with character level. PF: same six, but modifiers
feed a denser web (BAB, three saves, skill ranks, CMB/CMD) and there is no single proficiency bonus —
each thing scales separately. WoW d20: the same six scores; the live-MMO layer adds derived combat
ratings (attack power, spell power, crit %, haste, armor) that are *themselves* attributes derived from
gear. Diku: STR/INT/WIS/DEX/CON (+ apply tables). LP/Discworld: often *no* fixed attributes — bonuses
come from skills. TinyMUD: none.

**(b) Deterministic/content translation.** Pure derivation, no DM call. The 5e modifier is a derived
attribute `str_mod = floor((strength − 10) / 2)`; proficiency bonus is a derived attribute keyed off a
`level` attribute. PF's BAB / saves / CMB / CMD are each derived attributes (`bab`, `fort_save`,
`will_save`, `cmd = 10 + bab + str_mod + dex_mod + size_mod`). WoW's attack power = a formula over
gear-granted attributes. The *contentless* case is the default: an entity with no attribute defs has
none, and `attr()` returns 0 sanely (the bare-engine invariant).

**(d) Maps onto our model as.** `attribute_defs` rows + the formula stack (`attributes.go`). Score = a
literal-base attribute with a per-entity base override (chargen/point-buy writes it via `setAttrBase`);
modifier / proficiency / BAB / save / AP = `value_kind:"derived"` attributes whose base is a formula
over other attributes. The formula AST today supports `+ - * / min max clamp attr lit`.

**(e) Verdict: expressible-today**, with one formula-vocabulary gap. The 5e modifier needs `floor`
(integer division) and PF/WoW use conditional and step formulas (`floor`, `ceil`, a "every Nth level"
step). **[G1] Formula vocabulary extension** — add `floor`/`ceil`/`round`/`mod`/conditional (`if`/
`select`) heads to the formula AST. Small, additive, **Phase 6** (combat needs derived to-hit/AC
formulas anyway) or pulled into a chargen phase. Until then, content can approximate with the existing
`/` and `clamp`, but integer flooring is not exact — so this is a genuine (small) gap.

---

## 2. Checks, saves & contested rolls — the deterministic-roll primitive

This is the single biggest new mechanism the analysis surfaces. It is the engine home of **Thesis 1**.

**(a) Cross-system.** 5e: `d20 + ability mod + proficiency (if proficient) vs DC`; *advantage/
disadvantage* = roll 2d20 keep high/low; saving throws are checks vs a spell-save DC; attack rolls are
checks vs AC; ability checks vs a DM-set DC; contested = both roll, higher wins. PF: `d20 + modifier vs
DC`, no advantage, but many stacking typed bonuses; opposed rolls; combat maneuvers = `d20 + CMB vs
CMD`. WoW d20 RPG: same d20 core; the *MMO* layer replaces "does it hit?" with hit/crit % chances and
"does the CC land?" with a resist roll. d100/BRP (Delta Green, Legend): `roll d100 ≤ skill%`; degrees of
success (crit/special/fumble). PbtA (Dungeon World): `2d6 + stat`, 10+ full / 7–9 partial / 6− miss — a
*three-outcome* check. Blades: a *dice pool* (Nd6, highest die: 6 full / 4-5 partial / 1-3 bad, 6-6
crit). FATE: `4dF (−,0,+) + skill vs difficulty`, shifts matter. Cairn/Tunnel Goons: roll-under /
2d6+stat. Diku skills: a stored proficiency % rolled against. LP/Discworld: a skill *bonus* vs a task
difficulty, and crucially **the use itself is the advancement trigger** (§5).

The divergence is total in *dice shape* (d20 / d100 / 2d6 / NdF / dice-pool) and in *outcome arity*
(binary / half-on-success / three-tier / degrees-of-success / shifts). The **shape that is invariant**:
*resolve a randomized magnitude against a threshold (or another roll), classify into one of N
outcome bands, run the band's branch.*

**(b) Deterministic/content translation.** The builder authors a **check spec** as content; the engine
resolves it deterministically with the seeded per-zone RNG (already present in `effectCtx.rng`). A check
spec carries:
- `dice` — the randomized term, content-defined: `1d20`, `2d20kh1` (advantage), `1d100`, `2d6`, `4dF`,
  `3d6` (a pool count). The engine knows how to roll dice (`rollDice` exists); the *notation* is content.
- `bonus` — a formula over the actor's attributes (`+str_mod + prof`).
- `vs` — either a literal/formula **DC**, or `contested` (roll the defender's own check spec; compare).
- `bands` — an ordered list of `(threshold|margin → outcome label)`: binary `{success, failure}`;
  half-on-save `{success→half, failure→full}`; PbtA `{≥10→strong, 7..9→weak, ≤6→miss}`; BRP degrees
  `{crit, special, success, failure, fumble}`. The engine classifies the rolled total into the matching
  band and runs that band's op-list branch.
- `visibility` — `show` / `hide` / `summary` (the roll-visibility config; system default + per-check
  override).

This makes the climbable-wall concrete: a room exit / object carries `check = {dice:"1d20",
bonus:"dex_mod + athletics", vs:15, bands:{success→[move ...], failure→[deal_damage fall ...]}}`; the
builder authored the wall, the engine rolls. A saving throw is the *same* spec invoked from a spell's
op-list (`save = {dice:"1d20", bonus:"$target.dex_save", vs:"$caster.spell_dc", bands:{success→half,
failure→full}}`). An attack roll is the same spec vs the defender's AC attribute (§9). A 5e *condition*
that ends "on a successful save at end of turn" is the same spec fired on the affect tick.

**(c) Room-graph.** N/A for the roll itself; the *consequence* branch may be spatial (the failed climb
moves you down a room — §7's teleport-to-room-ref).

**(d) Maps onto our model as.** A new **`check` effect op** (the flow op that resolves a check spec and
runs the matching band's nested op-list — structurally identical to the existing `if`/`chance` flow ops,
which already recurse into `runOps`). Plus a **`check_def` / inline check spec** carried in JSONB on
abilities, exits, objects, and affect ticks. The dice roller and seeded RNG already exist; what's
missing is (i) dice *notation* parsing beyond `NdS` (keep-highest, dF, pools), (ii) the band classifier,
(iii) the `bonus`/`vs` formula evaluation against *both* actor and target, (iv) the visibility config and
its GMCP/text emission. The `OnCheck` event (constraint 3) fires here so content can react (a bardic-
inspiration reroll, a lucky halfling).

**(e) Verdict: needs-new-mechanism. [G2] The check/save/contested primitive** — the central gap.
Sub-parts: **[G2a]** extend dice notation (keep-high/low for advantage, `dF`, pool-count-successes);
**[G2b]** the `check` flow op + outcome-band classifier; **[G2c]** check-spec formula context with
`$actor`/`$target`/`$source` scoping; **[G2d]** roll-visibility config + emission; **[G2e]** the
`OnCheck` event. **Phase owner: Phase 6 (combat).** Rationale: attack rolls and saving throws are
checks, and the combat pipeline (§9) is *built from* this primitive — so the check primitive should land
*with* or *just before* combat, not after. It is the load-bearing abstraction for everything from a
climb to a fireball save to a Blades dice-pool action; getting its generality right (outcome **bands**,
not a hardcoded binary) is the most important single design decision in this document.

> **Design note — keep the engine ignorant of dice *shape*.** The engine must roll a *content-named*
> dice expression and classify into *content-named* bands. If the engine ever hardcodes "d20" or
> "success/failure", PbtA's three-tier and BRP's degrees and Blades' pools become inexpressible. The
> band list is the union abstraction; binary 5e is just the 2-band case.

---

## 3. Resources & their dynamics (HP / mana / SP / rage / energy / focus / ki / slots / per-rest)

This is binding-constraint #1, and the WoW capstone is what stresses it hardest.

**(a) Cross-system.** 5e: HP (vital, restored on rest), spell **slots** (per-level discrete pools,
refilled on long rest; some on short rest), a few per-rest dice (hit dice, channel divinity, ki,
sorcery points, superiority dice, wild shape). PF: HP, slots (prepared *or* spontaneous), per-day class
pools (ki, bardic performance rounds, channel energy), grit/panache, mythic power. WoW: the resource
*zoo* — **mana** (regens, large pool), **rage** (starts at 0, *builds* from dealing/taking damage,
decays out of combat), **energy** (small, *regens fast*, rogue), **focus** (hunter, like energy),
**runic power** (builds from rune use), **combo points** (a 0–5 builder consumed by finishers),
**soul shards / holy power / chi** (charge-style builders), and **cooldowns** as the dominant pacing
resource. Diku/ROM: HP / mana / move, all `vital`-ish, all passive-regen on a tick. LP/Discworld: often
just GP (guild points) for spells; HP. TinyMUD: none.

The divergence: **dynamics**. 5e slots refill on a *rest event*; rage *builds on a combat event* and
*decays on a timer when out of combat*; energy regens *fast and passively*; combo points are a *bounded
builder* spent by a *finisher*; mana regens *passively*. No single "regen rate" covers these.

**(b) Deterministic/content translation.** Already mostly content. A `resource_def` has a `max` (derived
attribute) and a `regen` (per-tick). The missing dynamics are **event-driven** and **conditional**:
- *Builders* (rage, runic power, combo points) — content hooks `OnHit`/`OnDamageTaken`/`OnAbilityResolved`
  → `modify_resource(self, rage, +N)`. This is *exactly* the events-as-glue constraint. The op already
  exists; the **event** to hook does not (Phase 6).
- *Decay out of combat* (rage) — a conditional regen: `regen: −N when not in combat`. Needs the regen
  rule to be *conditional* on a content predicate (a flag / a state), not a bare constant.
- *Per-rest pools* (slots, ki, channel divinity) — refill on a **rest event** (§4): `OnRest →
  restore(self, slot_1, max)`. A "rest" is content (a `rest` command/ability that fires the event +
  applies a recovery affect); the engine just needs the event and the restore op (both exist / are
  small).
- *Spell slots specifically* — N discrete pools (`slot_1 … slot_9`), each a resource whose `max` is a
  derived attribute from the class track (§6). Casting a level-3 spell `costs: {slot_3: 1}`; upcasting =
  content choosing which slot to spend (a small Lua/op branch, or distinct ability variants). Pact magic
  / sorcery-point conversion = content ops on a rest or on demand. This is the per-rest case plus a
  cost; both expressible once the rest event exists.
- *Bounded builders + finishers* (combo points) — a resource with `max:5`; finishers read the current
  value (`scaling` by `resource(self, combo)`), then zero it. Needs ops to *read a resource into a
  damage/scaling term* and `modify_resource` to 0 — the read-into-scaling is a small op gap.

**(d) Maps onto our model as.** `resource_defs` (built) for the pools; `attribute_defs` for their
derived maxes; `modify_resource`/`restore`/`heal` ops (built) for the changes; the **event bus** (Phase
6/7) to drive event-based gain/decay; a **conditional regen predicate** on `resource_def`; a **rest
event** (§4). Cooldowns already exist as a timing field on abilities (`armCooldown`) but are transient
(not persisted) and lack a step-3 "still cooling down" check — see §7.

**(e) Verdict: mostly expressible, three small new mechanisms.**
- **[G3] Event-driven resource dynamics** — requires the **engine event bus** firing `OnHit /
  OnDamageTaken / OnKill / OnAbilityResolved` to content op-lists (the universal glue). **Phase 6** (the
  events originate in combat) with the subscription surface finished in **Phase 7** (Lua handlers).
- **[G4] Conditional / formula regen** — let `resource_def.regen` be a formula/predicate (`−5 when
  flag:out_of_combat`) rather than a constant. Small, **Phase 6**.
- **[G5] A `rest` event + resource-refill semantics** — a content `rest`/`recover` action that fires an
  event pools subscribe to; short-rest vs long-rest are just two content events with different op-lists.
  **Phase 6** (pairs with regen) or a chargen phase. Also: a **`resource → scaling` read** so a finisher
  scales by combo points (extend `deal_damage`'s `scaling` to read a resource; tiny). And **cooldown
  persistence** (§7).

> **Binding-constraint check (resources):** the constraint holds, *provided the event bus lands*. Rage,
> energy, combo points, and slots are all generic pools + content dynamics — but "content dynamics" for
> the interesting cases *means* event subscriptions. The single most important enabling mechanism for
> the resource constraint is therefore **[G3] the event bus**, not anything resource-specific. `vital`
> already decouples "death pool" from "hp" (`vitalResource()` reads the flag), so a system with no HP at
> all (a pure social MUD) or a system where the death pool is "Wounds" (WoW d20's wound points, §3 of the
> WoW RPG) both work without engine change.

---

## 4. Rest / recovery / the tick

**(a) Cross-system.** 5e: short rest (1 hr — hit dice, some pools) / long rest (8 hr — HP full, slots,
most pools). PF: 8-hr rest for slots/HP. WoW: out-of-combat regen (fast for energy, eating/drinking for
mana/HP), cooldown decay in real time — *no* slot-rest concept. Diku/ROM: the **tick** (a fixed
real-time interval) regenerates HP/mana/move and is the heartbeat of recovery; the pulse scheduler *is*
this tick. LP: regen on a heartbeat too.

**(b) Translation.** The Diku tick is already the pulse (`runRegen` on the affect/regen tick). 5e/PF
rests are a content **event**: a `rest` ability with a cast-time (or a safe-room requirement) that fires
`OnRest`, which content op-lists turn into `restore` of slots/pools + HP. Short vs long = two events.
WoW's fast out-of-combat regen = conditional regen (§3 [G4]) keyed on a combat-state flag.

**(d) Maps onto our model as.** `runRegen` (built) for the tick; a `rest` `ability_def` + the `OnRest`
event ([G5]) for tabletop rests; conditional regen ([G4]) for combat-state-dependent rates.

**(e) Verdict: Diku tick expressible-today; tabletop rests need [G5] + [G4] (Phase 6).**

---

## 5. Progression, leveling, advancement modes & character-creation timing

Binding-constraint #2, deferred today (reserved `class_defs`/`race_defs`). The richest gap.

**(a) Cross-system.**
- *Character level vs class level.* 5e: a single **character level**; class features are grants at that
  level; multiclassing splits levels across classes but caps total at 20 and shares proficiency bonus.
  PF: **per-class levels** that sum to a character level; BAB/saves are per-class and *added*; prestige
  classes are extra tracks with entry prerequisites. WoW d20: class levels; the MMO feel adds **talent
  points** per level spent in **trees**, and **dual-spec** (two saved talent allocations).
- *Timing.* 5e/PF/WoW: class chosen at creation. Many MUDs: **newbie, then join a guild at level 5** —
  the class track *starts later*, joined by an in-game action. LP/Discworld: you join a guild and then
  **advance skills by using them** — no levels at all.
- *Advancement mode.* 5e/PF/WoW: **XP-threshold auto-level** (cross a threshold → gain a level → apply
  grants), or milestone (a content trigger grants a level). Diku: XP-auto-level, sometimes **train at a
  guildmaster** (spend gold/XP to gain practices, then practice skills). Older D&D / some MUDs:
  **train-at-a-trainer** (visit an NPC, spend currency). Point-buy: spend a pool per level (stat or
  talent points). **Use-based** (LP/Discworld, also BRP's "check the box, roll to improve on rest"):
  `OnSkillUse → chance-to-improve` — an event-driven track with *no levels*.

The divergence is total: number of tracks, when they start, what advances them, and whether "level" even
exists. The union abstraction the constraint already names: **N independent advancement tracks, each
with content-defined XP/progress sources, thresholds, and per-step grants, joined by content actions.**

**(b) Deterministic/content translation.** No DM here — leveling is mechanical. The model:
- A **track** is content: `{ progress_attr, thresholds[], grants_per_step }`. `progress_attr` is just an
  attribute (`xp`, `mining_skill`, `warrior_xp`). A threshold list maps progress → step. A step's
  **grants** are an op-list run once when the step is reached: `setAttrBase` raises level/stats,
  `grant_ability`, `grant_resource` (unlock a pool), `apply_affect` (a permanent passive), set a flag.
- **Advancement modes** are *which event feeds the track*:
  - XP-auto: `OnKill → modify_attribute(xp, +reward)`; the engine checks the track's thresholds and, on a
    crossing, fires `OnLevel` → runs the grants. (Diku.)
  - Train-at-trainer: an NPC `ability` that spends a currency and runs the grant op-list directly (no
    auto-threshold). (Old-school MUD.)
  - Point-buy: a level grants a "points" resource; a `spend_points` ability `setAttrBase(+1)` per point.
    (WoW talents, 5e ASIs.)
  - Use-based: `OnSkillUse → chance(p, modify_attribute(skill, +1))`; the skill attribute *is* the track,
    no level. (LP/Discworld, BRP.)
- **Timing** is content: a class track with an empty grant at step 0 and entry gated by a "join guild"
  ability that *adds the track to the entity* at level 5. Multiclass = a second class track added by a
  content action. Prestige class = a track with entry-prerequisite checks (a `check` against attributes).
- **Grants need a place to live.** Today, attributes have per-entity base overrides and abilities are
  granted as command verbs; a `class_def`/`race_def`/`background_def`/`feat_def`/`talent_def` is a
  content **bundle**: a set of grants (attrs/resources/abilities/flags/affects) applied when chosen or
  when a track step is reached. The engine knows the *kind* "template/bundle"; the bundles are content.

**(c) Room-graph.** N/A.

**(d) Maps onto our model as.** New def-tables (the reserved `class_defs`/`race_defs` + new
`background_defs`/`feat_defs`/`talent_defs`/`track_defs`), the **events** `OnKill`/`OnLevel`/
`OnSkillUse` (the glue), and new **grant ops** (`grant_ability`, `grant_track`, `modify_attribute_base`,
`grant_resource`). Character `state` already carries `templates`/`attributes`/`skills` — the persisted
shape exists. The lifecycle and effect-op interpreter already exist to *run* a grant op-list; what's
missing is the *track machinery* (threshold checking, step grants) and the *grant ops*.

**(e) Verdict: needs-new-mechanism — the largest single area. [G6] The progression-track subsystem:**
- **[G6a]** `track_defs` + an entity's set of tracks (progress attr, thresholds, per-step grant op-list),
  with the engine checking thresholds and firing `OnLevel`/`OnTrackStep`.
- **[G6b]** **Grant ops**: `modify_attribute_base` (the constraint explicitly calls this out — raise a
  stat's *base*, the per-entity override already exists), `grant_ability`, `grant_track`,
  `grant_resource`, `revoke_*`. Additive ops.
- **[G6c]** Template/bundle def-tables (`class_defs`, `race_defs`, `background_defs`, `feat_defs`,
  `talent_defs`) — content bundles of grants + track definitions.
- **[G6d]** `OnKill` / `OnSkillUse` events (XP-on-kill, use-based advancement) — part of [G3]'s bus.
- **[G6e]** Chargen flow (point-buy, choose race/class/background, allocate ASIs/talents) — the
  interactive front end.

**Phase owner: a dedicated chargen+progression phase** (the analysis recommends slotting it after Phase
7, paired with Phase 13 account/chargen; [G6d] events come free with [G3] in Phase 6). This is deferred
today by design; the value of *this* document is to fix the **shape** (N tracks + grant ops + bundles +
events) so the phase is designed right.

> **Binding-constraint check (progression):** the constraint holds and is *well-served* by the N-track
> model — but only if **grants are ops** (so a level-up is just an op-list, reusing the whole effect-op
> machinery and the event bus) and **tracks are content** (so use-based, XP-auto, train, and point-buy
> are all "which event feeds the track" rather than four engine code paths). The risk to watch: do *not*
> let "level" become an engine concept. `level` must stay an ordinary attribute that *some* tracks
> happen to raise; a use-based MUD has tracks with no `level` attribute at all.

---

## 6. Spell slots & per-class casting resources (the progression × resource intersection)

Called out separately because it sits across §3 and §5 and is the sharpest 5e-vs-WoW divergence.

**(a) Cross-system.** 5e: discrete slots per spell level, max derived from class level (the
multiclass-caster table sums fractional caster levels — the genuinely hairy bit). PF: prepared (memorize
specific spells into slots) vs spontaneous (sorcerer: known list, flexible slots); domain/school slots.
WoW: *no slots* — cooldowns + a mana pool; some abilities have charges. Warlock pact magic: few slots,
all max level, refill on short rest.

**(b/d) Translation + mapping.** Slots = N resources whose maxes are derived attributes off the class
track (§5 grants set them); casting `costs:{slot_n:1}`; rest refills them ([G5]). Prepared casting = a
"prepared spells" list in `state` (content; a prepare-spell action moves from known → prepared). The
**multiclass spell-slot table** (fractional caster levels → a shared slot table) is the one piece the
declarative formula stack can't cleanly express — it is a lookup table keyed on a sum of weighted class
levels. WoW cooldowns = the ability cooldown timer (built, modulo persistence §7).

**(e) Verdict:** slots, prepared-casting, pact magic, and WoW cooldowns = **expressible** once [G5]
(rest) + cooldown persistence land. The **5e multiclass slot table = needs-Lua** ([G7]): a Lua
`on_resolve`/derived-attribute hook computing the slot maxes from the multiclass formula — the canonical
"complex 20%" the Lua escape hatch exists for. **Phase 7.**

---

## 7. Cooldowns, lag, the global cooldown & casting time

**(a) Cross-system.** 5e/PF: no real-time cooldowns; "once per rest" is a per-rest pool (§3). WoW: the
core pacing mechanism — per-ability cooldowns (seconds to minutes), a **global cooldown** (GCD) after any
ability, charges. MUD/ROM: **skill lag** (`WAIT_STATE`) after a skill — the round-based GCD analog —
plus per-skill cooldowns. 5e casting time (1 action / bonus action / reaction / minutes) maps to lag /
cast-time.

**(b/d) Translation + mapping.** Ability `lag` (WAIT_STATE) = the GCD; `cooldown` = per-ability cooldown;
`cast_time` = a ritual/long cast. All three are *built fields* on `ability_def` and ride the pulse
scheduler (`scheduleCast`, `armCooldown`). What's incomplete: cooldowns are **transient** (not persisted
across save/load) and there is **no step-3 "still cooling down" gate** (the timer fires-and-logs today).
Reactions (5e reaction, opportunity attacks, counterspell) are a *different* shape — §9/§11.

**(e) Verdict: mostly expressible-today. [G8] Cooldown completion + persistence** — a per-ability
cooldown map, a step-3 "is this on cooldown?" requires-gate, and serialization into `state` (so a logout
doesn't refresh cooldowns). **Phase 6** (combat pacing). The GCD is just a shared-tag lag affect (`apply
a 'gcd' affect that prevents the 'ability' tag for N pulses` — pure content on the existing tag-CC model;
no engine change). Charges = a small resource pool that regens one per cooldown.

---

## 8. Races / origins / lineages & backgrounds

**(a) Cross-system.** 5e: race grants ability bonuses, speed, traits (darkvision, resistances,
proficiencies), sometimes innate spells; 5.2 SRD shifted ability bonuses to **background** + species
traits. PF: race grants ability mods, size, speed, type, racial traits, favored class. WoW: race grants
small stat/skill bonuses + racial abilities (e.g. an escape, a stat buff), faction. Diku: race = stat
modifiers + a few flags (infravision). LP: often raceless. TinyMUD: none.

**(b/d) Translation + mapping.** A race/origin/background is a **content bundle** (§5 [G6c]): a
`race_def`/`background_def` whose grants set attribute *bases* (`modify_attribute_base`), grant abilities
(darkvision = a passive `detect`; an innate spell = a granted ability with a per-rest cost), grant
resistances (a permanent `apply_affect` feeding the damage-type matrix, or a resist attribute), and set
flags (size, type, faction). Applied at chargen. Innate-spell-once-per-day = a granted ability with a
per-rest resource.

**(e) Verdict: needs the bundle + grant ops [G6b]/[G6c] (chargen phase).** Once those exist, races are
*pure content*. Resistances-as-affect and traits-as-passive-ability are already expressible; only the
*grant-at-creation* plumbing is missing.

---

## 9. Combat — initiative, rounds, attack resolution, AC, damage types, crits, multiattack, reactions

The Phase 6 heart. Combat is **Thesis 1 applied repeatedly** over content numbers.

**(a) Cross-system.**
- *Initiative / turn order.* 5e/PF: roll initiative (`d20 + dex_mod`), act in order each round. WoW/MUD:
  no initiative — **simultaneous rounds** on a pulse; everyone in the fight resolves on `PULSE_VIOLENCE`.
  This is the decided model ([COMBAT.md](COMBAT.md) §1): round-based, ROM-derived.
- *Attack resolution.* 5e: `d20 + atk_bonus vs AC` → hit → roll damage. PF: same + iterative attacks at
  −5 each (BAB ≥ 6). WoW MMO-feel: a hit/crit/miss table by attack rating vs defense. ROM (the decided
  model, COMBAT.md §3): a *layered* pipeline — to-hit, then a **dodge/parry/block** avoidance ladder, then
  **soak** by damage type, then apply. 5e's single AC roll is a *degenerate* case of this ladder (fold
  dodge/parry/block into one AC number).
- *AC / avoidance.* 5e: one AC (armor + dex + shield). PF: touch/flat-footed/normal AC, CMD. WoW d20:
  Defense bonus by class/level + armor. ROM: evasion + dodge/parry/block skills + armor soak.
- *Damage types, resist, crit.* All systems: typed damage with resist/vuln/immune; crits (5e: double
  dice on a nat 20; ROM: a crit chance multiplier). The damage-type matrix is **built**.
- *Multiattack.* 5e: extra attacks at higher level; PF: iterative; ROM: second/third/fourth-attack
  skills, dual-wield — `attacks/round` (COMBAT.md §2).
- *Reactions / opportunity / counterspell.* 5e: a reaction per round (opportunity attack on leave,
  Shield, Counterspell, Hellish Rebuke). This is an **interrupt** triggered by an *event* on another
  actor's turn — the hardest combat shape to express.

**(b) Deterministic/content translation.** The whole pipeline is content numbers over an engine pipeline
(COMBAT.md is explicit: the engine runs the *shape*, content supplies to-hit/soak/crit *formulas*). The
attack roll is a **check** ([G2]) `vs` the defender's AC attribute, with bands `{hit, miss}` (or `{crit,
hit, miss}` by margin / nat-20 special). Damage routes through the *built* `dealDamage` mitigation
pipeline (resist matrix + soak + the PvP gate). Initiative is a per-combatant `check` writing an order
(only needed if a system wants strict turn order; the default round model doesn't). The **roll-visibility
default** matters most here — a 5e pack shows "you hit AC 15 with a 22"; a WoW pack hides it behind "Your
Mortal Strike crits for 4,210."

**(c) Room-graph.** Combat is per-room (COMBAT.md §7): a fight is among entities in one room. Movement-in-
combat = fleeing to an adjacent room (a `move` that may provoke an opportunity reaction — §11). Reach /
melee-vs-ranged = "same room" (melee) vs "an adjacent room along an exit with line-of-sight" (ranged) —
**range as exit distance**. Positioning (flanking, cover) has *no* grid; model as abstract intra-room
position tags or simply drop it (most MUDs do). Call out: **5e's 5-foot-step / disengage / flanking
do not survive the room graph** — substitute an abstract "engaged/disengaged" affect and a flanking
*chance* tied to outnumbering, not facing.

**(d) Maps onto our model as.** Phase 6's round driver on `PULSE_VIOLENCE`; the swing pipeline calling
`dealDamage` (built) per the avoidance ladder; the **check primitive** [G2] for to-hit/initiative; the
damage-type matrix (built); `attacks/round` from an attribute; crits as a check band + a damage scale;
multiattack as a loop; conditions as affects (§10). Reactions need a new event-driven interrupt
mechanism.

**(e) Verdict:** the round/swing/avoidance/soak pipeline is **Phase 6 as already designed** (COMBAT.md) —
it builds *on* `dealDamage` and needs [G2] (to-hit is a check). The genuinely new combat mechanisms:
- **[G2]** to-hit/save/initiative as checks (already counted).
- **[G9] Event-driven reactions / interrupts** — an `OnX` event (OnLeaveRoom, OnCast, OnDamaged, OnHit)
  that lets a *third party's* content op-list fire *during* another actor's action and *modify or cancel*
  it (opportunity attack, Counterspell cancels a cast, Shield raises AC after seeing the roll, Hellish
  Rebuke on taking damage). This is the hardest combat shape: it needs (i) the event to fire mid-
  resolution at an interruptible point, (ii) a way for a reaction to *consume a per-round reaction
  resource*, (iii) a way to *alter the in-flight result* (cancel the cast, add to AC before the hit
  resolves). **Phase 6** for the event points + the simple cases (opportunity attack = OnLeaveRoom →
  a granted attack); the *result-altering* cases (Counterspell, Shield) likely **need-Lua** (Phase 7)
  because they reach into an in-flight ability's state. Flag for the combat-engineer: design the swing/
  cast pipeline with **named interruptible checkpoints** that fire events, so reactions are content.
- **[G1]** floor/ceil for to-hit/AC formulas (already counted).

---

## 10. Conditions / status effects

**(a) Cross-system.** 5e conditions: blinded, charmed, deafened, frightened, grappled, incapacitated,
invisible, paralyzed, petrified, poisoned, prone, restrained, stunned, unconscious, exhaustion (6
stacking levels). PF: the same plus dazed, dazzled, entangled, fatigued, nauseated, shaken, sickened,
staggered, etc. (a longer, more granular list). WoW: stun, root, snare, silence, disarm, fear, polymorph,
disorient, bleed/poison/disease DoTs, diminishing returns on repeated CC. MUD/ROM: stun, root, silence,
slow, blind, poison, bleed (COMBAT.md §6). The 5.2 SRD condition redesign maps *cleanly* onto the
already-built tag-CC model.

**(b/d) Translation + mapping.** This is the *best-fit* area in the whole document — it's exactly what
`affect_defs` + the `prevents` tag model were built for (PRINCIPLES.md corollary 3; the srd memory notes
this maps cleanly). Each condition = an `affect_def`: `restrained` prevents `move` + grants
disadvantage-on-attacks (a modifier, or a check-band tweak); `stunned` prevents `move`/`ability`/
`weapon`; `blinded` applies an attack penalty + prevents sight-targeting; `frightened` prevents
approach + a check penalty; `poisoned` = a modifier + a DoT tick; `exhaustion` = a **stacking** affect
(the built `stackCount` mode, max 6) whose magnitude scales penalties; `prone` = a position flag + melee
advantage to attackers. The derived-harm gate already auto-PvP-gates any `prevents`-bearing affect.
WoW **diminishing returns** (each successive same-category CC is shorter, then immune) = a content
pattern: an affect that, on apply, applies a hidden "DR" affect that shortens the next application —
expressible via stacking + duration scaling, or a small Lua `on_apply`.

**(e) Verdict: expressible-today** for the vast majority. Two refinements: **(i)** "save ends at end of
turn" conditions (paralyzed-until-save) need the **check primitive** [G2] fired on the affect tick
(`on_tick` runs a save check, success → expire) — counted under [G2]. **(ii)** Diminishing returns is a
Lua `on_apply` pattern (Phase 7) for exactness, or an approximation today. Otherwise: the single
cleanest mapping in the analysis — a confirmation the tag-CC model was the right call.

---

## 11. The magic / spell system

The deepest content area; broken into casting model, components, concentration, ritual, and the spatial
spells (their own sub-analysis).

### 11.1 Casting model (slots vs points vs cooldowns)
Covered in §3/§6/§7: 5e/PF slots = per-rest resources; WoW = mana + cooldowns; sorcery points / ki =
point pools; spell schools = a content tag on the ability (for dispel-by-school, counterspell, anti-magic
fields). **Verdict: expressible** once rest [G5] + cooldown persistence [G8] land.

### 11.2 Components (verbal / somatic / material) & spell save DC
5e spells need V/S/M; silence prevents verbal, restraint/grapple prevents somatic, material components may
be consumed. **Maps onto:** the **built** tag-CC model — a spell carries tags `{cast, verbal, somatic}`;
a `silence` affect prevents `verbal`; a material cost = a `reagent` cost (the ABILITIES.md cost model has
reagents; the built `costs` is resource-only today — **[G10] reagent/item costs** on abilities, a small
addition, Phase 6/loot). Spell save DC = a derived attribute (`8 + prof + casting_mod`) used as the
`vs` of a save check [G2]. **Verdict: expressible-today** except reagent costs [G10] (small).

### 11.3 Concentration
**(a)** 5e: a caster concentrates on *one* spell at a time; taking damage forces a CON save (DC 10 or
half the damage) or the spell ends; casting another concentration spell ends the first. **(b/d)
Translation:** a `concentration` affect on the caster that (i) is single-instance (`stackIgnore` /
`stack_scope:target`, ends the prior on a new cast), (ii) holds a reference to the concentrated effect so
ending it ends that effect, (iii) on the caster taking damage (`OnDamaged` event) runs a CON save check
[G2]; on failure, expire → which dispels the linked effect. The bookkeeping (linking the affect to a
spawned effect/summon and tearing it down) is the canonical "complex 20%."
**(e) Verdict: needs-Lua [G11].** Concentration is the textbook Lua `on_apply`/`on_tick`/`on_expire`
case (the memory and ABILITIES.md both flag it): an affect whose Lua state holds the concentrated
effect's handle, whose `OnDamaged` hook fires the save, and whose expiry tears down the linked effect.
**Phase 7**, riding the [G3] event bus + [G2] check + [G9] event points.

### 11.4 Ritual casting & long casts
5e ritual = cast a spell over 10 min without a slot. **Maps onto:** an ability variant with a long
`cast_time` and no slot cost (built timing fields). **Verdict: expressible-today.**

### 11.5 Spatial spells (Thesis 2 sub-analysis)
This is the room-graph payoff. Each spell is reframed onto the exit graph and the misfits are called out.

| Spell archetype | Tabletop (continuous space) | Room-graph translation | Maps onto |
|---|---|---|---|
| **Fireball / AoE burst** | 20-ft radius sphere, save for half | "the room" (all valid targets) or "room + adjacent rooms"; **loop the harm-gate + save per target** | **[G12]** AoE/area targeting: a `for_each(targets_in(scope), …)` over `ScopeRoomLiving`, each running `dealDamage` + a per-target save check. The harm-gate already runs *per op*, so looping is safe. The memory flags this as deferred in 5.3. |
| **Cone / line** (burning hands, lightning bolt) | a shaped template | collapses to "the room" (no geometry) or "an exit direction" (line = the room the exit leads to) | same AoE op; cone≈room, line≈`move`-direction target room |
| **Dimension Door / Misty Step / teleport (short)** | move up to N feet to a seen point | move to a **room ref**: a named destination, a marked anchor, or "a random adjacent room" | the **built** `teleport(target, room)` op (reserved in ABILITIES.md §3); needs the op wired (Phase 6) + a room-ref resolver |
| **Teleport (long, named-location)** | arrive at a known location, mishap table | move to a named room ref (a recall point, a beacon); the mishap table = a `check` with bands (on-target / off-target room / mishap damage) | `teleport` + `recall` ops + [G2] for the mishap table |
| **Teleport (precise coordinates)** | arrive at exact coordinates you can see | **genuine misfit** — there are no coordinates, only rooms. Substitute: teleport to the *room* containing a marked anchor, or the nearest room. State the loss explicitly. | content substitute; no precise-coord support |
| **Wall of Fire / Stone / Force, Web, grease** | a persistent shaped zone | a **room affect** (an affect attached to the *room* entity) that ticks damage on occupants or prevents an exit/`move` tag, with a duration | **[G13]** room-scoped affects: attach an affect to a Room entity (rooms are entities; the Affected runtime is generic), ticking over `room.contents`. Also models *anti-magic field*, *silence (the spell)*, *darkness*, *hallowed ground*, *consecrate/desecrate*, *spike growth*. |
| **Summon / conjure / animate** | spawn a creature at a point | `summon`/`spawn(proto, room)` (built op, reserved) into the caster's room; the summon is an ephemeral mob, optionally concentration-linked | the **built** `spawn`/`summon` ops (wire in Phase 6); concentration link via [G11] |
| **Flight / levitate / spider climb** | vertical/3D movement | the room graph is 2.5D via `up`/`down` exits; flight = the ability to use `up` exits or ignore a `fall`/`climb` check on an exit | a flag/affect that bypasses an exit's movement check; **partial** — true free flight over terrain has no analog |
| **Movement (push/pull/grapple positioning)** | shove N feet, grapple to hold in place | push/pull = a `move` to an adjacent room (the **built** `pull`/`push` ops, reserved); grapple = a `restrained`/`grappled` affect preventing `move` (tag-CC) — *not* positional | `push`/`pull` ops + a grapple affect; positioning lost, hold-in-place preserved |
| **Detection / scrying / sight** (detect magic, see invis, scry) | sense within a radius | `scan(room)` / `detect(category)` / `reveal` queries (built, reserved) over the room (or room+adjacent for "60-ft" effects); scry = a read of a remote room's contents | the **built** perception ops |
| **Light / darkness / fog** | a radius of light/obscurement | a room affect (visibility flag) — [G13]; affects targeting/`can_see` | room affect + the visibility filter (built in targeting) |

**(e) Verdict (spatial spells):** mostly **expressible** with two new mechanisms — **[G12] AoE/area
targeting** (loop the per-target harm-gate; Phase 6) and **[G13] room-scoped affects** (attach affects
to room entities; Phase 6/7) — plus wiring the *already-reserved* `teleport`/`spawn`/`summon`/`push`/
`pull`/`scan`/`recall` ops. **Genuine misfits called out:** precise-coordinate teleport (no coords),
free-flight-over-terrain (no continuous space), and grid positioning (flanking/cover/5-ft-step) — each
gets a content substitute (room-ref / exit-flag / abstract-engagement affect), with the fidelity loss
stated rather than papered over.

---

## 12. Equipment — weapons, armor, shields, magic items, attunement, wield

**(a) Cross-system.** 5e: weapons (dice + type + properties: finesse/versatile/reach/thrown/two-handed/
ammunition/light), armor (AC by category + dex cap + str req + stealth disadvantage), shields (+2 AC),
magic items (+N, attunement — max 3 attuned, charges, sentient), proficiency gating. PF: more granular —
enhancement bonuses, specific material (cold iron, silver, adamantine for DR bypass), masterwork, weapon
groups. WoW: item level, stat budgets, gem sockets, set bonuses, durability, BoP/BoE binding, gear score.
ROM/Diku: weapon dice + damroll, armor class by slot, wear locations, `wield`/`wear`/`hold`.

**(b/d) Translation + mapping.** The mudlib already has `Physical`/`Wearable`/`Weapon`/`Armor`
components ([MUDLIB.md](MUDLIB.md) §3) and item prototypes + COW deltas. A weapon = a `Weapon` component
(dice/type/verb); armor = `Armor` (values by damage type, feeding `soak` in the built mitigation
pipeline); shield = a `Wearable` in a shield slot granting a block chance / AC. Weapon *properties*
(finesse = use dex; versatile = bigger die two-handed; reach = "adjacent room" range; thrown = a ranged
attack consuming the item) = flags/tags on the component the combat formulas read. Magic-item `+N` =
instance-delta modifiers (the loot affix system, [LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md) §3). **Charges**
= a per-item resource pool. **Attunement** = a flag + an attuned-count limit (a content rule: a
`requires` gate `attuned_count < 3`) and an `attune` action. Proficiency gating = a `requires` check
against a granted proficiency flag/skill. Material-vs-DR (PF cold iron) = a damage-type/tag the resist
matrix reads. **Set bonuses** = an `OnEquip` event counting set pieces → apply an affect (events-as-glue).

**(e) Verdict:** weapons/armor/shields/wield = **Phase 6** (combat reads the components; the soak hook is
*built*, awaiting the armor component). Magic-item `+N`/charges/affixes = **Phase 11** loot (the delta +
quality system). **[G14] Item-borne modifiers & procs** — gear contributing to the attribute mod stack
(`attributes.go` has the `modSource` seam *built and waiting* — the doc says gear is a stub there) and
weapon/armor on-hit procs (events). Wiring gear into `modSources()` is **Phase 6**; procs ride [G3]/[G9].
Attunement limits + set bonuses + proficiency gating = content over `requires`/events + [G14]. The
attribute mod-stack already anticipating gear is a strong sign this fits.

---

## 13. Economy — money, coins, encumbrance, value

**(a) Cross-system.** 5e/PF: cp/sp/ep/gp/pp (a coin hierarchy), item values, encumbrance by STR. WoW:
copper/silver/gold (100:1), vendor prices, an auction house, BoP/BoE binding, repair costs. ROM/Diku:
gold (sometimes silver), weight-based carry limit, shop buy/sell with markup. TinyMUD: none.

**(b/d) Translation + mapping.** Coins = stackable item prototypes (a `gold` item with a `count` delta)
*or* a `currency` resource pool — content's choice (the persistence shape supports stacks; a resource is
cleaner for a single currency). The coin *hierarchy* = content (a vendor's price formula converts).
Encumbrance = a derived attribute (`carry_weight = str * 15`) vs the summed `Physical.weight` of inventory,
gating with a `check` or a `prevents` affect when over. Shops = a content `Mob` with a shopkeeper flag +
buy/sell abilities (markup is a formula). Binding/BoP = the [CRAFTING.md](CRAFTING.md) binding gate
(Phase 12). Auction house = a cross-shard service (Phase 8/12).

**(e) Verdict: expressible-today** for coins/weight/shops as content (stacks + a derived carry attribute
+ a shopkeeper mob). Encumbrance enforcement wants the **check** [G2] (over-weight → a movement penalty
affect) — small. The deeper economy (binding, auction, repair) is **Phase 12** as designed. No new core
mechanism beyond what loot/crafting already plan.

---

## 14. Monsters — statblocks, multiattack, legendary & lair actions, recharge, regeneration

**(a) Cross-system.** 5e: a statblock = attributes + AC/HP + attacks (multiattack) + traits + actions +
**legendary actions** (act between turns, a per-round budget) + **lair actions** (on initiative count 20,
environment effects) + **recharge** abilities (a breath weapon usable again on a d6 roll of 5–6 each
round) + regeneration + damage resistances/immunities + condition immunities. PF: similar, plus CR, SR
(spell resistance), DR/type. WoW: bosses with phases, enrage timers, adds, mechanics, threat tables, soft
enrage. ROM/Diku: HP/damage/AC + special procs (a `spec_fun`), aggression, wander.

**(b/d) Translation + mapping.** A monster = a `Mob` prototype + the same attribute/resource/affect/
ability content a player uses (the engine doesn't distinguish — a mob casts the same `fireball`). HP/AC =
attributes; attacks = abilities; multiattack = `attacks/round`; traits = passive affects; resistances =
the damage-type matrix + condition immunities = a `prevents`-immunity affect (an affect that grants
immunity to a tag/category). **Recharge** = a per-ability cooldown whose "ready" is a `check` (d6 ≥ 5) on
each round — a check [G2] on the round event. **Regeneration** = a `heal` on a tick affect (built).
**Legendary actions** = an event-driven budget: a per-round "legendary point" resource the boss spends on
extra abilities outside its turn — needs the round event + the action-budget pattern (events + a resource;
[G3]). **Lair actions** = a room-scoped scheduled effect on `PULSE_VIOLENCE` (a room affect [G13] or a
zone-script trigger firing abilities). **Boss phases / enrage** = content scripting (Lua triggers on
HP thresholds → apply an enrage affect / spawn adds) — the [WORLD-EVENTS.md](WORLD-EVENTS.md) / Lua layer.
**Threat/aggro** = COMBAT.md §7's threat list (Phase 6).

**(e) Verdict:** ordinary statblocks, multiattack, resistances, regeneration, recharge = **expressible**
(Phase 6 + [G2] for recharge). Legendary actions = the action-budget pattern over [G3] events + a resource
(**Phase 6**, no new primitive beyond the bus). Lair actions / phases / enrage = **Lua triggers** (Phase
7) + room affects [G13] — the [WORLD-EVENTS.md] orchestration (Phase 10) for region-wide boss ripples.
Threat = Phase 6. No monster-specific engine primitive needed beyond the event bus and [G13].

---

## 15. Presentation — map / overworld / GMCP (driver-emits-data)

**(a/b) The constraint.** Presentation is driver-emits-data; the rich *client* is out of scope; honor the
room graph. Some MUDs want a colorful UTF-8/extended-ASCII overworld map (Dwarf-Fortress style) when
leaving town, client themes, a HUD.

**(d) Maps onto our model as.** Two delivery paths, both already planned:
1. **GMCP structured data** (Phase 9): `Room.Info` (+ room **coords** [deferred, see the room-coordinates
   memory] + sector/terrain), `Char.Vitals/Stats/Status`, `Mud.*` (cooldowns, target, afflictions). The
   combat/affect events already emit the *data* (COMBAT.md §8, ABILITIES.md §8 reserve the GMCP emit
   points); Phase 9 wires the encoder. "GMCP-first, ANSI/text-fallback" so clients theme it.
2. **Server-side UTF-8 + color map** rendered as a view/mode and pushed as output — needs **room coords**,
   a **region/zone map model**, and a **color/ANSI output renderer** (the edge does UTF-8-safe input strip
   today; rich color *output* is flagged-future edge work).

**Room-graph constraint:** any map is a projection of the room/exit graph onto coords; it is *not*
continuous terrain. A "Dwarf-Fortress overworld" is a coords-per-room minimap, not a tile world.

**(e) Verdict:** **Phase 9** (GMCP) + room coords (deferred) deliver the structured-data path with **no
new core mechanism**. The server-side color map is **edge/future work** (a renderer + a zone map model) —
**[G15] ANSI/UTF-8 color output renderer + zone map model**, low priority, post-Phase-9 edge. The graphical
client app itself stays out of scope.

---

## 16. Basic-MUD support (the acceptance question)

The constraint is explicit: *can it still be a plain Diku / LP / Tiny MUD?* Confirming this is as
important as chasing 5e.

- **TinyMUD / MUSH (contentless social/building).** Bare engine + OLC. **Confirmed natural today.** The
  bare-engine invariant (`empty_world_test.go`) boots with zero attributes/resources/abilities; an entity
  with no content has no pools, no stats, `attr()`/`resourceCurrent()` return 0/absent sanely. Building
  (rooms/exits/objects) is the mudlib core (Phase 3) + content pipeline (Phase 4). Socials are act()
  templates (built). Channels are Phase 8. **The only gap is in-game OLC** (a building command set writing
  content rows) — a tooling feature, not an engine primitive; **[G16] in-game OLC** (a content phase /
  Phase 4 follow-on). Nothing in the rich-tabletop work threatens this — it's the *floor*, and the floor
  exists.
- **DikuMUD / Merc / ROM (fixed pools, levels, ticks).** **Confirmed natural / nearly fully built.** HP/
  mana/move = three `vital`-ish resources with `regen` on the pulse tick (built — `runRegen` *is* the
  Diku tick). Level = an attribute; XP-auto-level = a track [G6] fed by `OnKill` [G3]. Stat apply tables =
  derived attributes. Combat = Phase 6 (ROM is the *decided* model — COMBAT.md is literally "ROM,
  refined"). Skills-as-commands with lag = built (`ability_def.lag`). This is the **easy subset** the
  engine was shaped around; the only deferred pieces are the track [G6] and the combat pipeline (Phase 6),
  both already planned. A plain ROM is the most *directly* supported target.
- **LPMud / Discworld (skill-use, soft-coded objects).** **Mostly natural; one defining mechanism is the
  use-based track.** Skills = attributes; advance-through-use = `OnSkillUse → chance(p,
  modify_attribute(skill,+1))` ([G6] track in use-based mode + [G3] event). No levels = a track with no
  `level` attribute (the N-track model explicitly admits this). Soft-coded objects = **Lua** (Phase 7) —
  the engine's `Scripted` component + the curated Lua API is exactly the LPMud "every object has code"
  model. GP-for-spells = a resource. Guild-join = a content action adding a track. **Confirmed natural
  once [G6] (use-based mode) + [G3] (OnSkillUse) + Phase 7 land** — all already planned. The risk to
  watch (§5 note): if "level" leaks into the engine, the no-levels LP case breaks; the N-track model
  prevents this *by design*.

**Verdict:** all three heritage baselines remain first-class. TinyMUD works *today* (modulo OLC tooling
[G16]); Diku is the most-directly-supported target (Phase 6 + [G6] finish it); LP needs the use-based
track mode + OnSkillUse + Lua, all planned. **No part of chasing 5e/PF/WoW has cost the simple
baselines** — the same primitives (generic resources, N tracks, events, affects) serve all four tiers,
which is the strongest evidence the abstraction is right.

---

## 17. Consolidated gaps → roadmap

Every new mechanism the analysis surfaced, the systems that need it, and the phase that should own it.
"Expressible-today" areas are omitted (attributes, conditions, basic equipment-as-components, coins,
ordinary statblocks, ritual casts, the Diku tick, TinyMUD).

| # | New mechanism | Why / who needs it | Verdict | Phase owner |
|---|---|---|---|---|
| **G1** | Formula vocabulary: `floor`/`ceil`/`round`/`mod`/conditional heads | 5e modifiers, PF BAB/saves, WoW ratings — exact integer derivation | new (small) | **6** (or chargen) |
| **G2** | **The check/save/contested primitive** + outcome **bands** + dice notation (kh/kl/dF/pool) + visibility config + `OnCheck` | *everything*: climb checks, saves, attacks, initiative, contested, PbtA/BRP/Blades breadth, save-ends conditions | **new — the central gap** | **6** |
| **G3** | **The event bus** firing `OnHit/OnDamageTaken/OnKill/OnAbilityResolved/...` to content op-lists | rage/runic/combo builders, XP-on-kill, use-based skills, procs, set bonuses, legendary/recharge, concentration | new — the universal glue | **6** (origin) + **7** (Lua handlers) |
| **G4** | Conditional / formula resource regen | rage decay out of combat, WoW fast OOC regen | new (small) | **6** |
| **G5** | `rest`/`recover` event + resource-refill; resource→scaling read | 5e/PF slots & per-rest pools, short/long rest, combo finishers | new (small) | **6** / chargen |
| **G6** | **Progression-track subsystem**: track_defs + thresholds + grant ops (`modify_attribute_base`, `grant_ability/track/resource`) + bundle defs (class/race/background/feat/talent) + chargen | all leveling/multiclass/use-based/point-buy/train; races/feats/talents | **new — the largest area** | **dedicated chargen+progression phase** (+ [G3] events from 6) |
| **G7** | Multiclass spell-slot table (fractional caster levels) | 5e multiclassing | needs-Lua | **7** |
| **G8** | Cooldown completion: per-ability map + step-3 gate + persistence; charges | WoW cooldowns/charges, MUD per-skill cooldowns | new (small) | **6** |
| **G9** | **Event-driven reactions/interrupts** + interruptible-checkpoint events; result-altering | opportunity attacks, Counterspell, Shield, Hellish Rebuke | new; simple cases Phase 6, result-altering needs-Lua | **6** (points) / **7** (alter) |
| **G10** | Reagent / item costs on abilities | 5e material components, consumables | new (small) | **6** / **11** |
| **G11** | **Concentration** (linked-effect affect + OnDamaged save + teardown) | 5e/PF concentration spells | needs-Lua | **7** (on G2/G3/G9) |
| **G12** | **AoE / area targeting** (loop the per-target harm-gate + per-target save) | fireball, cone, line, any multi-target | new | **6** |
| **G13** | **Room-scoped affects** (attach affects to room entities; tick over occupants) | wall spells, web, darkness, silence-spell, anti-magic, lair actions, consecrate | new | **6**/**7** |
| **G14** | Item-borne modifiers into the attr mod-stack + weapon/armor procs (the `modSource` seam is built-and-waiting) | magic items `+N`, set bonuses, on-hit procs | new (seam exists) | **6** (mods) + **11** (affixes) |
| **G15** | Server-side ANSI/UTF-8 color output renderer + zone map model | overworld map, themed output | new (low priority) | **edge / post-9** |
| **G16** | In-game OLC (building command set writing content rows) | TinyMUD/MUSH building, all builder workflows | tooling | **content phase / Phase 4 follow-on** |

**The wire-up-the-reserved-ops list (no new primitive, just connect what's documented/reserved):**
`teleport`, `spawn`, `summon`, `push`/`pull`, `recall`, `scan`/`detect`/`reveal`/`identify`, the GMCP emit
points, the gear `modSource`, and the `attacks/round`/`soak`/threat hooks (COMBAT.md) — all reserved in
ABILITIES.md §3 / the code and slot into Phase 6/9.

**Top 8 new mechanisms (the headline):** [G2] the check primitive (Phase 6) · [G3] the event bus (Phase
6/7) · [G6] the progression-track subsystem (a chargen+progression phase) · [G12] AoE targeting (Phase 6)
· [G13] room-scoped affects (Phase 6/7) · [G9] event-driven reactions (Phase 6/7) · [G11] concentration
(Phase 7) · [G8] cooldown completion+persistence (Phase 6).

---

## 18. Open design questions for the user

**ALL RESOLVED 2026-06-26** — the user settled every fork before Phase 6; these are now
binding design inputs for Phases 6/7 and the chargen+progression phase:

1. **Roll visibility:** HIDDEN by default; opt-in `show`; overridable per pack/ability/check.
2. **Check primitive home:** lives in the EFFECT-OP INTERPRETER (invokable from exits, objects,
   affect-ticks, AND abilities), built as a near-term Phase-6 PREFIX — so non-combat checks
   (climb a wall) don't wait for the full combat pipeline.
3. **Spatial fidelity:** ACCEPT the room-graph's losses (no tactical grid / precise-coord
   teleport / flight positioning); use room-ref + abstract-engagement substitutes; build NO
   intra-room coordinate system.
4. **Outcome-band arity:** FULL ordered-band generality from day one (binary 5e = the 2-band
   case); the engine stays ignorant of BOTH dice shape and outcome arity.
5. **Progression:** N independent advancement TRACKS; `level` is an ordinary attribute (no
   engine specialness); all four advancement modes (XP / train / point-buy / use-based) are
   content; the dedicated chargen+progression phase lands AFTER Phase 7 (so it has the event
   bus + Lua).
6. **Reactions (v1):** the Phase-6 swing/cast pipeline exposes named interruptible CHECKPOINTS
   firing events; easy reactions (opportunity attacks) are declarative content; result-altering
   ones (Counterspell/Shield) are Lua.

The original analysis of each fork follows.

1. **How deterministic do we surface the dice? ([G2] visibility default.)** Per-system default + per-
   action override is the recommendation — but what's the *engine default*? A 5e pack wants the math shown
   ("22 vs AC 15 — hit"); a WoW/IRE-style pack wants it hidden behind flavor. Recommendation: hidden
   default, opt-in show, both overridable. **User call:** confirm the default and the granularity (per
   pack? per ability? per check?).

2. **Where does the check primitive live — combat phase or ability lifecycle? ([G2] home.)** Attack rolls
   and saves are checks *and* combat is built on them. Recommendation: the `check` flow op lives in the
   **effect-op interpreter** (so an exit/object/affect-tick/ability can all invoke it), and Phase 6
   *consumes* it for to-hit — i.e. build the primitive *slightly ahead of* the combat pipeline, not inside
   it. **User call:** accept that ordering (check primitive is a near-term Phase-6-prefix), or defer checks
   into combat and accept that non-combat checks (climb a wall) wait for Phase 6 too.

3. **How much spatial fidelity to chase? (Thesis 2 boundary.)** The room graph cleanly handles AoE-as-room,
   teleport-as-room-ref, range-as-exit-distance, and wall-spells-as-room-affects. It *loses* precise-coord
   teleport, free flight, and grid positioning (flanking/cover/5-ft-step). Recommendation: accept the loss,
   use the abstract-engagement / room-ref substitutes, *don't* build an intra-room coordinate system.
   **User call:** confirm we're not chasing tactical-grid fidelity (a large, system-specific effort that
   would strain the room-graph constraint).

4. **The outcome-band arity of [G2].** Binary (5e save) vs three-tier (PbtA) vs degrees (BRP) vs shifts
   (FATE) vs pools (Blades). Recommendation: an ordered **band list** (binary is the 2-band case) so the
   union abstraction holds across the whole catalog. **User call:** confirm we design for the full band
   generality now (cheap at design time, expensive to retrofit) vs. binary-now-extend-later.

5. **When does the progression/chargen phase land, and is "level" allowed to be special?** Recommendation:
   a dedicated phase after Phase 7 (so it has the event bus + Lua), N tracks, `level` strictly an ordinary
   attribute. **User call:** confirm the N-track-no-special-level shape (the LP/Discworld case depends on
   it) and the phase placement.

6. **Reactions: how far into result-altering do we go? ([G9].)** Opportunity attacks (a granted attack on
   OnLeaveRoom) are easy. Counterspell/Shield (cancel/alter an *in-flight* ability after seeing a roll) are
   hard and intrude on the combat pipeline's structure. Recommendation: design Phase 6's swing/cast pipeline
   with **named interruptible checkpoints** that fire events, support the easy cases declaratively, and put
   result-altering in Lua. **User call:** confirm we accept Lua-only result-altering reactions for v1, or
   want declarative Counterspell (more pipeline complexity).

---

## 19. Where the binding constraints are strained (and by which target)

Honest accounting of where a target system pushes a constraint to its edge:

- **Generic resources — strained by WoW's resource zoo, *held* by the event bus.** Rage (build + decay),
  combo points (bounded builder + finisher), runic power, energy — none is expressible as "a pool with a
  regen rate." They are expressible as "a pool + event-driven dynamics," which is on-constraint *only
  because* [G3] the event bus exists. The constraint holds, but its viability is **contingent on [G3]**;
  without the bus, the resource constraint is not actually satisfiable for the WoW capstone. This is the
  single most important downstream dependency in the document.
- **N-track progression — strained by 5e multiclass slot math and PF prestige prerequisites; *held* by
  letting grants be ops and one Lua escape ([G7]).** The track model is generous, but the 5e multiclass
  *spell-slot table* is a genuine lookup that doesn't fit declarative formulas — it is the one place the
  progression constraint needs the Lua hatch. That's acceptable (it's the documented 20%) but worth naming.
  The deeper risk is *cultural*: keeping `level` a mere attribute under pressure from three systems that
  all center "level" — the LP/Discworld no-levels case is the canary that keeps us honest.
- **Events-as-glue — not strained, but *load-bearing far beyond its current built state*.** The bus is
  referenced as Phase 6/7 and currently fires nothing to content. This analysis shows it underpins
  resources ([G3]), progression ([G6d]), reactions ([G9]), concentration ([G11]), procs ([G14]), and
  monster legendary/recharge — i.e. it is the highest-leverage unbuilt mechanism. Its generality
  (content-subscribable, scoped, ordered for durable cases per WORLD-EVENTS.md) must be designed deliberately,
  not bolted on.
- **Full-spectrum — not strained; *confirmed* (§16).** The same primitives serve TinyMUD through WoW. The
  only spectrum risk is the engine accreting a rich-tabletop assumption (a hardcoded "level", "hp", "d20",
  or "success/failure") that breaks the simple tiers — which the band-generality ([G2]) and N-track
  ([G6]) and `vital`-flag (built) decisions specifically guard against.

---

## 20. Validation requests (domain-expert agents)

Things this analysis asserts about engine reality or proposes as mechanism that the owning engineers
should confirm before the phases are designed:

- **combat-engineer:** confirm the Phase 6 swing/cast pipeline can host (i) [G2] checks as the to-hit/save
  resolution, (ii) [G9] named interruptible checkpoints firing events mid-resolution, (iii) [G12] AoE as a
  per-target loop over the *built* `dealDamage` gate, and (iv) `attacks/round` + the avoidance ladder over
  content formulas. Validate that the built `soak()`/`modSource` seams are where gear ([G14]) and armor land.
- **abilities-engineer:** confirm [G2] (a `check` flow op alongside `if`/`chance`), [G10] (reagent costs),
  [G13] (attaching affects to room entities via the generic Affected runtime), and the resource→scaling read
  all fit additively in the effect-op interpreter without a lifecycle change.
- **progression-engineer:** validate the [G6] N-track shape (track_defs + grant ops + bundle defs + the
  four advancement modes as "which event feeds the track") and that `modify_attribute_base` over the
  existing per-entity base override is the right grant primitive; confirm `level`-as-attribute holds.
- **scripting-engineer:** confirm [G7] (multiclass slot math), [G11] (concentration teardown), [G9]
  result-altering reactions, and monster lair/phase scripting are the right Lua-hatch boundary, and that the
  curated handle API + `self.state` + the event subscription surface support them.
- **persistence-engineer:** confirm [G8] (cooldown serialization into `state`), the track/grant state shape
  in `state` JSONB, and that the new def-tables ([G6c] class/race/background/feat/talent, check_defs,
  loot/affix) follow the per-kind-table + JSONB-tail + `pack` pattern with no schema strain.
- **mudlib-engineer:** confirm [G16] in-game OLC scope and that the TinyMUD/Diku/LP baselines (§16) hold
  against the mudlib core as built (containment, command table, act(), the `Scripted` component for LP).

---

# Launch-hardening burn-down round 2 (2026-07-01)

Post-roadmap security/hardening items burned down from `docs/REMAINING.md`, each verified (`make verify`) and
committed green. Kept here for posterity; the live TODO no longer lists them.

## §1 Security

- **Cross-shard handoff snapshot authentication (was MEDIUM).** `Handoff.Prepare` was unauthenticated (its
  token was a bare `sha256(character/epoch)` idempotency hash), so a forged Prepare on a reachable inter-shard
  port could inject an arbitrary `state_json` (item dupe / econ break) past the pack-set audit. Closed by
  signing the integrity-critical Prepare fields with Ed25519: a shared cluster keypair (config
  `handoff_signing_key`/`handoff_verify_key`, env `TELOS_HANDOFF_{SIGNING,VERIFY}_KEY`), source signs, the
  destination verifies at Prepare (PermissionDenied before any zone/state work), enforced only when a verify
  key is configured (keyless dev/test unaffected — mirrors the session-assertion seam). The digest is a
  length-prefixed SHA-256 over a domain separator + routing tuple + carried state
  (`internal/world/handoffsig.go`); a security-auditor pass confirmed it covers 100% of the rehydrate-consumed
  fields and the cheap-reject ordering is right. A startup WARN fires if only one half of the keypair is set.
- **Corpse-owner PersistID keying.** `CorpseOwner` now carries the killer's PersistID and the loot gate
  prefers it (name fallback when either side lacks a PID), so a freed+reclaimed name can't match a stale 60s
  loot window. `internal/world/death.go`, `container.go`.
- **Mail evict-oldest-READ retention sweep.** A full inbox holding any READ mail now evicts the oldest read
  message to accept new mail (an all-UNREAD inbox still refuses); parity across the pgx + mem stores.
  `internal/store/mail.go`, `internal/world/memstore.go`.
- **`__Host-` broker cookie prefix.** The OAuth flow cookie is named `__Host-telos_oauth` under TLS
  (Secure + Path=/ + no Domain), unprefixed in dev-over-http. `internal/web/session.go`.
- **Mid-session hear-access republish.** `republishCommsOnAccessChange` (hooked at the affect apply/expire
  sites, cheap-guarded to a no-op unless a channel actually gates hearing) re-publishes the comms hear-set
  when an affect crosses a channel's `min_attr` floor / require-flag. `internal/world/commsstate.go`,
  `affect_runtime.go`.
- **Durable `characters.state` byte cap.** A log-only soft cap (`maxDurableStateBytes`, mirrors the handoff
  carry cap) warns on unbounded state growth at the saver, off the zone goroutine. `internal/world/saver.go`.

## §8 Housekeeping

- **Dropped dead auth schema.** Migration `00017_drop_dead_auth_tables.sql` drops `account_auth` (passphrase)
  and `ssh_keys` — Phase 15 removed every reader (OAuth-only). Reviewed by persistence-engineer + auth-engineer.
- **Corrected the stale `internal/web/oauth.go` package comment** to the Phase-15 device/broker OAuth bridge
  (no dashboard/forms/Play bridge). auth-engineer-confirmed.
- **De-flaked two gate tests.** `TestSessionLockTakeoverKicksDisplacedConnection` — poll-Acquire until the
  login's own lock token is observed as the previous holder, a happens-before that stops the login's initial
  Acquire from clobbering the simulated takeover. `TestChannelLineRendersVerbatimNoTellPrefix` — retry the
  gossip send (harness `tryExpect`) until the async channel subscription is live (MemBus has no replay).
  Both reviewed by test-engineer + edge-engineer, `-race -count=10` clean.

Still open in §8 (contingent on unbuilt features or other clusters): `ClearPlayer` dir cleanup (awaits
`ClearPlayer`), cross-respawn op-list guard (with the respawn-sickness slice), multi-vital support (a 2nd
authored vital pool), instanced zones (a later content phase), the death-mag cap (folded into §4 `xp_value`),
a stale-Phase-14-docstrings sweep, and the builder-guide hot-reload note (docs/wiki project).

## §4 Content / itemization

- **Restorative ops honor dice + bonus.** `opHeal`/`opRestore` compute `amount + rolled dice + scoped bonus`
  like `opDealDamage` (a `2d8 + $actor.wis_bonus` heal), actor-scoped; the non-negative clamp preserves the
  ungated-heal invariant. Reviewed by abilities-engineer + security-auditor.
- **Formula evaluator fails closed on NaN/±Inf** (found in the heal-dice security review). `evalFinite` rejects
  a non-finite TOP-LEVEL result (a tamed intermediate Inf that min/clamp bounds is still allowed); the three
  formula consumers (check bonus, attribute base, grant base) route through it, closing the `int(+Inf)`=maxint
  damage / NaN-attribute-poison vector. Fuzz test now asserts finiteness; the stale "never NaN" comment fixed.
- **OnKill kill-magnitude cap.** `killMagnitude` prefers an explicit content `xp_value`, else the vital-pool
  max capped at `maxKillMagnitude`, so a tanky mob can't be farmed for outsized XP. Loot is threat-based /
  mag-independent (XP-scoped). Reviewed by combat-engineer + progression-engineer. (Closes the §8 death-mag item.)
- **Affect-hook reconciliation.** `OnApplyAffect`/`OnAffectExpire`/`OnAffectTick` already fire (7.8b) — the
  stale "no-op-with-a-log" comment on `fireOnApplyAffect`/`fireOnAffectExpire`/`fireOnTick` was corrected;
  a content `on_event` subscriber does get the callback. Only `OnRest` stays dark (no rest mechanic to fire it).
- **Recipe skill-gate resolves a track's level_attr.** `RecipeDTO.Track` (optional): the skill gate + quality
  scaling resolve the attribute from the track_def's `level_attr` live (fallback to the raw `skill` attr), so a
  recipe follows its track instead of duplicating the level attr. Wired through the recipe_defs JSONB (no
  migration); the demo leather-vest recipe now uses `track`. Reviewed by progression-engineer + persistence-engineer.
- **Profession cap: content-configurable + uncapped kind.** The cap is the content attribute `max_professions`
  (defaults to 2 when unset; a class/feat can raise it); `BundleDTO.Uncapped` marks a gathering/utility
  profession as unlimited/non-counting; only capped professions count (unknown => capped, safe). Wired through
  the bundle_defs JSONB (no migration). Reviewed by progression-engineer + persistence-engineer.
