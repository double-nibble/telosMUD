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

// signingFakeAccount redeems one code and issues REAL Ed25519-signed assertions with priv.
type signingFakeAccount struct {
	good string
	char string
	priv ed25519.PrivateKey
}

func (f *signingFakeAccount) ListCharacters(context.Context, string) ([]CharacterInfo, error) {
	return nil, nil
}

func (f *signingFakeAccount) RedeemLinkCode(_ context.Context, code, _ string) (string, []CharacterInfo, bool, error) {
	if code == f.good {
		return "acct-1", []CharacterInfo{{ID: "c1", Name: f.char}}, true, nil
	}
	return "", nil, false, nil
}

func (f *signingFakeAccount) IssueSessionAssertion(_ context.Context, account, character, session string) (string, error) {
	return assertion.Sign(f.priv, assertion.Claims{
		Account: account, Character: character, Session: session,
		Expires: time.Now().Add(time.Minute).Unix(),
	})
}

func (f *signingFakeAccount) Close() error { return nil }

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
	h.srv.WithAccountClient(&signingFakeAccount{good: "GOODCODE", char: "Verified", priv: priv})

	term := h.dial(t)
	term.expect(t, "Enter your link code")
	term.send(t, "GOODCODE")
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
	h.srv.WithAccountClient(&signingFakeAccount{good: "GOODCODE", char: "Forger", priv: wrongPriv})

	term := h.dial(t)
	term.expect(t, "Enter your link code")
	term.send(t, "GOODCODE")

	// The forged assertion is rejected at the world's Attach; the gate's stream fails and the connection
	// closes WITHOUT the player ever reaching the world.
	select {
	case <-term.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("expected the connection to close on a rejected assertion; got %q", term.acc.String())
	}
}
