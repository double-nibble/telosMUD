package world

import (
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// world_reconcile_retry_test.go covers bounded-retry delivery of a DROPPED zone-shape reconcile (#191 PR
// 3/3, the #194 reliability piece). A dropped reconcile is worse than a dropped Lua reload — a lost REMOVE
// leaves a ghost room — so the reloader re-posts it with bounded backoff off a short-lived goroutine (the
// subscriber goroutine stays non-blocking). These tests fill a zone inbox to force the drop, then observe
// the retry via the inbox / the in-flight counter. All bus-less, no running loop.

// withFastReconcileRetry shrinks the retry timing for a test and restores it after (the knobs are package
// vars, like handoffRPCTimeout).
func withFastReconcileRetry(t *testing.T, backoff time.Duration, attempts int) {
	t.Helper()
	ob, oa := reconcileRetryBackoff, reconcileRetryAttempts
	reconcileRetryBackoff, reconcileRetryAttempts = backoff, attempts
	t.Cleanup(func() { reconcileRetryBackoff, reconcileRetryAttempts = ob, oa })
}

// fillInbox saturates z.inbox so the next postOrDrop fails (the drop the retry recovers from).
func fillInbox(z *Zone) {
	for i := 0; i < cap(z.inbox); i++ {
		if !z.postOrDrop(reloadLuaMsg{}) {
			return
		}
	}
}

func retryTestInvalidation() contentbus.Invalidation {
	return contentbus.Invalidation{
		Kind: content.KindZone, Ref: "rt", Pack: "reloadtest",
		Version: 1, Rooms: []string{"rt:room:hall"}, StartRoom: "rt:room:hall",
	}
}

// TestReconcileRetryDeliversAfterTransientFullInbox proves a reconcile dropped by a momentarily-full inbox
// is re-posted once the inbox drains — the ghost-room fix. The primary post fails (inbox full); the retry
// goroutine keeps trying, and as the test drains the backlog the reconcileZoneMsg lands.
func TestReconcileRetryDeliversAfterTransientFullInbox(t *testing.T) {
	withFastReconcileRetry(t, 5*time.Millisecond, 50)
	src := content.NewMemSource()
	src.SetPack(reloadTestPackMultiRoom())
	bus := contentbus.NewMemBus()
	defer bus.Close()
	s := newReloadShard(t, src, bus)
	z := s.Zone()
	defer s.reloader.stop() // cancel any lingering retry goroutine at test end

	fillInbox(z)
	if z.postOrDrop(reloadLuaMsg{}) {
		t.Fatal("precondition: inbox should be full (postOrDrop should fail)")
	}

	// The primary post fails (full) → a retry goroutine is spawned.
	s.reloader.reconcileZone(retryTestInvalidation())

	// Drain the backlog; as room opens the retry re-posts the reconcile, which we detect in the stream.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case m := <-z.inbox:
			if rm, ok := m.(reconcileZoneMsg); ok && rm.zoneRef == "rt" {
				return // the retry delivered the reconcile — ghost-room fix works
			}
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	t.Fatal("bounded retry never delivered the dropped reconcile into the inbox")
}

// TestReconcileRetryNotStarvedByCommsBudget is the #345 guard (reconcile direction): the zone-shape reconcile
// retry draws from its OWN budget (maxReconcileRetryGoroutines), so a fully-exhausted COMMS republish budget
// must not starve a reconcile retry. We pin the comms budget to zero and prove a dropped reconcile is still
// re-delivered — the converse of TestCommsRepublishRetryNotStarvedByReconcileBudget.
func TestReconcileRetryNotStarvedByCommsBudget(t *testing.T) {
	withFastReconcileRetry(t, 5*time.Millisecond, 50)
	src := content.NewMemSource()
	src.SetPack(reloadTestPackMultiRoom())
	bus := contentbus.NewMemBus()
	defer bus.Close()
	s := newReloadShard(t, src, bus)
	z := s.Zone()
	defer s.reloader.stop()

	// Comms republish budget exhausted to zero. If reconcile shared it, the reconcile drop below would be abandoned.
	oldComms := maxCommsRepublishRetryGoroutines
	maxCommsRepublishRetryGoroutines = 0
	t.Cleanup(func() { maxCommsRepublishRetryGoroutines = oldComms })

	fillInbox(z)
	s.reloader.reconcileZone(retryTestInvalidation())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case m := <-z.inbox:
			if rm, ok := m.(reconcileZoneMsg); ok && rm.zoneRef == "rt" {
				return // reconcile retry ran independently of the exhausted comms budget (#345)
			}
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	t.Fatal("reconcile was starved by the exhausted COMMS republish budget — the budgets are not independent (#345)")
}

// TestReconcileRetryExhausts proves the retry gives up after a bounded number of attempts (rather than
// spinning forever) when the inbox stays saturated — the in-flight goroutine terminates.
func TestReconcileRetryExhausts(t *testing.T) {
	withFastReconcileRetry(t, 10*time.Millisecond, 3)
	src := content.NewMemSource()
	src.SetPack(reloadTestPackMultiRoom())
	bus := contentbus.NewMemBus()
	defer bus.Close()
	s := newReloadShard(t, src, bus)
	z := s.Zone()
	defer s.reloader.stop()

	fillInbox(z) // never drained → every retry attempt fails

	s.reloader.reconcileZone(retryTestInvalidation())
	if got := s.reloader.retryInFlight.Load(); got != 1 {
		t.Fatalf("expected exactly one in-flight retry after the drop, got %d", got)
	}

	// After ~attempts×backoff the goroutine exhausts its attempts and exits (counter back to 0).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.reloader.retryInFlight.Load() == 0 {
			return // gave up cleanly
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("retry goroutine never exhausted its attempts (retryInFlight stayed > 0)")
}

// TestReconcileRetryCanceledByStop proves stop() promptly cancels an in-flight retry (rather than leaking
// a goroutine that keeps posting to a torn-down shard). A long backoff makes the cancel observable well
// before the attempts would naturally exhaust.
func TestReconcileRetryCanceledByStop(t *testing.T) {
	withFastReconcileRetry(t, 5*time.Second, 20) // ~100s if it ran to exhaustion
	src := content.NewMemSource()
	src.SetPack(reloadTestPackMultiRoom())
	bus := contentbus.NewMemBus()
	defer bus.Close()
	s := newReloadShard(t, src, bus)
	z := s.Zone()

	fillInbox(z)
	s.reloader.reconcileZone(retryTestInvalidation())
	if got := s.reloader.retryInFlight.Load(); got != 1 {
		t.Fatalf("expected one in-flight retry, got %d", got)
	}

	s.reloader.stop() // closes retryDone → the retry goroutine returns at its next select

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.reloader.retryInFlight.Load() == 0 {
			return // canceled promptly, well under the 5s backoff
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stop() did not cancel the in-flight retry goroutine")
}
