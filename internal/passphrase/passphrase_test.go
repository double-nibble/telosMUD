package passphrase

import (
	"strings"
	"testing"
)

// passphrase_test.go — Phase 14.5: Argon2id hash/verify round-trip + rejection.

func TestHashVerifyRoundTrip(t *testing.T) {
	// A small cost so the test is fast; the PHC string carries it, so Verify uses the same.
	p := Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}
	hash, err := Hash("correct horse battery staple", p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash %q is not a PHC argon2id string", hash)
	}
	if err := Verify("correct horse battery staple", hash); err != nil {
		t.Fatalf("the correct passphrase should verify: %v", err)
	}
	if err := Verify("wrong passphrase", hash); err != ErrMismatch {
		t.Fatalf("a wrong passphrase should return ErrMismatch, got %v", err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "notphc", "$argon2id$v=19$m=8$onlyfour$x", "$bcrypt$..."} {
		if err := Verify("x", bad); err == nil || err == ErrMismatch {
			t.Fatalf("malformed hash %q should be a decode error, got %v", bad, err)
		}
	}
}

// TestHashIsSalted: two hashes of the same passphrase differ (random salt), but both verify.
func TestHashIsSalted(t *testing.T) {
	p := Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32}
	h1, _ := Hash("same", p)
	h2, _ := Hash("same", p)
	if h1 == h2 {
		t.Fatal("two hashes of the same passphrase must differ (random salt)")
	}
	if Verify("same", h1) != nil || Verify("same", h2) != nil {
		t.Fatal("both salted hashes must verify")
	}
}
