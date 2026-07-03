package world

import (
	"strings"
	"testing"
)

// visibility_test.go — #28: the canSee/nameFor chokepoint now honors the invisibility / detect-invis /
// holylight flags, and the two former phase5-visibility disclosure paths (lookRoom + whoLocal) route
// through it. A builder with holylight sees everything.

func clearOut(s *session) {
	for len(s.out) > 0 {
		<-s.out
	}
}

func TestCanSeeInvisibilityHolylightDetect(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	ghost := harmPlayer(z, room, "Ghost")

	if !z.canSee(viewer, ghost) {
		t.Fatal("baseline: an ordinary viewer should see an unconcealed entity")
	}
	setFlag(ghost, flagInvisible, true)
	if z.canSee(viewer, ghost) {
		t.Fatal("an invisible target must be hidden from an ordinary viewer")
	}
	if !z.canSee(ghost, ghost) {
		t.Fatal("you always see yourself, even while invisible")
	}
	setFlag(viewer, flagDetectInvis, true)
	if !z.canSee(viewer, ghost) {
		t.Fatal("detect_invis must pierce invisibility")
	}
	setFlag(viewer, flagDetectInvis, false)
	setFlag(viewer, flagHolylight, true)
	if !z.canSee(viewer, ghost) {
		t.Fatal("holylight must see everything, including an invisible target")
	}
}

// TestNameForHidesInvisibleAsSomeone pins the act()/messaging leak guard: an invisible referent renders as
// "Someone" to an ordinary viewer (never its real name), and as its real name to a holylight viewer.
func TestNameForHidesInvisibleAsSomeone(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	ghost := harmPlayer(z, room, "Ghost")
	setFlag(ghost, flagInvisible, true)

	if got := z.nameFor(viewer, ghost, false); got != "Someone" {
		t.Fatalf("nameFor an invisible entity = %q, want Someone (the leak guard)", got)
	}
	setFlag(viewer, flagHolylight, true)
	if got := z.nameFor(viewer, ghost, false); got != ghost.Name() {
		t.Fatalf("holylight nameFor = %q, want the real name %q", got, ghost.Name())
	}
}

func TestLookRoomOmitsInvisibleOccupant(t *testing.T) {
	z, _, room := harmZone(t)
	_ = harmPlayer(z, room, "Viewer")
	ghost := harmPlayer(z, room, "Ghost")
	vs := z.players["Viewer"]
	setFlag(ghost, flagInvisible, true)

	clearOut(vs) // discard arrival broadcasts so only the look render is inspected
	z.lookRoom(vs)
	if out := drainText(t, vs.out); strings.Contains(out, "Ghost") {
		t.Fatalf("an invisible occupant leaked into lookRoom: %q", out)
	}

	setFlag(vs.entity, flagHolylight, true)
	clearOut(vs)
	z.lookRoom(vs)
	if out := drainText(t, vs.out); !strings.Contains(out, "Ghost") {
		t.Fatal("a holylight viewer should see the invisible occupant in the room")
	}
}

// TestResolveInvisibleIndistinguishableFromAbsent pins the security property doing the heavy lifting for
// look/consider/kill <invis>: an invisible target resolves to NOTHING — identically to a nonexistent
// keyword — so the failure gives no oracle that "something invisible is here". Holylight resolves it.
func TestResolveInvisibleIndistinguishableFromAbsent(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	ghost := harmMob(z, room, "ghost")
	ghost.setKeywords([]string{"ghost"})

	if hits := z.Resolve(viewer, parseTargetSpec("ghost"), ScopeRoomLiving); len(hits) != 1 {
		t.Fatalf("baseline: expected to resolve the ghost, got %d", len(hits))
	}
	setFlag(ghost, flagInvisible, true)
	invis := z.Resolve(viewer, parseTargetSpec("ghost"), ScopeRoomLiving)
	absent := z.Resolve(viewer, parseTargetSpec("nobodyhere"), ScopeRoomLiving)
	if len(invis) != 0 {
		t.Fatalf("an invisible target must not resolve for an ordinary viewer, got %d", len(invis))
	}
	if len(absent) != len(invis) {
		t.Fatalf("invisible (%d) and absent (%d) targets must resolve identically (no oracle)", len(invis), len(absent))
	}
	setFlag(viewer, flagHolylight, true)
	if hits := z.Resolve(viewer, parseTargetSpec("ghost"), ScopeRoomLiving); len(hits) != 1 {
		t.Fatalf("holylight viewer should resolve the invisible ghost, got %d", len(hits))
	}
}

func TestWhoLocalOmitsInvisiblePlayer(t *testing.T) {
	z, _, room := harmZone(t)
	viewer := harmPlayer(z, room, "Viewer")
	ghost := harmPlayer(z, room, "Ghost")
	setFlag(ghost, flagInvisible, true)

	if out := z.whoLocal(viewer); strings.Contains(out, "Ghost") {
		t.Fatalf("an invisible player is listed in who: %q", out)
	}
	if out := z.whoLocal(ghost); !strings.Contains(out, "Ghost") {
		t.Fatal("a player should see themselves in who even while invisible")
	}
	setFlag(viewer, flagHolylight, true)
	if out := z.whoLocal(viewer); !strings.Contains(out, "Ghost") {
		t.Fatal("a holylight viewer should see an invisible player in who")
	}
}
