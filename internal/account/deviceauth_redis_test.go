package account

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// deviceauth_redis_test.go — the Redis DeviceAuthStore against miniredis, so the actual SET-with-TTL + the
// authorize/poll Lua scripts run (not just the in-memory model). miniredis is in-process, so this stays
// hermetic (no real Redis).

func newRedisDeviceAuthTest(t *testing.T) (DeviceAuthStore, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewRedisDeviceAuth(rdb), mr
}

func TestRedisDeviceAuthLifecycle(t *testing.T) {
	s, mr := newRedisDeviceAuthTest(t)
	ctx := context.Background()

	code, err := s.Start(ctx, time.Minute)
	if err != nil || code == "" {
		t.Fatalf("Start: code=%q err=%v", code, err)
	}

	// Pending.
	if st, _, found, err := s.Poll(ctx, code); err != nil || !found || st != DevicePending {
		t.Fatalf("poll pending: st=%q found=%v err=%v", st, found, err)
	}

	// Authorize (the Lua CAS) preserves the TTL — the key must still have an expiry set.
	if ok, err := s.Authorize(ctx, code, "acct-9"); err != nil || !ok {
		t.Fatalf("authorize: ok=%v err=%v", ok, err)
	}
	if ttl := mr.TTL(deviceAuthKeyPrefix + code); ttl <= 0 {
		t.Fatalf("authorize must KEEPTTL; ttl now %v", ttl)
	}

	// Authed poll returns the account and consumes the key (the Lua DEL).
	st, acct, found, err := s.Poll(ctx, code)
	if err != nil || !found || st != DeviceAuthed || acct != "acct-9" {
		t.Fatalf("authed poll: st=%q acct=%q found=%v err=%v", st, acct, found, err)
	}
	if mr.Exists(deviceAuthKeyPrefix + code) {
		t.Fatal("an authed poll must consume the session key (one-shot)")
	}
	if _, _, found, _ := s.Poll(ctx, code); found {
		t.Fatal("a consumed session must poll as not-found")
	}
}

func TestRedisDeviceAuthUnknown(t *testing.T) {
	s, _ := newRedisDeviceAuthTest(t)
	ctx := context.Background()
	// Authorize / poll on a code that was never started is a clean miss.
	if ok, err := s.Authorize(ctx, "ghost", "acct"); err != nil || ok {
		t.Fatalf("authorize unknown: ok=%v err=%v", ok, err)
	}
	if _, _, found, err := s.Poll(ctx, "ghost"); err != nil || found {
		t.Fatalf("poll unknown: found=%v err=%v", found, err)
	}
}
