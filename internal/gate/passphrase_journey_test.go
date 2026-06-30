package gate

import (
	"context"
	"testing"

	"github.com/double-nibble/telosmud/internal/directory"
)

// passphrase_journey_test.go — Phase 14.5: the `connect <name> <passphrase>` login through the gate. A wrong
// passphrase re-prompts (no drop); the right one spawns the named character.

// passphraseFakeAccount accepts exactly one name+passphrase pair.
type passphraseFakeAccount struct {
	name string
	pass string
}

func (passphraseFakeAccount) ListCharacters(context.Context, string) ([]CharacterInfo, error) {
	return nil, nil
}

func (passphraseFakeAccount) RedeemLinkCode(context.Context, string, string) (string, []CharacterInfo, bool, error) {
	return "", nil, false, nil
}

func (passphraseFakeAccount) IssueSessionAssertion(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (f passphraseFakeAccount) VerifyPassphrase(_ context.Context, name, pass, _ string) (bool, string, string, error) {
	if name == f.name && pass == f.pass {
		return true, "acct-1", "", nil
	}
	return false, "", "bad_credentials", nil
}

func (passphraseFakeAccount) Close() error { return nil }

func TestPassphraseLoginJourney(t *testing.T) {
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(passphraseFakeAccount{name: "Mage", pass: "secret"})

	term := h.dial(t)
	term.expect(t, "connect <name> <passphrase>") // the prompt advertises the passphrase option

	// A wrong passphrase is refused and re-prompts (the connection survives).
	term.send(t, "connect Mage wrong")
	term.expect(t, "Incorrect name or passphrase")

	// The correct passphrase logs in as that character and spawns into the world.
	term.send(t, "connect Mage secret")
	term.expect(t, "passphrase is visible on an unencrypted connection") // the cleartext warning
	term.expect(t, "The Temple Square")

	term.close(t)
}
