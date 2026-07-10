package world

import (
	"context"
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
		slog.Warn("rebalance drain failed", "zone", zoneID, "to", toShard, "err", err)
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
