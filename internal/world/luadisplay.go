package world

import (
	"fmt"
	"sort"

	"github.com/double-nibble/telosmud/internal/consoleui"
	lua "github.com/yuin/gopher-lua"
)

// luadisplay.go — the content DISPLAY-TEMPLATE render path (docs/REMAINING.md display-templating). A command
// that shows a sheet (score first, then who/inventory/…) asks the pack for a per-surface Lua render body via
// renderDisplaySheet; the body runs in the sandbox with `self` bound to the viewer's handle and returns the
// assembled string (built with the `ui` toolkit). Content owns the LAYOUT, labels, and stat order — a 5e vs
// WoW pack shows its own sheet without an engine change (the mechanism/flavor pillar). When a pack defines no
// template for a surface, the command falls back to a minimal built-in sheet so the verb always works.

// consultedDisplaySurfaces is the set of surfaces the engine ACTUALLY renders today. defineGlobals warns at
// load when a pack defines a template for a surface NOT in this set (a dead seam). Grow it as who/inventory/
// room/… are wired to their commands.
var consultedDisplaySurfaces = map[string]bool{
	"score": true,
}

// renderDisplaySheet runs the pack's Lua render body for `surface` with `self` bound to the viewing entity's
// handle, returning (sheet, true) on a string return, or ("", false) when the pack defines no template, the
// body fails to compile, errors, or returns a non-string (the caller then uses its built-in fallback). A clean
// ROOT invocation (a player-issued display command, not inside a cascade); the viewer is the invocation actor.
//
// Output sanitization: the returned sheet is content-authored but is a MULTI-LINE, pre-formatted block whose
// intended markup includes newlines, `{{TOKEN}}` color, and zero-width bidi controls (Cf). It is deliberately
// NOT run through textsan.CleanMarkup (which strips EVERY control rune, newlines included, and is for single-line
// free-text like say/tell). The correct layer is the telnet edge: Write's sanitizeOutput strips raw Cc controls
// EXCEPT CR/LF while preserving the Cf bidi controls, and renderColor then turns the color tokens into SGR — so
// a raw ESC a template embeds is stripped at the edge (proven by TestScoreSheetE2E rendering color, not markup).
func (z *Zone) renderDisplaySheet(surface string, self *Entity) (string, bool) {
	if z == nil || z.lua == nil || self == nil {
		return "", false
	}
	body := z.displayDef(surface)
	if body == "" {
		return "", false
	}
	rt := z.lua
	ch := rt.chunkFor("display:"+surface, body)
	if ch == nil {
		return "", false // no body / compile failed (inert)
	}
	binds := map[string]lua.LValue{"self": rt.newHandle(self)}
	return rt.invokeForString(ch, &luaInvocation{actor: self}, binds)
}

// defaultScoreSheet is the built-in fallback shown when a pack defines no `score` template: a minimal,
// content-agnostic sheet (name banner, level, and every resource the entity carries) assembled with the same
// layout engine content templates use — so `score` is a working verb for ANY pack, and a content template
// simply overrides it. Resources render in sorted-ref order for determinism.
func (z *Zone) defaultScoreSheet(e *Entity) string {
	s := consoleui.New().MaxWidth(defaultTermWidth)
	s.Banner(e.Name(), "=")
	s.Row([]string{"Level", fmt.Sprintf("%d", int(e.Attr("level")))}, consoleui.Left, consoleui.Right)

	var refs []string
	if b := z.defBundle(); b != nil && b.res != nil {
		for ref := range b.res.table() {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if rmax := e.ResourceMax(ref); rmax > 0 {
			s.Row([]string{ref, fmt.Sprintf("%d/%d", e.Resource(ref), rmax)}, consoleui.Left, consoleui.Right)
		}
	}
	s.Divider("=")
	return s.Render()
}
