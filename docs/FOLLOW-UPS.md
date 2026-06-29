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
- **Mail inbox cap / retention / `ListMail` LIMIT** — `internal/store/mail.go` + `internal/world/mailcmds.go`
  (8.7): mail send is rate-limited PER-SENDER, but nothing bounds a RECIPIENT's total inbox — N senders
  (or one attacker's several characters) can grow a victim's inbox without bound, and `ListMail` does an
  unbounded `SELECT`/render of the whole inbox each `mail`. Integrity/confidentiality are sound (the
  `WHERE to_player` scope holds); this is a griefing/storage vector (security MEDIUM, both reviewers
  deferred-with-record). Add: a per-recipient inbox cap on `SendMail` (reject or evict-oldest past a
  ceiling), a `LIMIT`/paging on `ListMail` (bound the query+render; the position-by-OFFSET addressing
  must page with it), and/or a read-mail retention sweep. Also reaps the directory-error dead-letter rows.
  (security/persistence MEDIUM, deferred) · *persistence/security*
- **`ClearPlayer` deferred coupling** — `cmd/telos-gate/main.go:93,108`: reconnect
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
- **No content verb/op grants a flag.** The restricted `guild` channel gates on a `guildmember`
  flag, but no content-authored action can SET that flag on a player — it must be seeded or set
  out-of-band, so a "join the guild" interaction can't be authored as pure content. Needs a
  flag-grant/revoke effect-op (with the obvious authz: a content action can only grant flags the
  pack declares it owns), closing the loop on flag-gated content. · *abilities/world*

### Comms chaos coverage follow-ups (W8, from the distsys review of comms_chaos_test.go)

- **End-to-end durable-tell redelivery test (Consume-driven).** `TestDurableTellNotLostOnEmitFailure`
  pins the cursor-after-emit ORDERING inside `deliverDrainedTell` but re-presents the message by a
  direct `tellDeliverMsg` post — it bypasses the live `Consume`→`deliverBounded` bounded-retry loop. Add
  a test that drives a durable tell through the real consumer with the bus failing the first 1–2 attempts
  then recovering (within `maxDeliver`), asserting the tell renders exactly once + the cursor lands at 1
  — the actual end-to-end never-lost guarantee. · *distsys/test*
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

## 5. Housekeeping

- Delete merged local branches as work lands (e.g. `test-standard-structure`).
