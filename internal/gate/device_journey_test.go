package gate

import (
	"context"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/directory"
)

// device_journey_test.go — Phase 15.3 BLACK-BOX onboarding via the terminal OAuth device flow, driven through
// the gate harness exactly as a player experiences it: connect, get a one-click link, and once the browser
// completes OAuth (the fake flips to "authed" after a couple of polls) the character spawns into the world.

// deviceFakeAccount runs the device flow: StartDeviceAuth hands out a link; PollDeviceAuth is "pending" until
// the pollsUntilAuth-th call, then "authed" with the account's characters. It embeds stubAccountClient for the
// other (unused) AccountClient methods.
type deviceFakeAccount struct {
	stubAccountClient
	pollsUntilAuth int
	polls          int
	chars          []CharacterInfo
}

func (f *deviceFakeAccount) StartDeviceAuth(context.Context, string) (string, string, time.Duration, error) {
	return "DEV123", "http://localhost:8080/login/DEV123", 5 * time.Millisecond, nil
}

func (f *deviceFakeAccount) PollDeviceAuth(context.Context, string) (string, string, []CharacterInfo, error) {
	f.polls++
	if f.polls < f.pollsUntilAuth {
		return "pending", "", nil, nil
	}
	return "authed", "acct-1", f.chars, nil
}

func TestDeviceLoginJourney(t *testing.T) {
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(&deviceFakeAccount{pollsUntilAuth: 2, chars: []CharacterInfo{{ID: "c1", Name: "Wanderer"}}})

	term := h.dial(t)

	// The gate greets, then shows the one-click sign-in link (not a name/code prompt).
	term.expect(t, "Welcome to TelosMUD.")
	term.expect(t, "To sign in, open this link")
	term.expect(t, "/login/DEV123")

	// Once the browser completes OAuth (the fake authes on the 2nd poll), the character menu appears; picking
	// the existing character spawns it.
	term.expect(t, "Choose a character:")
	term.expect(t, "1) Wanderer")
	term.send(t, "1")
	term.expect(t, "The Temple Square")
	term.expect(t, "A broad plaza of worn flagstones")

	term.close(t)
}

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
