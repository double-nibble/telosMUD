// Package sessionlock is the Phase-14.4 single-session lock (docs/ACCOUNT.md §10): one LIVE session per
// character, enforced ACROSS shards via a Redis key the live session heartbeats. A new login ACQUIRES the
// key with a fresh token (takeover, overwriting any prior holder); the displaced session's next Renew sees
// its token was replaced (owned=false) and self-kicks. A crashed connection's key self-expires via the TTL,
// so the lock never wedges. This complements the within-shard takeover (world/zone.go displacedKick, fast
// path) and the epoch/state_version single-WRITER guard (which prevents two shard OWNERS, not two sockets).
package sessionlock

import (
	"context"
	"time"
)

// Lock is the single-session lock store (Redis in production, in-memory for tests).
type Lock interface {
	// Acquire takes key for the caller's token with ttl, OVERWRITING any existing holder (the takeover
	// semantics — a new login always wins). Returns the PREVIOUS holder's token ("" if none) for logging.
	Acquire(ctx context.Context, key, token string, ttl time.Duration) (prev string, err error)
	// Renew refreshes the ttl IFF the caller still owns key (stored token == token). owned=false means the
	// caller was DISPLACED by a newer login (or the key expired) — the caller must self-kick.
	Renew(ctx context.Context, key, token string, ttl time.Duration) (owned bool, err error)
	// Release deletes key IFF the caller still owns it (a no-op when already displaced/expired, so a
	// late-leaving displaced session never deletes the new holder's lock).
	Release(ctx context.Context, key, token string) error
}

// Key builds the lock key for a character (namespaced so it never collides with the directory/presence keys).
func Key(character string) string { return "session:" + character }
