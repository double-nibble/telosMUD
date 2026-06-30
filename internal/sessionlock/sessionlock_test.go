package sessionlock

import (
	"context"
	"testing"
	"time"
)

// sessionlock_test.go — Phase 14.4: the takeover semantics of the in-memory Lock (the Redis impl mirrors it;
// a gated cross-shard test exercises Redis end to end). Acquire overwrites (takeover); the displaced holder
// can neither renew nor delete the new holder's lock; a key self-expires.

func TestTakeoverDisplacesPriorHolder(t *testing.T) {
	l := NewMem()
	ctx := context.Background()
	key := Key("Aragorn")

	// First login takes the lock.
	prev, err := l.Acquire(ctx, key, "token-A", time.Minute)
	if err != nil || prev != "" {
		t.Fatalf("first acquire: prev=%q err=%v, want empty/nil", prev, err)
	}
	if owned, _ := l.Renew(ctx, key, "token-A", time.Minute); !owned {
		t.Fatal("the holder should still own the lock")
	}

	// Second login TAKES OVER, reporting the prior holder.
	prev, err = l.Acquire(ctx, key, "token-B", time.Minute)
	if err != nil || prev != "token-A" {
		t.Fatalf("takeover acquire: prev=%q err=%v, want token-A", prev, err)
	}

	// The displaced holder (A) can no longer renew — it must self-kick.
	if owned, _ := l.Renew(ctx, key, "token-A", time.Minute); owned {
		t.Fatal("the displaced holder must NOT still own the lock")
	}
	// The new holder (B) owns it.
	if owned, _ := l.Renew(ctx, key, "token-B", time.Minute); !owned {
		t.Fatal("the new holder should own the lock")
	}
	// A late Release by the displaced holder must NOT delete B's lock.
	if err := l.Release(ctx, key, "token-A"); err != nil {
		t.Fatal(err)
	}
	if owned, _ := l.Renew(ctx, key, "token-B", time.Minute); !owned {
		t.Fatal("a displaced holder's Release must not free the new holder's lock")
	}
}

func TestLockSelfExpires(t *testing.T) {
	l := NewMem()
	ctx := context.Background()
	key := Key("Gimli")
	if _, err := l.Acquire(ctx, key, "t", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	// After the TTL the crashed holder's key is gone — a fresh login sees no prior holder.
	prev, _ := l.Acquire(ctx, key, "t2", time.Minute)
	if prev != "" {
		t.Fatalf("an expired lock should report no prior holder, got %q", prev)
	}
}

func TestReleaseFreesForNextLogin(t *testing.T) {
	l := NewMem()
	ctx := context.Background()
	key := Key("Legolas")
	_, _ = l.Acquire(ctx, key, "t1", time.Minute)
	if err := l.Release(ctx, key, "t1"); err != nil {
		t.Fatal(err)
	}
	if owned, _ := l.Renew(ctx, key, "t1", time.Minute); owned {
		t.Fatal("a released lock should no longer be owned")
	}
}
