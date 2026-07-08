package director

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingReaper blocks in ReapDeadLetterMail until release is closed, and counts how many reaps STARTED —
// used to prove single-flight (a second due tick must not launch a reap while one is in flight).
type blockingReaper struct {
	started atomic.Int64
	release chan struct{}
}

func (r *blockingReaper) ReapDeadLetterMail(_ context.Context, _, _ time.Time) (int64, error) {
	r.started.Add(1)
	<-r.release
	return 0, nil
}

// fakeMailReaper records every ReapDeadLetterMail call's cutoffs so a test can assert the reap cadence +
// the retention windows without a database.
type fakeMailReaper struct {
	mu     sync.Mutex
	calls  []reapCall
	reaped int64 // returned from each call
}

type reapCall struct{ orphan, hard time.Time }

func (r *fakeMailReaper) ReapDeadLetterMail(_ context.Context, orphanCutoff, hardCutoff time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, reapCall{orphan: orphanCutoff, hard: hardCutoff})
	return r.reaped, nil
}

func (r *fakeMailReaper) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *fakeMailReaper) lastCall() reapCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[len(r.calls)-1]
}

// tickAndWait drives one onTick and waits for any off-actor reap goroutine it launched to finish.
func tickAndWait(d *Director) {
	d.onTick(context.Background())
	d.workers.Wait()
}

// TestMailReapCadenceAndCutoffs: the leader reaps at most once per interval, and passes cutoffs derived from
// the director clock (orphan = now-grace, hard = now-hardTTL).
func TestMailReapCadenceAndCutoffs(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r := &fakeMailReaper{}
	const interval, grace, hardTTL = time.Hour, 30 * 24 * time.Hour, 180 * 24 * time.Hour
	d := New("", newMemStore(), discardLog()).
		WithNow(clock.now).
		WithMailReaper(r, interval, grace, hardTTL)
	d.leader.Store(true)

	// First tick: due immediately (lastReapAt zero) → one reap with the right cutoffs.
	tickAndWait(d)
	if r.callCount() != 1 {
		t.Fatalf("first leader tick should reap once, got %d", r.callCount())
	}
	c := r.lastCall()
	if want := clock.now().Add(-grace); !c.orphan.Equal(want) {
		t.Fatalf("orphan cutoff = %v, want now-grace %v", c.orphan, want)
	}
	if want := clock.now().Add(-hardTTL); !c.hard.Equal(want) {
		t.Fatalf("hard cutoff = %v, want now-hardTTL %v", c.hard, want)
	}

	// Ticking again BEFORE the interval elapses does NOT reap.
	clock.advance(interval - time.Minute)
	tickAndWait(d)
	if r.callCount() != 1 {
		t.Fatalf("a reap fired before the interval elapsed: %d calls", r.callCount())
	}

	// Past the interval → reaps again.
	clock.advance(2 * time.Minute)
	tickAndWait(d)
	if r.callCount() != 2 {
		t.Fatalf("a reap did not fire after the interval elapsed: %d calls", r.callCount())
	}
}

// TestMailReapLeaderGated: a non-leader director never reaps (exactly one owner fleet-wide).
func TestMailReapLeaderGated(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	r := &fakeMailReaper{}
	d := New("", newMemStore(), discardLog()).
		WithNow(clock.now).
		WithMailReaper(r, time.Hour, time.Hour, time.Hour)
	// leader defaults false.
	tickAndWait(d)
	if r.callCount() != 0 {
		t.Fatalf("a non-leader director reaped mail: %d calls", r.callCount())
	}
	d.leader.Store(true)
	tickAndWait(d)
	if r.callCount() != 1 {
		t.Fatalf("the leader did not reap after promotion: %d calls", r.callCount())
	}
}

// TestMailReapDisabledWithoutReaper: with no reaper wired, the tick is a clean no-op (the standalone default).
func TestMailReapDisabledWithoutReaper(t *testing.T) {
	d := New("", newMemStore(), discardLog())
	if d.mailReaper != nil {
		t.Fatal("a director built without WithMailReaper must have no reaper")
	}
	d.leader.Store(true)
	tickAndWait(d) // must not panic / touch anything
}

// TestMailReapClampsHardTTLToGrace: a misconfigured hardTTL shorter than the orphan grace is clamped up, so
// the hard arm can never reap deliverable mail younger than the orphan window.
func TestMailReapClampsHardTTLToGrace(t *testing.T) {
	d := New("", newMemStore(), discardLog()).
		WithMailReaper(&fakeMailReaper{}, time.Hour, 30*24*time.Hour, 7*24*time.Hour) // hardTTL < grace
	if d.reapHardTTL != d.reapOrphanGrace {
		t.Fatalf("hardTTL %v not clamped up to the orphan grace %v", d.reapHardTTL, d.reapOrphanGrace)
	}
}

// TestMailReapSingleFlight: while one reap is in flight, a due tick does NOT launch a second.
func TestMailReapSingleFlight(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	release := make(chan struct{})
	r := &blockingReaper{release: release}
	d := New("", newMemStore(), discardLog()).
		WithNow(clock.now).
		WithMailReaper(r, time.Nanosecond, time.Hour, time.Hour) // tiny interval so every tick is "due"
	d.leader.Store(true)

	d.onTick(context.Background()) // launches a reap that blocks on release
	// Wait for that first reap to actually be in flight (the goroutine ran started.Add(1) and is blocked).
	deadline := time.Now().Add(2 * time.Second)
	for r.started.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first reap never started")
		}
		time.Sleep(time.Millisecond)
	}

	clock.advance(time.Hour)
	d.onTick(context.Background()) // due again, but the first is still in flight → no second launch
	if got := r.started.Load(); got != 1 {
		t.Fatalf("single-flight violated: %d reaps started while one was in flight", got)
	}
	close(release) // let the first finish
	d.workers.Wait()
}
