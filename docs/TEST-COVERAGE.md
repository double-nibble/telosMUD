# Test coverage gap-map

This is the honest, unflinching map of every player-visible journey and cross-cutting
behavior built across Phases 1–7 against its current **black-box** coverage (e2e /
in-process-harness / integration / smoke). It is an accountability artifact AND the
burn-down backlog: each wave of follow-up work clears rows here.

> **Why this exists.** As of this writing the pyramid is inverted: **438 unit/integration
> tests, 1 e2e test, 2 Postgres-integration tests.** Unit tests are hermetic single-shot —
> they structurally cannot see the stateful, multi-run, multi-service, failure-path bugs
> that bite a distributed MUD in production (the seed/`deletePack` idempotency bug and a
> day-long red CI from a Dockerfile/lint-config break both slipped past `go test`). The
> black-box tiers below are where those regressions get caught. The grade for the black-box
> tiers today is honest: **thin**.

## How we verify (the discipline this doc enforces)

| Step | Command | Surface |
| --- | --- | --- |
| Pre-commit (hermetic) | `make verify` | gofmt + `go vet` + `go build` + `go test ./... -race` + `golangci-lint`. The exact hermetic CI matrix. Gated tests `t.Skip` (no infra). MUST be green before any commit. |
| Release-shaped (Docker) | `make verify-full` | `make verify` + `make smoke-twice` (the Dockerfile/compose/seed surface `go test` cannot see). |
| Gated Postgres tier | `make test-integration` | `tests/integration` + `internal/store` against a live Postgres (`make deps`). |
| Gated e2e tier | `make test-e2e` | `tests/e2e` (`-tags e2e`) against a live gate (`make up`); SKIPs if the gate is down. |
| After push | `gh run watch` / `gh run view` | The 6 CI jobs: `go`, `lint`, `proto`, `integration`, `smoke`, `e2e`. **Verification is not done until CI is green** — a tier that `go test` never exercised (Docker, lint-action config) only proves out in CI. |

The right tier for a behavior:
- **unit** — pure logic, one component, no seams (owned by domain engineers; QA *hardens*).
- **in-process harness** (`internal/gate/harness_test.go`) — real world shards + real gate over
  bufconn + scripted telnet client, **hermetic, fast, deterministic**. The sweet spot for
  cross-service journeys and chaos that don't need a real DB. **Extend this first.**
- **integration** (`tests/integration`, gated by `TELOS_TEST_DSN`) — real Postgres/Redis: the
  store/import/migration/idempotency layer that hermetic tests cannot see.
- **e2e** (`tests/e2e`, `-tags e2e`) — the whole Docker stack, real telnet, player-visible output.
  Reserve for what truly needs the real stack (real seed, real cross-shard wiring, real timing).
- **smoke** (`tests/smoke/smoke.sh`) — "does the stack even come up" + seed exits 0 + connect/look,
  run twice on the same volume.

Status legend: **DONE** = a black-box test pins it · **THIN** = partial/indirect coverage ·
**GAP** = no black-box coverage (unit-only or none). Priority: **P0** ship-blocker class ·
**P1** high-value · **P2** opportunistic.

---

## Area 1 — Regression-proofing (bugs that have already bitten us)

These are the highest-confidence-per-test rows: the failure already happened once.

| # | Behavior / bug | Best tier | Status | Priority |
| --- | --- | --- | --- | --- |
| 1 | **COW prototype corruption** — kill a mob → repop → re-kill; both kills clean, repop alive/full-HP/standing (not a posDead zombie) | in-process (world) + e2e | **DONE (Wave 1)** — `internal/world/cow_repop_cycle_test.go` (`TestCOWKillRepopRekillCycle`) drives the real death→corpse→repop-reset→re-kill cycle; controlled-break verified | P0 |
| 2 | **lookRoom render gap (98b69a6)** — `look` shows every entity (mob + ground item + another player), not just players | unit + in-process + e2e | **DONE** — unit `internal/world/look_render_test.go`; in-process `internal/gate/look_render_journey_test.go` (Wave 1, mob+item+player through the gate, controlled-break verified); e2e `combat_death_test.go` (goblin + corpse) | P0 |
| 3 | **Persistence round-trip** — connect, change state in-session (move), disconnect, reconnect, state intact | in-process | **DONE (Wave 1)** — `internal/gate/reconnect_roundtrip_test.go` (`TestReconnectRoundTripPreservesMovedRoom`: move → quit-flush → reconnect → moved room, live) | P0 |
| 4 | **Single-session takeover** — a second login for a connected character; newest connection wins, old goes mute (no duplicate body) | in-process | **DONE (Wave 1)** — `TestSecondLoginTakesOverSession` (pins today's contract; see contract note below) | P1 |
| 5 | **Seed / `deletePack` idempotency** — import a pack twice into real Postgres, no duplicate-key | integration + smoke | **DONE** — `tests/integration/store_pack_test.go::TestImportPackIdempotent`; `smoke-twice` (re-seed a populated volume) | P0 |
| 6 | **Reconnect input-seq mute fence** — a returning persisted player's first input is not dropped as a stale replay | in-process | **DONE** — `TestReconnectResetsInputSeqFence` | P1 |
| 7 | **Quit-flush-after-move race** — the logout flush reliably records the walked-to room | in-process | **DONE** — `TestQuitFlushReliableAfterMove` | P1 |
| 8 | **Cross-shard reconnect "mid-transfer" rejection** — relog after a handoff lands in the destination, not rejected | in-process + smoke | **DONE** — `TestCrossShardHandoffPersistsAndReconnects`; smoke cross-shard reconnect journey | P0 |
| 9 | **Dockerfile / compose / lint-action config break** (the day-long red CI) | smoke + CI | **THIN** — `smoke`/`smoke-twice` exercise the image build + compose; the lint-action *config* surface is only proven by the `lint` CI job, not locally. A `make verify` that mirrors the lint-action version would close it | P1 |
| 10 | **Combat death sequence** — kill → corpse renders → loot recoverable | e2e | **DONE** — `combat_death_test.go::TestCombatDeathSequence` | P1 |

## Area 2 — Distributed correctness (the seams between services)

| Behavior | Best tier | Status | Priority |
| --- | --- | --- | --- |
| Cross-shard walk, no visible seam, no lost input | in-process + e2e | **DONE** — `TestGateCrossShardHandoff`; e2e journey | P0 |
| Shard drop while a player is connected (the headline chaos) | in-process | **DONE** — `TestShardDropWhileConnected` (pins today's contract: socket closes, no auto-reconnect) | P0 |
| Shard-drop blast radius is one connection | in-process | **DONE** — `TestShardDropDoesNotHangOtherPlayers` | P1 |
| **Handoff interrupted mid-move** (dest shard unreachable mid-transfer) — player not lost/duplicated | in-process | **GAP** — `dropShard` exists; no test drops shard B *during* the handoff window. The "ownership conflict" hardening (zone.go) is unit-pinned only | P0 |
| **Shard restart with epoch resume** — directory epoch monotonicity holds; reconnecting player resumes | in-process / integration | **THIN** — `internal/world/resume_test.go` (unit); no black-box restart-and-resume journey | P1 |
| **Both world servers race to register** (the docker double-registration) — must not wedge | integration / smoke | **GAP** — debugged by hand, never pinned. Needs a real-Redis directory race test | P1 |
| **Backpressure / slow client** — non-blocking drop-on-full holds; the zone goroutine never stalls | in-process | **GAP** — `session.send`'s drop-on-full is unit-adjacent at best; no test wedges a reader and asserts the zone keeps pulsing | P1 |
| **Directory lease loss / expiry** — a zone's lease lapses and another shard claims it | integration | **GAP** — Redis-lease semantics untested black-box | P2 |
| Content hot-reload over NATS (`(kind,ref)` reload) | integration | **GAP** — `internal/contentbus` + reload is unit-only; no real-NATS reload journey | P2 |

## Area 3 — Phase 7 Lua sandbox (the curated escape hatch)

The sandbox's *safety* properties are exactly the cross-cutting, adversarial surface unit tests
under-serve. Strong unit coverage exists (`internal/world/lua*_test.go`); the black-box gaps:

| Behavior | Best tier | Status | Priority |
| --- | --- | --- | --- |
| Budget / circuit-breaker: a runaway script is killed, the zone survives | unit | **THIN** — `luabreaker_test.go`, `luaharm_test.go` (unit); no journey proving a runaway script in a LIVE zone doesn't stall other players | P1 |
| Whole-zone panic recovery — a script panic doesn't crash the shard | in-process | **GAP** — the panic-recovery resilience is not pinned as "a player in the zone keeps playing through a script panic" | P0 |
| Hot-reload a script live (edit → re-fires) without dropping players | in-process / e2e | **GAP** — `luareload_test.go` is unit; no live-reload journey | P1 |
| Sandbox escape attempts (os/io/ffi denied) stay denied | unit | **DONE-ish** — `luaharm_lint_test.go` + harm tests; keep unit, harden as the handle API grows | P1 |
| Room script fires on entry; scripted mob greets (Phase 7 "Done when") | in-process / e2e | **GAP** — the Phase 7 milestone journey has no black-box test | P1 |
| Pack defines/fires/handles a custom (engine-unknown) event (Phase 7 "Done when") | in-process | **GAP** | P1 |

## Area 4 — Onboarding journeys (the ROADMAP "Done when" lines as acceptance tests)

| Milestone (Phase "Done when") | Best tier | Status | Priority |
| --- | --- | --- | --- |
| P1: connect → look → north → look → say echoes | in-process + e2e | **DONE** — covered by every harness login + the e2e journey | P1 |
| P2: cross-shard walk, no seam, no lost input | in-process + e2e | **DONE** | P0 |
| P3: `get`/`wield`/`put`/`wear` + the right `act()` to bystanders | in-process | **GAP** — `internal/world/container_test.go` is unit; no black-box journey where bystander B sees A's `act()` line for get/wear | P1 |
| P4: character + world state survive a restart | in-process + e2e | **THIN** — reconnect-to-saved-room is DONE; a true *shard restart* (process down → up → resume) black-box journey is missing | P0 |
| P4: content idempotency (boot empty, seed, re-seed, intact) | integration + smoke | **DONE** | P0 |
| P5: data-defined `fireball` — casts, costs mana, typed damage, applies affect | in-process / e2e | **GAP** — `internal/world/ability_test.go` is unit; no black-box cast-fireball-at-a-mob journey | P1 |
| P6: full fight pipeline + fireball save-halves-AoE + rage-on-hit + kill+loot | e2e | **THIN** — death+loot is DONE; the AoE-save and OnHit-rage-bar legs are unit-only | P1 |
| P6: a check resolves; an event handler fires | unit | **THIN** — `check_test.go` / `event_test.go` (unit); no black-box | P2 |
| First-time login UX (name prompt, fresh spawn) | in-process + e2e | **DONE** — every harness/e2e login exercises it | P2 |

---

## Open contract questions surfaced by the chaos/regression tests

These tests pin **today's** behavior with a documented contract note; the assertion updates when
the owning engineer decides the intended contract:

- **Shard drop (`TestShardDropWhileConnected`)** — today the player's socket simply closes (no
  "the world hiccuped" notice, no auto-reconnect). Decide with the edge-engineer /
  distributed-systems-architect: notice-then-close, or directory-retry-and-failover?
- **Single-session takeover (`TestSecondLoginTakesOverSession`)** — today the displaced first
  connection is left silently **mute** (its `s.out` is swapped to the new socket) rather than getting
  a clean "logged in elsewhere" disconnect + socket close. Decide with the edge / persistence
  engineers: should the old connection be explicitly dropped with a message?
- **Second-login first-input drop** — the takeover re-binds the session preserving its input-seq
  fence, so the new connection's *first* input can be dropped as a stale replay (it needs a couple
  of inputs to get past the fence). A UX wart, not yet a correctness bug — flagged for the edge-engineer.

## Wave plan (burn-down order)

1. **Wave 1 — regression-proofing** (this change): rows 1–4 above (COW cycle, look-render journey,
   persistence round-trip, single-session takeover). DONE.
2. **Wave 2 — distributed correctness**: handoff-interrupted-mid-move, shard-restart-with-epoch-resume,
   double-registration race, backpressure/slow-client.
3. **Wave 3 — Phase 7 sandbox**: whole-zone panic recovery journey, live hot-reload, runaway-budget
   doesn't stall the zone, the Phase 7 "Done when" milestone journeys.
4. **Wave 4 — onboarding journeys**: get/wield/wear `act()` journey, fireball cast journey, the
   AoE-save + rage-on-hit legs of the Phase 6 milestone, true shard-restart persistence.
