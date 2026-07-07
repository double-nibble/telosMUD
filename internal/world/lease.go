package world

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/directory"
)

// ZoneLeaser is the slice of the directory the shard needs to OWN its hosted zones' leases at RUNTIME
// (Phase 16.4b): renew a claim, release it on clean shutdown, and hand a zone's lease atomically to a peer
// during a graceful drain. It is a WRITE authority, kept separate from Locator (the routing READ port) — the
// production impl is *directory.Redis; tests inject a fake. A nil leaser leaves the shard leasing nothing
// (single-shard / dev / hermetic tests), exactly the pre-16.4 behavior.
type ZoneLeaser interface {
	ClaimZone(ctx context.Context, zoneID, shardID string, ttl time.Duration) (bool, error)
	ReleaseZone(ctx context.Context, zoneID, shardID string) error
	HandoverZone(ctx context.Context, zoneID, fromShard, toShard string, ttl time.Duration) (bool, error)
}

// WithZoneLeasing moves zone-lease RENEWAL into the shard (from cmd/telos-world's per-zone renewZoneLease
// goroutines). The shard then renews every hosted zone's lease — boot zones AND runtime-adopted ones — and
// FENCES itself (onFence, the cmd-level ctx cancel) if it unexpectedly loses one, EXCEPT a zone it
// deliberately handed off during a drain, whose renewal stops silently. shardID is the directory
// write-authority key; ttl/renew default to DefaultZoneLease and ttl/3 when zero. onFence may be nil (no
// self-fence). Must be called before Run. nil leaser leaves leasing OFF (the shard renews nothing).
func (s *Shard) WithZoneLeasing(leaser ZoneLeaser, shardID string, ttl, renew time.Duration, onFence func()) *Shard {
	s.leaser = leaser
	s.shardID = shardID
	s.leaseTTL = ttl
	s.leaseRenew = renew
	s.onFence = onFence
	return s
}

// WithLocalZones marks zone ids as LOCAL, UNLEASED bootstrap zones (#212 embedded core pack): the
// shard hosts them without renewing a directory lease (every shard hosts its own copy) and never
// hands them off on a graceful drain. Must be called before Run. Safe to call with ids the shard
// does not host (they are simply recorded); a nil/empty call is a no-op.
func (s *Shard) WithLocalZones(ids ...string) *Shard {
	if len(ids) == 0 {
		return s
	}
	if s.localZones == nil {
		s.localZones = make(map[string]bool, len(ids))
	}
	for _, id := range ids {
		s.localZones[id] = true
	}
	return s
}

// isLocalZone reports whether zoneID is a local, unleased bootstrap zone. Read-only after
// construction, so no lock is needed (localZones is set before Run and never mutated after).
func (s *Shard) isLocalZone(zoneID string) bool { return s.localZones[zoneID] }

// leaseParams returns the effective ttl + renew cadence (applying defaults for zero values).
func (s *Shard) leaseParams() (ttl, renew time.Duration) {
	ttl = s.leaseTTL
	if ttl <= 0 {
		ttl = directory.DefaultZoneLease
	}
	renew = s.leaseRenew
	if renew <= 0 {
		renew = ttl / 3
	}
	return ttl, renew
}

// startZoneRenewal launches the renewal goroutine for one hosted zone under a child ctx whose cancel is
// stored in leaseStop, so a graceful handoff can stop THIS zone's renewal without touching the others or
// the shard. A no-op when leasing is off. Called from Run (boot zones) and HostZone/adopt (runtime zones).
func (s *Shard) startZoneRenewal(parent context.Context, zoneID string) {
	if s.leaser == nil {
		return
	}
	// A local bootstrap zone (#212 core pack) is hosted unleased on every shard — do not renew a
	// lease it never claimed (which would otherwise contend for a single directory key across shards).
	if s.isLocalZone(zoneID) {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	// If a renewal is somehow already running for this zone (a re-host), keep the existing one.
	if _, running := s.leaseStop[zoneID]; running {
		s.mu.Unlock()
		cancel()
		return
	}
	s.leaseStop[zoneID] = cancel
	s.mu.Unlock()
	//nolint:gosec // G118: ctx is a child of the shard's lifetime ctx (cancelled on shutdown/handoff) — exactly what this lease goroutine should follow.
	go s.renewZoneLease(ctx, zoneID)
}

// renewZoneLease keeps this shard's claim on zoneID alive until its ctx is cancelled (shutdown or a
// deliberate handoff). On an UNEXPECTED lease loss (a different shard took over — not our doing) it fences
// the shard via onFence, mirroring the pre-16.4 cmd/telos-world behavior. A zone we deliberately handed off
// (handedOff) never fences: its renewal returns silently and does NOT release (the new owner holds it).
func (s *Shard) renewZoneLease(ctx context.Context, zoneID string) {
	ttl, every := s.leaseParams()
	t := time.NewTicker(every)
	defer t.Stop()
	// confirmed becomes true once we have held the lease at least once. Until then a !ok claim is the
	// EXPECTED "adopting" state — a zone just hosted via AdoptZone renews while the draining source still
	// owns the lease, and takes over the instant the source's HandoverZone flip lands. Fencing only makes
	// sense AFTER we have confirmed ownership (a real loss); fencing on the pre-flip !ok would kill the
	// adopting target. A boot zone is already claimed (cmd ClaimFromPool), so its first renew confirms on
	// tick one and the grace never applies.
	confirmed := false
	for {
		select {
		case <-ctx.Done():
			// On a deliberate handoff the new owner holds the lease — never release (ReleaseZone is
			// owner-fenced, but we skip it to make the intent explicit). On a normal shutdown we still own
			// it, so release it so a peer can reclaim immediately instead of waiting out the TTL.
			if !s.zoneHandedOff(zoneID) {
				rctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = s.leaser.ReleaseZone(rctx, zoneID, s.shardID)
				cancel()
			}
			return
		case <-t.C:
			if s.zoneHandedOff(zoneID) {
				return // handed off between ticks: stop renewing, don't fence
			}
			rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			ok, err := s.leaser.ClaimZone(rctx, zoneID, s.shardID, ttl)
			cancel()
			switch {
			case err != nil:
				// Transient (Redis blip): keep trying. If it persists past the lease the claim lapses and a
				// later renewal returns !ok, fencing us below.
				slog.Warn("zone lease renewal error", "zone", zoneID, "shard", s.shardID, "err", err)
			case !ok:
				if s.zoneHandedOff(zoneID) {
					return // a handoff flipped ownership under us — expected, don't fence
				}
				if !confirmed {
					// Still adopting: the draining source owns the lease until its HandoverZone flip lands.
					slog.Debug("awaiting zone lease (adopting)", "zone", zoneID, "shard", s.shardID)
					continue
				}
				slog.Error("lost zone lease to another shard; fencing this shard", "zone", zoneID, "shard", s.shardID)
				if s.onFence != nil {
					s.onFence()
				}
				return
			default:
				confirmed = true
			}
		}
	}
}

// markZoneHandedOff records that zoneID's lease was deliberately handed to a peer during a drain, and stops
// its renewal goroutine — so the source's renewal never fences the shard nor races the atomic HandoverZone
// flip. Idempotent. Guarded by mu. (Distinct from Zone.markHandedOff, which flips a PLAYER's handoff-commit
// discriminator — same verb, different scope.)
func (s *Shard) markZoneHandedOff(zoneID string) {
	s.mu.Lock()
	s.handedOff[zoneID] = true
	stop := s.leaseStop[zoneID]
	delete(s.leaseStop, zoneID)
	s.mu.Unlock()
	if stop != nil {
		stop() // renewal goroutine returns without releasing (handedOff is set)
	}
}

// zoneHandedOff reports whether zoneID was deliberately handed off during a drain. Guarded by mu.
func (s *Shard) zoneHandedOff(zoneID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handedOff[zoneID]
}

// handoverZoneTo migrates zoneID's ownership from this (draining) shard to the target peer, so a subsequent
// player fan-out redirects into a zone the target already hosts (Phase 16.4b control-plane core). The order
// is the one the distributed-systems review prescribed:
//
//  1. RPC target.AdoptZone — the target builds + runs the zone and starts renewal (which waits out our
//     ownership). Doing the build BEFORE the flip means the atomic flip has no build latency inside it.
//  2. markHandedOff — stop OUR renewal for the zone and suppress its fence, so the flip below can't trip us.
//  3. HandoverZone — one atomic fenced CAS flips ownership source->target with a fresh lease; ShardForZone
//     transitions straight across with no ownerless window. The target's renewal then confirms ownership.
//
// Returns an error (leaving the zone owned by us) if the target can't host it or the fenced flip is refused
// (we are no longer the live owner). Requires leasing to be wired (WithZoneLeasing).
func (s *Shard) handoverZoneTo(ctx context.Context, zoneID, targetShardID, targetAddr string) error {
	if s.leaser == nil {
		return fmt.Errorf("handover %q: zone leasing not configured", zoneID)
	}
	client, err := s.peers(targetAddr)
	if err != nil {
		return fmt.Errorf("handover %q: dial target %s: %w", zoneID, targetAddr, err)
	}
	if _, err := client.AdoptZone(ctx, &handoffv1.AdoptZoneRequest{ZoneId: zoneID}); err != nil {
		return fmt.Errorf("handover %q: target %s adopt failed: %w", zoneID, targetShardID, err)
	}

	// Suppress our own fence/renewal for this zone BEFORE the flip, so our renewal can never mistake the
	// deliberate flip for a lost lease.
	s.markZoneHandedOff(zoneID)

	ttl, _ := s.leaseParams()
	ok, err := s.leaser.HandoverZone(ctx, zoneID, s.shardID, targetShardID, ttl)
	if err != nil {
		return fmt.Errorf("handover %q: lease flip: %w", zoneID, err)
	}
	if !ok {
		return fmt.Errorf("handover %q: fenced flip refused (no longer the live owner)", zoneID)
	}
	slog.Info("handed zone lease to peer", "zone", zoneID, "from", s.shardID, "to", targetShardID)
	return nil
}
