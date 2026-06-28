package presence

import (
	"context"
	"sync"
	"time"
)

// Mem is an in-process presence roster mirroring the Redis semantics (TTL age-out + the write-authority
// guard) without a broker. It exists so the cross-shard `who` tests run N shards against ONE shared
// roster in a single process (exactly as the commbus MemBus lets N shards share one bus), and so a
// no-Redis single-shard run can still degrade cleanly. It is concurrency-safe: multiple shard heartbeat
// goroutines and `who` reads touch it at once.
//
// Age-out is enforced lazily on read against a clock: List and the owner-guard treat an entry whose
// expires <= now as absent, so a "crashed" shard (one that stops calling Set) ages out exactly like
// Redis lets a key's PEXPIRE lapse — the test advances the clock with SetClock to make it deterministic.
type Mem struct {
	mu  sync.Mutex
	m   map[string]memEntry
	now func() time.Time // injectable clock for deterministic age-out tests
}

type memEntry struct {
	Entry
	expires time.Time
}

// NewMem builds an empty in-process roster using the wall clock.
func NewMem() *Mem {
	return &Mem{m: map[string]memEntry{}, now: time.Now}
}

// SetClock injects a clock for deterministic TTL age-out tests (the crashed-shard demonstration advances
// it past a stopped shard's TTL). nil restores the wall clock.
func (r *Mem) SetClock(now func() time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	r.now = now
}

// live reports whether an entry is still within its TTL at t. Caller holds r.mu.
func (e memEntry) live(t time.Time) bool { return e.expires.After(t) }

// Set applies the batched heartbeat under the write-authority guard (P8-A4): an entry whose key a
// DIFFERENT, still-live shard owns is refused (and ErrNotOwner returned after applying the rest); an
// unowned, expired, or self-owned key is (re)written and its TTL reset. Mirrors setPresence's Lua.
func (r *Mem) Set(_ context.Context, shardID string, entries []Entry, ttl time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	var refused bool
	for _, e := range entries {
		if cur, ok := r.m[e.PlayerID]; ok && cur.ShardID != shardID && cur.live(now) {
			refused = true
			continue
		}
		if e.LastSeen.IsZero() {
			e.LastSeen = now
		}
		e.ShardID = shardID // authority: the stored owner is always the calling shard
		r.m[e.PlayerID] = memEntry{Entry: e, expires: now.Add(ttl)}
	}
	if refused {
		return ErrNotOwner
	}
	return nil
}

// Remove drops the key only if shardID still owns it (the owner-guard).
func (r *Mem) Remove(_ context.Context, shardID, playerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.m[playerID]; ok && cur.ShardID == shardID {
		delete(r.m, playerID)
	}
	return nil
}

// List returns every entry still within its TTL at the current clock — expired entries are treated as
// absent (and pruned), so a crashed shard's players age out exactly like a lapsed Redis key.
func (r *Mem) List(_ context.Context) ([]Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	out := make([]Entry, 0, len(r.m))
	for id, e := range r.m {
		if !e.live(now) {
			delete(r.m, id) // lazy prune of an aged-out entry
			continue
		}
		out = append(out, e.Entry)
	}
	return out, nil
}

// Compile-time assertion.
var _ Roster = (*Mem)(nil)
