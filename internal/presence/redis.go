package presence

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is the cross-process presence roster (docs/PHASE8-PLAN.md slice 8.4). It mirrors the directory's
// Redis discipline (internal/directory/redis.go): hash-per-entity keys, a per-key PEXPIRE TTL, and the
// single Redis clock (redis.call('TIME') is implicit in PTTL here) so a skewed shard clock can't mis-judge
// ownership. PERSISTENCE.md already names `presence` under the Redis/operational tier — this is its home.
//
// Keys: "<ns>:presence:<playerId>" -> hash {name, shard, afk, seen}. The TTL (PEXPIRE) is the age-out
// mechanism: a live shard refreshes it on the heartbeat; a crashed shard stops, and the key EXPIRES, so
// the player drops out of `who` with no explicit cleanup (the same lease-expiry recovery the directory
// uses for zones).
type Redis struct {
	rdb *redis.Client
	ns  string
}

// NewRedis returns a presence roster over rdb. ns namespaces all keys (default "telos"), matching the
// directory so one Redis serves both maps without collision (the presence keyspace is "<ns>:presence:*",
// the directory's is "<ns>:dir:*").
func NewRedis(rdb *redis.Client, ns string) *Redis {
	if ns == "" {
		ns = "telos"
	}
	return &Redis{rdb: rdb, ns: ns}
}

func (r *Redis) key(playerID string) string { return r.ns + ":presence:" + playerID }
func (r *Redis) prefix() string             { return r.ns + ":presence:" }

// setPresenceSrc is the per-entry write-authority guard (P8-A4). It refuses (returns 0) ONLY when a
// DIFFERENT shard currently owns the key AND that ownership is still live (PTTL > 0). An unowned key, an
// expired one (PTTL <= 0), or the caller's own key is (re)written and its TTL reset. So a shard can
// mark/refresh only the players it hosts; it can never mark an arbitrary player online or evict a real one
// a live shard owns.
//
// It is sent via EVAL (the script SOURCE) inside the batched pipeline, NOT EVALSHA: go-redis's transparent
// NOSCRIPT->EVAL fallback does not fire inside a pipeline, and miniredis does not preload scripts, so a raw
// EVALSHA could NOSCRIPT mid-pipeline. Sending the source is one round-trip for the whole heartbeat batch.
//
//	KEYS[1]=presence key
//	ARGV[1]=shardID  ARGV[2]=name  ARGV[3]=afk(0/1)  ARGV[4]=seen_ms  ARGV[5]=ttl_ms  ARGV[6]=conceal(0/1)
//	ARGV[7]=chans (comma-joined hear-set refs, #90)
const setPresenceSrc = `
local owner = redis.call('HGET', KEYS[1], 'shard')
if owner and owner ~= ARGV[1] then
  if redis.call('PTTL', KEYS[1]) > 0 then
    return 0
  end
end
redis.call('HSET', KEYS[1], 'shard', ARGV[1], 'name', ARGV[2], 'afk', ARGV[3], 'seen', ARGV[4], 'conceal', ARGV[6], 'chans', ARGV[7])
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[5]))
return 1
`

// Set refreshes every entry in the batch in ONE pipelined round-trip (the batched heartbeat: O(1) Redis
// round-trips per shard per beat, not O(players)). Each entry is written through the ownership guard; if
// any entry was refused (a key a different live shard owns), Set returns ErrNotOwner after applying the
// rest — a forged/over-reaching entry is dropped without failing the legitimate refresh.
func (r *Redis) Set(ctx context.Context, shardID string, entries []Entry, ttl time.Duration) error {
	if len(entries) == 0 {
		return nil
	}
	pipe := r.rdb.Pipeline()
	cmds := make([]*redis.Cmd, len(entries))
	for i, e := range entries {
		afk := "0"
		if e.AFK {
			afk = "1"
		}
		conceal := "0"
		if e.Concealed {
			conceal = "1"
		}
		seen := e.LastSeen
		if seen.IsZero() {
			seen = time.Now()
		}
		cmds[i] = pipe.Eval(ctx, setPresenceSrc, []string{r.key(e.PlayerID)},
			shardID, e.Name, afk, seen.UnixMilli(), ttl.Milliseconds(), conceal, strings.Join(e.Channels, ","))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		// Pipeline-level error: surface it. (Per-command refusals are not pipeline errors; they are read
		// from each cmd below.)
		return err
	}
	var refused bool
	for _, c := range cmds {
		if res, err := c.Int(); err == nil && res == 0 {
			refused = true
		}
	}
	if refused {
		return ErrNotOwner
	}
	return nil
}

// removePresence deletes the key only if the caller still owns it (mirrors the directory's releaseZone /
// deregisterShard owner-guard): a stale removal from a shard that no longer hosts the player can't evict a
// player a newer shard has since taken over.  KEYS[1]=presence key  ARGV[1]=shardID
var removePresence = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'shard') == ARGV[1] then
  redis.call('DEL', KEYS[1])
end
return 1
`)

// Remove eagerly drops playerID's presence on a clean quit/leave, but only if shardID still owns it.
func (r *Redis) Remove(ctx context.Context, shardID, playerID string) error {
	return removePresence.Run(ctx, r.rdb, []string{r.key(playerID)}, shardID).Err()
}

// List scans the presence keyspace and returns every live entry — the `who` read. A key whose TTL has
// lapsed has already been removed by Redis (PEXPIRE), so a crashed shard's players are simply absent. The
// scan is bounded by the online player count (cache/paginate at extreme scale — §3 capacity note).
func (r *Redis) List(ctx context.Context) ([]Entry, error) {
	var (
		out    []Entry
		cursor uint64
	)
	prefix := r.prefix()
	for {
		keys, next, err := r.rdb.Scan(ctx, cursor, prefix+"*", 256).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			vals, err := r.rdb.HMGet(ctx, k, "name", "shard", "afk", "seen", "conceal", "chans").Result()
			if err != nil {
				return nil, err
			}
			if vals[0] == nil && vals[1] == nil {
				continue // expired between SCAN and HMGET — race-safe: just skip it
			}
			name, _ := vals[0].(string)
			shard, _ := vals[1].(string)
			afkStr, _ := vals[2].(string)
			seenStr, _ := vals[3].(string)
			concealStr, _ := vals[4].(string)
			chansStr, _ := vals[5].(string)
			seenMs, _ := strconv.ParseInt(seenStr, 10, 64)
			var chans []string
			if chansStr != "" {
				chans = strings.Split(chansStr, ",")
			}
			out = append(out, Entry{
				PlayerID:  strings.TrimPrefix(k, prefix),
				Name:      name,
				ShardID:   shard,
				AFK:       afkStr == "1",
				Concealed: concealStr == "1",
				Channels:  chans,
				LastSeen:  time.UnixMilli(seenMs),
			})
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return out, nil
}

// Compile-time assertion.
var _ Roster = (*Redis)(nil)
