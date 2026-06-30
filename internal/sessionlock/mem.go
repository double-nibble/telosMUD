package sessionlock

import (
	"context"
	"sync"
	"time"
)

// mem.go — an in-memory Lock for tests + a single-process dev run (no Redis). Same semantics as the Redis
// impl: overwrite-on-acquire (takeover), compare-on-renew/release, TTL expiry.

type memLock struct {
	mu      sync.Mutex
	entries map[string]memEntry
}

type memEntry struct {
	token   string
	expires time.Time
}

// NewMem builds an in-memory single-session Lock (tests / no-Redis dev).
func NewMem() Lock { return &memLock{entries: map[string]memEntry{}} }

func (l *memLock) liveLocked(key string) (memEntry, bool) {
	e, ok := l.entries[key]
	if !ok || time.Now().After(e.expires) {
		return memEntry{}, false
	}
	return e, true
}

func (l *memLock) Acquire(_ context.Context, key, token string, ttl time.Duration) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev := ""
	if e, ok := l.liveLocked(key); ok {
		prev = e.token
	}
	l.entries[key] = memEntry{token: token, expires: time.Now().Add(ttl)}
	return prev, nil
}

func (l *memLock) Renew(_ context.Context, key, token string, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.liveLocked(key); ok && e.token == token {
		l.entries[key] = memEntry{token: token, expires: time.Now().Add(ttl)}
		return true, nil
	}
	return false, nil
}

func (l *memLock) Release(_ context.Context, key, token string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[key]; ok && e.token == token {
		delete(l.entries, key)
	}
	return nil
}
