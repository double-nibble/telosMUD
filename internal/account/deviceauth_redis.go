package account

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// deviceauth_redis.go — the Redis DeviceAuthStore (Phase 15). A session is a key `devauth:<device_code>` whose
// value is "pending" or "authed\x1f<accountID>", SET with a TTL (native expiry). The browser-side Authorize is
// a Lua compare-and-set (overwrite ONLY if the key still exists, preserving the TTL) so a flip races cleanly
// against expiry; the gate-side Poll consumes the session atomically once it reads authed.

const deviceAuthKeyPrefix = "devauth:"

// deviceAuthSep separates the status + account in the stored value (a unit separator — never in a UUID).
const deviceAuthSep = "\x1f"

// authorizeScript flips a still-live session to authed, preserving its TTL. Returns 1 on success, 0 if the
// key is gone (expired / never existed). KEEPTTL keeps the original expiry so an authed-but-unclaimed session
// can't outlive the login window.
var authorizeScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 1 then
  redis.call('SET', KEYS[1], ARGV[1], 'KEEPTTL')
  return 1
end
return 0
`)

// pollScript reads a session and, when it is authed, DELETES it in the same call (one-shot consume). Returns
// the stored value (or false when the key is gone). Doing the read+conditional-delete in Lua makes the gate's
// "see authed → consume" atomic.
var pollScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == false then return false end
if string.sub(v, 1, 6) == 'authed' then
  redis.call('DEL', KEYS[1])
end
return v
`)

type redisDeviceAuth struct {
	rdb *redis.Client
}

// NewRedisDeviceAuth builds a Redis-backed device-auth store over an existing client.
func NewRedisDeviceAuth(rdb *redis.Client) DeviceAuthStore {
	return &redisDeviceAuth{rdb: rdb}
}

func (s *redisDeviceAuth) Start(ctx context.Context, ttl time.Duration) (string, error) {
	code, err := newDeviceCode()
	if err != nil {
		return "", err
	}
	if err := s.rdb.Set(ctx, deviceAuthKeyPrefix+code, string(DevicePending), ttl).Err(); err != nil {
		return "", fmt.Errorf("account: start device auth: %w", err)
	}
	return code, nil
}

func (s *redisDeviceAuth) Authorize(ctx context.Context, deviceCode, accountID string) (bool, error) {
	val := string(DeviceAuthed) + deviceAuthSep + accountID
	res, err := authorizeScript.Run(ctx, s.rdb, []string{deviceAuthKeyPrefix + deviceCode}, val).Int()
	if err != nil {
		return false, fmt.Errorf("account: authorize device: %w", err)
	}
	return res == 1, nil
}

func (s *redisDeviceAuth) Poll(ctx context.Context, deviceCode string) (DeviceStatus, string, bool, error) {
	val, err := pollScript.Run(ctx, s.rdb, []string{deviceAuthKeyPrefix + deviceCode}).Text()
	if errors.Is(err, redis.Nil) {
		return "", "", false, nil // unknown / expired
	}
	if err != nil {
		return "", "", false, fmt.Errorf("account: poll device: %w", err)
	}
	status, account, _ := strings.Cut(val, deviceAuthSep)
	if DeviceStatus(status) == DeviceAuthed {
		return DeviceAuthed, account, true, nil
	}
	return DevicePending, "", true, nil
}
