package gate

import (
	"context"
	"testing"

	"github.com/double-nibble/telosmud/internal/directory"
)

// linkcode_journey_test.go — Phase 14.2 BLACK-BOX onboarding via a LINK CODE, driven through the gate
// harness exactly as a player redeeming a code from the website experiences it. When an account service is
// wired the gate prompts for a code instead of a name; a bad code re-prompts, a good one selects the
// account's character and spawns into the world.

// fakeGateAccount is a stub AccountClient for the gate tests: it redeems exactly one code to a fixed
// character set, everything else misses.
type fakeGateAccount struct {
	good  string
	chars []CharacterInfo
}

func (f *fakeGateAccount) ListCharacters(context.Context, string) ([]CharacterInfo, error) {
	return f.chars, nil
}

func (f *fakeGateAccount) RedeemLinkCode(_ context.Context, code, _ string) (string, []CharacterInfo, bool, error) {
	if code == f.good {
		return "acct-1", f.chars, true, nil
	}
	return "", nil, false, nil
}

func (f *fakeGateAccount) IssueSessionAssertion(context.Context, string, string, string) (string, error) {
	return "", nil // the journey tests run with auth off (no verify key on the shard)
}

func (f *fakeGateAccount) Close() error { return nil }

// TestLinkCodeOnboardingJourney: with an account service wired, the gate prompts for a link code; a bad code
// re-prompts (no drop), and a good code with a single character spawns into the world.
func TestLinkCodeOnboardingJourney(t *testing.T) {
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(&fakeGateAccount{good: "GOODCODE", chars: []CharacterInfo{{ID: "c1", Name: "Linker"}}})

	term := h.dial(t)

	// The gate greets and asks for a LINK CODE (not a name) once an account service is configured.
	term.expect(t, "Welcome to TelosMUD.")
	term.expect(t, "Enter your link code")

	// A bad code re-prompts with a player-visible reason; the connection survives.
	term.send(t, "BADCODE1")
	term.expect(t, "invalid or has expired")

	// A good code redeems, selects the single character, and spawns into the home start room.
	term.send(t, "GOODCODE")
	term.expect(t, "The Temple Square")
	term.expect(t, "A broad plaza of worn flagstones")

	term.close(t)
}

// TestLinkCodeCharacterSelectMenu: a code that maps to MULTIPLE characters presents a numbered menu, and the
// chosen number spawns that character.
func TestLinkCodeCharacterSelectMenu(t *testing.T) {
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(&fakeGateAccount{
		good:  "MULTICOD",
		chars: []CharacterInfo{{ID: "c1", Name: "Aragorn"}, {ID: "c2", Name: "Legolas"}},
	})

	term := h.dial(t)
	term.expect(t, "Enter your link code")
	term.send(t, "MULTICOD")

	// The menu lists both characters; picking #2 spawns Legolas.
	term.expect(t, "Choose a character:")
	term.expect(t, "1) Aragorn")
	term.expect(t, "2) Legolas")
	term.send(t, "2")
	term.expect(t, "The Temple Square")

	term.close(t)
}

func (f *fakeGateAccount) VerifyPassphrase(_ context.Context, _, _, _ string) (bool, string, string, error) {
	return false, "", "bad_credentials", nil
}

func (f *fakeGateAccount) ResolveSSHKey(context.Context, string) (bool, string, error) {
	return false, "", nil
}
