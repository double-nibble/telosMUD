package gate

import (
	"context"
	"testing"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
)

// devauth_bind_test.go — build-AGNOSTIC parts of the TELOS_DEV_AUTOAUTH bind guard: the loopback classifier
// and the "bypass off ⇒ no bind restriction" case both hold in every build. The bind-guard cases that require
// the bypass to actually engage (WithDevAutoAuth(true) taking effect) live in devauth_dev_test.go behind
// `//go:build telos_devauth`, since the bypass is compiled out of a release build (#96); the release build's
// inert-guard behavior is asserted in devauth_release_test.go.

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

// TestDevAutoAuthOffAllowsNonLoopback: without the bypass, a non-loopback bind is fine (the guard only
// applies when the OAuth bypass is active). Holds in every build.
func TestDevAutoAuthOffAllowsNonLoopback(t *testing.T) {
	srv := New("0.0.0.0:0", directory.Static{Addr: "addr"}, commbus.OpenGate("", nil)).
		WithTransports(true, "", "", "") // bypass OFF (default)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := srv.ListenAndServe(ctx); err != nil {
		t.Fatalf("non-loopback bind with the bypass OFF should be allowed, got: %v", err)
	}
}
