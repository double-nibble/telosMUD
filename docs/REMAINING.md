# Remaining work — the live TODO list

Everything here is **not yet done**. It is the consolidated, de-duplicated backlog after the roadmap
(Phases 0–16) shipped and the launch-hardening burn-down (mail/handoff/corpse security, who-cache, drain
semaphore, weighted placement, runtime-zone scope registration) landed. Completed work — the phase plans and
every resolved follow-up — lives in [COMPLETED.md](COMPLETED.md).

Items tagged **[LARGE]** are really their own project (a subsystem / a design-heavy feature), not a bounded
hardening fix — call them out for dedicated planning rather than a quick slice.

---

## 1. Security

- **Cross-shard handoff snapshot AUTHENTICATION (MEDIUM).** `Handoff.Prepare` is unauthenticated (token =
  `sha256(character/epoch)[:16]`, no secret). A reachable inter-shard port + a character name + a small epoch
  lets a forged `Prepare` carry an arbitrary `state_json`; the pack-set audit only rejects *unknown*
  prototypes, so a forged carry can INJECT any *known* prototype (item dupe / econ break). The size caps +
  pack-set audit harden availability, not integrity. Fix: sign the snapshot (Ed25519, reuse the account
  assertion key seam) or inter-shard mTLS, verified at Prepare. `internal/world/handoff_server.go`.
- **Corpse owner keyed by character NAME, not PID (low, latent).** Safe while names are unique + immutable;
  if renaming / delete-recreate ever lands, a stale 60s window keyed by a freed name could match a new claim.
  Prefer a PersistID key if the PID is reliably set at kill time. `internal/world/death.go`.
- **Mail retention sweep + dead-letter reap (smaller).** The inbox cap bounds growth, but a read-mail
  retention sweep (evict-oldest-READ so a full inbox doesn't wedge on spam) and reaping the directory-error
  dead-letter rows are still open. `internal/store/mail.go`.
- **Web auth F4 — `__Host-` prefix on the broker flow cookie under TLS.** The only surviving bit of the 14.7
  audit after Phase 15 deleted the website (F2/F8 moot): give the OAuth `flowCookie` the `__Host-` prefix
  (Secure + Path=/ + no Domain) so it can't be planted over http / from a sibling subdomain.
  `internal/web/session.go`.
- **Mid-session hear-access staleness (low, bounded).** The gate's channel hear-set isn't re-published on a
  mid-session hear-ACCESS change (an affect crossing a channel's `min_attr` floor), so a player keeps hearing
  a restricted channel until their next toggle/handoff/relog. Only matters once a `min_attr`-gated channel
  ships. Fix: call `publishCommsConfig` from the affect-apply/expire hook. `internal/world/commsstate.go`.
- **`config.<player>` comms subject under future NATS authz (note, no code).** When subject-level NATS authz
  lands, put `telos.comms.config.*` under world-publish-only alongside `chan`/`tell`.
- **Durable `characters.state` byte cap.** The handoff carry now has `maxCarryStateBytes`; add a matching cap
  on the durable state write for symmetry. `internal/world/character.go`.

## 2. Scale / performance

- **Load/locality-aware placement — the pipeline (PARTIAL).** The PLANNER is load-aware (`placement.PlanWeighted`
  balances by per-zone weight, tested). Remaining: the occupancy SIGNAL pipeline (world → director) that
  supplies real weights, wiring the plan to DRIVE the drain executor (`BeginDrain`), a weight-proportional
  `RebalanceThreshold`, locality-aware colocation, and rebalance cooldowns. `internal/placement`, director.
- **Per-session `who` cooldown (smaller).** The ~1s roster cache is in; a per-session `who` cooldown would
  further blunt a single spammer.

## 3. Orchestration

- **[LARGE] Content-defined (Lua) director script (Phase 10.4).** The director's `SignalHandler` is a Go func;
  the production `cmd/telos-director` wires a NIL handler. A real deployment needs director logic as CONTENT —
  a sandboxed Lua VM in the director (the same model as a zone) reacting to signal-up + scheduling on the
  director tick. A whole subsystem.
- **Durable DOWN state broadcast + snapshot-on-join (Phase 10.4).** The director's state broadcast DOWN is
  transient; a zone that was down when a flag flipped misses it until the next set, and has no initial
  snapshot on join. Add a snapshot fetch (read region_state/world_state at zone boot, or a director "sync"
  reply) and/or a durable down tier.
- **Director-owned + serialized drain target selection (16.4b).** `BeginDrain` takes an injectable
  `TargetChooser`; production self-selects a peer. The DS review wants the DIRECTOR (Phase 10.6 leader) to own
  selection + SERIALIZE simultaneous drains (avoid two shards draining onto one target past its one-core
  ceiling, and split-brain during a fleet rollout). Wire a director-driven chooser; keep the decentralized one
  as the standalone/dev fallback.

## 4. Content / itemization

- **[LARGE] Worn-affix stat effect + content-defined wear slots (the itemization pass, Phase 12.3).** Equip is
  a stub: a worn item confers no bonus (no affect hook), and the wearable slot set is an engine-fixed enum.
  Wire the gear-modifier seam (a rolled item's affixes register as a `modSource` on wear, unregister on
  remove) and make the slot vocabulary content-defined (a `wear_slots` table).
- **The declarative `heal` effect-op ignores dice.** A `heal` op takes a flat amount; wire a dice-expression
  form (`2d8+WIS`) through it (and by symmetry any restorative op). The dice evaluator already exists.
- **Generic object-targeted salvage/craft verbs (13.4/13.5).** Each salvage/craft is one verb bound to a
  FIXED source proto / recipe ref. A real client wants `disenchant <item>` (object-targeted, item-TAG gated)
  and `craft <recipe>` (recipe chosen by argument).
- **Recipe skill-gate uses the level ATTR, not the track (13.5).** `RecipeDTO.Skill` names the skill-level
  attribute; it does not resolve the track's `level_attr`. Brittle if they diverge.
- **Profession cap: content-config + kind split (13.3, D2).** `craftProfessionCap` is a uniform constant (2);
  make it content-configurable and only applied to *crafting* professions (gathering/utility unlimited) via a
  profession "kind".
- **`on_roll(ctx)` Lua loot hatch (12.1).** The loot resolver is fully declarative; add the Lua escape hatch
  for conditional drops the declarative form can't express.
- **Normalized `affix_defs` table (12.3).** The affix pool is inline in each loot entry's `quality` spec; a
  shared `affix_defs` content table (named affixes by ref) de-duplicates pools and enables richer legendaries.
- **Demo spawn/death handler content (12.4).** The director broadcasts `spawn.boss` DOWN; ship demo
  `on_world("spawn.boss")` + boss-death `signal_world("boss.died")` content to close the live boss-loot loop
  end to end.
- **Per-mob `xp_value` / kill-magnitude cap (11.3/12).** `death.go` fires OnKill with `mag` = the victim's raw
  max-hp (builder-influenceable — a high-max-hp mob is an XP/loot farm). Read a content `xp_value` or
  cap/normalize the magnitude before it feeds XP or loot.
- **Shared-def hot reload (7.7 scope).** `reload.go buildPrototype` handles only Room/Item/Mob; a `(kind,ref)`
  invalidation for a SHARED def (ability/affect/formula/`pvp_allowed` policy) is skipped and `z.defs` is
  boot-immutable — no live edit path to a pvp policy / formula. When a slice swaps `z.defs` at runtime, hook
  that seam and re-run the pvp permissive→restrictive end-to-end check.

## 5. Player commands / HUD

- **vitals enable/disable + live on-change vitals.** A player-toggleable on-CHANGE emitter hooked at
  `setResourceCurrent` (where every vital change funnels), driving both the text prompt and GMCP Char.Vitals
  — so a combat round's HP drain updates a plain client's prompt and a rich client's gauge live (subsumes the
  9.2 combat-tick-HUD follow-up). A `vitals enable`/`disable` verb stores the preference.
- **[LARGE] Builder-defined `help` system.** A browsable `help` / `help <topic>` command backed by a
  `help_defs` content table (topic ref, title, body, category, "see also"), auto-including the registered
  command set. Ties to the docs project.
- **Inventory shows equipment; can't drop equipped; `keep`/`unkeep`.** (1) `inventory` folds in the worn
  items (flagged); (2) `drop` REFUSES an equipped item (require explicit `remove` first); (3) a
  `keep`/`unkeep` per-item no-drop flag that rides the carry + durable save (important once carried items
  grant abilities).
- **Mobs/occupants in the room over GMCP.** Add `Room.Players` (+ a mob/occupant list), emitted from lookRoom
  alongside Room.Info, change-detected, routed through the canSee/nameFor chokepoint.
- **Builder-extensible GMCP hooks.** Let content/Lua emit custom GMCP (the `Mud.*` namespace) via a sandboxed
  `gmcp.send(player, pkg, table)` handle, routed through the outbound support filter + `validGMCPPackage`
  guard, with a namespace allowlist so content can't spoof `Char.*`/`Core.*`.
- **GMCP Char.Items incremental deltas + `Char.Items.Contents`.** Char.Items.List is a full list re-sent on
  change; add `Char.Items.Add/Remove/Update` deltas + a container-contents payload + live room-item updates.
- **GMCP Comm.Channel raw text + `Comm.Channel.List/Players`.** Carry the raw message body (not just the
  rendered line) as a Message field so a client can route to a per-channel tab; emit the channel list/players.
- **GMCP Char.Stats gauge/stat flags + Char.Vitals gauge filter.** Emit only content-flagged resources
  (`gauge`/`hud` bool) and attributes (`stat` bool) so internal pools don't leak into the HUD.
- **GMCP Char.Status target visibility.** `charStatusJSON` emits the opponent's short bypassing the
  act/canSee filter; route it through `nameFor`/canSee once invis/disguise content lands.
- **"Comms unavailable" player notice.** When the comms bus is wholly down, a player sees no channels/tells
  and no notice; expose a `Bus.Available()` probe and emit a one-line notice after login.
- **Channel HEAR vs SPEAK access split.** `channelDef.canHear` delegates to the same predicate as `canSpeak`;
  split them for "announce" channels (anyone hears, only admins speak).

## 6. [LARGE] Builder / wizard trust tier

A privilege layer above player — its own project (much like documentation).

- **See-all visibility (holylight).** A builder always sees an `invisible`/hidden/dark/wizinvis entity — the
  elevated end of the `canSee`/`nameFor` chokepoint the visibility flags will introduce (the
  `phase5-visibility` TODO in `commands.go lookRoom`). This also finally provides the **`who` visibility
  filter** (hidden players filtered at the render boundary).
- **Object inspection (`stat`/vnum).** A builder examining a thing sees instance + prototype identity +
  internal state a player never does (an inspect/`stat`-style command).
- **Runtime-tweakable per-builder toggles.** Show/hide my own dice rolls, holylight on/off, wizinvis level,
  verbose debug echoes — flipped live, scoped to that session.

## 7. Observability / tests

- **Bus deliver-lag metric wiring (16.1/16.2).** `metrics.RecordBusLag` + `telos.bus.deliver_lag_ms` exist but
  have no call site: scoped-bus envelopes carry no publish timestamp. When the envelope next changes, stamp
  publish time and record (publish→deliver) at the deliver path.
- **Drain reclaim metrics + clean-disconnect (16.4b).** A straggler at the BeginDrain deadline is flushed +
  dropped on exit; emit OTel `drain_redirected`/`drain_reclaimed` counters (infra-fault vs client-fault) and
  send a "server restarting, reconnect" disconnect to stragglers.
- **Slow-client observability + backstop (16.3 review).** (1) Reframe the per-player "wedged" Warn off a
  windowed drop-RATE (the `consecutiveDrops` signal only catches a fully-stalled client). (2) Add a
  world-side `stream.Recv` idle deadline / max-blocked-`Send` bound so reclaim doesn't DEPEND on gate
  correctness (defends the in-trust-domain direct-shard path).
- **Reflect-walk DTO round-trip test.** Assert EVERY store DTO field round-trips (a reflect-walk over a fully
  populated synthetic pack), so a new field can't be silently dropped on the store path.
- **Comms chaos test doubles.** (1) Pin the MemJetStream park-at-`maxDeliver` divergence from real NATS
  (a "fails past maxDeliver → parks" test) + confirm prod AckWait/redelivery config. (2) An AFK-auto-reply
  best-effort-failure chaos test. (3) A subscribe-side / delivery-drop double (flakyBus only models a publish
  outage).
- **Combat reproducibility.** Production combat draws from the process-global `math/rand`
  (`internal/world/combat.go`), so a live fight isn't seedable/replayable. Thread a per-zone/per-fight seeded
  RNG through the resolver.
- **`reloadLua` chunk-cache invalidation is a substring match (perf, minor).** `reload.go` uses
  `strings.Contains(key, ref)`, over-invalidating; tighten with a keyed `ref → {chunk keys}` index if the
  chunk cache grows large.

## 8. Housekeeping / deferred features

- **`ClearPlayer` directory cleanup on logout.** Reconnect routing falls back to the home-zone shard, correct
  only while `ClearPlayer` is deferred (`cmd/telos-gate/main.go`). Revisit when it lands.
- **Cross-respawn op-list guard.** `runOps` (death seam) should skip remaining same-op-list ops on a target
  that died+respawned mid-list; build it WITH the respawn-sickness slice.
- **Multi-vital support.** `vitalResource` collapses all `vital: true` resources to the single lowest-ref one;
  generalize damage/death/respawn across vitals if/when a 2nd vital pool is authored.
- **Death-narration `mag` builder-influenceable.** (`death.go`) — see the per-mob `xp_value` cap in §4.
- **Instanced zones (party dungeons).** Multiple runtime instances of a zone on the Phase-10.6 dynamic-
  placement substrate: the director mints/reaps instances and routes a party to its own copy. A world/content
  feature for a later content phase; the placement coordinator + scoped bus are the substrate.
- **[LARGE] The 5e-SRD acceptance-test sample MUD.** The design target from
  [GAME-SYSTEMS-GAP-ANALYSIS.md](GAME-SYSTEMS-GAP-ANALYSIS.md): build a real content pack exercising the
  CC-BY 5e SRD (classes/spells/saves/rests as pure content) to PROVE the engine expresses a full game system
  with zero engine changes — the capstone that validates the "engine = mechanism, content = flavor" pillar.
  A content project, not engine work; the gap analysis stays the design reference.
- **Flaky tests.** (1) `gate.TestSessionLockTakeoverKicksDisplacedConnection` — de-flake with a
  generous/synchronized kick-message deadline (the "rebuild on OAuth login" note is moot). (2)
  `gate.TestChannelLineRendersVerbatimNoTellPrefix` — raise the per-line wait / settle the comms path under a
  loaded `-race` job.
- **Doc: top-level `state.x = …` re-runs on hot reload.** A reloaded script's non-handler body re-executes
  against the PRESERVED `self.state`, so `state.x = 0` clobbers a live value; idiomatic content guards it
  (`state.x = state.x or 0`). One-liner for `docs/PERSISTENCE.md` + the builder guide.
- **Delete merged local branches as work lands.**
