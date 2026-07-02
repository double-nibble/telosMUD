package world

import (
	"strings"

	"github.com/double-nibble/telosmud/internal/consoleui"
	lua "github.com/yuin/gopher-lua"
)

// luaui.go — the `ui` sandbox module: a thin, safe Lua binding over internal/consoleui, the
// "Bootstrap for console output" layout engine (docs/REMAINING.md display-templating). Content
// authors build score/who/inventory sheets by describing rows/dividers/banners/spans; the engine
// does ALL the width math (display-width + color-token + RTL aware). This is PURE formatting — no
// world state, no harm surface — so it needs none of the invocation/breaker machinery; it only
// allocates and mutates a Go *consoleui.Sheet held in a userdata (zone-goroutine-owned, no lock).
//
// Two DoS guards keep an untrusted author from turning layout into an alloc bomb (T13, mirroring
// the capped string builtins): an explicit fixed width is CLAMPED (a divider/pad fills the width,
// so ui.sheet(2e9) would otherwise allocate GBs in one op), and render() rejects an over-cap
// result before returning it.

// luaSheetTypeName is the metatable registry key for the sheet userdata type.
const luaSheetTypeName = "telos.uisheet"

const (
	// defaultTermWidth backs ui.sheet("full") until the client's negotiated NAWS width is plumbed
	// into the render context (an edge follow-up). 80 is the classic terminal default.
	defaultTermWidth = 80

	// The four DoS bounds (T13). Layout amplifies input: a WIDTH fills dividers/spans and pads every
	// cell, and rows/columns multiply. None of these amplifiers is a per-VM-instruction step, so the
	// budget/deadline can't see them — an auto-width sheet of many large-cell rows was an OOM-class
	// transient allocation (the security-audit finding). Each bound caps a distinct multiplier so the
	// worst-case render allocation is small and predictable, ENFORCED at construction/add time (before
	// the allocation), not post-hoc:
	//   - maxSheetWidth caps every column AND the total width (consoleui.MaxWidth) — the width amplifier;
	//   - maxSheetElems caps the number of rows/spans/dividers/banners — the line-count multiplier;
	//   - maxSheetCells caps columns per row — the per-row multiplier;
	//   - maxSheetInputBytes caps the cumulative bytes of all cell/text/fill content — content size,
	//     color-token inflation, and column count all in one.
	maxSheetWidth      = 1024
	maxSheetElems      = 2048
	maxSheetCells      = 64
	maxSheetInputBytes = 1 << 20 // 1 MiB, matching the sandbox string cap
)

// luaSheet is the Go payload of a sheet userdata: the layout builder plus the running DoS accounting
// (element count + cumulative input bytes). Zone-goroutine-owned, so no lock.
type luaSheet struct {
	sheet      *consoleui.Sheet
	elems      int
	inputBytes int
}

// admit charges one element carrying `bytes` of content against the per-sheet caps, raising a clean
// script error (fail-closed) if either the element count or the cumulative input would overflow. Called
// by every builder BEFORE the content reaches consoleui, so a pathological sheet errors at add time
// rather than allocating at render time.
func (s *luaSheet) admit(l *lua.LState, bytes int) {
	if s.elems+1 > maxSheetElems {
		l.RaiseError("ui.sheet has too many elements (max %d)", maxSheetElems)
		return
	}
	if s.inputBytes+bytes > maxSheetInputBytes {
		l.RaiseError("ui.sheet content too large (cap %d bytes)", maxSheetInputBytes)
		return
	}
	s.elems++
	s.inputBytes += bytes
}

// sumBytes totals the byte lengths of the cells (the content-size charge for a row).
func sumBytes(cells []string) int {
	n := 0
	for _, c := range cells {
		n += len(c)
	}
	return n
}

// installUITable registers the sheet userdata type and exposes the read-only `ui` global with the
// sheet constructor. Called once at sandbox build, after the allowlist env is in place.
func (rt *luaRuntime) installUITable() {
	L := rt.L

	// The sheet userdata type + its pointer-safe __tostring (T15) and locked metatable.
	smt := L.NewTypeMetatable(luaSheetTypeName)
	L.SetField(smt, "__tostring", L.NewFunction(func(l *lua.LState) int {
		l.Push(lua.LString("<ui.sheet>"))
		return 1
	}))
	L.SetField(smt, "__metatable", lua.LString("locked"))
	L.SetField(smt, "__index", L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"row":     rt.sheetRow,
		"rows":    rt.sheetRows,
		"span":    rt.sheetSpan,
		"divider": rt.sheetDivider,
		"banner":  rt.sheetBanner,
		"render":  rt.sheetRender,
	}))

	ui := L.NewTable()
	L.SetFuncs(ui, map[string]lua.LGFunction{"sheet": rt.uiSheet})

	g := L.Get(lua.GlobalsIndex).(*lua.LTable)
	g.RawSetString("ui", rt.readOnly(ui))
}

// uiSheet is the `ui.sheet([width])` constructor. No arg (or a non-number/unknown string) => auto
// width; a number => a clamped fixed width; the string "full" => the client terminal width.
func (rt *luaRuntime) uiSheet(l *lua.LState) int {
	var sh *consoleui.Sheet
	switch v := l.Get(1).(type) {
	case lua.LNumber:
		sh = consoleui.NewFixed(clampWidth(int(v)))
	case lua.LString:
		if strings.EqualFold(strings.TrimSpace(string(v)), "full") {
			sh = consoleui.NewFixed(defaultTermWidth)
		} else {
			sh = consoleui.New()
		}
	default:
		sh = consoleui.New()
	}
	sh.MaxWidth(maxSheetWidth) // hard width ceiling (bounds AUTO width too — the alloc-bomb guard)
	ud := l.NewUserData()
	ud.Value = &luaSheet{sheet: sh}
	l.SetMetatable(ud, l.GetTypeMetatable(luaSheetTypeName))
	l.Push(ud)
	return 1
}

// sheetRow adds a columnar row: sheet:row(cells [, aligns]). cells and aligns are Lua arrays;
// aligns[i] is "left"/"right"/"center" (default left). Returns self for chaining.
func (rt *luaRuntime) sheetRow(l *lua.LState) int {
	s := checkSheet(l, 1)
	if s == nil {
		return 0
	}
	cells := s.rowCells(l, 2)
	s.admit(l, sumBytes(cells))
	s.sheet.Row(cells, luaAligns(l, 3)...)
	return returnSelf(l)
}

// rowCells reads the Lua array at stack index n into a bounded cell slice, raising if the row has more
// columns than maxSheetCells (checked via the O(1) length BEFORE materializing the slice, so a giant
// column count can't allocate first).
func (s *luaSheet) rowCells(l *lua.LState, n int) []string {
	t, ok := l.Get(n).(*lua.LTable)
	if !ok {
		return nil
	}
	if t.Len() > maxSheetCells {
		l.RaiseError("ui.sheet row has too many columns (max %d)", maxSheetCells)
		return nil
	}
	return tableToCells(l, t)
}

// sheetRows is the ergonomic bulk builder: sheet:rows(list, mapFn). For each item in the array
// `list`, mapFn(item) is called and must return a cells array; that becomes one left-aligned row.
// It keeps format-string plumbing out of the layout concern (the author writes it once, in Lua).
func (rt *luaRuntime) sheetRows(l *lua.LState) int {
	s := checkSheet(l, 1)
	if s == nil {
		return 0
	}
	list := l.CheckTable(2)
	fn := l.CheckFunction(3)
	n := list.Len()
	for i := 1; i <= n; i++ {
		l.Push(fn)
		l.Push(list.RawGetInt(i))
		l.Call(1, 1) // errors propagate to the entry-point pcall (fail-closed)
		ret := l.Get(-1)
		l.Pop(1)
		t, ok := ret.(*lua.LTable)
		if !ok {
			continue
		}
		if t.Len() > maxSheetCells {
			l.RaiseError("ui.sheet row has too many columns (max %d)", maxSheetCells)
			return 0
		}
		cells := tableToCells(l, t)
		s.admit(l, sumBytes(cells))
		s.sheet.Row(cells)
	}
	return returnSelf(l)
}

// sheetSpan adds a full-width line: sheet:span(text [, align]).
func (rt *luaRuntime) sheetSpan(l *lua.LState) int {
	s := checkSheet(l, 1)
	if s == nil {
		return 0
	}
	text := l.CheckString(2)
	s.admit(l, len(text))
	s.sheet.Span(text, luaAlign(l.OptString(3, "left")))
	return returnSelf(l)
}

// sheetDivider adds a full-width fill line: sheet:divider(fill).
func (rt *luaRuntime) sheetDivider(l *lua.LState) int {
	s := checkSheet(l, 1)
	if s == nil {
		return 0
	}
	fill := l.CheckString(2)
	s.admit(l, len(fill))
	s.sheet.Divider(fill)
	return returnSelf(l)
}

// sheetBanner adds a centered title with fill: sheet:banner(text [, fill]).
func (rt *luaRuntime) sheetBanner(l *lua.LState) int {
	s := checkSheet(l, 1)
	if s == nil {
		return 0
	}
	text := l.CheckString(2)
	fill := l.OptString(3, "")
	s.admit(l, len(text)+len(fill))
	s.sheet.Banner(text, fill)
	return returnSelf(l)
}

// sheetRender assembles the sheet to a string. It rejects an over-cap result before returning it
// (T13) so a pathological template can't hand a giant string back into the VM.
func (rt *luaRuntime) sheetRender(l *lua.LState) int {
	s := checkSheet(l, 1)
	if s == nil {
		l.Push(lua.LString(""))
		return 1
	}
	out := s.sheet.Render()
	if len(out) > luaStrByteCap {
		l.RaiseError("ui.sheet render too large (cap %d bytes)", luaStrByteCap)
		return 0
	}
	l.Push(lua.LString(out))
	return 1
}

// --- helpers ------------------------------------------------------------------------------------

// returnSelf pushes the receiver (stack index 1) back so builder calls chain: s:row(..):divider(..).
func returnSelf(l *lua.LState) int {
	l.Push(l.Get(1))
	return 1
}

// checkSheet extracts the *luaSheet payload from the userdata at stack index n, or nil.
func checkSheet(l *lua.LState, n int) *luaSheet {
	ud, ok := l.Get(n).(*lua.LUserData)
	if !ok {
		return nil
	}
	s, ok := ud.Value.(*luaSheet)
	if !ok {
		return nil
	}
	return s
}

// tableToCells converts a Lua array table to []string, stringifying each element (so an author can
// pass a number cell like a level and it renders as text).
func tableToCells(l *lua.LState, t *lua.LTable) []string {
	length := t.Len()
	cells := make([]string, 0, length)
	for i := 1; i <= length; i++ {
		cells = append(cells, l.ToStringMeta(t.RawGetInt(i)).String())
	}
	return cells
}

// luaAligns reads an optional Lua array of alignment names at stack index n into []Align (nil when
// absent, so every column defaults to left).
func luaAligns(l *lua.LState, n int) []consoleui.Align {
	t, ok := l.Get(n).(*lua.LTable)
	if !ok {
		return nil
	}
	length := t.Len()
	aligns := make([]consoleui.Align, 0, length)
	for i := 1; i <= length; i++ {
		aligns = append(aligns, luaAlign(lua.LVAsString(t.RawGetInt(i))))
	}
	return aligns
}

// luaAlign maps an alignment name to a consoleui.Align (default left for anything unrecognized, so
// a typo fails soft rather than erroring mid-render).
func luaAlign(s string) consoleui.Align {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "right":
		return consoleui.Right
	case "center", "centre":
		return consoleui.Center
	default:
		return consoleui.Left
	}
}

// clampWidth bounds an explicit fixed width to [0, maxSheetWidth] (a negative width is left <=0,
// which consoleui treats as auto).
func clampWidth(w int) int {
	if w > maxSheetWidth {
		return maxSheetWidth
	}
	return w
}
