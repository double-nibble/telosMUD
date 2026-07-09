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

func (r *Redis) zoneKey(zoneID string) string      { return r.ns + ":dir:zone:" + zoneID }
func (r *Redis) shardKey(shardID string) string    { return r.ns + ":dir:shard:" + shardID }
func (r *Redis) playerKey(playerID string) string  { return r.ns + ":dir:player:" + playerID }
func (r *Redis) leaseKey(leaseID string) string    { return r.ns + ":dir:lease:" + leaseID }
func (r *Redis) drainResvKey(target string) string { return r.ns + ":dir:drainresv:" + target }
func (r *Redis) drainingKey(shardID string) string { return r.ns + ":dir:draining:" + shardID }

// ClaimLease takes or RENEWS a generic exclusive lease (Phase 10.1c leader election): it succeeds when
// the lease is free, EXPIRED, or already this owner's, then (re)sets the owner + a fresh TTL. It reuses
// the same time-fenced CAS script as ClaimZone, so exactly one owner can hold a given leaseID at a time
// — the director leader-election primitive (leaseID = the scope, owner = the director instance id).
func (r *Redis) ClaimLease(ctx context.Context, leaseID, owner string, ttl time.Duration) (bool, error) {
	res, err := claimZone.Run(ctx, r.rdb, []string{r.leaseKey(leaseID)}, owner, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// ReleaseLease frees a lease this owner holds (a graceful director resign), so a standby can take over
// immediately rather than waiting out the TTL. A no-op if owned by someone else (the CAS arbitrates).
func (r *Redis) ReleaseLease(ctx context.Context, leaseID, owner string) error {
	return releaseZone.Run(ctx, r.rdb, []string{r.leaseKey(leaseID)}, owner).Err()
}

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

// HandoverZone atomically flips a zone's live lease from one shard to another (Phase 16.4b graceful
// drain). It is a FENCED single-op CAS: the flip happens only if fromShard is STILL the live owner, and it
// sets toShard as owner with a FRESH ttl in the same script — so there is never a not-found window a peer
// could observe (unlike release-then-claim, where the destination's HostZone build would leave the zone
// ownerless for tens–hundreds of ms). Returns false (no error) when fromShard is not the current live owner
// (it already lost/expired the lease, or a newer owner took over) — the caller must then abort the handover
// rather than assume the flip happened. The destination must ALREADY host the zone (HostZone before the
// flip) so a player redirected the instant ShardForZone resolves to it can be Prepared.
func (r *Redis) HandoverZone(ctx context.Context, zoneID, fromShard, toShard string, ttl time.Duration) (bool, error) {
	res, err := handoverZone.Run(ctx, r.rdb, []string{r.zoneKey(zoneID)}, fromShard, toShard, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// handoverZone flips owner from ARGV[1] to ARGV[2] only if ARGV[1] is the current LIVE owner, setting a
// fresh ARGV[3]-ms lease. Atomic, so ShardForZone never observes an ownerless gap during the flip. Same
// redis.call('TIME') clock as claimZone. KEYS[1]=zone key  ARGV[1]=from  ARGV[2]=to  ARGV[3]=ttl_ms
var handoverZone = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local owner = redis.call('HGET', KEYS[1], 'owner')
local exp = redis.call('HGET', KEYS[1], 'expires')
if owner ~= ARGV[1] then return 0 end
if not exp or tonumber(exp) <= now then return 0 end
redis.call('HSET', KEYS[1], 'owner', ARGV[2], 'expires', now + tonumber(ARGV[3]))
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[3]) * 3)
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

// ListShards returns the ids of every LIVE shard (registered with an unexpired endpoint) — the live-fleet
// view the placement coordinator watches (docs/PLACEMENT.md §2/§4). It SCANs the shard-key namespace and
// filters by the same live-endpoint check as EndpointForShard, so a crashed shard whose registration has
// lapsed is excluded (its zones are then orphans to reassign). Order is unspecified; the coordinator sorts.
func (r *Redis) ListShards(ctx context.Context) ([]string, error) {
	prefix := r.shardKey("")
	var ids []string
	var cursor uint64
	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			shardID := k[len(prefix):]
			if shardID == "" {
				continue
			}
			// Reuse the live-endpoint judgement: a lapsed registration resolves to "" and is skipped.
			if ep, err := shardEndpoint.Run(ctx, r.rdb, []string{k}).Text(); err == nil && ep != "" {
				ids = append(ids, shardID)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return ids, nil
}

// --- Drain-target reservation + draining marker (#41) -----------------------------------------------
//
// A graceful drain hands its zones' players to a peer. When several shards drain at once (a fleet
// rolling redeploy) they must not all pick the SAME least-loaded peer and blow past its ~one-core
// occupancy — but the load signal (a peer's player count) lags by a heartbeat, so a target that just
// AGREED to absorb 50 players still reads its old, low count. The reservation bridges that blind window:
// a drainer atomically records how many players it is about to send a target; the count is folded into
// the admission decision until the target's next heartbeat reflects the real higher load. Per-TARGET and
// COUNTING (not fleet-global / single-flight) so N drains fan concurrently onto N distinct peers, and one
// fat shard's zones spread across several targets as each fills. Reservations self-expire (TTL) so a
// crashed drainer's hold clears without a deadlock; the ceiling is SOFT (the caller proceeds over it
// rather than stall a drain — a dropped socket is worse than transient overload the rebalancer corrects).

// reserveDrainTarget admits a reservation of ARGV incoming players onto a target iff the SUM of in-flight
// reservations plus incoming fits the caller-computed headroom (ceiling minus the target's current load).
// Counting via a per-drainer hash field so a drainer's holds accumulate across its zones and are cleared
// as a unit on release. Returns 1 if admitted, 0 if the target is (reservation-)full.
// KEYS[1]=drainResvKey(target)  ARGV: drainer, incoming, headroom, ttl_ms
var reserveDrainTarget = redis.NewScript(`
local vals = redis.call('HGETALL', KEYS[1])
local total = 0
for i = 2, #vals, 2 do total = total + tonumber(vals[i]) end
if total + tonumber(ARGV[2]) > tonumber(ARGV[3]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], ARGV[1], tonumber(ARGV[2]))
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[4]))
return 1
`)

// ReserveDrainTarget atomically reserves headroom on target for drainer to send `incoming` players, iff
// the in-flight reservation sum plus incoming fits `headroom` (the caller passes ceiling minus the
// target's current load; a non-positive headroom always refuses). Returns false when the target is
// reservation-full — the caller re-selects another peer or, if all are full, proceeds over the soft
// ceiling. The reservation self-expires after ttl (the backstop for a crashed drainer).
func (r *Redis) ReserveDrainTarget(ctx context.Context, target, drainer string, headroom, incoming int, ttl time.Duration) (bool, error) {
	res, err := reserveDrainTarget.Run(ctx, r.rdb, []string{r.drainResvKey(target)},
		drainer, incoming, headroom, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// ReleaseDrainTarget clears a drainer's reservation on target once its handoff completes, freeing the
// headroom immediately rather than waiting out the TTL. Best-effort (the TTL is the correctness backstop).
//
// IMPORTANT: this HDELs the drainer's WHOLE accumulated field. Because ReserveDrainTarget accumulates all of
// a drainer's zones (HINCRBY the same field), release is a WHOLE-DRAIN operation — call it ONCE per target
// at drain completion, over the set of distinct targets used, NEVER inside the per-zone loop (a per-zone
// release would wipe sibling zones' reservations and under-count headroom for concurrent drainers).
func (r *Redis) ReleaseDrainTarget(ctx context.Context, target, drainer string) error {
	return r.rdb.HDel(ctx, r.drainResvKey(target), drainer).Err()
}

// SetDraining marks shardID as draining (a marker key with a TTL backstop), so the drain-target selector
// EXCLUDES it as a candidate — otherwise a full fleet rollout could ping-pong (A drains onto B while B
// drains onto A). Cleared on drain completion; the TTL reclaims it if the drainer crashes mid-drain.
func (r *Redis) SetDraining(ctx context.Context, shardID string, ttl time.Duration) error {
	return r.rdb.Set(ctx, r.drainingKey(shardID), "1", ttl).Err()
}

// ClearDraining removes shardID's draining marker (drain completed or aborted).
func (r *Redis) ClearDraining(ctx context.Context, shardID string) error {
	return r.rdb.Del(ctx, r.drainingKey(shardID)).Err()
}

// ListDraining returns the set of shard ids currently marked draining, for the selector to exclude.
func (r *Redis) ListDraining(ctx context.Context) (map[string]bool, error) {
	prefix := r.drainingKey("")
	out := map[string]bool{}
	var cursor uint64
	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			if id := k[len(prefix):]; id != "" {
				out[id] = true
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return out, nil
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

// PlayerEpoch returns the player's last-recorded ownership epoch, or found=false (with a
// nil error) when the player has no placement yet. A thin wrapper over PlayerPlacement so
// the world can RESUME the epoch on a fresh login without importing Placement: the epoch is
// globally monotonic per player via the directory, so the next handoff's CAS (which writes
// stored+1) is accepted instead of rejected as stale.
func (r *Redis) PlayerEpoch(ctx context.Context, playerID string) (uint64, bool, error) {
	place, err := r.PlayerPlacement(ctx, playerID)
	if errors.Is(err, ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return place.Epoch, true, nil
}

// ClearPlayer removes a player's placement (on clean logout).
func (r *Redis) ClearPlayer(ctx context.Context, playerID string) error {
	return r.rdb.Del(ctx, r.playerKey(playerID)).Err()
}

// PlayerShard is the world-Locator-facing wrapper over PlayerPlacement (Phase 8.5 tell routing,
// P8-D5): it resolves which shard a player currently lives on as (shardID, found, err) without the
// caller importing Placement, mirroring PlayerEpoch's wrapper shape. found=false (nil error) when the
// player has no placement yet (a never-seen name) — the tell path refuses such a target. This is the
// EPOCH-AUTHORITATIVE player->shard map; tell routing reads it, NEVER the presence roster (P8-A4).
func (r *Redis) PlayerShard(ctx context.Context, playerID string) (string, bool, error) {
	place, err := r.PlayerPlacement(ctx, playerID)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return place.ShardID, true, nil
}
