package world

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// instance_drain_test.go — #411's drain hole, which is a guaranteed ZERO-DROP VIOLATION on every SIGTERM,
// and the accounting hole that fixing it naively opens up in the other direction.
//
// BeginDrain excluded only local bootstrap zones, so instances entered the HANDOVER set. There, handoverZoneTo
// has no lease to flip and drainPlayer hands each occupant off IN PLACE to `z.id` — an instance id that no
// peer can resolve, let alone host. Every Prepare fails, every occupant stays resident, and all of them are
// dropped as stragglers at the deadline.
//
// The fix is two-sided. Excluding instances from the HANDOVER stops the failed handoffs. Keeping them in the
// ACCOUNTING set is what stops that exclusion from silently deleting their occupants from the drain readout:
// they are still flushed durably, still told to clean-disconnect + classify, and still counted. Ejecting them
// to a live destination (upgrading that clean reconnect to a seamless redirect) is slice 3's job, because the
// exit anchor that says WHERE is slice 3's.

// TestDrainExcludesInstancesFromTheHandover: an instance must never be offered to the handover machinery.
// The assertion is on the DIRECTORY: handoverZoneTo's first act is to read the zone's lease, so if an
// instance is in the handover set its (nonexistent) lease is read — which is both meaningless and the leading
// edge of a handover that can only fail.
func TestDrainExcludesInstancesFromTheHandover(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	markInstanceable(t, lc, "darkwood", "crypt") // the #72 content opt-in; see markInstanceable
	leaser := &recordingLeaser{fakeLeaser: newFakeLeaser()}
	dialFailed := func(string) (handoffv1.HandoffClient, error) {
		return nil, fmt.Errorf("no peer in this test")
	}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "", nil, dialFailed).
		WithZoneLeasing(leaser, "shard-a", time.Second, time.Hour, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	waitCond(t, "shard running", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil
	})

	inst := mustMint(t, sh, "darkwood", "acct-1")

	choose := func(_ string, _ int) (string, string, error) { return "shard-b", "addr-b", nil }
	if _, err := sh.BeginDrain(ctx, choose, 500*time.Millisecond); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}

	if n := leaser.reads(inst.id); n != 0 {
		t.Fatalf("the drain consulted the directory %d time(s) for INSTANCE %q — it is in the handover set, so "+
			"its occupants would be handed off to a zone id no peer can host and dropped at the deadline",
			n, inst.id)
	}
	// The control: an ordinary zone IS in the handover set, so the assertion above is about instancing.
	if n := leaser.reads("darkwood"); n == 0 {
		t.Fatal("the drain skipped the authored darkwood too — the exclusion is too broad")
	}
}

// TestDrainStillAccountsForInstanceOccupants is the other side of that exclusion, and the one that is easy to
// lose: an instance is excluded from the HANDOVER, never from the ACCOUNTING.
//
// Dropping instances out of the drain set entirely would trade an over-count of Redirected for a much worse
// under-count — their occupants would simply vanish from the tally, get no reclaim notice, and be dropped
// with the process with nothing in the readout saying so. The assertion is therefore on the RESULT: a player
// resident in an instance at the deadline must appear as Reclaimed, which is the drain's honest "you will
// reconnect from durable state" outcome, not as nothing at all.
func TestDrainStillAccountsForInstanceOccupants(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()

	// Build the instance BY HAND rather than via MintInstance so the occupant can be placed before its actor
	// starts — the test goroutine is then the only writer, exactly as at zone construction. (There is no entry
	// path in this slice; that is slice 3.)
	inst := newInstanceZone("darkwood#occupied", "darkwood")
	inst.shard = sh
	inst.protos = sh.protos
	inst.buildZone(sh.content)
	sh.adopt(inst.id, inst)
	sh.mu.Lock()
	sh.instances[inst.id] = &instanceRecord{id: inst.id, template: "darkwood", account: "acct-1", minted: time.Now()}
	sh.mu.Unlock()

	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	inst.newPlayerEntity(s, "Hero")
	s.entity.short = "Hero"
	Move(s.entity, inst.rooms["darkwood:room:grove"])
	inst.setPlayer("Hero", s)

	ictx, icancel := context.WithCancel(context.Background())
	defer icancel()
	go inst.Run(ictx)
	waitCond(t, "the instance's occupancy to be visible off-goroutine", func() bool { return inst.pop.Load() == 1 })

	// No peer: every handover fails, so this exercises the degraded path the instance shares with an ordinary
	// zone whose target is gone. What matters is that the instance's resident is IN the tally.
	choose := func(_ string, _ int) (string, string, error) { return "", "", fmt.Errorf("no peer in this test") }
	res, err := sh.BeginDrain(context.Background(), choose, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}
	if res.Reclaimed < 1 {
		t.Fatalf("the drain reported Reclaimed=%d (infra=%d client=%d): a player resident in an INSTANCE was "+
			"dropped out of the accounting entirely — no flush notice, no tally, nothing in the readout to say "+
			"they were dropped with the process", res.Reclaimed, res.ReclaimedInfra, res.ReclaimedClient)
	}
}

// TestMintRefusedWhileDraining. BeginDrain snapshots the hosted zones exactly once, so an instance minted
// after that snapshot is in NO drain set: not handed over (correct), but also not flushed and not reclaimed.
// Its occupants would be dropped silently and uncounted — the same hole the accounting fix above closes, from
// the other end. reserveInstanceSlot checked s.closed/runCtx/runWG but not s.draining.
func TestMintRefusedWhileDraining(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	// Mint once BEFORE, so the refusal below is provably about draining and not about the shard being unable
	// to mint at all in this fixture.
	mustMint(t, sh, "darkwood", "acct-1")

	sh.mu.Lock()
	sh.draining = true
	sh.mu.Unlock()

	if z, err := sh.MintInstance(context.Background(), "darkwood", "acct-1"); err == nil {
		t.Fatalf("MintInstance succeeded on a DRAINING shard (zone %q); the drain already snapshotted its "+
			"zones, so this instance is in no drain set at all and its occupants would be dropped uncounted", z.id)
	} else if !strings.Contains(err.Error(), "draining") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

// recordingLeaser counts ZoneLease READS per zone on top of the ordinary fake, so a test can assert the drain
// never even looked at a zone.
type recordingLeaser struct {
	*fakeLeaser
	seen map[string]int
}

func (r *recordingLeaser) ZoneLease(ctx context.Context, zoneID string) (string, uint64, error) {
	r.mu.Lock()
	if r.seen == nil {
		r.seen = map[string]int{}
	}
	r.seen[zoneID]++
	r.mu.Unlock()
	return r.fakeLeaser.ZoneLease(ctx, zoneID)
}

func (r *recordingLeaser) reads(zoneID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[zoneID]
}

// TestDrainWaitsForAnInstanceArrivalInFlight is the landmine the quiescence split armed for slice 3, defused
// before slice 3 can step on it.
//
// Splitting the step-2 wait onto the HANDOVER set is right for RESIDENCY: an instance is never redirected, so
// waiting for its pop to fall is waiting for something nothing will cause, and burning the deadline there
// starves the durable flush that is the only thing protecting its occupants. But quiescence is THREE counters,
// and `incoming` is not about redirection at all — it is #409's in-flight intra-shard ARRIVAL claim, taken by
// claimTransferTarget in the same mu hold that resolves the destination. Dropping instances out of that gate
// says "nobody is in flight" while somebody is.
//
// It is unreachable today (there is no entry path; that is slice 3). The moment slice 3 points
// claimTransferTarget at an instance destination, the drain concludes quiescent with a live session in flight,
// orders the flush + straggler reclaim AHEAD of the arrival, and the player lands in a zone that has already
// been flushed and disconnected — dropped with the process, uncounted and unflushed. allZonesQuiescent's own
// doc-comment describes that failure verbatim.
//
// So: the RESIDENCY wait stays over `handover`, and the `incoming` gate covers every zone. An occupied
// instance has pop > 0, not incoming > 0, so this keeps the "don't burn the deadline on a pinned instance"
// property intact.
func TestDrainWaitsForAnInstanceArrivalInFlight(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()

	inst := mustMint(t, sh, "darkwood", "acct-1")
	// The in-flight arrival claim, as claimTransferTarget takes it. The zone stays EMPTY (pop 0), which is the
	// whole trap: every residency counter says "drained".
	inst.incoming.Add(1)

	const deadline = 400 * time.Millisecond
	choose := func(_ string, _ int) (string, string, error) { return "", "", fmt.Errorf("no peer in this test") }
	start := time.Now()
	if _, err := sh.BeginDrain(context.Background(), choose, deadline); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}
	if elapsed := time.Since(start); elapsed < deadline/2 {
		t.Fatalf("the drain concluded quiescent in %s with an intra-shard transfer IN FLIGHT to an instance: it "+
			"will order the durable flush and the straggler reclaim ahead of the arrival, so the session lands "+
			"in a zone that has already been flushed and disconnected and is dropped with the process — "+
			"uncounted and unflushed (#409)", elapsed)
	}
}

// TestDrainDoesNotWaitOnAnOCCUPIEDInstance is the other side of the gate above, and the property the split
// exists to buy: an instance with a RESIDENT (pop > 0, incoming 0) must not hold the drain, because nothing
// will ever redirect them and the deadline spent waiting is deadline stolen from the durable flush that is
// the only thing protecting them.
func TestDrainDoesNotWaitOnAnOCCUPIEDInstance(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()

	inst := newInstanceZone("darkwood#pinned", "darkwood")
	inst.shard = sh
	inst.protos = sh.protos
	inst.buildZone(sh.content)
	sh.adopt(inst.id, inst)
	sh.mu.Lock()
	sh.instances[inst.id] = &instanceRecord{id: inst.id, template: "darkwood", account: "acct-1", minted: time.Now()}
	sh.mu.Unlock()

	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	inst.newPlayerEntity(s, "Hero")
	s.entity.short = "Hero"
	Move(s.entity, inst.rooms["darkwood:room:grove"])
	inst.setPlayer("Hero", s)

	ictx, icancel := context.WithCancel(context.Background())
	defer icancel()
	go inst.Run(ictx)
	waitCond(t, "the instance's occupancy to be visible off-goroutine", func() bool { return inst.pop.Load() == 1 })

	const deadline = 2 * time.Second
	choose := func(_ string, _ int) (string, string, error) { return "", "", fmt.Errorf("no peer in this test") }
	start := time.Now()
	if _, err := sh.BeginDrain(context.Background(), choose, deadline); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= deadline {
		t.Fatalf("the drain burned its whole %s deadline waiting for an OCCUPIED instance to quiesce; nothing "+
			"redirects an instance's residents, so that wait can only starve the durable flush in step 3",
			deadline)
	}
}

// blockingSnapshot is a ScopeSnapshotSource that can be ARMED to park its next read until the test releases
// it. It gives a deterministic hold INSIDE MintInstance's phase 2 (build + seed, off the lock), which is the
// window the mint's drain refusal has to cover and the only place a test can stand to prove it.
//
// It starts unarmed so the shard's own boot-time seed (startScopeReplication → seedFromSnapshot) is not the
// thing it catches.
type blockingSnapshot struct {
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (b *blockingSnapshot) arm() {
	b.mu.Lock()
	b.armed = true
	b.mu.Unlock()
}

func (b *blockingSnapshot) wait(ctx context.Context) {
	b.mu.Lock()
	armed := b.armed
	b.armed = false // one-shot: only the first read after arming parks
	b.mu.Unlock()
	if !armed {
		return
	}
	select {
	case b.entered <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
	case <-ctx.Done():
	}
}

func (b *blockingSnapshot) SnapshotWorldState(ctx context.Context) (map[string][]byte, error) {
	b.wait(ctx)
	return map[string][]byte{}, nil
}

func (b *blockingSnapshot) SnapshotRegionState(ctx context.Context, _ string) (map[string][]byte, error) {
	b.wait(ctx)
	return map[string][]byte{}, nil
}

// TestMintRefusedWhenTheDrainStartsMidBuild closes the race the reserve-time refusal alone does not cover.
//
// The `s.draining` check lives in phase 1 (reserveInstanceSlot); the phase-3 publish re-check tested
// s.closed / s.runCtx / s.runWG but NOT s.draining. Between them sits a full buildZone — every room spawned,
// every boot reset run — plus a seedZone store round trip: hundreds of milliseconds to seconds, and the
// whole window is a hole. A BeginDrain starting in it snapshots zonesList() BEFORE adoptLocked publishes, so
// the instance lands in NO drain set at all: not counted in `initial`, not flushed by s.Drain, not sent a
// reclaim notice. Its occupants would be dropped with the process, silently and uncounted — the exact hole
// the reserve-time refusal exists to close, entered through the back door.
func TestMintRefusedWhenTheDrainStartsMidBuild(t *testing.T) {
	src := &blockingSnapshot{entered: make(chan struct{}, 1), release: make(chan struct{})}
	sh, cancel := runningShardWith(t, []string{"midgaard", "darkwood"}, "midgaard", func(sh *Shard) {
		lc, err := content.LoadDemoPack()
		if err != nil {
			t.Fatal(err)
		}
		markInstanceable(t, lc, "darkwood", "crypt") // the #72 content opt-in; see markInstanceable
		sh.WithScopeBus(scopebus.New(commbus.NewMemBus()), lc.Regions).WithScopeSnapshot(src)
	})
	defer cancel()

	src.arm()
	type mintResult struct {
		z   *Zone
		err error
	}
	done := make(chan mintResult, 1)
	go func() {
		z, err := sh.MintInstance(context.Background(), "darkwood", "acct-1")
		done <- mintResult{z, err}
	}()

	select {
	case <-src.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the mint never reached its phase-2 seed; the test cannot hold it mid-build")
	}
	// The drain begins while the zone is built but not yet published — it cannot appear in zonesList().
	sh.mu.Lock()
	sh.draining = true
	sh.mu.Unlock()
	close(src.release)

	res := <-done
	if res.err == nil {
		t.Fatalf("MintInstance published instance %q even though the shard began draining mid-build. The "+
			"drain already snapshotted its zones, so this zone is in NO drain set: uncounted, unflushed, and "+
			"its occupants dropped with the process", res.z.id)
	}
	if !strings.Contains(res.err.Error(), "draining") {
		t.Fatalf("unexpected refusal: %v", res.err)
	}
	// And the slot must have been given back, or an abandoned mint permanently consumes the account's quota.
	sh.mu.Lock()
	held := len(sh.instances)
	sh.mu.Unlock()
	if held != 0 {
		t.Fatalf("the refused mint left %d instance record(s) behind; the cap slot leaked", held)
	}
}

// TestDrainDoesNotStallOnAZoneReapedMidDrain. The instance reaper rides runCtx and keeps sweeping DURING a
// drain — `s.draining` gates minting, not reaping — so an empty instance can be retired between BeginDrain's
// zonesList() snapshot and its own reclaim. That leaves a *Zone in the drain's slice whose actor is stopped.
//
// Step 3 posts on the RAW inbox channel rather than through z.post, so unlike every other sender it does not
// watch z.dead: the buffered send succeeds against a dead actor, nobody ever answers, and the drain sits
// there until the drain context expires. In cmd/telos-world that is the shutdown deadline — roughly a
// 45-second stall on SIGTERM, on a shard that has nothing left to do. It is an availability bug, not a
// durability one (the flush barriers run on their own fresh contexts), which is exactly why it would be
// mistaken for "shutdown is just slow".
//
// The reproduction is the end state that race produces: a zone in s.zones with a closed `dead` channel and no
// actor. What matters is that BeginDrain returns promptly rather than burning the caller's whole context.
func TestDrainDoesNotStallOnAZoneReapedMidDrain(t *testing.T) {
	// Persistence ENABLED, because step 3 has two raw-channel round trips against every zone and only one of
	// them runs on an ephemeral shard: s.Drain's own flush + wait (world.go) has the identical shape and the
	// identical stall, and it runs FIRST. A shard with no saver would exercise half the fix.
	sh, cancel := runningShardWith(t, []string{"midgaard", "darkwood"}, "midgaard", func(sh *Shard) {
		sh.WithPersistence(NewMemStore(), nil)
	})
	defer cancel()

	// A zone whose actor has already stopped, exactly as UnhostZone leaves one: dead closed, nothing draining
	// the inbox. It is quiescent, so step 2 has no reason to wait on it either.
	reaped := newInstanceZone("darkwood#reaped", "darkwood")
	reaped.shard = sh
	reaped.protos = sh.protos
	reaped.buildZone(sh.content)
	close(reaped.dead)
	sh.adopt(reaped.id, reaped)

	// The drain context is generous — the point is that the drain must not need it. A 45s production stall is
	// this same gap measured against cmd/telos-world's shutdown deadline.
	ctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	choose := func(_ string, _ int) (string, string, error) { return "", "", fmt.Errorf("no peer in this test") }
	start := time.Now()
	if _, err := sh.BeginDrain(ctx, choose, 300*time.Millisecond); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the drain took %s waiting on a zone whose actor is already gone: it posted the straggler "+
			"reclaim on the raw inbox channel, which a torn-down zone's buffer happily accepts, and then waited "+
			"for a reply nobody will ever send. On SIGTERM that is the whole shutdown deadline spent doing "+
			"nothing (~45s in cmd/telos-world)", elapsed)
	}
}
