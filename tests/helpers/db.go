// Package helpers holds Go test helpers and harnesses shared across the
// integration and e2e tiers (per the project TEST STANDARD, see docs/TESTING.md).
package helpers

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/db"
	"github.com/double-nibble/telosmud/internal/store"
	"github.com/stretchr/testify/require"
)

// TestDSN returns the gated Postgres DSN from TELOS_TEST_DSN, or skips the test
// when it is unset. The default hermetic `go test ./...` runs with no database,
// so a gated integration test calls this first and is a no-op there; CI (or a dev
// who exports the DSN, as `make test-integration` does) actually runs it.
func TestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TELOS_TEST_DSN")
	if dsn == "" {
		t.Skip("TELOS_TEST_DSN not set; skipping Postgres integration test")
	}
	return dsn
}

// OpenTestPool skips when TELOS_TEST_DSN is unset, migrates the schema (idempotent —
// goose tracks applied versions), opens a live pool, and registers its Close with
// t.Cleanup. Every gated integration test starts from this so the schema is present
// and each test cleans up after itself.
func OpenTestPool(t *testing.T) *store.Pool {
	t.Helper()
	dsn := TestDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, db.Migrate(ctx, dsn), "migrate test db")
	p, err := store.Open(ctx, dsn)
	require.NoError(t, err, "open test db")
	t.Cleanup(p.Close)
	return p
}
