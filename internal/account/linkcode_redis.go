package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// linkcode_redis.go — the Redis LinkCodeStore (Phase 14.2). A code is a key `linkcode:<code>` whose value is
// "accountID\x1fcharacterID", SET with a TTL (native expiry). Redeem uses GETDEL — Redis 6.2+'s atomic
// get-and-delete — so the code is consumed exactly once even under a concurrent redeem race (the one-shot
// guarantee), with no Lua script or WATCH/MULTI dance.

const linkCodeKeyPrefix = "linkcode:"

// linkCodeSep separates account + character in the stored value (a unit separator — never appears in a UUID).
const linkCodeSep = "\x1f"

// redisLinkCodes is the Redis-backed LinkCodeStore.
type redisLinkCodes struct {
	rdb *redis.Client
}

// NewRedisLinkCodes builds a Redis-backed link-code store over an existing client.
func NewRedisLinkCodes(rdb *redis.Client) LinkCodeStore {
	return &redisLinkCodes{rdb: rdb}
}

func (s *redisLinkCodes) Mint(ctx context.Context, accountID, characterID string, ttl time.Duration) (string, error) {
	code, err := newLinkCode()
	if err != nil {
		return "", err
	}
	val := accountID + linkCodeSep + characterID
	if err := s.rdb.Set(ctx, linkCodeKeyPrefix+code, val, ttl).Err(); err != nil {
		return "", fmt.Errorf("account: store link code: %w", err)
	}
	return code, nil
}

func (s *redisLinkCodes) Redeem(ctx context.Context, code string) (string, string, bool, error) {
	val, err := s.rdb.GetDel(ctx, linkCodeKeyPrefix+code).Result()
	if errors.Is(err, redis.Nil) {
		return "", "", false, nil // unknown / already-redeemed / expired
	}
	if err != nil {
		return "", "", false, fmt.Errorf("account: redeem link code: %w", err)
	}
	account, character, _ := strings.Cut(val, linkCodeSep)
	return account, character, true, nil
}
