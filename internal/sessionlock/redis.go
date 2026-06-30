package sessionlock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// redis.go — the Redis Lock. Acquire is `SET key token EX ttl GET` (atomic overwrite + return the prior
// holder). Renew + Release are compare-and-act Lua scripts (the stored token must equal ours), so a
// displaced session can neither renew nor delete the new holder's lock — the one-owner guarantee under a race.

// renewScript: refresh the ttl only if we still own the key. Returns 1 (owned, renewed) or 0 (displaced).
var renewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0`)

// releaseScript: delete the key only if we still own it.
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`)

type redisLock struct {
	rdb *redis.Client
}

// NewRedis builds a Redis-backed single-session Lock over an existing client.
func NewRedis(rdb *redis.Client) Lock { return &redisLock{rdb: rdb} }

func (l *redisLock) Acquire(ctx context.Context, key, token string, ttl time.Duration) (string, error) {
	prev, err := l.rdb.SetArgs(ctx, key, token, redis.SetArgs{TTL: ttl, Get: true}).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil // no prior holder
	}
	if err != nil {
		return "", fmt.Errorf("sessionlock: acquire %s: %w", key, err)
	}
	return prev, nil
}

func (l *redisLock) Renew(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	res, err := renewScript.Run(ctx, l.rdb, []string{key}, token, ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("sessionlock: renew %s: %w", key, err)
	}
	return res == 1, nil
}

func (l *redisLock) Release(ctx context.Context, key, token string) error {
	if err := releaseScript.Run(ctx, l.rdb, []string{key}, token).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("sessionlock: release %s: %w", key, err)
	}
	return nil
}
