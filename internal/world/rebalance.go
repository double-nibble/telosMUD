package world

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// rebalance.go is the shard side of coordinator-driven placement rebalancing (#42 slice 3). The director
// leader publishes a per-zone DIRECTIVE ("move zone Z to shard T") to the directory; the zone's CURRENT
// owner — the only shard that renews Z's lease — reads it on its per-zone lease-renewal tick and drains just
// that ONE zone to T. The directive only TRIGGERS the already-fenced HandoverZone flip; it is never
// authority for ownership (the lease CAS is), so no lost/dup/reordered directive can double-own a zone.

// RebalancePort is the directory seam the owning shard uses to read + clear the coordinator's rebalance
// directive for a zone (#42). *directory.Redis satisfies it; nil disables coordinator-driven rebalancing.
type RebalancePort interface {
	ReadRebalance(ctx context.Context, zoneID string) (toShard string, found bool, err error)
	RefreshRebalance(ctx context.Context, zoneID, toShard string, ttl time.Duration) error
	ClearRebalance(ctx context.Context, zoneID, toShard string) error
}

// WithRebalance wires the directory port the shard polls for rebalance directives on its lease-renewal tick
// (#42 slice 3). Optional — without it the shard is never coordinator-rebalanced (SIGTERM drain unaffected).
func (s *Shard) WithRebalance(p RebalancePort) *Shard {
	s.rebalancePort = p
	return s
}

const (
	// rebalanceDrainDeadline bounds a single-zone rebalance drain (mirrors the SIGTERM drain deadline): a
	// straggler still resident at it is flushed + reclaimed (reconnect from durable state), not zero-drop.
	rebalanceDrainDeadline = 30 * time.Second
	// rebalanceDirectiveTTL re-arms a directive while its drain is in flight — comfortably longer than the
	// deadline so the directive outlives the drain without a per-tick refresh, yet short enough that a
	// crashed owner's directive self-expires promptly (the coordinator then re-plans).
	rebalanceDirectiveTTL = 90 * time.Second
	// rebalanceRetryBackoff suppresses re-attempting a failed move on every ~5s renewal tick (a persistently
	// unreachable target would otherwise re-dial each tick until the directive TTL lapses).
	rebalanceRetryBackoff = 30 * time.Second
)

// anchorDeferBudget caps how long a rebalance may be deferred because the zone is somebody's exit anchor
// (#421). Past it, the anchored occupants are ejected and the move proceeds.
//
// A cap is not optional. A dungeon fed by a busy town would otherwise have SOMEBODY inside essentially
// always, so a naive pin defers that town's rebalance indefinitely — and every deferred cycle burns a
// coordinator cooldown, so the load imbalance the rebalance exists to fix just persists.
//
// Sized longer than an ordinary dungeon run (so the common case is never disturbed) and shorter than an
// operator's patience with a shard that will not shed load. Var for tests.
var anchorDeferBudget = 3 * time.Minute

// maybeRebalance runs on the zone's lease-renewal goroutine (a confirmed-owned tick): it reads any pending
// rebalance directive for the zone and, if this shard isn't already draining it, launches the single-zone
// drain on a SEPARATE goroutine. The renewal goroutine MUST keep ticking (renewing the lease) until
// handoverZoneTo's flip stops it — running the drain inline would let the lease expire mid-drain and a peer
// could reclaim the zone (double-own). A no-op without a rebalance port or a directive.
func (s *Shard) maybeRebalance(ctx context.Context, zoneID string) {
	if s.rebalancePort == nil {
		return
	}
	toShard, found, err := s.rebalancePort.ReadRebalance(ctx, zoneID)
	if err != nil || !found {
		return
	}
	// SELF-TARGET guard (critical): once the flip lands, the NEW owner (== toShard) also renews this zone's
	// lease and re-reads the still-present directive. Without this it would self-handover — stop renewing a
	// live zone it serves → lease expiry → double-own. The new owner's confirmed tick IS the "move complete"
	// signal, so it simply clears its own now-satisfied directive.
	if toShard == s.shardID {
		_ = s.rebalancePort.ClearRebalance(ctx, zoneID, toShard)
		return
	}
	// Back off after a recent failed attempt so a persistently-unreachable target (or a stale endpoint)
	// isn't re-dialed on every ~5s renewal tick until the directive's TTL lapses.
	s.mu.Lock()
	if until, ok := s.rebalanceBackoff[zoneID]; ok && time.Now().Before(until) {
		s.mu.Unlock()
		return
	}
	inFlight := s.rebalancing[zoneID]
	if !inFlight {
		s.rebalancing[zoneID] = true
	}
	s.mu.Unlock()
	if inFlight {
		// A drain of this zone is already running: just re-arm the directive TTL (fenced to toShard) so it
		// survives the in-flight window rather than launching a second drain.
		_ = s.rebalancePort.RefreshRebalance(ctx, zoneID, toShard, rebalanceDirectiveTTL)
		return
	}
	addr, err := s.dir.EndpointForShard(ctx, toShard)
	if err != nil || addr == "" {
		slog.Warn("rebalance: target endpoint unresolved; skipping", "zone", zoneID, "to", toShard, "err", err)
		s.failRebalance(zoneID)
		return
	}
	//nolint:gosec // G118: runRebalance DELIBERATELY uses an independent context, not the renewal ctx —
	// handoverZoneTo's flip cancels the renewal ctx (markZoneHandedOff), which must NOT abort the drain's
	// wait + straggler reclaim. Passing the request-scoped ctx here would defeat the whole design.
	go s.runRebalance(zoneID, toShard, addr)
}

// runRebalance executes the drain off the renewal goroutine and clears the in-flight flag + the directive on
// completion. It uses an INDEPENDENT context (not the renewal goroutine's, which handoverZoneTo's flip
// cancels via markZoneHandedOff) so the wait + straggler-reclaim aren't aborted the instant ownership flips.
func (s *Shard) runRebalance(zoneID, toShard, toAddr string) {
	ctx, cancel := context.WithTimeout(context.Background(), rebalanceDrainDeadline+15*time.Second)
	defer cancel()
	res, err := s.RebalanceZone(ctx, zoneID, toShard, toAddr, rebalanceDrainDeadline)
	if err != nil {
		// Back off and leave the directive to expire / be re-issued — don't clear it (the move didn't happen).
		// Clearing on a non-move would tell the coordinator the load had shifted when it had not, and it would
		// re-plan against a wrong model.
		//
		// An anchor DEFERRAL (#421) is an expected outcome rather than a fault, so it is logged at Info by
		// settleAnchorsBeforeRebalance and not repeated as a failure here. It takes the same backoff, which is
		// what gives it its retry cadence, and the same do-not-clear treatment, which is what makes the
		// coordinator keep asking.
		if !errors.Is(err, errZoneAnchored) {
			slog.Warn("rebalance drain failed", "zone", zoneID, "to", toShard, "err", err)
		}
		s.failRebalance(zoneID)
		return
	}
	slog.Info("rebalance drain complete", "zone", zoneID, "to", toShard,
		"redirected", res.Redirected, "reclaimed", res.Reclaimed)
	// Signal completion by clearing the directive (fenced to toShard, so it won't wipe a re-pointed one). The
	// coordinator set the zone's cooldown at issue time, so nothing to do there.
	cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.rebalancePort.ClearRebalance(cctx, zoneID, toShard)
	ccancel()
	s.succeedRebalance(zoneID)
}

// errZoneAnchored defers a rebalance because instance occupants are anchored to the zone (#421). It is a
// DEFERRAL, not a failure: the caller must not clear the directive, so the coordinator keeps a true model of
// where the load is and the natural retry cadence re-attempts the move.
var errZoneAnchored = errors.New("zone is the exit anchor of a live instance occupant; deferring the rebalance")

// settleAnchorsBeforeRebalance decides whether zoneID may be handed to a peer right now (#421).
//
// Three outcomes, in order of preference:
//
//   - Nobody is anchored here: proceed immediately. This is the overwhelmingly common case, and it costs one
//     bounded query only when a rebalance directive actually lands.
//   - Somebody is anchored and the defer budget is not spent: refuse with errZoneAnchored. A rebalance is a
//     load-balancing optimization; deferring it for the length of a dungeon run is cheap, while moving the
//     zone out from under a party costs them their run or their routing.
//   - Somebody is anchored and the budget IS spent: eject them and proceed. Progress must be guaranteed —
//     see anchorDeferBudget for why an uncapped defer is its own outage.
//
// The eject is the same walk-back-to-the-door the drain does, so the players land in the anchor room and are
// then redirected with everybody else by the handover that follows. They lose the dungeon run; they do not
// lose their session or their state.
func (s *Shard) settleAnchorsBeforeRebalance(ctx context.Context, zoneID string) error {
	if !s.zoneIsAnchored(ctx, zoneID) {
		s.clearAnchorDefer(zoneID)
		return nil
	}
	if since, expired := s.anchorDeferExpired(zoneID); !expired {
		slog.Info("rebalance deferred: the zone is a live instance occupant's exit anchor; moving it now would "+
			"route their reconnect to a shard that does not hold their session",
			"zone", zoneID, "deferred_for", time.Since(since).Round(time.Second), "budget", anchorDeferBudget)
		return errZoneAnchored
	}
	slog.Warn("rebalance defer budget exhausted: ejecting instance occupants anchored to this zone so the "+
		"move can proceed", "zone", zoneID, "budget", anchorDeferBudget)
	s.ejectOccupantsAnchoredTo(ctx, zoneID)
	s.clearAnchorDefer(zoneID)
	return nil
}

// ejectOccupantsAnchoredTo walks every instance occupant whose anchor is zoneID back out to it, so the zone
// can then be handed over with them counted as ordinary residents of it.
//
// Whole-instance granularity: a party enters together through one door, so an instance's occupants
// overwhelmingly share an anchor, and ejecting the few who do not costs them the same interrupted run rather
// than anything worse. Precision here would buy nothing and would need a second round trip to get.
func (s *Shard) ejectOccupantsAnchoredTo(ctx context.Context, zoneID string) {
	var affected []*Zone
	for _, z := range s.zonesList() {
		if !z.isInstance() {
			continue
		}
		ch := make(chan []string, 1)
		select {
		case z.inbox <- anchorsMsg{resp: ch}:
		case <-z.dead:
			continue
		case <-ctx.Done():
			return
		case <-time.After(anchorQueryBarrier):
			continue
		}
		select {
		case anchors := <-ch:
			for _, a := range anchors {
				if a == zoneID {
					affected = append(affected, z)
					break
				}
			}
		case <-ctx.Done():
			return
		case <-time.After(anchorQueryBarrier):
		}
	}
	// Reuse the drain's own eject, which claims and releases the destination through the instance's OWN
	// goroutine (never the caller's) and is bounded by its barrier.
	s.ejectInstanceOccupants(ctx, affected)
}

// anchorDeferExpired reports how long zoneID's rebalance has been deferred for anchors, and whether the
// budget is spent. The first call starts the clock.
func (s *Shard) anchorDeferExpired(zoneID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	since, ok := s.anchorDeferSince[zoneID]
	if !ok {
		since = time.Now()
		s.anchorDeferSince[zoneID] = since
	}
	return since, time.Since(since) >= anchorDeferBudget
}

// clearAnchorDefer forgets a zone's defer clock, so a later unrelated deferral gets a full budget rather
// than inheriting an old one.
func (s *Shard) clearAnchorDefer(zoneID string) {
	s.mu.Lock()
	delete(s.anchorDeferSince, zoneID)
	s.mu.Unlock()
}

// failRebalance clears the in-flight flag and arms a retry backoff so a persistently-failing move isn't
// re-attempted on every renewal tick.
func (s *Shard) failRebalance(zoneID string) {
	s.mu.Lock()
	delete(s.rebalancing, zoneID)
	s.rebalanceBackoff[zoneID] = time.Now().Add(rebalanceRetryBackoff)
	s.mu.Unlock()
}

// succeedRebalance clears the in-flight flag and any backoff after a completed move.
func (s *Shard) succeedRebalance(zoneID string) {
	s.mu.Lock()
	delete(s.rebalancing, zoneID)
	delete(s.rebalanceBackoff, zoneID)
	s.mu.Unlock()
}

// RebalanceZone drains ONE zone to a chosen peer for a coordinator rebalance (#42) — the single-zone analog
// of BeginDrain, but WITHOUT the shard-wide draining flag (the shard keeps serving its other zones and
// accepting fresh logins). It flips the zone's ownership to the target (fenced), fans its players off over
// the cross-shard handoff (sockets stay open — zero drop), waits until the zone empties or the deadline,
// then flushes + reclaims any straggler (reconnect from durable state). Runs OFF the zone goroutine.
//
// Finally it UNHOSTS the now-empty zone (#288): stops its actor and drops it from s.zones, so a shard that
// lives through many rebalances does not accumulate a zombie zone goroutine per migration. The teardown runs
// AFTER the ownership flip and after the zone has emptied, which is exactly what UnhostZone's preconditions
// require. It is best-effort: a refusal or a wedged actor is logged and the rebalance still reports success,
// because the players and the lease have already moved — the residue is a leak, not a correctness break, and
// failing the rebalance here would make the coordinator retry a move that has already happened.
func (s *Shard) RebalanceZone(ctx context.Context, zoneID, toShard, toAddr string, deadline time.Duration) (res DrainResult, err error) {
	// #421: settle the EXIT ANCHOR question BEFORE handoverZoneTo, because the flip is what breaks the
	// invariant — ShardForZone follows the LEASE, not the hosting, so once ownership moves, the placement
	// records of everyone anchored here route reconnects to a shard with no session for them.
	//
	// A guard placed later (on quiescence, or in UnhostZone) would be worse than none: the flip would land
	// anyway, and this shard would be left hosting a zone it no longer owns, refused by every subsequent
	// teardown, while runRebalance told the coordinator the move had completed.
	if err := s.settleAnchorsBeforeRebalance(ctx, zoneID); err != nil {
		return res, err
	}
	z := s.zoneByID(zoneID)
	if z == nil {
		return res, fmt.Errorf("rebalance %q: not hosted here", zoneID)
	}
	initial := int(z.pop.Load())

	// 1. Fenced ownership flip to the target, then tell the zone to fan its players off to it.
	if err := s.handoverZoneTo(ctx, zoneID, toShard, toAddr); err != nil {
		return res, fmt.Errorf("rebalance %q -> %s: %w", zoneID, toShard, err)
	}
	z.post(drainZoneMsg{})

	// 2. Wait until the zone empties (players redirected) or the deadline elapses.
	dl := time.After(deadline)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
wait:
	for {
		// Wait for QUIESCENCE, not merely for the player count to hit zero: a brand-new character who quit
		// inside their create round-trip has left z.players while a final logout snapshot is still parked,
		// waiting on a createdMsg that only this zone's actor can replay. Tearing the zone down in that window
		// loses the write (Zone.quiescent).
		if z.quiescent() {
			break wait
		}
		select {
		case <-ctx.Done():
			break wait
		case <-dl:
			break wait
		case <-tick.C:
		}
	}

	// 3. Flush this zone durably, then clean-disconnect + classify the stragglers still resident (#43 reclaim,
	// scoped to one zone). Post the flush BEFORE the reclaim so the durable flush handler is FIFO-ordered
	// ahead of the disconnect on the zone inbox. Both select on ctx so a wedged zone can't block forever.
	z.post(drainFlushMsg{})
	ch := make(chan reclaimTally, 1)
	select {
	case z.inbox <- reclaimStragglersMsg{resp: ch}:
		select {
		case t := <-ch:
			res.ReclaimedInfra, res.ReclaimedClient = t.infra, t.client
		case <-ctx.Done():
			res.ReclaimedInfra = int(z.pop.Load())
		}
	case <-ctx.Done():
		res.ReclaimedInfra = int(z.pop.Load())
	}
	res.Reclaimed = res.ReclaimedInfra + res.ReclaimedClient
	res.Redirected = initial - res.Reclaimed
	if res.Redirected < 0 {
		res.Redirected = 0
	}

	// 4. Tear the empty zone down (#288). Use a FRESH context: ctx may already be past its deadline (the wait
	// above breaks on it), and the zone still has to be unhosted — leaving it is the leak this closes. The
	// budget only has to cover a directory read and the actor's last message.
	unhostCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unhostActorGrace)
	defer cancel()
	if uerr := s.UnhostZone(unhostCtx, zoneID); uerr != nil {
		// Expected for this shard's HOME zone and for local bootstrap zones, which UnhostZone refuses on
		// purpose (see its preconditions) — a rebalance of those moves the players and the lease but leaves
		// the object. Anything else here is a wedged actor or an unreadable directory, and it leaks.
		slog.Warn("rebalance: the migrated zone was not unhosted; its actor goroutine lives until restart",
			"zone", zoneID, "to", toShard, "err", uerr)
	}
	return res, nil
}
