package assertion

import (
	"bytes"
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

// TestVerifyRejectsForgedTierElevation (#27 security): a dedicated NEGATIVE guard for the elevation-forge
// vector — keep every other claim, flip only the tier (player -> admin), reattach the original signature ->
// rejected. Distinct from TestVerifyRejectsTamperedPayload (which forges the account). (The complementary
// POSITIVE property — that the tier is actually inside the signed bytes — is pinned by TestSignVerifyRoundTrip,
// which signs Tier:"admin" and asserts it round-trips.)
func TestVerifyRejectsForgedTierElevation(t *testing.T) {
	pub, priv := mustKeys(t)
	now := time.Unix(1000, 0)
	tok, _ := Sign(priv, Claims{Account: "acct-1", Session: "s", Tier: "player", Expires: now.Add(time.Minute).Unix()})

	// Same account/session/expiry, tier flipped player->admin, original signature reused: the sig covers the
	// tier byte, so verification must fail rather than trust the forged elevation.
	fb, _ := json.Marshal(Claims{Account: "acct-1", Session: "s", Tier: "admin", Expires: now.Add(time.Minute).Unix()})
	_, sigB64, _ := strings.Cut(tok, ".")
	forged := b64.EncodeToString(fb) + "." + sigB64
	if _, err := Verify(pub, forged, now); err != ErrSignature {
		t.Fatalf("forged tier elevation: err = %v, want ErrSignature", err)
	}
}

// TestVerifyRejectsDuplicateKeyTierElevation (#27 security) locks the SIGN==VERIFY byte-identity property:
// Verify checks the signature over the raw payload bytes and unmarshals THOSE SAME bytes, so there is no
// canonicalization seam an attacker can wedge between "what was signed" and "what is read". The attack it
// guards: take a legitimately-signed player token and append a DUPLICATE `"tier":"admin"` key — Go's
// encoding/json is last-key-wins, so a naive reader would see admin. Because the bytes now differ from what
// was signed (and the attacker cannot re-sign), Verify must reject with ErrSignature. A future refactor that
// canonicalized/normalized the payload BEFORE checking the signature (then unmarshalled the raw form) would
// silently reopen this; this test fails the moment that happens.
func TestVerifyRejectsDuplicateKeyTierElevation(t *testing.T) {
	pub, priv := mustKeys(t)
	now := time.Unix(1000, 0)
	tok, _ := Sign(priv, Claims{Account: "acct-1", Session: "s", Tier: "player", Expires: now.Add(time.Minute).Unix()})

	payloadB64, sigB64, _ := strings.Cut(tok, ".")
	raw, err := b64.DecodeString(payloadB64)
	if err != nil {
		t.Fatalf("decode signed payload: %v", err)
	}
	// Inject a duplicate tier key so a last-key-wins unmarshal would read admin.
	forgedRaw := bytes.Replace(raw, []byte(`"tier":"player"`), []byte(`"tier":"player","tier":"admin"`), 1)
	if bytes.Equal(forgedRaw, raw) {
		t.Fatal("precondition: expected to find and duplicate the tier key in the signed payload")
	}

	// Precondition: the forged bytes REALLY do decode to admin (so it's the signature check, not JSON
	// semantics, that saves us — otherwise this test would be vacuous).
	var c Claims
	if err := json.Unmarshal(forgedRaw, &c); err != nil || c.Tier != "admin" {
		t.Fatalf("precondition: duplicate-key payload should unmarshal to admin, got tier=%q err=%v", c.Tier, err)
	}

	forged := b64.EncodeToString(forgedRaw) + "." + sigB64
	if _, err := Verify(pub, forged, now); err != ErrSignature {
		t.Fatalf("duplicate-key tier elevation: err = %v, want ErrSignature", err)
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
