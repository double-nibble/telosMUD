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
- **10.3b TODO (zone read-replica + Lua reads):** each shard keeps a read-only replica of the region/world
  state it cares about, updated when a director broadcasts a change over the scoped bus. Entry points:
  the shard struct (internal/world/world.go `Shard`) holds the replica + a `scopebus.Subscribe` on its
  region/world scopes; the Lua surface is added in internal/world/luaentry.go (the `world`/`region` handle
  tables) → synchronous cached `world.flag("x")` / `region:get("k")`. Each zone learns its region from
  `LoadedContent.Regions` (a zone-ref → region-ref lookup). The director doesn't broadcast yet (10.4), so
  10.3b is tested by publishing a synthetic scope broadcast on the bus and asserting the Lua read sees it.
- **10.3c TODO (signal-up):** `signal_region`/`signal_world` Lua builtins → a zone commands UP to the
  director via `scopebus.SignalDurable` (a command, never a cross-scope mutation). The director-side
  apply is 10.4; 10.3c is the zone→bus emit + its Lua surface + the no-cross-scope-write invariant test.

### 10.4 — Director writes + broadcast + remote effects
- The director's authoritative write API (`world.set`/`region:set` — single-writer, versioned persist +
  broadcast down). `broadcast_region`/`broadcast_world` fan-out to member zones across shards.
  `spawn_in`/remote-effect → a COMMAND on the target zone's inbox, applied locally. `on_region`/`on_world`
  zone handlers react to broadcasts locally.

### 10.5 — Capstone (the done-when)
- Boss death → `signal_region` (durable) → region director sets mood + `broadcast_region` → member zones
  on different shards react locally — surviving a director restart (standby promotion + JetStream replay +
  idempotency). Demo content + the e2e milestone + the director-restart-mid-ripple chaos test.

### 10.6 — Dynamic zone placement (director as zone coordinator)
- World servers CLAIM zones from a pool (replaces `TELOS_ZONES`). LIVENESS stays decentralized via the
  existing lease-claim (any server/standby claims an orphan on lease expiry; directory CAS = one winner;
  jittered retries) — works with NO director. The director adds BALANCE: nudge toward `floor(Z/S)`/server,
  locality-aware, with rebalance hysteresis + cooldowns; standby promotion; graceful zone DRAIN (the
  per-player handoff fanned over a zone); crash-failover rehydrate from the durability ladder. Adopting it
  touches ZERO gate/handoff code (the directory seam already decouples "which zone" from "which server").

## Open decisions (WORLD-EVENTS §10 flagged three; surface + settle with the owning engineer as hit)
- D1/D2/D3 (the doc references a §10 that isn't written): the signal coalescing/debounce shape; the
  read-replica delivery guarantee; the reliability-tier default. Settle per-slice with the
  distributed-systems-architect, then lock in a test.
