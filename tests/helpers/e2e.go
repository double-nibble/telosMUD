package helpers

import (
	"net"
	"os"
	"testing"
	"time"
)

// DefaultE2EAddr is the dev `make up` gate address. The e2e tier targets it unless
// TELOS_E2E_ADDR overrides it (e.g. a CI compose network or a non-default port).
const DefaultE2EAddr = "localhost:4000"

// E2EAddr returns the gate address the e2e tier should dial, then SKIPS the test
// when that gate is not reachable — so the hermetic `go test ./...` on a machine
// with the stack DOWN sees a clean skip, never a failure (per docs/TESTING.md).
//
// Gating rules (mirrors the integration tier's TELOS_TEST_DSN gate):
//   - addr comes from TELOS_E2E_ADDR, defaulting to localhost:4000.
//   - if a short TCP dial to addr fails, the stack isn't up: t.Skip.
//   - CI's e2e job brings the stack up first (make up), so the dial succeeds and
//     the test actually runs.
//
// A successful dial here is a liveness probe only; the test opens its own
// TelnetClient (Dial) for the real session.
func E2EAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("TELOS_E2E_ADDR")
	if addr == "" {
		addr = DefaultE2EAddr
	}
	// gosec G704 (SSRF taint) flags addr coming from an env var. This is a TEST helper:
	// addr is the gate address a developer/CI controls (TELOS_E2E_ADDR / make test-e2e),
	// never end-user input, and the dial is a liveness probe that decides skip-vs-run.
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second) //nolint:gosec // test-only liveness probe of a dev/CI-controlled gate addr (not user input)
	if err != nil {
		t.Skipf("e2e gate %q not reachable (%v); bring the stack up (make up) or set TELOS_E2E_ADDR to run the e2e tier", addr, err)
	}
	_ = conn.Close()
	return addr
}
