package account

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/assertion"
)

// assertion_test.go — Phase 14.3: IssueSessionAssertion signs a token the matching public key verifies; with
// no signing key it returns an empty token (auth disabled).

func TestIssueSessionAssertion(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	svc := newTestService(newFakeStore()).WithSigningKey(priv)
	svc.now = func() time.Time { return time.Unix(1000, 0) } // pin the clock

	resp, err := svc.IssueSessionAssertion(context.Background(), &accountv1.IssueSessionAssertionRequest{
		AccountId: "acct-1", CharacterId: "Aragorn", SessionId: "sess-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := assertion.Verify(pub, resp.GetAssertion(), time.Unix(1001, 0))
	if err != nil {
		t.Fatalf("the issued token should verify: %v", err)
	}
	if claims.Account != "acct-1" || claims.Character != "Aragorn" || claims.Session != "sess-9" {
		t.Fatalf("claims = %+v, want acct-1/Aragorn/sess-9", claims)
	}
	if claims.Expires != time.Unix(1000, 0).Add(assertionTTL).Unix() {
		t.Fatalf("exp = %d, want now+ttl", claims.Expires)
	}
}

func TestIssueSessionAssertionDisabledWithoutKey(t *testing.T) {
	svc := newTestService(newFakeStore()) // no WithSigningKey
	resp, err := svc.IssueSessionAssertion(context.Background(), &accountv1.IssueSessionAssertionRequest{
		AccountId: "a", SessionId: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetAssertion() != "" {
		t.Fatalf("assertion = %q, want empty (signing disabled)", resp.GetAssertion())
	}
}
