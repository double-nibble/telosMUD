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

## 1. Lint nolint `TODO(owner)` cleanups

The golangci-lint gate is clean + blocking; genuine findings are parked behind
reasoned `//nolint:<linter> // TODO(owner): …`. Resolve each (and remove the
nolint) when the area is next touched, or in an end-of-roadmap lint sweep.

| Item | Location | Owner |
|---|---|---|
| `state_version` CAS int↔uint conversions — add explicit non-negative bound | `internal/store/character.go:58,114,123` | persistence |
| Pulse/cooldown/cast-time `uint64()` conversions — add small-count bounds | `internal/world/ability.go:170,354,358`, `character.go:252`, `pulse_test.go:45` | world |
| `renew*` goroutines use `ctx.Background()` — confirm the right lifetime ctx | `cmd/telos-world/main.go:145,166` | distsys |
| `max` local shadows the builtin in the per-round refresh hot path — rename | `internal/world/combat.go:203` | world |
| `PULSE_VIOLENCE` ALL_CAPS (Diku homage) — decide rename vs keep (touches code+tests+docs) | `internal/world/combat.go:47` | world |
| telnet `writeRaw` mid-protocol writes unchecked — decide if a failed negotiation write drops the session | `internal/telnet/telnet.go:250,252` | edge |
| Config path is operator-supplied — validate/confine (G304); test-file perms (G306) | `internal/config/config.go:77`, `config_test.go:31` | config |
| Test-only `unsafe` pointer-identity helper (G103) | `internal/world/prototype_test.go:32` | world |
| Unused Phase-N placeholders — hot-reload hook, tick-stop helper, containment-query hook, `flags`/`account` stubs | `internal/world/{defs.go:90, affect_runtime.go:187, entity.go:167, components.go:28,130}` | world |
| Journey-test scaffolding (`waitFor`, `echoAbsent`) for tests not yet written | `internal/gate/{harness_test.go:328, persistence_journey_test.go:176}` | test-eng |

## 2. Code tech-debt / design deferrals

- **Room-affect tick cadence** — `affect_room.go:189`: the room tick fires EVERY
  pulse and re-leases the CC to every occupant; should lease at `tickInterval`. (perf/hardening) · *world*
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
- **Retire the redundant resume-seq wire fields (`Play` protocol)** —
  `api/proto/telosmud/play/v1/play.proto:34` (`Attach.input_seq`) and `:143`
  (`Redirect.resume_input_seq`). Both carry a resume point that the *receiving* side
  already derives authoritatively and ignores on the wire: the world ignores
  `Attach.input_seq` (it dedups by its own `appliedSeq`, seeded from the handoff
  snapshot), and the gate ignores `Redirect.resume_input_seq` (it replays from the
  destination's `ServerFrame.ack_input_seq` on the `Attached` frame — see
  `internal/gate/gate.go` `runStream`/`doReplay`). The Go-side dead `resumeSeq` param is
  already gone (§3); what remains is the *protocol-level* cleanup.

  **Deferred deliberately — this touches the gate↔world contract** (`play.proto`,
  PROTOCOL.md §1, the handoff path) and is a coordinated wire change, not a local edit.
  Options when next touched: (a) **delete both fields** and lean entirely on the
  receiver-authoritative `ack_input_seq` (simplest; the cleaner end state), or (b) **keep
  them but reclassify as diagnostics-only** in the proto comments (a cheap sanity/observability
  signal — source's *claimed* resume point vs. what the destination actually acked).
  Recommend (a) unless we find a use for the cross-check. Do this in an end-of-roadmap
  protocol sweep or whenever `play.proto` is next revised. · *edge/distsys*

## 3. Possible latent bugs (also surfaced as chips)

- ~~**`gate.go:256` `resumeSeq` accepted but never read** — possible session-resume /
  reconnect frame-replay not plumbed. Investigate + plumb or drop.~~ · *gate* · chip
  `task_44fcce5f` — **RESOLVED:** investigated; resume IS wired end to end, just not via
  this param. The destination shard is authoritative — it reports its applied high-water
  mark on the `Attached` frame's `ack_input_seq`, which drives `doReplay`. The redirect's
  `resume_input_seq` was a redundant source-side estimate. Dropped the dead `runStream`
  param + its nolint (kept `redirectTarget.resumeSeq` for the diagnostic log). The
  remaining *wire-level* redundancy is now tracked in §2 below.
- **`combat_test.go:448` empty `if z.move(...)`** — a combat test that may not assert
  the move outcome it intends. Make it assert. · *combat* · chip `task_0db5e6e9`

## 4. Deferred features / design directions

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

## 5. Housekeeping

- Delete merged local branches as work lands (e.g. `test-standard-structure`).
