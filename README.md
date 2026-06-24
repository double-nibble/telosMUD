# TelosMUD

Imagine a platform engineer who builds large distributed systems decided to
rewrite his favorite childhood text-based game engine.

A modern, over-engineered, horizontally-scalable MUD driver.

Players connect over **telnet** (SSH later), authenticate against an account created
via **OAuth** on the companion website, and play in a world simulated by a fleet of
**zone-sharded world servers**. Rich clients (Mudlet, custom web clients) get minimap +
HUD data over **GMCP**.

This repo contains the **mudlib**, content-agnostic game engine, plus the
services that run it at scale. World *content* (rooms, mobs, items, quests) is
authored later in Lua and lives under `content/`.

## Planes

| Plane         | Service(s)        | Responsibility                                              |
|---------------|-------------------|------------------------------------------------------------|
| Web/Account   | `telos-account`   | OAuth, account & character records, telnet link codes      |
| Edge          | `telos-gate`      | Telnet/SSH, protocol negotiation, GMCP framing, MCCP        |
| World         | `telos-world`     | Zone-sharded simulation, mudlib, Lua scripting             |
| Orchestration | `telos-director`  | Region/world directors, cross-zone & global events, spawns |
| Coordination  | NATS, Redis, PG   | Chat/presence/handoff bus, session routing, durable state  |

## Docs

- [docs/PRINCIPLES.md](docs/PRINCIPLES.md) — **the driving pillar:** engine = mechanism, content = flavor
- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — system design, concurrency, sharding, data model
- [docs/PROTOCOL.md](docs/PROTOCOL.md) — gate<->world stream + cross-shard handoff contracts
- [docs/MUDLIB.md](docs/MUDLIB.md) — entity/component model, world tree, command parser, targeting
- [docs/COMBAT.md](docs/COMBAT.md) — round-based combat, avoidance+soak, afflictions
- [docs/ABILITIES.md](docs/ABILITIES.md) — data/Lua-driven skill & effect framework
- [docs/PERSISTENCE.md](docs/PERSISTENCE.md) — schema, durability ladder, content storage
- [docs/LUA.md](docs/LUA.md) — scripting API surface & sandbox/safety model
- [docs/WORLD-EVENTS.md](docs/WORLD-EVENTS.md) — scopes, world/region state, directors, cross-zone events
- [docs/LOOT-AND-SPAWNS.md](docs/LOOT-AND-SPAWNS.md) — loot tables, rarity/affixes, pity, scheduled boss spawns
- [docs/CRAFTING.md](docs/CRAFTING.md) — rarity/binding, professions, deconstruction, crafting, material economy
- [docs/ACCOUNT.md](docs/ACCOUNT.md) — OAuth -> telnet auth bridge, SSH, sessions
- [docs/GMCP.md](docs/GMCP.md) — GMCP package spec for minimap/HUD clients
- [docs/ROADMAP.md](docs/ROADMAP.md) — phased build plan (distributed skeleton first)

## Key decisions

- **Concurrency:** actor-per-zone. One goroutine owns a zone's rooms + entities; no locks
  on game state. Entities are plain data, not goroutines.
- **Sharding:** the unit is a *zone*. Zones map to world-server shards via a Redis-backed
  directory; adjacent zones are colocated to minimize cross-shard movement.
- **Scripting:** embedded Lua (`gopher-lua`), one sandboxed VM per zone, hot-reloadable.
- **Transport:** gRPC bidi stream per player (gate <-> home shard) for ordered I/O; NATS for
  chat, presence, who-list, and cross-shard zone handoff.
- **Persistence:** Postgres for content + player saves; Redis for sessions, presence,
  locks, and write-back caches.
