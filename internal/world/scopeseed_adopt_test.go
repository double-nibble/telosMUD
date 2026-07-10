package world

import (
	"context"
	"sync"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// syncZone blocks until the zone's actor loop has processed everything already in its inbox.
//
// This is the ONLY sound barrier here, and getting it wrong is easy: Zone.Run selects over {ctx.Done(),
// inbox}, so cancelling the shard does NOT drain the inbox — Go picks a ready case at random, and a pending
// seed can be discarded on cancel. A test that asserted after cancelling the shard would pass or fail on
// scheduling luck. Instead we post a message and wait for its reply: the inbox is FIFO, so once the loop has
// answered US, it has already applied every message posted before ours — including the seed.
func syncZone(t *testing.T, z *Zone) {
	t.Helper()
	out := make(chan *playv1.ServerFrame, 1)
	z.post(whoFallbackMsg{out: out})
	select {
	case <-out:
	case <-time.After(10 * time.Second):
		t.Fatal("zone loop never answered the sync probe")
	}
}

// scopeseed_adopt_test.go pins #280: a zone hosted AFTER boot (HostZone — the 16.4a drain adoption) seeds
// its scope replica from the authoritative store, exactly as a boot zone does via seedFromSnapshot.
//
// Without it a drain-adopted zone starts with an EMPTY replica and learns each world/region key only when
// it is next broadcast. A sticky flag ("war active") therefore reads false on that zone, potentially
// forever — which is the #44 symptom, reappearing at precisely the failover scope replication exists to
// survive.
//
// The fix is not "call seedFromSnapshot from registerZone". By then the zone is already in s.zones, so a
// world delta can be sitting in its inbox, and applyScopeSeed is a full-map REPLACE — a seed landing after
// a delta would clobber newer state. The seed must be posted BEFORE the zone is exposed. Both properties
// are pinned below.

// hookedSnapshot is a ScopeSnapshotSource that runs a hook at world-read time, so a test can observe the
// shard's state at the exact moment the seed is being read.
type hookedSnapshot struct {
	fakeScopeSnapshot
	onWorldRead func()
}

func (h hookedSnapshot) SnapshotWorldState(ctx context.Context) (map[string][]byte, error) {
	if h.onWorldRead != nil {
		h.onWorldRead()
	}
	return h.fakeScopeSnapshot.SnapshotWorldState(ctx)
}

// adoptShard builds a shard hosting only midgaard, with a scope bus and a snapshot source. darkwood is a
// member of the demo's "heartlands" region alongside midgaard, so adopting it exercises BOTH scopes.
func adoptShard(t *testing.T, snap ScopeSnapshotSource) *Shard {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	mb := commbus.NewMemBus()
	t.Cleanup(func() { _ = mb.Close() })
	return NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil).
		WithScopeBus(scopebus.New(mb), lc.Regions).
		WithScopeSnapshot(snap)
}

// runShard starts the shard and returns a stop func that cancels it and BLOCKS until Run returns. Run waits
// on the zone WaitGroup, so after stop() every zone goroutine has exited and a test may read zone state
// without racing the actor loop.
func runShard(t *testing.T, sh *Shard) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sh.Run(ctx)
		close(done)
	}()
	// Run sets runCtx on its own goroutine; HostZone refuses until it is set. Wait for readiness so the
	// test exercises adoption rather than a startup race.
	deadline := time.Now().Add(5 * time.Second)
	for {
		sh.mu.Lock()
		ready := sh.runCtx != nil
		sh.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("shard never became ready to host zones")
		}
		time.Sleep(time.Millisecond)
	}
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("shard did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

// TestHostZoneSeedsTheAdoptedZonesReplica is the #280 regression. A drain-adopted zone must come up with the
// authoritative world AND region state already in its replica, not an empty one.
func TestHostZoneSeedsTheAdoptedZonesReplica(t *testing.T) {
	snap := fakeScopeSnapshot{
		world:  map[string][]byte{"invasion_active": []byte("true")},
		region: map[string]map[string][]byte{"heartlands": {"mood": []byte(`"tense"`)}},
	}
	sh := adoptShard(t, snap)
	stop := runShard(t, sh)

	z, err := sh.HostZone(context.Background(), "darkwood")
	if err != nil {
		t.Fatalf("HostZone: %v", err)
	}
	if z.scopes.regionID != "heartlands" {
		t.Fatalf("adopted zone regionID = %q, want heartlands", z.scopes.regionID)
	}

	// Wait for the zone loop to consume the seed, THEN stop the shard so the goroutine has exited and the
	// replica can be read without racing it.
	syncZone(t, z)
	stop()

	if got := string(z.scopes.world["invasion_active"]); got != "true" {
		t.Fatalf("adopted zone's WORLD replica = %v; the seed never landed, so a sticky world flag reads false there (#280)", z.scopes.world)
	}
	if got := string(z.scopes.region["mood"]); got != `"tense"` {
		t.Fatalf("adopted zone's REGION replica = %v; want mood=\"tense\" (#280)", z.scopes.region)
	}
}

// TestHostZoneSeedsBeforeExposingTheZone pins the ORDERING invariant the fix rests on, and the reason a
// naive seedFromSnapshot call inside registerZone is wrong: the snapshot must be read and posted while the
// zone is still invisible to the delta fan-out.
//
// zonesList (which world-scope deltas iterate) reads s.zones. If the zone were already published there, a
// delta could be posted to its inbox ahead of the seed, and applyScopeSeed's full-map replace would then
// overwrite newer state with the snapshot.
func TestHostZoneSeedsBeforeExposingTheZone(t *testing.T) {
	var mu sync.Mutex
	var exposedAtSeedTime bool

	sh := adoptShard(t, nil) // replaced below, once we can close over sh
	snap := hookedSnapshot{
		fakeScopeSnapshot: fakeScopeSnapshot{
			world:  map[string][]byte{"invasion_active": []byte("true")},
			region: map[string]map[string][]byte{"heartlands": {"mood": []byte(`"tense"`)}},
		},
		onWorldRead: func() {
			mu.Lock()
			defer mu.Unlock()
			// The zone must NOT be routable yet: adoptLocked has not run.
			exposedAtSeedTime = sh.zoneByID("darkwood") != nil
		},
	}
	sh.scopes.snapshot = snap

	stop := runShard(t, sh)
	if _, err := sh.HostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("HostZone: %v", err)
	}
	stop()

	mu.Lock()
	defer mu.Unlock()
	if exposedAtSeedTime {
		t.Fatal("the zone was already published in s.zones when its seed was read — a world delta could have " +
			"beaten the seed into its inbox, and the full-replace seed would clobber it (#280)")
	}
}

// TestSeedZoneIsANoOpWithoutASnapshotSource pins the bare-engine path: a shard with no store (the
// single-shard tests) adopts a zone with an empty replica and no panic, exactly as before.
func TestSeedZoneIsANoOpWithoutASnapshotSource(t *testing.T) {
	sh := adoptShard(t, nil) // WithScopeSnapshot(nil) leaves seeding disabled
	stop := runShard(t, sh)

	z, err := sh.HostZone(context.Background(), "darkwood")
	if err != nil {
		t.Fatalf("HostZone: %v", err)
	}
	syncZone(t, z)
	stop()

	if len(z.scopes.world) != 0 || len(z.scopes.region) != 0 {
		t.Fatalf("a shard with no snapshot source must adopt an unseeded zone, got world=%v region=%v",
			z.scopes.world, z.scopes.region)
	}
}

// TestSeedZoneSkipsTheRegionSeedForARegionlessZone: the demo's crypt is deliberately region-less. Adopting
// it must seed the world scope and leave the region replica empty — applyScopeSeed would drop the region
// seed anyway, but we should not read a region snapshot we cannot use.
func TestSeedZoneSkipsTheRegionSeedForARegionlessZone(t *testing.T) {
	var regionReads int
	var mu sync.Mutex
	snap := regionCountingSnapshot{
		fakeScopeSnapshot: fakeScopeSnapshot{
			world:  map[string][]byte{"invasion_active": []byte("true")},
			region: map[string]map[string][]byte{"heartlands": {"mood": []byte(`"tense"`)}},
		},
		onRegionRead: func() { mu.Lock(); regionReads++; mu.Unlock() },
	}
	sh := adoptShard(t, snap)
	stop := runShard(t, sh)

	// Boot already seeded midgaard, which IS a heartlands member — so zero the counter here. What we are
	// measuring is whether ADOPTING a region-less zone reads a region snapshot it could not use anyway.
	mu.Lock()
	regionReads = 0
	mu.Unlock()

	z, err := sh.HostZone(context.Background(), "crypt")
	if err != nil {
		t.Fatalf("HostZone: %v", err)
	}
	syncZone(t, z)
	stop()

	if z.scopes.regionID != "" {
		t.Fatalf("premise: crypt must be region-less, got regionID %q", z.scopes.regionID)
	}
	if got := string(z.scopes.world["invasion_active"]); got != "true" {
		t.Fatalf("a region-less zone must still get the WORLD seed: %v", z.scopes.world)
	}
	if len(z.scopes.region) != 0 {
		t.Fatalf("a region-less zone must have no region replica: %v", z.scopes.region)
	}
	mu.Lock()
	defer mu.Unlock()
	if regionReads != 0 {
		t.Fatalf("read the region snapshot %d times for a region-less zone; want 0", regionReads)
	}
}

type regionCountingSnapshot struct {
	fakeScopeSnapshot
	onRegionRead func()
}

func (r regionCountingSnapshot) SnapshotRegionState(ctx context.Context, regionID string) (map[string][]byte, error) {
	if r.onRegionRead != nil {
		r.onRegionRead()
	}
	return r.fakeScopeSnapshot.SnapshotRegionState(ctx, regionID)
}

// slowSnapshot blocks the world read until its context is cancelled, so a test can prove the caller's
// deadline (not just seedZone's own clock) bounds the read.
type slowSnapshot struct {
	fakeScopeSnapshot
	entered chan struct{}
	once    sync.Once
}

func (s *slowSnapshot) SnapshotWorldState(ctx context.Context) (map[string][]byte, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestSeedZoneHonoursTheCallersDeadline pins the fix for the drain-path stall (distsys review). AdoptZone
// used to ignore the caller's context, so a slow snapshot store made the DRAINING shard block on an RPC
// whose server-side read was running out its own independent clock. BeginDrain hands zones over serially,
// so that is drain budget spent per zone — during a failover, exactly when the store is most likely slow.
//
// Now the RPC's context reaches seedZone. Cancelling it aborts the read promptly, and the zone still comes
// up unseeded rather than the adoption failing.
func TestSeedZoneHonoursTheCallersDeadline(t *testing.T) {
	snap := &slowSnapshot{entered: make(chan struct{})}
	sh := adoptShard(t, snap)
	stop := runShard(t, sh)
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := sh.HostZone(ctx, "darkwood")
		done <- err
	}()

	select {
	case <-snap.entered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("the snapshot read never started")
	}
	cancel() // the draining source gave up

	// The tolerance must be well UNDER adoptSeedTimeout, or this passes even when the caller's context is
	// ignored: seedZone's own clock would expire and HostZone would return anyway, just later. Half the
	// budget is an unambiguous gap.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a cancelled seed must degrade to 'no seed', not fail the adoption: %v", err)
		}
	case <-time.After(adoptSeedTimeout / 2):
		t.Fatal("HostZone did not return promptly after the caller's context was cancelled — the seed read " +
			"is not bounded by the RPC deadline, so a slow store stalls the drain (#280)")
	}
}

// TestAdoptSeedTimeoutIsTighterThanBoot pins the intent: the adoption path sits on the drain critical path,
// so it must give up on a slow store faster than boot does. A regression here is silent (drains just get
// slower), so it is worth asserting.
func TestAdoptSeedTimeoutIsTighterThanBoot(t *testing.T) {
	if adoptSeedTimeout >= scopeSnapshotTimeout {
		t.Fatalf("adoptSeedTimeout (%v) must be tighter than the boot budget (%v): AdoptZone blocks the "+
			"draining source, and BeginDrain hands zones over serially", adoptSeedTimeout, scopeSnapshotTimeout)
	}
}
