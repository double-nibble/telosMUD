package world

import "testing"

// look_render_test.go pins the room-contents rendering of `look` (lookRoom, commands.go). The original
// implementation rendered ONLY other players ("X is here") and silently skipped mobs, ground items, and
// corpses — so a goblin you could `kill` never appeared in the room description, and a corpse never
// showed. These tests assert every visible occupant renders: a mob's/item's/corpse's `long` line, and a
// player's "is here" presence, with a short-name fallback when no long is authored.

func TestLookRoomRendersMobsItemsAndPlayers(t *testing.T) {
	z := newZone("test")
	z.id = "test"
	s := makeRoomPlayer(z, "Looker")

	// A mob (Living, NOT PlayerControlled) with a room-presence long line — the goblin case.
	goblin := makeMobTarget(z, s.entity, "a small goblin")
	goblin.setLong("A wiry goblin bares its teeth.")

	// A ground item (no Living, no PlayerControlled) with a ground long line.
	knife := z.newEntity(ProtoRef("test:obj:knife"))
	knife.setShort("a rusty knife")
	knife.setLong("A rusty, notched knife lies here.")
	Move(knife, s.entity.location)

	// A long-less occupant (e.g. an engine artifact without an authored long) → short-name fallback.
	thing := z.newEntity(ProtoRef("test:obj:thing"))
	thing.setShort("the corpse of a small goblin")
	Move(thing, s.entity.location)

	// Another player still renders the "X is here" presence form (unchanged behavior).
	makePlayerTargetInRoom(z, s.entity, "Bystander")

	z.lookRoom(s)
	lines := drainCombat(s)

	for _, want := range []string{
		"A wiry goblin bares its teeth.",        // mob long
		"A rusty, notched knife lies here.",     // ground item long
		"the corpse of a small goblin is here.", // long-less fallback (corpse)
		"Bystander is here.",                    // player presence
	} {
		if !contains(lines, want) {
			t.Fatalf("look did not render %q; got:\n%v", want, lines)
		}
	}

	// The looker itself is never listed.
	if contains(lines, "Looker is here.") {
		t.Fatalf("look rendered the looker's own presence; got:\n%v", lines)
	}
}

// TestLookRoomColorsExitsCyan pins the engine auto-color (Track 1): the exits line emits the `{{FG_CYAN}}`
// color markup (the gate renders it to ANSI SGR, or strips it for `color off` — tested in internal/telnet).
// The world's job is to emit the token, so this asserts the markup, not the rendered escape.
func TestLookRoomColorsExitsCyan(t *testing.T) {
	z := newZone("test")
	s := makeRoomPlayer(z, "Looker")
	s.entity.location.room.exits = map[string]ProtoRef{"north": "test:room:n", "east": "test:room:e"}

	z.lookRoom(s)
	lines := drainCombat(s)

	// sortedExits orders by canonical direction (n/s/e/w/…) → "north, east", wrapped in the cyan token + reset.
	if want := "Exits: {{FG_CYAN}}north, east{{RESET}}"; !contains(lines, want) {
		t.Fatalf("lookRoom did not color exits cyan; want %q in:\n%v", want, lines)
	}
}
