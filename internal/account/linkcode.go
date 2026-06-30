package account

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"
)

// linkcode.go — Phase-14.2 LINK CODES (docs/ACCOUNT.md §4), the credential-less telnet bridge. Signed into
// the website, a user clicks "Play"; the account mints a short-lived, single-use code in Redis keyed to
// (account[, character]). The user types `connect <code>` at the gate, which redeems it (atomic consume) and
// enters the world. One-shot + short TTL + high entropy keeps the cleartext-interception window tiny and
// non-replayable.

// linkCodeTTL is how long a minted code is valid (ACCOUNT.md §4: ~5 min).
const linkCodeTTL = 5 * time.Minute

// linkCodeLen is the code length in characters; with a 32-symbol alphabet that is 40 bits of entropy.
const linkCodeLen = 8

// linkCodeAlphabet is an unambiguous base-32 alphabet (Crockford-style: no I/L/O/U, no 0/1) so a code is
// easy to read aloud / retype off a screen.
const linkCodeAlphabet = "ABCDEFGHJKMNPQRSTVWXYZ23456789#@" // 32 symbols

// LinkCodeStore is the single-use code store (Redis in production, in-memory for tests). Mint creates a code
// bound to (accountID, characterID — characterID may be ""); Redeem ATOMICALLY consumes it (a second redeem
// of the same code finds nothing). The atomicity is what makes a code one-shot under a race.
type LinkCodeStore interface {
	Mint(ctx context.Context, accountID, characterID string, ttl time.Duration) (string, error)
	Redeem(ctx context.Context, code string) (accountID, characterID string, found bool, err error)
}

// newLinkCode returns a fresh random code from the unambiguous alphabet (crypto/rand).
func newLinkCode() (string, error) {
	buf := make([]byte, linkCodeLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("account: link-code entropy: %w", err)
	}
	out := make([]byte, linkCodeLen)
	for i, b := range buf {
		out[i] = linkCodeAlphabet[int(b)%len(linkCodeAlphabet)]
	}
	return string(out), nil
}

// memLinkCodes is an in-memory LinkCodeStore for tests + a single-process dev run (no Redis). Production
// uses the Redis impl for cross-process visibility + native TTL.
type memLinkCodes struct {
	mu      sync.Mutex
	entries map[string]memLinkEntry
}

type memLinkEntry struct {
	accountID, characterID string
	expires                time.Time
}

// NewMemLinkCodes builds an in-memory link-code store (tests / no-Redis dev).
func NewMemLinkCodes() LinkCodeStore {
	return &memLinkCodes{entries: map[string]memLinkEntry{}}
}

func (s *memLinkCodes) Mint(_ context.Context, accountID, characterID string, ttl time.Duration) (string, error) {
	code, err := newLinkCode()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[code] = memLinkEntry{accountID: accountID, characterID: characterID, expires: time.Now().Add(ttl)}
	return code, nil
}

func (s *memLinkCodes) Redeem(_ context.Context, code string) (string, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[code]
	if !ok {
		return "", "", false, nil
	}
	delete(s.entries, code) // single-use: consume on redeem
	if time.Now().After(e.expires) {
		return "", "", false, nil // expired (and now removed)
	}
	return e.accountID, e.characterID, true, nil
}
