# tests/integration

Integration tests live here, **all under this single directory** (per the project TEST STANDARD).

These exercise real seams between components — most commonly against a real backing
service (Postgres / Redis / NATS) brought up by `make deps`. They are **gated** so the
default hermetic `go test ./...` never requires docker or a live database:

- The Postgres-backed tests skip themselves when `TELOS_TEST_DSN` is unset
  (`t.Skip(...)`), so `go test ./...` passes with no database.
- Run them with `make test-integration` (which sets `TELOS_TEST_DSN` for you and
  expects `make deps` to be up), or in CI where a Postgres service is stood up.

Conventions (TEST STANDARD):
- Prefer **table-driven** tests.
- Prefer **interfaces + dependency injection** so units test in isolation.
- Use **`github.com/stretchr/testify`** (`require` for fatal preconditions, `assert`
  for soft checks).
- Black-box where practical: these tests use package `integration` and import the
  code under test through its **exported** API. A test that genuinely needs
  unexported internals stays co-located with its target as a unit test instead.

See `docs/TESTING.md` for the full standard.
