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
// load when a pack defines a template for a surface NOT in this set (a dead seam). Grow it as more commands
// are wired.
//
// Two binding shapes back these surfaces:
//   - render(self) — a self-subject sheet (renderDisplaySheet). The subject's handle is the whole input.
//   - render(self, list) — a COLLECTION sheet (renderDisplayList). The viewer plus a list of plain-data
//     records, for a surface whose subject is a set of things that are not live entities of this zone.
var consultedDisplaySurfaces = map[string]bool{
	"score":     true, // cmdScore     (render(self))
	"inventory": true, // cmdInventory (render(self) via self:contents())
	"equipment": true, // cmdEquipment (render(self) via self:equipment()/self:equipment_slots())
	"room":      true, // lookRoom     (render(self) via self:room():exits()/:occupants()/:room_items())
	"who":       true, // cmdWho       (render(self, list) — the cross-shard roster, see renderDisplayList)
}

// renderDisplaySheet runs the pack's Lua render body for `surface` with `self` bound to the viewing entity's
// handle, returning (sheet, true) on a string return, or ("", false) when the pack defines no template, the
// body fails to compile, errors, or returns a non-string (the caller then uses its built-in fallback). A clean
// ROOT invocation (a player-issued display command, not inside a cascade); the viewer is the invocation actor.
//
// Output sanitization: the returned sheet is content-authored but is a MULTI-LINE, pre-formatted block whose
// intended markup includes newlines, `{{TOKEN}}` color, and consoleui's zero-width bidi ISOLATE/MARK controls
// (FSI…PDI + LRM, for RTL column stability). It is deliberately NOT run through textsan.CleanMarkup (which
// strips EVERY control rune, newlines included, AND the whole bidi subset — it's for single-line free-text like
// say/tell). The correct layer is the telnet edge: Write's sanitizeOutput strips raw Cc controls EXCEPT CR/LF
// and the STRONG bidi OVERRIDE block (U+202A–U+202E, isStrongBidiOverride) while PRESERVING consoleui's balanced
// isolates + LRM (#25b narrowed the edge strip to overrides only, so the sheet's own grid survives); renderColor
// then turns the color tokens into SGR — so a raw ESC or a smuggled override a template embeds is dropped at the
// edge (color render proven by TestScoreSheetE2E; isolate survival by TestConsoleUISheetKeepsIsolatesThroughEdge).
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

// displayRecord is ONE plain-data row bound into a collection template's `list`. It is deliberately NOT a
// handle: a collection surface's rows may describe things that are not live entities of this zone (a `who`
// roster row is a player hosted on ANOTHER shard — there is no *Entity here to hand out, and minting a handle
// for one would break the no-cross-zone-reach invariant, T7). Fields are pushed as a Lua record table.
//
// Only string/number/bool leaves: a record is a snapshot, so a template reading it can never reach back into
// live world state off the goroutine that owns it.
type displayRecord struct {
	fields map[string]lua.LValue
	order  []string // insertion order — irrelevant to Lua's record lookup, kept for deterministic tests
}

// newDisplayRecord builds an empty record.
func newDisplayRecord() *displayRecord {
	return &displayRecord{fields: map[string]lua.LValue{}}
}

// set adds a field to the record (last write wins).
func (r *displayRecord) set(key string, v lua.LValue) *displayRecord {
	if _, seen := r.fields[key]; !seen {
		r.order = append(r.order, key)
	}
	r.fields[key] = v
	return r
}

// str/boolean are the leaf setters, so callers never touch lua.LValue. (A numeric setter lands the day a
// collection surface carries a number — `who`'s roster rows are all strings/bools.)
func (r *displayRecord) str(key, v string) *displayRecord          { return r.set(key, lua.LString(v)) }
func (r *displayRecord) boolean(key string, v bool) *displayRecord { return r.set(key, lua.LBool(v)) }

// renderDisplayList is renderDisplaySheet's COLLECTION sibling: it runs the pack's Lua render body for
// `surface` with TWO binds — `self` (the viewer's handle) and `list` (an array of plain-data records) —
// returning (sheet, true) on a string return, or ("", false) when the pack defines no template, the body
// fails to compile, errors, or returns a non-string (the caller then uses its built-in fallback).
//
// SINGLE-WRITER (docs/ARCHITECTURE.md): the Lua VM is one-per-zone and zone-goroutine-owned, so this — like
// every other invoke — MUST be called ON the zone goroutine. A collection surface whose data is fetched by
// blocking I/O off-goroutine (`who` reads the presence roster) posts the fetched records BACK to the zone
// inbox and renders here, in the message handler. It never renders in the fetching goroutine.
//
// SECURITY: `entries` is the ALREADY-FILTERED set. Concealment/visibility must be applied by the caller
// BEFORE binding, never left to the template — a record that reaches Lua is a record the viewer may see.
//
// Output sanitization: see renderDisplaySheet (the same telnet-edge contract).
func (z *Zone) renderDisplayList(surface string, self *Entity, entries []*displayRecord) (string, bool) {
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
	binds := map[string]lua.LValue{
		"self": rt.newHandle(self),
		"list": rt.newRecordList(entries),
	}
	return rt.invokeForString(ch, &luaInvocation{actor: self}, binds)
}

// newRecordList materializes the records as a Lua array VALUE of record tables (1-based, in order). Unlike
// pushHandleList it returns the table rather than pushing it (it is used as a bind, not a return value). An
// empty input yields an empty table, so a template can always `for _, e in ipairs(list)` safely.
func (rt *luaRuntime) newRecordList(entries []*displayRecord) lua.LValue {
	t := rt.L.NewTable()
	n := 0
	for _, e := range entries {
		if e == nil {
			continue // never leave a hole: ipairs would stop early at a nil index
		}
		rec := rt.L.NewTable()
		for _, k := range e.order {
			rec.RawSetString(k, e.fields[k])
		}
		n++
		t.RawSetInt(n, rec)
	}
	return t
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
