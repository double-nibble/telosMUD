package account

import (
	"context"
	"crypto/ed25519"
	"errors"
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
	// #27: an account with no tier row (the fake) FAILS SAFE to player — an error/absence never elevates.
	if claims.Tier != "player" {
		t.Fatalf("tier = %q, want player (fail-safe default)", claims.Tier)
	}
}

// TestIssueSessionAssertionCarriesTier (#27): the account's stored tier is signed into the assertion, so the
// world can trust it offline.
func TestIssueSessionAssertionCarriesTier(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeStore()
	fs.tiers["acct-admin"] = "admin"
	svc := newTestService(fs).WithSigningKey(priv)

	resp, err := svc.IssueSessionAssertion(context.Background(), &accountv1.IssueSessionAssertionRequest{
		AccountId: "acct-admin", CharacterId: "Gandalf", SessionId: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := assertion.Verify(pub, resp.GetAssertion(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claims.Tier != "admin" {
		t.Fatalf("tier = %q, want admin (signed from the store)", claims.Tier)
	}
}

// TestIssueSessionAssertionManageTiersBit (#369): the resolved manage-tiers capability rides back with the
// assertion so the gate can locally gate staff-verb (promote/demote) visibility. An admin (grants FlagAdmin
// per the default ladder) reports true; a player reports false; an unknown account fails safe to player →
// false. It is resolved even WITHOUT a signing key (a signing-less deployment's gate still needs the bit).
func TestIssueSessionAssertionManageTiersBit(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeStore()
	fs.tiers["acct-admin"] = "admin"
	fs.tiers["acct-player"] = "player"

	cases := []struct {
		acct   string
		signed bool
		want   bool
	}{
		{"acct-admin", true, true},
		{"acct-player", true, false},
		{"acct-admin", false, true}, // no signing key: the bit is still resolved
		{"acct-player", false, false},
		{"acct-missing", true, false}, // unknown account fails safe to player → no manage-tiers
	}
	for _, tc := range cases {
		svc := newTestService(fs)
		if tc.signed {
			svc = svc.WithSigningKey(priv)
		}
		resp, err := svc.IssueSessionAssertion(context.Background(), &accountv1.IssueSessionAssertionRequest{
			AccountId: tc.acct, CharacterId: "C", SessionId: "s",
		})
		if err != nil {
			t.Fatalf("%s signed=%v: %v", tc.acct, tc.signed, err)
		}
		if resp.GetManageTiers() != tc.want {
			t.Errorf("%s signed=%v: ManageTiers=%v, want %v", tc.acct, tc.signed, resp.GetManageTiers(), tc.want)
		}
	}

	// Ladder unavailable → fail-safe false even for a real admin (a promote would be refused anyway, so the
	// verb stays hidden rather than showing to an actor the service can't authoritatively authorize).
	svc := newTestService(fs).WithSigningKey(priv).WithTrustLadderUnavailable()
	resp, err := svc.IssueSessionAssertion(context.Background(), &accountv1.IssueSessionAssertionRequest{
		AccountId: "acct-admin", CharacterId: "C", SessionId: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetManageTiers() {
		t.Error("ManageTiers must be false when the trust ladder is unavailable (fail-safe)")
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

// TestIssueSessionAssertionTierReadErrorFailsSafeToPlayer (#27 fail-safe): when the tier read ERRORS
// (distinct from a missing row, which TestIssueSessionAssertion already covers), the assertion must still
// issue AND carry tier="player". An error must never surface the stored elevated tier, and must never fail
// the whole assertion — the invariant stated at service.go IssueSessionAssertion ("an error must never
// elevate a tier"). Without this, a transient tier-store blip on an admin's login would either 500 the
// login or, worse under a buggy refactor, sign whatever partial value the read returned.
func TestIssueSessionAssertionTierReadErrorFailsSafeToPlayer(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeStore()
	fs.tiers["acct-admin"] = "admin"                  // an elevated tier really exists in the store...
	fs.tierErr = errors.New("tier store unavailable") // ...but the read fails
	svc := newTestService(fs).WithSigningKey(priv)

	resp, err := svc.IssueSessionAssertion(context.Background(), &accountv1.IssueSessionAssertionRequest{
		AccountId: "acct-admin", CharacterId: "Gandalf", SessionId: "s",
	})
	if err != nil {
		t.Fatalf("a tier-read error must not fail the assertion: %v", err)
	}
	claims, err := assertion.Verify(pub, resp.GetAssertion(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claims.Tier != "player" {
		t.Fatalf("tier = %q, want player (a tier-read error must fail safe, never elevate to the stored admin)", claims.Tier)
	}
	// The manage-tiers bit (#369) must fail safe the same way — a read error must never hand the gate a
	// staff-visibility signal derived from the (unread) elevated tier.
	if resp.GetManageTiers() {
		t.Error("ManageTiers must be false on a tier-read error (fail safe, never elevate)")
	}
}
