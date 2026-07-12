package main

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/world"
)

// drain_ttl_test.go pins the #334 invariant: the drain-target reservation TTL must outlast the drain.
//
// The reservation is placed ONCE, when ChooseDrainTarget selects a peer, and is NOT refreshed during the
// wait for the zones to empty. If the TTL were shorter than the deadline (it was 15s vs a 30s deadline), a
// slow-but-alive drain would lose its per-field hold mid-flight — a concurrent drainer would then read the
// target's stale pre-migration load with no reservation correcting it and over-commit.
func TestDrainReservationTTLCoversTheDrainDeadline(t *testing.T) {
	// The TTL must STRICTLY exceed the deadline, not merely equal it. The reservation is stamped in
	// BeginDrain's step-1 selection loop BEFORE the wait-for-empty clock starts, so an early zone's hold has
	// to cover that offset; and a player landing on the target right at the deadline needs one more presence
	// heartbeat before the target's registration reflects its weight. A TTL that only reached the deadline
	// would let a slow-but-alive drain lose its hold mid-flight and a concurrent drainer over-commit on the
	// target's stale load (#334). The margin is world.PresenceReflectWindow (asserted below).
	if drainReservationTTL <= drainHandoffDeadline {
		t.Fatalf("drainReservationTTL (%v) must EXCEED drainHandoffDeadline (%v): the reservation is placed "+
			"once and never refreshed, and is stamped before the wait clock starts, so it must outlast the "+
			"whole drain plus a reflect-window margin (#334)", drainReservationTTL, drainHandoffDeadline)
	}
}

// TestReservationTTLOutlivesThePresenceReflectWindow guards the OTHER side of the ordering the reservation
// machinery depends on: presence heartbeat (10s) < PresenceReflectWindow (12s) < drainReservationTTL. On a
// SUCCESSFUL handover the hold is shortened toward PresenceReflectWindow (ExpireDrainTargetSoon, #284), which
// is only meaningful if the base TTL is strictly larger. This asserts against the REAL exported
// world.PresenceReflectWindow — not a hardcoded mirror — so a future change to that constant is caught here
// rather than silently drifting.
func TestReservationTTLOutlivesThePresenceReflectWindow(t *testing.T) {
	if drainReservationTTL <= world.PresenceReflectWindow {
		t.Fatalf("drainReservationTTL (%v) must exceed world.PresenceReflectWindow (%v): a successful "+
			"handover's hold is retired TOWARD the reflect window, so the base TTL must be larger for the "+
			"shorten to bite rather than being a no-op (#284/#334)",
			drainReservationTTL, world.PresenceReflectWindow)
	}
}
