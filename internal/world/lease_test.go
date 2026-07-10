package world

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
)

// fakeLeaser is an in-memory ZoneLeaser recording claim/release/handover calls, with a per-zone "deny" flag
// so a test can simulate another shard taking a lease over (a lost claim).
type fakeLeaser struct {
	mu        sync.Mutex
	claims    map[string]int
	releases  map[string]int
	handovers []string
	denied    map[string]bool
	owner     map[string]string
	gen       map[string]uint64
}

func newFakeLeaser() *fakeLeaser {
	return &fakeLeaser{
		claims: map[string]int{}, releases: map[string]int{}, denied: map[string]bool{},
		owner: map[string]string{}, gen: map[string]uint64{},
	}
}

func (f *fakeLeaser) ClaimZone(_ context.Context, zoneID, shardID string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims[zoneID]++
	if f.denied[zoneID] {
		return false, nil
	}
	if f.owner[zoneID] != shardID {
		f.owner[zoneID] = shardID
		f.gen[zoneID]++
	}
	return true, nil
}

func (f *fakeLeaser) ReleaseZone(_ context.Context, zoneID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases[zoneID]++
	delete(f.owner, zoneID) // the generation survives a release — it is monotonic per zone, not per owner
	return nil
}

// HandoverZone mirrors the real owner-fenced Lua, refusals included: a flip from a shard that is not the live
// owner, and a self-handover, both fail and bump nothing. A double that always reports "flipped" would hide
// exactly the bug the fence exists to catch.
func (f *fakeLeaser) HandoverZone(_ context.Context, zoneID, from, to string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if from == to || f.owner[zoneID] != from {
		return false, nil
	}
	f.handovers = append(f.handovers, from+"->"+to+":"+zoneID)
	f.gen[zoneID]++
	f.owner[zoneID] = to
	return true, nil
}

// ZoneLease models the directory's #315 fence: the generation moves on an ownership CHANGE, never on a
// renewal. A fake that returned a constant would let a stale-generation bug pass every test that uses it.
func (f *fakeLeaser) ZoneLease(_ context.Context, zoneID string) (string, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.owner[zoneID], f.gen[zoneID], nil
}

func (f *fakeLeaser) deny(zoneID string)      { f.mu.Lock(); f.denied[zoneID] = true; f.mu.Unlock() }
func (f *fakeLeaser) claimCount(z string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.claims[z] }
func (f *fakeLeaser) releaseCount(z string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releases[z]
}

// TestShardRenewsAndReleasesZoneLease: with leasing wired, the shard renews every hosted zone's lease and,
// on a CLEAN shutdown, releases it so a peer can reclaim immediately (the pre-16.4 cmd contract, now in the
// shard).
func TestShardRenewsAndReleasesZoneLease(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	leaser := newFakeLeaser()
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil).
		WithZoneLeasing(leaser, "shard-a", 30*time.Millisecond, 10*time.Millisecond, func() {})
	ctx, cancel := context.WithCancel(context.Background())
	go sh.Run(ctx)

	waitCond(t, "midgaard lease renewed", func() bool { return leaser.claimCount("midgaard") >= 2 })

	cancel()
	waitCond(t, "midgaard lease released on clean shutdown", func() bool { return leaser.releaseCount("midgaard") >= 1 })
}

// TestShardFencesOnLostLease: an UNEXPECTED lease loss (another shard took over — the leaser denies our
// renewal) fences the shard via onFence, exactly as the old cmd renewal did — a shard that no longer owns a
// zone must stop.
func TestShardFencesOnLostLease(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	leaser := newFakeLeaser()
	fenced := make(chan struct{}, 1)
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil).
		WithZoneLeasing(leaser, "shard-a", 30*time.Millisecond, 10*time.Millisecond, func() {
			select {
			case fenced <- struct{}{}:
			default:
			}
		})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)

	waitCond(t, "renewing", func() bool { return leaser.claimCount("midgaard") >= 1 })
	leaser.deny("midgaard") // simulate a peer taking the lease over

	select {
	case <-fenced:
	case <-time.After(2 * time.Second):
		t.Fatal("shard did not fence on an unexpected lost lease")
	}
}

// TestHandedOffZoneStopsRenewalWithoutFenceOrRelease: a zone deliberately handed off during a drain stops
// renewing, does NOT fence the shard even though the source can no longer claim it, and is NOT released by
// the source (the NEW owner holds it) — the invariant that lets a drain hand a lease off without killing
// the still-draining source.
func TestHandedOffZoneStopsRenewalWithoutFenceOrRelease(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	leaser := newFakeLeaser()
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil).
		WithZoneLeasing(leaser, "shard-a", 30*time.Millisecond, 10*time.Millisecond, func() {
			t.Error("a deliberately handed-off zone must NOT fence the shard")
		})
	ctx, cancel := context.WithCancel(context.Background())
	go sh.Run(ctx)

	waitCond(t, "renewing", func() bool { return leaser.claimCount("midgaard") >= 1 })

	sh.markZoneHandedOff("midgaard") // stops the renewal goroutine
	leaser.deny("midgaard")          // even if a stray tick claimed, it would now be denied — must not fence

	// Give several renew intervals to prove renewal stopped and no fence fired.
	time.Sleep(60 * time.Millisecond)

	cancel()
	time.Sleep(40 * time.Millisecond)
	if leaser.releaseCount("midgaard") != 0 {
		t.Fatalf("handed-off zone was released by the source (%d); the new owner holds it", leaser.releaseCount("midgaard"))
	}
}
