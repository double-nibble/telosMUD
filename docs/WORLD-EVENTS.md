# Scopes, world state & cross-zone events

Scaffolding for consequences that exceed a single object or zone: an area-wide reaction to a
boss death, a quest that re-shapes other rooms, a MUD-wide invasion. The engine's existing
docs gave us per-zone events ([ARCHITECTURE.md](ARCHITECTURE.md) §3) and NATS transport — this
doc adds the **scope hierarchy**, the **director actors**, and the **scoped event bus** that
turn those into first-class, builder-usable mechanisms.

Status: **proposal** — three choices flagged in §10.

> **The golden rule:** cross-scope effects are *message-passing, never shared mutation*. A
> script never reaches into another zone (the [LUA.md](LUA.md) §4 invariant stands). It
> *signals*; the engine routes the signal; each affected zone applies the consequence locally
> in its own goroutine. This is the same philosophy as the cross-shard handoff.

---

## 1. The scope hierarchy

Five addressable scopes, each a state container an event can target:

| Scope    | Owns                                   | Single-writer home          |
|----------|----------------------------------------|-----------------------------|
| Entity   | per-entity state (`self.state`)        | the owning zone goroutine   |
| Room     | room state                             | the owning zone goroutine   |
| **Zone** | zone state (the actor)                 | the zone goroutine          |
| **Region** | region state (a builder "area/city") | the **region director** (§3)|
| **World** | global state                          | the **world director** (§3) |

- A **region** is a content-defined grouping (`region_defs`: an id + its member zones). It may
  span multiple zones/shards — a "city" the builder thinks of as one place is often several
  zones (recall hot zones can be split). Region ≠ shard.
- Region and world state have a **single owning writer** (a director), so even global state
  never has two writers — the actor model, one level up.

## 2. Reads vs writes across scopes (CQRS-ish)

- **Reads are local & cached.** Each zone keeps a read-only replica of the region/world state
  it cares about; zone scripts read it synchronously (`world.flag("invasion")`,
  `region:get("mood")`). Replicas update when a director broadcasts a change. Eventually
  consistent, lock-free.
- **Writes go to the owner.** A zone script never mutates region/world state directly; it
  **signals** the director, which applies the change (single writer) and broadcasts the new
  value down to member zones. No write contention, ever.

## 3. Director actors — orchestration that belongs to no zone

A **director** is a singleton stateful actor owning a region's or the world's state and running
its orchestration logic (a "director script"). It has its own state bag, heartbeat, and event
inbox — and is the serialization point for its scope.

**Directors run in a dedicated `telos-director` service tier** — a fourth deployable alongside
`telos-gate`, `telos-world`, and `telos-account`. Each director is still an *actor* internally
(inbox + tick + sandboxed Lua VM, the same model as a zone), so the mental model carries over —
but it is hosted **out-of-band from the simulation shards**, so orchestration never competes
with zone ticks for CPU and can be scaled and deployed independently. **Leader election** (a
Redis lease / etcd) ensures exactly one live instance owns each region/world scope, with
failover to a standby on crash. Directors reach zones over the scoped event bus (§4) and
persist scope state to Postgres directly. So the actor pattern now spans four tiers: gate
(edge), zone (simulation), director (orchestration), account (auth).

A MUD-wide invasion *is* a world-director script: it starts the event, sends spawn commands to
target zones, tracks kills reported back by those zones, advances phases on its heartbeat, and
concludes with rewards — none of which lives in any room.

## 4. The scoped event bus

A game-level abstraction over NATS. Channels are named by scope:

```
world.<event>            region.<id>.<event>            zone.<id>.<event>
```

- **Publish:** `signal(scope, event, payload)` — to a director (a command) or to subscribers (a
  broadcast).
- **Subscribe:** scripts declare interest; the engine subscribes the hosting zone/director:
  `on_world(event, fn)`, `on_region(event, fn)`, `on_zone(event, fn)`.
- **Routing:** a region/world broadcast fans out over NATS to every shard hosting a member
  zone (and the director); each delivers it to the relevant zone's **event inbox**, where the
  handler runs single-threaded and mutates only local entities.

### Reliability tiers
Events declare a tier (see §10 D3):

- **`transient`** — cosmetic, fire-and-forget over NATS core (e.g. "distant horns sound"). Lost
  if a shard is momentarily down; no harm.
- **`durable`** — state-changing, over **JetStream**: at-least-once delivery, persisted, with
  **idempotency keys** so redelivery or a shard restart never double-applies, and per-scope
  **ordering** (the director is the sequencer). The invasion's start/phase/end events are
  durable; a shard that was down catches up on reconnect.

## 5. Lua API additions

Extends the [LUA.md](LUA.md) surface. Zone scripts get read + signal; director scripts get the
authoritative write API.

```lua
-- in any zone script (read replica + signal up):
world.flag("invasion_active")            -- cached read
region:get("mood")                       -- cached read
signal_region("boss_slain", {by = killer:id()})   -- command up to the region director
on_region("city_liberated", function(ev) ... end) -- react to a broadcast, locally
on_world("invasion.phase", function(ev) ... end)

-- in a director script (owns the scope, single writer):
world.set("invasion_active", true)       -- authoritative write
region:set("mood", "liberated")
broadcast_region("city_liberated", {hero = ev.by})        -- fan out to member zones
spawn_in("duskwall:gate", "mob:raider")  -- remote effect → delivered as a command to that zone
mud.after(300, next_wave)                -- director heartbeat scheduling
```

**Remote effect ops:** a director acting "at a distance" (spawning a mob in a specific room of
a zone it doesn't own) does not mutate that zone — `spawn_in` compiles to a **command** placed
on the target zone's inbox and applied there locally. Same single-writer guarantee.

## 6. Worked examples — the three cases

**(a) Boss death ripples across a city**
1. Boss dies in room R (zone A). Its `on("death")` runs in A's goroutine.
2. Script: `signal_region("boss_slain", {by = killer:id()})` (durable).
3. The Duskwall **region director** receives it, sets `region.mood = "liberated"`, and
   `broadcast_region("city_liberated", …)`.
4. Every member zone (A, B, …, across shards) gets it in its inbox; subscribed room/mob scripts
   react locally — guards leave, gates open, vendors restock, ambient text changes.

**(b) A quest changes how other rooms behave** — two distinct flavors:
- **Per-player** (the world looks different *to you*): a room script checks
  `actor:has_flag("amulet_done")`. Purely local, already supported, no new scaffolding.
- **World-altering** (the change is permanent for everyone): quest completion does
  `signal_world("gate_opened", …)` → the world director sets the flag and broadcasts; rooms
  everywhere react and the change persists. Uses the scaffolding here.

**(c) A MUD-wide invasion** — a world-director script:
1. Triggered (timer/admin/threshold) → `world.set("invasion_active", true)`,
   `broadcast_world("invasion.start")` (durable).
2. On its heartbeat, sends `spawn_in(...)` waves to target zones; zones report kills via
   `signal_world("raider_killed", …)`.
3. Director tallies, advances phases (`broadcast_world("invasion.phase", {n=2})`), and on
   completion broadcasts the outcome + triggers rewards. All orchestration in one actor; all
   consequences applied locally by zones.

## 7. Persistence

- **World state** → a `world_state(key, value JSONB, version)` table; **region state** →
  `region_state(region_id, key, value JSONB, version)`. Owned/written only by the respective
  director; versioned for the same optimistic-concurrency backstop as characters
  ([PERSISTENCE.md](PERSISTENCE.md) §7).
- `region_defs` (id + member zones) is ordinary content in the per-type definition tables.
- Directors ride the same durability ladder; on failover the standby loads the last persisted
  scope state and replays any un-acked durable events.

## 8. Failure semantics

- **Director crash** → leader election promotes a standby; it restores persisted scope state
  and JetStream replays in-flight durable events (idempotency prevents double-apply).
- **Member shard down during a broadcast** → durable events are redelivered on reconnect; the
  zone catches up. Transient events are simply missed (acceptable by tier).
- **Signal storm** (a thousand mobs die at once) → directors coalesce/debounce where the script
  opts in (`signal_region(..., {coalesce = true})`), and the durable stream provides
  backpressure rather than overrunning a director.

## 9. What this does *not* change

The single-writer invariant, the no-cross-zone-handle rule, and the actor-per-zone spine are
all intact — this scaffolding is built *on* them, not around them. Cross-scope power comes
entirely from messages, directors, and local application.
