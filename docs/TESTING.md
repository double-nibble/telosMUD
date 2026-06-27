# Testing standard

This is the canonical, binding test standard for TelosMUD. It describes the test
layout, how to run each tier, the gating that keeps the default suite hermetic, and
the conventions (testify, table-driven, dependency injection) every new test follows.

## Conventions

- **Prefer table-driven tests.** One test function, a `[]struct{...}` of cases, a
  `t.Run(tc.name, ...)` loop. New tests follow this; existing tests are converted
  opportunistically, not in a big-bang rewrite.
- **Prefer interfaces + dependency injection** so a unit tests in isolation (inject a
  fake/seam rather than reaching a real dependency).
- **Use `github.com/stretchr/testify`** (`require` for fatal preconditions that should
  stop the test, `assert` for soft checks that should report-and-continue). It is a
  direct dependency in `go.mod`.
- **Deterministic:** seed every RNG, drive timers/pulses explicitly, and poll a
  condition with a deadline instead of `time.Sleep`-and-hope.
- **Every fixed bug earns a regression test** in (or right after) the fix, with a doc
  comment that names the bug/milestone it pins.

### Table-driven + testify example

```go
func TestThing(t *testing.T) {
    cases := []struct {
        name string
        in   int
        want int
    }{
        {"zero", 0, 0},
        {"double", 2, 4},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            got, err := Double(tc.in)
            require.NoError(t, err)        // fatal: stop this case
            assert.Equal(t, tc.want, got)  // soft: report the value
        })
    }
}
```

The conversion exemplar for the standard is
`tests/integration/store_pack_test.go` (black-box package, exported-API only,
testify, table-driven sub-tests).

## Layout (where each kind of test lives)

| Kind | Location | Notes |
| --- | --- | --- |
| **Unit** | co-located, same package (`internal/*/*_test.go`) | The norm (~46 files). Table-driven, deterministic, **no external deps**. Owned by the domain engineers. |
| **Integration** | `tests/integration/` (single dir) | Real seams / real backing services. **Gated** (see below). Black-box where practical (package `integration`, exported API only). |
| **Shared helpers / harnesses** | `tests/helpers/` | Reusable across integration/e2e (DSN-connect, stack-up, wait-for-condition). A **one-off** helper for a single unit test stays co-located instead. |
| **E2E** | `tests/e2e/` | Black-box acceptance: start the stack, connect, run commands, assert output. May be scripts. |
| **Smoke / system** | `tests/smoke/` | "Does the whole stack come up." `smoke.sh` lives here. |
| **Non-Go test scripts** | `tests/scripts/` | Reusable shell fragments / fixtures the smoke/e2e tiers source. |

A test that genuinely needs **unexported internals** of its target cannot live in a
black-box package — it stays **co-located as a unit test**. Example:
`internal/store/store_test.go` keeps `TestCharacterCRUD` (it pokes the unexported
`p.pool` to clean up a row), while the exported-API round-trip / re-import
idempotency tests moved to `tests/integration/`.

## Running each tier

| Command | Runs |
| --- | --- |
| `make test` (`go test ./...`) | The **hermetic** default suite: all unit tests + co-located/integration tests that **skip** without a DSN. Never needs docker or a database. Must stay fast. |
| `make test-race` | `go test -race -count=100 ./...`. |
| `make test-integration` | The **gated** Postgres integration tier (`./tests/integration/... ./internal/store/...`). Sets `TELOS_TEST_DSN` for you; needs `make deps` up. |
| `make smoke` | Full docker stack up once; assert healthy + seed exits 0 + a player can `look`. |
| `make smoke-twice` | Smoke, but bring the stack up TWICE on the same volume — the re-seed/idempotency catch. |
| `make lint` | `golangci-lint run` (the standard; run locally before every commit). |

## Gating: how the default suite stays hermetic

The default `go test ./...` MUST never require docker or a live Postgres. The
integration tier is gated on `TELOS_TEST_DSN`: each gated test calls a skip-helper
first and `t.Skip`s when the env var is unset.

- Use the shared helper `tests/helpers.OpenTestPool(t)` (or `helpers.TestDSN(t)`):
  it skips when `TELOS_TEST_DSN` is unset, migrates the schema, opens a pool, and
  registers cleanup. So a local `go test ./...` with no database passes, while
  `make test-integration` (which exports the DSN) and CI (which stands up a Postgres
  service) actually run them.
- This gate is the layer that was MISSING when the `deletePack`/seed-idempotency bug
  shipped: it only reproduced against **real Postgres** on the **second** import.
  `tests/integration/store_pack_test.go::TestImportPackIdempotent` is its regression.

## Smoke test

`tests/smoke/smoke.sh` brings up the full compose stack (postgres, redis, nats,
migrate, seed, two world shards, gate) and asserts the things a hermetic unit test
cannot see: the one-shot **seed exits 0**, every service is healthy, a telnet client
can connect and `look`, and the cross-shard reconnect journey works. Run it from the
**repo root** (`make smoke` / `make smoke-twice` do this; the compose path is
`deploy/docker-compose.yml`, resolved relative to the working directory).

## golangci-lint

The project runs `golangci-lint run` **locally before committing** and in CI. The
config is `.golangci.yml` (schema v2). Enabled linters (deliberate, documented set):

- **errcheck** — unchecked error returns (the class that lets a real failure pass silently).
- **govet** — go vet's suspicious-construct passes.
- **ineffassign** — assignments to a variable that are never read.
- **staticcheck** — correctness + simplification rule set.
- **unused** — unused constants, vars, funcs, types.
- **misspell** — commonly-misspelled English words.
- **unconvert** — redundant type conversions.
- **gosec** — security-focused static analysis.
- **revive** — golint-successor style + correctness net.
- Formatters: **gofmt** + **gofumpt** (`golangci-lint fmt` applies them).

The CI `lint` job is **blocking**: the codebase is clean against `.golangci.yml`.
A finding is resolved one of three ways — fixed in code, config-excluded (test-only
seeded-RNG / teardown noise), or carrying an explicit `//nolint:<linter> // TODO(owner):
<reason>` for a domain follow-up. Never add a bare `//nolint`: always name the linter
and give a reason.

> **House rule (user's):** if `golangci-lint` returns a *class* of findings that is not
> obviously worth fixing, do not blanket-disable the linter or sweep `nolint` — surface
> the class and decide what to do about it.
