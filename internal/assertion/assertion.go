// Package assertion is the signed session-assertion primitive (docs/ACCOUNT.md §9, Phase 14.3): the short-
// lived, Ed25519-signed token telos-account issues at login and a world shard verifies OFFLINE on attach,
// so the world trusts the gate's asserted {account, character, session} identity WITHOUT a per-connect RPC
// to account — and a compromised gate cannot forge an identity it was not granted.
//
// The token is a compact two-part string `base64url(payloadJSON).base64url(sig)` (JWT-shaped but minimal —
// no alg negotiation, Ed25519 only, so there is no "alg: none" downgrade class). The payload is the Claims.
package assertion

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Claims is the assertion payload: who the connection authenticated as, and until when.
type Claims struct {
	Account   string `json:"acc"`            // account id
	Character string `json:"chr,omitempty"`  // selected character (name/id)
	Session   string `json:"sid"`            // the gate session id (stable across a redirect)
	Expires   int64  `json:"exp"`            // expiry, unix seconds
	Tier      string `json:"tier,omitempty"` // account trust tier (#27): player/builder/admin — signed, so the world trusts it offline. Empty = player (unverified/legacy)
}

// Common errors callers can branch on.
var (
	ErrMalformed = errors.New("assertion: malformed token")
	ErrSignature = errors.New("assertion: bad signature")
	ErrExpired   = errors.New("assertion: expired")
)

var b64 = base64.RawURLEncoding

// Sign produces a signed token for the claims. The signature covers the exact payload bytes that are
// transmitted, so verification re-checks the bytes it received (no re-marshal ambiguity).
func Sign(priv ed25519.PrivateKey, c Claims) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", fmt.Errorf("assertion: bad private key size %d", len(priv))
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("assertion: marshal claims: %w", err)
	}
	sig := ed25519.Sign(priv, payload)
	return b64.EncodeToString(payload) + "." + b64.EncodeToString(sig), nil
}

// Verify checks the token against pub and returns its claims. It rejects a malformed token, a bad signature,
// and an EXPIRED token (exp <= now). `now` is injected so tests are deterministic; pass time.Now() in prod.
func Verify(pub ed25519.PublicKey, token string, now time.Time) (Claims, error) {
	if len(pub) != ed25519.PublicKeySize {
		return Claims{}, fmt.Errorf("assertion: bad public key size %d", len(pub))
	}
	payloadB64, sigB64, ok := strings.Cut(token, ".")
	if !ok {
		return Claims{}, ErrMalformed
	}
	payload, err := b64.DecodeString(payloadB64)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	sig, err := b64.DecodeString(sigB64)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	if !ed25519.Verify(pub, payload, sig) {
		return Claims{}, ErrSignature
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, ErrMalformed
	}
	if now.Unix() >= c.Expires {
		return Claims{}, ErrExpired
	}
	return c, nil
}

// ParsePublicKey decodes a base64 (std) Ed25519 public key (the world's verify key from config).
func ParsePublicKey(b64std string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64std))
	if err != nil {
		return nil, fmt.Errorf("assertion: decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("assertion: public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey decodes a base64 (std) Ed25519 private key. It accepts both the 64-byte full key and the
// 32-byte SEED (deriving the full key), so an operator can store either.
func ParsePrivateKey(b64std string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64std))
	if err != nil {
		return nil, fmt.Errorf("assertion: decode private key: %w", err)
	}
	switch len(raw) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	default:
		return nil, fmt.Errorf("assertion: private key is %d bytes, want %d (key) or %d (seed)",
			len(raw), ed25519.PrivateKeySize, ed25519.SeedSize)
	}
}
