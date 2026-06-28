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
> black-box tiers below are where those regressions get caught.
>
> **UPDATE (after the four-wave coverage push):** the black-box tiers are no longer thin. The
> in-process gate harness (`internal/gate`) and running-zone journeys (`internal/world`) now pin
> every P0 black-box behavior and the named P1 onboarding/sandbox/distributed journeys — each
> controlled-break-verified where a breakable line exists. The starting grade was honest ("thin",
> 1 e2e + 2 integration on top of the unit base); the closing grade is in the per-area tables and
> the Closing Picture section at the end.

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
| Cross-shard walk, no visible seam, no lost input | in-process + e2e | **DONE** — `TestGateCrossShardHandoff`; `TestCrossShardHandoffInputContinuity` (Wave 2: ordered far-side burst lands intact + placement transfers); e2e journey | P0 |
| **Cross-shard handoff preserves state + input continuity** | in-process | **DONE (Wave 2)** — `internal/gate/handoff_state_test.go::TestCrossShardHandoffInputContinuity` (player-visible no-loss/ordered carry over the seam + directory placement). NOTE: the exactly-once dedup *mechanism* is the unit test `TestCrossShardHandoff`'s; the in-process gate timing doesn't reproduce a replay double-apply (documented in the test) | P0 |
| Shard drop while a player is connected (the headline chaos) | in-process | **DONE** — `TestShardDropWhileConnected` (pins today's contract: socket closes, no auto-reconnect) | P0 |
| Shard-drop blast radius is one connection | in-process | **DONE** — `TestShardDropDoesNotHangOtherPlayers` | P1 |
| **Handoff interrupted mid-move** (dest shard unreachable) — player not lost/duplicated | in-process | **DONE (Wave 2)** — `handoff_state_test.go::TestHandoffInterruptedDestinationUnreachable` (dest unreachable → player thawed, restored to source room, "The way is barred.", no phantom directory ownership; controlled-break verified). See contract note below | P0 |
| **True shard restart with persistence** — shard process down→up, character state survives, routes correctly | in-process / integration | **DONE (Wave 2)** — `internal/gate/shard_restart_test.go::TestShardRestartPreservesPersistedState` (drop shard, boot a FRESH shard from the same store at a new endpoint, reconnect → saved room; controlled-break verified). The *epoch-monotonicity* leg remains unit-only (`resume_test.go`) | P1 |
| **state_version CAS contention** — concurrent saves of one character; exactly one wins | integration (gated) | **DONE (Wave 2)** — `internal/store/store_test.go::TestSaveCharacterConcurrentCAS` (8 concurrent saves at the same base version → exactly one ok, rest cleanly rejected, final version=base+1; verified against real Postgres + controlled-break) | P0 |
| **Both world servers race to register** (the docker double-registration) — must not wedge | integration / smoke | **GAP** — debugged by hand, never pinned. Needs a real-Redis directory race test | P1 |
| **Backpressure / slow client** — non-blocking drop-on-full holds; the zone goroutine never stalls | in-process | **GAP** — `session.send`'s drop-on-full is unit-adjacent at best; no test wedges a reader and asserts the zone keeps pulsing | P1 |
| **Directory lease loss / expiry** — a zone's lease lapses and another shard claims it | integration | **GAP** — Redis-lease semantics untested black-box | P2 |
| Content hot-reload over NATS (`(kind,ref)` reload) | integration | **GAP** — `internal/contentbus` + reload is unit-only; no real-NATS reload journey | P2 |

## Area 3 — Phase 7 Lua sandbox (the curated escape hatch)

The sandbox's *safety* properties are exactly the cross-cutting, adversarial surface unit tests
under-serve. Strong unit coverage exists (`internal/world/lua*_test.go`); the black-box gaps:

| Behavior | Best tier | Status | Priority |
| --- | --- | --- | --- |
| Budget / circuit-breaker: a runaway script is killed, the zone survives | running-zone | **DONE (Wave 3)** — `internal/world/lua_sandbox_journey_test.go::TestRunawayLuaCommandDoesNotWedgeZone` (a `while true do end` custom command on a LIVE running zone is budget-aborted + breaker-quarantined while a SECOND player keeps playing; controlled-break confirmed the zone WEDGES without the budget). Unit layer: `luabreaker_test.go` | P1 |
| Whole-zone panic recovery — a script panic doesn't crash the shard | running-zone | **DONE (Wave 3)** — `lua_sandbox_journey_test.go::TestPanicInLuaPathRecoversAndZoneServes` (a Go-panicking builtin reached through real Lua content; the zone survives, blast radius is one command). Defense-in-depth: I could NOT construct a crashing variant (a positive finding). Core-handler panic: `TestZoneRecoversFromHandlerPanic` | P0 |
| Player self.state survives a logout/relogin through the real ladder | running-zone | **DONE (Wave 3)** — `internal/world/lua_state_journey_test.go::TestPlayerLuaStateSurvivesLogoutReloginLadder` (a Lua quest counter mutated in-session survives the async logout flush + fresh-login rehydrate on a RUNNING shard; controlled-break verified). The 7.6 marshaller itself: `luastate_test.go` | P1 |
| Hot-reload a script live (edit → re-fires) without dropping players + self.state survives | running-zone | **DONE** — `luareload_test.go::TestMobGreetingReloadsLiveStatePersists` + `TestHotReloadMobLuaFullPath` (the full bus→reloader→inbox→reloadLua path; live instance picks up the new handler, self.state preserved). Already end-to-end; not re-done in Wave 3 | P1 |
| Sandbox escape attempts (os/io/ffi denied) stay denied | unit | **DONE-ish** — `luaharm_lint_test.go` + harm tests; keep unit, harden as the handle API grows | P1 |
| Pack defines/fires/handles a custom (engine-unknown) event (Phase 7 "Done when") | unit | **DONE** — `luahook_test.go::TestCustomEventRoundTrips` + `TestCustomEventBudgetAndGate` (a `mud.fire("pack:Event")` round-trip, budget-bounded + gated). Already covered; a black-box layer would not strengthen it (Wave 3 scoping decision) | P1 |
| Room script fires on entry; scripted mob greets (Phase 7 "Done when") | in-process / e2e | **THIN** — the trigger fires (`TestTriggerGreetRemembersViaState`, unit) but no full gate→greet journey on a scripted content pack | P2 |

## Area 4 — Onboarding journeys (the ROADMAP "Done when" lines as acceptance tests)

| Milestone (Phase "Done when") | Best tier | Status | Priority |
| --- | --- | --- | --- |
| P1: connect → look → north → look → say echoes | in-process + e2e | **DONE** — covered by every harness login + the e2e journey | P1 |
| P2: cross-shard walk, no seam, no lost input | in-process + e2e | **DONE** | P0 |
| P3: `get`/`wield`/`wear` + the right `act()` to bystanders | in-process | **DONE (Wave 4)** — `internal/gate/onboarding_journey_test.go::TestItemInteractionActJourney` (Actor gets/wields/wears the demo market's sword + helmet; a bystander Watcher sees every third-person `$n gets/wields/wears $p` line; equipment + inventory reflect the change; controlled-break verified on the bystander broadcast). Unit: `container_test.go` | P1 |
| P4: character + world state survive a restart | in-process + e2e | **DONE (Wave 2)** — reconnect-to-saved-room + the true shard-restart journey (`internal/gate/shard_restart_test.go::TestShardRestartPreservesPersistedState`: process down → fresh shard from the same store at a new endpoint → reconnect into the saved room) | P0 |
| P4: content idempotency (boot empty, seed, re-seed, intact) | integration + smoke | **DONE** | P0 |
| P5: data-defined `fireball` — casts, costs mana, typed damage, applies affect | in-process / e2e | **GAP** — `internal/world/ability_test.go` is unit; no black-box cast-fireball-at-a-mob journey. DEFERRED (no wave assigned; owner: combat/abilities engineer) | P1 |
| P6: full fight pipeline + fireball save-halves-AoE + rage-on-hit + kill+loot | e2e | **THIN** — death+loot is DONE (e2e `combat_death_test.go`); the AoE-save and OnHit-rage-bar legs are unit-only. DEFERRED | P1 |
| P6: a check resolves; an event handler fires | unit | **THIN** — `check_test.go` / `event_test.go` (unit); no black-box. DEFERRED | P2 |
| First-time onboarding (connect → name prompt → spawn → look → first move) | in-process + e2e | **DONE (Wave 4)** — `onboarding_journey_test.go::TestFirstTimeOnboardingJourney` (the full new-player path, player-visible output at each step). SEAM: real chargen/auth is Phase 14 — the name IS the character today; the test notes where the class/race/stat steps slot in | P2 |
| Connect-time errors (bad login re-prompts, recovers) | in-process | **DONE (Wave 4)** — `onboarding_journey_test.go::TestBadLoginRepromptsThenSucceeds` (a leading-digit / embedded-dot name is rejected player-visibly and the gate re-prompts, then a valid name spawns; controlled-break verified). Name collision / second-login takeover: `TestSecondLoginTakesOverSession` (Wave 1) | P2 |
| Scripted-mob greet milestone (Phase 7 "Done when": a script fires on entry and a mob greets you) | in-process | **DONE (Wave 4)** — `internal/gate/scripted_greet_journey_test.go::TestScriptedMobGreetsPlayerThroughGate` (a content pack's Lua `greet` handler greets a real telnet player BY NAME through the gate; a second player gets their own personalized greeting; controlled-break verified on the greet fire). Unit: `luaentry_points_test.go` | P2 |

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
- **Handoff interrupted — destination unreachable (`TestHandoffInterruptedDestinationUnreachable`)** —
  when the source can't even reach the destination's Handoff service, the player is cleanly thawed,
  restored to the source room, and told "The way is barred." (a non-event for their location). This is
  the GOOD contract and is now pinned. NOTE the *other* interruption — destination Prepare SUCCEEDED
  but the gate then can't re-dial the destination for the Redirect — has a HARSHER observable: the gate
  writes "The world is unreachable. Goodbye." and drops the socket, while the directory already moved
  ownership to the (unreachable) destination. That window is the crash-failover case PLACEMENT.md §6
  describes and is NOT yet covered (it needs the gate to retry the directory / re-resolve a healthy
  shard). Flagged for the distributed-systems-architect; tracked as a remaining Area-2 gap.

## Wave plan (burn-down order)

1. **Wave 1 — regression-proofing** (committed): COW cycle, look-render journey, persistence
   round-trip, single-session takeover. DONE.
2. **Wave 2 — distributed correctness** (this change): cross-shard input continuity, handoff
   interrupted (destination unreachable), true shard-restart persistence, state_version CAS
   contention. DONE. Remaining Area-2 gaps deferred to a later wave: the redirect-target-unreachable
   crash-failover window, the double-registration race, backpressure/slow-client, the epoch-monotonicity
   leg of shard restart, directory-lease expiry, NATS hot-reload.
3. **Wave 3 — Phase 7 sandbox** (this change): runaway-script-doesn't-wedge-the-running-zone,
   whole-zone Go-panic recovery, player self.state survives the real logout/relogin ladder. DONE.
   Live hot-reload and the custom-event lane were found ALREADY end-to-end (luareload/luahook) and
   not re-done (scoping decision). Deferred: the full gate→scripted-mob-greet milestone journey (P2).
4. **Wave 4 — onboarding journeys** (this change, the FINAL coverage wave): the full first-time
   onboarding journey, the get/wield/wear `act()` journey, the bad-login re-prompt journey, and the
   gate→scripted-mob-greet Phase 7 milestone. DONE.

## Closing picture (after Wave 4 — the coverage push is complete)

The four-wave push closed every **P0** black-box GAP and all the named P1 onboarding/sandbox/distributed
gaps. What was DONE vs. what remains DEFERRED (with the owner/tier it's tracked under):

**DONE (black-box / journey coverage now exists):**
- Regression-proofing (Wave 1): COW kill→repop→re-kill, look renders all room contents, persistence
  round-trip, single-session takeover.
- Distributed correctness (Wave 2): cross-shard input continuity, handoff interrupted (destination
  unreachable), true shard-restart persistence, `state_version` CAS contention.
- Phase 7 sandbox (Wave 3): runaway script doesn't wedge the running zone, whole-zone Go-panic
  recovery, player self.state through the real persistence ladder. (Live hot-reload + custom-event
  lane were already end-to-end.)
- Onboarding (Wave 4): first-time onboarding, get/wield/wear `act()`, bad-login re-prompt,
  scripted-mob greet milestone.

**DEFERRED (still GAP/THIN — tracked for a future wave or the owning engineer):**
- *Distributed (Area 2):* redirect-target-unreachable crash-failover window; double-registration race;
  backpressure/slow-client; the epoch-monotonicity leg of shard restart; directory-lease expiry; NATS
  content hot-reload. → distributed-systems-architect.
- *Combat/abilities (Area 4):* a black-box `fireball` cast journey (P5); the AoE-save + OnHit-rage-bar
  legs of the Phase 6 milestone (P6); a black-box check/event-handler journey (P6). → combat/abilities
  engineer. These need content/ability scaffolding above the unit tier; the unit coverage is solid, so
  these are P1/P2, not ship-blockers.
- *Onboarding (Area 4):* real chargen/auth is Phase 14 — the first-time journey covers everything that
  exists today and extends when chargen lands.
