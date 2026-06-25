package directory

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrNotFound is returned when a zone or player has no recorded placement.
var ErrNotFound = errors.New("directory: not found")

// ErrZoneClaimed is returned when a zone is already owned by a different, still-live
// shard — the guard that prevents two shards both believing they host one zone.
var ErrZoneClaimed = errors.New("directory: zone already claimed by another shard")

// DefaultZoneLease is how long a zone claim stays valid without renewal. Shards
// heartbeat well within this window; a crashed shard's claim expires after it, so a
// zone can be re-hosted elsewhere without a dead binding lingering forever.
const DefaultZoneLease = 15 * time.Second

// Redis is the Phase 2 directory: the authoritative zone->shard and player->shard
// maps backed by Redis (docs/ARCHITECTURE.md §4).
//
//   - Shards call RegisterZone on boot for every zone they host.
//   - The gate calls ShardForZone / PlayerPlacement to decide which shard to dial.
//   - The cross-shard handoff calls SetPlayerShard with a monotonically increasing
//     epoch, so a delayed or duplicated handoff can never route a player back to an
//     older shard (the directory side of the single-writer guarantee).
type Redis struct {
	rdb *redis.Client
	ns  string
}

// NewRedis returns a directory over rdb. ns namespaces all keys (default "telos"),
// so multiple logical worlds can share one Redis.
func NewRedis(rdb *redis.Client, ns string) *Redis {
	if ns == "" {
		ns = "telos"
	}
	return &Redis{rdb: rdb, ns: ns}
}

func (r *Redis) zoneKey(zoneID string) string     { return r.ns + ":dir:zone:" + zoneID }
func (r *Redis) playerKey(playerID string) string { return r.ns + ":dir:player:" + playerID }

// claimZone takes/renews an exclusive lease on a zone. It succeeds when the zone is
// unowned, its lease has expired, or the caller is already the owner (a renewal);
// it fails (returns 0) only when a DIFFERENT shard holds a still-live lease. This is
// what stops two shards from both claiming one zone (the cardinal single-writer
// guarantee, one level up from players). A Redis key TTL backstops the lease so a
// fully-dead zone's binding eventually disappears even without a reader.
//
// All time comparisons use redis.call('TIME') — the single Redis clock — rather than
// each caller's wall clock, so a shard with a skewed clock can't mis-judge a lease
// (e.g. steal a live one). KEYS[1]=zone key  ARGV[1]=shardAddr  ARGV[2]=ttl_ms
var claimZone = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local owner = redis.call('HGET', KEYS[1], 'owner')
local exp = redis.call('HGET', KEYS[1], 'expires')
if owner and owner ~= ARGV[1] and exp and tonumber(exp) > now then
  return 0
end
redis.call('HSET', KEYS[1], 'owner', ARGV[1], 'expires', now + tonumber(ARGV[2]))
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]) * 3)
return 1
`)

// ClaimZone takes or renews shardAddr's exclusive lease on zoneID for ttl. It
// reports whether the claim was granted; false means another live shard owns it
// (the caller must not host the zone). Shards call this on boot and then heartbeat
// it (renewal) well within ttl.
func (r *Redis) ClaimZone(ctx context.Context, zoneID, shardAddr string, ttl time.Duration) (bool, error) {
	res, err := claimZone.Run(ctx, r.rdb, []string{r.zoneKey(zoneID)}, shardAddr, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// RegisterZone is a convenience wrapper that claims a zone with the default lease and
// turns a lost claim into ErrZoneClaimed.
func (r *Redis) RegisterZone(ctx context.Context, zoneID, shardAddr string) error {
	ok, err := r.ClaimZone(ctx, zoneID, shardAddr, DefaultZoneLease)
	if err != nil {
		return err
	}
	if !ok {
		return ErrZoneClaimed
	}
	return nil
}

// ReleaseZone gives up shardAddr's claim on zoneID (clean shutdown), but only if it
// still owns it — so it can't yank a zone a newer owner has taken over.
func (r *Redis) ReleaseZone(ctx context.Context, zoneID, shardAddr string) error {
	return releaseZone.Run(ctx, r.rdb, []string{r.zoneKey(zoneID)}, shardAddr).Err()
}

var releaseZone = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'owner') == ARGV[1] then
  redis.call('DEL', KEYS[1])
end
return 1
`)

// zoneOwner returns the current owner only if its lease is still live (judged
// against redis.call('TIME'), the same clock the claim uses), else "".
var zoneOwner = redis.NewScript(`
local owner = redis.call('HGET', KEYS[1], 'owner')
if not owner then return '' end
local exp = redis.call('HGET', KEYS[1], 'expires')
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
if not exp or tonumber(exp) <= now then return '' end
return owner
`)

// ShardForZone returns the address of the shard hosting zoneID, or ErrNotFound. It
// honors the lease against the Redis clock: an expired (un-renewed) claim reads as
// not-found, so the gate never routes a player to a shard that may be dead.
func (r *Redis) ShardForZone(ctx context.Context, zoneID string) (string, error) {
	owner, err := zoneOwner.Run(ctx, r.rdb, []string{r.zoneKey(zoneID)}).Text()
	if err != nil {
		return "", err
	}
	if owner == "" {
		return "", ErrNotFound
	}
	return owner, nil
}

// Placement is where a player currently lives and the epoch that put them there.
type Placement struct {
	ShardAddr string
	Epoch     uint64
}

// casPlacement writes {addr, epoch} only when the new epoch is strictly greater
// than any stored epoch. This makes player placement monotonic: a stale or
// duplicated handoff (lower-or-equal epoch) is a no-op, so it can never roll a
// player back to a shard they already left. Returns 1 if applied, else 0.
var casPlacement = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'epoch')
if cur and tonumber(cur) >= tonumber(ARGV[2]) then
  return 0
end
redis.call('HSET', KEYS[1], 'addr', ARGV[1], 'epoch', ARGV[2])
return 1
`)

// SetPlayerShard atomically records that playerID now lives on shardAddr as of
// epoch, iff epoch is newer than any prior placement. It reports whether the
// write applied (false means a newer/equal placement already won the race).
func (r *Redis) SetPlayerShard(ctx context.Context, playerID, shardAddr string, epoch uint64) (bool, error) {
	res, err := casPlacement.Run(ctx, r.rdb, []string{r.playerKey(playerID)}, shardAddr, epoch).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// PlayerPlacement returns where playerID currently lives, or ErrNotFound.
func (r *Redis) PlayerPlacement(ctx context.Context, playerID string) (Placement, error) {
	vals, err := r.rdb.HMGet(ctx, r.playerKey(playerID), "addr", "epoch").Result()
	if err != nil {
		return Placement{}, err
	}
	if vals[0] == nil {
		return Placement{}, ErrNotFound
	}
	addr, _ := vals[0].(string)
	var epoch uint64
	if s, ok := vals[1].(string); ok {
		epoch, _ = strconv.ParseUint(s, 10, 64)
	}
	return Placement{ShardAddr: addr, Epoch: epoch}, nil
}

// ClearPlayer removes a player's placement (on clean logout).
func (r *Redis) ClearPlayer(ctx context.Context, playerID string) error {
	return r.rdb.Del(ctx, r.playerKey(playerID)).Err()
}
