package assertion

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// assertion_test.go — Phase 14.3: the signed-assertion primitive. A valid token round-trips; a forged
// signature, a tampered payload, a wrong key, an expired token, and a malformed token are all rejected.

func mustKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv := mustKeys(t)
	now := time.Unix(1000, 0)
	claims := Claims{Account: "acct-1", Character: "Aragorn", Session: "sess-9", Expires: now.Add(time.Minute).Unix(), Tier: "admin"}

	tok, err := Sign(priv, claims)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(pub, tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if got != claims {
		t.Fatalf("round-trip claims = %+v, want %+v", got, claims)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	pub, priv := mustKeys(t)
	now := time.Unix(1000, 0)
	tok, _ := Sign(priv, Claims{Account: "a", Session: "s", Expires: now.Unix()}) // exp == now -> expired
	if _, err := Verify(pub, tok, now); err != ErrExpired {
		t.Fatalf("expired token: err = %v, want ErrExpired", err)
	}
}

func TestVerifyRejectsWrongKey(t *testing.T) {
	_, priv := mustKeys(t)
	otherPub, _ := mustKeys(t)
	now := time.Unix(1000, 0)
	tok, _ := Sign(priv, Claims{Account: "a", Session: "s", Expires: now.Add(time.Minute).Unix()})
	if _, err := Verify(otherPub, tok, now); err != ErrSignature {
		t.Fatalf("wrong key: err = %v, want ErrSignature", err)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	pub, priv := mustKeys(t)
	now := time.Unix(1000, 0)
	tok, _ := Sign(priv, Claims{Account: "acct-1", Session: "s", Expires: now.Add(time.Minute).Unix()})

	// Forge a different payload but keep the original signature: must fail (the sig covers the payload).
	fb, _ := json.Marshal(Claims{Account: "acct-EVIL", Session: "s", Expires: now.Add(time.Minute).Unix()})
	_, sigB64, _ := strings.Cut(tok, ".")
	tampered := b64.EncodeToString(fb) + "." + sigB64
	if _, err := Verify(pub, tampered, now); err != ErrSignature {
		t.Fatalf("tampered payload: err = %v, want ErrSignature", err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	pub, _ := mustKeys(t)
	now := time.Unix(1000, 0)
	for _, tok := range []string{"", "no-dot", "!!!.###", "onlyonepart."} {
		if _, err := Verify(pub, tok, now); err == nil {
			t.Fatalf("malformed token %q should be rejected", tok)
		}
	}
}

func TestKeyParsing(t *testing.T) {
	pub, priv := mustKeys(t)
	seed := priv.Seed()

	// Public key round-trips through base64.
	pk, err := ParsePublicKey(base64.StdEncoding.EncodeToString(pub))
	if err != nil || !pk.Equal(pub) {
		t.Fatalf("public key parse: %v / equal=%v", err, pk.Equal(pub))
	}
	// Private key parses from BOTH the full key and the 32-byte seed.
	full, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(priv))
	if err != nil || !full.Equal(priv) {
		t.Fatalf("private key (full) parse: %v", err)
	}
	fromSeed, err := ParsePrivateKey(base64.StdEncoding.EncodeToString(seed))
	if err != nil || !fromSeed.Equal(priv) {
		t.Fatalf("private key (seed) parse: %v", err)
	}
}
