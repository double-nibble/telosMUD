package world

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// anchor_rebalance_test.go — #421: a zone that is somebody's exit anchor must not be handed to a peer out
// from under them.
//
// The anchor is written into the placement record — the reconnect ROUTING key — on every durable write while
// a player is inside an instance. Its "the anchor names a zone THIS shard hosts" property is true at entry
// and is not maintained. Hand the lease to a peer and the record starts routing reconnects to a shard with no
// session for the character, which fresh-logs them from the durable row while this shard still holds the live
// one.
//
// WHAT THIS IS AND IS NOT. The correctness fence for the resulting double-own is #432 — an owner epoch at the
// durable sink, where it cannot be forgotten and where it covers the paths that have nothing to do with
// instances (a second login landing on another shard needs no rebalance at all). What this guard buys is
// playability: a party mid-dungeon does not have a reconnecting member pulled out to the anchor room while
// the rest of them are somewhere that member can no longer reach. So it FAILS OPEN by design.

// TestRebalanceIsDeferredWhileTheZoneIsSomeonesAnchor is the core of #421.
func TestRebalanceIsDeferredWhileTheZoneIsSomeonesAnchor(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()

	anchoredInstance(t, sh, "hero", "midgaard")

	if !sh.zoneIsAnchored(context.Background(), "midgaard") {
		t.Fatal("a zone with a live instance occupant anchored to it did not report as anchored")
	}
	// An unrelated zone must not be pinned by the same occupant — otherwise the guard would stop being a
	// guard and start being a blanket veto on rebalancing anything while any instance is live.
	if sh.zoneIsAnchored(context.Background(), "darkwood") {
		t.Fatal("an unrelated zone reported as anchored")
	}

	err := sh.settleAnchorsBeforeRebalance(context.Background(), "midgaard")
	if !errors.Is(err, errZoneAnchored) {
		t.Fatalf("settleAnchorsBeforeRebalance = %v, want errZoneAnchored — the move must be DEFERRED, and "+
			"deferred here, before handoverZoneTo, because the lease flip is what breaks the routing", err)
	}
}

// TestRebalanceProceedsWhenNobodyIsAnchoredThere. The guard must be cheap and invisible in the common case;
// a rebalance of a zone nobody is anchored to has to go through untouched.
func TestRebalanceProceedsWhenNobodyIsAnchoredThere(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()

	if _, err := sh.MintInstance(context.Background(), "darkwood", "acct-1"); err != nil {
		t.Fatalf("mint: %v", err)
	}
	// An instance exists but has no occupants, so nothing is anchored anywhere.
	if err := sh.settleAnchorsBeforeRebalance(context.Background(), "midgaard"); err != nil {
		t.Fatalf("an unanchored zone was refused: %v", err)
	}
}

// TestAnchorDeferIsBounded is the anti-starvation half, and it is not a nicety.
//
// A dungeon fed by a busy town has SOMEBODY inside essentially always, so an uncapped pin would defer that
// town's rebalance forever — and every deferred cycle burns a coordinator cooldown, so the load imbalance the
// rebalance exists to correct simply persists. Past the budget the occupants are ejected (they land at the
// anchor room and are redirected with everyone else) and the move goes ahead.
func TestAnchorDeferIsBounded(t *testing.T) {
	budget := anchorDeferBudget
	anchorDeferBudget = 50 * time.Millisecond
	t.Cleanup(func() { anchorDeferBudget = budget })

	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()

	anchoredInstance(t, sh, "hero", "midgaard")

	// First call starts the clock and defers.
	if err := sh.settleAnchorsBeforeRebalance(context.Background(), "midgaard"); !errors.Is(err, errZoneAnchored) {
		t.Fatalf("first attempt = %v, want a deferral", err)
	}

	waitCond(t, "the defer budget to expire and the move to be allowed through", func() bool {
		return sh.settleAnchorsBeforeRebalance(context.Background(), "midgaard") == nil
	})
}

// TestAnchorDeferClockResetsWhenTheAnchorGoesAway. A zone deferred earlier for an unrelated party must get a
// FULL budget the next time, not inherit a spent one — otherwise the second party's dungeon run is cut short
// by the first party's clock.
func TestAnchorDeferClockResetsWhenTheAnchorGoesAway(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()

	inst := anchoredInstance(t, sh, "hero", "midgaard")
	if err := sh.settleAnchorsBeforeRebalance(context.Background(), "midgaard"); !errors.Is(err, errZoneAnchored) {
		t.Fatalf("expected a deferral, got %v", err)
	}
	sh.mu.Lock()
	_, armed := sh.anchorDeferSince["midgaard"]
	sh.mu.Unlock()
	if !armed {
		t.Fatal("the defer clock was not started")
	}

	// The party leaves: no occupant, no anchor.
	clearAnchoredOccupant(t, sh, inst)
	if err := sh.settleAnchorsBeforeRebalance(context.Background(), "midgaard"); err != nil {
		t.Fatalf("after the anchor went away the move was still refused: %v", err)
	}
	sh.mu.Lock()
	_, stillArmed := sh.anchorDeferSince["midgaard"]
	sh.mu.Unlock()
	if stillArmed {
		t.Fatal("the defer clock survived the anchor going away; a later, unrelated deferral would inherit " +
			"a spent budget and be ejected immediately")
	}
}

// TestAnchorQueryFailsOpenOnATimeout. This guard is playability, not correctness (#432 is the fence), so
// every way it can fail to get an answer must let the rebalance through. A guard that could WEDGE the load
// balancer would be worse than the problem it addresses.
func TestAnchorQueryFailsOpenOnATimeout(t *testing.T) {
	barrier := anchorQueryBarrier
	anchorQueryBarrier = 20 * time.Millisecond
	t.Cleanup(func() { anchorQueryBarrier = barrier })

	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()

	anchoredInstance(t, sh, "hero", "midgaard")

	// An already-cancelled context stands in for every way the query can fail to complete.
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if sh.zoneIsAnchored(ctx, "midgaard") {
		t.Fatal("the anchor query reported anchored when it could not actually get an answer; it must fail " +
			"OPEN, because it is a playability guard and a wedged load balancer is the worse outcome")
	}
}

// anchoredInstance builds a live instance with one occupant whose exit anchor is anchorZone, and starts its
// actor.
//
// Built BY HAND rather than through MintInstance so the occupant can be placed BEFORE the actor starts — the
// test goroutine is then the only writer, exactly as at zone construction. Reaching into z.players once the
// actor is running would race the very thing the anchor query has to read safely. Same pattern the drain
// tests use.
func anchoredInstance(t *testing.T, sh *Shard, character, anchorZone string) *Zone {
	t.Helper()
	inst := newInstanceZone("darkwood#anchored", "darkwood")
	inst.shard = sh
	inst.protos = sh.protos
	if err := inst.buildZone(sh.liveContent()); err != nil {
		t.Fatalf("build the instance: %v", err)
	}
	sh.adopt(inst.id, inst)
	sh.mu.Lock()
	sh.instances[inst.id] = &instanceRecord{id: inst.id, template: "darkwood", account: "acct-1", minted: time.Now()}
	sh.mu.Unlock()

	s := &session{
		character:  character,
		out:        make(chan *playv1.ServerFrame, 256),
		epoch:      1,
		anchorZone: anchorZone,
		anchorRoom: "midgaard:room:square",
	}
	inst.newPlayerEntity(s, character)
	s.entity.short = character
	Move(s.entity, inst.rooms["darkwood:room:grove"])
	inst.setPlayer(character, s)

	ictx, icancel := context.WithCancel(context.Background())
	t.Cleanup(icancel)
	go inst.Run(ictx)
	waitCond(t, "the instance's occupancy to be visible off-goroutine", func() bool { return inst.pop.Load() == 1 })
	return inst
}

// clearAnchoredOccupant removes the placed session again, through the zone's OWN goroutine (the actor is
// running by now, so the map is no longer the test's to touch).
func clearAnchoredOccupant(t *testing.T, sh *Shard, inst *Zone) {
	t.Helper()
	// Go through ejectInstanceOccupants, not a raw ejectInstanceMsg. The message alone evicts with
	// drainEject=true, which claims via claimEjectTarget — and that only admits the arrival while the eject
	// WINDOW is open. Posting it bare makes the local claim fail and the evict fall through to a cross-shard
	// handoff, which is not what "the party walked out" means and needs a peer dialer this shard has none of.
	// The entry point opens the window, which is exactly what production does.
	sh.ejectInstanceOccupants(context.Background(), []*Zone{inst})
	waitCond(t, "the occupant to leave the instance", func() bool { return inst.pop.Load() == 0 })
}

// handoverProbeLeaser records whether the ownership flip was attempted, so a test can assert the anchor
// guard runs BEFORE it rather than merely returning an error afterwards.
type handoverProbeLeaser struct {
	mu         sync.Mutex
	handedOver []string
}

func (l *handoverProbeLeaser) ClaimZone(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (l *handoverProbeLeaser) ReleaseZone(context.Context, string, string) error { return nil }

func (l *handoverProbeLeaser) HandoverZone(_ context.Context, zoneID, _, _ string, _ time.Duration) (bool, error) {
	l.mu.Lock()
	l.handedOver = append(l.handedOver, zoneID)
	l.mu.Unlock()
	return true, nil
}

func (l *handoverProbeLeaser) ZoneLease(context.Context, string) (string, uint64, error) {
	return "shard-a", 1, nil
}

func (l *handoverProbeLeaser) flips() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.handedOver...)
}

// TestRebalanceZoneRefusesBeforeFlippingTheLease is the wiring test, and it pins the single most important
// property of this fix: WHERE the guard sits.
//
// A guard placed later — on quiescence, or inside UnhostZone — would be worse than no guard at all.
// ShardForZone follows the LEASE, not the hosting, so the flip is what breaks every anchored player's
// reconnect routing. Refusing after it would leave the routing broken AND leave this shard hosting a zone it
// no longer owns, which every subsequent teardown refuses, while runRebalance reports the move as complete.
//
// Without this test the guard could be moved below handoverZoneTo and every other test in this file would
// still pass, because they call settleAnchorsBeforeRebalance directly.
func TestRebalanceZoneRefusesBeforeFlippingTheLease(t *testing.T) {
	leaser := &handoverProbeLeaser{}
	sh, cancel := runningShardWith(t, []string{"midgaard", "darkwood"}, "midgaard", func(s *Shard) {
		s.WithZoneLeasing(leaser, "shard-a", time.Minute, time.Minute, nil)
	})
	defer cancel()

	anchoredInstance(t, sh, "hero", "midgaard")

	_, err := sh.RebalanceZone(context.Background(), "midgaard", "shard-b", "peer:9090", time.Second)
	if !errors.Is(err, errZoneAnchored) {
		t.Fatalf("RebalanceZone = %v, want errZoneAnchored", err)
	}
	if flips := leaser.flips(); len(flips) != 0 {
		t.Fatalf("the lease was handed over (%v) despite the refusal. The flip is what breaks the anchored "+
			"players' reconnect routing, so a guard that runs after it fixes nothing and additionally strands "+
			"this shard hosting a zone it does not own", flips)
	}
	if sh.zoneByID("midgaard") == nil {
		t.Fatal("the zone was torn down by a refused rebalance")
	}
}
