package director

import (
	"context"
	"sync"
	"testing"
	"time"
)

// memLease is an in-memory LeaseClaimer for hermetic election tests, mirroring the Redis lease's
// time-fenced mutual exclusion: a claim succeeds when the lease is free, EXPIRED, or already this
// owner's, then (re)sets the owner + expiry.
type memLease struct {
	mu     sync.Mutex
	leases map[string]memLeaseEntry
}
type memLeaseEntry struct {
	owner   string
	expires time.Time
}

func newMemLease() *memLease { return &memLease{leases: map[string]memLeaseEntry{}} }

func (m *memLease) ClaimLease(_ context.Context, id, owner string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if e, ok := m.leases[id]; ok && e.owner != owner && e.expires.After(now) {
		return false, nil
	}
	m.leases[id] = memLeaseEntry{owner: owner, expires: now.Add(ttl)}
	return true, nil
}

func (m *memLease) ReleaseLease(_ context.Context, id, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.leases[id]; ok && e.owner == owner {
		delete(m.leases, id)
	}
	return nil
}

func leaderCount(ds ...*Director) int {
	n := 0
	for _, d := range ds {
		if d.IsLeader() {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for: %s", what)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestDirectorLeaderElectionFailover pins the core HA contract (the done-when's "survives a director
// restart"): two directors campaign for the SAME scope; exactly one leads and the other is a warm
// standby; when the leader resigns (its process stops), the standby takes over within a lease cycle.
func TestDirectorLeaderElectionFailover(t *testing.T) {
	lease := newMemLease()
	store := newMemStore()
	const ttl = 90 * time.Millisecond

	d1 := New("", store, discardLog()).WithElection(lease, "inst-1").WithLeaseTTL(ttl).WithTick(time.Hour)
	d2 := New("", store, discardLog()).WithElection(lease, "inst-2").WithLeaseTTL(ttl).WithTick(time.Hour)

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel1)
	t.Cleanup(cancel2)
	go d1.Run(ctx1)
	go d2.Run(ctx2)

	// Exactly one director leads; the other is a standby.
	waitFor(t, "exactly one leader", func() bool { return leaderCount(d1, d2) == 1 })

	leader, standby, cancelLeader := d1, d2, cancel1
	if d2.IsLeader() {
		leader, standby, cancelLeader = d2, d1, cancel2
	}
	if standby.IsLeader() {
		t.Fatal("both directors believe they lead — split brain")
	}

	// Kill the leader (its process stops) → it resigns the lease → the standby takes over.
	_ = leader
	cancelLeader()
	waitFor(t, "standby promoted to leader", standby.IsLeader)
}

// TestDirectorNoElectionIsAlwaysLeader: with no claimer wired (single-process), a director leads from
// startup so Set/Get and orchestration work without any Redis.
func TestDirectorNoElectionIsAlwaysLeader(t *testing.T) {
	d := New("", newMemStore(), discardLog()).WithTick(5 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	waitFor(t, "leader without election", d.IsLeader)
}
