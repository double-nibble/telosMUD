package account

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
)

// passphrase_service_test.go — Phase 14.5: VerifyPassphrase + SetPassphrase, with lockout after repeated
// failures and the no-account-leak behavior.

func TestPassphraseLoginFlow(t *testing.T) {
	fs := newFakeStore()
	fs.nameAccount["Hero"] = "acct-1" // the character Hero belongs to acct-1
	svc := newTestService(fs)
	ctx := context.Background()

	// Set a passphrase via the RPC (the website path).
	if _, err := svc.SetPassphrase(ctx, &accountv1.SetPassphraseRequest{AccountId: "acct-1", Passphrase: "open sesame"}); err != nil {
		t.Fatal(err)
	}

	// Correct passphrase -> ok + the account id.
	r, err := svc.VerifyPassphrase(ctx, &accountv1.VerifyPassphraseRequest{Name: "Hero", Passphrase: "open sesame", ConnInfo: "1.1.1.1"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.GetOk() || r.GetAccountId() != "acct-1" {
		t.Fatalf("correct passphrase = %+v, want ok + acct-1", r)
	}

	// Wrong passphrase -> bad_credentials (never reveals the account exists).
	r, _ = svc.VerifyPassphrase(ctx, &accountv1.VerifyPassphraseRequest{Name: "Hero", Passphrase: "wrong", ConnInfo: "1.1.1.1"})
	if r.GetOk() || r.GetReason() != "bad_credentials" {
		t.Fatalf("wrong passphrase = %+v, want bad_credentials", r)
	}

	// An unknown character name -> the SAME bad_credentials (no account-existence leak).
	r, _ = svc.VerifyPassphrase(ctx, &accountv1.VerifyPassphraseRequest{Name: "Nobody", Passphrase: "x", ConnInfo: "1.1.1.1"})
	if r.GetOk() || r.GetReason() != "bad_credentials" {
		t.Fatalf("unknown name = %+v, want bad_credentials", r)
	}
}

func TestPassphraseLockoutAfterRepeatedFailures(t *testing.T) {
	fs := newFakeStore()
	fs.nameAccount["Target"] = "acct-9"
	svc := newTestService(fs)
	ctx := context.Background()
	_, _ = svc.SetPassphrase(ctx, &accountv1.SetPassphraseRequest{AccountId: "acct-9", Passphrase: "real"})

	// Hammer wrong passphrases until the account locks (passphraseLockAfter consecutive failures).
	for i := 0; i < passphraseLockAfter; i++ {
		_, _ = svc.VerifyPassphrase(ctx, &accountv1.VerifyPassphraseRequest{Name: "Target", Passphrase: "nope", ConnInfo: "2.2.2.2"})
	}
	// Now even the CORRECT passphrase is refused with "locked" + a retry hint.
	r, _ := svc.VerifyPassphrase(ctx, &accountv1.VerifyPassphraseRequest{Name: "Target", Passphrase: "real", ConnInfo: "2.2.2.2"})
	if r.GetOk() || r.GetReason() != "locked" {
		t.Fatalf("after %d failures = %+v, want locked", passphraseLockAfter, r)
	}
	if r.GetRetryAfterMs() == 0 {
		t.Fatal("a locked response should carry a retry_after hint")
	}
}

func TestSetPassphraseRejectsEmpty(t *testing.T) {
	svc := newTestService(newFakeStore())
	if _, err := svc.SetPassphrase(context.Background(), &accountv1.SetPassphraseRequest{AccountId: "a"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty passphrase should be InvalidArgument, got %v", err)
	}
}
