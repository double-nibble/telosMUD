# Phase 16 — Hardening & scale

Status: **LOCKED (2026-06-30).** Building 16.1 → 16.4 + capstone. The last roadmap phase before the
end-of-roadmap wiki. Prove the topology scales and survives operations: see the system under synthetic load,
protect a shard from slow clients, and drain a shard for a rolling redeploy with zero dropped connections.

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
- **16.4 — Graceful shard drain.** Wire `Shard.Drain` on SIGTERM: stop accepting new attaches → hand every
  live player off to a peer shard (director-coordinated, reusing the handoff) → flush → exit. The gate's
  redirect handling keeps each socket open across the move (zero dropped connections).
- **Capstone.** The bot swarm sustains the target tick rate at the agreed scale (16.1 confirms p99 tick-lag),
  AND a rolling redeploy drains a shard with zero dropped connections (the load test keeps running across it).

## Deferred (not Phase 16)

- **Instanced zones** — party dungeon copies on the Phase-10.6 dynamic-placement substrate (director mints/
  reaps instances + routes a party to one). A world/content feature, folded into a later content phase.
  Recorded in docs/FOLLOW-UPS.md.
