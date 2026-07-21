package director

import (
	"context"
	"time"
)

// election.go is the director leader election (WORLD-EVENTS.md §3, Phase 10.1c): exactly ONE live
// director instance owns a given scope (region/world), the others are warm standbys ready to take over
// on its crash. It is a Redis lease (the directory's ClaimLease, the same time-fenced CAS as a zone
// lease), so the CAS is the final arbiter — even a buggy/duplicated director can never make two
// instances both believe they lead, the same split-brain safety the placement design relies on.

// DefaultLeaseTTL is the scope-lease lifetime; the leader renews every TTL/3, so a crashed leader's
// lease expires within ~TTL and a standby claims it. Short enough for snappy failover, long enough that
// a brief GC pause or network blip doesn't drop leadership.
const DefaultLeaseTTL = 9 * time.Second

// LeaseClaimer is the leader-election seam: ClaimLease takes/renews an exclusive named lease for an
// owner (returning whether the owner now HOLDS it), ReleaseLease frees it on a graceful resign.
// *directory.Redis satisfies it (ClaimLease/ReleaseLease); tests inject an in-memory fake.
type LeaseClaimer interface {
	ClaimLease(ctx context.Context, leaseID, owner string, ttl time.Duration) (held bool, err error)
	ReleaseLease(ctx context.Context, leaseID, owner string) error
}

// WithElection wires leader election: this director will campaign for an exclusive lease on its scope
// under instanceID. Without it (no claimer), the director is always the leader — the single-process /
// test default. Call before Run.
func (d *Director) WithElection(claimer LeaseClaimer, instanceID string) *Director {
	d.claimer = claimer
	d.instanceID = instanceID
	d.leaseID = "director:" + scopeLabel(d.regionID)
	if d.leaseTTL == 0 {
		d.leaseTTL = DefaultLeaseTTL
	}
	return d
}

// WithLeaseTTL overrides the lease lifetime (tests use a short TTL for fast failover). Call before Run.
func (d *Director) WithLeaseTTL(ttl time.Duration) *Director {
	if ttl > 0 {
		d.leaseTTL = ttl
	}
	return d
}

// IsLeader reports whether this instance currently owns its scope lease. A standby returns false; it
// still serves Get/Set (its state loads from the durable store on demand), but a director gates its
// ACTIVE orchestration — broadcasts, scheduled waves (10.4+) — on leadership so only the leader drives
// the scope. Safe to call from any goroutine.
func (d *Director) IsLeader() bool { return d.leader.Load() }

// campaign (Run goroutine) takes/renews the scope lease and updates the leadership flag. A claim ERROR
// (Redis unreachable) FAILS SAFE — the director steps down rather than risk acting without a confirmed
// lease, so a partitioned instance can't double-lead.
func (d *Director) campaign(ctx context.Context) {
	held, err := d.claimer.ClaimLease(ctx, d.leaseID, d.instanceID, d.leaseTTL)
	if err != nil {
		d.log.WarnContext(ctx, "director lease campaign failed; stepping down", "err", err)
		d.leader.Store(false)
		return
	}
	if was := d.leader.Swap(held); was != held {
		d.log.InfoContext(ctx, "director leadership changed", "leader", held, "instance", d.instanceID, "scope", scopeLabel(d.regionID))
	}
}

// resign frees the scope lease on a graceful shutdown so a standby takes over immediately instead of
// waiting out the TTL. Uses a fresh context (the Run ctx is already cancelled at this point).
func (d *Director) resign() {
	if d.claimer != nil && d.leader.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = d.claimer.ReleaseLease(ctx, d.leaseID, d.instanceID)
	}
	d.leader.Store(false)
}
