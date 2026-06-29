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
