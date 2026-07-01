# Follow-up tasks (deferred backlog)

A running list of **cleanup, tech-debt, and consciously-deferred work** we are NOT
doing now and will revisit later — most in an end-of-roadmap sweep, or when the
owning phase/area is next touched. This is *not* phase work (that lives in
[ROADMAP.md](ROADMAP.md)); it's the stuff we punt to keep moving.

**How to use:** append here when you defer something instead of leaving it only in
a code comment. Each entry should have a `file:line`, an owner, and one line of
why-deferred. Check items off (or delete) as they're resolved. Do a pass at the
end of the roadmap.

---

## 0. Pending reviews (committed with reduced rigor)

- ~~**Phase 7 slice 7.7 (hot reload, commit `abfc5d9`) needs its formal review trio.**~~
  **DISCHARGED** — the security-auditor, distributed-systems-architect, and persistence-engineer
  all formally reviewed 7.7 and returned **CLEAR / SOUND** (no HIGH/MEDIUM). `chunkFor` recompile
  is fail-safe (broken edit keeps last-good; nil chunk denies); the reload can't open a gate; the
  single-writer spine (atomic cache swap on the subscriber goroutine, per-zone Lua via inbox post)
  holds; old-gen timers drop; `self.state` survives the swap; boot/reload prototype parity holds.
  One auditor recommendation was applied here (per-instance breaker reset on a fix-reload, with
  `TestPerInstanceBreakerResetsOnHotReload`); the rest are recorded as new §2 items below.
  **(No pending reduced-rigor reviews remain.)**

## 1. Lint nolint `TODO(owner)` cleanups — ~~RESOLVED~~

The whole TODO-nolint backlog was burned down: mechanical conversions bounded
(`pulseCount`/`pulsesToInt`/`nonNegU64`/`stateVersionParam`); `max` shadow renamed;
config perms/path, telnet best-effort writes, and the `unsafe` test helper fixed;
the unused `maybeStopTick`/`defRegistry.reload`/`contentsByKeyword`/`waitFor`/`echoAbsent`
deleted; and the deliberate suppressions (`PULSE_VIOLENCE` Diku name, the `renew*` ctx
G118, the operator-supplied config G304, the `flags`/`account` data-model stubs) reclassified
from TODO to permanent reasoned nolints. The golangci-lint gate is clean + blocking with
no TODO-nolints remaining; new ones should be resolved or reclassified as they appear.

## 2. Code tech-debt / design deferrals

- ~~**`rx:replace_target` redirect is not wired (only re-gated)**~~ — **RESOLVED** (7.9
  completion): `luareact.go` `rxReplaceTarget` now RECORDS `r.newTarget` on a PASSED
  `guardHarmful(harmActor, newTarget)` re-gate and returns `true`; a FAILED re-gate (non-consenting
  player / detached / cross-zone) still returns `false` and records nothing (the gate-block test
  holds). The OnDamageTaken seam `applyDamageReaction` reads `r.newTarget` back and re-runs the WHOLE
  RAW blow against the new target through the SHARED `dealDamage` pipeline (`applyDamageRedirect`), so
  the blow is RE-MITIGATED against the new target's OWN resistances/soak, fires its OWN OnDamageTaken
  reactions/affects, builds threat + the lit combat events, and can kill it through the uniform death
  seam — while the ORIGINAL target takes 0. The redirect threads the SAME `eventBudget`/`depth` as the
  firing reaction ctx (no fresh root, no privileged depth — `depthOf`/`budgetOf`), so an A→B→A redirect
  loop terminates at the shared `maxEventDepth`/`maxEventHandlers` budget and the zone-level
  `eventCascadeDepth` backstop (never crashes/spins the zone). The re-applied blow re-routes the normal
  `dealDamage`/`guardHarmful` gate; no direct entity-state write (the `luaharm_lint` binding-funnel lint
  stays green). Tests (`luareact_test.go`): redirect lands re-mitigated against the new target's soak
  (original takes 0); the new target's OWN damage-shield reaction fires on the redirected blow; an
  A→B→A redirect loop terminates and the zone keeps serving; the existing non-consenting-player retarget
  stays gate-BLOCKED. Verified `make verify` green incl. `-race`. (deferred capability — DONE) ·
  *scripting/combat/security*
- ~~**`pendingFinalFlush` stash has no active eviction**~~ — RESOLVED: `zone.go` now posts a
  one-shot `createFailedMsg` on the create goroutine's permanent-failure branch, and
  `characterCreateFailed` delete-evicts the orphaned stash (security-auditor re-confirmed the
  cross-session same-name deletion is structurally impossible). Tests: eviction + false-eviction guard.
- ~~**pgx-gated + chaos coverage for the create-window logout race**~~ — RESOLVED: the
  `TELOS_TEST_DSN`-gated, real-Postgres tier landed in `internal/gate/createrace_pgx_test.go` (co-located
  with the MemStore `createrace_repro_test.go`), driving the full gate→world Play path with a real
  `*store.Pool` wrapped in a thin `gatedPgxStore` decorator that blocks/delays `CreateCharacter`:
  (1) `TestPgxCreateWindowLogoutRacePersistsMove` — move+quit inside the window, release the real INSERT,
  assert the moved room is durably persisted; (2) `TestPgxCreateWindowFailLeavesNoRow` — permanent create
  failure leaves Postgres with zero rows (eviction holds, nothing resurrected); (3)
  `TestPgxCreateWindowChaosRandomOffsets` — 20 iterations (looped to 400 in CI-style runs) at randomized
  create-completion offsets across success/failure, asserting the durable end-state is ALWAYS correct;
  (4) `TestPgxCrossSessionSameNameEvictionDoesNotClobber` — the belt-and-suspenders cross-session test (A
  gated-fail + quit-in-window, B gated-success reusing the name on a second zone + quit-after-move, A's
  failure released LAST → B's market row survives). All four `t.Skip` with no DSN; verified green against
  real PG (incl. `-race`). (test coverage) · *test-engineer/persistence*
- ~~**Room-affect tick cadence** — `affect_room.go:189`: the room tick fires EVERY
  pulse and re-leases the CC to every occupant; should lease at `tickInterval`. (perf/hardening) · *world*~~
  **RESOLVED:** `roomTickOnce` now mirrors the per-entity tick — the per-occupant re-lease
  fires only at the affect's `tickInterval` boundary (`sinceTick >= tickInterval`); a tickless
  CC field (no `tick:` block) is leased once for its whole duration and does ZERO per-occupant
  pulse work. `roomAffectLeaseSlack` (>=1) keeps coverage continuous (lease > interval) for both
  a standing occupant and a worst-phase mid-interval entrant. Tests:
  `TestRoomAffectReleasesAtTickInterval` (cadence, controlled-breakable) +
  `TestRoomAffectMidIntervalEntrantStaysRooted` (entry contract).
- **Shared-def hot reload is not wired (7.7 scope)** — `reload.go` `buildPrototype` handles only
  Room/Item/Mob; a `(kind,ref)` invalidation for a SHARED def (ability/affect/formula/`pvp_allowed`
  policy) is "unbuildable, skipped" and `z.defs` is boot-immutable, so there is NO live edit path to
  a pvp policy / formula today. The source-aware `chunkFor` + the `reloadLua` chunk-drop are the
  correct fail-safe FOUNDATION for it; when a slice swaps `z.defs` at runtime, hook that seam and
  **re-run the pvp permissive→restrictive end-to-end check** against a live policy edit. (deferred) · *scripting/security*
- **`notifyZones` blocking-posts can head-of-line-stall the shard reload pipeline** — `reload.go:180`:
  the subscriber goroutine does a blocking `z.post(reloadLuaMsg)` to EVERY hosted zone; one wedged /
  saturated zone inbox stalls all later invalidations shard-wide. Low-probability today; if a shard
  hosts many zones (Phase-10 placement packs more per process), make the `reloadLuaMsg` post
  non-blocking (a dropped notice is recoverable — the cache is already swapped; next invalidation
  re-posts). (distsys/hardening) · *world/distsys*
- **`reloadLua` chunk-cache invalidation is a substring match** — `reload.go:163` uses
  `strings.Contains(key, ref)` (plus dropping `pvp_allowed`/`formula:` every reload), so it
  over-invalidates (a harmless recompile-from-current-source). Correctness-safe; tighten with a keyed
  `ref → {chunk keys}` index if the chunk cache ever grows large. (perf, minor) · *world*
- **Phase-10: `r.shard.zones` concurrent iteration** — `notifyZones` iterates the shard's zone map
  from the subscriber goroutine; safe now because the map is written only during boot (`adopt`,
  pre-`Run`). When dynamic zone placement (Phase 10) lets a shard claim/release zones at runtime, this
  becomes a concurrent read vs a live writer — snapshot under a lock or post through a shard-owned
  goroutine. (distsys) · *world/distsys, Phase 10*
- **Doc: a top-level `state.x = …` initializer re-runs on hot reload** — the reloaded script's
  non-handler body re-executes against the PRESERVED `self.state` table, so a top-level `state.x = 0`
  clobbers a live value (e.g. a quest counter) on an unrelated edit. Idiomatic content guards it
  (`state.x = state.x or 0`). One-liner for `docs/PERSISTENCE.md` (the `self.state` section) + the
  builder authoring guide. (doc) · *persistence/mudlib*
- **`who` scale: SCAN + N×HMGET per call, unbounded** — `internal/presence/redis.go` (8.4): every
  `who` spawns a goroutine doing a full keyspace SCAN + an HMGET per online player, with no rate-limit
  or result cache; a `who` flood or a large roster is the first scale pressure point. Off-zone-goroutine
  + 5s-timeout-bounded so it can't stall the actor loop, so it's a scale item not a correctness bug.
  Before high-concurrency launch: a short per-session `who` cooldown OR a shared ~1s-TTL cached roster
  snapshot (collapse N spammers to one SCAN/sec). (scale, deferred) · *distsys/persistence*
- **`who` visibility filter** — `internal/world/presence.go` `renderWho` (8.4): `who` lists every online
  player cross-shard with NO visibility filter, so an invisible/builder-hidden/wizinvis player appears.
  Acceptable now (no visibility flags exist yet); when the visibility tier lands (the [[builder/wizard
  trust tier]] §4 + the `phase5-visibility` TODO), `renderWho` must filter hidden players at the RENDER
  boundary (the cross-shard read returns all entries; the per-viewer privilege filter is the chokepoint),
  and presence should carry a visibility flag. (feature, tied to the visibility tier) · *mudlib/edge*
- **Mid-session hear-access staleness** — `internal/world/commsstate.go` (8.6): the gate's channel
  hear-set is re-published on login/handoff/toggle, but NOT on a mid-session hear-ACCESS change (an
  affect/attribute change crossing a channel's `min_attr` floor). So a player who drops below a
  restricted channel's threshold keeps HEARING it until their next toggle/handoff/relog — a bounded,
  hear-only window (speaking is gated live per-send; `require_flag` access can't change mid-session).
  Only matters once a `min_attr`-gated restricted channel ships. Fix: call `publishCommsConfig` from
  the affect-apply/expire + attribute-recompute hook. (security LOW, bounded) · *edge/world*
- **`config.<player>` comms subject under future NATS authz** — `commbus` (8.6): `telos.comms.config.*`
  is deliberately NOT `isACLGuarded` (engine mechanism, like presence; a gate subscribes only its
  concrete `config.<self>`, gates never publish). Safe today on the same broker-honesty assumption the
  whole in-process chan/tell ACL already rests on. When subject-level NATS authz lands, put `config.*`
  under world-publish-only alongside `chan`/`tell` so the `isACLGuarded` exclusion isn't misread.
  (security note, no code now) · *distsys/security*
- ~~**Mail inbox cap / `ListMail` LIMIT**~~ **RESOLVED (security hardening)** — `world.MailInboxCap` (100)
  bounds a recipient's TOTAL inbox: pgx `SendMail` refuses past the cap via an ATOMIC count-subquery +
  conditional INSERT (no TOCTOU), returning the distinct `world.ErrMailboxFull` so `mailcmds` renders "X's
  mailbox is full" (not "unavailable"); `ListMail` gained a `LIMIT` at the cap. MemStore mirrors it. Tests:
  hermetic `TestMailInboxCap` + gated pgx `TestMailInboxCapPersists`. **Still deferred (smaller):** a
  read-mail retention SWEEP + reaping the directory-error dead-letter rows (the cap already bounds growth). ·
  *persistence/security*
- **`ClearPlayer` deferred coupling** — `cmd/telos-gate/main.go` (~:127,142,145): reconnect
  routing falls back to the home-zone shard, correct ONLY while `ClearPlayer` is
  deferred. Revisit when `ClearPlayer` (directory cleanup on logout) lands. · *gate/distsys*
- **Cross-respawn op-list guard** — `runOps` (death seam) should skip remaining
  same-op-list ops on a target that died+respawned mid-list. Safe today (re-gated);
  build it WITH the respawn-sickness slice, when there's an invariant to protect.
  (security S1) · *combat/progression* — see [death-prevention notes].
- **Multi-vital unsupported** — `vitalResource` collapses all `vital: true` resources
  to the single lowest-ref one, so a 2nd vital pool (stamina/blood) would be dead
  config. Generalize damage/death/respawn across vitals if/when authored. · *world*
- **Death/corpse hardening (Phase 11 + security)** — `death.go:150` death-narration
  `mag = victim max-hp` is builder-influenceable; `death.go:194` the corpse is an
  UNOWNED free-for-all (no loot ownership). Both are intentional minimal-slice
  behavior to revisit with the progression/loot ruleset. · *progression/security*
- ~~**Retire the redundant `Redirect.resume_input_seq` wire field (`Play` protocol)**~~ —
  RESOLVED (option a): deleted `Redirect.resume_input_seq` from `play.proto` (field 3 now
  `reserved`) + all the Go plumbing that only fed its diagnostic log (the `redirectFrame`
  param, `redirectMsg`/`redirectTarget` fields, the source-side `snap.GetAppliedSeq()` feed,
  and the two gate debug logs). The gate replays authoritatively from the destination's
  `ServerFrame.ack_input_seq` on the `Attached` frame, so no resume point travels on the
  redirect. `Attach.input_seq` is KEPT — the single-session clean-kick made it load-bearing.
  Verified: in-process handoff/resume/cross-shard tests `-race` green, and `make smoke-twice`
  (the full Docker stack with the regenerated proto) passed incl. the cross-shard reconnect.
- ~~**Lua relocation combat-fidelity** (7.3c distsys review)~~ — RESOLVED: `relocateWithinZone`
  now applies per-method semantics — `h:move` fires `OnLeaveRoom` + PROVOKES opportunity attacks
  (walk-like); `h:teleport`/`h:recall` BYPASS (blink/yank, no OA — the point of a teleport). All
  three force-`disengage` the mover (preserving the no-fighting-pointer-spans-a-room invariant),
  fire `OnEnter` on arrival (parity with the engine move), and re-check liveness (`stillHere()`)
  after the leave checkpoint and each arrival hook (a lethal `aggroOnEntry`/OnEnter can't cause a
  use-after-relocation). The new fires thread `parentCtx()` (shared depth/eventBudget); a
  teleport→OnEnter→teleport loop is bounded by the 7.8 `eventCascadeDepth` backstop (terminates,
  no crash — scripting/security reviewed SOUND, combat reviewed). Tests in
  `luaharm_relocate_test.go` (disengage, per-method OA, liveness-recheck — mutation-verified).
  Note: Lua relocation now fires `OnEnter`, so content subscribed to it reacts to Lua moves too. · *combat*

## 3. Possible latent bugs (also surfaced as chips)

- ~~**`gate.go:256` `resumeSeq` accepted but never read** — possible session-resume /
  reconnect frame-replay not plumbed. Investigate + plumb or drop.~~ · *gate* · chip
  `task_44fcce5f` — **RESOLVED:** investigated; resume IS wired end to end, just not via
  this param. The destination shard is authoritative — it reports its applied high-water
  mark on the `Attached` frame's `ack_input_seq`, which drives `doReplay`. The redirect's
  `resume_input_seq` was a redundant source-side estimate. Dropped the dead `runStream`
  param + its nolint (kept `redirectTarget.resumeSeq` for the diagnostic log). The
  remaining *wire-level* redundancy is now tracked in §2 below.
- ~~**`combat_test.go` empty `if z.move(...)`**~~ — RESOLVED: `TestCannotMoveWhileFighting`
  asserts the move outcome on `s.entity.location` + the exclusion message (the `if z.move(...)`
  refusal Fatals); the one local-move whose bool is always-false is now an explicit `_ =` with a
  comment, the relocation asserted on location. · *combat* · chip `task_0db5e6e9`
- ~~**Single-session takeover leaves the first connection silently mute**~~ — *edge/persistence*
  design call surfaced by `TestSecondLoginTakesOverSession` (the coverage wave-1 push). **RESOLVED:**
  implemented the single-session CLEAN-KICK contract. A second login for a still-live character now
  cleanly kicks the old connection: the displaced socket gets a player-visible "logged in elsewhere"
  notice + a Disconnect frame (the quit teardown path) and is closed; the new connection's stale
  dedup fence is reset to its fresh resume point so its first input is applied, not swallowed. The
  takeover decision lives in the world (`zone.go attach` `case s != nil`, discriminating a live
  takeover via `!s.detached` from a link-dead resume); the fence reset keys off the gate's
  `Attach.input_seq` (a fresh, restarted numbering at-or-below the carried `appliedSeq` clamps the
  fence). A genuine link-dead RECONNECT/handoff resume is untouched — only a SECOND CONCURRENT login
  triggers the kick. `TestSecondLoginTakesOverSession` + `TestTakeoverResetsInputFence` assert the new
  contract.

## 4. Deferred features / design directions

### Player command/HUD ideas (user, 2026-06-29)

- **`vitals enable/disable` + live vitals updates.** Players should be able to opt IN to having their
  vitals (hp/mana/move/…) pushed on every CHANGE — not just on the prompt. Two delivery paths, one
  toggle: (a) the TEXT prompt re-sent whenever a vital changes (combat damage, regen) for a plain
  client, and (b) GMCP Char.Vitals on change for a rich client (already emitted on the prompt today;
  this drives it on the underlying change too). A `vitals enable`/`vitals disable` verb stores the
  preference (the per-player config / comms-state-style subtree). This SUBSUMES the existing "combat-tick
  HUD emit" Phase-9 follow-up: the real shape is a player-toggleable on-change emitter, hooked at
  setResourceCurrent (the one place a vital changes), change-detected, gated on the player's toggle. · *edge/mudlib*
- **A builder-defined `help` system.** An exhaustive, BROWSABLE help command — `help` lists commands/
  topics, `help <topic>` shows detail, more than a bare verb list. Topics are CONTENT (builders author
  them, like channels/abilities): a `help_defs` content table (topic ref, title, body, category, "see
  also" links, and which command/ability it documents) so the engine names no help text. Auto-include
  the registered command set (each Command/ability can carry a short help string) plus builder topics.
  Ties to the documentation-engineer agent + the end-of-roadmap wiki. · *mudlib/content*
- **Inventory shows equipment; can't drop equipped; `keep`/`unkeep` no-drop flag.** Three related
  item-handling changes: (1) `inventory` should fold in the `equipment` view (show worn items, flagged),
  so one command is the full picture — the GMCP Char.Items.List already carries worn items with the "W"
  attrib; mirror that in the text command. (2) `drop` must REFUSE an equipped item (today cmdDrop silently
  auto-removes the worn slot before dropping — `container.go`; change it to require an explicit `remove`
  first, so you can't fat-finger your weapon onto the floor mid-fight). (3) A `keep <item>` / `unkeep
  <item>` verb sets a per-item no-drop flag (an ItemJSON/Wearable-adjacent bool that rides the carry +
  the durable save) so a player can't accidentally drop a kept item — important for items that GRANT
  skills/abilities by being carried (a guild totem), which Phase 11 progression will lean on. The
  keep-flag + carried-item-grants-abilities shape fits the Phase 11/12 itemization pass. · *mudlib/progression*


- **"Comms unavailable" player notice (Phase 8.6, 8.2-note).** When the comms bus is wholly down
  (NATS unreachable ⇒ a disabled `commbus.Bus`), comms are silently off — a player sees no channels/
  tells and no notice. Deferred deliberately in 8.6: a disabled bus is byte-identical to a pre-Phase-8
  process, and detecting "disabled" from the `Bus` interface would couple the gate to bus internals,
  weakening the content-free-sink invariant. If wanted, expose a `Bus.Available()`/role-degraded probe
  and have the gate emit a one-line notice after login. · *edge/orchestration*
- **Channel HEAR vs SPEAK access split (Phase 8.6).** `channelDef.canHear` currently delegates to the
  same predicate as `canSpeak` (a restricted channel restricts both). A content shape for "hear-only"
  / "speak-only" channels (an announce channel anyone hears but only admins speak) would split the
  `channelAccess` into separate hear/speak predicates at the obvious `canHear` divergence point. · *content/world*
- **Builder/wizard trust tier — elevated visibility + debug tooling** (much later; post-core).
  Builders/immortals need a privilege level ABOVE player:
  - **See-all visibility.** A builder always sees what players can't — an `invisible`-affected
    player (a skill that hides them from other players, modulo a perception/`canSee` check) is
    ALWAYS visible to a builder; likewise hidden/dark/wizinvis entities. This is the ELEVATED end
    of the `canSee`/`nameFor` chokepoint the dark/invis flags will introduce (the
    `phase5-visibility` TODO in `internal/world/commands.go` `lookRoom`). The "holylight" tradition.
  - **Inspection surfaces underlying object data.** A builder examining a thing sees instance +
    prototype identity and internal state a player never does — illustratively "a rusty dagger
    (instance 0x342f of 0x33ee)" where a player sees just "a rusty dagger". Likely an
    inspect/`stat`-style command or a builder-facing long description, NOT the player short. The
    Diku `stat`/vnum-display lineage.
  - **Runtime-tweakable per-builder toggles.** e.g. show/hide my own combat dice rolls, holylight
    on/off, wizinvis level, verbose event/debug echoes — flipped live, scoped to that session.
  - General: builders are first-class *debuggers/inspectors*, not just authors. Ties to the builder
    persona in the end-of-roadmap wiki and the permission/trust model; the visibility half is the
    elevated counterpart to the player-facing `canSee` gate. · *mudlib/edge*

- **Cross-shard handoff: destination pack-set VALIDATION (the unknown-prototype data-loss window).**
  The full-state carry (`internal/world/handoff.go` `buildSnapshot` `StateJson`, applied in
  `internal/world/zone.go` `prepare`) re-spawns carried items by prototype ref on the destination.
  If the destination shard enables a DIFFERENT pack set than the source, an item's prototype can be
  unknown there: `loadItem` skips it with a LOUD `Warn` and the arriving player gets a one-line notice
  ("Some of your items did not transfer to this area.") — but the item is GONE. This is a data-loss
  window save/load does NOT have (same content on both ends of a reload). The deeper fix: validate the
  destination's enabled-pack set covers the carried prototypes BEFORE accepting the handoff (reject /
  re-route / stage the item), rather than dropping post-commit. Until then the carry conserves exactly
  what save/load does plus the loud warn + notice. · *persistence/distsys*
- **Cross-shard handoff: total inventory node/byte CAP (depth cap only today).** The carry's container
  nesting is currently bounded only by `maxItemNestDepth` (`internal/world/character.go` `loadItem`):
  a degenerate/adversarial tree is truncated at depth 16 with a loud log. The script/comms/tell
  subtrees each have their own size caps; inventory still lacks a TOTAL node-count / byte ceiling on
  the Prepare payload (a wide-but-shallow tree is unbounded). Add a total-node or marshalled-byte cap
  on the carry (and ideally on the durable `characters.state` write) as the complete guard. · *persistence*

### Content-authoring gaps surfaced by the richer demo pack (Phase 8 playtest)

Building the richer demo content (11 abilities, reaction mobs, 5 affects, restricted `guild`
channel, gear) exercised authoring paths that the thin starter pack never did, exposing four
content-expressiveness gaps. None block the engine; each is a place where content can't yet say
something an author would reasonably want to:

- **Equip applies no stat modifiers (the equip stub).** Wearing an item moves it to an equipment
  slot and nothing more — a "+1 sword" or "ring of protection" confers no bonus, because the
  equip path has no hook into the affect/attribute layer. **Deferred to itemization (Phase 11/12)
  per the user's decision** — equip-mods belong with affix rolls / item quality, not bolted onto
  the slot move now. The clean shape: an equipped item contributes a (suppressible) affect bundle
  installed on wear and removed on remove, riding the SAME affect-stacking the ability layer
  already has. · *abilities/progression*
- **Wear slots are a closed enum.** The wearable slot set is engine-fixed; content can't author a
  novel slot (e.g. a system with "tabard" or "sigil" slots, or a two-handed/off-hand split a
  different game system wants). Folds into the itemization pass — make the slot vocabulary
  content-defined (a `wear_slots` content table) the same way resources/attributes are, per the
  "extensibility across game systems" pillar. · *abilities/content*
- **The declarative `heal` effect-op ignores dice.** A content ability's `heal` op takes a flat
  amount; it can't express a dice expression (`2d8+WIS`) the way damage-adjacent design wants,
  so a 5e-style "cure wounds" can only approximate. The dice evaluator exists (combat uses it);
  the gap is wiring a dice-expression form through the declarative heal op (and, by symmetry,
  any restorative op). · *abilities/combat*
- ~~**No content verb/op grants a flag.**~~ **RESOLVED (Phase 11.1)** — `effect_op_grant.go` ships
  `set_flag(target, flag)` + `clear_flag(target, flag)` effect-ops (persisted via `setFlag`), so a content
  ability CAN now set/clear the `guildmember` flag (the exact example) and a "join the guild" interaction is
  authorable as pure content. Both go through the PvP/harm gate like every op. · *abilities/world*

### Comms chaos coverage follow-ups (W8, from the distsys review of comms_chaos_test.go)

- ~~**End-to-end durable-tell redelivery test (Consume-driven).**~~ RESOLVED —
  `TestDurableTellRedeliversWithinMaxDeliver` (comms_chaos_test.go) drives a real `tell` through the full
  PublishDurable → Consume → deliverBounded path with the first tell emit failing (failTellN), asserting
  the bounded-redelivery loop retries and delivers EXACTLY ONCE (cursor → 1, no dup). The actual
  end-to-end never-lost guarantee is now pinned.
- **MemJetStream park-at-maxDeliver DIVERGES from NATS (document + pin).** `deliverBounded`
  (`internal/commbus/jetstream.go`) re-runs the handler on the same in-memory msg up to `maxDeliver` (3)
  rapid attempts then PARKS (drops); the MemJetStream cursor (`memjs.go`) advances before delivery and
  never rewinds. So a publish outage lasting longer than 3 fast retries LOSES the tell under the test
  double, whereas real NATS JetStream redelivers with `AckWait` backoff and only advances on ACK — so a
  minutes-long outage survives. Pin this as known test-double behavior (a "fails past maxDeliver → parks"
  test) so the divergence is explicit, and confirm the prod NATS path has the AckWait/redelivery config
  that makes the never-lost promise real. · *distsys*
- **AFK auto-reply best-effort failure (cheap, same flakyBus).** `tell.go`'s AFK auto-reply is
  `_ = z.commsBus().Publish(...)` (error discarded, after the cursor advance). Add a chaos test: a
  flakyBus failing during the AFK reply must still ACK the original tell + advance the cursor + render to
  the target — i.e. a best-effort reply failure can't NAK the durable path into a redelivery storm. · *test*
- **Subscribe-side / delivery-side partition (flakyBus can't model it).** flakyBus fails `Publish`, so it
  models a publish-side outage. It cannot model "the broker accepted the publish but the gate's
  subscription is dead" (world publishes nil-error, gate never renders) — a silently-dropped channel line
  the speaker thinks went out. Needs a delivery-dropping double (a Subscribe that stops feeding the
  handler). · *distsys/test*

### Phase 9 (GMCP) follow-ups

- **Mobs/occupants in the room over GMCP (user request).** Char.Items.List "room" carries ground items
  + corpses, but not the LIVING occupants (other players, mobs) — a rich client can't show "who/what is
  here" structurally. Add a Room.Players (other players) and a mob/occupant list (the GMCP doc names
  Room.Players; a Room.Mobs or a richer occupants payload covers mobs), emitted from lookRoom alongside
  Room.Info, change-detected. Route player names through the canSee/nameFor chokepoint (visibility). · *edge*
- **Builder-extensible GMCP hooks (user request / design direction).** Today every GMCP package is
  engine-emitted (Char.*/Room.*/Comm.*). Let CONTENT/Lua emit custom GMCP — a builder-defined package
  (the Mud.* namespace, or an arbitrary advertised package) pushed from a Lua trigger / a content rule —
  so a builder can send quest/HUD/widget data without an engine change. Needs: a sandboxed Lua handle
  (e.g. `gmcp.send(player, pkg, table)`) that marshals a data-only table to JSON, routes through the gate
  support filter + validGMCPPackage (the same outbound injection guard), and a package-name allowlist/
  namespace policy so content can't spoof Char.*/Core.*. Ties to the "every action is hookable" pillar
  and the Mud.* namespace already reserved in docs/GMCP.md. · *edge/scripting*

#### From the 9.2 edge-engineer review

- **Combat-tick HUD emit (the live HP gauge).** Char.Vitals is emitted only alongside the text prompt
  (sendPrompt), so it updates on each command — but a combat round (`runCombatRound`/`resolveSwings`,
  combat.go) drains HP on the pulse with NO prompt, so a rich client's gauge FREEZES mid-fight and only
  catches up on the next keystroke. **SUBSUMED by the `vitals enable/disable` + live-vitals-updates
  item in §4** — the real fix is a player-toggleable on-CHANGE emitter hooked at setResourceCurrent
  (where every vital change funnels), driving both the text prompt and GMCP Char.Vitals. · *edge/combat*
- **Player-facing-gauge / stat flags (content schema).** Char.Vitals (gmcp.go charVitalsJSON) emits EVERY
  registered resource, including internal pools a builder may define (`perRound` reactions budgets, a
  `rage`/counter pool) that are mechanics, not gauges — they'd leak as `"reactions":N` in the HUD. Latent
  (no sample content defines one yet), but the `perRound` convention is documented. Char.Stats is deferred
  for the SAME reason (no "which attributes are player-facing stats" predicate). Resolve both with content
  flags: a resource `gauge`/`hud` bool (Vitals) and an attribute `stat` bool (Stats) — emit only flagged
  defs. [Char.Stats is being implemented with the `stat` flag in 9.2b; mirror it for resources here.] · *edge/content*
- **Char.Items incremental deltas + Contents (Phase 9.4 v1 deferral).** Char.Items.List (inv + room)
  is emitted as a FULL list, change-detected on the prompt — correct panels, but a get/drop re-sends the
  whole list rather than a `Char.Items.Add/Remove/Update` delta, and a room item another player drops only
  shows on the viewer's next command (not live). Add the incremental delta messages (bandwidth + live
  room updates) and `Char.Items.Contents` (a container the player opened) when the panels need them. The
  room "icon" field is also unfilled (no content icon yet). · *edge*
- **Comm.Channel.Text raw text (Phase 9.5).** The GMCP Comm.Channel.Text `text` field carries the
  fully-RENDERED line ("[Gossip] Alice: hi") because that is all the gate's commsClient has — the raw
  message body isn't carried separately on the comms bus Message. A client routing to a per-channel tab
  would prefer the raw text ("hi") + the talker. Carry the raw text as a Message field (a Phase-8 comms
  shape change) and emit it here. Also Comm.Channel.List/Players are not yet emitted. · *edge/comms*
- **Char.Status target visibility.** charStatusJSON emits `e.living.fighting.Name()` (the entity short),
  bypassing the act/look visibility filter. Safe today (it's the player's OWN opponent — you can't fight
  what you can't perceive), but once invis/disguise content lands it would surface a hidden foe's true
  short; route it through the same `nameFor`/canSee chokepoint then. · *edge/mudlib*

## 5. Housekeeping

- **Combat reproducibility — production draws from the process-global `math/rand`** (`internal/world/combat.go:646`,
  code TODO not previously in this list). A live fight is not seedable/replayable, so a bug can't be
  deterministically reproduced. Thread a per-zone (or per-fight) seeded RNG through the combat resolver instead
  of the global default. · *combat/test*
- Delete merged local branches as work lands (e.g. `test-standard-structure`).
- **Flaky `gate.TestChannelLineRendersVerbatimNoTellPrefix` under CI -race load.** Timed out once at 10s
  waiting for a channel line on a docs-only commit (no code change), passed on re-run + 5x locally under
  -race. The 10s deadline is tight for channel delivery (comms bus → gate client) under a loaded `go -race`
  job. De-flake: raise the per-line wait or settle the comms path before asserting. · *test/edge*

## 6. Phase 10 (orchestration) deferred work

- **Rebalance DRAIN executor (Phase 10.6).** `placement.Plan` computes the desired moves and the director
  coordinator LOGS them, but nothing EXECUTES a graceful rebalance: draining a zone's live players to the
  new owner via the cross-shard handoff fanned over the whole zone (reusing `Shard.Drain` + the per-player
  handoff). Until then the coordinator is observe-only and balance is boot-time/failover-driven. · *orchestration*
- **Runtime zone-add for a standby (Phase 10.6).** A standby world server that re-claims an orphaned zone
  after a failure cannot host it without a restart — a live shard cannot add a zone at runtime today. Needs
  a `Shard.AdoptZone(lc, zoneID)` that builds + starts a zone goroutine into a running shard. Until then,
  decentralized failover re-claims the lease but the zone is served only after the claimer restarts. · *orchestration*
- **Content-defined director script (Phase 10.4).** The director's `SignalHandler` (orchestration logic) is
  a Go func — the production `cmd/telos-director` wires a NIL handler (signals are drained+acked, no logic).
  A real deployment needs director logic as CONTENT (a sandboxed Lua VM in the director, the same model as a
  zone, reacting to signal-up + scheduling on the director tick). The 10.5 capstone proves the machinery
  with a Go handler. · *orchestration/scripting*
- **Durable DOWN state broadcast + snapshot-on-join (Phase 10.4).** The director's state broadcast DOWN is
  TRANSIENT (a live push). A zone that was down when a flag flipped misses it until the next set; it has no
  initial snapshot of current scope state on join. Add a snapshot fetch (read region_state/world_state at
  zone boot, or a director "sync" reply to a zone "I'm here") and/or a durable down tier. · *orchestration*
- **Load/locality-aware placement balance (Phase 10.6).** `placement.Plan` balances by zone COUNT; a newbie
  town ≫ an empty wilderness. Move to load-aware (player count / tick time) and locality-aware (keep adjacent
  zones colocated so common moves stay in-process) balancing, with rebalance cooldowns (PLACEMENT.md §7). · *orchestration*

## 7. Phase 12 (loot & spawns) deferred work

- **Worn-affix stat effect (Phase 12.3).** A rolled item's `Quality` affixes are stored + persisted but do
  NOT yet modify the wearer's stats. Wire the gear-modifier seam: on equip, register the affixes as a
  `modSource` (attributes.go `addModSource`, the existing stub) on the wearer; unregister on remove. Then a
  "+5 strength" sword actually grants +5 when worn. · *progression/combat*
- **Normalized `affix_defs` table (Phase 12.3).** The affix pool is inline in a loot entry's `quality` spec
  (coarse v1). A shared `affix_defs` content table (named affixes referenced by ref) would de-duplicate
  pools across items + allow a richer legendary pool by reference. · *progression*
- **`on_roll(ctx)` Lua loot hatch (Phase 12.1).** The resolver is fully declarative. Add the Lua escape
  hatch for conditional drops the declarative form can't express ("the Sunsword only drops while the realm
  is at war", "guarantee it on a first-ever kill") — declarative for the 80%, Lua for the rest (LOOT §5). · *progression/scripting*
- **Per-mob XP value / kill-magnitude cap (Phase 11.3/12).** death.go fires OnKill with `mag` = the victim's
  raw max-hp (builder-influenceable — a high-max-hp mob is an XP/loot farm). Read a content `xp_value`
  attribute or cap/normalize the magnitude before it feeds XP or loot luck. · *combat/progression*
- **Scheduled-spawn zone reaction (Phase 12.4).** The director broadcasts `spawn.boss` DOWN; the actual
  spawn needs a content `on_world("spawn.boss")` handler that matches the zone + runs `mud.spawn` (and the
  boss's death must `signal_world("boss.died", {ref})`). The capstone proves the loot half; ship demo
  spawn/death handler content to close the live loop end-to-end. · *orchestration/scripting*
- **Profession cap: content-config + kind split (Phase 13.3, D2).** `craftProfessionCap` (profession.go) is a
  uniform constant (2). D2 wants it CONTENT-configurable and only applied to *crafting* professions —
  gathering/utility professions unlimited. Needs a profession "kind" (a bundle field, or read the bundle's
  `kind`) + a pack-global cap setting. · *progression*
- **Round-trip test only covers DEMO-exercised fields (Phase 13.3 lesson).** TestStorePackRoundTrip caught the
  ability `requires_grant`/`skill` field-drop ONLY because the 13.3 craft verb was the first demo ability to
  set them — the gap shipped silently in 11.3/11.4a. Add a store-layer test that asserts EVERY DTO field
  round-trips (e.g. a reflect-walk over a fully-populated synthetic pack), so a new field can't be dropped on
  the store path just because no demo content uses it yet. · *persistence/test*
- **Generic salvage/craft verbs (Phase 13.4/13.5).** Each salvage/craft is one verb bound to a FIXED source
  proto / recipe ref in its on_resolve (disenchant→sword, forge→craft:leather_vest). A real client wants
  `disenchant <item>` (object-targeted, gated on an item TAG) and `craft <recipe>` (recipe chosen by
  argument). Needs object-target resolution into the op + a recipe-name arg lane. · *progression/parser*
- **Recipe skill gate uses the level ATTR, not the track (Phase 13.5).** RecipeDTO.Skill names the skill
  LEVEL attribute (e.g. leatherworking) + min_skill; it does not resolve the track's level_attr. Fine while
  the convention holds (track level_attr == the named attr) but brittle if they diverge. · *progression*
- **Web auth hardening (re-triaged after the Phase-15 pivot).** The 14.7 audit flagged F2/F4/F8, but Phase 15
  DELETED the website (the dashboard, the persistent SESSION cookie, and `/play` + `/logout`), so: **F2 MOOT**
  (no stateless session bearer token remains — only the single-use `flowCookie`, cleared after the OAuth
  callback), **F8 MOOT** (`/play` + `/logout` no longer exist). **F4 still applies, shrunk:** the broker's
  `flowCookie` (`internal/web/session.go`) should take the `__Host-` prefix when served over TLS (Secure +
  Path=/ + no Domain) so it can't be planted over http / from a sibling subdomain (login-fixation on the OAuth
  state). Minor, deployment-time; `secureCookies` is already config-driven. · *web/auth/security*
- **Flaky session-lock takeover test under -race CI (Phase 15.1 sighting).** `internal/gate`
  `TestSessionLockTakeoverKicksDisplacedConnection` timed out (10s) waiting for "logged in from another
  location" on the loaded `-race` CI runner (passed 5/5 locally; resolved by a job re-run). The takeover-kick
  delivery is timing-sensitive. (Re-triaged: it uses a hermetic no-account gate harness with the legacy
  name-login, which is legitimate + still present, so the "rebuild on the OAuth login" note is MOOT — the
  remaining work is just to de-flake: give the kick-message wait a generous/synchronized deadline.) · *gate/test*- **Instanced zones (deferred from Phase 16, 2026-06-30).** Multiple runtime instances of a zone on the
  Phase-10.6 dynamic-placement substrate: the director mints/reaps zone instances and routes a party to its
  own copy (a dungeon instance). Deferred out of Phase 16 (hardening/scale) as a world/content feature; fold
  into a later content phase. The placement coordinator + the scoped event bus are the substrate. · *world/orchestration*
- **Bus deliver-lag metric unwired (Phase 16.1/16.2).** `metrics.RecordBusLag` +
  the `telos.bus.deliver_lag_ms` instrument exist but have no production call site:
  scoped-bus event envelopes carry no publish timestamp, so deliver-lag can't be
  computed without adding one to the wire format. When the scopebus envelope next
  changes, stamp publish time and record (publish->deliver) at the deliver path.
  `internal/metrics/metrics.go:86`, `internal/scopebus/scopebus.go` · *orchestration/obs*
- **Bot-swarm sync token + load realism (Phase 16.2).** The load bot keys each
  command's round-trip on the literal `"> "` prompt (`internal/botswarm/botswarm.go:39`).
  Safe today (prompts are per-session not room-broadcast, GMCP is off for the bot, and
  demo prose never contains `"> "`), but brittle against content that emits `"> "` in
  prose (a tell/quote) — a false match would smear latencies. If the mix ever drives
  output that can contain the token, switch to a unique per-command sentinel the server
  echoes. Also: the mix is single-zone (midgaard) read-only movement — it never exercises
  writes (combat/items) or the cross-shard handoff (the riskiest concurrency path). Broaden
  the mix when load-testing handoff at scale. · *test/distributed*
- **Slow-client backpressure refinements (Phase 16.3 review).** Three deferred items the 16.3 reviews
  flagged, none blocking: (1) the per-player "wedged" Warn (`internal/world/session.go`) only catches a
  FULLY-stalled client because `consecutiveDrops` resets on any successful enqueue — a LIMPING client
  (drops most, drains a little) never trips it; reframe the human signal off a windowed drop-RATE (EWMA or
  drops/(drops+sends) over a sliding window) while keeping consecutiveDrops as the "definitely dead"
  sub-signal. The shard-wide `telos.gate.frames_dropped_total` metric already covers the limping case, so
  this is observability polish. (2) No WORLD-side reclaim backstop: a client dialing the shard Play gRPC
  DIRECTLY (bypassing the gate) and never reading pins the slot + writer goroutine + out buffer
  indefinitely — reclaim depends entirely on the gate's write-deadline. Prod is protected by the Attach
  signed-assertion check (`internal/world/server.go`), so it's in-trust-domain, but add a `stream.Recv`
  idle deadline or a max-blocked-`Send` bound as defense-in-depth so reclaim doesn't DEPEND on gate
  correctness. (3) Honest reclaim latency for the 16.4 capstone: a wedged telnet client is reclaimed after
  ~30s (write deadline) + the link-death grace, not 30s — and a wedged client caught MID-DRAIN is
  deadline-dropped by design, so "zero-drop" graceful drain (16.4) must be scoped to HEALTHY connections
  and separately count deadline-reclaimed ones. · *world/gate/distributed*
- **Bounded drain fan-out concurrency (Phase 16.4b review).** `Zone.drainZone` (internal/world/drain.go)
  fans `beginHandoff` over EVERY resident at once — N residents = N concurrent Handoff.Prepare RPCs to the
  single target shard. Correct (each handoff is epoch/CAS-fenced) but a synchronized burst at the ~1-2k/box
  ceiling is a real load spike on the target. Add a per-target semaphore (~32 in flight) or pace the
  drain-emit before leaning on this under a production rolling redeploy. · *world/distributed*
- **Drain reclaim = clean-disconnect + honest metric (Phase 16.4b).** A straggler still resident at the
  BeginDrain deadline (its handoff failed / it never re-dialed) is currently only FLUSHED (Drain) and counted
  in DrainResult.Reclaimed; the shard exit then drops its socket. That's the intended honest drop, but it is
  not yet an explicit clean-disconnect frame nor split into infra-fault vs client-fault buckets (the 16.3
  review's `drain_reclaimed` labelling). Emit OTel drain_redirected/drain_reclaimed counters + send a
  "server restarting, reconnect" disconnect to stragglers. · *world/obs*
- **Director-owned drain target selection (Phase 16.4b, decentralized default shipped).** BeginDrain takes an
  injectable TargetChooser; the hermetic tests + single-box use a fixed/self-select peer. The DS review wants
  the DIRECTOR (Phase 10.6 leader) to own selection + SERIALIZE simultaneous drains (avoid two shards draining
  onto one target past its one-core zone ceiling, and split-brain during a fleet rollout). Wire a
  director-driven chooser (load-aware + admission-capped) for multi-shard rollouts; the decentralized chooser
  stays the standalone/dev fallback. · *orchestration/distributed*
- **Runtime-hosted zone scope-replica registration (Phase 16.4a defer).** A zone brought up via HostZone is
  NOT added to the scope replication's zoneRegion map (that's built once in WithScopeBus, pre-Run), so a
  region/world scope delta won't route to a runtime-adopted zone until re-registered. Wire HostZone (or the
  drain coordinator that owns the placement flip) to register the new zone with `scopeReplication`. · *world/orchestration*
