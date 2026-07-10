package placement

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSelectDrainTargetLeastLoadedExcludingSelf(t *testing.T) {
	cands := []ShardLoad{
		{ShardID: "shard-a", Players: 100}, // self
		{ShardID: "shard-b", Players: 40},
		{ShardID: "shard-c", Players: 10}, // least loaded
		{ShardID: "shard-d", Players: 25},
	}
	target, over := SelectDrainTarget(cands, "shard-a", 5, 1000)
	if target != "shard-c" {
		t.Errorf("target = %q, want shard-c (least loaded)", target)
	}
	if over {
		t.Error("overCeiling = true, want false (10+5 <= 1000)")
	}
}

func TestSelectDrainTargetTieBreakByID(t *testing.T) {
	cands := []ShardLoad{
		{ShardID: "shard-z", Players: 10},
		{ShardID: "shard-m", Players: 10},
		{ShardID: "shard-a", Players: 10},
	}
	target, _ := SelectDrainTarget(cands, "self", 1, 1000)
	if target != "shard-a" {
		t.Errorf("target = %q, want shard-a (tie broken by id)", target)
	}
}

func TestSelectDrainTargetOverCeiling(t *testing.T) {
	cands := []ShardLoad{{ShardID: "shard-b", Players: 990}}
	target, over := SelectDrainTarget(cands, "self", 20, 1000)
	if target != "shard-b" {
		t.Errorf("target = %q, want shard-b (least loaded even if over)", target)
	}
	if !over {
		t.Error("overCeiling = false, want true (990+20 > 1000)")
	}
}

func TestSelectDrainTargetNoCandidateButSelf(t *testing.T) {
	cands := []ShardLoad{{ShardID: "self", Players: 5}}
	if target, _ := SelectDrainTarget(cands, "self", 1, 1000); target != "" {
		t.Errorf("target = %q, want \"\" (only self is not a candidate)", target)
	}
}

// fakeDrainFleet is an in-memory DrainFleet: fixed candidates + endpoints, with a set of shard ids whose
// endpoint resolution "lapsed" (returns an error) to exercise the drop-and-reselect path.
type fakeDrainFleet struct {
	cands  []ShardLoad
	lapsed map[string]bool
	err    error
}

func (f fakeDrainFleet) DrainCandidates(context.Context) ([]ShardLoad, error) {
	return f.cands, f.err
}

func (f fakeDrainFleet) EndpointForShard(_ context.Context, id string) (string, error) {
	if f.lapsed[id] {
		return "", errors.New("lapsed")
	}
	return "addr-" + id, nil
}

// fakeReserver is a counting reservation with a per-target ceiling of `full` (once reserved reaches it,
// further reserves fail). Records the total reserved per target.
type fakeReserver struct {
	reserved map[string]int
	fullAt   map[string]int // target -> headroom cap; a reserve fails if reserved+incoming would exceed it
	released []string       // targets whose reservation was handed back (an abandoned selection, #284)
}

func newFakeReserver() *fakeReserver {
	return &fakeReserver{reserved: map[string]int{}, fullAt: map[string]int{}}
}

// ReleaseDrainTarget drops a reservation the selector abandoned (#284).
func (r *fakeReserver) ReleaseDrainTarget(_ context.Context, target, _ string) error {
	r.released = append(r.released, target)
	delete(r.reserved, target)
	return nil
}

func (r *fakeReserver) ReserveDrainTarget(_ context.Context, target, _ string, headroom, incoming int, _ time.Duration) (bool, error) {
	limit := headroom
	if c, ok := r.fullAt[target]; ok {
		limit = c
	}
	if r.reserved[target]+incoming > limit {
		return false, nil
	}
	r.reserved[target] += incoming
	return true, nil
}

func TestChooseDrainTargetReservesLeastLoaded(t *testing.T) {
	fleet := fakeDrainFleet{cands: []ShardLoad{
		{ShardID: "self", Players: 0},
		{ShardID: "shard-b", Players: 50},
		{ShardID: "shard-c", Players: 5},
	}}
	resv := newFakeReserver()
	id, addr, err := ChooseDrainTarget(context.Background(), fleet, resv, "self", 10, 1000, time.Second)
	if err != nil {
		t.Fatalf("ChooseDrainTarget: %v", err)
	}
	if id != "shard-c" || addr != "addr-shard-c" {
		t.Fatalf("got (%q,%q), want (shard-c, addr-shard-c)", id, addr)
	}
	if resv.reserved["shard-c"] != 10 {
		t.Errorf("reserved shard-c = %d, want 10", resv.reserved["shard-c"])
	}
}

func TestChooseDrainTargetReselectsWhenLeastLoadedIsFull(t *testing.T) {
	fleet := fakeDrainFleet{cands: []ShardLoad{
		{ShardID: "shard-b", Players: 40},
		{ShardID: "shard-c", Players: 5}, // least loaded, but its reservation is already full
	}}
	resv := newFakeReserver()
	resv.fullAt["shard-c"] = 0 // shard-c admits no reservation at all
	id, _, err := ChooseDrainTarget(context.Background(), fleet, resv, "self", 10, 1000, time.Second)
	if err != nil {
		t.Fatalf("ChooseDrainTarget: %v", err)
	}
	if id != "shard-b" {
		t.Errorf("target = %q, want shard-b (shard-c reservation full, re-selected)", id)
	}
}

func TestChooseDrainTargetProgressWhenAllFull(t *testing.T) {
	fleet := fakeDrainFleet{cands: []ShardLoad{
		{ShardID: "shard-b", Players: 40},
		{ShardID: "shard-c", Players: 5},
	}}
	resv := newFakeReserver()
	resv.fullAt["shard-b"] = 0
	resv.fullAt["shard-c"] = 0 // every candidate is reservation-full
	id, _, err := ChooseDrainTarget(context.Background(), fleet, resv, "self", 10, 1000, time.Second)
	if err != nil {
		t.Fatalf("ChooseDrainTarget must never stall a drain: %v", err)
	}
	if id != "shard-c" {
		t.Errorf("target = %q, want shard-c (progress beats the ceiling: least-loaded overall)", id)
	}
}

func TestChooseDrainTargetOverCeilingForcesProgress(t *testing.T) {
	// Every candidate's RAW load is already over the ceiling — the selector must still pick one, not error.
	fleet := fakeDrainFleet{cands: []ShardLoad{
		{ShardID: "shard-b", Players: 1200},
		{ShardID: "shard-c", Players: 1100},
	}}
	id, _, err := ChooseDrainTarget(context.Background(), fleet, newFakeReserver(), "self", 10, 1000, time.Second)
	if err != nil {
		t.Fatalf("over-ceiling must still make progress: %v", err)
	}
	if id != "shard-c" {
		t.Errorf("target = %q, want shard-c (least-loaded over the ceiling)", id)
	}
}

func TestChooseDrainTargetDropsLapsedEndpoint(t *testing.T) {
	fleet := fakeDrainFleet{
		cands:  []ShardLoad{{ShardID: "shard-b", Players: 40}, {ShardID: "shard-c", Players: 5}},
		lapsed: map[string]bool{"shard-c": true}, // shard-c reserved fine but its registration lapsed
	}
	resv := newFakeReserver()
	id, addr, err := ChooseDrainTarget(context.Background(), fleet, resv, "self", 10, 1000, time.Second)
	if err != nil {
		t.Fatalf("ChooseDrainTarget: %v", err)
	}
	if id != "shard-b" || addr != "addr-shard-b" {
		t.Errorf("got (%q,%q), want (shard-b, addr-shard-b) after dropping lapsed shard-c", id, addr)
	}

	// #284: the reserve on shard-c was ADMITTED before its endpoint failed to resolve. We are never sending
	// it players, so its hold must be released here — the caller only ever learns about the RETURNED target,
	// so if the selector does not clean up after itself the hold leaks until its TTL. That is the exact stale
	// hold this issue exists to remove, triggered by the endpoint race most likely during a fleet rollout.
	if len(resv.released) != 1 || resv.released[0] != "shard-c" {
		t.Fatalf("released = %v; want [shard-c] — a reservation on an abandoned target must be handed back", resv.released)
	}
	if resv.reserved["shard-c"] != 0 {
		t.Fatalf("shard-c still holds %d reserved players after being abandoned", resv.reserved["shard-c"])
	}
}

// TestChooseDrainTargetDoesNotReleaseARefusedReserve: a reserve that was REFUSED wrote nothing, so there is
// nothing to hand back. Releasing anyway would be a harmless HDEL, but it would also wipe a hold this drainer
// legitimately placed on that target for a DIFFERENT zone earlier in the same drain.
func TestChooseDrainTargetDoesNotReleaseARefusedReserve(t *testing.T) {
	fleet := fakeDrainFleet{
		cands: []ShardLoad{{ShardID: "shard-b", Players: 5}, {ShardID: "shard-c", Players: 40}},
	}
	resv := newFakeReserver()
	resv.fullAt["shard-b"] = 0 // every reserve on shard-b is refused

	id, _, err := ChooseDrainTarget(context.Background(), fleet, resv, "self", 10, 1000, time.Second)
	if err != nil {
		t.Fatalf("ChooseDrainTarget: %v", err)
	}
	if id != "shard-c" {
		t.Fatalf("got %q, want shard-c after shard-b refused", id)
	}
	if len(resv.released) != 0 {
		t.Fatalf("released = %v; a REFUSED reserve wrote nothing and must not be released — doing so would "+
			"wipe a hold this drainer placed on that target for a different zone (#284)", resv.released)
	}
}

func TestChooseDrainTargetNoPeer(t *testing.T) {
	fleet := fakeDrainFleet{cands: []ShardLoad{{ShardID: "self", Players: 0}}}
	if _, _, err := ChooseDrainTarget(context.Background(), fleet, newFakeReserver(), "self", 1, 1000, time.Second); err == nil {
		t.Error("want an error when no live non-draining peer exists")
	}
}
