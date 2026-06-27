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

The **death-sequence phase runs by default**: the fresh player melees the goblin with
plain `kill goblin` (no weapon, no special verb), polls for `is DEAD!`, then asserts the
corpse renders (`the corpse of a small goblin lies here.`), the loot is visible
(`look corpse` lists `a rusty knife`), and the knife is recoverable
(`get knife from corpse`). Starter combat is tuned so a fresh unarmed player reliably
wins: unarmed swings deal real damage (content `unarmed_dice` 1d6) and passive regen
pauses in combat, so the goblin (15 hp, no soak) dies in ~6 rounds (median; 3-13 over
60 seeds, zero player deaths). Measured live (5 pristine kills): 4-10 rounds in
~10-25s, ~2.5-3s/round (PULSE_VIOLENCE 10 x 250ms + handler overhead); the full e2e
ran 6/6 green at 11-24s wall-clock. The death poll caps generously at 90s.

`TELOS_E2E_KILL` is an OPTIONAL override — set it to a faster one-shot kill verb for
local speed. The committed/CI path runs the real melee kill with no special env.

```
make test-e2e                              # default: real melee kill
TELOS_E2E_KILL='<one-shot verb>' make test-e2e   # optional: fast override
```

**Precondition — a live goblin.** The demo zones set `reset_secs: 90`, so a killed
goblin repops within ~90s. CI runs against a fresh `make up` stack (goblin always
present). A fast-repeated LOCAL run can race the not-yet-repopped goblin, so space
reruns by the repop stride (~90s) or `docker compose -f deploy/docker-compose.yml
restart world-darkwood` to force a clean repop between runs. The render assertion (above)
IS the live-goblin precondition: it fails fast with a clear message if the goblin has not
repopped, rather than entering combat against a corpse.

## Harness

The shared telnet driver lives in `tests/helpers/` (per the standard — reused across
tiers), not inline here:

- `tests/helpers/telnet.go` — `Dial` opens a connection + background reader (telnet
  IAC stripped); `Send` writes a line; `Expect` / `ExpectFrom` / `ExpectAny` poll the
  received stream for a substring with a deadline.
- `tests/helpers/e2e.go` — `E2EAddr(t)` resolves `TELOS_E2E_ADDR` and SKIPs when the
  gate is unreachable.
