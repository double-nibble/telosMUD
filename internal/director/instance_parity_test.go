package director

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/world"
)

// instance_parity_test.go — the director half of #411's parity guards (the world half, on the ref charset,
// is internal/world/instance_parity_test.go). House pattern: internal/luasandbox.
//
// It lives HERE and not in internal/world because world must not import director — the dependency runs
// director -> world — so world's reservedScheduleEvents list duplicates SpawnBossEvent as a bare string
// literal. This test is the only thing binding the two ends of that duplication.

// TestReservedScheduleEventParity. world withholds engine-RESERVED scoped events from runtime-minted zone
// instances (#411), and today that list is exactly the scheduled-boss spawn this package broadcasts.
//
// The exclusion exists because a schedule is a WORLD-scope object with world-scope scarcity: one boss, one
// loot table, one timer. The broadcast fans out to every hosted zone, so without the exclusion one schedule
// spawns that boss in the shared zone AND in every live private copy simultaneously — and each kill signals
// boss.died back up, rescheduling the ONE shared world timer, last-writer-wins. The scarce thing stops being
// scarce and the shared timer becomes player-drivable.
//
// Rename SpawnBossEvent and, without this test, world's literal simply stops matching: the fan-out is
// re-enabled in every instance, with no compile error, no runtime error, and no failing test anywhere. This
// binds the two so that rename fails the build.
func TestReservedScheduleEventParity(t *testing.T) {
	if !world.ReservedScheduleEvent(SpawnBossEvent) {
		t.Fatalf("director.SpawnBossEvent = %q is NOT in world's reserved-schedule-event list. The two are "+
			"duplicated literals (world must not import director), so they have drifted: the scheduled-boss "+
			"broadcast now reaches every live zone INSTANCE, spawning the boss and its full loot table in "+
			"every private copy at once, and each kill reschedules the one shared world timer", SpawnBossEvent)
	}

	// The CONTROL: the list is deliberately NARROW — only engine-reserved events are withheld. A
	// content-authored world event must still reach instances (the engine cannot know an author's intent;
	// mud.zone() is how content expresses that choice for itself). If this ever starts returning true, the
	// exclusion has been widened into a blanket mute and content-authored world events are silently inert in
	// every instance.
	if world.ReservedScheduleEvent("gate_opened") {
		t.Fatal("a CONTENT-authored world event is being withheld from instances; the reserved list has been " +
			"widened into a blanket mute, which makes authored world events silently inert in every copy")
	}
	// BossDiedEvent is the UP direction and is not withheld by this list at all — an instance's signal-up is
	// refused at the source (luascope.go denyInInstance), which is a different and stronger mechanism.
	if world.ReservedScheduleEvent(BossDiedEvent) {
		t.Fatalf("%q is in the DOWN-direction reserved list; the up direction is refused at the source instead, "+
			"and conflating the two hides which mechanism is actually protecting the shared timer", BossDiedEvent)
	}
}
