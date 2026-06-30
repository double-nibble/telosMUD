# Roadmap

Strategy: stand up the real distributed topology around a trivial world early, prove
scale-out, then deepen the engine *inside* the working system. Each phase ends with a
demoable, testable milestone. Phases are grouped into tracks; within the engine track,
content-driven systems land only after the content pipeline (Phase 4) exists.

⭐ = highest-risk integration work — get these right early; everything rides on them.

---

## Track A — Spine (prove the topology)

### Phase 0 — Foundations
Monorepo + `go.work`; protobuf toolchain (`buf`); CI (build/test/lint/vet); `docker-compose`
with Postgres + Redis + NATS; config (env+yaml), `slog` logging, OpenTelemetry wiring.
**Done when:** `make up` brings up deps; `make test` is green on an empty engine.

### Phase 1 — Vertical slice skeleton ⭐
The whole pipe end-to-end with the minimum world. ([PROTOCOL.md](PROTOCOL.md))
- `telos-gate`: telnet accept, basic option negotiation, line I/O.
- `telos-world`: one shard, one hardcoded zone, two rooms; `look`, `say`, `move`.
- `transport`: gate ↔ world gRPC bidi `Play` stream; route `ClientFrame`/`ServerFrame`.
- `directory`: real interface, single-shard impl.

**Done when:** telnet in → see a room → `north` → see the next room → `say hi` echoes.

### Phase 2 — Multi-shard + handoff ⭐
The scale-out proof. ([PROTOCOL.md](PROTOCOL.md) §2–3)
- Two world shards; `zone→shard` directory in Redis.
- Two-phase cross-shard handoff (Prepare/Commit + epoch; gate re-dial; input replay).

**Done when:** a player walks from a zone on shard A into a zone on shard B with no visible
seam, and no input is lost across the handoff.

## Track B — Engine

### Phase 3 — Mudlib core
Turn the toy world into a real object model. ([MUDLIB.md](MUDLIB.md))
- Entity/component model (ECS-lite, flyweight + COW), uniform containment.
- Command parser with abbreviation + Diku targeting (`2.sword`, `all.coin`).
- `act()` perspective messaging, heartbeat scheduler, containers/inventory.

**Done when:** you can `get`, `wield`, `put`, and `wear` items, and others see the right
act() messages.

### Phase 4 — Persistence & content pipeline
The "everything is content" backbone — needed before any content-driven system.
([PERSISTENCE.md](PERSISTENCE.md), [PRINCIPLES.md](PRINCIPLES.md))
- Per-type definition tables + `state` JSONB; migrations; `pack` namespacing.
- Content loader (boot-load definitions into shards) and `(kind,ref)` hot-reload over NATS.
- Save strategy: durability ladder (memory → Redis checkpoint → Postgres), `state_version`.
- Zone resets/repop.

**Done when:** the bare engine boots empty, loads a content pack, and a character + world
state survive a restart.

### Phase 5 — Attributes, resources, affects & ability framework
The generic substrate + the effect-op vocabulary. ([ABILITIES.md](ABILITIES.md))
- Content-defined attributes/resources/damage-types/flags; modifier stack + derivation.
- `Affected` runtime (durations, stacking, ticks); tag-based CC.
- Ability lifecycle (declarative `on_resolve`), effect ops, automatic PvP/hostility gate.

**Done when:** a data-defined `fireball` casts, costs mana, deals typed damage, and applies a
content-defined affect — all without engine code changes.

### Phase 6 — Combat (+ the check primitive, the event bus, AoE & room affects) ✅
**Status: complete (slices 6.1–6.5).** Round-based resolution on top of the substrate — and the phase
that builds the load-bearing primitives combat is assembled *from*. ([COMBAT.md](COMBAT.md),
[PHASE6-PLAN.md](PHASE6-PLAN.md), [GAME-SYSTEMS-GAP-ANALYSIS.md](GAME-SYSTEMS-GAP-ANALYSIS.md))

Foundational primitives (built *with/before* the fight loop — to-hit and saves *are* these):
- **The check/save/contested primitive** [G2] — a `check` flow op beside `if`/`chance`: a
  content-named dice expression (keep-high/low for advantage, `dF`, pools) classified into an
  *ordered list of outcome bands* (binary 5e = the 2-band case; PbtA 3-tier, BRP degrees all fit).
  Roll visibility is config (hidden default, opt-in `show`). Invokable from exits/objects/affect-
  ticks too — a climb check needn't wait for combat.
- **The event-bus origin** [G3] — engine events (`OnHit`/`OnDamageTaken`/`OnKill`/
  `OnAbilityResolved`/`OnCheck`/`OnLeaveRoom`) fire to content op-lists. The universal glue:
  rage/combo builders, XP-on-kill, procs, and the reaction checkpoints all hang off it (declarative
  handlers here; Lua handlers in Phase 7). The first realization of the
  [universal-hookability pillar](PRINCIPLES.md#pillar-every-action-and-event-is-hookable) — the
  taxonomy ships *partial* (several kinds reserved-but-unlit, no builder-defined events yet); Phase 7
  completes it.
- **AoE / area targeting** [G12] — loop the *built* `dealDamage` harm-gate per target (the room, or
  the room + adjacent rooms) with a per-target save.
- **Room-scoped affects** [G13] — affects attached to the room entity, ticking over its occupants
  (wall/web/darkness/silence-field/lair actions).

Combat resolution on those primitives:
- `PULSE_VIOLENCE`, attacks/round, the avoidance ladder + soak pipeline; to-hit is a check vs AC.
- **Cooldown completion + persistence** [G8] — per-ability cooldown map, a step-3 "still cooling
  down" gate, serialized into `state` (logout doesn't refresh cooldowns); the GCD is a shared-tag
  lag affect.
- **Named interruptible checkpoints** [G9] in the swing/cast pipeline that fire events — easy
  reactions (opportunity attack on `OnLeaveRoom`) are declarative; result-altering ones
  (Counterspell/Shield) are Lua (Phase 7).
- Conditional/formula resource regen [G4] + a `rest`/`recover` event [G5]; `floor`/`ceil`/`round`/
  `mod` formula heads [G1]; gear modifiers into the attr mod-stack [G14].
- Skills-as-commands with lag/cooldowns; threat/assist; **uniform, cancellable death** (6.5) — the
  depletion→death seam lives in the shared `dealDamage` funnel so *any* damage (swing/spell/AoE/DoT/OA)
  kills, and an `on_depleted` hook can cancel death (a death-ward: hp→1 + rooted) — the reference
  before-checkpoint for the hookability pillar.

**Done when:** you fight a mob through the full pipeline (to-hit check → miss/dodge/parry/block →
soak), a fireball's save halves its damage across everyone in the room, a rage bar builds on hit via
an `OnHit` handler, you kill the mob and loot its corpse — all from content, no engine changes.

### Phase 7 — Lua scripting ✅
The curated escape hatch + sandbox — and the home of the complex ~20% the declarative op-list can't
express. ([LUA.md](LUA.md))
- `gopher-lua`, one VM per zone, curated handle API, strict budget + circuit breaker.
- Triggers, ability `on_resolve` in Lua, affect hooks, hot reload; `self.state` persistence.
- Lua **event handlers** on the Phase 6 bus; **result-altering reactions** [G9] (Counterspell/Shield
  reaching into an in-flight ability), **concentration** [G11], and the 5e **multiclass spell-slot
  table** [G7] — the documented escape-hatch cases.
- **Builder-defined events + taxonomy completion** — the
  [universal-hookability pillar](PRINCIPLES.md#pillar-every-action-and-event-is-hookable) made
  concrete on the bus:
  - A **content-namespaced custom-event lane**: builders *fire* and *subscribe to* their own named
    events (a sailing system's `OnShipDock`), not just the engine's enumerated kinds — today's closed
    `eventKinds` validation map grows a `pack:event` lane (still depth/width-budgeted and gate-funneled
    like an engine event; no privileged status).
  - **Light the reserved engine kinds** whose owners exist by now — `OnApplyAffect`/`OnAffectTick`/
    `OnAffectExpire` and an `OnEnter` movement hook — so "a missing hook is an engine bug" holds. The
    cross-phase kinds get lit by their owning phase (`OnRest` with regen [G5], `OnLevelUp` with
    progression Phase 11, `OnLogin` with auth Phase 14).

**Done when:** a room script fires on entry and a scripted mob greets you — edited live — and a pack
defines, fires, and handles an event the engine has never heard of.

Phase 6 (the event bus) + Phase 7 (Lua) are the prerequisites for the progression phase (Phase 11).

## Track C — World & clients

### Phase 8 — Comms over NATS ✅
- Channels (`gossip`, `newbie`), tells, `who`, presence — all cross-shard via NATS.
- JetStream for offline tells/mail.

**Done when:** two players on different shards chat and see each other in `who`.

### Phase 9 — GMCP ✅
Rich-client data. ([GMCP.md](GMCP.md)) (`Room.Info` can be pulled forward to Phase 3.)
- Negotiation (option 201), `Core.Supports` filtering, MCCP2, NAWS.
- `Char.Vitals/Stats/Status`, `Room.Info`, `Char.Items.*`, `Comm.Channel.Text`, `Mud.*`.

**Done when:** Mudlet shows a live vitals gauge and a minimap that updates as you walk.

### Phase 10 — Orchestration (directors, scopes, event bus) ✅
Supra-zone state and cross-zone consequences. ([WORLD-EVENTS.md](WORLD-EVENTS.md))
> Done. Dynamic-placement core landed (claim-from-pool + the coordinator's decision engine); the live
> rebalance-drain executor + runtime zone-add are documented follow-ups (FOLLOW-UPS.md §6).
- `telos-director` tier with leader election; region/world state (single-writer).
- **Cross-zone** scoped event bus: `transient` (NATS core) + `durable` (JetStream, idempotent,
  ordered) — extends the Phase 6 *in-zone, synchronous* bus to region/world scopes across shards.
- Remote effect commands into zones; the Lua `world.*` / `region:*` / `signal_*` API.
- **Dynamic zone placement** (the director hosts the zone coordinator): world servers
  *claim* zones from a pool instead of declaring them, with balancing, standbys, and
  failover/rebalance. Builds on Phase 4 (crash-failover rehydrates from the durability
  ladder). Design: [PLACEMENT.md](PLACEMENT.md). Replaces static `TELOS_ZONES`.

**Done when:** a boss death in one zone ripples a region-wide change across zones on different
shards, and survives a director restart.

## Track D — Progression & economy

### Phase 11 — Character progression & chargen ✅
> Done (11.1–11.5 + capstone). The grant/track/bundle machinery + all four advancement modes landed; the
> interactive **chargen front end is deferred to Phase 14** (account/login), which the bundles feed.
The largest content area the gap analysis surfaced ([G6]); needs the event bus (Phase 6) + Lua
(Phase 7), which is why it lands here — though it depends on *nothing* in Track C and may be pulled
forward. ([GAME-SYSTEMS-GAP-ANALYSIS.md](GAME-SYSTEMS-GAP-ANALYSIS.md) §5)
- **N independent advancement tracks** (`track_defs`) — character level, guild/class levels, use-
  based skills — each with content-defined XP sources, thresholds, and per-level grants. `level` is
  an ordinary attribute, not an engine concept.
- **Grant ops** — `modify_attribute_base`, `grant_ability`/`grant_track`/`grant_resource`/flags.
- **Content bundles** — `class_def`/`race_def`/`background_def`/`feat_def`/`talent_def`: pure content
  that grants resources/attrs/abilities/flags and defines tracks (multiclass, "join a guild at 5").
- **All four advancement modes** as "which event feeds the track": XP-threshold auto-level (Diku),
  train-at-a-trainer, point-buy per level, and use-based (LP/Discworld — `OnSkillUse` → chance-to-
  improve). Plus chargen (the creation-time grant flow; the account/login flow is Phase 14).

**Done when:** a character is created from a class+race bundle, gains XP on kills (auto-leveling one
track), trains a skill through use on another, and the build survives a restart — all content.

### Phase 12 — Loot & scheduled spawns ✅
> Done (12.1–12.4 + capstone). Loot resolver + pity + per-instance quality + director-owned weekly
> spawns. Deferred: a worn affix's stat effect (the gear-modifier seam), an `on_roll` Lua hatch, a
> normalized `affix_defs` table, per-mob xp-value cap (see FOLLOW-UPS.md).
([LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md))
- Loot resolver on death: roll kinds, rarity tiers, personal loot, pity counters.
- Item quality/affix rolls into instance deltas (coarse v1; deep affixes deferred).
- Director-owned durable scheduled spawns (weekly boss; wall-clock, restart-safe).

**Done when:** a weekly boss spawns on schedule and drops personal loot with a working pity
timer.

### Phase 13 — Crafting & economy ✅
> Done (13.1–13.5 + capstone). Binding/transfer gate, stackable materials, professions (= a bundle + a
> track + a membership set, no new def table), salvage/disenchant (tier-bound components, owner may
> deconstruct bound gear), `recipe_defs` (station = a room flag) + the §9 material loop. Deferred: generic
> `disenchant <item>`/`craft <recipe>` verbs, profession-cap content-config, augment affix depth
> (FOLLOW-UPS.md).
([CRAFTING.md](CRAFTING.md))
- Rarity/binding (BoP rules, tier-dependent component binding) + the transfer/bind gate.
- Professions, recipes, stackable items, deconstruction (salvage yields = weighted rolls).
- `consume_item`/`produce_item`/`augment_item` ops; crafting stations.

**Done when:** you disenchant a bound epic into tradeable mats and craft a new item at a
station.

## Track E — Services & scale

### Phase 14 — Auth & website
([ACCOUNT.md](ACCOUNT.md)) (replaces the stub login used since Phase 1.)
- `telos-account`: OAuth (Google/Discord/GitHub), accounts, and the chargen *flow* (the progression-
  track grants/bundles it drives are Phase 11).
- Link-code + passphrase + SSH pubkey; TLS/SSH transports; signed session assertions;
  single-session lock.

**Done when:** create an account on the web, get a link code, `connect` over TLS/SSH.

### Phase 15 — Terminal-native OAuth (login rework)
([PHASE15-PLAN.md](PHASE15-PLAN.md)) Reworks Phase 14's front-end: the website + passphrase + SSH logins
are replaced by a single **terminal-native OAuth device flow** — no passwords, auth externalized.
- `connect` → a one-click link → the browser does OAuth (brokered, PKCE) → the telnet session is authed.
- Prompt-driven character select + chargen (the content-driven chargen *engine* from 14.8 is reused).
- OAuth-only: passphrase + SSH pubkey auth removed; plain telnet + TLS `telnets://` are the only transports.

**Done when:** `connect` over TLS → click the link → OAuth in the browser → create a character via prompts →
play → reconnect (survives restart). No password/key path exists in prod.

### Phase 16 — Hardening & scale
- Bot-swarm load tester (synthetic telnet + GMCP); tick-lag, occupancy, NATS-lag metrics.
- Backpressure on slow clients; graceful shard drain for rolling redeploys; instanced zones.

**Done when:** N thousand synthetic players sustain target tick rate; a shard can be drained
and redeployed with zero dropped connections.

---

## Starting point
Build **Phase 0**, then the **Phase 1–2 spine** before any engine depth — they carry the
riskiest integration (the gate↔world stream and the cross-shard handoff), and every later
phase assumes them. After that the engine track (3–7) is mostly linear; tracks C–E layer on
top once the content pipeline (Phase 4) is in place.
