package gate

import (
	"context"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
)

// devauth_bind_test.go — the TELOS_DEV_AUTOAUTH bind guard: the no-OAuth dev bypass must refuse to bind any
// non-loopback (off-host reachable) address, so an accidental prod-exposed backdoor is impossible, not just
// discouraged.

func TestIsLoopbackListen(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:4000":          true,
		"localhost:4000":          true,
		"[::1]:4000":              true,
		"127.0.0.5:4000":          true,  // all of 127.0.0.0/8 is loopback
		":4000":                   false, // empty host → all interfaces
		"0.0.0.0:4000":            false,
		"192.168.1.10:4000":       false,
		"example.com:4000":        false, // a non-localhost hostname is fail-closed (not resolved here)
		"[::]:4000":               false, // unspecified v6 → all interfaces
		"[::ffff:0.0.0.0]:4000":   false, // IPv4-mapped wildcard → decodes to 0.0.0.0, NOT loopback
		"[::ffff:127.0.0.1]:4000": true,  // IPv4-mapped loopback → genuinely loopback
		"LOCALHOST:4000":          false, // case-sensitive: only lowercase "localhost" is trusted
		"garbage":                 false, // unparseable → not safe
	}
	for addr, want := range cases {
		if got := isLoopbackListen(addr); got != want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", addr, got, want)
		}
	}
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

// TestDevAutoAuthOffNoBindRestriction: without the bypass, a non-loopback bind is fine (the guard only
// applies when the OAuth bypass is active).
func TestDevAutoAuthOffAllowsNonLoopback(t *testing.T) {
	srv := New("0.0.0.0:0", directory.Static{Addr: "addr"}, commbus.OpenGate("", nil)).
		WithTransports(true, "", "", "") // bypass OFF (default)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := srv.ListenAndServe(ctx); err != nil {
		t.Fatalf("non-loopback bind with the bypass OFF should be allowed, got: %v", err)
	}
}
