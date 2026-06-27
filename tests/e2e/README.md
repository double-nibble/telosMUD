# tests/e2e

End-to-end, **black-box acceptance tests** verifying external use cases: start the
stack, connect a real telnet client, run commands, assert the player-visible output
(per the project TEST STANDARD — see `docs/TESTING.md`). These map to the ROADMAP
"Done when:" milestone lines.

## Running

```
make test-e2e          # builds -tags e2e, dials TELOS_E2E_ADDR (default localhost:4000)
```

Needs the full stack up (`make up`). The tier is **double-gated** so the hermetic
`go test ./...` never runs it:

1. the **`e2e` build tag** keeps these files out of the default build (the package has
   no test files without `-tags e2e`);
2. `helpers.E2EAddr(t)` **SKIPs** when the gate is not reachable — a dev with the stack
   down sees a clean skip, never a failure.

CI's `e2e` job brings the stack up and runs `make test-e2e`.

## Tests

### `combat_death_test.go` — room rendering + the combat death sequence

A fresh, uniquely-named character (not the persisted `kurt`) spawns at
`midgaard:room:temple` and walks **temple -> market -> grove -> hollow**. The
market -> grove step **crosses a shard boundary** (midgaard shard-a -> darkwood
shard-b), so the journey also exercises the cross-shard handoff (it polls for each
room name across handoff latency).

In the hollow it asserts the goblin's long-line renders in `look`
(`A wiry goblin bares its teeth, clutching a rusty knife.`) — the committed CI
regression catch for the **lookRoom render gap** (commit 98b69a6): mobs / ground
items / corpses were silently skipped in `look` even though targeting resolved them.
This assertion FAILS if that fix is reverted.

**Precondition — a live goblin.** The demo zones set no `reset_secs`, so a killed mob
does NOT repop until its shard restarts (which re-runs the boot reset). CI runs against
a fresh `make up` stack, so the goblin is always present. Against a long-lived dev stack
where someone already killed it, `docker compose -f deploy/docker-compose.yml restart
world-darkwood` respawns it.

The **death-sequence phase** (kill the goblin -> assert the corpse renders
`the corpse of a small goblin lies here.` -> recover its rusty knife from the corpse)
is written and ready, but **gated on the `TELOS_E2E_KILL` env var**:

> **Why the death phase is gated.** Empirically (verified against the live stack), a
> fresh player cannot reliably kill the hollow goblin in a CI-reasonable time.
> Bare-handed (strength 10 -> str_bonus 0, damroll 0) the player deals ~no damage and
> the goblin never dies. Even after grabbing + wielding the committed Market Square
> steel longsword (2d6 slash), the kill ran **2.5+ minutes with ~35 landed blows** and
> the goblin (85 hp + slash-resist + soak + hp regen) was still alive, while it landed
> 35 hits + 5 crits on the player — a real player-death risk on a long fight. Melee is
> too slow and too variable to gate CI on. The throwaway `nuke` one-shot spell is **not
> committed**, so it is deliberately not used here.
>
> To enable the death phase, set `TELOS_E2E_KILL` to a **deterministic** one-shot kill
> command (e.g. a committed test-only spell/op). The test then issues it, polls for
> `is DEAD!`, and runs the corpse-render + loot assertions.

```
TELOS_E2E_KILL='<one-shot kill command>' make test-e2e
```

## Harness

The shared telnet driver lives in `tests/helpers/` (per the standard — reused across
tiers), not inline here:

- `tests/helpers/telnet.go` — `Dial` opens a connection + background reader (telnet
  IAC stripped); `Send` writes a line; `Expect` / `ExpectFrom` / `ExpectAny` poll the
  received stream for a substring with a deadline.
- `tests/helpers/e2e.go` — `E2EAddr(t)` resolves `TELOS_E2E_ADDR` and SKIPs when the
  gate is unreachable.
