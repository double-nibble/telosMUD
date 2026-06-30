package account

import (
	"context"
	"testing"
	"time"
)

// deviceauth_test.go — the in-memory DeviceAuthStore lifecycle (Phase 15). The Redis impl shares the contract;
// a gated Redis test covers it in the service slice.

func TestMemDeviceAuthLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewMemDeviceAuth()

	code, err := s.Start(ctx, time.Minute)
	if err != nil || code == "" {
		t.Fatalf("Start: code=%q err=%v", code, err)
	}

	// Pending until authorized.
	st, acct, found, err := s.Poll(ctx, code)
	if err != nil || !found || st != DevicePending || acct != "" {
		t.Fatalf("poll before auth: st=%q acct=%q found=%v err=%v", st, acct, found, err)
	}

	// Authorize flips it to authed with the account.
	ok, err := s.Authorize(ctx, code, "acct-1")
	if err != nil || !ok {
		t.Fatalf("authorize: ok=%v err=%v", ok, err)
	}

	// The first authed poll returns the account and CONSUMES the session.
	st, acct, found, err = s.Poll(ctx, code)
	if err != nil || !found || st != DeviceAuthed || acct != "acct-1" {
		t.Fatalf("poll after auth: st=%q acct=%q found=%v err=%v", st, acct, found, err)
	}
	// A second poll finds nothing (one-shot consume).
	if _, _, found, _ := s.Poll(ctx, code); found {
		t.Fatal("an authed session must be consumed on the first authed poll")
	}
}

func TestMemDeviceAuthUnknownAndExpired(t *testing.T) {
	ctx := context.Background()
	s := NewMemDeviceAuth()

	// Authorizing / polling an unknown code is a clean miss, not an error.
	if ok, err := s.Authorize(ctx, "nope", "acct"); err != nil || ok {
		t.Fatalf("authorize unknown: ok=%v err=%v", ok, err)
	}
	if _, _, found, err := s.Poll(ctx, "nope"); err != nil || found {
		t.Fatalf("poll unknown: found=%v err=%v", found, err)
	}

	// An expired session is gone (found=false), and can't be authorized.
	code, _ := s.Start(ctx, -time.Second) // already expired
	if ok, _ := s.Authorize(ctx, code, "acct"); ok {
		t.Fatal("an expired session must not authorize")
	}
	if _, _, found, _ := s.Poll(ctx, code); found {
		t.Fatal("an expired session must poll as not-found")
	}
}
