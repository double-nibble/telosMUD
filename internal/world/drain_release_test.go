package world

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	presencepkg "github.com/double-nibble/telosmud/internal/presence"
)

// drain_release_test.go pins #284 part 1: BeginDrain hands back the headroom it reserved on its targets when
// the drain finishes, instead of leaving each reservation to sit until its TTL lapses.
//
// Until then, for up to the reservation TTL after a drainer's players had actually landed and registered in
// the target's presence, BOTH the reservation and the now-real migrated load counted against that target. The
// over-count never overloads anyone — it is conservative — but it denies concurrent drainers real headroom,
// which is precisely the fleet-rollout case the reservation exists to coordinate.

// fakeReleaser records how each target's reservation was retired, and by whom.
//
// It asserts the DRAINER argument. That identity is the whole correctness premise of the retire: the
// reservation is a hash FIELD keyed by drainer, so a mismatch between the id used to reserve (`cfg.ShardID`,
// via placement.ChooseDrainTarget) and the id used to retire (`s.shardID`, set by WithZoneLeasing) means the
// retire touches nothing and the hold sits until its TTL — silently, with every test still green.
type fakeReleaser struct {
	mu       sync.Mutex
	deleted  []string // targets whose hold was dropped outright (no players sent)
	expired  []string // targets whose hold was shortened (players sent)
	drainers map[string]bool
	err      error
}

func newFakeReleaser() *fakeReleaser { return &fakeReleaser{drainers: map[string]bool{}} }

func (f *fakeReleaser) ReleaseDrainTarget(_ context.Context, target, drainer string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, target)
	f.drainers[drainer] = true
	return f.err
}

func (f *fakeReleaser) ExpireDrainTargetSoon(_ context.Context, target, drainer string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired = append(f.expired, target)
	f.drainers[drainer] = true
	return true, f.err
}

func (f *fakeReleaser) snapshot() (deleted, expired []string, drainers []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for d := range f.drainers {
		drainers = append(drainers, d)
	}
	return append([]string(nil), f.deleted...), append([]string(nil), f.expired...), drainers
}

// targets is every target retired, by either route.
func (f *fakeReleaser) targets() []string {
	del, exp, _ := f.snapshot()
	return append(del, exp...)
}

// assertDrainer fails unless every retire carried exactly the expected drainer id.
func (f *fakeReleaser) assertDrainer(t *testing.T, want string) {
	t.Helper()
	_, _, drainers := f.snapshot()
	if len(drainers) != 1 || drainers[0] != want {
		t.Fatalf("retire used drainer ids %v, want exactly [%q] — the reservation is a hash field keyed by "+
			"drainer, so a mismatch retires nothing and the hold sits until its TTL (#284)", drainers, want)
	}
}

// drainShard builds a two-zone shard with a leasing shard id (the drainer identity the reservation is keyed
// by) and the given releaser.
func drainShard(t *testing.T, rel DrainTargetReleaser) *Shard {
	t.Helper()
	sh := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil)
	sh.shardID = "shard-a" // normally set by WithZoneLeasing; the directory is not needed here
	if rel != nil {
		sh.WithDrainTargetReleaser(rel)
	}
	return sh
}

// runBoundedDrain runs BeginDrain with a short context. The zone actors are not running, so the reclaim probe
// falls through on ctx timeout — which is fine: we are asserting the release, which happens in a defer.
func runBoundedDrain(t *testing.T, sh *Shard, choose TargetChooser) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := sh.BeginDrain(ctx, choose, 1*time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// TestBeginDrainReleasesEachDistinctTargetOnce is the core of #284, and the constraint the review panel was
// explicit about: release is a WHOLE-DRAIN operation, once per distinct target.
//
// ReserveDrainTarget accumulates ALL of a drainer's zones into ONE hash field per target, so releasing
// per-zone would HDEL the sibling zones' reservations for that drainer/target pair — under-counting the
// headroom a concurrent drainer sees, which is the opposite of the bug being fixed.
func TestBeginDrainReleasesEachDistinctTargetOnce(t *testing.T) {
	rel := newFakeReleaser()
	sh := drainShard(t, rel)

	// Both zones choose the SAME target. Two zones, one distinct target: exactly one release.
	runBoundedDrain(t, sh, func(string, int) (string, string, error) {
		return "shard-b", "addr-b", nil
	})

	got := rel.targets()
	if len(got) != 1 || got[0] != "shard-b" {
		t.Fatalf("retires = %v; want exactly one retire of shard-b — a per-zone retire would wipe the "+
			"sibling zone's reservation for this drainer/target pair (#284)", got)
	}
	rel.assertDrainer(t, "shard-a")
}

// TestBeginDrainReleasesEveryDistinctTarget: a fat shard's zones spread across several peers as each fills.
// Each of those peers must get its headroom back.
func TestBeginDrainReleasesEveryDistinctTarget(t *testing.T) {
	rel := newFakeReleaser()
	sh := drainShard(t, rel)

	var n int
	var mu sync.Mutex
	runBoundedDrain(t, sh, func(string, int) (string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return "shard-b", "addr-b", nil
		}
		return "shard-c", "addr-c", nil
	})

	got := rel.targets()
	if len(got) != 2 {
		t.Fatalf("releases = %v; want one per distinct target", got)
	}
	seen := map[string]bool{got[0]: true, got[1]: true}
	if !seen["shard-b"] || !seen["shard-c"] {
		t.Fatalf("releases = %v; want shard-b and shard-c", got)
	}
}

// TestBeginDrainReleasesATargetWhoseHandoverFailed: `choose` already reserved headroom on the target before
// the handover was attempted. An abandoned zone's reservation is exactly the stale hold #284 is about, so it
// must be released even though that zone's players end up reclaimed from durable state instead.
func TestBeginDrainReleasesATargetWhoseHandoverFailed(t *testing.T) {
	rel := newFakeReleaser()
	sh := drainShard(t, rel)

	// No directory and no peer dialer, so handoverZoneTo fails for every zone. The drain degrades to
	// reclaim-from-durable — and still hands the reservation back.
	runBoundedDrain(t, sh, func(string, int) (string, string, error) {
		return "shard-b", "addr-b", nil
	})

	if got := rel.targets(); len(got) != 1 || got[0] != "shard-b" {
		t.Fatalf("releases = %v; a target whose handover failed must still have its reservation released", got)
	}
}

// TestBeginDrainReleasesNothingWhenNoTargetWasChosen: a chooser that finds no peer never reserved anything.
func TestBeginDrainReleasesNothingWhenNoTargetWasChosen(t *testing.T) {
	rel := newFakeReleaser()
	sh := drainShard(t, rel)

	runBoundedDrain(t, sh, func(string, int) (string, string, error) {
		return "", "", errors.New("no live peer")
	})

	if got := rel.targets(); len(got) != 0 {
		t.Fatalf("releases = %v; want none (no target was ever chosen, so nothing was reserved)", got)
	}
}

// TestDrainReleaseFailureIsNotFatal: the per-field TTL is the correctness backstop, so a Redis blink during
// shutdown must not fail the drain.
func TestDrainReleaseFailureIsNotFatal(t *testing.T) {
	rel := newFakeReleaser()
	rel.err = errors.New("redis is down")
	sh := drainShard(t, rel)

	runBoundedDrain(t, sh, func(string, int) (string, string, error) {
		return "shard-b", "addr-b", nil
	})
	if got := rel.targets(); len(got) != 1 {
		t.Fatalf("the release was not attempted: %v", got)
	}
}

// TestDrainReleaseIsSkippedWithoutAReleaser pins the optional-port contract: a single-shard/dev world with no
// directory never reserved anything and must not panic.
func TestDrainReleaseIsSkippedWithoutAReleaser(t *testing.T) {
	sh := drainShard(t, nil)
	runBoundedDrain(t, sh, func(string, int) (string, string, error) {
		return "shard-b", "addr-b", nil
	})
}

// TestReleaseDrainTargetsUsesAFreshContext: BeginDrain's ctx is usually already past its deadline by the time
// the deferred release runs (the drain deadline is what got us here). If the release inherited it, it would
// be cancelled before leaving the process and the reservation would sit until its TTL — the exact behavior
// #284 removes.
func TestReleaseDrainTargetsUsesAFreshContext(t *testing.T) {
	var sawCancelled bool
	rel := &ctxProbeReleaser{onCall: func(ctx context.Context) { sawCancelled = ctx.Err() != nil }}
	sh := drainShard(t, rel)

	// An ALREADY-cancelled drain context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = sh.BeginDrain(ctx, func(string, int) (string, string, error) {
		return "shard-b", "addr-b", nil
	}, time.Second)

	if !rel.called {
		t.Fatal("the release never ran on a cancelled drain")
	}
	if sawCancelled {
		t.Fatal("the release inherited BeginDrain's cancelled context — it would never reach Redis (#284)")
	}
}

type ctxProbeReleaser struct {
	called bool
	onCall func(context.Context)
}

func (p *ctxProbeReleaser) ReleaseDrainTarget(ctx context.Context, _, _ string) error {
	p.called = true
	p.onCall(ctx)
	return nil
}

func (p *ctxProbeReleaser) ExpireDrainTargetSoon(ctx context.Context, _, _ string, _ time.Duration) (bool, error) {
	p.called = true
	p.onCall(ctx)
	return true, nil
}

// TestBeginDrainDropsTheHoldOfATargetThatGotNoPlayers: its handover failed, so it is holding headroom for
// players that will never arrive. That hold is pure waste and goes at once — a DELETE, not a shortened expiry.
func TestBeginDrainDropsTheHoldOfATargetThatGotNoPlayers(t *testing.T) {
	rel := newFakeReleaser()
	sh := drainShard(t, rel)

	// No directory and no peer dialer, so handoverZoneTo fails for every zone.
	runBoundedDrain(t, sh, func(string, int) (string, string, error) {
		return "shard-b", "addr-b", nil
	})

	deleted, expired, _ := rel.snapshot()
	if len(deleted) != 1 || deleted[0] != "shard-b" {
		t.Fatalf("deleted = %v; a target whose handover failed must have its hold DROPPED outright", deleted)
	}
	if len(expired) != 0 {
		t.Fatalf("expired = %v; a target that received no players must not keep a bridging hold", expired)
	}
}

// TestRetireDrainTargetsShortensTheHoldOfATargetThatGotPlayers is the safety property the orchestration review
// insisted on. Deleting a successful target's hold at drain completion would REOPEN the blind window the
// reservation exists to bridge: the players have landed, but the target's presence heartbeat has not yet
// reported their weight, so a concurrent drainer would read a stale low load, find no reservation, and
// over-commit. The hold is shortened to about one heartbeat instead.
func TestRetireDrainTargetsShortensTheHoldOfATargetThatGotPlayers(t *testing.T) {
	rel := newFakeReleaser()
	sh := drainShard(t, rel)

	// Drive the retire directly with a target marked as having received players: the unit-level BeginDrain
	// cannot complete a real handover (no directory, no peers).
	sh.retireDrainTargets(map[string]bool{"shard-b": true, "shard-c": false})

	deleted, expired, _ := rel.snapshot()
	if len(expired) != 1 || expired[0] != "shard-b" {
		t.Fatalf("expired = %v; a target that RECEIVED players must keep a shortened hold, not lose it "+
			"outright — deleting it reopens the presence-heartbeat blind window (#284)", expired)
	}
	if len(deleted) != 1 || deleted[0] != "shard-c" {
		t.Fatalf("deleted = %v; a target that received none must have its hold dropped", deleted)
	}
	rel.assertDrainer(t, "shard-a")
}

// TestPresenceReflectWindowOutlivesTheHeartbeat pins the constant's intent: the shortened hold must survive
// long enough for the target's presence heartbeat to report the migrated players, or the retire is unsafe.
func TestPresenceReflectWindowOutlivesTheHeartbeat(t *testing.T) {
	if presenceReflectWindow <= presencepkg.DefaultHeartbeat {
		t.Fatalf("presenceReflectWindow (%v) must exceed the presence heartbeat (%v): the hold's whole job "+
			"after a successful handover is to bridge until the target reports the migrated weight",
			presenceReflectWindow, presencepkg.DefaultHeartbeat)
	}
}
