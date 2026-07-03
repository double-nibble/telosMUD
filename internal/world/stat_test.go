package world

import (
	"strings"
	"testing"
)

// stat_test.go — #29 Slice 1: the staff inspection command `stat` and its rank-based dispatch gate (rebuilt
// on the content trust ladder). Covers (a) the USE gate: a player (rank 0) can neither see nor run `stat`,
// staff can; (b) the TARGET gate: staff may not inspect a target that outranks them; and (c) the sheet.

// TestStatGateHidesFromMortal pins the wiz-command posture: a player (baseline rank 0) who types a staff
// verb gets the unknown-verb response ("Huh?"), never the command's output nor a "you can't" that would
// leak its existence. A staff member (a positive-rank tier) runs it and sees the sheet.
func TestStatGateHidesFromMortal(t *testing.T) {
	z, mortal := abilityTestZone(t) // no tier => rank 0 => mortal

	z.dispatch(mortal, "stat me")
	if !drainContains(t, mortal, "Huh?") {
		t.Fatal("a player typing `stat` must get the unknown-verb response (the command is invisible)")
	}

	builder := makeRoomPlayer(z, "Buildy")
	builder.tier = tierBuilder // rank 20 (default ladder) => staff
	z.dispatch(builder, "stat me")
	if !drainContains(t, builder, "rid #") {
		t.Fatal("a staff member typing `stat me` must get the inspection sheet")
	}
}

// TestStatTargetGate: the TARGET rank comparison — a builder may stat a plain player (baseline) but NOT an
// admin (higher rank). Self is always allowed.
func TestStatTargetGate(t *testing.T) {
	z, builder := abilityTestZone(t)
	builder.tier = tierBuilder // rank 20

	// A plain player target (no tier => rank 0): allowed.
	player := makePlayerTargetInRoom(z, builder.entity, "Pleb")
	_ = player
	z.dispatch(builder, "stat Pleb")
	if !drainContains(t, builder, "rid #") {
		t.Fatal("a builder must be able to stat a plain player (rank 0 <= builder)")
	}

	// An admin target (rank 40 > builder 20): refused with the tier message, no sheet.
	admin := makePlayerTargetInRoom(z, builder.entity, "Boss")
	admin.tier = tierAdmin
	z.dispatch(builder, "stat Boss")
	if !drainContains(t, builder, "higher trust tier") {
		t.Fatal("a builder must not be able to stat an admin (higher rank)")
	}
}

// TestStatRegisteredLowPriority: the staff verb `stat` is registered LAST, so its abbreviation priority is
// strictly lower (larger value) than every mortal verb — it can never win an abbreviation collision.
func TestStatRegisteredLowPriority(t *testing.T) {
	stat, ok := baseTable.resolve("stat")
	if !ok || stat.Name != "stat" {
		t.Fatal("stat must be registered in the base table")
	}
	if stat.MinRank <= 0 {
		t.Fatal("stat must carry a positive MinRank (staff gate)")
	}
	for _, mortal := range []string{"north", "say", "score", "who", "look"} {
		m, ok := baseTable.resolve(mortal)
		if !ok {
			t.Fatalf("mortal verb %q missing from base table", mortal)
		}
		if stat.priority <= m.priority {
			t.Errorf("stat priority (%d) must be lower than mortal %q (%d) so it never shadows",
				stat.priority, mortal, m.priority)
		}
	}
}

// TestStatSheetLiving checks the Living body: the prototype key, the Living component, the vitals line,
// and the serialized-state block are all present.
func TestStatSheetLiving(t *testing.T) {
	z, actor := abilityTestZone(t)
	mob := makeMobTarget(z, actor.entity, "goblin")
	setFlag(mob, "aggressive", true)

	sheet := statSheet(mob)
	for _, want := range []string{"goblin", "proto: test:mob", "Living", "aggressive", "position: standing", "vitals: hp", "state:"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("stat sheet missing %q\n---\n%s", want, sheet)
		}
	}
}

// TestStatFlagsMarksReserved: statFlags shows reserved trust flags (which dumpFlags hides) and marks
// them with a trailing "*", so a builder sees the true elevation state.
func TestStatFlagsMarksReserved(t *testing.T) {
	_, actor := abilityTestZone(t)
	e := actor.entity
	setFlag(e, "pvp", true)
	setFlag(e, flagHolylight, true)

	got := statFlags(e)
	if !strings.Contains(got, "holylight*") {
		t.Errorf("reserved flag holylight must be shown and marked with *, got %q", got)
	}
	if !strings.Contains(got, "pvp") || strings.Contains(got, "pvp*") {
		t.Errorf("a normal flag must be shown UNmarked, got %q", got)
	}
}

// TestStatRoom: a bare `stat` inspects the room, surfacing its exits and authored room flags — routing
// internals a player never sees.
func TestStatRoom(t *testing.T) {
	_, actor := abilityTestZone(t)
	room := actor.entity.location
	room.room.exits = map[string]ProtoRef{"north": "test:room2"}
	room.room.namedFlags = map[string]bool{"safe": true}

	sheet := statSheet(room)
	for _, want := range []string{"exits: north->test:room2", "room-flags: safe"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("room stat sheet missing %q\n---\n%s", want, sheet)
		}
	}
}

// TestStatNoTarget: `stat <nonexistent>` reports a miss instead of dumping anything.
func TestStatNoTarget(t *testing.T) {
	z, actor := abilityTestZone(t)
	actor.tier = tierBuilder // staff, so the verb resolves
	z.dispatch(actor, "stat nonesuch")
	if !drainContains(t, actor, "nothing here by that name") {
		t.Fatal("stat of a missing target must report a miss")
	}
}
