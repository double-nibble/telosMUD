# Remaining work — dependency-ordered backlog

Everything here is **not yet done**. It is the consolidated, de-duplicated backlog after the roadmap
(Phases 0–16) and three burn-down rounds shipped. Completed work lives in
[COMPLETED.md](COMPLETED.md).

**How this list is organized.** The old §1–§8 *domain* grouping is replaced by dependency-ordered **tracks**:
items that touch the SAME code seam are clustered so we edit a chokepoint once, and hard "do-X-before-Y"
constraints are called out so we don't build against a stub and then rework it. Each item keeps its owning
component tag (`· *mudlib/edge*` …). **[LARGE]** = a subsystem / design-heavy feature that wants its own
planning pass, not a quick slice. Tracks are listed in a sensible *start* order, but tracks with no shared
seam are independent and can run in parallel — the constraints that matter are the per-item **Prereq** notes.

---

## Shared seams (the touch-the-same-code map)

- **Render path** — `internal/world/act.go`, `lookRoom`, item-listing, `internal/telnet` `Write`/`sanitizeOutput`,
  `internal/textwidth`, `internal/consoleui`, `internal/colormarkup` → *Track 1* SHIPPED (ANSI color, capitalization,
  coalescing, UTF-8/RTL, and the whole display-templating subsystem — rounds 3–4); only small follow-ups remain
  (more display surfaces on the shipped mechanism, GMCP token-strip, the coalescing `(N)`→GMCP count).
- **`canSee`/`nameFor` visibility chokepoint** → *Track 2* (holylight/visibility flags, `who`-filter, GMCP
  `Char.Status` visibility, `Room.Players`).
- **`Resolve`/parser + content aliases** → *Track 3* (alias system, `craft <name>`, salvaging object-targeting).
- **Equip/wear + the gear-modifier `modSource`** → *Track 4* (worn-affix + `wear_slots`, inventory-by-slot,
  `affix_defs`, salvaging).
- **`setResourceCurrent`/regen + `OnRest`** → *Track 5* (rest mechanic, live on-change vitals).
- **`BeginDrain` + director + scoped-bus envelope** → *Track 6* (drain ownership, placement pipeline, drain
  metrics, durable DOWN, bus-lag, mail reaper, Lua director).
- **GMCP outbound path** → *Track 7* (Char.Items deltas, Comm.Channel, Char.Stats flags, builder gmcp.send) —
  note the cross-links into Tracks 1/2.
- **Store DTO round-trip + content loader** → *Track 0* (reflect-walk net) and *Track 8* (directory-tree loader
  → demo packs).
- **`reload.go`** → *Track 9* (shared-def hot reload, `reloadLua` substring, determinism RNG).

## Hard ordering constraints (violate → you rework)

1. **Visibility flags** (Track 2 holylight) **before** GMCP `Char.Status` visibility + `Room.Players` routing.
2. **`wear_slots` content-defined** (Track 4) **before** inventory-renders-worn-items-by-slot (Track 4).
3. **Alias/`Resolve` resolver** (Track 3) **before** unified `craft <name>` + salvaging object-targeting.
4. **Director-owned drain ownership** (Track 6) **before / with** wiring placement to DRIVE `BeginDrain`.
5. **Rest mechanic** (Track 5) **before** firing `OnRest` and **before** the 5eSRD pack's short/long rest (Track 8).
6. **Directory-tree loader** (Track 8) **before** the 5eSRD / WoWSRD packs.
7. ~~Display-width helper → score framing~~ — SATISFIED (round 4): `internal/consoleui` frames all sheets with
   the color-token-aware `internal/textwidth` measure; any future word-wrap must reuse it.

---

## Suggested ordering for the next burn-down (round 5)

The Track-1 render subsystem (color, capitalization, coalescing, UTF-8/RTL, the whole display-templating stack)
is DONE. What's left is a handful of small/medium items scattered across tracks, plus a cluster of [LARGE]s. The
user's operating model: knock out the small ones each pass, plan the LARGEs, repeat until only LARGEs remain.
Proposed order:

- **A. Small-items sweep (quick, de-risking) — start here.** Track 0: close the `Commands`/`PvpLua`/`Formulas`
  Postgres gap (now a KNOWN pattern — identical to the `display_defs` persistence just shipped) + the scope-state
  CAS re-run-safety fix; the GMCP `{{token}}`-strip guard rail (Track 1, security-adjacent, before content is
  broadly colored); the stale Phase-14 docstrings sweep; the `on_roll` unknown-ref diagnostic (Track 4); the
  `drop`-refuses-equipped + `keep`/`unkeep` (Track 4, the independent parts); Track 10 channel HEAR/SPEAK split +
  "comms unavailable" notice; the per-session `who` cooldown.
- **B. Medium items.** Track 5 rest mechanic → fire `OnRest` + the live on-change vitals (same `setResourceCurrent`
  funnel); the discrete Track 6 items (mail dead-letter reaper, drain reclaim metrics + bus-lag stamp, durable
  DOWN snapshot-on-join); the Track 7 GMCP enrichments (Char.Items deltas — carries the coalescing `(N)` count,
  Comm.Channel raw text, Char.Stats/Vitals gauge flags).
- **C. The [LARGE]s — plan each, then build, in dependency order.**
  1. **Content install & reload** (Track 8, design SETTLED) — highest leverage: it unblocks the demo-content
     project (the acceptance test) and spans director + loader + reload. Split `demo.yaml` into the tree first.
  2. **Visibility / builder-wizard trust tier** (Track 2) — foundation for GMCP `Char.Status`/`Room.Players` +
     the `who` filter + the trusted full-screen-ANSI mode + the remaining display surfaces' visibility.
  3. **Cross-content alias/targeting** (Track 3) — unblocks the Track 4 gear cluster.
  4. **Track 4 gear cluster** (after Track 3): worn-affix + `wear_slots`, `affix_defs`, salvaging.
  5. **Lua director script** (Track 6), **builder `help` system** (standalone), and finally the **demo packs**
     (Track 8: 5eSRD/WoWSRD — the zero-engine-change acceptance test, after content-install + the features they
     showcase).

---

## Track 0 — Regression nets (do first; they de-risk the store + render churn below)

*Reflect-walk store net + display-width helper + UTF-8/bidi/grapheme render tests SHIPPED — see COMPLETED.md
"Burn-down round 3". Still-open follow-ups they surfaced:*

- **Commands / PvpLua / Formulas have no Postgres import/load path (found by the reflect net).** A YAML pack's
  custom Lua verbs (`Commands`), PvP policy (`PvpLua`), and ruleset `Formulas` are NOT persisted through
  `ImportPack`/`Load` — no INSERT/DELETE/SELECT in `internal/store/import.go`/`content.go`. A DB-SEEDED pack
  silently loses them (they survive only via the embedded-YAML load path). Decide whether that's intentional
  (embedded-only) or a gap to close with a store path (a def table or pack_meta columns) + reflect-net coverage.
  **`DisplayDefs` was the same shape and was CLOSED (round 4): a `display_defs` table (migration 00018) + import/
  load, now in the reflect net — `Commands` INSERT/DELETE/SELECT is the exact same pattern if we close it.**
  NOTE: the content-install [LARGE] moves the source of truth to an EXTERNAL versioned store (git/S3), so this PG
  gap may become moot for those three — decide alongside that design. · *persistence*
- **UTF-8 coverage gaps NOT yet closed by the render-path tests (found in review).** The say-echo + output
  sanitize cover the `$t` verbatim + strip seams; still UNVERIFIED for multibyte: (1) **GMCP JSON payloads**
  — highest value, they BYPASS `sanitizeOutput` entirely (Room.Info names, Comm.Channel bodies) and have only
  JSON-escaping as a backstop; (2) `act()` `$n`/`$N` NAME substitution through `nameFor` + `CleanName`/
  `stripNonGraphic`; (3) `lookRoom` / item-short render path; (4) mail/tell `capRunes` — a rune-count cap can
  cut a grapheme cluster mid-sequence. · *edge/tests*
- **Bidi-override spoofing at the trust boundary (security follow-up).** Legitimate Arabic+English needs only
  IMPLICIT bidi (no control chars), but explicit bidi OVERRIDE/embedding controls (RLO U+202E, LRO, isolates)
  enable "Trojan Source"-style display spoofing of names/text. The edge preserves them today (they're Cf, not
  Cc, so `sanitizeOutput` keeps them; `textwidth` measures them 0). Decide whether to strip/neutralize the
  override subset at ingress — a security-auditor call, weighed against not breaking legitimate bidi. · *security/edge*

## Track 1 — Plain-telnet render path  ·  seam: `act()` / `lookRoom` / item-listing / `sanitizeOutput`

*Why clustered:* all four edit the same render functions and the ESC/width seam; sequencing avoids re-touching.

*ANSI color (the `{{TOKEN}}` SGR layer + `color on/off` + exits/demo auto-color) and presentation
initial-cap SHIPPED — see COMPLETED.md "Burn-down round 3". Still-open color follow-ups:*

- **ANSI color — deferred follow-ups.** (1) **PERSIST** the `color on/off` pref across sessions via the ACCOUNT
  — color stays an EDGE concern; the persistence path must NOT route through the input-seq stream (would
  reintroduce the world/edge coupling slice 2 deliberately avoids). Session-scoped today. (2) **GMCP token-strip
  — priority guard rail now that the demo advertises `{{tokens}}` in content:** a token in a room NAME (→ GMCP
  `Room.Info`) or a `channel_def` format (→ `Comm.Channel.Text` raw body) ships literal `{{tokens}}` to a rich
  client (JSON-escaped so injection-safe, just cosmetic). Strip via a `renderColor(s,false)` helper on the GMCP
  text fields, or a content-lint rejecting tokens in names/formats, BEFORE builders are broadly told they can
  color any content. (3) ~~width-aware framing~~ — DONE: `internal/consoleui` measures visible width
  (color-token + display-width aware) for all sheet framing; any FUTURE word-wrap must reuse that. (4) optional
  **semantic aliases** (`{{ENEMY}}` → direct tokens, only if a pack wants global re-theming). See
  [[content-alias-and-salvage-direction]]. · *edge/mudlib*
*The display-templating subsystem SHIPPED (COMPLETED.md "Burn-down round 4"): `internal/consoleui` layout engine
+ `internal/colormarkup` shared tokenizer, RTL bidi isolation, the `ui.sheet()` Lua sandbox binding (DoS-hardened),
the `display_defs` content table + Postgres persistence (migration 00018), and the `score`/`sc`, `inventory`,
`equipment` surfaces (Lua render body per surface, built-in fallback). Template language = a sandboxed Lua render
func + the `ui` toolkit. Still-open in this area:*

- **Remaining display surfaces (small each, on the shipped mechanism).** Wire more commands to
  `renderDisplaySheet` as they fit: (a) **`who`** — renders a COLLECTION fetched ASYNC off the zone goroutine, so
  it needs a list binding + the Lua render threaded back onto the zone goroutine (single-writer), not the
  render(self) shape; (b) **`look`/room** — exits + occupants + coalesced items = a real handle-API expansion;
  (c) a per-slot **equipment LABEL accessor** on the handle (`self:equipment()` is a bare list today) so an
  equipment template can show `<worn on head>`. guild/profession surfaces have NO command yet and need new
  content → they belong to the demo-content project (Track 8), not here. · *mudlib/edge*
- **Display-templating cleanup follow-ups (minor).** (a) `consoleui.truncateVisible` drops a cell's closing
  `{{RESET}}` mid-truncation → color can bleed until the edge's end-of-frame reset (cosmetic, bounded; the isolate
  pair it DOES close since it owns them); (b) the bidi isolate/LRM controls should be gated on the edge's
  negotiated CHARSET (stripped for non-UTF-8 clients, like ANSI is gated on color) — an EDGE follow-up, since
  consoleui can't see the client encoding. See [[content-display-templates-direction]]. · *edge/mudlib*
- **Coalescing → GMCP `Char.Items.List` count.** Mirror the `A torch (5)` `(N)` count into the GMCP item list —
  coordinate with Track 7 (do it with/after the Char.Items delta restructure so the count field isn't re-placed). · *edge*

## Track 2 — Visibility & the `canSee`/`nameFor` chokepoint  ·  [LARGE] foundation + its consumers

*Why clustered:* the GMCP visibility-consumers explicitly wait on the visibility system — build the chokepoint first.

- **[LARGE] Builder / wizard trust tier** — a privilege layer above player, its own project (much like docs).
  - **See-all visibility (holylight) + the visibility flags.** A builder always sees an
    `invisible`/hidden/dark/wizinvis entity — the elevated end of the `canSee`/`nameFor` chokepoint the
    visibility flags introduce (`phase5-visibility` TODO in `commands.go lookRoom`). This is the FOUNDATION the
    consumers below need; it also delivers the **`who` visibility filter** (hidden players filtered at the render
    boundary). · *mudlib/edge*
  - **Object inspection (`stat`/vnum).** A builder examining a thing sees instance + prototype identity +
    internal state a player never does.
  - **Runtime-tweakable per-builder toggles.** Show/hide own dice rolls, holylight on/off, wizinvis level,
    verbose debug echoes — flipped live, session-scoped.
  - **Trusted full-screen ANSI / `screen.play(frames)` (tier 2 of the color design).** The ANSI color item
    (Track 1) is SGR-only by design — safe for anyone, but it cannot move the cursor or redraw the screen. A
    `telnet towel.blinkenlights.nl`-style animation (cursor positioning, erase, scroll regions) is a distinct
    TRUSTED output mode: raw ANSI written to the socket BYPASSING `sanitizeOutput`, invocable only by trusted
    code (a login splash / `clear` / HUD = engine-owned; a builder-authored intro animation = a sandboxed
    `screen.*` capability gated behind THIS trust tier). Safety is by PROVENANCE, not an allowlist — player text
    never reaches it, so cursor/screen control stays out of untrusted hands. Orthogonal to the Track-1 token
    layer (no pipeline change). · *edge/scripting*
- **GMCP `Char.Status` target visibility.** `charStatusJSON` emits the opponent's short bypassing act/canSee;
  route it through `nameFor`/canSee. **Prereq:** the visibility flags above (constraint 1). · *edge*
- **Mobs/occupants in the room over GMCP (`Room.Players`).** Add `Room.Players` (+ a mob/occupant list) from
  `lookRoom` alongside Room.Info, change-detected, **routed through the canSee/nameFor chokepoint**. **Prereq:**
  visibility flags (else you route through a stub and rework). · *edge/mudlib*

## Track 3 — Naming, targeting & discovery  ·  seam: `Resolve`/parser + content aliases

*Why first in its chain:* unify resolution before the consumers (Track 4 salvaging, `craft <name>`) so they
don't each hand-roll keyword parsing that the alias system then supersedes.

- **[LARGE] Cross-content alias / keyword targeting system (+ discovery listings).** A builder declares SHORT
  aliases on ANY content object; the parser resolves a player's partial phrase to it — one mechanism for items,
  mobs, recipes, abilities. E.g. `weapon:heavy_wooden_sword` → "heavy sword"/"wood sword"/"sword";
  `enemy:big_scary_skeletal_guard` → "guard"/"skeleton"/"big skeleton"; recipes → `craft <name>`. Pillar is
  DISCOVERABILITY: pairs with listing commands ("what can I craft?") that print exactly the names a player then
  types. Touches the `Resolve` layer for ALL content types + the parser + per-content authoring → a design pass
  before slices (recipes-first, then generalize to items/mobs). Subsumes the `craft <recipe>` need. See
  [[content-alias-and-salvage-direction]]. · *mudlib/content/edge*

## Track 4 — Itemization & gear  ·  seam: equip/wear, gear-modifier `modSource`, affix data

*Why this order:* `wear_slots` content-defined + the `modSource`-on-wear seam are the foundation the render and
salvaging items build on; `affix_defs` and worn-affix both touch the affix data model.

- **[LARGE] Worn-affix stat effect + content-defined wear slots (Phase 12.3).** Equip is a stub: a worn item
  confers no bonus (no affect hook) and the wearable slot set is an engine-fixed enum. Wire the gear-modifier
  seam (a rolled item's affixes register as a `modSource` on wear, unregister on remove) and make the slot
  vocabulary content-defined (a `wear_slots` table). **Foundation for:** inventory-by-slot render (below) +
  making gear matter (Track 8 packs lean on it). · *mudlib/progression*
- **Inventory shows equipment; can't drop equipped; `keep`/`unkeep`.** (1) `inventory` folds in worn items
  (flagged, by slot); (2) `drop` REFUSES an equipped item (require explicit `remove`); (3) a `keep`/`unkeep`
  per-item no-drop flag riding the carry + durable save. **Prereq:** `wear_slots` content-defined (constraint 2)
  for the by-slot render; the drop-refusal + keep/unkeep are independent and can land earlier. · *mudlib/edge*
- **[LARGE] Normalized `affix_defs` table (Phase 12.3).** A shared `affix_defs` content table (named affixes by
  ref) de-duplicates the pools inline in each loot entry's `quality` spec and enables richer legendaries. A
  first-class def table on the scale of `recipe_defs`/`bundle_defs`: migration (`00019_affix_defs.sql` — 00018 is
  now `display_defs`; the `ref/pack/JSONB body` pattern), an `AffixDefDTO`, loader + `LoadedContent.Affixes`, a per-shard registry,
  build-time resolution (a `quality` affix entry gains a `ref` alternative to inline `attr/min/max`, resolved in
  `buildLootTableDef`), the store round-trip, tests, demo usage. **Design fork:** first-class table (edit-once →
  propagates on reload; the normalized choice) vs. loader-time EXPANSION (resolve refs into inline pools before
  storage — small/no-migration but BAKES values). The first-class table matches
  [[content-alias-and-salvage-direction]]. **Rides the Track 0 reflect-walk net.** · *progression/persistence/content*
- **[LARGE] MMO-style salvaging subsystem (disenchant, 13.4).** `disenchant <item>` object-targeted (resolve a
  held item by keyword) + item-TAG gated; the YIELD like modern-MMO salvage: (1) DERIVES from rarity TIER +
  item LEVEL via a tier+level→table map; (2) a per-item `salvage_table` OVERRIDE; (3) a per-item un-salvageable
  BLOCK flag; (4) a salvaging SKILL requirement scaled by item level/rarity; (5) an OVER-SKILL BONUS. Today
  `salvage_item(item, table)` takes a FIXED source proto + fixed table, no tag gate/derivation/skill. Slices:
  object-target + tag gate + per-item override/block first, then derived tables + skill gate + over-skill bonus.
  **Prereq:** Track 3 resolver for clean object-targeting; the per-item `salvage_table` field rides the Track 0
  net. See [[content-alias-and-salvage-direction]]. · *progression/content*
- **`on_roll` unknown-ref diagnostic (low priority).** An `on_roll` body returning a ref that doesn't resolve to
  a prototype is a silent `deliverLoot` no-op (`spawn == nil`) — consistent with the declarative path, but the
  ref is opaque Lua so a typo vanishes. A debug log (or content-lint) on the `spawn(ref) == nil` path. Independent
  of the rest of this track. · *progression/observability*

## Track 5 — Rest & regen  ·  seam: `setResourceCurrent`/regen + `OnRest`

- **Rest mechanic → light `OnRest`.** `OnApplyAffect`/`OnAffectExpire`/`OnAffectTick` now fire; `OnRest` is
  defined but has no fire site because there's no rest command / rest-regen. Build rest (a `rest`/`sit` verb +
  resting regen), THEN fire `OnRest`. **Prereq for:** the 5eSRD pack's short/long rest (Track 8, constraint 5). · *abilities/world*
- **vitals enable/disable + live on-change vitals.** A player-toggleable on-CHANGE emitter hooked at
  `setResourceCurrent` (where every vital change funnels), driving both the text prompt and GMCP Char.Vitals —
  so a combat round's HP drain updates a plain prompt and a rich gauge live (subsumes the 9.2 combat-tick-HUD
  follow-up). A `vitals enable`/`disable` verb stores the preference. **Same funnel as resting regen** — do with
  or after the rest mechanic. · *edge/mudlib*

## Track 6 — Director, scale & drain  ·  seam: `BeginDrain` + director + scoped-bus envelope

*Why this order:* settle drain OWNERSHIP before placement DRIVES it; piggyback the bus-lag stamp on the envelope
change; the Lua director script is the big substrate the smaller Go handlers could later move into — last.

- **Director-owned + serialized drain target selection (16.4b).** `BeginDrain` takes an injectable
  `TargetChooser`; production self-selects a peer. Make the DIRECTOR (Phase 10.6 leader) own selection +
  SERIALIZE simultaneous drains (avoid two shards draining onto one target past its one-core ceiling, and
  split-brain during a fleet rollout); keep the decentralized chooser as the standalone/dev fallback. **Settle
  before** placement drives drain (constraint 4). · *orchestration*
- **Load/locality-aware placement — the pipeline (PARTIAL).** The PLANNER is load-aware
  (`placement.PlanWeighted`, tested). Remaining: the occupancy SIGNAL pipeline (world → director) supplying real
  weights, wiring the plan to DRIVE `BeginDrain`, a weight-proportional `RebalanceThreshold`, locality-aware
  colocation, and rebalance cooldowns. `internal/placement`, director. · *orchestration/scale*
- **Drain reclaim metrics + clean-disconnect (16.4b).** A straggler at the `BeginDrain` deadline is flushed +
  dropped; emit OTel `drain_redirected`/`drain_reclaimed` counters (infra- vs client-fault) and send a "server
  restarting, reconnect" disconnect to stragglers. Same `BeginDrain` seam as above. · *observability*
- **Durable DOWN state broadcast + snapshot-on-join (Phase 10.4)  [+ piggyback bus deliver-lag].** The director's
  state broadcast DOWN is transient; a zone down when a flag flipped misses it and has no snapshot on join. Add a
  snapshot fetch (read region_state/world_state at boot, or a director "sync" reply) and/or a durable down tier.
  **Piggyback:** this touches the scoped-bus envelope — stamp publish time and record `metrics.RecordBusLag`
  (`telos.bus.deliver_lag_ms`, which today has no call site) at the deliver path while the envelope is open. · *orchestration/observability*
- **Mail dead-letter reap (background job).** A PERIODIC reaper for undeliverable/orphaned mail (rows to a name
  that never logs in) — a director-owned scheduler tick like the weekly spawn scheduler. `internal/store/mail.go`.
  Uses the director scheduler infra this track exercises. · *persistence/orchestration*
- **Slow-client observability + backstop (16.3).** (1) Reframe the per-player "wedged" Warn off a windowed
  drop-RATE (`consecutiveDrops` only catches a fully-stalled client). (2) A world-side `stream.Recv` idle
  deadline / max-blocked-`Send` bound so reclaim doesn't DEPEND on gate correctness. Reliability; loosely here. · *edge/observability*
- **[LARGE] Content-defined (Lua) director script (Phase 10.4).** The director's `SignalHandler` is a Go func;
  `cmd/telos-director` wires NIL. A real deployment needs director logic as CONTENT — a sandboxed Lua VM in the
  director (the zone model) reacting to signal-up + scheduling on the director tick. The substrate the drain
  chooser / mail reaper / placement-signal logic could later move into — build those as Go first, this last. · *orchestration*

## Track 7 — GMCP enrichment  ·  seam: GMCP outbound path

*Cross-links:* `Char.Status` visibility + `Room.Players` live in Track 2 (they gate on visibility);
`Char.Items` count from coalescing lives in Track 1 — do the delta restructure below aware of it.

- **GMCP Char.Items incremental deltas + `Char.Items.Contents`.** Char.Items.List is a full re-send on change;
  add `Char.Items.Add/Remove/Update` deltas + a container-contents payload + live room-item updates. **Coordinate
  with Track 1 coalescing** (the `(N)` count field) so the count lands in the delta shape, not the old full-list. · *edge*
- **GMCP Comm.Channel raw text + `Comm.Channel.List/Players`.** Carry the raw message body (not just the rendered
  line) as a Message field so a client can tab per channel; emit the channel list/players. · *edge*
- **GMCP Char.Stats gauge/stat flags + Char.Vitals gauge filter.** Emit only content-flagged resources
  (`gauge`/`hud` bool) and attributes (`stat` bool) so internal pools don't leak into the HUD. Pairs with the
  `score` content-layout (both decide "what's player-visible"). · *edge/content*
- **Builder-extensible GMCP hooks.** Let content/Lua emit custom GMCP (`Mud.*`) via a sandboxed
  `gmcp.send(player, pkg, table)` handle, routed through the outbound support filter + `validGMCPPackage` guard,
  with a namespace allowlist so content can't spoof `Char.*`/`Core.*`. Scripting-surface addition. · *edge/scripting*

## Track 8 — Content packs capstone  ·  seam: the loader + everything downstream

*Why last:* the packs are the ACCEPTANCE TEST — they exercise worn-affix + salvaging (Track 4), aliases
(Track 3), and rest (Track 5). The directory-tree loader precedes the packs (constraint 6).

- **[LARGE] Multi-file demo packs — the multi-system acceptance sprint.** Two parts:
  - **[LARGE] Content install & reload pipeline [FIRST] — DESIGN SETTLED (2026-07-01), see
    [[content-install-and-reload-direction]].** Grew beyond a dir-walk into the whole install story:
    (1) **layout** — a pack is a directory TREE of small files (`content/packs/<pack>/` with a `pack.yaml` manifest
    + `attributes.yaml`/`abilities/*.yaml`/`zones/<z>/rooms/*.yaml`/…), merged by the loader (extend across-packs
    last-write-wins to across-FILES); (2) **source of truth = an EXTERNAL versioned store** (git/S3; CI publishes
    an immutable versioned snapshot — NO in-game editing); (3) **DIRECTOR-coordinated apply** for multi-shard
    consistency: author publishes version N → director validates (compiles?) → broadcasts a SCOPED `content.reload`
    over the event bus → each shard PULLS + hot-swaps (triggers: git-push/CI, a webhook, or an in-game
    `reload <scope>` builder command; director poll as fallback); (4) **scope-mapped reload** — zone/region/world;
    AREA content reloads live, SHARED defs (attrs/abilities/combat) may need a rolling reboot (accepted); (5) an
    **embedded CORE pack** for bootstrap (a start room + builder verbs + minimal scaffold) so a fresh server boots
    and builders can connect+reload. Spans this track + Track 6 (director) + Track 9 (shared-def reload). Payoff:
    edit ONE area/boss file, push, reload just that area. First move: split `demo.yaml` into the tree.
  - **The three packs (content authoring) [after the engine features they showcase].** (1) Split `demo.yaml`
    into `content/packs/demo/basic/…` (Diku/ROM reference tree). (2) `content/packs/5eSRD` — the CC-BY 5e SRD as
    pure content (Vancian slots, six abilities → modifiers + proficiency, advantage, class/subclass/background,
    **short/long rest — needs Track 5**). (3) `content/packs/WoWSRD` — the WoW-d20 skeleton (rage/energy/focus/
    combo, talent trees, cooldown pacing, threat, raid/loot economy). ZERO engine changes for flavor is the
    acceptance test. (Pathfinder = optional 4th.) · *content/persistence*
- **Demo spawn/death handler content (12.4) — near-term, independent of the loader tree.** Ship demo
  `on_world("spawn.boss")` + boss-death `signal_world("boss.died")` content (the hooks + `mud.spawn` exist) to
  close the live boss-loot loop end to end. **When touching the demo, add an `uncapped: true` gathering
  profession** so the GATED store-round-trip `DeepEqual` covers the `uncapped` bundle flag (currently blind —
  only a `true` can drop it). Can land anytime; naturally rides whatever demo work is active. · *content/orchestration*

## Track 9 — Hot reload & determinism infra  ·  seam: `reload.go`, `combat.go`

- **Shared-def hot reload (7.7).** `reload.go buildPrototype` handles only Room/Item/Mob; a `(kind,ref)`
  invalidation for a SHARED def (ability/affect/formula/`pvp_allowed` policy) is skipped and `z.defs` is
  boot-immutable — no live edit path to a pvp policy / formula. When a slice swaps `z.defs` at runtime, hook that
  seam and re-run the pvp permissive→restrictive end-to-end check. **This is the "shared defs" leg of the settled
  content-install reload design ([[content-install-and-reload-direction]]): the user accepts shared-def/mudlib
  changes may need a ROLLING REBOOT rather than a live swap — so a safe live path here is optional, not required.** · *world/persistence*
- **`reloadLua` chunk-cache invalidation is a substring match (perf, minor).** `reload.go` uses
  `strings.Contains(key, ref)`, over-invalidating; tighten with a keyed `ref → {chunk keys}` index if the chunk
  cache grows large. Same file as above — batch them. · *scripting/perf*
- **Combat reproducibility.** Production combat draws from the process-global `math/rand` (`combat.go`), so a
  live fight isn't seedable/replayable. Thread a per-zone/per-fight seeded RNG through the resolver. · *world/tests*

## Track 10 — Comms & channels  ·  mostly independent

- **Channel HEAR vs SPEAK access split.** `channelDef.canHear` delegates to the same predicate as `canSpeak`;
  split them for "announce" channels (anyone hears, only admins speak). · *comms*
- **"Comms unavailable" player notice.** When the comms bus is wholly down a player sees no channels/tells and no
  notice; expose a `Bus.Available()` probe and emit a one-line notice after login. · *comms/edge*
- **Comms chaos test doubles.** (1) Pin the MemJetStream park-at-`maxDeliver` divergence from real NATS + confirm
  prod AckWait/redelivery config. (2) An AFK-auto-reply best-effort-failure chaos test. (3) A subscribe-side /
  delivery-drop double (flakyBus only models a publish outage). · *tests*
- **`config.<player>` comms subject under future NATS authz (BLOCKED, note only).** When subject-level NATS authz
  lands, put `telos.comms.config.*` under world-publish-only alongside `chan`/`tell`. Nothing to build until NATS
  subject authz ships. · *security/comms*

## Standalone [LARGE]

- **[LARGE] Builder-defined `help` system.** A browsable `help` / `help <topic>` backed by a `help_defs` content
  table (topic ref, title, body, category, "see also"), auto-including the registered command set. Ties to the
  docs/wiki project; largely self-contained (rides the Track 0 reflect-walk net for its DTO). · *mudlib/docs*

## Independent / anytime (no seam entanglement)

- **Per-session `who` cooldown (smaller).** The ~1s roster cache is in; a per-session cooldown further blunts a
  single spammer. · *scale*
- **Stale Phase-14 docstrings sweep (comment-only).** Several headers still describe removed passphrase/SSH-login:
  `internal/account/service.go:4`, `internal/store/account.go:17`, `cmd/telos-account/main.go:3`,
  `internal/gate/gate.go:3/182/226`, `internal/config/config.go:25`. Correct to the OAuth-only state. · *cleanup*
- **Builder-guide note: top-level `state.x = …` re-runs on hot reload.** A reloaded script's non-handler body
  re-executes against the PRESERVED `self.state`, so `state.x = 0` clobbers a live value; idiomatic content
  guards it (`state.x = state.x or 0`). PERSISTENCE.md note added; this remains for the builder guide. · *docs*
- **Gated scope-state CAS tests aren't re-run safe (found alongside the reflect net).** `TestWorldStateRoundTripAndCAS`
  + `TestRegionStateRoundTripAndCAS` (`internal/store/scopestate_test.go`) do a version-0 CAS-create and never
  TRUNCATE their rows, so they PASS only on a fresh DB (CI) and FAIL on a re-run against a persistent local DB.
  Fix: `t.Cleanup` a TRUNCATE, or key each run off a unique suffix — the "gated tests must be re-run safe"
  discipline the pack/reflect tests already honor (they strip-and-replace). · *tests*
- **[Launch milestone] Squash the goose migrations to a baseline.** By launch there are 18+ incremental
  `db/migrations/*.sql`. Goose can't reverse-engineer/regenerate — the method is a SQUASH: fresh PG → `goose up`
  all → `pg_dump --schema-only` → replace `00001..N` with one `00001_baseline.sql` → apply to a fresh DB +
  `pg_dump` again + diff-verify EMPTY. Clean ONLY while there is NO live server (dev+CI apply from zero, no
  `goose_db_version` history to reconcile); do it ONCE at a stable/launch milestone, not repeatedly. Wrap as a
  `make squash-migrations` target. At squash time also REVIEW the def tables — the content-install [LARGE] moves
  content to an external store, so some def tables (`recipe_defs`, `display_defs`, …) may be vestigial and
  shouldn't be baked into the baseline. See [[content-install-and-reload-direction]]. · *persistence/hygiene*
- **Delete merged local branches as work lands.** · *hygiene*

## Blocked / deferred (waiting on another slice — don't start cold)

- **Cross-respawn op-list guard.** `runOps` (death seam) should skip remaining same-op-list ops on a target that
  died+respawned mid-list; build it WITH the respawn-sickness slice. · *world*
- **`ClearPlayer` directory cleanup on logout.** Reconnect routing falls back to the home-zone shard, correct
  only while `ClearPlayer` is deferred (`cmd/telos-gate/main.go`). Revisit when it lands. · *edge*
- **Multi-vital support.** `vitalResource` collapses all `vital: true` resources to the single lowest-ref one;
  generalize damage/death/respawn across vitals if/when a 2nd vital pool is authored. · *world*
- **Instanced zones (party dungeons).** Multiple runtime instances of a zone on the Phase-10.6 dynamic-placement
  substrate: the director mints/reaps instances and routes a party to its own copy. A later content phase; the
  placement coordinator + scoped bus are the substrate. · *orchestration/content*

---

## Recently burned down (context)

*Launch-hardening rounds 1–2 (COMPLETED.md → "Launch-hardening burn-down round 2"):* handoff snapshot Ed25519
auth, corpse-owner PersistID keying, mail evict-oldest-READ sweep, `__Host-` broker cookie, mid-session
hear-access republish, durable `characters.state` byte cap; the `heal`/restorative dice+bonus form, the formula
NaN/±Inf fail-closed guard, the OnKill kill-magnitude cap (`xp_value` + fallback), the reserved-affect-event-kinds
reconciliation, the recipe skill-gate `track` resolution, the content-configurable profession cap + uncapped
kind; the two flaky gate tests, the orphaned `account_auth`/`ssh_keys` drop migration (00017), the stale
`oauth.go` header.

*Round 4 (2026-07-01) — COMPLETED.md → "Burn-down round 4":* the content display-templating subsystem —
`internal/consoleui` layout engine + `internal/colormarkup` shared tokenizer, baked-in RTL bidi isolation, the
`ui.sheet()` Lua sandbox binding (DoS-hardened after a security-audit OOM find), the `display_defs` content table
+ Postgres persistence (migration 00018), and the `score`/`sc` + `inventory` + `equipment` surfaces. Also settled
the content-install & reload [LARGE] design and logged the migration squash-to-baseline as a launch follow-up.

*Round 3 (2026-07-01) — COMPLETED.md → "Burn-down round 3":* the `rollOpAmount` dedupe; the
`learn_profession.profession` → `kind:profession` content-lint; the `on_roll(ctx)` Lua loot hatch; the
reflect-walk DTO store round-trip net (which caught + fixed the resource `on_event_lua`/`on_reaction_lua` and
ability `on_event` silent drops); the `internal/textwidth` display-width helper; the UTF-8/bidi/grapheme/
zero-width render-path tests + live RTL e2e; the ANSI color `{{TOKEN}}` layer (slices 1–3: renderer, `color
on/off`, exits/demo auto-color); and the presentation initial-cap.
