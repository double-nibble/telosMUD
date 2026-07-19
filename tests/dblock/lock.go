// Package dblock holds the cross-package Postgres advisory lock the gated test tiers use to serialize
// writes to global, singleton database state.
//
// It is a LEAF package on purpose: tests/helpers imports internal/store, so internal/store's own tests
// cannot import helpers without a cycle — and internal/store is precisely where several of the tests that
// mutate the content-version singleton live. Keeping the lock free of any project dependency lets every
// tier use the same one, which is the only way it actually serializes anything.
package dblock

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// contentRegistryLockKey is the advisory-lock key that serializes tests mutating the CONTENT VERSION
// SINGLETON. Arbitrary but fixed; it only has to be unique among advisory locks this project takes.
const contentRegistryLockKey int64 = 0x7e105_c0117 //nolint:revive // a stable, deliberately odd sentinel

// LockContentRegistry serializes a test against every other test that writes the content-version
// singleton, and releases the lock when the test ends.
//
// # Why this is necessary rather than defensive
//
// `content_version` is ONE row and `content_pack_registry` is wholly REPLACED by every ImportVersion — so
// the "installed content version" is global mutable state shared by the entire database. Go runs different
// PACKAGES in parallel, and `make test-integration` / CI's integration job both run
// `./tests/integration/... ./internal/store/...` in one invocation against ONE Postgres. So a registry
// write in one package can land between another package's setup and assertion.
//
// That is not hypothetical: it turned a prune assertion red in CI while passing locally and on the
// preceding PR, because the failure depends on interleaving rather than on anything in the diff. Any test
// that calls ImportVersion (or otherwise depends on the registry's contents) must hold this lock.
//
// The lock is taken on a DEDICATED connection, not from the store pool: a pooled connection can be handed
// to another caller between statements, and a session-scoped advisory lock released by whoever happens to
// hold that connection would silently stop serializing anything.
func LockContentRegistry(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("TELOS_TEST_DSN")
	if dsn == "" {
		t.Skip("TELOS_TEST_DSN not set; skipping Postgres integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err, "connect for the content-registry advisory lock")
	_, err = conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, contentRegistryLockKey)
	require.NoError(t, err, "take the content-registry advisory lock")
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		// Closing the connection releases a session-scoped advisory lock even if the unlock fails.
		_, _ = conn.Exec(cctx, `SELECT pg_advisory_unlock($1)`, contentRegistryLockKey)
		_ = conn.Close(cctx)
	})
}
