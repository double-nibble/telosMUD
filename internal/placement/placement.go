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
	"fmt"
	"log/slog"
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

// RebalanceThreshold is the FLOOR imbalance tolerated before a rebalance move — the hysteresis that stops
// the fleet thrashing over a 1-unit gap (PLACEMENT.md §7). The EFFECTIVE threshold is WEIGHT-PROPORTIONAL
// (rebalanceThreshold): the larger the per-shard load, the larger the gap tolerated. This is the #42
// prerequisite for driving real drains — once the coordinator balances by player WEIGHT (PlanWeighted), a
// fixed 1-player threshold would migrate a zone to shave a single player off the busiest shard (thrash). The
// floor keeps the pure zone-COUNT plan on small fleets unchanged (there mean load is tiny, so the floor
// dominates and only a gap of >1 triggers a move, exactly as before).
const RebalanceThreshold = 1

// rebalanceFraction is the share of the MEAN per-shard load tolerated as imbalance before a rebalance: a gap
// smaller than ~this fraction of the average shard's load is not worth a migration. 0.15 tolerates a ~15%
// spread — so a 1000-player-per-shard fleet tolerates a ~150-player gap, not a 1-player one.
const rebalanceFraction = 0.15

// rebalanceThreshold is the effective imbalance tolerated for a load distribution: the larger of the
// absolute floor (RebalanceThreshold) and rebalanceFraction of the mean per-shard load. total is the summed
// weight across all live shards (invariant under Phase-2 moves, which only shift weight between shards), so
// it is computed once per plan. Pure + deterministic.
func rebalanceThreshold(total, shards int) int {
	if shards <= 0 {
		return RebalanceThreshold
	}
	prop := int(rebalanceFraction * (float64(total) / float64(shards)))
	if prop > RebalanceThreshold {
		return prop
	}
	return RebalanceThreshold
}

// Plan computes the rebalancing moves toward an even spread (PLACEMENT.md §4, the coordinator's decision
// engine). Inputs: the live shard ids, the current zone→owner assignment (owner "" or a dead shard = the
// zone is unclaimed), and the full zone pool. It is PURE (no I/O) and deterministic, so it is exhaustively
// testable and a buggy/duplicated coordinator can at worst issue churny-but-bounded moves (the directory
// CAS, not the plan, is the safety arbiter — PLACEMENT.md §3).
//
// Two phases: (1) ASSIGN every unclaimed pool zone to the least-loaded live shard (From=="" — a fresh
// claim, not a drain); (2) REBALANCE — while the busiest shard exceeds the idlest by more than the
// threshold, move one zone from busiest to idlest (a graceful drain). Plan balances by zone COUNT; use
// PlanWeighted to balance by per-zone player load. With no live shards, Plan returns nil.
func Plan(liveShards []string, assignment map[string]string, pool []string) []Move {
	return PlanWeighted(liveShards, assignment, pool, nil)
}

// PlanWeighted is Plan balancing by per-zone WEIGHT (e.g. live player count / tick cost) instead of raw
// zone count — so a busy newbie town counts more than an empty wilderness (PLACEMENT.md §7 load-aware
// balance). zoneWeight[zone] is the zone's weight; a zone absent from the map (or weight <= 0) defaults to
// 1, so a nil map reproduces the zone-COUNT Plan exactly (a nil map also keeps the FIXED rebalance floor —
// the weight-proportional threshold applies only to a real weight map). Still PURE + deterministic (the
// directory CAS is the safety arbiter, not the plan). Use PlanColocated to also prefer region-colocation.
func PlanWeighted(liveShards []string, assignment map[string]string, pool []string, zoneWeight map[string]int) []Move {
	return PlanColocated(liveShards, assignment, pool, zoneWeight, nil)
}

// PlanColocated is PlanWeighted that also prefers LOCALITY (#42): when a Phase-2 rebalance drains a zone off
// the busiest shard, it prefers to move a zone whose REGION-MATES (zoneRegion[zone]=region) already sit on
// the destination, so the load-driven move also colocates the region — keeping its cross-zone / region-state
// traffic in-process. Locality biases only WHICH zone is moved; the DESTINATION stays the strict least-
// loaded, so Phase-2's termination proof is untouched, and it never trades load for locality. (It is applied
// in Phase 2, the coordinator-DRIVEN path, not Phase-1 assignment — a From=="" assignment is claimed
// decentrally by the world, which the coordinator does not steer, so a Phase-1 preference would be inert.)
// A nil zoneRegion (or all-region-less zones) reproduces PlanWeighted exactly. Still PURE + deterministic.
func PlanColocated(liveShards []string, assignment map[string]string, pool []string, zoneWeight map[string]int, zoneRegion map[string]string) []Move {
	if len(liveShards) == 0 {
		return nil
	}
	live := map[string]bool{}
	for _, s := range liveShards {
		live[s] = true
	}
	weight := func(zone string) int {
		if w, ok := zoneWeight[zone]; ok && w > 0 {
			return w
		}
		return 1 // an unweighted/empty zone still costs 1 slot, so a nil map == the zone-count plan
	}
	// Current load per live shard = the summed WEIGHT of the pool zones it owns.
	load := map[string]int{}
	for _, s := range liveShards {
		load[s] = 0
	}
	// rc[shard][region] = how many pool zones the shard owns in that region — the locality signal.
	rc := map[string]map[string]int{}
	for _, s := range liveShards {
		rc[s] = map[string]int{}
	}
	owner := map[string]string{} // zone -> its live owner ("" if unclaimed/dead)
	for _, zone := range pool {
		o := assignment[zone]
		if o != "" && live[o] {
			owner[zone] = o
			load[o] += weight(zone)
			if r := zoneRegion[zone]; r != "" {
				rc[o][r]++
			}
		} else {
			owner[zone] = ""
		}
	}

	var moves []Move

	// Phase 1: assign unclaimed zones to the least-loaded shard (deterministic order for reproducibility).
	// Locality is deliberately NOT applied here: a From=="" assignment is claimed DECENTRALLY by the world
	// (ClaimFromPool), which the coordinator does not drive, so a locality preference on Phase-1 placement
	// would be computed-but-inert. Locality lives in Phase 2, the coordinator-DRIVEN path. (rc is still
	// maintained so Phase 2 sees the true region layout.)
	for _, zone := range pool {
		if owner[zone] != "" {
			continue
		}
		to := leastLoaded(liveShards, load)
		moves = append(moves, Move{Zone: zone, From: "", To: to})
		owner[zone] = to
		load[to] += weight(zone)
		if r := zoneRegion[zone]; r != "" {
			rc[to][r]++
		}
	}

	// Phase 2: rebalance by draining one zone at a time from the busiest to the idlest until the spread is
	// within the threshold. Each move strictly reduces the gap (a moved zone's weight leaves hi and joins
	// lo), so it terminates in a bounded number of moves. The threshold is WEIGHT-PROPORTIONAL only when
	// balancing by real weights (#42) — a fixed 1-player threshold would migrate a zone to shave a single
	// player off the busiest shard (thrash). The pure zone-COUNT plan (a nil weight map) keeps the fixed
	// floor, so its "within 1 zone" contract is unchanged at every scale. All zones are assigned by now, so
	// the total load is final and invariant under the moves below (they only shift weight between shards).
	threshold := RebalanceThreshold
	if zoneWeight != nil {
		total := 0
		for _, s := range liveShards {
			total += load[s]
		}
		threshold = rebalanceThreshold(total, len(liveShards))
	}
	for {
		hi := mostLoaded(liveShards, load)
		lo := leastLoaded(liveShards, load)
		gap := load[hi] - load[lo]
		if gap <= threshold {
			break
		}
		// Pick a movable zone (weight < gap) on hi to drain to the strict least-loaded lo. LOCALITY (#42):
		// among the movable zones, prefer one whose region-mates ALREADY sit on lo, so the rebalance move
		// improves colocation as well as load. The DESTINATION is unchanged (strict least-loaded), so Phase-2
		// termination is untouched — each move still strictly shrinks the gap. With no region map this reduces
		// to the heaviest-movable choice (fewest moves), so PlanWeighted is unchanged.
		zone := pickColocatingZone(hi, lo, owner, zoneRegion, pool, weight, gap, rc)
		if zone == "" {
			break
		}
		w := weight(zone)
		if r := zoneRegion[zone]; r != "" {
			rc[hi][r]-- // the zone leaves hi's region tally and joins lo's
			rc[lo][r]++
		}
		moves = append(moves, Move{Zone: zone, From: hi, To: lo})
		owner[zone] = lo
		load[hi] -= w
		load[lo] += w
	}
	return moves
}

// pickColocatingZone chooses which movable zone on `hi` (weight strictly < gap, so the move reduces the
// spread and Phase 2 terminates) to drain to `lo`. It prefers a zone whose region-mates ALREADY sit on `lo`
// (rc[lo][region]) — so the load-driven move ALSO improves colocation (#42) — and among equally-colocating
// candidates the HEAVIEST (fastest gap reduction), ties by pool order for determinism. With no region info
// (rc[lo][region]==0 for every candidate, e.g. a nil zoneRegion) it reduces to the heaviest-movable choice,
// so PlanWeighted is unchanged. "" when no zone on hi has weight < gap (an indivisible-heavy stall).
func pickColocatingZone(hi, lo string, owner, zoneRegion map[string]string, pool []string, weight func(string) int, gap int, rc map[string]map[string]int) string {
	best, bestMates, bestW := "", -1, -1
	for _, zone := range pool {
		if owner[zone] != hi {
			continue
		}
		w := weight(zone)
		if w >= gap {
			continue // moving it would over-correct (flip the imbalance to lo) — the termination guard
		}
		mates := 0
		if r := zoneRegion[zone]; r != "" {
			mates = rc[lo][r]
		}
		if mates > bestMates || (mates == bestMates && w > bestW) {
			best, bestMates, bestW = zone, mates, w
		}
	}
	return best
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

func sortedShards(shards []string) []string {
	out := append([]string(nil), shards...)
	sort.Strings(out)
	return out
}

// --- Drain-target selection (#41) -------------------------------------------------------------------
//
// When a shard drains it hands each zone's players to a live PEER. The selection is DIRECTOR-OWNED policy
// (this package is the director's decision engine) executed DECENTRALIZED on the draining shard, serialized
// against simultaneous drains through the directory's counting reservation — NOT a synchronous RPC to the
// director, because a drain is liveness-critical and must keep working when the director is itself mid-
// rollout (PLACEMENT.md §3: liveness never depends on the coordinator). SelectDrainTarget is the pure core;
// ChooseDrainTarget composes it with the reservation + endpoint resolution over injected seams.

// ShardLoad is a live candidate shard and its current player occupancy — the load signal drain-target
// selection minimizes. It is a struct (not a bare map[string]int) so #42 can add per-zone weight / locality
// tags behind the same SelectDrainTarget signature without churning callers.
type ShardLoad struct {
	ShardID string
	Players int
}

// SelectDrainTarget picks the least-loaded candidate to absorb `incoming` players, excluding `self`, ties
// broken by shard id for determinism. It is PURE (no I/O). overCeiling reports that even the least-loaded
// pick would exceed `ceiling` once it absorbs `incoming` — the caller still gets that pick (progress beats
// a SOFT ceiling: a dropped socket is worse than transient overload the rebalancer corrects), it is just
// signalled to proceed without a reservation gate. target is "" only when there is no candidate but self.
func SelectDrainTarget(cands []ShardLoad, self string, incoming, ceiling int) (target string, overCeiling bool) {
	best, bestN := "", int(^uint(0)>>1)
	for _, c := range sortedByID(cands) {
		if c.ShardID == self {
			continue
		}
		if c.Players < bestN {
			best, bestN = c.ShardID, c.Players
		}
	}
	if best == "" {
		return "", false
	}
	return best, bestN+incoming > ceiling
}

// DrainFleet supplies the live drain-target candidates + endpoint resolution. Excludes lapsed registrations
// AND currently-draining shards (so a fleet rollout can't ping-pong A<->B). An adapter over the directory +
// presence roster satisfies it; tests inject an in-memory fake.
type DrainFleet interface {
	DrainCandidates(ctx context.Context) ([]ShardLoad, error)
	EndpointForShard(ctx context.Context, shardID string) (string, error)
}

// DrainReserver is the directory's counting reservation seam (serializes simultaneous drains onto one
// target). *directory.Redis satisfies it.
type DrainReserver interface {
	ReserveDrainTarget(ctx context.Context, target, drainer string, headroom, incoming int, ttl time.Duration) (bool, error)
	// ReleaseDrainTarget drops a reservation this selector made on a target it then ABANDONED (its endpoint
	// lapsed between the candidate read and the resolve). Without it the hold leaks until its TTL — the very
	// stale hold #284 exists to remove, and triggered by the endpoint race that is most likely during the
	// fleet rollout the guard is for.
	ReleaseDrainTarget(ctx context.Context, target, drainer string) error
}

// ChooseDrainTarget selects + reserves a live peer for `self` to hand `incoming` players (one draining zone)
// to. It reads the candidates, picks least-loaded via SelectDrainTarget, and atomically reserves that
// target's headroom (ceiling minus its current load); if a concurrent drainer already filled it, it drops
// that target and re-selects. It NEVER stalls a drain: if every candidate is reservation-full — or the
// least-loaded is already over the soft ceiling — it proceeds on the overall least-loaded anyway (progress
// beats the ceiling; the reservation is still recorded best-effort so a following drainer sees the load).
// Reservations self-expire (ttl); the fast-path release on completion is the caller's / a #42 concern.
// Returns the target id + dial endpoint, or an error only when NO live non-draining peer exists at all.
func ChooseDrainTarget(ctx context.Context, fleet DrainFleet, resv DrainReserver, self string, incoming, ceiling int, ttl time.Duration) (shardID, addr string, err error) {
	cands, err := fleet.DrainCandidates(ctx)
	if err != nil {
		return "", "", err
	}
	// Phase 1 — place UNDER the soft ceiling, least-loaded first. Each pass reserves the pick's headroom and
	// takes it if admitted (or if the pick is already over the raw ceiling — then no under-ceiling placement
	// exists, so proceed anyway). A reservation-refused (a concurrent drainer filled it) or lapsed-endpoint
	// target is dropped and re-selected. `remaining` shrinks each iteration, so the loop terminates.
	remaining := withoutSelf(cands, self)
	for len(remaining) > 0 {
		target, over := SelectDrainTarget(remaining, self, incoming, ceiling)
		if target == "" {
			break
		}
		ok, rerr := resv.ReserveDrainTarget(ctx, target, self, ceiling-loadOf(remaining, target), incoming, ttl)
		if rerr != nil {
			return "", "", rerr
		}
		if ok || over {
			if id, a, e := resolveOrDrop(ctx, fleet, target); e == nil {
				return id, a, nil
			}
		}
		// We are abandoning this target. If our reserve was ADMITTED, we are holding headroom on a shard we
		// will never send players to — release it now rather than leak it until the TTL (#284). A refused
		// reserve wrote nothing, so there is nothing to release.
		if ok {
			releaseAbandoned(ctx, resv, target, self)
		}
		remaining = dropShard(remaining, target)
	}

	// Phase 2 — every candidate was reservation-full (none was over the raw ceiling to force a Phase-1
	// return). A drain must NEVER stall on a soft ceiling — a dropped socket is worse than transient overload
	// the rebalancer corrects — so force-proceed on the least-loaded RESOLVABLE candidate, ignoring
	// reservations (recorded best-effort). Re-selects on a lapsed endpoint; errors only if NONE resolves.
	force := withoutSelf(cands, self)
	for len(force) > 0 {
		target, _ := SelectDrainTarget(force, self, incoming, ceiling)
		if target == "" {
			break
		}
		reserved, _ := resv.ReserveDrainTarget(ctx, target, self, ceiling-loadOf(force, target), incoming, ttl)
		if id, a, e := resolveOrDrop(ctx, fleet, target); e == nil {
			return id, a, nil
		}
		if reserved {
			releaseAbandoned(ctx, resv, target, self)
		}
		force = dropShard(force, target)
	}
	return "", "", errNoDrainPeer
}

// releaseAbandoned drops a reservation the selector made on a target it then discarded. Best-effort: the
// per-field TTL is the correctness backstop, and a selection must never fail because a release did.
func releaseAbandoned(ctx context.Context, resv DrainReserver, target, self string) {
	if err := resv.ReleaseDrainTarget(ctx, target, self); err != nil {
		slog.Warn("drain selector: could not release a reservation on an abandoned target; it will expire on its TTL",
			"target", target, "drainer", self, "err", err)
	}
}

// withoutSelf returns a fresh slice of the candidates excluding self (the draining shard is never its own
// target). Fresh so the caller can compact it in place without touching the shared candidate list.
func withoutSelf(cands []ShardLoad, self string) []ShardLoad {
	out := make([]ShardLoad, 0, len(cands))
	for _, c := range cands {
		if c.ShardID != self {
			out = append(out, c)
		}
	}
	return out
}

var errNoDrainPeer = fmt.Errorf("placement: no live peer shard to drain onto")

// resolveOrDrop resolves target's endpoint, distinguishing "resolved" from "lapsed" so the caller can drop
// a target whose registration expired between the candidate list and the resolve and re-select another.
func resolveOrDrop(ctx context.Context, fleet DrainFleet, target string) (string, string, error) {
	addr, err := fleet.EndpointForShard(ctx, target)
	if err != nil || addr == "" {
		if err == nil {
			err = errNoDrainPeer
		}
		return "", "", err
	}
	return target, addr, nil
}

func loadOf(cands []ShardLoad, id string) int {
	for _, c := range cands {
		if c.ShardID == id {
			return c.Players
		}
	}
	return 0
}

// dropShard removes id from in, compacting in place (in is the caller's own `remaining` scratch slice, never
// the shared candidate list — so this must not be called on a slice the caller still needs whole).
func dropShard(in []ShardLoad, id string) []ShardLoad {
	out := in[:0]
	for _, c := range in {
		if c.ShardID != id {
			out = append(out, c)
		}
	}
	return out
}

func sortedByID(cands []ShardLoad) []ShardLoad {
	out := append([]ShardLoad(nil), cands...)
	sort.Slice(out, func(i, j int) bool { return out[i].ShardID < out[j].ShardID })
	return out
}
