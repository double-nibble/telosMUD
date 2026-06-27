# tests/helpers

Shared Go test **helpers and harnesses** used across `tests/integration/` and
`tests/e2e/` (per the project TEST STANDARD).

Put a helper here when it is reused by more than one test (e.g. a DSN-connect
helper, a stack-up helper, a wait-for-condition poller, a scripted-telnet client).

**Exception:** a one-off helper for a single unit test stays **co-located** with
that test — do not pull co-located unit-test helpers out into this package.

See `docs/TESTING.md` for the full standard.
