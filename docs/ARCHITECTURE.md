# TelosMUD Architecture

> Target: design as if it must support millions of concurrent players. Real MUDs run in
> the hundreds-to-thousands, but the topology below is the standard "stateless edge +
> sharded stateful core + message bus" triad that scales out cleanly.

## 1. System overview

```
                         ┌──────────────┐
   Browser ── OAuth ───▶ │ telos-account│  (Google/Discord/GitHub)
                         │  OAuth broker │  issues accounts, characters
                         └──────┬───────┘
                                │ gRPC (device-code auth)
                                ▼
 telnet            ┌────────────────────────┐        per-player gRPC bidi stream
 clients ─────────▶│      telos-gate (N)     │──────────────┐
 (Mudlet, etc.)    │  protocol edge, GMCP    │              │
                   └────────────┬───────────-┘              ▼
                                │                  ┌────────────────────┐
                                │ NATS (chat,      │   telos-world (M)   │  zone shards
                                │ presence, who)   │  actor-per-zone     │
                                └─────────────────▶│  Lua VM per zone    │
                                                   └─────────┬──────────┘
                                                             │
                            ┌───────────────┬────────────────┼───────────────┐
                            ▼               ▼                ▼               ▼
                         Postgres        Redis             NATS         Directory
                      content+saves   sessions/cache    world bus    zone->shard map
```

Independently-scalable planes:

- **Edge (`telos-gate`)** — terminates terminal connections. Stateless beyond live sockets;
  scale horizontally behind an L4 (TCP) load balancer, per region.
- **World (`telos-world`)** — the simulation. Stateful, sharded by zone. Scales by adding
  shards and rebalancing zones.
- **Orchestration (`telos-director`)** — region/world director actors that own supra-zone
  state and drive cross-zone & global events (area reactions, world-altering quests, MUD-wide
  invasions). Leader-elected per scope, isolated from simulation shards. See
  [WORLD-EVENTS.md](WORLD-EVENTS.md).
- **Coordination** — Postgres, Redis, NATS, and a directory service tie it together.

## 2. Connection lifecycle

1. Client opens telnet to a gate (via LB). Gate runs telnet option negotiation
   (see §6) and, if the client supports it, GMCP.
2. Gate runs the **auth handshake** (§7), resolving the connection to an `accountID` +
   `characterID` via `telos-account`.
3. Gate looks up the character's current zone in the **directory**, finds the owning shard,
   and opens a **gRPC bidi stream** to it: `PlayConn(stream<PlayerIn, PlayerOut>)`.
4. Gate pumps decoded player input as `PlayerIn` frames; world pushes `PlayerOut` frames
   (text + GMCP messages) which the gate encodes back onto the wire.
5. On **cross-shard movement**, the source shard performs a handoff (§4); the gate
   transparently re-dials the destination shard. The TCP connection never moves.
6. On disconnect, the gate signals the shard; the shard runs link-death handling
   (linkdead timer, then save + despawn or leave the avatar sleeping).

The **gate holds the socket; the world holds the player**. This separation is what lets us
redeploy world shards without dropping connections (drain + handoff), and scale edges
independently of simulation load.

## 3. Concurrency model — actor per zone

The core invariant: **a zone is owned by exactly one goroutine.** All rooms and entities in
that zone are mutated only by that goroutine, so game logic needs **no locks**.

```go
func (z *Zone) Run(ctx context.Context) {
    pulse := time.NewTicker(PulseInterval) // e.g. 100ms
    for {
        select {
        case <-ctx.Done():
            z.shutdown(); return
        case cmd := <-z.inbox:
            // player/admin commands routed to this zone
            z.dispatch(cmd)
        case ev := <-z.events:
            // cross-zone / world events (arrivals, tells)
            z.handle(ev)
        case <-pulse.C:
            // heartbeat: combat, regen, affects, resets
            z.tick()
        }
    }
}
```

- **Entities are data, not goroutines.** A player, mob, or item is a plain struct owned by
  its zone. This avoids the classic "10k goroutines fighting over locks" failure mode and
  keeps simulation deterministic per zone.
- **Input path:** gate -> shard's stream handler -> router -> `zone.inbox`. The router maps
  `playerID -> *Zone` within the shard.
- **Output path:** game code calls `entity.Send(msg)` -> buffered on the player's session ->
  flushed as `PlayerOut` frames on the gRPC stream -> gate -> wire.
- **Mobs/NPCs** share the same event + command machinery as players; their "sink" routes to
  AI hooks (and Lua) instead of a socket.
- **Lua** runs *inside* the zone goroutine (one VM per zone), so scripts see a consistent,
  single-threaded world. Long scripts are bounded by an instruction-count quota so one bad
  trigger can't stall a zone.

### Tick / heartbeat
A single base pulse (e.g. 100ms) drives derived timers per zone:
`PULSE_VIOLENCE` (combat round ~2---4s), regen, affect decay, and zone resets (repop). Each
zone owns its own timers, so there is no global lock-step. A tick that exceeds its budget is
logged + metered (lag pulse) for capacity planning.

## 4. Sharding & movement

- **Unit of sharding = zone** (a.k.a. area: a coherent cluster of rooms — "Midgaard",
  "The Sewers"). Zones are assigned to shards by consistent hashing, overridable via the
  directory for manual pinning / rebalancing.
- **Locality matters.** Adjacent zones are colocated on the same shard so the common case —
  walking room to room — is an in-process channel send, not a network hop.
- **Directory service** (Redis-backed): authoritative routing, resolved in two hops so a
  zone's logical owner is decoupled from where that owner runs:
  `zone -> shard-id` (a leased claim — the cardinal single-writer guard, one shard per zone),
  `shard-id -> endpoint` (each shard registers and heartbeats its own dial address), and
  `player -> shard-id`. To route a player into another zone you resolve `zone -> shard-id ->
  endpoint`, so a zone can be moved between shards by rewriting one lease — no exit, snapshot,
  or peer list names an address. Gates and shards consult it; it publishes change events on
  NATS for cache busting.
  - **Invariant: shard ids are unique per process** (assign them like k8s pod / StatefulSet
    ordinals). The zone lease treats "same owner = renewal", so two processes sharing a shard
    id would both pass the claim and become two writers. The directory enforces this
    defensively — a second process registering a live shard id under a different endpoint is
    refused at boot (`ErrShardConflict`), before it can claim any zone — but the orchestrator
    is the real guarantor of uniqueness.
- **Intra-shard movement:** zone A hands the entity to zone B via a Go channel. Cheap.
- **Cross-shard movement (handoff):**
  1. Source zone serializes a player snapshot (stats, inventory, affects, position).
  2. Sends it to the destination shard (gRPC `Handoff` RPC) which rehydrates the entity in
     the target zone and ACKs.
  3. Directory updated (`player -> newShard`); gate notified to re-dial; old session torn
     down after the gate's new stream is live (brief double-buffer to avoid lost input).
- **Hot zones** (newbie areas, town square) are the real scaling limit — one zone = one
  goroutine = one core. Mitigations: occupancy caps and sharding a "town" into sub-zones.
  Documented as a known constraint, not hand-waved.

## 5. Coordination plane

- **Postgres** — durable store. Content (read-mostly, cached in shards at load) and player
  saves (write-back). Content read-replicas; player tables partitionable by account hash.
- **Redis** — sessions/presence, the directory backing store, distributed locks (e.g.
  "only one live session per character"), rate limits, and hot caches. Pub/sub for fast
  invalidation.
- **NATS** — the world bus. Subjects for chat channels, tells, who-list/presence, global
  events (boss spawns, server broadcasts), and handoff coordination. JetStream where
  durability/replay matters (e.g. offline tells, mail). Scales to millions of msgs/sec.
- **Directory** — thin service/library over Redis resolving `zone->shard` / `player->shard`.

### Save strategy
Player state is saved on a cadence (e.g. every 60s), on logout, and on significant events
(level up, major loot, quest completion). Writes go through a Redis write-back cache so
Postgres sees batched, not chatty, traffic.

## 6. Terminal protocol (the gate)

The gate implements the telnet option dance and normalizes everything to `PlayerIn`/
`PlayerOut`:

- **Telnet options:** `ECHO` (off for password entry), `SGA`, `NAWS` (window size -> reflow,
  minimap sizing), `TTYPE`/MTTS (terminal/client capabilities), `CHARSET` (UTF-8 negotiation).
- **MCCP2/3** — zlib stream compression (telnet options 86/87). Big win for bandwidth at
  scale; negotiated per connection.
- **GMCP** — telnet option **201 (0xC9)**. Subnegotiation framing
  `IAC SB 201 <"Package.Message" + space + JSON> IAC SE`. See [GMCP.md](GMCP.md).
- **Line discipline** — telnet edit, IAC escaping, partial-line prompts (`>` without
  newline), and ANSI/xterm-256 color passthrough.

Each connection uses two goroutines (read/write) over a buffered framer. With tuned socket
buffers a single gate handles ~50---100k connections; millions = dozens of gates per region.

## 7. Authentication (OAuth <-> telnet)

Telnet can't speak OAuth, so the gate bridges via a device-code flow. Auth is **OAuth-only** —
there are no passwords, passphrases, or link codes on the wire.

1. Telnet: the player types `connect`. The gate mints a short-lived device code and hands the
   player a one-click browser link to the **`telos-account` OAuth broker**.
2. In the browser, the player completes OAuth (Google/Discord/GitHub); the broker resolves-or-
   creates the account and flips the pending device session to authed.
3. The gate polls the broker, resolves the connection to the account, acquires the single-
   session lock in Redis, and drives prompt-based character selection / chargen.
4. On a dev deployment, `TELOS_DEV_AUTOAUTH` bypasses the broker for smoke testing.

## 8. Repository layout

Monorepo, Go workspace (`go.work`). The engine lives in `internal/world` (the simulation:
entities, components, zones, command parser, act() messaging, combat, tick, event bus,
affects, comms, the Lua runtime, GMCP) — pure engine, content-agnostic. It depends on
persistence/transport only through interfaces (`internal/store`, `internal/directory`,
the bus packages), so it stays testable and headless.

```
telosmud/
  go.work
  api/proto/                 # protobuf: play (gate<->world), account, handoff
  cmd/
    telos-gate/              # telnet edge
    telos-world/             # world shard
    telos-account/           # OAuth broker + account/character API
    telos-director/          # region/world director (orchestration tier)
    telos-migrate/           # schema migrations
    telos-seed/              # content-pack importer
    telos-botswarm/          # synthetic-telnet load generator
  internal/
    world/                   # ── THE ENGINE ── entities, components, zones, command
                             #   parser + targeting, act(), combat, tick, event bus,
                             #   affects, comms, Lua runtime + sandbox + hot reload,
                             #   GMCP; cross-shard handoff (content-agnostic)
    content/                 # content packs + the DTO->prototype loader, chargen
    account/                 # account/character service, device-code auth
    web/                     # the telos-account OAuth broker (device-code bridge)
    assertion/               # signed account assertions
    telnet/                  # option negotiation, MCCP, NAWS, GMCP framing
    gate/                    # edge session lifecycle, linkdeath, stream to world
    store/                   # Postgres + Redis repositories, mail
    directory/               # zone->shard / player->shard locator, leases
    placement/               # claim-from-pool + the rebalance planner
    director/                # director actor: scopes, scoped bus, leader election
    scopebus/ contentbus/ commbus/ presence/   # NATS-backed buses
    sessionlock/ checkpoint/ config/ metrics/ obs/ textsan/
    botswarm/                # load-tester internals
  deploy/                    # docker-compose, pg/redis/nats config
  docs/
```

## 9. Mudlib core abstractions

- **Entity (ECS-lite):** an id plus components — `Describable`, `Physical` (weight/size),
  `Container`, `Living` (hp/mp/stats), `Mobile`, `Wieldable`, `Scripted`. Avoids deep
  inheritance; new content types = new component combos.
- **World tree:** Zone -> Room -> Entities; containers nest recursively.
- **Command:** `interface{ Names() []string; Execute(ctx *CmdCtx) }`, registered in a trie
  for abbreviation. The parser handles ordinals/quantifiers (`get all.coin`, `wield 2.sword`),
  and scoped targeting (inventory/room/equipment).
- **act() messaging:** the beloved Diku idiom — one call emits per-observer perspectives:
  actor sees "You pick up a sword", the target sees "Kurt picks up a sword", bystanders see
  the right thing, and the item/visibility rules are centralized.
- **Heartbeat hooks:** entities register for pulses (combat round, regen, affect tick).
- **Event bus (per zone):** typed events (`EnterRoom`, `Death`, `Say`, `GetItem`) fan out to
  observers — AI, GMCP emitters, and Lua triggers all subscribe through the same channel.
- **Output sink:** every entity has a `Sink`; players fan to the gate, mobs to AI/Lua, log
  channels to admin tooling — uniform `Send`.
- **Lua API:** scripts get a curated surface (`room`, `me`, `actor`, `send`, `spawn`,
  `after(ms, fn)`, event hooks). No `os`/`io`; CPU-bounded; one VM per zone.

## 10. Scaling summary & known limits

| Layer     | Scales by                         | Bottleneck / mitigation                              |
|-----------|-----------------------------------|------------------------------------------------------|
| Gate      | add instances behind L4 LB        | fd limits, buffers; ~50---100k conns/instance          |
| World     | add shards, rebalance zones       | hot single zone = 1 core -> caps / sub-zones          |
| NATS      | clustering, subject partitioning  | fan-out on huge global channels -> shard channels     |
| Postgres  | read replicas, partitioning       | player write rate -> Redis write-back + batching      |
| Redis     | cluster mode                      | hot keys (presence) -> local TTL caches on shards     |

## 11. Cross-cutting

- **Observability:** OpenTelemetry traces across gate->world->store; Prometheus metrics
  (conns, tick lag, zone occupancy, NATS lag, save latency); structured logs via `slog`.
- **Graceful ops:** shards support drain (stop accepting handoffs, migrate players, save),
  enabling rolling redeploys without mass disconnects.
- **Testing:** the mudlib runs headless (no network) for fast unit/integration tests; a bot
  swarm tool drives synthetic telnet load against gates for capacity testing.
