package world

import (
	"context"
	"log/slog"
	"time"

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
				slog.Error("lost zone lease to another shard; fencing this shard", "zone", zoneID, "shard", s.shardID)
				if s.onFence != nil {
					s.onFence()
				}
				return
			}
		}
	}
}

// markHandedOff records that zoneID's lease was deliberately handed to a peer during a drain, and stops its
// renewal goroutine — so the source's renewal never fences the shard nor races the atomic HandoverZone flip.
// Idempotent. Guarded by mu.
func (s *Shard) markHandedOff(zoneID string) {
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
