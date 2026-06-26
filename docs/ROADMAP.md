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

### Phase 6 — Combat
Round-based resolution on top of the substrate. ([COMBAT.md](COMBAT.md))
- `PULSE_VIOLENCE`, attacks/round, the avoidance ladder + soak pipeline.
- Skills-as-commands with lag/cooldowns; death → corpse; threat/assist.

**Done when:** you can fight a mob through the full pipeline (miss/dodge/parry/block/soak),
kill it, and loot its corpse.

### Phase 7 — Lua scripting
The curated escape hatch + sandbox. ([LUA.md](LUA.md))
- `gopher-lua`, one VM per zone, curated handle API, strict budget + circuit breaker.
- Triggers, ability `on_resolve` in Lua, affect hooks, hot reload; `self.state` persistence.

**Done when:** a room script fires on entry and a scripted mob greets you — edited live.

## Track C — World & clients

### Phase 8 — Comms over NATS
- Channels (`gossip`, `newbie`), tells, `who`, presence — all cross-shard via NATS.
- JetStream for offline tells/mail.

**Done when:** two players on different shards chat and see each other in `who`.

### Phase 9 — GMCP
Rich-client data. ([GMCP.md](GMCP.md)) (`Room.Info` can be pulled forward to Phase 3.)
- Negotiation (option 201), `Core.Supports` filtering, MCCP2, NAWS.
- `Char.Vitals/Stats/Status`, `Room.Info`, `Char.Items.*`, `Comm.Channel.Text`, `Mud.*`.

**Done when:** Mudlet shows a live vitals gauge and a minimap that updates as you walk.

### Phase 10 — Orchestration (directors, scopes, event bus)
Supra-zone state and cross-zone consequences. ([WORLD-EVENTS.md](WORLD-EVENTS.md))
- `telos-director` tier with leader election; region/world state (single-writer).
- Scoped event bus: `transient` (NATS core) + `durable` (JetStream, idempotent, ordered).
- Remote effect commands into zones; the Lua `world.*` / `region:*` / `signal_*` API.
- **Dynamic zone placement** (the director hosts the zone coordinator): world servers
  *claim* zones from a pool instead of declaring them, with balancing, standbys, and
  failover/rebalance. Builds on Phase 4 (crash-failover rehydrates from the durability
  ladder). Design: [PLACEMENT.md](PLACEMENT.md). Replaces static `TELOS_ZONES`.

**Done when:** a boss death in one zone ripples a region-wide change across zones on different
shards, and survives a director restart.

## Track D — Progression & economy

### Phase 11 — Loot & scheduled spawns
([LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md))
- Loot resolver on death: roll kinds, rarity tiers, personal loot, pity counters.
- Item quality/affix rolls into instance deltas (coarse v1; deep affixes deferred).
- Director-owned durable scheduled spawns (weekly boss; wall-clock, restart-safe).

**Done when:** a weekly boss spawns on schedule and drops personal loot with a working pity
timer.

### Phase 12 — Crafting & economy
([CRAFTING.md](CRAFTING.md))
- Rarity/binding (BoP rules, tier-dependent component binding) + the transfer/bind gate.
- Professions, recipes, stackable items, deconstruction (salvage yields = weighted rolls).
- `consume_item`/`produce_item`/`augment_item` ops; crafting stations.

**Done when:** you disenchant a bound epic into tradeable mats and craft a new item at a
station.

## Track E — Services & scale

### Phase 13 — Auth & website
([ACCOUNT.md](ACCOUNT.md)) (replaces the stub login used since Phase 1.)
- `telos-account`: OAuth (Google/Discord/GitHub), accounts, content-driven chargen.
- Link-code + passphrase + SSH pubkey; TLS/SSH transports; signed session assertions;
  single-session lock.

**Done when:** create an account on the web, get a link code, `connect` over TLS/SSH.

### Phase 14 — Hardening & scale
- Bot-swarm load tester (synthetic telnet + GMCP); tick-lag, occupancy, NATS-lag metrics.
- Backpressure on slow clients; graceful shard drain for rolling redeploys; instanced zones.

**Done when:** N thousand synthetic players sustain target tick rate; a shard can be drained
and redeployed with zero dropped connections.

---

## Starting point
Build **Phase 0**, then the **Phase 1–2 spine** before any engine depth — they carry the
riskiest integration (the gate↔world stream and the cross-shard handoff), and every later
phase assumes them. After that the engine track (3–7) is mostly linear; tracks C–E layer on
top once the content pipeline (Phase 4) is in place.
