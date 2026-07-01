# Remaining work — the live TODO list

Everything here is **not yet done**. It is the consolidated, de-duplicated backlog after the roadmap
(Phases 0–16) shipped and the launch-hardening burn-down (mail/handoff/corpse security, who-cache, drain
semaphore, weighted placement, runtime-zone scope registration) landed. Completed work — the phase plans and
every resolved follow-up — lives in [COMPLETED.md](COMPLETED.md).

Items tagged **[LARGE]** are really their own project (a subsystem / a design-heavy feature), not a bounded
hardening fix — call them out for dedicated planning rather than a quick slice.

---

## 1. Security

*Burned down (see COMPLETED.md → "Launch-hardening burn-down round 2"): handoff snapshot Ed25519
authentication, corpse-owner PersistID keying, mail evict-oldest-READ retention sweep, the `__Host-` broker
cookie prefix, mid-session hear-access republish, and the durable `characters.state` byte cap.*

- **Mail dead-letter reap (background job).** The evict-oldest-READ sweep landed; still open is a PERIODIC
  reaper for undeliverable/orphaned mail (rows to a name that never logs in) — a maintenance job needing a
  scheduler tick (director-owned, like the weekly spawn scheduler), not a per-send store fix.
  `internal/store/mail.go`.
- **`config.<player>` comms subject under future NATS authz (note, no code).** When subject-level NATS authz
  lands, put `telos.comms.config.*` under world-publish-only alongside `chan`/`tell`. Nothing to build until
  NATS subject authz ships.

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

- **[LARGE] Multi-file demo packs — the multi-system acceptance sprint.** A sprint of its own; the real
  capstone that proves the "engine = mechanism, content = flavor" pillar holds across DIFFERENT game systems.
  Two parts:
  - **Directory-tree pack assembly (loader).** Today a pack is a single `internal/content/packs/demo.yaml`.
    Support a pack as a TREE of small files the loader walks + assembles into one logical pack — e.g.
    `content/packs/<pack>/common/{attributes,weapons,armor}.yaml`,
    `content/packs/<pack>/areas/<area>/{rooms,enemies,bosses,vendors}/*.yaml`, and
    `content/packs/<pack>/areas/<area>/scripts/*.lua`. The assembly must feed the SAME import/`LoadedContent`
    path (both the embedded-pack loader and the DB seed import). Payoff: a builder edits ONE area/boss/vendor
    file without touching the rest.
  - **The three demo packs (content authoring).** (1) Split the current `demo.yaml` into
    `content/packs/demo/basic/…` (a simple Diku/ROM-flavored pack) as the reference tree. (2) `content/packs/5eSRD`
    — the CC-BY D&D 5e SRD as pure content (Vancian slots, six abilities → modifiers + proficiency,
    advantage, class/subclass/background, short/long rest). (3) `content/packs/WoWSRD` — the WoW-d20 skeleton
    (rage/energy/focus/combo resources, talent trees, cooldown pacing, threat, a raid/loot economy). Each pack
    must run with ZERO engine changes for flavor — that is the acceptance test. (The gap analysis also names
    Pathfinder as a third tabletop capstone; a 4th pack if wanted.) Absorbs the former "5e-SRD sample MUD"
    item. · *content/persistence*

- **[LARGE] Worn-affix stat effect + content-defined wear slots (the itemization pass, Phase 12.3).** Equip is
  a stub: a worn item confers no bonus (no affect hook), and the wearable slot set is an engine-fixed enum.
  Wire the gear-modifier seam (a rolled item's affixes register as a `modSource` on wear, unregister on
  remove) and make the slot vocabulary content-defined (a `wear_slots` table).
*Burned down (see COMPLETED.md → "Launch-hardening burn-down round 2"): the `heal`/restorative dice+bonus
form, the formula NaN/±Inf fail-closed guard (found in the heal-dice review), the OnKill kill-magnitude cap
(`xp_value` + fallback cap — also §8 death-mag), the reserved-affect-event-kinds reconciliation (those hooks
already fire; only OnRest is dark, pending a rest mechanic), the recipe skill-gate `track` resolution, and
the content-configurable profession cap + uncapped kind.*

- **Dedupe the op amount roll (optional, low priority).** opDealDamage and opHeal now duplicate the
  `amount + dice(diceNum/diceCount) + bonus` block; extract a `rollOpAmount(c, op)` helper so they can't drift
  (abilities-engineer advisory during the heal-dice review). · *abilities/world*
- **Generic object-targeted salvage/craft verbs (13.4/13.5).** Each salvage/craft is one verb bound to a
  FIXED source proto / recipe ref. A real client wants `disenchant <item>` (object-targeted, item-TAG gated)
  and `craft <recipe>` (recipe chosen by argument).
- **Content-lint: `learn_profession.profession` must name a `kind: profession` bundle (found in the cap review).**
  The uncapped/capped resolution keys off `bundleDefs().get(profession_ref)` (ref == bundle ref by convention);
  a content-lint rule asserting every `learn_profession.profession` names a matching `kind: profession` bundle
  ref would machine-check that convention instead of relying on authorial discipline. Low severity (the engine
  already defaults to capped on a miss — conservative). · *progression/content*
- **`on_roll(ctx)` Lua loot hatch (12.1).** The loot resolver is fully declarative; add the Lua escape hatch
  for conditional drops the declarative form can't express.
- **Normalized `affix_defs` table (12.3).** The affix pool is inline in each loot entry's `quality` spec; a
  shared `affix_defs` content table (named affixes by ref) de-duplicates pools and enables richer legendaries.
- **Demo spawn/death handler content (12.4).** The director broadcasts `spawn.boss` DOWN; ship demo
  `on_world("spawn.boss")` + boss-death `signal_world("boss.died")` content to close the live boss-loot loop
  end to end. (When touching the demo, also add an `uncapped: true` gathering profession so the gated
  store-round-trip `DeepEqual` covers the new `uncapped` bundle flag — currently blind, only a `true` can drop.)
- **`OnRest` event kind is dark (needs a rest mechanic).** `OnApplyAffect`/`OnAffectExpire`/`OnAffectTick`
  now fire (reconciled); `OnRest` is defined but has no fire site because there is no rest command / rest-regen
  mechanic to fire it from. Lighting it requires BUILDING rest (a `rest`/`sit` verb + resting regen) first, not
  just wiring a hook. · *abilities/world*
- **Shared-def hot reload (7.7 scope).** `reload.go buildPrototype` handles only Room/Item/Mob; a `(kind,ref)`
  invalidation for a SHARED def (ability/affect/formula/`pvp_allowed` policy) is skipped and `z.defs` is
  boot-immutable — no live edit path to a pvp policy / formula. When a slice swaps `z.defs` at runtime, hook
  that seam and re-run the pvp permissive→restrictive end-to-end check.

## 5. Player commands / HUD

- **`score` command — the character stat sheet.** A player types `score` (classic `sc`) and sees a framed
  summary of their character: name + title/epithet, the vital pools (`HP: 150(150)`, `SP: 205(205)` — current
  (max) per resource), gold/currency, XP as `have/next-level`, carry weight (as a % of capacity), the
  progression levels (overall `Level`, plus any per-track level like `Guild Level`), and the attribute block
  (STR/DEX/CON/INT/WIS/CHR — grid-formatted). Everything shown is already engine state (resources, attributes,
  progression tracks, carry weight, currency); this is a RENDER of it. Design question to settle: the LAYOUT
  and the SET of rows/attributes are flavor, so the template should be CONTENT-DEFINED (a content-authored
  score layout referencing resource/attribute/track refs, rendered by the engine) rather than a hardcoded
  Go sheet — otherwise a 5e vs. WoW pack couldn't show its own stat names/order. Plain-telnet render path;
  GMCP clients already get the same data structured via `Char.Stats`/`Char.Vitals`. · *mudlib/edge*

- **ANSI color (16-color palette).** Colorize output so it reads like a classic MUD — enemies red, exits
  cyan, items/gold, damage, channel names, etc. The world emits SEMANTIC color TOKENS (a category, not raw
  ESC — e.g. an `{enemy}`/`{exit}` markup class, content-nameable per the mechanism/flavor pillar); the GATE
  renders tokens → ANSI SGR downstream of the control-strip. NOTE the existing seam: `internal/telnet/telnet.go`
  `Write`/`sanitizeOutput` STRIPS ESC today, with a documented comment that a future ANSI renderer must
  produce the SGR bytes DOWNSTREAM of (not through) the control-strip, or the strip must whitelist well-formed
  SGR — so the world never ships raw ESC (injection-safe). Include a per-player `color on/off` toggle (the
  conventional MUD control; a client that can't do ANSI gets plain text), and a small default token→color map
  (enemy/exit/item/damage/heal/channel/system). GMCP clients already get structured data; this is the plain-
  telnet render path. · *edge/mudlib*

- **Coalesce identical items in listings (`A torch. (5)`).** In the room-contents render (`lookRoom`),
  inventory (`inventory`/`i`), and container listings (`look <corpse>`, get-from), group identical items onto
  ONE line with a `(N)` count instead of repeating the line N times. This is DISPLAY-time grouping of discrete
  entities — distinct from the Phase-13.2 `Stack` component (true stackable materials, which already carry a
  count). Grouping key: same rendered short name AND no distinguishing per-instance state (don't merge a
  bound/quality-affixed/differently-worn item with a plain one — group by prototype + equal delta, or fall
  back to identical short). Keep single items uncounted (`A torch.`). Mirror the count in GMCP
  `Char.Items.List` (a count field) so rich clients group too, and keep it consistent with how the `Stack`
  materials render their count. · *edge/mudlib*

- **Presentation capitalization.** Capitalize for readability: (1) at the START of a line/sentence, an item
  short or a message beginning with a lowercase article renders capitalized — `a torch` → `A torch.` on a
  room-contents/inventory line, `a goblin arrives.` → `A goblin arrives.` — while the SAME short stays
  lowercase mid-sentence (`You get a torch.`); (2) character/proper names always render capitalized. The
  natural seam is the render layer: `act()` (`internal/world/act.go`) for perspective messages (the classic
  Diku initial-cap-the-leading-token rule) and `lookRoom` / item-listing lines. Content authors still write
  shorts lowercase (`a torch`); the engine capitalizes at presentation, not in the data. · *edge/mudlib*

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
- **UTF-8 rendering tests (multibyte-clean edge path).** UTF-8 should already pass through end-to-end, but
  there's no explicit coverage. Add tests asserting multibyte content (`Hello, 世界`, emoji, combining marks,
  RTL) survives the full edge render path intact — through `Write`/`sanitizeOutput` (the ESC control-strip must
  NOT corrupt or split multibyte sequences), `act()` perspective messaging, `lookRoom`/item shorts, GMCP JSON
  payloads, and mail/tell bodies. Also pin behavior at boundaries: never split a rune across a chunk/flush,
  and confirm any width-based framing (e.g. the future `score` sheet / column layout) measures DISPLAY width,
  not byte length. If a gap turns up, fix it; the deliverable is the regression tests either way. · *edge/tests*
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
- **Instanced zones (party dungeons).** Multiple runtime instances of a zone on the Phase-10.6 dynamic-
  placement substrate: the director mints/reaps instances and routes a party to its own copy. A world/content
  feature for a later content phase; the placement coordinator + scoped bus are the substrate.
- **The 5e-SRD / multi-system acceptance packs** are now folded into the "[LARGE] Multi-file demo packs"
  item in §4 (basic + 5eSRD + WoWSRD). The game-systems gap analysis (archived in COMPLETED.md) stays the
  design reference for them.
*Burned down (see COMPLETED.md → "Launch-hardening burn-down round 2"): the two flaky gate tests
(`TestSessionLockTakeoverKicksDisplacedConnection`, `TestChannelLineRendersVerbatimNoTellPrefix`), the
orphaned `account_auth`/`ssh_keys` drop migration (00017), and the stale `internal/web/oauth.go` header.*

- **Stale Phase-14 docstrings sweep (comment-only).** Beyond oauth.go (done), several headers still describe
  removed passphrase/SSH-login functionality: `internal/account/service.go:4`, `internal/store/account.go:17`,
  `cmd/telos-account/main.go:3`, `internal/gate/gate.go:3/182/226`, `internal/config/config.go:25`. Correct
  them to the OAuth-only state (found during the 00017 review). Low-risk cleanup; touches account/gate/config.
- **Builder-guide note: top-level `state.x = …` re-runs on hot reload.** A reloaded script's non-handler body
  re-executes against the PRESERVED `self.state`, so `state.x = 0` clobbers a live value; idiomatic content
  guards it (`state.x = state.x or 0`). The PERSISTENCE.md note is added; this remains for the builder guide
  (the docs/wiki project).
- **Delete merged local branches as work lands.**
