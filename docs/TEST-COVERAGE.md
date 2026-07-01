# Test coverage gap-map + the maximal-coverage program

This is the honest, unflinching map of TelosMUD's test coverage as of **HEAD (Phases 1–8
complete)** and the **prioritized wave plan** for the sustained "maximal coverage" push the
owner has asked for ("as much test coverage as imaginable… confidence that everything works";
slow tests are explicitly fine).

It supersedes the earlier four-wave **black-box** gap-map (that push is DONE — see the
"Black-box push (Waves 1–4, DONE)" section at the bottom). This refresh widens the lens from
"player-visible journeys" to **every system × every test TYPE**: deep functional/unit edges,
gated-infra integration, e2e journeys, chaos/failure-injection, property/fuzz, and stress/soak.

> **The class of bug we exist to kill** stays the same: stateful, real-dependency, multi-run,
> multi-service, failure-path bugs that hermetic single-shot unit tests structurally cannot see.
> Two have already shipped: the seed/`deletePack` idempotency bug (only failed on the *second*
> run against *real* Postgres) and the **cross-shard gear-carry** drop (`buildSnapshot` carried
> an empty state subtree, so a player walking midgaard→darkwood lost worn/carried gear, stats,
> affects, and cooldowns — fixed in `b8d764f`, pinned at the *world seam* by
> `handoff_carry_test.go` but **never as a player-visible e2e walk-with-gear journey**).

---

## Deliverable 1 — where we actually are (measured at HEAD)

### Hermetic `go test ./... -cover` (the per-commit surface)

| Package | Coverage | Read |
| --- | --- | --- |
| `internal/textsan` | **94.7%** | strong; the obvious fuzz target |
| `internal/presence` | **89.5%** | `mem` 96.7 / `redis` 89.0 (mem path; real-Redis edges gated) |
| `internal/gate` | **82.8%** | the in-process harness carries this; high for an edge |
| `internal/world` | **80.8%** | the engine; **but** the average hides thin files (below) |
| `internal/telnet` | **78.1%** | IAC/negotiation; decent |
| `internal/config` | **69.0%** | |
| `internal/directory` | **63.6%** | `redis.go` 76.5, `directory.go` **0.0** (interface/ctor only exercised via redis) |
| `internal/commbus` | **59.0%** | mem paths strong (`memjs` 91.5, `membus` 89.6); **NATS paths 12–38%** (gated) |
| `internal/contentbus` | **43.3%** | `membus` 97.4; `nats.go` **15.0**, `contentbus.go`/`publish.go` **0.0** (gated) |
| `internal/content` | **40.3%** | `loader.go` 82.8; `demo.go` 45.8; `definition.go` **0.0** |
| `internal/store` | **0.0%** | **entirely DSN-gated** — `t.Skip` without `TELOS_TEST_DSN`; the real number only shows in the integration tier |
| `internal/checkpoint` | **0.0%** | no hermetic test; exercised only via the gated store/integration tier |
| `internal/obs` | **0.0%** | observability shims, untested |
| `cmd/telos-*`, `db`, `api/gen` | **0.0%** | mains/generated; smoke + integration cover the behavior |

**The reported numbers UNDERCOUNT.** Three tiers are invisible to a bare `-cover` run and must
be accounted for separately:
- **Gated Postgres integration** (`TELOS_TEST_DSN` set): `internal/store` + `tests/integration`
  jump from 0% to real coverage; this is the only tier that exercises `checkpoint`, the CAS, the
  durability flush, and `import.go` against real Postgres. CI runs it in the `integration` job.
- **e2e** (`-tags e2e`, live gate): one journey today (`combat_death_test.go`). Not in `-cover`.
- **smoke** (`tests/smoke/smoke.sh`, `--twice`): the Docker/seed/compose surface. No Go coverage.
- **`-race -count=100`** (`make test-race`): a *different* signal than coverage — it's the only
  thing that exercises the concurrent zone-goroutine/saver/session interleavings.

### Test-type census (the structural gap)

- **641** `func Test*` · **11** `func Benchmark*` · **0** `func Fuzz*`.
- **No property-based or fuzz tests exist at all.** The four highest-value fuzz targets
  (`textsan`, the parser, the durability StateJSON round-trip, the handoff snapshot round-trip)
  have zero adversarial-input coverage.
- **No chaos harness** beyond the two in-process gate chaos tests (`shard_drop_chaos_test.go`,
  `shard_restart_test.go`). NATS-down / Redis-down / Postgres-down degradation is untested.
- **Benchmarks** exist but there is **no stress/soak tier** (many-player/many-zone, long pulse
  loops, comms fan-out under load).

### Thin files inside the well-covered packages (the deep-unit backlog)

`internal/world` averages 80.8% but the *seams and failure paths* are the thin part:

| File | ~Cov | What's unverified |
| --- | --- | --- |
| `handoff_server.go` | **42%** | the failure legs: Prepare timeout, Attach with a bad/stale token, redirect-target-unreachable, abort-and-thaw |
| `zone.go` | **43%** | freeze/thaw reaper edges, panic-recovery branches, shutdown drain |
| `components.go` | **43%** | component (de)serialization branches |
| `scripted.go` | **50%** | scripted-mob/room trigger error paths |
| `living.go` | **56%** | position transitions, resource clamps at boundaries |
| `tell.go` / `commscmds.go` | **60%** | offline tells, ignore-filter, toggle interactions |
| `luaharm.go` / `reload.go` | **67–68%** | sandbox-escape denials, reload error paths |
| `effect_op_handlers.go` | **71%** | per-op edges across the 13-op registry (below) |
| `formula.go` | **73%** | formula eval error/NaN/div-by-zero branches |
| `combat_commands.go` | **73%** | combat command error paths |

The **13 effect ops** (`deal_damage, heal, restore, modify_resource, apply_affect,
remove_affect, dispel, act, send, if, chance, check` + the `self/other/actor/victim` scope
resolvers) each have a happy-path test; the **per-op boundary/error matrix** (negative amounts,
missing target, resource over/underflow, affect-stacking-vs-refresh, nested `if/chance/check`)
is thin.

---

## Deliverable 2 — the maximal-coverage gap-map, by SYSTEM × TIER

Status legend: **DONE** = a test pins it at the right tier · **THIN** = partial/indirect ·
**GAP** = unverified. Tier abbreviations: U=unit, H=in-process harness, I=gated integration,
E=e2e, C=chaos, F=fuzz/property, S=stress/soak.

### S1 — Combat (attack/avoidance/soak/damage/death)
| Behavior | Tier | Status |
| --- | --- | --- |
| Attack roll → avoidance → soak → typed damage | U | **DONE** (`combat_test.go`, `formula_damage_test.go`) |
| Death → corpse → loot → repop (COW cycle) | U+E | **DONE** (`cow_repop_cycle_test.go`, `combat_death_test.go`) |
| Avoidance/soak pipeline EDGES (0/negative/overflow, immunity, resist/vuln) | U | **THIN** |
| Multi-attacker / threat / target-switch | U | **GAP** |
| Death-as-cancellable-checkpoint (`on_depleted` cancels: death-ward) | U | **THIN** |
| Full fight pipeline through the gate (init→swing→death→loot) | E | **THIN** |
| Combat under `-race` with concurrent attackers | S | **GAP** |

### S2 — Abilities / affects / effect-ops
| Behavior | Tier | Status |
| --- | --- | --- |
| Ability cast: cost/cooldown/target/resource gate | U | **DONE** (`ability_test.go`) |
| Each of the 13 effect-ops, happy path | U | **DONE** |
| Effect-op boundary/error matrix (per op) | U | **THIN** — the deep-unit backlog |
| Affect stacking vs refresh vs replace; expiry tick; remaining-conserved | U | **THIN** (`affect_test.go` partial) |
| Data-defined `fireball`: cast→mana→typed-dmg→affect, no engine change | H/E | **GAP** (unit-only) |
| AoE save-halves; room affects | U | **DONE** (`aoe_test.go`, `affect_room_test.go`) |

### S3 — Event bus + reactions + check primitive
| Behavior | Tier | Status |
| --- | --- | --- |
| In-zone event publish/subscribe; reserved events fire | U | **DONE** (`event_test.go`) |
| Check primitive: band classification, margin, DC, advantage | U | **DONE** (`check_test.go`) |
| Every check BAND edge (crit/fumble/degrees/PbtA-tiers/derived edges) | U | **THIN** |
| Reaction fires → handler → effect, through a running zone | H | **THIN** |
| Custom (engine-unknown) event round-trip | U | **DONE** (`luahook_test.go`) |

### S4 — Lua sandbox / scripting / hot-reload / self.state
| Behavior | Tier | Status |
| --- | --- | --- |
| Sandbox escape denied (os/io/ffi/loadstring) | U | **DONE-ish** (`luaharm*_test.go`) — harden as handle API grows |
| Budget/circuit-breaker: runaway script killed, zone survives | H | **DONE** (`lua_sandbox_journey_test.go`) |
| Whole-zone panic recovery | H | **DONE** |
| self.state survives logout/relogin ladder | H | **DONE** (`lua_state_journey_test.go`) |
| Hot-reload live script, players intact, state preserved | U/H | **DONE** (`luareload_test.go`) |
| **Sandbox holds under ARBITRARY content (no escape/no crash, fuzzed)** | F | **GAP** |

### S5 — Persistence / durability ladder / CAS
| Behavior | Tier | Status |
| --- | --- | --- |
| Save/load round-trip (in-memory store) | U | **DONE** (`store_test.go` mem) |
| Durability ladder: checkpoint(Redis)→flush(PG)→final, reason selection | U | **THIN** (`saver` logic unit-only) |
| `state_version` CAS contention, exactly one wins | I | **DONE** (`store_test.go::TestSaveCharacterConcurrentCAS`) |
| Zombie-fence / input-seq fence | H | **DONE** (`reconnect_roundtrip_test.go`) |
| **Durability ladder END-TO-END against real Redis+Postgres** | I | **THIN** — CAS is pinned; the full ckpt→flush→reload ladder isn't |
| **CAS/zombie-fence under real contention (N savers, real PG)** | I | **THIN** |
| **Arbitrary StateJSON save→load identity** | F | **GAP** |
| Checkpoint (`internal/checkpoint`) against real Redis | I | **GAP** (0% hermetic) |

### S6 — Cross-shard handoff (a real bug just shipped here: gear-carry)
| Behavior | Tier | Status |
| --- | --- | --- |
| Cross-shard walk, no seam, no lost input | H+E | **DONE** |
| Full-state CARRY at the world seam (gear/stats/affects/cooldowns) | U | **DONE** (`handoff_carry_test.go`, the regression for `b8d764f`) |
| **Cross-shard walk WITH GEAR, player-visible (the gear-carry e2e)** | H/E | **GAP** — the regression has no journey-level pin |
| Handoff interrupted, destination unreachable → thaw+restore | H | **DONE** (`handoff_state_test.go`) |
| Redirect-target-unreachable crash-failover window | C | **GAP** (documented open contract) |
| Prepare timeout / abort-and-thaw | C | **THIN** |
| Exactly-once dedup on replay (mid-transfer) | U | **DONE** (unit), **GAP** at H |
| **Handoff snapshot arbitrary-state round-trip parity** | F | **GAP** |

### S7 — Comms (channels/tells/who/presence/mail/toggles/ignore/hear-filter)
| Behavior | Tier | Status |
| --- | --- | --- |
| Channel send/recv; tells; who; presence (in-memory bus) | U+H | **DONE** (`channel_test.go`, `tell_test.go`, `comms_*_journey_test.go`) |
| Toggles / ignore / hear-filter | U+H | **DONE-ish** (`comms_toggle_journey_test.go`, `commsstate_test.go`) |
| Mail (in-memory store) | U | **DONE** (`mail_test.go`) |
| **Mail/tells/presence against REAL NATS + REAL Redis/PG** | I | **GAP** — only `mem`/`memjs` paths run hermetically |
| Offline tell → mail fallback; cross-shard tell routing | H/I | **THIN** |
| **NATS down → comms degrade gracefully** | C | **GAP** |
| **JetStream redelivery storm / dedup** | C | **GAP** — `jetstream_nats.go` 12.5% covered |
| Comms fan-out under load (N players, N channels) | S | **GAP** |

### S8 — Parser / commands
| Behavior | Tier | Status |
| --- | --- | --- |
| Verb dispatch, abbreviation, args | U | **DONE** (`parser_test.go`, `commands_test.go`) |
| Every command's error/edge path (no-target, bad-arg, posn-gate) | U | **THIN** |
| **Arbitrary bytes → no panic (parser fuzz)** | F | **GAP** |

### S9 — textsan
| Behavior | Tier | Status |
| --- | --- | --- |
| CleanLine/CleanName/CleanMarkup happy paths | U | **DONE** (94.7%) |
| **Arbitrary input → control-free + idempotent (fuzz)** | F | **GAP** |

### S10 — Content loader / hot-reload
| Behavior | Tier | Status |
| --- | --- | --- |
| Pack load/validate (in-memory) | U | **DONE** (`loader_test.go`) |
| `definition.go` validation branches | U | **GAP** (0% covered) |
| Seed idempotency (import twice, real PG) | I+smoke | **DONE** (`store_pack_test.go`, `smoke-twice`) |
| Content hot-reload over REAL NATS (`(kind,ref)`) | I/C | **GAP** |
| **Content-bus invalidation flood** | C | **GAP** |

### S11 — Gate / edge / telnet
| Behavior | Tier | Status |
| --- | --- | --- |
| Connect/look/move/say journeys | H+E | **DONE** |
| Telnet IAC negotiation | U | **DONE** (`telnet_test.go`, 78%) |
| **Slow/blocked socket → non-blocking drop-on-full holds, zone pulses** | C | **GAP** (the named backpressure gap) |
| Shard drop while connected; blast radius = 1 connection | H | **DONE** (`shard_drop_chaos_test.go`) |

### S12 — Auth-adjacent / session / reconnect
| Behavior | Tier | Status |
| --- | --- | --- |
| Single-session takeover (newest wins) | H | **DONE** (`session_test.go`, harness) |
| Reconnect to saved room; input-seq fence reset | H | **DONE** (`reconnect_roundtrip_test.go`) |
| Bad-login re-prompt / name validation | H+U | **DONE** (`onboarding_journey_test.go`, `name_test.go`) |
| Directory double-registration race (real Redis) | I | **GAP** (debugged by hand, never pinned) |
| Directory lease loss/expiry | I | **GAP** |

---

## Program status (executed)

All ten waves' high-value coverage landed. Highlights and the THREE real bugs the program caught:

- **W5 ✅** gear-carry cross-shard regression (controlled-break-verified) + nested/inventory carry; the
  effect-op core + boundary/error matrix (the uncovered restore/send/act ops; missing-arg guards; the
  negative-heal anti-weaponization clamp; modify_resource floor-at-0).
- **W6 ✅** 6 fuzz targets (textsan, parseTargetSpec, dispatch, luaCompile, StateJSON round-trip,
  formulaEval) + a scheduled **nightly** active-fuzz tier (`make fuzz`). 🐛 **FuzzTextsan found a real
  bug** in 15s: CleanLine returned ~3× MaxLineBytes on invalid-UTF-8+control input (cap applied before
  the U+FFFD-expanding strip) — fixed (cap-after-strip), security-auditor-reviewed.
- **W7 ✅** the gated real-NATS suite now RUNS in CI (a new `comms` job, NATS with `-js`). 🐛 Activating
  it caught `TestJetStreamRealOfflineThenOnline` **latently broken against real NATS** (an invalid dotted
  consumer name + a constant idempotency key the mem stand-in never validated) — fixed + rerun-robust.
- **W8 ✅** comms/tell/presence failure-injection (flakyBus/failingRoster): zone-survives-bus-failure +
  recovery, durable-tell NAK-before-cursor-advance ordering AND end-to-end recover-within-maxDeliver,
  who-degrades-to-local on a roster read error. (Shard-drop, crash-restart-restore, handoff-interrupted,
  CAS/zombie-writer were already covered.)
- **W9 ✅** the stress/soak tier (`make soak`, nightly): churn + concurrent-load under `-race`, asserting
  no wedge/panic/leak (residents + goroutines) over 100k cycles.
- **W10 ✅** the formula-eval fuzz (the thin formula.go failure surface) + the capstone kill→loot→equip
  milestone journey. The other thin-file failure paths (effect-ops, StateJSON, the comms/handoff failure
  legs) are now covered by the W5/W6/W8 work above.

🐛 **Also W6-adjacent:** the richer demo content surfaced a real persistence bug — the store dropped a
prototype's Lua through the Postgres `protoBody` JSONB (the same class as the earlier dropped `Living`).

Remaining work is incremental and tracked in docs/REMAINING.md (the comms-chaos deepenings —
MemJetStream park-at-maxDeliver vs NATS, the subscribe-side partition double, the AFK best-effort path —
plus the per-file coverage-% long tail). The 3-tier CI is in place: per-commit hermetic + gated
(Postgres `integration`, NATS `comms`), and the `nightly` workflow (active fuzz + deep soak).

## The wave plan (the program to execute — prioritized highest-confidence-per-effort first)

Each wave is a coherent committable unit. **gated** = needs `make deps`/`make up` infra and runs
only in CI's gated jobs (or `make test-integration`/`test-e2e`); **slow** = belongs in a nightly
tier, not per-commit. Reviewers named per the subagent-review rule (owning engineer + cross-cutting
expert). Default `go test ./...` MUST stay hermetic and fast — every gated/fuzz/stress test is
behind a build tag, an env guard, or a separate make target.

### Wave 5 — the gear-carry e2e + deep-unit effect-op/affect/check matrices (FAST, hermetic)
Highest confidence per effort: closes the just-shipped regression at journey level and fills the
thinnest hermetic gaps. No infra.
- **Gear-carry journey** (H, then E): a player wears+carries gear, walks cross-shard, and SEES the
  gear intact on the far side (the `b8d764f` regression as a player-visible walk). *This is the
  single highest-value missing test.*
- Effect-op boundary/error matrix: each of the 13 ops × {negative/zero amount, missing target,
  resource over/underflow, nested if/chance/check}.
- Affect stacking/refresh/replace/expiry matrix; remaining-conserved on refresh.
- Check-band edge matrix: crit/fumble/degrees/PbtA-tiers/derived-edge bands.
- Avoidance/soak pipeline edges (immunity/resist/vuln, 0/negative/overflow).
- Reviewers: combat/abilities engineer + test-engineer.
- CI: per-commit (hermetic).

### Wave 6 — property/fuzz seed corpora (FAST hermetic seeds; long fuzz in nightly)
The structural gap: 0 fuzz tests today. Each `Fuzz*` runs its seed corpus hermetically in
`go test` (fast); the long `-fuzz` runs go nightly.
- `FuzzTextsan` — arbitrary input → control-char-free + idempotent (`Clean(Clean(x))==Clean(x)`).
- `FuzzParser` — arbitrary bytes → no panic, bounded output.
- `FuzzStateJSONRoundTrip` — arbitrary StateJSON → save→load identity (durability round-trip).
- `FuzzHandoffSnapshot` — arbitrary carry state → buildSnapshot→prepare parity.
- `FuzzLuaSandbox` — arbitrary content script → no escape, no Go panic, budget-bounded.
- Infra needed: a `tests/fuzz` corpus dir + a `make fuzz` target + a nightly CI job with a
  time budget per target.
- Reviewers: per-target owner (textsan→edge, parser/handoff/lua→world, StateJSON→persistence) +
  test-engineer.
- CI: seed corpus per-commit; `-fuzz` nightly (slow).

### Wave 7 — gated-infra integration: durability ladder + comms on real backing services (gated)
The tier that let the seed bug ship. Behind `TELOS_TEST_DSN` / real-NATS-URL guards; CI gated job.
- Durability ladder end-to-end against real Redis+Postgres: checkpoint→flush→final, reason
  selection, reload-from-each-tier, the finalize-flush zombie-fence probe.
- CAS/zombie-fence under real contention (N concurrent savers, real PG) — deepen the existing CAS
  test into the full fence matrix.
- `internal/checkpoint` against real Redis (0% today).
- Comms against real NATS + real Redis/PG: mail durability, offline-tell→mail, presence, who.
- Content loader + idempotent re-import (deepen beyond the seed-twice case: partial/overlapping
  packs, `deletePack` table coverage for every def table).
- Content hot-reload over real NATS (`(kind,ref)` invalidation journey).
- Infra needed: real-NATS env guard + helper (mirror `OpenTestPool`); CI gated job already has PG,
  add NATS+Redis services.
- Reviewers: persistence engineer + comms/edge engineer + test-engineer.
- CI: gated `integration` job (extend with NATS+Redis). Slow-ish, not per-commit hermetic.

### Wave 8 — chaos / failure-injection (the owner's headline want)
Inject the failure, confirm it reproduces the bad behavior, THEN assert graceful degradation + no
data loss. Mix of in-process (H/C, hermetic) and gated-infra (real services killed mid-test).
- Shard DROP mid-session (deepen: assert the locked-in contract, not just today's behavior).
- Handoff FAILS mid-flight: destination unreachable (DONE), **Prepare timeout / abort-and-thaw**,
  **redirect-target-unreachable crash-failover window** (the open-contract gap).
- NATS down → comms degrade (send fails soft, no zone stall, recovery on reconnect).
- Redis down → presence/checkpoint degrade (the durable PG tier still saves; player not lost).
- Postgres down → mail/save degrade (checkpoint still mirrors; reconnect recovers on PG return).
- Slow/blocked socket → `session.send` drop-on-full holds, zone goroutine never stalls (named gap).
- JetStream redelivery storm → dedup holds (`jetstream_nats.go` is 12.5% covered).
- Content-bus invalidation flood → reloader debounces / doesn't wedge.
- Both world servers race to register (real-Redis directory race — the docker double-reg).
- Directory lease loss/expiry → another shard claims, no split-brain.
- Infra needed: a reusable **chaos harness** (kill/restart a backing service mid-test; a
  pauseable/blockable socket; a fault-injecting bus wrapper). Some legs hermetic (in-process,
  fault-injected fakes), some gated (real service killed).
- Reviewers: distributed-systems-architect + edge engineer + test-engineer.
- CI: hermetic legs per-commit; real-service-kill legs in the gated/nightly tier. Slow.

### Wave 9 — stress / soak (slow, nightly)
Confidence under load + the concurrency signal coverage can't give.
- `make test-race` already runs `-count=100`; add targeted high-`-count` race suites for the
  zone-goroutine/saver/session interleavings and concurrent combat.
- Many-player / many-zone harness: N players across M shards, sustained movement + handoff churn.
- Long-running pulse/affect/combat loops (advance the zone goroutine for K pulses; assert no leak,
  no drift, affects expire correctly over time).
- Comms fan-out under load (N players × N channels; assert no drop beyond the documented
  drop-on-full, no goroutine leak).
- Infra needed: a spin-up-N-shards/N-players helper (extend the gate harness); benchmark→soak
  bridges; a leak detector (goroutine count delta).
- Reviewers: distributed-systems-architect + test-engineer.
- CI: nightly only. Slow by design (owner is fine with slow).

### Wave 10 — remaining functional/milestone journeys + thin-file sweep
Cleanup wave: the deferred milestone journeys + the remaining low-coverage files.
- Black-box `fireball` cast journey (P5); AoE-save + OnHit-rage-bar legs through the gate (P6).
- Check/event-handler black-box journey (P6).
- Scripted-room-on-entry full journey (P7, beyond the unit trigger).
- Thin-file sweep: `handoff_server.go`, `zone.go`, `components.go`, `scripted.go`, `formula.go`
  error/edge branches to lift them off the floor.
- Reviewers: owning engineer per journey + test-engineer.
- CI: per-commit (hermetic) where possible; the few that need the stack go to the e2e job.

### CI structure for the program
- **Per-commit hermetic** (`go` job, today's `go test ./... -race`): all unit/H tests, all fuzz
  SEED corpora, the hermetic chaos legs. Stays fast; never needs infra.
- **Gated per-commit** (`integration` + `smoke` + `e2e` jobs, extended): the Wave-7 real-infra
  integration + the real-service-kill chaos legs. Extend the `integration` job with NATS+Redis
  services (it has PG today).
- **Nightly slow tier** (NEW scheduled workflow): `-fuzz` runs with a time budget, the Wave-9
  stress/soak suites, high-`-count` race soaks, the long chaos scenarios. Gated behind a `cron`
  schedule + `workflow_dispatch`; failures page but don't block commits.

### New test infrastructure / fixtures the program needs
1. **Chaos harness** (`tests/helpers` or `internal/.../chaos_test` support): kill/restart a backing
   service mid-test; a fault-injecting `commbus.Bus`/`contentbus` wrapper; a pauseable/blockable
   socket (the harness already has `pauseReader`/`resumeReader` — extend to a writer-block).
2. **Fuzz corpus** (`tests/fuzz` seed dirs) + `make fuzz` + the nightly job.
3. **Real-NATS test guard + helper** mirroring `OpenTestPool` (skip-without-URL, with cleanup).
4. **N-shard / N-player spin-up helper** (factor out of the gate harness for stress/soak).
5. **Goroutine-leak detector helper** for the soak tier.
6. **Richer content fixtures** — a richer demo pack is being authored in parallel; the
   gear-carry journey, the `fireball` journey, and the scripted-room journey all need it. Coordinate
   so the journeys land against the new pack rather than ad-hoc test content.

---

## Black-box push (Waves 1–4, DONE — historical record)

The earlier four-wave push closed every **P0** black-box GAP and the named P1
onboarding/sandbox/distributed journeys via the in-process gate harness + running-zone journeys.
Summary (full per-row detail is in the git history of this file at the four-wave commits):
- **Wave 1 — regression-proofing:** COW kill→repop→re-kill; look renders all room contents;
  persistence round-trip; single-session takeover.
- **Wave 2 — distributed correctness:** cross-shard input continuity; handoff interrupted
  (destination unreachable); true shard-restart persistence; `state_version` CAS contention.
- **Wave 3 — Phase 7 sandbox:** runaway script doesn't wedge the running zone; whole-zone Go-panic
  recovery; self.state through the real persistence ladder.
- **Wave 4 — onboarding journeys:** first-time onboarding; get/wield/wear `act()`; bad-login
  re-prompt; scripted-mob greet milestone.

### Open contract questions still surfaced (decide with the owning engineer, then lock in a test)
- **Shard drop** — today the socket simply closes (no notice, no auto-reconnect). notice-then-close,
  or directory-retry-and-failover? (edge / distributed-systems-architect)
- **Single-session takeover** — the displaced first connection is left silently mute, not cleanly
  dropped with a message. (edge / persistence)
- **Redirect-target-unreachable** — Prepare SUCCEEDED but the gate can't re-dial for the Redirect:
  the gate writes "The world is unreachable. Goodbye." and drops, while the directory already moved
  ownership to the unreachable destination. The crash-failover window of PLACEMENT.md §6 — NOT yet
  covered; needs the gate to retry the directory / re-resolve a healthy shard.
  (distributed-systems-architect) — scheduled in Wave 8.
