// Package placement is dynamic zone placement (docs/PLACEMENT.md, Phase 10.6): world servers CLAIM zones
// from a shared pool rather than statically DECLARE them, and a director-hosted coordinator nudges the
// fleet toward an even spread. The split is deliberate (PLACEMENT.md §3):
//
//   - LIVENESS is decentralized. A server claims unclaimed zones from the pool via the directory's
//     time-fenced CAS (ClaimZone) — the same lease that already serializes ownership. A crashed server's
//     zones become unclaimed when its leases expire and ANY live server re-claims them. Availability never
//     depends on the coordinator: claim-from-pool works with no director running.
//   - BALANCE is the coordinator's job (an OPTIMIZER, not a dependency). It observes the live fleet + the
//     zone assignment and PLANS rebalancing moves toward an even spread, with hysteresis so the fleet does
//     not thrash. If the coordinator is down, zones are still claimed + served — just possibly unbalanced.
//
// This package holds the two pure, hermetically-testable cores: ClaimFromPool (the liveness primitive a
// world server runs on boot) and Plan (the coordinator's decision engine). Executing a planned move drains
// the zone's live players via the existing cross-shard handoff — wired by the caller.
package placement

import (
	"context"
	"sort"
	"time"
)

// ZoneClaimer is the directory seam ClaimFromPool needs: the time-fenced CAS claim that gives exactly one
// owner per zone. *directory.Redis satisfies it (ClaimZone); tests inject an in-memory claimer.
type ZoneClaimer interface {
	ClaimZone(ctx context.Context, zoneID, shardID string, ttl time.Duration) (bool, error)
}

// ClaimFromPool tries to claim each zone in pool for shardID (decentralized liveness, PLACEMENT.md §4).
// It returns the zones this server WON — the set it should host. A saturated fleet (every pool zone
// already owned by a live server) yields an empty set: this server is a STANDBY (registered, heartbeating,
// holding no leases, ready to take a zone on the next failure — warm failover capacity). A claim error is
// recorded and that zone skipped (it stays unclaimed for another server / a retry), never fatal — boot
// degrades, it does not crash. The pool order is honored so a deterministic-order pool gives reproducible
// claims in a test; jitter for a real claim storm is the caller's concern (PLACEMENT.md §7).
func ClaimFromPool(ctx context.Context, claimer ZoneClaimer, shardID string, pool []string, ttl time.Duration) (won []string, errs map[string]error) {
	for _, zone := range pool {
		ok, err := claimer.ClaimZone(ctx, zone, shardID, ttl)
		if err != nil {
			if errs == nil {
				errs = map[string]error{}
			}
			errs[zone] = err
			continue
		}
		if ok {
			won = append(won, zone)
		}
	}
	return won, errs
}

// Fleet is the live-state view the coordinator observes (docs/PLACEMENT.md §2): the live shard set and
// each zone's current owner. *directory.Redis satisfies it (ListShards + ShardForZone). ShardForZone
// returns ("", false, nil) for an unclaimed zone — the coordinator treats that as needing assignment.
type Fleet interface {
	ListShards(ctx context.Context) ([]string, error)
	ShardForZone(ctx context.Context, zone string) (owner string, found bool, err error)
}

// Observe reads the current placement state for pool: the live shards and the zone→owner assignment. It is
// the coordinator's input-gathering step (kept separate from Plan so the decision engine stays pure +
// exhaustively testable). A zone with no owner — or an owner not in the live set — is left unassigned in
// the returned map, which Plan then (re)assigns (a crashed shard's zones become orphans to reclaim).
func Observe(ctx context.Context, fleet Fleet, pool []string) (live []string, assignment map[string]string, err error) {
	live, err = fleet.ListShards(ctx)
	if err != nil {
		return nil, nil, err
	}
	assignment = make(map[string]string, len(pool))
	for _, zone := range pool {
		owner, found, err := fleet.ShardForZone(ctx, zone)
		if err != nil {
			return nil, nil, err
		}
		if found {
			assignment[zone] = owner
		}
	}
	return live, assignment, nil
}

// Move is one planned zone rebalance: hand Zone from shard From to shard To (a graceful drain — the
// source is alive). The coordinator emits these; the caller executes them via the cross-shard handoff
// fanned over the zone (PLACEMENT.md §5.1).
type Move struct {
	Zone string
	From string // "" => the zone was UNCLAIMED (an assignment, not a drain; no source to drain)
	To   string
}

// RebalanceThreshold is the load-imbalance (in zone count) the coordinator tolerates before moving a zone
// — the hysteresis that stops the fleet thrashing on every tick (PLACEMENT.md §7). With a gap of 1 between
// the busiest and idlest server being acceptable, only a gap of >1 triggers a move.
const RebalanceThreshold = 1

// Plan computes the rebalancing moves toward an even spread (PLACEMENT.md §4, the coordinator's decision
// engine). Inputs: the live shard ids, the current zone→owner assignment (owner "" or a dead shard = the
// zone is unclaimed), and the full zone pool. It is PURE (no I/O) and deterministic, so it is exhaustively
// testable and a buggy/duplicated coordinator can at worst issue churny-but-bounded moves (the directory
// CAS, not the plan, is the safety arbiter — PLACEMENT.md §3).
//
// Two phases: (1) ASSIGN every unclaimed pool zone to the least-loaded live shard (From=="" — a fresh
// claim, not a drain); (2) REBALANCE — while the busiest shard exceeds the idlest by more than the
// threshold, move one zone from busiest to idlest (a graceful drain). v1 balances by zone COUNT;
// load-aware balance (player count / tick time) and locality are the documented follow-ups (PLACEMENT.md
// §7). With no live shards, Plan returns nil (nothing can be hosted).
func Plan(liveShards []string, assignment map[string]string, pool []string) []Move {
	if len(liveShards) == 0 {
		return nil
	}
	live := map[string]bool{}
	for _, s := range liveShards {
		live[s] = true
	}
	// Current load per live shard (zones it owns that are in the pool).
	load := map[string]int{}
	for _, s := range liveShards {
		load[s] = 0
	}
	owner := map[string]string{} // zone -> its live owner ("" if unclaimed/dead)
	for _, zone := range pool {
		o := assignment[zone]
		if o != "" && live[o] {
			owner[zone] = o
			load[o]++
		} else {
			owner[zone] = ""
		}
	}

	var moves []Move

	// Phase 1: assign unclaimed zones to the least-loaded shard (deterministic order for reproducibility).
	for _, zone := range pool {
		if owner[zone] != "" {
			continue
		}
		to := leastLoaded(liveShards, load)
		moves = append(moves, Move{Zone: zone, From: "", To: to})
		owner[zone] = to
		load[to]++
	}

	// Phase 2: rebalance by draining one zone at a time from the busiest to the idlest until the spread is
	// within the threshold. Bounded by the number of zones (each move strictly reduces the gap), so it
	// always terminates.
	for {
		hi := mostLoaded(liveShards, load)
		lo := leastLoaded(liveShards, load)
		if load[hi]-load[lo] <= RebalanceThreshold {
			break
		}
		zone := pickZoneOn(hi, owner, pool)
		if zone == "" {
			break // defensive: the busiest owns no movable pool zone
		}
		moves = append(moves, Move{Zone: zone, From: hi, To: lo})
		owner[zone] = lo
		load[hi]--
		load[lo]++
	}
	return moves
}

// leastLoaded returns the live shard with the fewest zones (ties broken by id for determinism).
func leastLoaded(shards []string, load map[string]int) string {
	best, bestN := "", int(^uint(0)>>1)
	for _, s := range sortedShards(shards) {
		if load[s] < bestN {
			best, bestN = s, load[s]
		}
	}
	return best
}

// mostLoaded returns the live shard with the most zones (ties broken by id for determinism).
func mostLoaded(shards []string, load map[string]int) string {
	best, bestN := "", -1
	for _, s := range sortedShards(shards) {
		if load[s] > bestN {
			best, bestN = s, load[s]
		}
	}
	return best
}

// pickZoneOn returns a pool zone currently owned by shard (deterministic order), or "".
func pickZoneOn(shard string, owner map[string]string, pool []string) string {
	for _, zone := range pool {
		if owner[zone] == shard {
			return zone
		}
	}
	return ""
}

func sortedShards(shards []string) []string {
	out := append([]string(nil), shards...)
	sort.Strings(out)
	return out
}
