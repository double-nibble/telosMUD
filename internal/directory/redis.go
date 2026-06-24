package directory

import (
	"context"
	"errors"
	"strconv"

	"github.com/redis/go-redis/v9"
)

// ErrNotFound is returned when a zone or player has no recorded placement.
var ErrNotFound = errors.New("directory: not found")

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

// RegisterZone records that the shard at shardAddr hosts zoneID. Idempotent; a
// shard re-registers its zones on every boot.
func (r *Redis) RegisterZone(ctx context.Context, zoneID, shardAddr string) error {
	return r.rdb.Set(ctx, r.zoneKey(zoneID), shardAddr, 0).Err()
}

// ShardForZone returns the address of the shard hosting zoneID, or ErrNotFound.
func (r *Redis) ShardForZone(ctx context.Context, zoneID string) (string, error) {
	addr, err := r.rdb.Get(ctx, r.zoneKey(zoneID)).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotFound
	}
	return addr, err
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
