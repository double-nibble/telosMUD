package world

import (
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// drainAllText reads every currently-queued frame (non-blocking) and returns their output markup joined by
// newlines — used when a test issues more than one command so a lingering prompt frame from the previous
// dispatch doesn't get mistaken for the next command's output.
func drainAllText(out chan *playv1.ServerFrame) string {
	var b strings.Builder
	for {
		select {
		case f := <-out:
			if o := f.GetOutput(); o != nil {
				b.WriteString(o.GetMarkup())
				b.WriteByte('\n')
			}
		default:
			return b.String()
		}
	}
}

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

// TestInventorySurface: cmdInventory renders a pack `inventory` template when present (and the template can read
// the viewer's carried items via self:contents()), else the built-in coalesced listing, and fails closed to the
// built-in on a broken (non-string) template.
func TestInventorySurface(t *testing.T) {
	t.Run("template binds self:contents()", func(t *testing.T) {
		z := newZone("inv")
		z.defBundle().displayDefs["inventory"] = `
			local s = ui.sheet()
			for _, item in ipairs(self:contents()) do
				s:row({item:name()})
			end
			return s:render()`
		s := scorePlayer(z, "Bilbo")
		addTestItem(z, s.entity, "a magic ring", []string{"ring"})
		z.dispatch(s, "inventory")
		if out := drainText(t, s.out); !strings.Contains(out, "a magic ring") {
			t.Fatalf("inventory template did not see the self:contents() item: %q", out)
		}
	})
	t.Run("fallback", func(t *testing.T) {
		z := newZone("inv")
		s := scorePlayer(z, "Bilbo")
		z.dispatch(s, "inventory")
		if out := drainText(t, s.out); !strings.Contains(out, "You are carrying") {
			t.Fatalf("no-template inventory should use the built-in listing: %q", out)
		}
	})
	t.Run("non-string template falls back", func(t *testing.T) {
		z := newZone("inv")
		z.defBundle().displayDefs["inventory"] = `return 42`
		s := scorePlayer(z, "Bilbo")
		z.dispatch(s, "inventory")
		if out := drainText(t, s.out); !strings.Contains(out, "You are carrying") {
			t.Fatalf("a non-string inventory template should fall back to the built-in: %q", out)
		}
	})
}

// TestDisplaySurfaceIsolation pins that a template for one surface does NOT affect another (guards the
// "wrong surface key" bug class): defining `inventory` leaves `equipment` on its built-in listing.
func TestDisplaySurfaceIsolation(t *testing.T) {
	z := newZone("iso")
	z.defBundle().displayDefs["inventory"] = `return "CUSTOM-INV"`
	s := scorePlayer(z, "Frodo")

	z.dispatch(s, "inventory")
	if out := drainAllText(s.out); !strings.Contains(out, "CUSTOM-INV") {
		t.Fatalf("inventory template not applied: %q", out)
	}
	z.dispatch(s, "equipment")
	if out := drainAllText(s.out); !strings.Contains(out, "You are using") {
		t.Fatalf("equipment must stay on its built-in when only inventory is templated: %q", out)
	}
}

// TestEquipmentSurface: cmdEquipment renders a pack `equipment` template when present, else the built-in
// by-slot listing.
func TestEquipmentSurface(t *testing.T) {
	t.Run("template", func(t *testing.T) {
		z := newZone("eq")
		z.defBundle().displayDefs["equipment"] = `
			local s = ui.sheet()
			s:banner("GEAR", "=")
			return s:render()`
		s := scorePlayer(z, "Gimli")
		z.dispatch(s, "equipment")
		if out := drainText(t, s.out); !strings.Contains(out, "GEAR") {
			t.Fatalf("equipment template not rendered: %q", out)
		}
	})
	t.Run("fallback", func(t *testing.T) {
		z := newZone("eq")
		s := scorePlayer(z, "Gimli")
		z.dispatch(s, "equipment")
		if out := drainText(t, s.out); !strings.Contains(out, "You are using") {
			t.Fatalf("no-template equipment should use the built-in listing: %q", out)
		}
	})
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
