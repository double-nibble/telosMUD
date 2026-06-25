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

// ErrShardConflict is returned when a shard id is already registered to a different,
// still-live endpoint — i.e. two distinct processes booted with the same shard id. The
// second is refused before it can claim any zone (where same-id owners would both read
// as renewals and silently become two writers). Shard ids must be unique per process.
var ErrShardConflict = errors.New("directory: shard id already registered to a different endpoint")

// DefaultZoneLease is how long a zone claim stays valid without renewal. Shards
// heartbeat well within this window; a crashed shard's claim expires after it, so a
// zone can be re-hosted elsewhere without a dead binding lingering forever.
const DefaultZoneLease = 15 * time.Second

// DefaultShardLease is how long a shard's id->endpoint registration stays resolvable
// without a heartbeat. A crashed shard's endpoint disappears after it, so callers stop
// dialing a dead address.
const DefaultShardLease = 15 * time.Second

// Redis is the Phase 2 directory: the authoritative routing maps backed by Redis
// (docs/ARCHITECTURE.md §4). Routing is two-level so a zone's logical owner is
// decoupled from where that owner currently runs:
//
//   - zone   -> shard-id   (ClaimZone / ShardForZone): the lease that says which
//     SHARD owns a zone. The owner is a stable shard id ("shard-117"), not an
//     address — so a zone's binding survives a shard moving hosts or changing port.
//   - shard-id -> endpoint (RegisterShard / EndpointForShard): where a shard is
//     reachable right now. Each shard registers and heartbeats its own endpoint.
//   - player -> shard-id   (SetPlayerShard / PlayerPlacement): where a player lives,
//     written with a monotonic epoch so a stale/duplicated handoff can never route a
//     player back to a shard they already left.
//
// So "route a player north into darkwood" resolves as zone->shard-id->endpoint:
// ShardForZone("darkwood") -> "shard-b", EndpointForShard("shard-b") -> "world-b:9090",
// then dial that. A control plane can move darkwood to another shard by rewriting one
// lease; no exit, snapshot, or peer list mentions an address.
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
func (r *Redis) shardKey(shardID string) string   { return r.ns + ":dir:shard:" + shardID }
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
// (e.g. steal a live one). KEYS[1]=zone key  ARGV[1]=shardID  ARGV[2]=ttl_ms
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

// ClaimZone takes or renews shardID's exclusive lease on zoneID for ttl. It
// reports whether the claim was granted; false means another live shard owns it
// (the caller must not host the zone). Shards call this on boot and then heartbeat
// it (renewal) well within ttl. The owner recorded is the stable shard id; resolve it
// to a dial endpoint with EndpointForShard.
func (r *Redis) ClaimZone(ctx context.Context, zoneID, shardID string, ttl time.Duration) (bool, error) {
	res, err := claimZone.Run(ctx, r.rdb, []string{r.zoneKey(zoneID)}, shardID, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// RegisterZone is a convenience wrapper that claims a zone with the default lease and
// turns a lost claim into ErrZoneClaimed.
func (r *Redis) RegisterZone(ctx context.Context, zoneID, shardID string) error {
	ok, err := r.ClaimZone(ctx, zoneID, shardID, DefaultZoneLease)
	if err != nil {
		return err
	}
	if !ok {
		return ErrZoneClaimed
	}
	return nil
}

// ReleaseZone gives up shardID's claim on zoneID (clean shutdown), but only if it
// still owns it — so it can't yank a zone a newer owner has taken over.
func (r *Redis) ReleaseZone(ctx context.Context, zoneID, shardID string) error {
	return releaseZone.Run(ctx, r.rdb, []string{r.zoneKey(zoneID)}, shardID).Err()
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

// ShardForZone returns the id of the shard hosting zoneID, or ErrNotFound. It honors
// the lease against the Redis clock: an expired (un-renewed) claim reads as not-found,
// so a caller never routes to a shard that may be dead. Resolve the returned shard id
// to a dial endpoint with EndpointForShard.
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

// registerShard records/refreshes shardID's reachable endpoint with a TTL. A shard
// owns its own id, so a write of the SAME endpoint is a plain renewal. It returns 0
// only when a DIFFERENT endpoint already holds a still-live registration under this id
// — two processes sharing a shard id — so the second is refused. A same-endpoint
// restart (same pod/address) renews cleanly; a fully-lapsed id is reusable (the old
// holder is gone, so it can't be a live second writer). All time via redis.call('TIME').
// KEYS[1]=shard key  ARGV[1]=endpoint  ARGV[2]=ttl_ms
var registerShard = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local cur = redis.call('HGET', KEYS[1], 'endpoint')
local exp = redis.call('HGET', KEYS[1], 'expires')
if cur and cur ~= ARGV[1] and exp and tonumber(exp) > now then
  return 0
end
redis.call('HSET', KEYS[1], 'endpoint', ARGV[1], 'expires', now + tonumber(ARGV[2]))
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]) * 3)
return 1
`)

// RegisterShard publishes (or heartbeats) where shardID is reachable for ttl. Shards
// call this on boot — before claiming any zone — and then renew it well within ttl, so
// a zone's owner is always resolvable to a live endpoint. It returns ErrShardConflict
// if another live process already holds this id under a different endpoint (the guard
// that keeps a duplicated shard id from silently becoming two writers).
func (r *Redis) RegisterShard(ctx context.Context, shardID, endpoint string, ttl time.Duration) error {
	res, err := registerShard.Run(ctx, r.rdb, []string{r.shardKey(shardID)}, endpoint, ttl.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if res == 0 {
		return ErrShardConflict
	}
	return nil
}

// shardEndpoint returns the registered endpoint only while its registration is still
// live (judged against redis.call('TIME')), else "".
var shardEndpoint = redis.NewScript(`
local ep = redis.call('HGET', KEYS[1], 'endpoint')
if not ep then return '' end
local exp = redis.call('HGET', KEYS[1], 'expires')
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
if not exp or tonumber(exp) <= now then return '' end
return ep
`)

// EndpointForShard resolves a shard id to its current dial endpoint, or ErrNotFound if
// the shard is unregistered or its registration has lapsed (it may have crashed).
func (r *Redis) EndpointForShard(ctx context.Context, shardID string) (string, error) {
	ep, err := shardEndpoint.Run(ctx, r.rdb, []string{r.shardKey(shardID)}).Text()
	if err != nil {
		return "", err
	}
	if ep == "" {
		return "", ErrNotFound
	}
	return ep, nil
}

var deregisterShard = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'endpoint') == ARGV[1] then
  redis.call('DEL', KEYS[1])
end
return 1
`)

// DeregisterShard removes shardID's endpoint registration (clean shutdown), so callers
// stop resolving it immediately instead of waiting out the TTL. It only removes the
// registration if it still points at endpoint — so a dying process can't delete the
// fresh registration of a same-id replacement that has already taken over.
func (r *Redis) DeregisterShard(ctx context.Context, shardID, endpoint string) error {
	return deregisterShard.Run(ctx, r.rdb, []string{r.shardKey(shardID)}, endpoint).Err()
}

// Placement is which shard a player currently lives on and the epoch that put them
// there. ShardID is a stable shard id; resolve it to a dial endpoint with
// EndpointForShard.
type Placement struct {
	ShardID string
	Epoch   uint64
}

// casPlacement writes {shard, epoch} only when the new epoch is strictly greater
// than any stored epoch. This makes player placement monotonic: a stale or
// duplicated handoff (lower-or-equal epoch) is a no-op, so it can never roll a
// player back to a shard they already left. Returns 1 if applied, else 0.
var casPlacement = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'epoch')
if cur and tonumber(cur) >= tonumber(ARGV[2]) then
  return 0
end
redis.call('HSET', KEYS[1], 'shard', ARGV[1], 'epoch', ARGV[2])
return 1
`)

// SetPlayerShard atomically records that playerID now lives on shardID as of
// epoch, iff epoch is newer than any prior placement. It reports whether the
// write applied (false means a newer/equal placement already won the race).
func (r *Redis) SetPlayerShard(ctx context.Context, playerID, shardID string, epoch uint64) (bool, error) {
	res, err := casPlacement.Run(ctx, r.rdb, []string{r.playerKey(playerID)}, shardID, epoch).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// PlayerPlacement returns which shard playerID currently lives on, or ErrNotFound.
func (r *Redis) PlayerPlacement(ctx context.Context, playerID string) (Placement, error) {
	vals, err := r.rdb.HMGet(ctx, r.playerKey(playerID), "shard", "epoch").Result()
	if err != nil {
		return Placement{}, err
	}
	if vals[0] == nil {
		return Placement{}, ErrNotFound
	}
	shardID, _ := vals[0].(string)
	var epoch uint64
	if s, ok := vals[1].(string); ok {
		epoch, _ = strconv.ParseUint(s, 10, 64)
	}
	return Placement{ShardID: shardID, Epoch: epoch}, nil
}

// ClearPlayer removes a player's placement (on clean logout).
func (r *Redis) ClearPlayer(ctx context.Context, playerID string) error {
	return r.rdb.Del(ctx, r.playerKey(playerID)).Err()
}
