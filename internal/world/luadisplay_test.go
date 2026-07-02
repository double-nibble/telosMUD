package world

import (
	"strings"
	"testing"
)

// luadisplay_test.go — gates for the content display-template path (luadisplay.go): the `score` command renders
// a pack-defined template when present, else the built-in fallback sheet.

// scorePlayer builds a player in a room REGISTERED in z.rooms, so a self handle re-resolves (entityByRID walks
// z.rooms) — the state a logged-in player always has in-game.
func scorePlayer(z *Zone, name string) *session {
	s := makeRoomPlayer(z, name)
	z.rooms[ProtoRef("test:room")] = s.entity.location
	return s
}

// TestScoreFallbackSheet: a bare zone (no content) still gives `score` a working built-in sheet (name + level).
func TestScoreFallbackSheet(t *testing.T) {
	z := newZone("score")
	s := scorePlayer(z, "Hero")
	z.dispatch(s, "score")
	out := drainText(t, s.out)
	if !strings.Contains(out, "Hero") {
		t.Fatalf("fallback score sheet missing the player name: %q", out)
	}
	if !strings.Contains(out, "Level") {
		t.Fatalf("fallback score sheet missing the Level line: %q", out)
	}
}

// TestScoreContentTemplate: a pack-defined `score` template overrides the fallback, renders through the `ui`
// toolkit, and binds `self` to the viewer.
func TestScoreContentTemplate(t *testing.T) {
	z := newZone("score")
	z.defBundle().displayDefs["score"] = `
		local s = ui.sheet()
		s:banner("CHARACTER", "=")
		s:row({"Name", self:name()}, {"left", "right"})
		return s:render()`
	s := scorePlayer(z, "Tara")
	z.dispatch(s, "score")
	out := drainText(t, s.out)
	if !strings.Contains(out, "CHARACTER") {
		t.Fatalf("content template not rendered (no banner): %q", out)
	}
	if !strings.Contains(out, "Tara") {
		t.Fatalf("content template did not bind self:name(): %q", out)
	}
}

// TestScoreTemplateErrorFallsBack: a broken template (returns a non-string) fails closed to the built-in sheet
// rather than sending garbage or nothing.
func TestScoreTemplateErrorFallsBack(t *testing.T) {
	z := newZone("score")
	z.defBundle().displayDefs["score"] = `return 42` // not a string
	s := scorePlayer(z, "Broken")
	z.dispatch(s, "score")
	out := drainText(t, s.out)
	if !strings.Contains(out, "Broken") { // fell back to the built-in name banner
		t.Fatalf("a non-string template should fall back to the built-in sheet: %q", out)
	}
}
