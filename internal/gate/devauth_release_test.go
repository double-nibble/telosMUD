//go:build !telos_devauth

package gate

import (
	"context"
	"testing"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
)

// devauth_release_test.go — the #96 ACCEPTANCE tests for a DEFAULT (release) build: the TELOS_DEV_AUTOAUTH
// bypass is compiled out, so no call and no config can turn it on. These run under `go test ./...` (no tag)
// exactly as production is built; the dev-tagged counterpart (devauth_dev_test.go) covers the engaged bypass.

// TestDevAutoAuthCompiledOutInRelease is the core guarantee: even when a caller (a stale prod config, a
// compromised orchestrator) invokes WithDevAutoAuth(true), the bypass does NOT engage — devAuthActive() stays
// false, so an account-configured gate keeps enforcing OAuth. This is the release build's contract.
func TestDevAutoAuthCompiledOutInRelease(t *testing.T) {
	srv := New(":4000", directory.Static{Addr: "addr"}, commbus.OpenGate("", nil)).
		WithAccountClient(&stubAccountClient{}). // account service configured
		WithDevAutoAuth(true)                    // ...and someone tries to flip the bypass

	if srv.devAuthActive() {
		t.Fatal("release build (no -tags telos_devauth) must NOT activate the dev-autoauth bypass; " +
			"the OAuth bypass must be physically absent (#96)")
	}
}

// TestDevAutoAuthBindGuardInertInRelease: since the bypass can never engage in a release build, the loopback
// bind guard is dead code — a non-loopback bind is NOT refused even after WithDevAutoAuth(true). (The guard
// only ever runs in a dev-tagged build, where the bypass is real; see devauth_dev_test.go.)
func TestDevAutoAuthBindGuardInertInRelease(t *testing.T) {
	srv := New("0.0.0.0:0", directory.Static{Addr: "addr"}, commbus.OpenGate("", nil)).
		WithTransports(true, "", "", "").
		WithDevAutoAuth(true) // no-op in release

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := srv.ListenAndServe(ctx); err != nil {
		t.Fatalf("release build: the dev-autoauth bind guard must be inert (bypass absent), got: %v", err)
	}
}
