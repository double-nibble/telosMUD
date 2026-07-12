package directory

import (
	"context"
	"errors"
	"fmt"
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
func (r *Redis) occKey(zoneID string) string       { return r.ns + ":dir:occ:" + zoneID }
func (r *Redis) rebalanceKey(zoneID string) string { return r.ns + ":dir:rebalance:" + zoneID }
func (r *Redis) cooldownKey(zoneID string) string  { return r.ns + ":dir:cooldown:" + zoneID }

// claimLease is the generic time-fenced CAS: take the lease when it is free, EXPIRED, or already ours, then
// (re)set owner + a fresh TTL. Same shape and same redis.call('TIME') clock as claimZone, minus the zone's
// generation counter — a leader-election lease has no fence token to carry, and it wants the key TTL for GC.
// KEYS[1]=lease key  ARGV[1]=owner  ARGV[2]=ttl_ms
var claimLease = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local owner = redis.call('HGET', KEYS[1], 'owner')
local exp = redis.call('HGET', KEYS[1], 'expires')
if owner and owner ~= ARGV[1] and exp and tonumber(exp) > now then
  return 0
end
redis.call('HSET', KEYS[1], 'owner', ARGV[1], 'expires', now + tonumber(ARGV[2]))
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]) + 5000)
return 1
`)

// releaseLease drops a lease this owner still holds, so a standby can take it immediately.
var releaseLease = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'owner') == ARGV[1] then
  redis.call('DEL', KEYS[1])
end
return 1
`)

// ClaimLease takes or RENEWS a generic exclusive lease (Phase 10.1c leader election): it succeeds when
// the lease is free, EXPIRED, or already this owner's, then (re)sets the owner + a fresh TTL — so exactly one
// owner can hold a given leaseID at a time (the director leader-election primitive: leaseID = the scope,
// owner = the director instance id).
func (r *Redis) ClaimLease(ctx context.Context, leaseID, owner string, ttl time.Duration) (bool, error) {
	res, err := claimLease.Run(ctx, r.rdb, []string{r.leaseKey(leaseID)}, owner, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// ReleaseLease frees a lease this owner holds (a graceful director resign), so a standby can take over
// immediately rather than waiting out the TTL. A no-op if owned by someone else (the CAS arbitrates).
func (r *Redis) ReleaseLease(ctx context.Context, leaseID, owner string) error {
	return releaseLease.Run(ctx, r.rdb, []string{r.leaseKey(leaseID)}, owner).Err()
}

// claimZone takes/renews an exclusive lease on a zone. It succeeds when the zone is
// unowned, its lease has expired, or the caller is already the owner (a renewal);
// it fails (returns 0) only when a DIFFERENT shard holds a still-live lease. This is
// what stops two shards from both claiming one zone (the cardinal single-writer
// guarantee, one level up from players).
//
// It also maintains `gen`, the zone's monotonic lease GENERATION (#315). It bumps on every ownership CHANGE
// — a fresh claim, a takeover after the lease lapsed — but NOT on a renewal by the current owner, because an
// AdoptZone signed at generation N must survive until its own handover flips it.
//
// `gen` must NEVER re-tread a value it has already issued, because an Ed25519 signature does not expire: a
// captured AdoptZone bound to generation N goes live again the moment the counter returns to N. Two defenses,
// because one is not enough:
//
//   - the key is PERSISTed, not TTL'd, so it does not lapse out from under a quiet zone; and
//   - on FIRST creation `gen` is SEEDED from the Redis clock (ms since epoch) rather than starting at 0.
//
// The seed is what survives the events PERSIST cannot: FLUSHDB, an allkeys-* eviction, a failover to a replica
// that lost writes, a restore from an older snapshot, a changed key namespace. After any of those the counter
// restarts from the CURRENT millisecond, which is far above every value it has ever issued, so no historical
// request can ever match again. (The residual assumption is that the directory's clock does not jump backwards
// by more than the zone's ownership-change count — orders of magnitude weaker than trusting two shards' clocks
// to agree, which is what #262 required.)
//
// Reading TIME in a write script requires EFFECTS replication (Redis 5+, mandatory in 7+), which the drain
// reservations already depend on. The seed makes the anti-replay property depend on it too. `gen` stays an
// exact integer for ~250 years: HINCRBY is a signed int64, and ms-since-epoch (~1.7e12) plus any plausible
// number of ownership changes stays far inside both that range and Lua's exact-integer range.
//
// Liveness still comes from `expires` alone (zoneOwner ignores an owner whose lease has lapsed), so a persisted
// key holds no authority once its lease is stale — it is one small hash per zone id, and zone ids are a bounded
// content set.
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
if owner ~= ARGV[1] then
  if redis.call('HEXISTS', KEYS[1], 'gen') == 0 then
    redis.call('HSET', KEYS[1], 'gen', now)
  end
  redis.call('HINCRBY', KEYS[1], 'gen', 1)
end
redis.call('HSET', KEYS[1], 'owner', ARGV[1], 'expires', now + tonumber(ARGV[2]))
redis.call('PERSIST', KEYS[1])
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

// releaseZone drops the OWNERSHIP fields but never the key: `gen` must survive, or a captured AdoptZone
// bound to generation N could be replayed once the counter restarted from zero (#315). A released zone reads
// as unowned anyway — zoneOwner returns ” without a live `expires` — so nothing routes to it. The key is one
// small hash per zone id, and zone ids are a bounded content set.
var releaseZone = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'owner') == ARGV[1] then
  redis.call('HDEL', KEYS[1], 'owner', 'expires')
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
// fresh ARGV[3]-ms lease, and bumps the zone's lease generation (#315) — the bump is what consumes the
// AdoptZone this flip completes. Atomic, so ShardForZone never observes an ownerless gap during the flip.
// Same redis.call('TIME') clock as claimZone.
//
// A self-handover (from == to) is refused HERE, not just in the Go caller: it is not an ownership change, so
// bumping the generation for it would invalidate every in-flight AdoptZone for the zone while flipping
// nothing. (It would also re-set the TTL of a lease its owner has stopped renewing — the #42 review guard.)
// KEYS[1]=zone key  ARGV[1]=from  ARGV[2]=to  ARGV[3]=ttl_ms
var handoverZone = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local owner = redis.call('HGET', KEYS[1], 'owner')
local exp = redis.call('HGET', KEYS[1], 'expires')
if ARGV[1] == ARGV[2] then return 0 end
if owner ~= ARGV[1] then return 0 end
if not exp or tonumber(exp) <= now then return 0 end
redis.call('HINCRBY', KEYS[1], 'gen', 1)
redis.call('HSET', KEYS[1], 'owner', ARGV[2], 'expires', now + tonumber(ARGV[3]))
redis.call('PERSIST', KEYS[1])
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

// zoneLease reads {owner, gen} for a zone, honoring the lease expiry: a lapsed owner reads as ”. gen is
// returned regardless, because it must be readable for an unowned zone too.
var zoneLease = redis.NewScript(`
local owner = redis.call('HGET', KEYS[1], 'owner')
local exp = redis.call('HGET', KEYS[1], 'expires')
local gen = redis.call('HGET', KEYS[1], 'gen')
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
if (not owner) or (not exp) or tonumber(exp) <= now then owner = '' end
return { owner, gen or '0' }
`)

// ZoneLease returns a zone's live owner ("" when unowned or lapsed) and its lease GENERATION (#315).
//
// The generation is monotonic per zone and increments on every ownership CHANGE: a claim by a shard that did
// not already own it (including a takeover after the lease lapsed) and the HandoverZone flip. A renewal by
// the current owner does NOT bump it — otherwise an AdoptZone signed at generation N would go stale before
// its destination could verify it. It is seeded from the Redis clock on first claim (see claimZone), so the
// absolute value is meaningless; only "did it change" is.
//
// It is the fence token AdoptZone binds: the source signs the generation it observes while still holding the
// lease, and the destination checks it against this. The moment the flip lands the generation moves, so the
// request is dead — a replay is unrepresentable rather than merely time-bounded.
//
// A zone that has never been claimed reads ("", 0).
func (r *Redis) ZoneLease(ctx context.Context, zoneID string) (owner string, gen uint64, err error) {
	vals, err := zoneLease.Run(ctx, r.rdb, []string{r.zoneKey(zoneID)}).Slice()
	if err != nil {
		return "", 0, err
	}
	if len(vals) != 2 {
		return "", 0, fmt.Errorf("zone lease %q: malformed reply", zoneID)
	}
	owner, _ = vals[0].(string)
	genStr, _ := vals[1].(string)
	if genStr != "" {
		gen, _ = strconv.ParseUint(genStr, 10, 64)
	}
	return owner, gen, nil
}

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

// reserveDrainTarget admits a reservation of ARGV incoming players onto a target iff the SUM of the LIVE
// in-flight reservations plus incoming fits the caller-computed headroom (ceiling minus the target's current
// load). Counting via a per-drainer hash field so a drainer's holds accumulate across its zones and are
// cleared as a unit on release. Returns 1 if admitted, 0 if the target is (reservation-)full.
//
// EACH FIELD CARRIES ITS OWN EXPIRY, as "<count>:<expires_at_ms>" (#284). The previous shape stored a bare
// count and refreshed the WHOLE key's PEXPIRE on every reserve — so under the fleet rollout this guard exists
// for, a crashed drainer's stale hold survived indefinitely as long as OTHER drainers kept reserving onto the
// same hot target. The reserved sum stayed inflated, the target refused real reservations, and more drainers
// spilled to the soft-ceiling fallback. The guard was weakest exactly under the concurrency it is for. Now a
// stale field is excluded from the sum and pruned on sight. A field's own expiry is REFRESHED on each of its
// drainer's reserves — that drainer is demonstrably alive.
//
// TIME COMES FROM THE SERVER, not the caller. Every drainer's expiry is then measured against ONE clock, so
// inter-drainer skew cannot corrupt the cross-drainer sum. Passing each drainer's own `now` would have made a
// SLOW clock the dangerous case: a correct-clock peer would prune a live drainer's hold early, under-count the
// target, and let a peer pile on — an unsafe direction, unbounded on a host with no NTP. (A fast clock merely
// over-counts, which is conservative.) redis.call('TIME') before a write is allowed under effects replication,
// the default since Redis 5, and miniredis dispatches it in EVAL (verified) with SetTime as the test seam.
//
// The KEY's expiry is set to the MAX live field expiry plus a grace, so the key always outlives every field it
// holds. A blanket PEXPIRE would let a short-ttl reserve stomp the key's expiry below a longer-lived field's,
// evicting a live hold early.
//
// KEYS[1]=drainResvKey(target)  ARGV: drainer, incoming, headroom, ttl_ms
var reserveDrainTarget = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local ttl = tonumber(ARGV[4])
local vals = redis.call('HGETALL', KEYS[1])
local total = 0
local mine = 0
local maxexp = 0
for i = 1, #vals, 2 do
  local field = vals[i]
  local v = vals[i+1]
  local sep = string.find(v, ':', 1, true)
  local cnt, exp
  if sep then
    cnt = tonumber(string.sub(v, 1, sep - 1))
    exp = tonumber(string.sub(v, sep + 1))
  else
    cnt = tonumber(v)  -- a legacy bare-count field written before this shape; treat as expiring now
    exp = 0
  end
  if (not cnt) or (not exp) or exp <= now then
    redis.call('HDEL', KEYS[1], field)
  else
    if field == ARGV[1] then
      mine = cnt
    else
      if exp > maxexp then maxexp = exp end
    end
    total = total + cnt
  end
end
if total + tonumber(ARGV[2]) > tonumber(ARGV[3]) then
  if maxexp > 0 then redis.call('PEXPIREAT', KEYS[1], maxexp + 1000) end
  return 0
end
local myexp = now + ttl
redis.call('HSET', KEYS[1], ARGV[1], (mine + tonumber(ARGV[2])) .. ':' .. myexp)
if myexp > maxexp then maxexp = myexp end
redis.call('PEXPIREAT', KEYS[1], maxexp + 1000)
return 1
`)

// ReserveDrainTarget atomically reserves headroom on target for drainer to send `incoming` players, iff
// the LIVE in-flight reservation sum plus incoming fits `headroom` (the caller passes ceiling minus the
// target's current load; a non-positive headroom always refuses). Returns false when the target is
// reservation-full — the caller re-selects another peer or, if all are full, proceeds over the soft
// ceiling. Each reservation carries a per-field expiry of ttl, measured against the REDIS server's clock,
// so a crashed drainer's hold stops counting after its own ttl regardless of what other drainers do.
func (r *Redis) ReserveDrainTarget(ctx context.Context, target, drainer string, headroom, incoming int, ttl time.Duration) (bool, error) {
	res, err := reserveDrainTarget.Run(ctx, r.rdb, []string{r.drainResvKey(target)},
		drainer, incoming, headroom, ttl.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// ReleaseDrainTarget clears a drainer's reservation on target OUTRIGHT.
//
// Use it only when the target will receive NO players from this drainer — the reservation was made and then
// abandoned (a lapsed endpoint mid-selection, a handover that failed). Its hold is pure waste and should go
// immediately.
//
// Do NOT use it when the handover SUCCEEDED. The reservation exists to bridge the window between the players
// landing on the target and the target's presence heartbeat reflecting their weight; deleting the hold the
// moment the drain completes reopens exactly that window, and a concurrent drainer would read the target's
// stale (low) load with no reservation to correct it, and over-commit. Use ExpireDrainTargetSoon instead.
//
// This deletes the drainer's WHOLE accumulated field. ReserveDrainTarget accumulates all of a drainer's zones
// into one field, so release is a WHOLE-DRAIN operation — once per target at drain completion, over the set
// of distinct targets touched, NEVER inside the per-zone loop (a per-zone delete would wipe the sibling
// zones' reservations and under-count headroom for concurrent drainers).
func (r *Redis) ReleaseDrainTarget(ctx context.Context, target, drainer string) error {
	return r.rdb.HDel(ctx, r.drainResvKey(target), drainer).Err()
}

// expireDrainTargetSoon rewrites a drainer's field to expire at now+ttl_ms, keeping its count. Fenced: it
// touches nothing if the field is absent. Never EXTENDS an expiry — a shorter one already in place wins.
// KEYS[1]=drainResvKey(target)  ARGV: drainer, ttl_ms, now_ms
var expireDrainTargetSoon = redis.NewScript(`
local v = redis.call('HGET', KEYS[1], ARGV[1])
if not v then return 0 end
local sep = string.find(v, ':', 1, true)
if not sep then return 0 end
local cnt = tonumber(string.sub(v, 1, sep - 1))
local exp = tonumber(string.sub(v, sep + 1))
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local want = now + tonumber(ARGV[2])
if (not cnt) or (not exp) or want >= exp then return 0 end
redis.call('HSET', KEYS[1], ARGV[1], cnt .. ':' .. want)
return 1
`)

// ExpireDrainTargetSoon shortens a drainer's reservation on target to expire in `in`, instead of running out
// its full TTL (#284).
//
// This is the right move once a drain's handoffs have COMPLETED. The reservation's whole job is to bridge the
// blind window between the migrated players landing on the target and the target's next presence heartbeat
// reflecting their weight. Deleting the hold at drain completion would reopen that window — a concurrent
// drainer reads the target's stale, low load, finds no reservation, and over-commits. But leaving it for the
// full TTL double-counts the players for the remainder, once presence HAS caught up, which needlessly denies
// a peer real headroom. Shortening it to roughly one heartbeat threads both.
//
// Never extends: a field already expiring sooner is left alone. A missing field is a no-op. Reports whether
// it rewrote anything.
func (r *Redis) ExpireDrainTargetSoon(ctx context.Context, target, drainer string, in time.Duration) (bool, error) {
	res, err := expireDrainTargetSoon.Run(ctx, r.rdb, []string{r.drainResvKey(target)},
		drainer, in.Milliseconds()).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
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

// --- Zone occupancy signal (#42) --------------------------------------------------------------------
//
// The placement coordinator balances the fleet by per-zone WEIGHT (PlanWeighted), but weight needs a live
// signal: how many players are actually in each zone. The owning shard publishes its hosted zones' player
// counts here on the same heartbeat cadence as its lease renewal; the coordinator reads the whole map to
// weight the plan. Each key carries a TTL so a crashed shard's occupancy ages out (its zones then read as
// unweighted → weight 1, the zone-count default) rather than lingering as phantom load.

// SetZoneOccupancy publishes the current player count for a zone this shard owns, with a TTL so a crashed
// owner's signal ages out. Called on the zone-lease renewal cadence (off the zone goroutine; pop is atomic).
func (r *Redis) SetZoneOccupancy(ctx context.Context, zoneID string, players int, ttl time.Duration) error {
	return r.rdb.Set(ctx, r.occKey(zoneID), players, ttl).Err()
}

// ZoneOccupancies returns every live zone -> player count, the weight signal the placement coordinator
// feeds to PlanWeighted. A zone whose owner crashed (its key lapsed) is simply absent → the planner defaults
// it to weight 1. Order is unspecified.
func (r *Redis) ZoneOccupancies(ctx context.Context) (map[string]int, error) {
	prefix := r.occKey("")
	out := map[string]int{}
	var cursor uint64
	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, prefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			zoneID := k[len(prefix):]
			if zoneID == "" {
				continue
			}
			n, err := r.rdb.Get(ctx, k).Int()
			if err != nil {
				continue // lapsed between the SCAN and the GET, or a non-int value — skip it
			}
			out[zoneID] = n
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return out, nil
}

// --- Rebalance directive + cooldown (#42 slice 3) ---------------------------------------------------
//
// The placement coordinator (leader) balances the fleet by draining a single zone from a busy shard to an
// idler peer. It has NO shard->director RPC, so it publishes the move as a DIRECTIVE keyed by zone; the
// zone's CURRENT owner — the only shard that renews that zone's lease — reads it on its per-zone renewal
// tick and executes the drain. The directive is a HINT that TRIGGERS the already-fenced HandoverZone flip;
// it is NEVER authority for ownership (the lease CAS is), so no lost/dup/reordered directive can double-own.
// The cooldown (keyed by ZONE so it survives the ownership change) stops a boundary zone ping-ponging.

// IssueRebalance records/refreshes a directive to move zoneID to toShard (stable shard id — the owner
// resolves its endpoint at execution). ATOMIC (a single SET with expiry) so a crash can never leave a
// directive with no TTL. Idempotent by zone: a repeat with the same toShard just refreshes the TTL; a
// different toShard is last-write-wins. The TTL is drain-deadline-scoped, so a crashed owner's directive
// self-expires. Written by the coordinator leader.
func (r *Redis) IssueRebalance(ctx context.Context, zoneID, toShard string, ttl time.Duration) error {
	return r.rdb.Set(ctx, r.rebalanceKey(zoneID), toShard, ttl).Err()
}

// ReadRebalance returns the target shard of a pending rebalance directive for zoneID, or found=false if
// none. Read by the owning shard (to act) and by the coordinator (to skip re-issuing an in-flight move).
func (r *Redis) ReadRebalance(ctx context.Context, zoneID string) (toShard string, found bool, err error) {
	to, err := r.rdb.Get(ctx, r.rebalanceKey(zoneID)).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return to, to != "", nil
}

// refreshRebalance re-arms the directive TTL only if it still points at toShard — so an in-flight drain's
// heartbeat can't keep alive a directive the coordinator has since re-pointed at a different target.
// KEYS[1]=rebalanceKey  ARGV[1]=toShard  ARGV[2]=ttl_ms
var refreshRebalance = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]))
  return 1
end
return 0
`)

// RefreshRebalance re-arms a directive's TTL while the drain to toShard is in-flight (owner heartbeat),
// fenced so it no-ops if the directive was re-pointed or cleared.
func (r *Redis) RefreshRebalance(ctx context.Context, zoneID, toShard string, ttl time.Duration) error {
	return refreshRebalance.Run(ctx, r.rdb, []string{r.rebalanceKey(zoneID)}, toShard, ttl.Milliseconds()).Err()
}

// clearRebalance deletes the directive only if it still points at toShard (a fenced clear, so a completing
// drain can't wipe a directive the coordinator has since re-pointed at a new target). KEYS[1]=rebalanceKey
// ARGV[1]=toShard
var clearRebalance = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('DEL', KEYS[1])
end
return 1
`)

// ClearRebalance removes zoneID's directive once its drain to toShard completes (fenced by toShard).
func (r *Redis) ClearRebalance(ctx context.Context, zoneID, toShard string) error {
	return clearRebalance.Run(ctx, r.rdb, []string{r.rebalanceKey(zoneID)}, toShard).Err()
}

// SetCooldown marks zoneID as recently rebalanced, so the coordinator won't move it again until the TTL
// elapses (anti-ping-pong). Keyed by zone so it survives the ownership change; set by the coordinator at
// issue time. NOT read by the failover claim path (a crashed shard's zone is always reassignable).
func (r *Redis) SetCooldown(ctx context.Context, zoneID string, ttl time.Duration) error {
	return r.rdb.Set(ctx, r.cooldownKey(zoneID), "1", ttl).Err()
}

// OnCooldown reports whether zoneID is within its post-rebalance cooldown window.
func (r *Redis) OnCooldown(ctx context.Context, zoneID string) (bool, error) {
	n, err := r.rdb.Exists(ctx, r.cooldownKey(zoneID)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Placement is which shard a player currently lives on and the epoch that put them
// there. ShardID is a stable shard id; resolve it to a dial endpoint with
// EndpointForShard.
type Placement struct {
	ShardID string
	// ZoneID is the zone the player was last resident in (#320). It is the ROUTING key a reconnect
	// should use: a shard id goes stale the moment the zone is rebalanced onto another shard, whereas
	// the zone is stable and ShardForZone resolves its current owner. Empty on a legacy record written
	// before this field existed — callers fall back to ShardID.
	ZoneID string
	Epoch  uint64
	// Nonce is the per-session value the last registering session stamped (#329). It is the axis that lets a
	// clean-logout tombstone fence a same-shard/same-epoch relog. 0 on a legacy or handoff-CAS-only record
	// that has never been through registerPlacement.
	Nonce uint64
}

// casPlacement writes {shard, zone, epoch} only when the new epoch is strictly greater
// than any stored epoch. This makes player placement monotonic: a stale or
// duplicated handoff (lower-or-equal epoch) is a no-op, so it can never roll a
// player back to a shard they already left. Returns 1 if applied, else 0.
//
// It DELETES the session nonce (#329). The handoff bumps the epoch onto a new shard whose destination
// session has not yet registered, so any nonce still in the record belongs to the SOURCE session and is now
// stale. Leaving it would make clearPlayerShard's present-only fence see a present-but-stale nonce, and a
// destination player who quits before their register drains (the coalescing writer collapses register+clear
// to the clear) would carry the destination nonce, mismatch the stale source nonce, and be wrongly fenced —
// leaving a permanent stale `shard` field. Deleting it restores the genuine "no nonce yet" state the
// present-only fence is built for: the destination's own clear then tombstones on shard+epoch, and the
// destination's register re-stamps the real nonce. A same-shard relog (constant epoch) never runs this CAS,
// so the #329 fence it enables is untouched.
var casPlacement = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'epoch')
if cur and tonumber(cur) >= tonumber(ARGV[3]) then
  return 0
end
redis.call('HSET', KEYS[1], 'shard', ARGV[1], 'zone', ARGV[2], 'epoch', ARGV[3])
redis.call('HDEL', KEYS[1], 'nonce')
return 1
`)

// registerPlacement is the LOGIN / zone-change write (#320). It differs from casPlacement in exactly
// one way: it accepts an EQUAL epoch. A login legitimately re-registers at the epoch it just resumed
// from this very directory (server.go reads PlayerEpoch, the zone seeds the session at that value), so
// demanding a strictly greater epoch would make every login a silent no-op.
//
// It still cannot clobber a NEWER placement: an in-flight handoff writes epoch+1 through casPlacement,
// and a login arriving at the older epoch sees `cur > mine` and refuses. The stored epoch never
// decreases (it is kept at the max), so the monotonic fence the handoff CAS depends on survives.
//
// It also stamps a per-session NONCE (ARGV[4], #329). Every register overwrites it with the registering
// session's value, so the record's nonce always names the session that most recently became resident. That
// is what lets clearPlayerShard fence a SAME-shard, SAME-epoch relog: a late logout carrying the prior
// session's nonce no longer matches the record a fresh login rewrote. The nonce is written only when the
// register applies (an epoch that loses to a newer one leaves it untouched).
//
// Returns 1 if applied, 0 if a newer epoch already owns the record.
var registerPlacement = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], 'epoch')
local mine = tonumber(ARGV[3])
if cur and tonumber(cur) > mine then
  return 0
end
local keep = mine
if cur and tonumber(cur) > keep then keep = tonumber(cur) end
redis.call('HSET', KEYS[1], 'shard', ARGV[1], 'zone', ARGV[2], 'epoch', keep, 'nonce', ARGV[4])
return 1
`)

// SetPlayerShard atomically records that playerID now lives on shardID, in zoneID, as of
// epoch, iff epoch is newer than any prior placement. It reports whether the
// write applied (false means a newer/equal placement already won the race).
func (r *Redis) SetPlayerShard(ctx context.Context, playerID, shardID, zoneID string, epoch uint64) (bool, error) {
	res, err := casPlacement.Run(ctx, r.rdb, []string{r.playerKey(playerID)}, shardID, zoneID, epoch).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// RegisterPlacement records that playerID is resident on shardID in zoneID at epoch (#320). It is called
// when a player JOINS a zone — a fresh login, a link-dead resume, or an intra-shard zone transfer — none
// of which advance the ownership epoch, so it accepts an equal epoch where SetPlayerShard (the
// cross-shard handoff CAS) demands a strictly greater one.
//
// Before #320 the ONLY writer of this hash was the handoff CAS. A player who had never been handed off
// therefore had NO placement: unroutable on reconnect, and invisible to the tell/mail existence oracle,
// which answered "there is no player by that name" for them. This is the write that fixes both.
//
// Blocking Redis I/O: call it OFF the zone goroutine.
func (r *Redis) RegisterPlacement(ctx context.Context, playerID, shardID, zoneID string, epoch, nonce uint64) (bool, error) {
	res, err := registerPlacement.Run(ctx, r.rdb, []string{r.playerKey(playerID)}, shardID, zoneID, epoch, nonce).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// clearPlayerShard is the CLEAN-LOGOUT TOMBSTONE (#70). It removes only the `shard` field, and only when
// that field still names the caller AT the caller's epoch AND session nonce — a compare-and-delete, not a
// blind HDEL.
//
// The fence matters. A clear racing a concurrent re-login (the player reconnected elsewhere and their new
// shard already registered) reads a different shard, or a newer epoch, and no-ops. So a late-draining
// logout write can never evict a live placement.
//
// THE NONCE AXIS (#329) closes the one case shard+epoch could not: a SAME-shard relog resumes the SAME
// epoch (registerPlacement accepts an equal epoch by design), so a late clear would match a live record on
// both older axes. Each register stamps the registering session's nonce, so the relog's fresh nonce no
// longer matches the quitting session's — the clear is fenced. The check is present-only: a record with NO
// nonce (a handoff-CAS-only record before its destination has registered, or a legacy pre-#329 record) is
// still clearable on shard+epoch alone. That is safe — those records have no concurrent same-shard relog to
// protect against — and it keeps the handoff CAS (which writes no nonce) working unchanged.
//
// `epoch`, `zone`, and `nonce` deliberately SURVIVE the tombstone:
//   - epoch is the monotonic fence the handoff CAS compares against. Deleting the key would let a delayed
//     or retried SetPlayerShard from an earlier handoff find `cur == nil` and APPLY, resurrecting a stale
//     placement.
//   - zone is the reconnect routing key (#320). A returning player must still resolve to whoever owns the
//     zone they logged out in.
//   - nonce identifies the session that owned the record; keeping it is harmless (the next register
//     overwrites it) and dropping it would only widen the present-only fence's blind spot.
//
// Returns 1 if the field was cleared, 0 if the fence rejected it.
var clearPlayerShard = redis.NewScript(`
local shard = redis.call('HGET', KEYS[1], 'shard')
local epoch = redis.call('HGET', KEYS[1], 'epoch')
local nonce = redis.call('HGET', KEYS[1], 'nonce')
if shard ~= ARGV[1] then return 0 end
if not epoch or tonumber(epoch) ~= tonumber(ARGV[3]) then return 0 end
if nonce and nonce ~= ARGV[4] then return 0 end
if ARGV[2] ~= '' then
  redis.call('HSET', KEYS[1], 'zone', ARGV[2])
end
redis.call('HDEL', KEYS[1], 'shard')
return 1
`)

// ClearPlayerShard tombstones a cleanly-logged-out player: it drops the `shard` field iff the record still
// names shardID at epoch, keeping `epoch` (the handoff fence) and recording zoneID as the zone they logged
// out in (the reconnect routing key, #320).
//
// It WRITES the zone rather than merely preserving it because the world's placement writer coalesces per
// player: a logout enqueued while a zone-change registration is still pending would otherwise replace it,
// and the record would name the zone the player walked out of. Passing the quitting zone here makes the
// tombstone carry the same information the superseded registration would have. An empty zoneID leaves the
// stored zone untouched.
//
// The `nonce` is the quitting session's per-session value (#329). The clear applies only if it matches the
// record's nonce (or the record has none), so a same-shard/same-epoch relog — which rewrote the nonce — is
// fenced out.
//
// Call it ONLY on a clean, player-initiated quit — never on link death, never mid-handoff. Blocking Redis
// I/O: off the zone goroutine.
func (r *Redis) ClearPlayerShard(ctx context.Context, playerID, shardID, zoneID string, epoch, nonce uint64) (bool, error) {
	res, err := clearPlayerShard.Run(ctx, r.rdb, []string{r.playerKey(playerID)}, shardID, zoneID, epoch, nonce).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// PlayerPlacement returns where playerID currently lives (shard, zone, epoch), or ErrNotFound.
//
// EXISTENCE IS KEYED ON `epoch`, NOT `shard` (#70). A cleanly-logged-out player is tombstoned by dropping
// the shard field, so `shard` is absent for anyone offline — but the character still EXISTS, and the
// tell/mail existence oracle (PlayerShard) must keep saying so, or tells to offline players would be
// refused with "there is no player by that name". `epoch` is written by every placement write and never
// removed except by a full ClearPlayer, so its presence is the character's existence.
//
// A caller therefore sees ShardID == "" for an offline player. ZoneID is "" only on a legacy record written
// before #320 added the field.
func (r *Redis) PlayerPlacement(ctx context.Context, playerID string) (Placement, error) {
	vals, err := r.rdb.HMGet(ctx, r.playerKey(playerID), "shard", "epoch", "zone", "nonce").Result()
	if err != nil {
		return Placement{}, err
	}
	if vals[1] == nil {
		return Placement{}, ErrNotFound
	}
	shardID, _ := vals[0].(string)
	var epoch uint64
	if s, ok := vals[1].(string); ok {
		epoch, _ = strconv.ParseUint(s, 10, 64)
	}
	zoneID, _ := vals[2].(string)
	var nonce uint64
	if s, ok := vals[3].(string); ok {
		nonce, _ = strconv.ParseUint(s, 10, 64)
	}
	return Placement{ShardID: shardID, ZoneID: zoneID, Epoch: epoch, Nonce: nonce}, nil
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

// ClearPlayer DELETES a player's whole placement record — epoch, zone and shard.
//
// This is CHARACTER DELETION, not logout. A clean logout uses ClearPlayerShard, which tombstones only the
// shard field. Deleting the record outright is destructive in two ways that are easy to miss:
//   - it drops the monotonic `epoch`, so a delayed or retried SetPlayerShard from an earlier handoff finds
//     `cur == nil` and APPLIES, resurrecting a stale placement;
//   - it makes the character non-existent to the tell/mail existence oracle, so a tell to them is refused
//     with "there is no player by that name".
//
// Both are correct for a deleted character and wrong for a logged-out one.
func (r *Redis) ClearPlayer(ctx context.Context, playerID string) error {
	return r.rdb.Del(ctx, r.playerKey(playerID)).Err()
}

// PlayerShard is the world-Locator-facing wrapper over PlayerPlacement (Phase 8.5 tell routing,
// P8-D5): it resolves which shard a player currently lives on as (shardID, found, err) without the
// caller importing Placement, mirroring PlayerEpoch's wrapper shape. This is the EPOCH-AUTHORITATIVE
// player->shard map; tell routing reads it, NEVER the presence roster (P8-A4).
//
// found reports EXISTENCE, not presence. found=false (nil error) means a never-seen name, and the tell path
// refuses such a target. A character who has logged in and cleanly quit is found=true with shardID == ""
// (#70's tombstone): they exist, they are addressable, and their tell drains from the durable subject when
// they next log in. Callers that need "is currently hosted somewhere" must check shardID != "" as well —
// and callers that need "is currently CONNECTED" want the presence roster, not this (#325).
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
