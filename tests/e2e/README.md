# tests/e2e

End-to-end, **black-box acceptance tests** verifying external use cases: start the
stack, connect a client, run commands, assert the output (per the project TEST
STANDARD). These map to the ROADMAP "Done when:" milestone lines.

E2E tests **may be scripts** as well as Go tests. The whole-stack player-journey
asserts currently live inside the smoke harness (`tests/smoke/smoke.sh`, e.g. the
cross-shard reconnect guard); pure black-box acceptance journeys that do not need
the smoke bring-up belong here.

See `docs/TESTING.md` for the full standard.
