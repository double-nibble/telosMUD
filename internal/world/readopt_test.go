package world

import (
	"context"
	"testing"
	"time"
)

// readopt_test.go pins #288's reachable half: a zone this shard once handed away, and that the coordinator
// later hands BACK, must renew its lease again.
//
// `handedOff` was set-only. The renewal loop consults it to stop renewing without fencing, so a re-adopted
// zone's renewer returned on its first tick and the lease quietly lapsed — while this shard kept hosting and
// serving the zone. ShardForZone then resolves nobody, and any shard may ClaimZone it: a second host for a
// zone we are still writing to. That breaks single-writer.

// leaseShard builds a running two-zone shard with a leasing id and a fake leaser.
func leaseShard(t *testing.T, leaser ZoneLeaser) (*Shard, func()) {
	t.Helper()
	sh := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil).
		WithZoneLeasing(leaser, "shard-a", 0, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sh.Run(ctx); close(done) }()
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
			t.Fatal("shard never became ready")
		}
		time.Sleep(time.Millisecond)
	}
	return sh, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("shard did not stop")
		}
	}
}

// nopLeaser satisfies ZoneLeaser without a directory.
type nopLeaser struct{}

func (nopLeaser) ClaimZone(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (nopLeaser) ReleaseZone(context.Context, string, string) error { return nil }

// ZoneLease reports this shard as the live owner at a fixed generation. These tests drive the re-adoption
// path through HostZone directly, never through handoverZoneTo, so the generation never has to move here; the
// moving-generation case is covered end-to-end by TestKeyedZoneHandoverEndToEnd's A->B->A leg.
func (nopLeaser) ZoneLease(context.Context, string) (string, uint64, error) {
	return "shard-a", 1, nil
}

func (nopLeaser) HandoverZone(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}

// TestReadoptedZoneRenewsItsLeaseAgain is the regression. Hand darkwood away, then have the coordinator hand
// it back: the shard must resume renewing its lease, or the lease lapses under a zone it is still serving.
func TestReadoptedZoneRenewsItsLeaseAgain(t *testing.T) {
	sh, stop := leaseShard(t, nopLeaser{})
	defer stop()

	sh.markZoneHandedOff("darkwood")
	sh.mu.Lock()
	_, renewing := sh.leaseStop["darkwood"]
	sh.mu.Unlock()
	if renewing {
		t.Fatal("premise: the handoff must have stopped renewal")
	}
	if !sh.zoneHandedOff("darkwood") {
		t.Fatal("premise: the zone must be flagged handed-off")
	}

	// The coordinator rebalances darkwood back to us. AdoptZone -> HostZone.
	if _, err := sh.HostZone(context.Background(), "darkwood"); err != nil {
		t.Fatalf("re-adopt: %v", err)
	}

	if sh.zoneHandedOff("darkwood") {
		t.Fatal("a re-adopted zone is still flagged handed-off: its renewal loop returns on the first tick, " +
			"and its lease lapses while this shard keeps serving it (#288)")
	}
	sh.mu.Lock()
	_, renewing = sh.leaseStop["darkwood"]
	sh.mu.Unlock()
	if !renewing {
		t.Fatal("a re-adopted zone is not renewing its lease — it will lapse, ShardForZone will resolve " +
			"nobody, and any shard may ClaimZone a zone this one is still writing to (#288)")
	}
}

// TestReadoptionDoesNotRebuildTheZone pins the #262 property that survives: the early return still returns the
// SAME zone object. A replayed AdoptZone must never rebuild a zone (fresh rooms, fresh resets, a duplicate
// actor goroutine) under the live one.
func TestReadoptionDoesNotRebuildTheZone(t *testing.T) {
	sh, stop := leaseShard(t, nopLeaser{})
	defer stop()

	before := sh.zoneByID("darkwood")
	sh.markZoneHandedOff("darkwood")
	after, err := sh.HostZone(context.Background(), "darkwood")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("HostZone REBUILT a zone it already hosts — a replayed AdoptZone would duplicate its actor, " +
			"rooms and resets under the live one (#262)")
	}
}

// TestReadoptionOfANeverHandedOffZoneIsAPureNoOp: the ordinary idempotent hit must not touch renewal at all.
// Restarting it on every AdoptZone would let a replay churn the renewal goroutine.
func TestReadoptionOfANeverHandedOffZoneIsAPureNoOp(t *testing.T) {
	sh, stop := leaseShard(t, nopLeaser{})
	defer stop()

	sh.mu.Lock()
	stopFn := sh.leaseStop["darkwood"]
	sh.mu.Unlock()
	if stopFn == nil {
		t.Fatal("premise: a boot zone should be renewing")
	}

	if _, err := sh.HostZone(context.Background(), "darkwood"); err != nil {
		t.Fatal(err)
	}
	sh.mu.Lock()
	stopFn2 := sh.leaseStop["darkwood"]
	sh.mu.Unlock()
	if stopFn2 == nil {
		t.Fatal("renewal was torn down by an idempotent re-host")
	}
	if sh.zoneHandedOff("darkwood") {
		t.Fatal("an idempotent re-host set the handed-off flag")
	}
}

// --- the bounded adopting state (#288, security review) ----------------------------------------------

// refusingLeaser always refuses the claim: it models another shard holding a live lease.
type refusingLeaser struct {
	claims chan struct{}
}

func (r refusingLeaser) ClaimZone(context.Context, string, string, time.Duration) (bool, error) {
	select {
	case r.claims <- struct{}{}:
	default:
	}
	return false, nil // another owner holds a LIVE lease
}

func (refusingLeaser) ReleaseZone(context.Context, string, string) error {
	return nil
}

func (refusingLeaser) ZoneLease(context.Context, string) (string, uint64, error) {
	return "shard-other", 1, nil // someone else holds a live lease
}

func (refusingLeaser) HandoverZone(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}

// TestUnconfirmedAdoptionAbandonsTheZone is the bound the security review required.
//
// The pre-confirm "adopting" state used to idle forever, polling ClaimZone for the shard's whole lifetime.
// ClaimZone is fenced only against a LIVE lease, so such a renewer WINS the zone the moment its rightful
// owner's lease lapses — a crash, a partition, a GC pause past the 15s TTL — and starts writing to a zone it
// was never given. An AdoptZone whose HandoverZone flip never lands — the source died mid-drain — plants
// exactly that, and no signature check can tell it apart from a healthy adoption at the moment it arrives.
//
// A legitimate adoption confirms within a round trip of its flip, so the deadline never bites it.
func TestUnconfirmedAdoptionAbandonsTheZone(t *testing.T) {
	old := adoptConfirmDeadline
	adoptConfirmDeadline = 150 * time.Millisecond
	t.Cleanup(func() { adoptConfirmDeadline = old })

	leaser := refusingLeaser{claims: make(chan struct{}, 1)}
	sh := NewMultiShard([]string{"midgaard"}, "midgaard", "addr-a", nil, nil).
		WithZoneLeasing(leaser, "shard-a", 300*time.Millisecond, 20*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { sh.renewZoneLease(ctx, "darkwood", true); close(done) }()

	// It must poll at least once (it is genuinely adopting) ...
	select {
	case <-leaser.claims:
	case <-time.After(3 * time.Second):
		t.Fatal("the adopting renewer never tried to claim")
	}
	// ... and then give up, rather than camp on the zone forever waiting for its owner to blink.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("an adoption whose flip never landed kept polling ClaimZone forever; it would seize the zone " +
			"the instant the true owner's lease lapsed (#288)")
	}
}

// TestAdoptConfirmDeadlineCoversALegitimateFlip pins the sizing: the bound must comfortably exceed the window
// in which a real handoff's HandoverZone flip lands, or it would abandon legitimate adoptions.
func TestAdoptConfirmDeadlineCoversALegitimateFlip(t *testing.T) {
	if adoptConfirmDeadline < pendingTTL {
		t.Fatalf("adoptConfirmDeadline (%v) must be at least the handoff's pendingTTL (%v): a legitimate "+
			"adoption's flip follows its AdoptZone by a round trip, and abandoning it would drop the zone",
			adoptConfirmDeadline, pendingTTL)
	}
}

// TestConfirmedRenewerIsNotSubjectToTheDeadline: once we have actually held the lease, the deadline is behind
// us — a long-lived owner must never abandon its own zone.
func TestConfirmedRenewerIsNotSubjectToTheDeadline(t *testing.T) {
	old := adoptConfirmDeadline
	adoptConfirmDeadline = 50 * time.Millisecond
	t.Cleanup(func() { adoptConfirmDeadline = old })

	sh := NewMultiShard([]string{"midgaard"}, "midgaard", "addr-a", nil, nil).
		WithZoneLeasing(nopLeaser{}, "shard-a", 300*time.Millisecond, 20*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sh.renewZoneLease(ctx, "midgaard", false); close(done) }()

	// nopLeaser always grants the claim, so the renewer confirms on tick one and must keep running well past
	// the (tiny) adopting deadline.
	select {
	case <-done:
		t.Fatal("a CONFIRMED owner abandoned its own zone's lease renewal")
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	<-done
}
