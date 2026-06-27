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
