// Package passphrase hashes + verifies MUD passphrases with Argon2id (docs/ACCOUNT.md §5/§12, Phase 14.5).
// The hash is stored as a self-describing PHC string ($argon2id$v=19$m=...,t=...,p=...$salt$hash), so the
// parameters travel WITH each hash and can be raised over time without invalidating older hashes. The
// passphrase path is the website-less convenience login; it is discouraged over PLAIN telnet (cleartext) —
// the gate warns, and TLS/SSH (Phase 14.6) make it safe.
package passphrase

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are the Argon2id cost parameters. Defaults follow the OWASP baseline (m=19 MiB, t=2, p=1); they are
// tunable via config and ride each hash's PHC string, so raising them never breaks existing hashes.
type Params struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLen     uint32
	KeyLen      uint32
}

// DefaultParams is the OWASP-baseline Argon2id cost.
var DefaultParams = Params{Memory: 19 * 1024, Iterations: 2, Parallelism: 1, SaltLen: 16, KeyLen: 32}

// ErrMismatch is returned by Verify when the passphrase does not match the hash.
var ErrMismatch = errors.New("passphrase: mismatch")

// Hash returns a PHC-encoded Argon2id hash of plain using p.
func Hash(plain string, p Params) (string, error) {
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("passphrase: salt: %w", err)
	}
	key := argon2.IDKey([]byte(plain), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// Verify checks plain against a PHC-encoded Argon2id hash. It returns nil on a match, ErrMismatch on a
// non-match, and a wrapped error on a malformed hash. The comparison is constant-time.
func Verify(plain, encoded string) error {
	p, salt, want, err := decode(encoded)
	if err != nil {
		return err
	}
	//nolint:gosec // len(want) is the stored hash length (<=64 bytes); fits uint32
	got := argon2.IDKey([]byte(plain), salt, p.Iterations, p.Memory, p.Parallelism, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) == 1 {
		return nil
	}
	return ErrMismatch
}

// decode parses a PHC Argon2id string back into its params + salt + hash.
func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return Params{}, nil, nil, fmt.Errorf("passphrase: not an argon2id PHC string")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, fmt.Errorf("passphrase: unsupported argon2 version")
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return Params{}, nil, nil, fmt.Errorf("passphrase: bad params: %w", err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("passphrase: bad salt: %w", err)
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, fmt.Errorf("passphrase: bad hash: %w", err)
	}
	return p, salt, hash, nil
}
