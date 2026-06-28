package gate

import (
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/directory"
)

// look_render_journey_test.go is the BLACK-BOX (through the gate, player-visible) regression for
// the lookRoom render gap (commit 98b69a6) — Wave 1.
//
// THE BUG: lookRoom rendered ONLY other players ("X is here") and silently skipped mobs, ground
// items, and corpses — so a goblin you could `kill` never appeared in the room description, and a
// dropped item / corpse never showed. A player at a telnet prompt looked and saw an EMPTY-looking
// room that actually held a mob they could attack.
//
// look_render_test.go (internal/world) pins this at the model level (lookRoom directly). THIS test
// pins it through the GATE, exactly as a human player experiences it: a real scripted client logs
// in, walks the demo world, and `look`s — and must see EVERY entity type's presence line in one
// look: a MOB's long-line, a GROUND ITEM's long-line, and ANOTHER PLAYER's "is here". A revert of
// the render fix makes one of these vanish and fails the corresponding case.
//
// Two single-shard scenarios cover the three content kinds against the demo pack's own placed
// content (no combat, no drop mechanics — deterministic):
//   - midgaard market: ground ITEMS (the reset places a longsword + helmet on the floor) + a second
//     PLAYER standing there.
//   - darkwood hollow: the goblin MOB (the reset spawns it) + a second PLAYER standing there.
func TestLookRendersAllRoomContents(t *testing.T) {
	cases := []struct {
		name string
		zone string // the single demo zone to host (its start room is where players spawn)
		// walk is the sequence of moves the two players take from the start room to the room under
		// test (empty = look right where they spawn).
		walk []string
		// roomName is a substring of the destination room's name, polled to confirm both players
		// arrived before the look assertion runs.
		roomName string
		// want are the presence lines a look in that room MUST render — a mix of mob long-lines,
		// ground-item long-lines, and the other player's "is here" presence.
		want []string
	}{
		{
			name:     "market_ground_items_and_player",
			zone:     "midgaard", // start_room: midgaard:room:temple
			walk:     []string{"north"},
			roomName: "Market Square",
			want: []string{
				"A steel longsword lies here.", // ground item (mob-less): the lookRoom item case
				"An iron helmet rests here.",   // a second ground item, to prove it is not a one-off
				"Bystander is here.",           // the other player's presence (the always-worked case)
			},
		},
		{
			name:     "hollow_mob_and_player",
			zone:     "darkwood", // start_room: darkwood:room:grove
			walk:     []string{"north"},
			roomName: "Dark Hollow",
			want: []string{
				"A wiry goblin bares its teeth, clutching a rusty knife.", // the MOB long-line: the headline render gap
				"Bystander is here.", // the other player's presence
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const addr = "addr-a"
			h := newHarness(t)
			// A single-zone shard hosting just this scenario's zone; Static routes every login to it,
			// so both players land in the SAME zone goroutine and share its rooms.
			h.addShard(tc.zone, addr, nil, nil)
			h.serveGate(directory.Static{Addr: addr})

			// The "bystander" logs in first and walks to the room under test, so they are standing
			// there (a visible occupant) when the looker arrives and looks.
			bystander := h.dial(t)
			bystander.login(t, "Bystander")
			for _, dir := range tc.walk {
				bystander.send(t, dir)
			}
			bystander.expect(t, tc.roomName) // confirm the bystander reached the room.

			// The looker logs in and walks to the same room.
			looker := h.dial(t)
			looker.login(t, "Looker")
			for _, dir := range tc.walk {
				looker.send(t, dir)
			}
			looker.expect(t, tc.roomName)

			// A fresh, EXPLICIT look. The move already rendered the room once, so we scope the
			// assertion to THIS look by first emitting a unique marker the looker can see, then
			// asserting each presence line appears AFTER it — guaranteeing we read the look's output,
			// not an earlier render. (The terminal accumulator is append-only; the marker bounds it.)
			looker.send(t, "say render-check")
			looker.expect(t, "You say, 'render-check'")
			looker.send(t, "look")

			for _, want := range tc.want {
				// expect polls with a deadline (no sleep). A revert of the lookRoom fix drops the mob /
				// ground-item line, so this times out and fails with the accumulated transcript.
				looker.expect(t, want)
			}

			// The looker's OWN presence must never render (lookRoom skips self).
			if got := looker.acc.String(); strings.Contains(got, "Looker is here.") {
				t.Fatalf("look rendered the looker's own presence; output:\n%s", got)
			}

			looker.close(t)
			bystander.close(t)
		})
	}
}
