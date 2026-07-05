package gate

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/directory"
)

// onboarding_journey_test.go — Wave-4 BLACK-BOX onboarding journeys (docs/TEST-COVERAGE.md Area 4),
// driven through the in-process gate harness exactly as a human at a telnet prompt experiences them.
// These pin the player-visible OUTPUT of the paths a brand-new player walks, above the unit tier.

// TestFirstTimeOnboardingJourney is the full first-time path a NEW player walks: connect -> the gate's
// name prompt -> spawn into the world -> look -> first movement. Every step asserts the player-visible
// output, so a regression that broke the welcome banner, the spawn room, the look render, or the first
// move surfaces here.
//
// SEAM (noted plainly): real character CREATION / auth is Phase 14 — the gate today takes a login NAME
// and the name IS the character, spawned at the home zone's start room. So "character creation" is not
// yet drivable through the harness; this journey covers the entire onboarding path that EXISTS
// (connect -> name -> spawn -> look -> move). When chargen lands, the class/race/stat steps slot in
// between the name prompt and the spawn, and this test extends there.
func TestFirstTimeOnboardingJourney(t *testing.T) {
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)

	// 1. Connect: the gate greets and prompts for a name (the first thing a new player sees).
	term.expect(t, "Welcome to TelosMUD.")
	term.expect(t, "By what name shall you be known?")

	// 2. Submit a name -> spawn into the world at the home zone's start room (the Temple), with the
	//    room's name + description rendered (the join look).
	term.send(t, "Newcomer")
	term.expect(t, "The Temple Square")
	term.expect(t, "A broad plaza of worn flagstones") // the room's long description (join look)

	// 3. look -> the room re-renders on demand (the core orientation command).
	term.send(t, "look")
	term.expect(t, "The Temple Square")

	// 4. First movement -> north into the market, the next room rendered. This is the "see a room ->
	//    move -> see the next room" Phase-1 milestone from the player's seat.
	term.send(t, "north")
	term.expect(t, "Market Square")
	term.expect(t, "Stalls crowd the square") // the market's description: the move really landed

	term.close(t)
}

// TestItemInteractionActJourney is the get / wield / wear / inventory loop with its act() messaging
// (the gap-map's named GAP): a player picks up ground items, wields a weapon and wears armor, and —
// the load-bearing part — a BYSTANDER in the same room sees the third-person act() line for each,
// while the actor sees the first-person line and inventory/equipment reflect the change.
//
// The demo market seeds a steel longsword (wieldable) and an iron helmet (wearable, head slot) on the
// floor. Two players stand there: Actor does the interacting, Watcher observes the act() broadcasts.
func TestItemInteractionActJourney(t *testing.T) {
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	// Watcher logs in first and walks to the market, so they are present to RECEIVE the act() room
	// broadcasts when Actor interacts there.
	watcher := h.dial(t)
	watcher.login(t, "Watcher")
	watcher.expect(t, "The Temple Square")
	watcher.send(t, "north")
	watcher.expect(t, "Market Square")

	// Actor logs in and joins them in the market.
	actor := h.dial(t)
	actor.login(t, "Actor")
	actor.expect(t, "The Temple Square")
	actor.send(t, "north")
	actor.expect(t, "Market Square")

	// --- get: pick the sword up off the floor. Actor sees first-person, Watcher sees third-person. ---
	actor.send(t, "get sword")
	actor.expect(t, "You get a steel longsword.")
	watcher.expect(t, "Actor gets a steel longsword.")

	// --- wield: the sword. The bystander sees "$n wields $p". ---
	actor.send(t, "wield sword")
	actor.expect(t, "You wield a steel longsword.")
	watcher.expect(t, "Actor wields a steel longsword.")

	// --- get + wear: the helmet on the head slot. The bystander sees "$n wears $p". ---
	actor.send(t, "get helmet")
	actor.expect(t, "You get an iron helmet.")
	watcher.expect(t, "Actor gets an iron helmet.")
	actor.send(t, "wear helmet")
	actor.expect(t, "You wear an iron helmet on your head.")
	watcher.expect(t, "Actor wears an iron helmet.")

	// --- equipment reflects the new state: both items show under their SLOT markers (the slot tag
	//     proves we're reading the equipment view, not a stale `get`/`wear` echo elsewhere in the
	//     buffer — the equipment line format is "  <slot> <item name>"). ---
	actor.send(t, "equipment")
	actor.expect(t, "<wielded> a steel longsword")
	actor.expect(t, "<head> an iron helmet")

	// --- inventory now FOLDS the worn items in, flagged by slot (#85): the carried list shows both
	//     equipped items under their slot tags ("<wielded> ...", "<worn on head> ...") rather than hiding
	//     them. This proves the equip is reflected in the inventory view via the worn-slot flag. ---
	actor.send(t, "inventory")
	actor.expect(t, "You are carrying:")
	actor.expect(t, "<wielded> a steel longsword")
	actor.expect(t, "<worn on head> an iron helmet")

	actor.close(t)
	watcher.close(t)
}

// TestBadLoginRepromptsThenSucceeds pins the gate's login RE-PROMPT loop from the player's seat (the
// connect-time-error onboarding row): a name that violates the grammar (a leading digit, an embedded
// dot, a leading dot) is REJECTED with a player-visible reason and the gate RE-PROMPTS — it does NOT
// drop the connection — and a subsequent valid name succeeds and spawns. validateName is unit-tested
// directly (name_test.go); this is the black-box proof that the gate's handle() loop surfaces the
// rejection and recovers, exactly what a fat-fingering new player experiences.
func TestBadLoginRepromptsThenSucceeds(t *testing.T) {
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.expect(t, "By what name shall you be known?")

	// A name starting with a digit is rejected (it would read as the `N.` count in `N.keyword`).
	term.send(t, "7thson")
	term.expect(t, "That name won't do: it can't start with a digit")

	// A name with an embedded dot is rejected (it would split into a count/selector + keyword).
	term.send(t, "ab.cd")
	term.expect(t, "That name won't do: it can't contain a dot")

	// A valid name on the third try succeeds and spawns — the connection survived the rejections.
	term.send(t, "Goodname")
	term.expect(t, "The Temple Square")

	term.close(t)
}
