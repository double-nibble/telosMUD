package world

import (
	"context"
	"testing"
	"time"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/content"
)

// unadopt_test.go — #327, the AdoptZone deadline inversion.
//
// AdoptZone makes this shard BUILD and run a zone; the source's HandoverZone flip, several steps later, is
// what actually gives it to us. Two ways the destination ends up with a permanent zombie:
//   - the source's drain deadline elapses while the RPC is in flight, so the source returns BEFORE its flip
//     and keeps the lease; or
//   - the flip never lands at all (the source died mid-drain, or lost a race).
//
// Either way this shard is left hosting a zone it will never own, whose renewal parks in the pre-confirm
// "adopting" state forever, running resets and subscribing to scope. Single-writer holds (the source keeps
// the lease, nothing routes here), so it is a leak, not a correctness break.
//
// The fix is ONE compensation point: the adopting renewer. It already bounds its own wait (adoptConfirmDeadline,
// #288); it now UN-ADOPTS the zone when that wait expires. This covers both cases, because both leave the
// renewer unconfirmed. It is deliberately NOT done synchronously in the AdoptZone handler on ctx.Err(): a
// concurrent sibling AdoptZone could have legitimately flipped ownership to us while this call's context died,
// and tearing the zone down then would delete a zone we now own. The renewer is immune — a landed flip makes
// its ClaimZone succeed, which sets confirmed and stops it ever un-adopting.

// TestUnconfirmedAdoptionUnadoptsTheZone: a runtime-adopted zone whose flip never confirms is torn back down.
func TestUnconfirmedAdoptionUnadoptsTheZone(t *testing.T) {
	old := adoptConfirmDeadline
	adoptConfirmDeadline = 150 * time.Millisecond
	t.Cleanup(func() { adoptConfirmDeadline = old })

	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	// The zone's lease is held by a live peer, so this shard's ClaimZone is refused on every tick and the
	// adoption never confirms — the flip never came.
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "addr-b", nil, nil).
		WithZoneLeasing(refusingLeaser{claims: make(chan struct{}, 1)}, "shard-b",
			300*time.Millisecond, 20*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	shardDone := make(chan struct{})
	go func() { sh.Run(ctx); close(shardDone) }()
	// Stop the shard and WAIT for it before the deferred adoptConfirmDeadline restore runs, or a renewer
	// goroutine still reading the deadline would race the cleanup's write.
	defer func() { cancel(); <-shardDone }()
	waitCond(t, "shard running", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil
	})

	// Adopt darkwood at runtime: the build succeeds and renewal starts in the pre-confirm adopting state.
	if _, err := sh.HostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if sh.ZoneByID("darkwood") == nil {
		t.Fatal("premise: the adopted zone must be hosted")
	}

	// The deadline elapses. Before #327 the renewer just returned, leaving the zone hosted forever.
	waitCond(t, "the unconfirmed adoption is un-adopted", func() bool { return sh.ZoneByID("darkwood") == nil })
	sh.mu.Lock()
	_, actorLive := sh.actorDone["darkwood"]
	_, renewing := sh.leaseStop["darkwood"]
	sh.mu.Unlock()
	if actorLive {
		t.Fatal("the abandoned zone's actor goroutine survives the abandoned adoption")
	}
	if renewing {
		t.Fatal("the abandoned adoption's renewal registration survives")
	}
	if sh.ZoneByID("midgaard") == nil {
		t.Fatal("abandoning one adoption tore down an unrelated zone")
	}
}

// perZoneLeaser grants or refuses ClaimZone per zone id, so a test can make one zone confirm (its renewer
// then never reads the deadline) while another is held unconfirmed. Owner reads are answered from the same
// table: a granted zone reads as owned by this shard, a refused one by a peer.
type perZoneLeaser struct {
	self   string
	refuse map[string]bool
}

func (l perZoneLeaser) ClaimZone(_ context.Context, zoneID, _ string, _ time.Duration) (bool, error) {
	return !l.refuse[zoneID], nil
}
func (perZoneLeaser) ReleaseZone(context.Context, string, string) error { return nil }
func (perZoneLeaser) HandoverZone(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}

func (l perZoneLeaser) ZoneLease(_ context.Context, zoneID string) (string, uint64, error) {
	if l.refuse[zoneID] {
		return "shard-peer", 1, nil
	}
	return l.self, 1, nil
}

// TestUnconfirmedBootZoneIsNotUnadopted is the guard the distsys review required, and the reason un-adoption
// is gated on `adopted` rather than merely on `!confirmed`.
//
// A shard boots MANY zones from its claim pool; only one is `home`. A boot zone that genuinely cannot confirm
// its lease within the deadline — a Redis outage right at boot while a peer grabbed the lapsed lease — must NOT
// be torn down. It holds a real claim; losing it is a fence condition, not a zone to delete. Only a RUNTIME
// adoption is ever un-adopted.
//
// Driven at the renewer level (and its goroutine JOINED) so nothing leaks a read of the deadline into the
// cleanup: the home zone's renewer confirms and never reads it; the zone under test runs on a renewer this
// test owns end to end.
func TestUnconfirmedBootZoneIsNotUnadopted(t *testing.T) {
	old := adoptConfirmDeadline
	adoptConfirmDeadline = 150 * time.Millisecond
	t.Cleanup(func() { adoptConfirmDeadline = old })

	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	// midgaard (home) is granted, so its boot renewer confirms on tick one and never reads the deadline.
	// darkwood is refused, standing in for a boot zone that has lost its lease.
	leaser := perZoneLeaser{self: "shard-b", refuse: map[string]bool{"darkwood": true}}
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "addr-b", nil, nil).
		WithZoneLeasing(leaser, "shard-b", 300*time.Millisecond, 20*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	shardDone := make(chan struct{})
	go func() { sh.Run(ctx); close(shardDone) }()
	defer func() { cancel(); <-shardDone }()
	waitCond(t, "shard running", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil
	})

	// Host darkwood so there is a real zone to (not) tear down, then retire the adopting renewer HostZone
	// started, before it can tick — this test supplies darkwood's renewal itself, as a BOOT zone (adopted
	// false).
	if _, err := sh.HostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("host: %v", err)
	}
	sh.retireZoneRenewal("darkwood")
	if sh.ZoneByID("darkwood") == nil {
		t.Fatal("premise: darkwood must be hosted")
	}

	// Run darkwood's renewal as a boot zone. It is refused, so it never confirms, hits the deadline, and — with
	// adopted=false — stops renewing WITHOUT un-adopting. Joined, so its deadline reads can never race cleanup.
	rctx, rcancel := context.WithCancel(context.Background())
	defer rcancel()
	done := make(chan struct{})
	go func() { sh.renewZoneLease(rctx, "darkwood", false); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the boot-zone renewer never gave up on its unconfirmable lease")
	}

	if sh.ZoneByID("darkwood") == nil {
		t.Fatal("a boot zone that lost its lease was UN-ADOPTED — this shard deleted a zone it legitimately " +
			"claimed, a self-inflicted outage (#327 must gate un-adoption on runtime adoption)")
	}
}

// TestUnadoptRefusesAZoneThatAcquiredPlayers: the compensation is best-effort by design, and "best" must never
// mean "drops a player". A player can bind into a zone via Handoff.Prepare before the source's flip. Keeping a
// zone we cannot safely tear down is the correct outcome — the leak is recoverable, a dropped player is not.
func TestUnadoptRefusesAZoneThatAcquiredPlayers(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShardFromContent(lc, []string{"midgaard", "darkwood"}, "midgaard", "addr-b", nil, nil).
		WithZoneLeasing(stubLeaser{owner: "shard-a", gen: 7}, "shard-b", time.Second, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sh.Run(ctx)
	waitCond(t, "boot zone actors armed", func() bool {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		return sh.runCtx != nil && len(sh.actorDone) == 2
	})

	z := sh.zoneByID("darkwood")
	reply := make(chan error, 1)
	z.post(prepareMsg{
		snap:  &handoffv1.PlayerSnapshot{CharacterId: "Traveller"},
		epoch: 1,
		token: "tok-1",
		reply: reply,
	})
	if err := <-reply; err != nil {
		t.Fatalf("prepare: %v", err)
	}
	waitCond(t, "the handed-off player landed", func() bool { return z.pop.Load() == 1 })

	sh.unadoptZone("darkwood", "test")

	if sh.zoneByID("darkwood") == nil {
		t.Fatal("the compensation tore down a zone holding a player mid-handoff — their carried snapshot is " +
			"the only copy of their state")
	}
}
