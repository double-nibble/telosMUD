# TelosMUD Architecture

> Target: design as if it must support millions of concurrent players. Real MUDs run in
> the hundreds-to-thousands, but the topology below is the standard "stateless edge +
> sharded stateful core + message bus" triad that scales out cleanly.

## 1. System overview

```
                         ┌──────────────┐
   Browser ── OAuth ───▶ │ telos-account│  (Google/Discord/GitHub)
                         │  + website   │  issues accounts, characters, link codes
                         └──────┬───────┘
                                │ gRPC/REST (auth, link-code verify)
                                ▼
 telnet/SSH        ┌────────────────────────┐        per-player gRPC bidi stream
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
- **Directory service** (Redis-backed): authoritative `zone -> shard` and `player -> shard`
  maps. Gates and shards consult it; it publishes change events on NATS for cache busting.
- **Intra-shard movement:** zone A hands the entity to zone B via a Go channel. Cheap.
- **Cross-shard movement (handoff):**
  1. Source zone serializes a player snapshot (stats, inventory, affects, position).
  2. Sends it to the destination shard (gRPC `Handoff` RPC) which rehydrates the entity in
     the target zone and ACKs.
  3. Directory updated (`player -> newShard`); gate notified to re-dial; old session torn
     down after the gate's new stream is live (brief double-buffer to avoid lost input).
- **Instanced zones** (dungeons, player housing) are spun up on demand on the least-loaded
  shard and torn down when empty.
- **Hot zones** (newbie areas, town square) are the real scaling limit — one zone = one
  goroutine = one core. Mitigations: occupancy caps, sharding a "town" into sub-zones, or
  instancing. Documented as a known constraint, not hand-waved.

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
- **SSH (later)** — `golang.org/x/crypto/ssh`; clean public-key auth (register pubkey on the
  website -> no password on the wire). GMCP has no SSH equivalent, so rich data rides a
  parallel channel or an in-band escape for SSH clients.

Each connection uses two goroutines (read/write) over a buffered framer. With tuned socket
buffers a single gate handles ~50---100k connections; millions = dozens of gates per region.

## 7. Authentication (OAuth <-> telnet)

Telnet can't speak OAuth, so we bridge:

1. User signs in on the website via OAuth (Google/Discord/GitHub) -> account + character(s)
   created in `telos-account`.
2. Website offers either a short-lived **link code** (6---8 chars, Redis TTL, one-shot) or a
   user-set **MUD passphrase** (Argon2id hashed).
3. Telnet: `connect <character> <code|passphrase>`. The gate verifies via `telos-account`,
   acquires the single-session lock in Redis, and binds the connection to the character.
4. GMCP clients may submit credentials via `Char.Login` instead of typed commands.
5. **SSH (later):** register an SSH public key on the website; the gate authenticates by key
   and maps it straight to the account — no secrets typed in the world.

## 8. Repository layout

Monorepo, Go workspace (`go.work`). The **mudlib is `internal/mudlib`** — pure engine,
depends only on interfaces (no concrete transport/DB), so it stays testable and content-
agnostic.

```
telosmud/
  go.work
  api/proto/                 # protobuf: gate<->world, account, handoff
  cmd/
    telos-gate/              # telnet/SSH edge
    telos-world/             # world shard
    telos-account/           # OAuth + account/character API
    telos-admin/             # ops CLI (rebalance zones, broadcast, drain)
  internal/
    mudlib/                  # ── THE ENGINE ──  (content-agnostic)
      entity/               #   ECS-lite: id + components
      world/                #   zone / room / exits
      command/              #   parser, verb registry, targeting ("get 2.sword")
      act/                  #   act()-style perspective messaging ("$n gets $p")
      combat/               #   pluggable combat rules
      tick/                 #   heartbeat scheduler
      event/                #   in-zone typed event bus
      affect/               #   timed buffs/effects
      comm/                 #   channels, tells, says
      script/               #   Lua runtime + exposed API, sandbox, hot reload
      gmcp/                 #   GMCP registry, packages, per-conn encoder
      persist/              #   save/load interfaces (impl lives in store/)
    telnet/                  # option negotiation, MCCP, NAWS, GMCP framing
    ssh/                     # (later)
    transport/               # gRPC client/server glue, PlayerIn/Out framing
    bus/                     # NATS wrappers (chat, presence, handoff)
    store/                   # Postgres + Redis repositories (implements persist)
    directory/               # zone->shard / player->shard locator
    session/                 # connection/session lifecycle, linkdeath
  content/                   # (LATER) world data + Lua — NOT engine code
  deploy/                    # docker-compose, k8s, pg/redis/nats config
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
| World     | add shards, rebalance zones       | hot single zone = 1 core -> caps / sub-zones / instances |
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
