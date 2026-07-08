//go:build telos_devauth

package gate

import (
	"context"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
)

// devauth_dev_test.go — the tests that require the TELOS_DEV_AUTOAUTH bypass to actually ENGAGE. They compile
// only under `-tags telos_devauth`, because in a release build WithDevAutoAuth is a hard-refuse no-op and
// devAuthActive() is a constant false (#96), so the bypass — and its bind guard — are absent. CI runs this
// tier via `make test-devauth`; the release counterpart (devauth_release_test.go) asserts the bypass stays
// off in a default build.

// TestDevAutoAuthBypassesOAuth (Phase 15.6): with TELOS_DEV_AUTOAUTH on, an account-backed gate accepts the
// bare name login instead of the browser OAuth flow — the headless smoke/e2e path.
func TestDevAutoAuthBypassesOAuth(t *testing.T) {
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(&chargenFakeAccount{}) // account-configured...
	h.srv.WithDevAutoAuth(true)                    // ...but the dev bypass uses the name login

	term := h.dial(t)
	term.expect(t, "By what name shall you be known?") // NOT the OAuth sign-in link
	term.send(t, "Tester")
	term.expect(t, "The Temple Square") // spawns straight in, no browser
	term.close(t)
}

// TestDevAutoAuthRefusesNonLoopbackBind: with the bypass on, ListenAndServe returns a clean startup error
// (before binding) for a non-loopback plain-telnet address. The refusal path returns immediately, so no ctx
// juggling is needed.
func TestDevAutoAuthRefusesNonLoopbackBind(t *testing.T) {
	srv := New(":4000", directory.Static{Addr: "addr"}, commbus.OpenGate("", nil)).
		WithTransports(true, "", "", ""). // plain telnet on, binding ":4000" (all interfaces)
		WithDevAutoAuth(true)

	err := srv.ListenAndServe(context.Background())
	if err == nil {
		t.Fatal("ListenAndServe should have refused a non-loopback bind under TELOS_DEV_AUTOAUTH")
	}
	if !strings.Contains(err.Error(), "TELOS_DEV_AUTOAUTH") {
		t.Fatalf("refusal error should name the bypass; got: %v", err)
	}
}

// TestDevAutoAuthAllowsLoopbackBind: the same bypass with a loopback bind passes the guard (it then blocks on
// the accept loop until ctx cancels — we cancel immediately and require no guard error surfaced).
func TestDevAutoAuthAllowsLoopbackBind(t *testing.T) {
	srv := New("127.0.0.1:0", directory.Static{Addr: "addr"}, commbus.OpenGate("", nil)).
		WithTransports(true, "", "", "").
		WithDevAutoAuth(true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // unblock the accept loop right away; the guard must have already passed

	if err := srv.ListenAndServe(ctx); err != nil {
		t.Fatalf("loopback bind under the bypass should be allowed, got: %v", err)
	}
}

// TestDevAutoAuthAllowRemoteBindEscape: the explicit acknowledgment (for sandboxed orchestration where
// exposure is controlled by a container's port publish) lets a dev-autoauth gate bind a non-loopback
// address without the refusal — the compose smoke stack relies on this.
func TestDevAutoAuthAllowRemoteBindEscape(t *testing.T) {
	srv := New("0.0.0.0:0", directory.Static{Addr: "addr"}, commbus.OpenGate("", nil)).
		WithTransports(true, "", "", "").
		WithDevAutoAuth(true).
		WithDevAutoAuthAllowRemoteBind(true) // the deliberate sandbox ack

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := srv.ListenAndServe(ctx); err != nil {
		t.Fatalf("ALLOW_REMOTE_BIND ack should permit a non-loopback bind under the bypass, got: %v", err)
	}
}
