# tests/smoke

Smoke / system tests (per the project TEST STANDARD): does the whole stack even
come up, and do system-level concerns (migrations, seed idempotency) hold against
the real docker stack.

- `smoke.sh` — brings up the FULL docker compose stack (postgres, redis, nats,
  migrate, seed, two world shards, gate), asserts every service is healthy, the
  one-shot **seed exits 0**, and a telnet client can connect and `look`. Pass
  `--twice` (or `SMOKE_TWICE=1`) to bring the stack up TWICE against the same
  persisted Postgres volume — the seed/idempotency regression reproduction.

Run with `make smoke` (once) or `make smoke-twice` (the re-seed catch). Both
invoke this script **from the repo root** (the compose file path is
`deploy/docker-compose.yml`, resolved relative to the working directory).

See `docs/TESTING.md` for the full standard.
