# tests/scripts

Non-Go helper **scripts** for tests (per the project TEST STANDARD): reusable shell
fragments, fixtures, or data-prep scripts shared by the smoke / e2e tiers.

A self-contained test script that IS a smoke or e2e test belongs in `tests/smoke/`
or `tests/e2e/` respectively; this directory is for **reusable** non-Go fragments
those tests source or call.

See `docs/TESTING.md` for the full standard.
