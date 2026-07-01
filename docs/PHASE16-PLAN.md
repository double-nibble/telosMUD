# Phase 16 — Hardening & scale

Status: **COMPLETE (2026-07-01).** All slices (16.1 metrics · 16.2 bot-swarm · 16.3 backpressure · 16.4 FULL
zero-drop drain) landed CI-green. The user chose the FULL drain (over the narrow clean-save fallback), which
un-deferred runtime zone-add + the rebalance executor: runtime `HostZone`, the fenced `HandoverZone` lease
flip, zone-lease renewal moved into the shard, the `AdoptZone` RPC, `BeginDrain` + the SIGTERM wiring, and the
hermetic zero-drop capstone. Deferred (docs/FOLLOW-UPS.md): bounded drain fan-out concurrency, drain metrics +
clean-disconnect, director-owned/serialized target selection, runtime-zone scope-replica registration.
The last roadmap phase — next is the end-of-roadmap wiki.

Original plan below (kept for the decision record).

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
- **16.4 — Graceful shard drain (FULL zero-drop, approved 2026-06-30).** The user chose the full version over
  the narrower "clean-save + reconnect" fallback, which un-defers two Phase-10.6 pieces (runtime zone-add +
  the rebalance drain executor). Players keep playing the SAME zone across a rolling redeploy. Built as four
  sub-slices, each verify+review+green:
  - **16.4a — Runtime zone-add (world).** `Shard.HostZone(id)`: the standby already has every zone's room
    prototypes (`defineContent` fills the cache from ALL loaded zones, not just the won set), so hosting a new
    zone at runtime is: build the zone from the retained `LoadedContent`, `adopt` it, launch `z.Run` on the
    shard's run ctx, and claim its directory lease + start renewal. Makes `s.zones` runtime-mutable — guard it
    with the existing `s.mu` behind `zoneByID`/`zonesList` accessors (per-attach/move reads, not per-tick, so a
    mutex is fine). The standby model already exists (a shard that wins no zones from its pool runs as a
    standby, registered + heartbeating); this gives it the "live re-claim" its own boot comment promised.
  - **16.4b — Drain choreography (world + director).** `Shard.BeginDrain(ctx)`: (1) set a `draining` flag that
    REJECTS new fresh-login attaches (a re-dial/handoff bind is still accepted so an in-flight move completes);
    (2) resolve a target shard for each hosted zone — the director assigns the drained zones to a standby via
    `HostZone` + a lease handover (release-then-claim so `ShardForZone` flips to the standby); (3) fan
    `beginHandoff` over every live player to the new owner (reusing the exact cross-shard handoff — the gate
    holds the socket open across the Redirect); (4) wait until every zone is empty or a deadline, then `Drain`
    (flush) + return. Correctness: single-owner lease handover, the both-serve window, epoch monotonicity, and
    in-flight handoffs started before the drain flag — reviewed by the distributed-systems architect before it
    lands.
  - **16.4c — SIGTERM wiring (cmd/telos-world).** Replace the post-cancel `GracefulStop` (best-effort flush
    only) with: on signal → `BeginDrain(ctx-with-timeout)` while the zone+saver goroutines stay ALIVE → wait
    for drain-complete or timeout → then cancel + `GracefulStop` + exit. The drain runs BEFORE the zone loops
    stop, which is the whole point (§6 said the flush must precede ctx cancel).
  - **16.4d — Capstone test.** A hermetic 2-process (+standby) topology: players on shard S under load,
    SIGTERM S, players migrate to the standby now hosting the same zone and keep issuing commands with zero
    dropped connections; S exits clean. Honest scope (per the 16.3 review): "zero-drop" covers HEALTHY
    connections — a client wedged mid-drain is deadline-reclaimed and counted separately.
- **Capstone.** The bot swarm sustains the target tick rate at the agreed scale (16.1 confirms p99 tick-lag),
  AND a rolling redeploy drains a shard with zero dropped connections (the load test keeps running across it).

## Deferred (not Phase 16)

- **Instanced zones** — party dungeon copies on the Phase-10.6 dynamic-placement substrate (director mints/
  reaps instances + routes a party to one). A world/content feature, folded into a later content phase.
  Recorded in docs/FOLLOW-UPS.md.
