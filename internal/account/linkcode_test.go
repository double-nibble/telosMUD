package account

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/store"
)

// linkcode_test.go — Phase 14.2: the link-code store (single-use + expiry) and the Mint/Redeem RPCs.

func TestNewLinkCodeFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, err := newLinkCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != linkCodeLen {
			t.Fatalf("code %q length = %d, want %d", code, len(code), linkCodeLen)
		}
		for _, r := range code {
			if !contains(linkCodeAlphabet, r) {
				t.Fatalf("code %q contains out-of-alphabet rune %q", code, r)
			}
		}
		seen[code] = true
	}
	if len(seen) < 190 { // 40 bits of entropy — collisions across 200 draws should be ~none
		t.Fatalf("only %d/200 distinct codes — entropy looks weak", len(seen))
	}
}

func contains(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func TestMemLinkCodeSingleUse(t *testing.T) {
	cs := NewMemLinkCodes()
	ctx := context.Background()

	code, err := cs.Mint(ctx, "acct-1", "char-9", linkCodeTTL)
	if err != nil {
		t.Fatal(err)
	}

	// First redeem returns the binding.
	acct, char, found, err := cs.Redeem(ctx, code)
	if err != nil || !found {
		t.Fatalf("first redeem: found=%v err=%v, want found", found, err)
	}
	if acct != "acct-1" || char != "char-9" {
		t.Fatalf("redeem returned (%q,%q), want (acct-1,char-9)", acct, char)
	}
	// Second redeem of the same code finds nothing (single-use).
	_, _, found, err = cs.Redeem(ctx, code)
	if err != nil || found {
		t.Fatalf("second redeem: found=%v err=%v, want NOT found", found, err)
	}
}

func TestMemLinkCodeExpiry(t *testing.T) {
	cs := NewMemLinkCodes()
	ctx := context.Background()
	code, err := cs.Mint(ctx, "acct-1", "", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, _, found, _ := cs.Redeem(ctx, code); found {
		t.Fatal("an expired code must not redeem")
	}
}

func TestLinkCodeRPCRoundTrip(t *testing.T) {
	fs := newFakeStore()
	fs.chars["acct-1"] = []store.CharacterSummary{{ID: "c1", Name: "Aragorn"}}
	svc := newTestService(fs).WithLinkCodes(NewMemLinkCodes())
	ctx := context.Background()

	mint, err := svc.MintLinkCode(ctx, &accountv1.MintLinkCodeRequest{AccountId: "acct-1"})
	if err != nil {
		t.Fatal(err)
	}
	if mint.GetCode() == "" || mint.GetTtlMs() == 0 {
		t.Fatalf("mint = %+v, want a code + ttl", mint)
	}

	// Redeem returns the account's characters.
	red, err := svc.RedeemLinkCode(ctx, &accountv1.RedeemLinkCodeRequest{Code: mint.GetCode(), ConnInfo: "1.2.3.4"})
	if err != nil {
		t.Fatal(err)
	}
	if red.GetAccountId() != "acct-1" || len(red.GetCharacters()) != 1 || red.GetCharacters()[0].GetName() != "Aragorn" {
		t.Fatalf("redeem = %+v, want acct-1 with Aragorn", red)
	}

	// A second redeem of the consumed code is NotFound.
	if _, err := svc.RedeemLinkCode(ctx, &accountv1.RedeemLinkCodeRequest{Code: mint.GetCode()}); status.Code(err) != codes.NotFound {
		t.Fatalf("second redeem should be NotFound, got %v", err)
	}
	// A never-minted code is NotFound too.
	if _, err := svc.RedeemLinkCode(ctx, &accountv1.RedeemLinkCodeRequest{Code: "ZZZZZZZZ"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown code should be NotFound, got %v", err)
	}
}

func TestLinkCodeRPCUnavailableWithoutStore(t *testing.T) {
	svc := newTestService(newFakeStore()) // no WithLinkCodes
	if _, err := svc.MintLinkCode(context.Background(), &accountv1.MintLinkCodeRequest{AccountId: "a"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("Mint without a code store should be Unavailable, got %v", err)
	}
}
