package world

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// instance_reap_concurrency_test.go — #419: one wedged instance must not delay every other reap.
//
// The sweep was serial, and UnhostZone waits up to unhostActorGrace for a zone's actor to return. So a
// single wedged instance held every other reap behind it for the full grace, and k wedged instances held
// the tail for 10k seconds while the ticker coalesced behind the whole thing. Nothing about these teardowns
// is ordered with respect to each other, so the serialization bought nothing and cost the worst case.

// TestReapSweepIsConcurrentSoOneWedgedInstanceDoesNotBlockTheRest is the load-bearing test, and it is built
// so that a revert to a serial sweep FAILS rather than merely gets slower.
//
// The shape: one instance whose actor is wedged (it never returns, so UnhostZone waits out the full grace)
// plus several healthy ones. Serial, the healthy reaps cannot even BEGIN until the wedged one times out.
// Concurrent, they finish while it is still waiting. The assertion is on that ordering — the healthy ones
// completed before the wedged one's grace elapsed — not on a wall-clock threshold, so it does not become a
// flaky timing test on a loaded CI box.
func TestReapSweepIsConcurrentSoOneWedgedInstanceDoesNotBlockTheRest(t *testing.T) {
	// A grace long enough that a serial sweep could not possibly finish the healthy reaps inside it, but
	// short enough that the test's own failure case is quick.
	grace := unhostActorGrace
	unhostActorGrace = 2 * time.Second
	t.Cleanup(func() { unhostActorGrace = grace })

	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	// The wedged instance: hold its actor's done channel open so UnhostZone waits out the whole grace. This
	// is the real mechanism — a zone goroutine that does not return — reproduced by withholding the signal
	// the teardown blocks on.
	wedged, err := sh.MintInstance(context.Background(), "darkwood", "acct-wedged")
	if err != nil {
		t.Fatalf("mint the wedged instance: %v", err)
	}
	stuck := make(chan struct{})
	sh.mu.Lock()
	sh.actorDone[wedged.id] = stuck // UnhostZone will now wait on a channel nobody closes
	sh.mu.Unlock()
	// Released exactly once, and unconditionally — including on a failed assertion, since otherwise a failing
	// run leaks the sweep goroutine plus a worker parked for the rest of the grace. No cleanup restoring the
	// real done channel: re-populating actorDone by id alone for an already-unhosted zone is exactly the
	// pattern unhost.go warns against (a successor's entry evicted by a predecessor's key).
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(stuck) }) }
	defer release()

	healthy := make([]string, 0, 3)
	for i, acct := range []string{"acct-1", "acct-2", "acct-3"} {
		z, err := sh.MintInstance(context.Background(), "darkwood", acct)
		if err != nil {
			t.Fatalf("mint healthy instance %d: %v", i, err)
		}
		healthy = append(healthy, z.id)
	}

	// Reap them all in one batch, exactly as a sweep would.
	due := append([]string{wedged.id}, healthy...)
	var healthyDone atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	start := time.Now()
	go func() {
		defer wg.Done()
		sh.reapConcurrently(context.Background(), due)
	}()

	// Every healthy instance must be gone WELL before the wedged one's grace elapses. Serial, none of them
	// would even have been attempted yet.
	deadline := time.Now().Add(unhostActorGrace - 500*time.Millisecond)
	for time.Now().Before(deadline) {
		n := int64(0)
		for _, id := range healthy {
			if sh.zoneByID(id) == nil {
				n++
			}
		}
		healthyDone.Store(n)
		if n == int64(len(healthy)) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	elapsed := time.Since(start)
	if got := healthyDone.Load(); got != int64(len(healthy)) {
		t.Fatalf("only %d of %d healthy instances were reaped while ONE wedged instance was still timing out "+
			"(%v elapsed, grace %v). A serial sweep makes every reap wait out every wedged actor ahead of it "+
			"— k wedged instances delay the tail by k×%v (#419)",
			got, len(healthy), elapsed, unhostActorGrace, unhostActorGrace)
	}

	release() // let the wedged teardown finish so the sweep returns
	wg.Wait()
}

// TestConcurrentReapsOfOneTemplateLeaveTheGaugeAtZero is the test that would have caught the bug this
// change introduced, and it is worth stating why it is not a niche case.
//
// The per-template instance gauge is computed under the shard mutex, so concurrent teardowns COMPUTE in a
// serialized order. It used to be published after the actor wait — and that order is not serialized. Two
// workers retiring the last two copies of a template compute 1 and 0; if the one that computed 1 finishes
// its wait last, the gauge settles at 1 with nothing live, and because the reaper only publishes on
// teardown, nothing ever samples that series again. Permanently wrong, silently.
//
// Not a tight race either: the reorder window spans the whole actor wait, and instance ids are
// `<template>#<random>` so a sorted `due` dispatches same-template copies adjacently — into the same batch.
// Copies of one dungeon idling out together is the ordinary case, not the exotic one.
func TestConcurrentReapsOfOneTemplateLeaveTheGaugeAtZero(t *testing.T) {
	sh, cancel := runningShardWith(t, []string{"midgaard"}, "midgaard",
		withLimits(8, 16, 16, time.Minute))
	defer cancel()

	var ids []string
	for _, acct := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		z, err := sh.MintInstance(context.Background(), "darkwood", acct)
		if err != nil {
			t.Fatalf("mint for %s: %v", acct, err)
		}
		ids = append(ids, z.id)
	}

	sh.reapConcurrently(context.Background(), ids)

	// Every copy is gone, so the count for the template must be zero — whatever order the teardowns
	// finished in.
	sh.mu.Lock()
	live := sh.instanceCountLocked("darkwood")
	sh.mu.Unlock()
	if live != 0 {
		t.Fatalf("after reaping every copy, instanceCountLocked(darkwood) = %d, want 0", live)
	}
	if got := len(sh.instances); got != 0 {
		t.Fatalf("%d instance records survived a full concurrent reap", got)
	}
}

// TestUnhostClosesDeadEvenWhenTheActorWaitTimesOut. The removal is irreversible by the time the wait starts
// — zone out of s.zones, record out of s.instances, actor cancelled — so `dead` has to be closed on EVERY
// path out, not just the happy one.
//
// It is what makes a later post ABANDON its send rather than fill the inbox and block its sender forever,
// and the saver's shared drainer acks into a zone inbox with no context to bail on. The correlation is what
// makes it serious: the only way to reach the timeout is a wedged actor that has stopped draining its
// inbox, which is precisely the state where a straggler save-ack blocks — a shard-wide durable-write wedge
// of the same class as #288. The concurrent reaper made that exit routine rather than exotic.
func TestUnhostClosesDeadEvenWhenTheActorWaitTimesOut(t *testing.T) {
	grace := unhostActorGrace
	unhostActorGrace = 100 * time.Millisecond
	t.Cleanup(func() { unhostActorGrace = grace })

	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	inst, err := sh.MintInstance(context.Background(), "darkwood", "acct-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	stuck := make(chan struct{})
	defer close(stuck)
	sh.mu.Lock()
	sh.actorDone[inst.id] = stuck
	sh.mu.Unlock()

	err = sh.UnhostZone(context.Background(), inst.id)
	if err == nil {
		t.Fatal("precondition: the teardown should have timed out waiting for the wedged actor")
	}

	select {
	case <-inst.dead:
	default:
		t.Fatal("the zone was removed but z.dead was left OPEN on the timeout path. Every later post to it " +
			"then fills the inbox and blocks its sender forever — including the saver's drainer, which acks " +
			"into a zone inbox with no context to bail on: a shard-wide durable-write wedge")
	}
}

// TestReapSweepWaitsForItsBatch. The sweep must not fire-and-forget: the ticker would then keep
// re-dispatching teardowns for the same wedged instance every tick, piling unbounded goroutines onto a zone
// that takes the full grace to fail. Waiting means a slow sweep just delays the next one, which the ticker
// already coalesces.
func TestReapSweepWaitsForItsBatch(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	ids := make([]string, 0, 3)
	for _, acct := range []string{"acct-1", "acct-2", "acct-3"} {
		z, err := sh.MintInstance(context.Background(), "darkwood", acct)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		ids = append(ids, z.id)
	}

	sh.reapConcurrently(context.Background(), ids)

	// Every instance is gone by the time it RETURNS, not eventually — no polling here on purpose.
	for _, id := range ids {
		if sh.zoneByID(id) != nil {
			t.Fatalf("instance %q survived a sweep that had already returned; reapConcurrently must wait for "+
				"its batch, or successive ticks stack goroutines on the same zones", id)
		}
	}
}

// TestReapSweepStopsDispatchingOnceTheShardIsStopping. A cancelled shard should not keep starting new
// teardowns, but it must not abandon the ones already in flight either — those hold a zone half-torn-down.
func TestReapSweepStopsDispatchingOnceTheShardIsStopping(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard"}, "midgaard")
	defer cancel()

	var ids []string
	for _, acct := range []string{"acct-1", "acct-2"} {
		z, err := sh.MintInstance(context.Background(), "darkwood", acct)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		ids = append(ids, z.id)
	}

	ctx, stop := context.WithCancel(context.Background())
	stop()
	sh.reapConcurrently(ctx, ids)

	// DETERMINISTIC: with ctx already done, nothing is dispatched at all. A select over both a done ctx and a
	// free semaphore has two ready cases and Go chooses uniformly at random, so the old shape dispatched
	// roughly half the batch — which made this test a coin flip that asserted nothing either way.
	for _, id := range ids {
		if sh.zoneByID(id) == nil {
			t.Fatalf("instance %q was reaped by a sweep whose context was already cancelled; dispatch must "+
				"stop deterministically, not on average", id)
		}
	}
}

// TestReapSweepBoundsItsConcurrency. Unbounded parallelism is not the fix: each teardown takes s.mu, which
// is on the routing path every transfer and every mint uses, and the per-shard instance cap is 256. A shard
// in trouble answering with 256 concurrent teardowns all contending for the routing mutex is its own outage.
func TestReapSweepBoundsItsConcurrency(t *testing.T) {
	if instanceReapConcurrency <= 1 {
		t.Fatalf("instanceReapConcurrency = %d; the sweep would be serial again (#419)", instanceReapConcurrency)
	}
	if instanceReapConcurrency > 32 {
		t.Fatalf("instanceReapConcurrency = %d; each worker contends for the shard routing mutex, so this "+
			"must stay a small bound rather than approaching the per-shard instance cap", instanceReapConcurrency)
	}
}
