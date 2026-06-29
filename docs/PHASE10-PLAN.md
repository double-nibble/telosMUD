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

### 10.2 — Scoped event bus (cross-zone transport)
- Scope channels `world.<event>` / `region.<id>.<event>` / `zone.<id>.<event>` over NATS core
  (`transient`) + JetStream (`durable`: at-least-once, idempotency keys, per-scope ordering, director =
  sequencer). The signal/broadcast plumbing, extending the Phase-8 commbus pattern.

### 10.3 — region_defs + zone read-replica + signal-up
- `region_defs` content (id + member zones) in the definition tables. The zone-side CACHED read replica
  of region/world state (eventually consistent, lock-free) → Lua `world.flag("x")` / `region:get("k")`.
  `signal_region`/`signal_world` (a zone commands UP to the director; never mutates cross-scope).

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
