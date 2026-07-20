package directory

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// eviction.go — the startup check that tells an operator their directory Redis is configured to evict
// coordination state (#340).
//
// # The directory is coordination state, not cache
//
// Everything in this package is a fact the fleet cannot re-derive. Redis is not a memoization layer here;
// it is the only place several of these facts exist. Losing a key is not a slow path, it is a wrong answer:
//
//   - `dir:zone:<id>` (no TTL, PERSISTed) holds the lease OWNER and its monotonic GENERATION — the fence
//     that makes a signed AdoptZone a single-use token for one specific handover rather than a standing
//     capability to force a shard to host a zone (#315).
//   - `dir:player:<id>` (no TTL) holds the placement epoch that stops a stale handoff from routing a player
//     back to a shard they already left.
//
// And the TTL'd ones, which are the reason `volatile-*` is refused as well. Each fails OPEN when missing —
// its absence is not an error, it is a confident wrong answer:
//
//   - `dir:lease:<id>` is the DIRECTOR'S LEADER-ELECTION lease. claimLease refuses only when a DIFFERENT
//     owner holds a LIVE lease, so if the key is evicted the incumbent's renewal and a standby's claim both
//     succeed and both believe they lead. Two live directors issuing rebalance directives against the same
//     zones is the worst outcome on this list, and it falsifies election.go's stated invariant that the CAS
//     is the final arbiter.
//   - `dir:shard:<id>` is the endpoint registration. registerShard refuses a DIFFERENT endpoint only while a
//     live registration exists, so eviction lets two processes sharing a shard id both register — defeating
//     the guard that keeps a duplicated shard id from silently becoming two writers.
//   - `dir:tmplinuse:<template>` says parties are inside live instances of a template (#416). It carries the
//     SHORTEST TTL in the directory, so `volatile-ttl` evicts it first by construction, and its absence is
//     indistinguishable from "nobody is running copies of this" — letting a content pull strip the pack out
//     from under them.
//   - `dir:draining:<shard>` marks a shard as draining; losing it sends peers at a shard on its way out.
//   - `dir:occ:<zone>` and `dir:cooldown:<zone>` feed the rebalancer: losing them makes a busy zone read as
//     empty, and removes the anti-thrash guard — and every extra rebalance is an ownership change that bumps
//     a zone's lease generation.
//   - `dir:drainresv:<target>` holds reserved drain headroom (#284). Listed last deliberately: that ceiling
//     is documented as SOFT (a caller proceeds over it rather than stall a drain), so its loss is real but
//     not in the same class as the two above it.
//
// # This Redis is not only the directory
//
// The same instance carries the CHECKPOINT tier — full character JSON on a 1h TTL, the ~10s rung of the
// durability ladder. Under any `volatile-*` policy those are prime eviction candidates, and losing them
// silently demotes that rung to save-on-logout: a state-rollback surface across a crash. So the argument for
// noeviction here is broader than the directory's own keys.
//
// # Why PERSIST is not enough, and why volatile-* is no longer enough either
//
// PERSIST defeats TTL expiry and `volatile-*` eviction, which is why the zone hash carries it. It does NOT
// defeat `allkeys-*` eviction under maxmemory, FLUSHDB/FLUSHALL, a failover to a replica that lost writes,
// or an RDB/AOF restore from an older point.
//
// #340 originally concluded "noeviction, or a volatile-* policy — never allkeys-*". That was right for the
// zone hash alone and wrong for the directory as a whole: the TTL'd keys listed above are first-class
// `volatile-*` eviction candidates, and the leader-election lease and the shard registration are each a
// single-writer guard that eviction turns off.
//
// # Whether this REFUSES THE BOOT depends on whether the coordination Redis is DEDICATED (#429)
//
// A boot refusal was the obvious shape and it is the wrong one today, for a reason that is about this
// deployment rather than about the check: telos-world points ONE Redis client at four tiers — the directory,
// the checkpoint tier (full character JSON on a 1h TTL, the dominant consumer by volume), the presence
// roster, and telos-account's device-auth codes. Config exposes a single `Addr` with no DB index, so the
// eviction policy is necessarily instance-wide.
//
// Mandating `noeviction` on that instance is not obviously safe, and can be worse than the thing it
// prevents. With no `maxmemory` ceiling, `noeviction` never returns OOM to clients — Redis simply grows
// until the container limit and is OOM-KILLED, which wipes every zone lease, generation and placement epoch
// at once. That is the FLUSHALL-equivalent this issue exists to prevent, caused by the remedy. With a
// ceiling set, every write at the ceiling errors, including lease renewal — which fences shards fleet-wide,
// simultaneously, since they share the instance.
//
// So an operator running `allkeys-lru` on a SHARED instance may be doing the correct thing for the Redis
// they actually have, and refusing to boot would order them into a worse configuration.
//
// #429 supplied the missing prerequisite: `redis.directory_addr` points the coordination keyspace at its own
// instance. That splits the population in two and the gate follows it exactly —
//
//   - SHARED (no directory_addr): warn-only, as before. The tradeoff above is real and the operator may have
//     resolved it correctly for their deployment; a loud, specific, actionable Error line is the honest
//     ceiling on what this can claim to know.
//   - DEDICATED (directory_addr set): FATAL. The tradeoff has been dissolved — nothing on that instance wants
//     eviction, it is small and bounded, and an evicting policy there is simply a misconfiguration. There is
//     no worse configuration to be ordered into, so refusing to start is now the safe direction rather than
//     the reckless one.
//
// A gate whose severity tracks whether its own prerequisite is met, rather than being flipped by hand later
// and forgotten.
//
// # The check also RE-RUNS periodically, and that half must never fence
//
// A boot-only check cannot see `CONFIG SET maxmemory-policy allkeys-lru` on a live fleet — which is exactly
// how this misconfiguration gets introduced, by an operator responding to a memory alert — and shards run for
// weeks. So the shard re-checks on a slow tick and WARNS. That half is warn-only on BOTH shapes, dedicated or
// not, and deliberately so: a live shard owns zone leases and player sockets, and exiting under it would turn
// a configuration mistake into a player-visible outage plus an unplanned lease handover. Boot is the only
// place refusing is cheap.
//
// # What the loss of `gen` would actually do
//
// If the generation restarted from zero the zone would churn 1 → 2 → 3…, and on passing the value some
// long-captured AdoptZone was signed at, that request would verify AGAIN. An Ed25519 signature never
// expires and the handoff mesh is plaintext gRPC, so capture is realistic for an on-path attacker. The
// generation is seeded from the Redis CLOCK rather than from zero for exactly this reason
// (TestZoneLeaseGenerationNeverRetreadsAValueAfterAReset), so a wipe restarts the counter far above every
// value ever issued. That is a strong backstop, not a licence to lose the key — its residual assumption is
// that the directory's clock does not jump backwards by more than the zone's ownership-change count.

// EvictionFinding is the result of the startup check: whether the directory Redis will evict coordination
// state, and what to tell the operator. Unsafe is true ONLY when eviction can actually occur.
type EvictionFinding struct {
	Policy    string // the reported maxmemory-policy, "" if it could not be read
	MaxMemory int64  // the reported maxmemory in bytes; 0 means no ceiling, so eviction never happens
	Unsafe    bool   // an evicting policy AND a ceiling for it to act at
	Unknown   bool   // the config could not be read (managed Redis commonly disables CONFIG GET)
}

// EvictionGate is the BOOT decision derived from a finding (#429), factored out of the hosts so the
// fail-closed behavior is unit-testable — the same shape handoffAuthGate uses in cmd/telos-world.
//
// dedicated says the coordination keyspace has its own Redis instance (redis.directory_addr). It is the only
// input beyond the finding, and it is what decides whether an evicting policy is a refusal or a warning; see
// the file header.
//
// A non-nil fatal means "do not start". Everything else has already been logged by CheckEvictionPolicy.
func EvictionGate(f EvictionFinding, dedicated bool) (fatal error) {
	if !f.Unsafe || !dedicated {
		return nil
	}
	return fmt.Errorf("the DEDICATED coordination redis has maxmemory-policy %q with a %d-byte ceiling, so "+
		"it will discard coordination state under memory pressure: a lost zone lease generation, placement "+
		"epoch, leader-election lease or instance-template claim is a WRONG answer, not a slow one. Nothing "+
		"on this instance wants eviction — it holds only the directory, and it is small and bounded — so set "+
		"`maxmemory-policy noeviction` on it. (Set a `maxmemory` ceiling too, with headroom, so noeviction "+
		"returns OOM to clients rather than growing until the container OOM-kills it and wipes everything at "+
		"once.)", f.Policy, f.MaxMemory)
}

// CheckEvictionPolicy reads the directory Redis's eviction configuration and LOGS what it finds. It never
// returns an error and never prevents startup by itself — the boot decision is EvictionGate's, and whether it
// refuses depends on the coordination Redis being dedicated (see the file header). The finding is returned so
// a caller (or a test) can make that decision and assert on it.
//
// It reads BOTH the policy and the ceiling, because the policy alone cannot answer the question. With
// `maxmemory 0` — the default — eviction cannot occur at all, no matter what the policy says, so an
// evicting policy on an uncapped instance is a latent config smell rather than an active hazard, and
// reporting it as active would cry wolf on the most common shape of the problem.
//
// An unreadable config is reported as UNKNOWN, not as safe. `CONFIG GET` is disabled or restricted on most
// managed Redis offerings (ElastiCache, Memorystore, Upstash) — and notably ElastiCache DEFAULTS to
// `volatile-lru`, so the platforms where this check is blind are exactly the platforms whose defaults are
// wrong. The operator is told to verify by hand.
func (r *Redis) CheckEvictionPolicy(ctx context.Context) EvictionFinding {
	policy, err := r.evictionPolicy(ctx)
	if err != nil {
		slog.Warn("could not read the directory redis maxmemory-policy; VERIFY BY HAND that it is "+
			"`noeviction` — the directory holds coordination state (zone lease generations, placement epochs, "+
			"drain reservations, instance-template claims) and an eviction policy silently turns those into "+
			"wrong answers rather than slow ones (#340). CONFIG GET is commonly disabled on managed Redis, and "+
			"ElastiCache defaults to volatile-lru, so this is worth checking rather than assuming",
			"err", err)
		return EvictionFinding{Unknown: true}
	}
	maxMemory := r.maxMemory(ctx) // unreadable => 0 => treated as no ceiling
	f := classifyEviction(policy, maxMemory)

	switch {
	case f.Unsafe:
		slog.Error("THE DIRECTORY REDIS IS CONFIGURED TO EVICT COORDINATION STATE. maxmemory-policy is "+
			"an evicting policy and a maxmemory ceiling is set, so it WILL discard directory keys under "+
			"memory pressure — losing a zone lease generation, a placement epoch, a drain reservation or an "+
			"instance-template claim produces a WRONG answer, not a slow one (#340). Set `maxmemory-policy "+
			"noeviction`. If this Redis is ALSO serving the checkpoint/presence tiers, split them first "+
			"(`redis.directory_addr`, #429) — noeviction on a cache-sized instance risks an OOM-kill that "+
			"wipes the whole directory, which is worse than the problem. If it is ALREADY dedicated, the boot "+
			"gate that follows this line refuses the start outright",
			"maxmemory_policy", f.Policy, "maxmemory_bytes", f.MaxMemory)
	case f.Policy != "" && isEvictingPolicy(f.Policy):
		slog.Warn("the directory redis has an evicting maxmemory-policy, but no maxmemory ceiling, so nothing "+
			"is evicted today. Anyone who later sets a ceiling turns this into silent coordination-state loss "+
			"— set `maxmemory-policy noeviction` now (#340)",
			"maxmemory_policy", f.Policy)
	default:
		slog.Debug("directory redis eviction policy verified", "maxmemory_policy", f.Policy)
	}
	return f
}

// evictionRecheckInterval is how often a running process re-reads the policy (#429).
//
// Slow on purpose. This is watching for an OPERATOR ACTION (`CONFIG SET` in response to a memory alert), not
// for a fast-moving signal, so minutes of detection lag costs nothing and a tight loop would add a CONFIG
// round trip to a coordination Redis for no benefit.
const evictionRecheckInterval = 5 * time.Minute

// WatchEvictionPolicy re-checks the policy on a slow tick until ctx is done, logging through
// CheckEvictionPolicy exactly as the boot check does. Intended to run in its own goroutine for the life of
// the process.
//
// IT NEVER FENCES, NEVER EXITS AND NEVER RETURNS AN ERROR, on either the shared or the dedicated shape, and
// that asymmetry with the boot gate is the design rather than an oversight. A live shard owns zone leases and
// player sockets; a running process that killed itself over a configuration change would convert an operator
// mistake into a player-visible outage plus an unplanned lease handover — strictly worse than the eviction
// risk it is reacting to, and self-inflicted at the worst moment (the fleet is already under memory
// pressure, which is why someone reached for CONFIG SET). Boot is the only place where refusing is cheap,
// because nothing is depending on the process yet.
func (r *Redis) WatchEvictionPolicy(ctx context.Context) {
	t := time.NewTicker(evictionRecheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Bounded, so a hung Redis cannot wedge this goroutine; the next tick simply tries again.
			c, cancel := context.WithTimeout(ctx, 3*time.Second)
			r.CheckEvictionPolicy(c)
			cancel()
		}
	}
}

// evictionPolicy reads the server's maxmemory-policy, trying CONFIG GET first and falling back to
// INFO memory.
//
// The fallback is what makes this check work where it matters most. `CONFIG` is a restricted command on
// ElastiCache and blocked on Memorystore — and ElastiCache DEFAULTS to `volatile-lru`, so a CONFIG-only
// probe is blind on exactly the platform whose default is unsafe. `INFO memory` reports the same value as
// `maxmemory_policy:` and is readable there.
func (r *Redis) evictionPolicy(ctx context.Context) (string, error) {
	if v, err := r.configString(ctx, "maxmemory-policy"); err == nil && strings.TrimSpace(v) != "" {
		return v, nil
	}
	info, err := r.rdb.Info(ctx, "memory").Result()
	if err != nil {
		return "", err
	}
	return infoField(info, "maxmemory_policy"), nil
}

// infoField pulls one `name:value` field out of an INFO section. Returns "" when absent, which the caller
// treats as unknown rather than as safe.
func infoField(info, name string) string {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, name+":")
		if ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// classifyEviction is the decision itself, split from the round trips so it is directly testable — miniredis
// does not implement CONFIG, so a fake server cannot exercise this through CheckEvictionPolicy.
func classifyEviction(rawPolicy string, maxMemory int64) EvictionFinding {
	policy := strings.ToLower(strings.TrimSpace(rawPolicy))
	f := EvictionFinding{Policy: policy, MaxMemory: maxMemory}
	if policy == "" {
		// The server answered without telling us anything: the same state of knowledge as a failed read.
		f.Unknown = true
		return f
	}
	// Unsafe needs BOTH: a policy that evicts, and a ceiling for it to evict at.
	f.Unsafe = isEvictingPolicy(policy) && maxMemory > 0
	return f
}

// isEvictingPolicy reports whether a maxmemory-policy discards keys. Both families count: `allkeys-*` can
// take the PERSISTed zone hash, and `volatile-*` can take any of the directory's TTL'd keys.
func isEvictingPolicy(policy string) bool {
	return strings.HasPrefix(policy, "allkeys-") || strings.HasPrefix(policy, "volatile-")
}

// configString reads one CONFIG GET value.
func (r *Redis) configString(ctx context.Context, param string) (string, error) {
	vals, err := r.rdb.ConfigGet(ctx, param).Result()
	if err != nil {
		return "", err
	}
	return vals[param], nil
}

// maxMemory reads the configured memory ceiling in bytes, CONFIG GET first then INFO memory, returning 0
// for anything unreadable or unparsable.
//
// 0 is the same as "no ceiling", which is the right direction to guess: it downgrades a finding from unsafe
// to a warning rather than inventing an active hazard out of a parse failure.
func (r *Redis) maxMemory(ctx context.Context) int64 {
	raw, err := r.configString(ctx, "maxmemory")
	if err != nil || strings.TrimSpace(raw) == "" {
		info, ierr := r.rdb.Info(ctx, "memory").Result()
		if ierr != nil {
			return 0
		}
		raw = infoField(info, "maxmemory")
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if perr != nil {
		return 0
	}
	return n
}
