package sessionlock

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// redis_test.go — Phase 14.4: the Redis Lock against miniredis, so the actual SET..GET + the compare-and-act
// Lua scripts (renew/release) are exercised, not just the in-memory model.

func newRedisLock(t *testing.T) (Lock, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedis(rdb), mr
}

func TestRedisTakeoverAndCompareScripts(t *testing.T) {
	l, _ := newRedisLock(t)
	ctx := context.Background()
	key := Key("Aragorn")

	prev, err := l.Acquire(ctx, key, "A", time.Minute)
	if err != nil || prev != "" {
		t.Fatalf("first acquire: prev=%q err=%v", prev, err)
	}
	if owned, err := l.Renew(ctx, key, "A", time.Minute); err != nil || !owned {
		t.Fatalf("owner renew: owned=%v err=%v", owned, err)
	}

	// Takeover: B acquires, the prior holder A is reported.
	prev, err = l.Acquire(ctx, key, "B", time.Minute)
	if err != nil || prev != "A" {
		t.Fatalf("takeover acquire: prev=%q err=%v, want A", prev, err)
	}
	// The displaced A can neither renew nor release B's lock (the Lua compare).
	if owned, _ := l.Renew(ctx, key, "A", time.Minute); owned {
		t.Fatal("displaced holder A must not renew")
	}
	if err := l.Release(ctx, key, "A"); err != nil {
		t.Fatal(err)
	}
	if owned, _ := l.Renew(ctx, key, "B", time.Minute); !owned {
		t.Fatal("A's release must not have freed B's lock")
	}
	// B can release its own lock.
	if err := l.Release(ctx, key, "B"); err != nil {
		t.Fatal(err)
	}
	if owned, _ := l.Renew(ctx, key, "B", time.Minute); owned {
		t.Fatal("after B releases, the lock is free")
	}
}

func TestRedisLockTTLExpiry(t *testing.T) {
	l, mr := newRedisLock(t)
	ctx := context.Background()
	key := Key("Gimli")
	if _, err := l.Acquire(ctx, key, "t", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(31 * time.Second) // advance miniredis past the TTL
	if owned, _ := l.Renew(ctx, key, "t", time.Minute); owned {
		t.Fatal("an expired lock must not still be owned")
	}
}
