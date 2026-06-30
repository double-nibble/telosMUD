package gate

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/assertion"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/world"
)

// assertion_journey_test.go — Phase 14.3 END-TO-END session assertions: the gate issues a signed token at
// login and carries it in Attach; a world shard configured with the matching public key verifies it OFFLINE
// and accepts (or rejects a token signed by the wrong key). This drives the whole chain through the harness.

// signingFakeAccount authes a device login and issues REAL Ed25519-signed assertions with priv. (The session
// assertion is issued after login regardless of the login method, so it now rides the device flow.)
type signingFakeAccount struct {
	stubAccountClient
	char string
	priv ed25519.PrivateKey
}

func (f *signingFakeAccount) StartDeviceAuth(context.Context, string) (string, string, time.Duration, error) {
	return "DEV", "http://localhost:8080/login/DEV", 5 * time.Millisecond, nil
}

func (f *signingFakeAccount) PollDeviceAuth(context.Context, string) (string, string, []CharacterInfo, error) {
	return "authed", "acct-1", []CharacterInfo{{ID: "c1", Name: f.char}}, nil
}

func (f *signingFakeAccount) IssueSessionAssertion(_ context.Context, account, character, session string) (string, error) {
	return assertion.Sign(f.priv, assertion.Claims{
		Account: account, Character: character, Session: session,
		Expires: time.Now().Add(time.Minute).Unix(),
	})
}

// TestSessionAssertionAcceptedByWorld: a valid signed assertion is verified by the shard and the player
// spawns — the full gate-issues / world-verifies chain with auth ON.
func TestSessionAssertionAcceptedByWorld(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	const addr = "addr-a"
	h := newHarness(t)
	sh := world.NewShard("midgaard", addr, nil, nil).WithVerifyKey(pub) // the shard ENFORCES assertions
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(&signingFakeAccount{char: "Verified", priv: priv})

	term := h.dial(t)
	term.expect(t, "To sign in, open this link")
	term.expect(t, "Choose a character:")
	term.send(t, "1")
	term.expect(t, "The Temple Square") // the world verified the assertion + spawned the player
	term.close(t)
}

// TestSessionAssertionRejectedByWorld: a token signed by the WRONG key fails the shard's verification, so the
// attach is refused and the connection drops without the player ever spawning.
func TestSessionAssertionRejectedByWorld(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(nil) // the shard's verify key
	if err != nil {
		t.Fatal(err)
	}
	_, wrongPriv, err := ed25519.GenerateKey(nil) // a DIFFERENT signing key — the gate signs with this
	if err != nil {
		t.Fatal(err)
	}
	const addr = "addr-a"
	h := newHarness(t)
	sh := world.NewShard("midgaard", addr, nil, nil).WithVerifyKey(pub)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(&signingFakeAccount{char: "Forger", priv: wrongPriv})

	term := h.dial(t)
	term.expect(t, "To sign in, open this link")
	term.expect(t, "Choose a character:")
	term.send(t, "1")

	// The forged assertion is rejected at the world's Attach; the gate's stream fails and the connection
	// closes WITHOUT the player ever reaching the world.
	select {
	case <-term.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("expected the connection to close on a rejected assertion; got %q", term.acc.String())
	}
}
