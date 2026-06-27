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

## 3. Possible latent bugs (also surfaced as chips)

- **`gate.go:256` `resumeSeq` accepted but never read** — possible session-resume /
  reconnect frame-replay not plumbed. Investigate + plumb or drop. · *gate* · chip `task_44fcce5f`
- **`combat_test.go:448` empty `if z.move(...)`** — a combat test that may not assert
  the move outcome it intends. Make it assert. · *combat* · chip `task_0db5e6e9`

## 4. Housekeeping

- Delete merged local branches as work lands (e.g. `test-standard-structure`).
