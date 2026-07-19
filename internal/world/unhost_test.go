package world

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/test/bufconn"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/directory"
)

// unhost_test.go — #288's runtime zone teardown. The primitive is small; what needs pinning is every way it
// could be UNSAFE, and the one way a torn-down zone can come back wrong.

// unhostLeaser is a ZoneLeaser whose ZoneLease answers a settable owner, so a test can place the zone's
// ownership where it needs it. UnhostZone consults exactly this to decide whether the shard may let go.
type unhostLeaser struct {
	owner string
	err   error
}

func (unhostLeaser) ClaimZone(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (unhostLeaser) ReleaseZone(context.Context, string, string) error { return nil }
func (unhostLeaser) HandoverZone(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}

func (u unhostLeaser) ZoneLease(context.Context, string) (string, uint64, error) {
	return u.owner, 1, u.err
}

// unhostShard builds a running two-zone shard (home=midgaard) with the given leaser. It retains the demo
// content, because a zone that has been torn down can only come back by being REBUILT.
func unhostShard(t *testing.T, leaser ZoneLeaser) (*Shard, func()) {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil).
		WithZoneLeasing(leaser, "shard-a", 0, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sh.Run(ctx); close(done) }()
	// Wait for the ACTORS to be armed, not merely for runCtx to be set: Run publishes runCtx first and only
	// then launches the boot zones, so a test that raced that window would see a zone with no actor.
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})
	return sh, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("shard did not stop")
		}
	}
}

// TestUnhostZoneStopsTheActorAndRemovesTheZone is the headline. "Removed" is not "deleted from a map" — the
// actor goroutine must actually return, because it owns the zone's Lua VM and tears it down in its own defer.
// A teardown that only forgot the map entry would leave the goroutine pulsing forever, which is exactly the
// leak this closes.
func TestUnhostZoneStopsTheActorAndRemovesTheZone(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"}) // a peer owns darkwood now
	defer stop()

	if sh.zoneByID("darkwood") == nil {
		t.Fatal("premise: darkwood must be hosted")
	}
	sh.mu.Lock()
	done := sh.actorDone["darkwood"]
	sh.mu.Unlock()
	if done == nil {
		t.Fatal("premise: a hosted zone must have an actor done channel")
	}

	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("unhost: %v", err)
	}
	if sh.zoneByID("darkwood") != nil {
		t.Fatal("the zone is still in s.zones after UnhostZone")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the zone's actor goroutine never returned — the teardown only forgot the map entry")
	}
	// The shard keeps serving everything else. This is a zone-remove, not a shutdown.
	if sh.zoneByID("midgaard") == nil {
		t.Fatal("UnhostZone tore down a zone it was not asked to")
	}

	// Idempotent: unhosting a zone this shard does not host is a nil no-op, so a retrying coordinator never
	// sees an error for work already done.
	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("a second UnhostZone must be a no-op, got %v", err)
	}
}

// TestUnhostZoneRefusesAZoneThisShardStillOwns is the correctness guard, and the reason the primitive does a
// directory read at all.
//
// A zone this shard still owns is one the directory still routes players TO. Tearing it down would leave the
// renewal loop renewing a lease under nothing, and every arriving player would be told the zone is not hosted
// here — a black hole for as long as the lease keeps being renewed. The caller must hand the lease over first.
func TestUnhostZoneRefusesAZoneThisShardStillOwns(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{owner: "shard-a"}) // WE are the owner
	defer stop()

	err := sh.UnhostZone(context.Background(), "darkwood")
	if err == nil {
		t.Fatal("UnhostZone tore down a zone this shard still owns — arriving players would find no zone")
	}
	if !strings.Contains(err.Error(), "still owns") {
		t.Fatalf("the refusal must name the reason, got %v", err)
	}
	if sh.zoneByID("darkwood") == nil {
		t.Fatal("a refused UnhostZone must leave the zone hosted and serving")
	}
}

// TestUnhostZoneFailsClosedWhenOwnershipIsUnreadable: the ownership check is the only thing standing between a
// teardown and a black hole, so a directory it cannot read must abort. Leaving a zombie zone is a leak;
// tearing down a zone we may still own is a correctness break. Prefer the leak.
func TestUnhostZoneFailsClosedWhenOwnershipIsUnreadable(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{err: errors.New("redis: connection refused")})
	defer stop()

	if err := sh.UnhostZone(context.Background(), "darkwood"); err == nil {
		t.Fatal("UnhostZone must refuse when it cannot confirm it no longer owns the zone")
	}
	if sh.zoneByID("darkwood") == nil {
		t.Fatal("a failed ownership read must leave the zone hosted")
	}
}

// TestUnhostZoneRefusesAPopulatedZone: a resident player's session state may only be touched by the zone
// goroutine, so there is no correct way to evict them from the teardown path. Drain first.
func TestUnhostZoneRefusesAPopulatedZone(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
	defer stop()

	z := sh.zoneByID("darkwood")
	out := login(t, sh, z, "Resident") // a REAL player, not a poked counter
	defer drainChan(out)
	waitCond(t, "the player is resident", func() bool { return z.pop.Load() == 1 })

	err := sh.UnhostZone(context.Background(), "darkwood")
	if err == nil {
		t.Fatal("UnhostZone tore down a zone with a player still in it")
	}
	if !strings.Contains(err.Error(), "resident") {
		t.Fatalf("the refusal must name the reason, got %v", err)
	}
	if sh.zoneByID("darkwood") == nil {
		t.Fatal("a refused UnhostZone must leave the zone hosted")
	}
}

// TestUnhostZoneRefusesAZoneHoldingAPendingHandoff: a player rehydrated by Handoff.Prepare but not yet bound
// by the gate's re-dial lives in z.players too, so `pop` covers them. Pin it — the guarantee is what lets
// UnhostZone use one cheap atomic instead of a round-trip to the actor, and a future refactor of `prepare`
// that stopped calling setPlayer would silently start dropping mid-handoff players.
func TestUnhostZoneRefusesAZoneHoldingAPendingHandoff(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
	defer stop()

	z := sh.zoneByID("darkwood")
	reply := make(chan error, 1)
	z.claimInboundArrival() // the claim the production resolver takes under s.mu; the handler releases one unconditionally (#413)
	z.post(prepareMsg{
		snap:  &handoffv1.PlayerSnapshot{CharacterId: "Traveller"},
		epoch: 1,
		token: "tok-1",
		reply: reply,
	})
	if err := <-reply; err != nil {
		t.Fatalf("prepare: %v", err)
	}
	waitCond(t, "the pending player is counted", func() bool { return z.pop.Load() == 1 })

	if err := sh.UnhostZone(context.Background(), "darkwood"); err == nil {
		t.Fatal("UnhostZone tore down a zone holding a pending cross-shard handoff — that player would " +
			"never bind, and their carried snapshot is the only copy of their state")
	}
}

// TestUnhostZoneRefusesTheHomeAndLocalZones: a shard that has drained everything else still has to serve a
// lobby. The home zone and the embedded core-pack zones (#212, hosted unleased on every shard) are what make
// a standby a usable server rather than a black hole, and no rebalance ever moves them.
func TestUnhostZoneRefusesTheHomeAndLocalZones(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil).
		WithZoneLeasing(unhostLeaser{owner: "shard-b"}, "shard-a", 0, 0, nil).
		WithLocalZones("darkwood")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})

	if err := sh.UnhostZone(context.Background(), "midgaard"); err == nil {
		t.Fatal("UnhostZone removed the home zone")
	}
	if err := sh.UnhostZone(context.Background(), "darkwood"); err == nil {
		t.Fatal("UnhostZone removed a local bootstrap zone — the shard would have no core pack")
	}
	if sh.zoneByID("midgaard") == nil || sh.zoneByID("darkwood") == nil {
		t.Fatal("a refused UnhostZone must leave the zone hosted")
	}
}

// TestUnhostZoneClearsTheHandedOffFlag is the trap, and the reason this could not be a three-line function.
//
// `handedOff` tells this shard's renewal loop "stop renewing, do not fence" for a zone we deliberately gave
// away. Before teardown existed, a zone handed BACK to us hit HostZone's idempotent early return, whose
// re-adoption branch clears the flag. Once the zone is actually REMOVED, a re-adoption takes the full BUILD
// path instead — that branch never runs. A `handedOff` entry left behind would then be read by the rebuilt
// zone's fresh renewal loop, which bails on its first tick: the shard hosts and serves a zone whose lease it
// never renews, the 15s lease lapses, and any shard may claim a zone we are still writing to.
//
// That is precisely the #288 split-brain, re-entered through the door the teardown opens.
func TestUnhostZoneClearsTheHandedOffFlag(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
	defer stop()

	sh.markZoneHandedOff("darkwood")
	if !sh.zoneHandedOff("darkwood") {
		t.Fatal("premise: the zone must be flagged handed-off")
	}
	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("unhost: %v", err)
	}
	if sh.zoneHandedOff("darkwood") {
		t.Fatal("the handed-off flag outlived the zone: a rebuilt zone's renewal loop will bail on its " +
			"first tick, and the lease it is serving under will lapse (#288)")
	}

	// Prove it end-to-end: the coordinator hands darkwood back, HostZone REBUILDS it (the object is gone), and
	// the rebuilt zone must be renewing.
	z, err := sh.HostZone(context.Background(), "darkwood")
	if err != nil {
		t.Fatalf("re-adopt after teardown: %v", err)
	}
	if z == nil {
		t.Fatal("re-adopt returned no zone")
	}
	sh.mu.Lock()
	_, renewing := sh.leaseStop["darkwood"]
	sh.mu.Unlock()
	if !renewing {
		t.Fatal("a zone rebuilt after teardown is not renewing its lease")
	}
	if sh.zoneHandedOff("darkwood") {
		t.Fatal("the rebuilt zone is flagged handed-off")
	}
}

// TestUnhostedZoneCanBeReadoptedAndServes: the teardown must leave nothing behind that stops the zone coming
// back. A rebuilt zone is a NEW object with a fresh actor, and the shard must route to it.
func TestUnhostedZoneCanBeReadoptedAndServes(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
	defer stop()

	before := sh.zoneByID("darkwood")
	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("unhost: %v", err)
	}
	after, err := sh.HostZone(context.Background(), "darkwood")
	if err != nil {
		t.Fatalf("re-adopt: %v", err)
	}
	if after == before {
		t.Fatal("HostZone returned the torn-down zone object — the map entry was never removed")
	}
	if sh.zoneByID("darkwood") != after {
		t.Fatal("routing does not resolve to the rebuilt zone")
	}

	// Its actor is live: a FIFO round-trip through the inbox proves the loop is draining, which polling a flag
	// would not. (whoFallbackMsg renders on the zone goroutine and writes to the channel we hand it.)
	probe := make(chan presence, 1)
	after.post(presenceMsg{id: "nobody", reply: probe})
	select {
	case <-probe:
	case <-time.After(5 * time.Second):
		t.Fatal("the rebuilt zone's actor is not consuming its inbox")
	}
}

// TestUnhostZoneWithoutALeaserTrustsTheCaller: a single-shard/dev shard has no directory and therefore no
// ownership to confirm. The primitive must still work there (tests, and BeginDrain on a lone shard), with the
// caller responsible for the precondition the directory would otherwise enforce.
func TestUnhostZoneWithoutALeaserTrustsTheCaller(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})

	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("an unleased shard must be able to unhost, got %v", err)
	}
	if sh.zoneByID("darkwood") != nil {
		t.Fatal("the zone survived")
	}
}

// TestRebalanceUnhostsTheMigratedZone is #288's actual ask, at the level the leak occurs: after a coordinator
// rebalance moves a zone's players and its lease to a peer, the SOURCE must not keep the zone object.
//
// It uses a real Redis directory and a real gRPC AdoptZone over bufconn, because the teardown's ownership
// precondition is answered by the directory — a test with a fake leaser could not tell a correct teardown from
// one that tore down a zone it still owned. darkwood rather than midgaard: midgaard is A's HOME zone, which
// UnhostZone deliberately refuses.
func TestRebalanceUnhostsTheMigratedZone(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterShard(ctx, "shard-b", "addr-b", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-a"))

	lisA := bufconn.Listen(1 << 20)
	lisB := bufconn.Listen(1 << 20)
	lisByAddr := map[string]*bufconn.Listener{"addr-a": lisA, "addr-b": lisB}
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		lis := lisByAddr[addr]
		if lis == nil {
			return nil, fmt.Errorf("unknown shard %q", addr)
		}
		return handoffv1.NewHandoffClient(dialBuf(t, lis)), nil
	}
	noFence := func() {}

	shardA := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", dir, peers).
		WithZoneLeasing(dir, "shard-a", time.Second, 80*time.Millisecond, noFence)
	shardB := NewShardFromContent(lc, nil, "", "addr-b", dir, peers).
		WithZoneLeasing(dir, "shard-b", time.Second, 80*time.Millisecond, noFence)
	serveShard(t, shardA, lisA)
	serveShard(t, shardB, lisB)

	waitCond(t, "shard-a owns darkwood", func() bool {
		owner, gen, lerr := dir.ZoneLease(ctx, "darkwood")
		return lerr == nil && owner == "shard-a" && gen != 0
	})
	if shardA.ZoneByID("darkwood") == nil {
		t.Fatal("premise: A must host darkwood")
	}

	res, err := shardA.RebalanceZone(ctx, "darkwood", "shard-b", "addr-b", 5*time.Second)
	if err != nil {
		t.Fatalf("RebalanceZone: %v", err)
	}
	if res.Redirected != 0 || res.Reclaimed != 0 {
		t.Fatalf("an empty zone should move nobody, got %+v", res)
	}

	// The move happened...
	if owner, _ := dir.ShardForZone(ctx, "darkwood"); owner != "shard-b" {
		t.Fatalf("darkwood owner after rebalance = %q, want shard-b", owner)
	}
	if shardB.ZoneByID("darkwood") == nil {
		t.Fatal("B does not host darkwood after the rebalance")
	}
	// ...and A kept nothing. Before #288 this assertion failed: A held an empty, unowned, un-renewed zone whose
	// actor goroutine pulsed on forever, one per migration for the life of the process.
	if shardA.ZoneByID("darkwood") != nil {
		t.Fatal("the source shard still hosts the migrated zone — a zombie actor goroutine per rebalance (#288)")
	}
	shardA.mu.Lock()
	_, actorLive := shardA.actorDone["darkwood"]
	_, renewing := shardA.leaseStop["darkwood"]
	handedOff := shardA.handedOff["darkwood"]
	shardA.mu.Unlock()
	if actorLive {
		t.Fatal("the migrated zone's actor goroutine is still registered on the source")
	}
	if renewing {
		t.Fatal("the source is still renewing the migrated zone's lease")
	}
	if handedOff {
		t.Fatal("the handed-off flag outlived the zone; a rebuilt zone here would never renew its lease")
	}

	// A keeps serving its own zones — a zone-remove, not a shutdown.
	if shardA.ZoneByID("midgaard") == nil {
		t.Fatal("the rebalance tore down a zone it was not asked to")
	}
}

// TestDisarmZoneActorOnlyClearsItsOwnBookkeeping drives the identity check in disarmZoneActor DIRECTLY.
//
// The race it guards: a torn-down zone's goroutine unwinds asynchronously, and its deferred disarm both closes
// its done channel and clears s.actorStop/s.actorDone. If it cleared them by zone id alone, a HostZone that
// re-adopted the same id first would have its successor's entries evicted by the predecessor — leaving a live
// zone with no cancel and no done channel, so the next UnhostZone would stall for the whole grace period and
// then give up on a perfectly healthy actor.
//
// Driving this through UnhostZone+HostZone would be VACUOUS: UnhostZone waits on `done`, which disarm closes
// only after clearing the maps, so the predecessor's clear always lands first and the ordering under test is
// never produced. Arm the two generations by hand instead.
func TestDisarmZoneActorOnlyClearsItsOwnBookkeeping(t *testing.T) {
	sh := NewMultiShard([]string{"midgaard"}, "midgaard", "addr-a", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sh.mu.Lock()
	_, _, firstDone := sh.armZoneActorLocked(ctx, "darkwood")  // the zone being torn down
	_, _, secondDone := sh.armZoneActorLocked(ctx, "darkwood") // its replacement, armed first
	sh.mu.Unlock()

	// The predecessor's goroutine finally unwinds and disarms. It must touch only its own channel.
	sh.disarmZoneActor("darkwood", firstDone)

	select {
	case <-firstDone:
	default:
		t.Fatal("disarm did not close its own done channel; the UnhostZone waiting on it would hang")
	}
	select {
	case <-secondDone:
		t.Fatal("the predecessor's disarm closed the SUCCESSOR's done channel — the next UnhostZone would " +
			"return immediately while that zone's actor was still running")
	default:
	}
	sh.mu.Lock()
	_, haveStop := sh.actorStop["darkwood"]
	gotDone := sh.actorDone["darkwood"]
	sh.mu.Unlock()
	if !haveStop || gotDone != secondDone {
		t.Fatal("the predecessor's disarm evicted the successor's bookkeeping — the live zone now has no " +
			"cancel and no done channel, and the next UnhostZone stalls for the full grace period")
	}
}

// TestUnhostedZoneSurvivesTeardownRehostCycles is the smoke test over the real primitives: cycling a zone
// through teardown and re-adoption must leave it live and draining its inbox every time.
func TestUnhostedZoneSurvivesTeardownRehostCycles(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
	defer stop()

	for i := 0; i < 5; i++ {
		if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
			t.Fatalf("round %d: unhost: %v", i, err)
		}
		if _, err := sh.HostZone(context.Background(), "darkwood"); err != nil {
			t.Fatalf("round %d: re-host: %v", i, err)
		}
		sh.mu.Lock()
		_, haveStop := sh.actorStop["darkwood"]
		done := sh.actorDone["darkwood"]
		sh.mu.Unlock()
		if !haveStop || done == nil {
			t.Fatalf("round %d: the re-hosted zone lost its actor bookkeeping", i)
		}
	}
	probe := make(chan presence, 1)
	sh.zoneByID("darkwood").post(presenceMsg{id: "nobody", reply: probe})
	select {
	case <-probe:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-hosted zone's actor is not consuming its inbox")
	}
}

// TestPostToATornDownZoneDoesNotBlock is the shard-wide-wedge regression.
//
// `post` blocks until the inbox has room — the backpressure every caller depends on. After a teardown nothing
// drains that inbox, so a full one would block its sender forever. Most senders bail on a context; the SAVER
// does not. Its saveOk/saveConflict acks ride the ONE shared drainer goroutine that persists every zone on the
// shard, and it holds `req.zone` pointers to zones that may have been torn down under it. One wedged ack there
// is a durability outage for the whole shard.
//
// Fill the inbox past its buffer, tear the zone down, and prove a further post returns.
func TestPostToATornDownZoneDoesNotBlock(t *testing.T) {
	sh, stop := unhostShard(t, unhostLeaser{owner: "shard-b"})
	defer stop()

	z := sh.zoneByID("darkwood")
	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("unhost: %v", err)
	}

	// Saturate the (now undrained) inbox, then post one more. Without the teardown signal this blocks forever.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < cap(z.inbox)+16; i++ {
			z.post(saveOkMsg{id: "ghost", newVersion: 1}) // exactly what the shared saver drainer posts
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("posting to a torn-down zone blocked — the shared saver drainer wedges here, and every " +
			"zone on the shard stops being persisted")
	}
}

// TestUnhostZoneRefusesAZoneThatStillOwesADurableWrite is the bug the engine review found, and the reason
// "the zone is empty" cannot mean "pop == 0".
//
// A brand-new character who quits INSIDE their async CreateCharacter round-trip has no durable id yet, so
// leave() cannot flush them. It parks their final logout snapshot in pendingFinalFlush and removes them from
// z.players — pop drops to zero while a durable write is still owed. The createdMsg that stamps the fresh PID
// onto that snapshot and enqueues the saveFinal is delivered to THIS zone's inbox, and nowhere else.
//
// So a teardown gated only on pop would stop the actor, the createdMsg would land in a queue nobody drains,
// and everything the player did inside the create window would be lost silently — while UnhostZone's own
// doc comment claimed a late message to a torn-down zone is harmless.
func TestUnhostZoneRefusesAZoneThatStillOwesADurableWrite(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	store := &gateFailStore{
		MemStore: NewMemStore(),
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		// failErr nil: the create SUCCEEDS, just slowly. The stash will be replayed.
	}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil).
		WithPersistence(store, store).
		WithZoneLeasing(unhostLeaser{owner: "shard-b"}, "shard-a", 0, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})

	z := sh.zoneByID("darkwood")
	out := login(t, sh, z, "Doomed")
	<-store.entered // the create goroutine is live and blocked; pid is still nil
	drainChan(out)
	quit(t, z, "Doomed") // leave() parks the stash and drops the player

	waitCond(t, "the logout stash is parked", func() bool { return zoneProbe(z, "Doomed").stashed })
	if z.pop.Load() != 0 {
		t.Fatal("premise: the player must have left z.players (pop == 0) while the stash is parked")
	}

	// pop says empty. The zone is not. Teardown must refuse.
	err = sh.UnhostZone(context.Background(), "darkwood")
	if err == nil {
		t.Fatal("UnhostZone tore down a zone that still owed a durable write — the parked logout snapshot " +
			"and everything the player did inside the create window are silently lost")
	}
	if !strings.Contains(err.Error(), "parked logout flush") {
		t.Fatalf("the refusal must name the reason, got %v", err)
	}
	if sh.zoneByID("darkwood") == nil {
		t.Fatal("a refused UnhostZone must leave the zone hosted so it can still drain its own inbox")
	}

	// Let the create finish. The zone replays the stash, the saveFinal lands, and the write we protected is
	// actually on disk — asserting durability, not merely that the counter reached zero.
	close(store.release)
	waitCond(t, "the deferred logout flush is persisted", func() bool {
		_, ok, lerr := store.LoadCharacter(context.Background(), "Doomed")
		return lerr == nil && ok
	})
	waitCond(t, "the zone becomes quiescent", z.quiescent)

	// Now the teardown is allowed, and the durable row survives it.
	if err := sh.UnhostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("a quiescent zone must be unhostable, got %v", err)
	}
	if _, ok, _ := store.LoadCharacter(context.Background(), "Doomed"); !ok {
		t.Fatal("the durable row vanished across the teardown")
	}
}
