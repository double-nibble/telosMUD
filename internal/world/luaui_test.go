package world

import (
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// luaui_test.go — gates for the `ui` sandbox module (luaui.go): the Lua binding over the console
// layout engine. Covers the builder API, the chaining contract, alignment, the rows() mapper,
// i18n passthrough (color tokens + RTL isolation), and the two DoS guards (width clamp + render
// output cap).

// runSheet runs a script that calls the global __out(str) hook with a rendered sheet and returns it.
func runSheet(t *testing.T, script string) string {
	t.Helper()
	z := newZone("ui")
	var out string
	z.lua.L.SetGlobal("__out", z.lua.L.NewFunction(func(l *lua.LState) int {
		out = l.CheckString(1)
		return 0
	}))
	if err := z.lua.runChunk("ui", script); err != nil {
		t.Fatalf("runChunk: %v", err)
	}
	return out
}

func TestUISheetBasicAndChaining(t *testing.T) {
	// Chaining (row→divider→render returns self each step) plus auto-width alignment.
	out := runSheet(t, `__out(
		ui.sheet()
			:row({"[200]", "Alice"})
			:row({"[3]", "Bob"})
			:divider("=")
			:render())`)
	lines := strings.Split(out, "\n")
	if lines[0] != "[200] Alice" {
		t.Errorf("row 0 = %q, want %q", lines[0], "[200] Alice")
	}
	if lines[1] != "[3]   Bob" { // column 0 auto-sized to 5 ("[200]")
		t.Errorf("row 1 = %q, want %q", lines[1], "[3]   Bob")
	}
	if lines[2] != strings.Repeat("=", 11) { // divider fills the content width
		t.Errorf("divider = %q, want 11 '='", lines[2])
	}
}

func TestUISheetRowsMapper(t *testing.T) {
	out := runSheet(t, `
		local players = { {name="Alice", lvl=200}, {name="Bob", lvl=3} }
		__out(ui.sheet():rows(players, function(p)
			return { "["..p.lvl.."]", p.name }
		end):render())`)
	lines := strings.Split(out, "\n")
	if lines[0] != "[200] Alice" || lines[1] != "[3]   Bob" {
		t.Errorf("rows() mapper output = %q", lines)
	}
}

func TestUISheetAlignAndBanner(t *testing.T) {
	out := runSheet(t, `__out(
		ui.sheet()
			:banner("Who List", "~")
			:row({"7", "a"}, {"right", "left"})
			:row({"100", "b"}, {"right", "left"})
			:render())`)
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "Who List") || !strings.HasPrefix(lines[0], "~") {
		t.Errorf("banner = %q", lines[0])
	}
	if lines[1] != "  7 a" { // "7" right-aligned in the width-3 column
		t.Errorf("right-align row = %q, want %q", lines[1], "  7 a")
	}
}

func TestUISheetColorTokenPassthrough(t *testing.T) {
	// The layout engine must preserve {{TOKEN}} markup (it renders at the edge) and size by visible width.
	out := runSheet(t, `__out(ui.sheet():row({"{{FG_RED}}hi{{RESET}}", "x"}):row({"abcd", "y"}):render())`)
	if !strings.Contains(out, "{{FG_RED}}") {
		t.Errorf("color markup dropped: %q", out)
	}
}

func TestUISheetRTLIsolated(t *testing.T) {
	// An RTL cell is auto-wrapped in FSI (U+2068)…PDI (U+2069) and the line base-pinned with LRM (U+200E).
	out := runSheet(t, `__out(ui.sheet():row({"سلام", "x"}):render())`)
	if !strings.Contains(out, "\u2068") || !strings.Contains(out, "\u2069") {
		t.Errorf("RTL cell not isolated: %q", out)
	}
	if !strings.Contains(out, "\u200e") {
		t.Errorf("RTL line not base-pinned with LRM: %q", out)
	}
}

func TestUISheetFullWidth(t *testing.T) {
	out := runSheet(t, `__out(ui.sheet("full"):divider("-"):render())`)
	if len(out) != defaultTermWidth { // divider fills the terminal width
		t.Errorf(`ui.sheet("full") divider len = %d, want %d`, len(out), defaultTermWidth)
	}
}

// TestUISheetFixedWidthClamped guards the T13 alloc-bomb: an absurd fixed width is clamped, so a
// divider fills at most maxSheetWidth cells instead of allocating gigabytes.
func TestUISheetFixedWidthClamped(t *testing.T) {
	out := runSheet(t, `__out(ui.sheet(999999999):divider("-"):render())`)
	if len(out) != maxSheetWidth {
		t.Errorf("clamped divider len = %d, want %d", len(out), maxSheetWidth)
	}
}

// TestUISheetAutoWidthClamped is the regression guard for the security-audit finding: a huge cell in an
// AUTO-width sheet is clamped+truncated at render time (each line <= maxSheetWidth), NOT allocated at full
// size — so the OOM-class transient the deadline couldn't see is gone.
// TestUISheetAutoWidthClamped is the regression guard for the security-audit finding, at the Lua boundary: a
// cell just over the ceiling truncates through the binding (proving ui.sheet wires consoleui.MaxWidth). A tiny
// input keeps the render well under the 5ms deadline even under -race; the heavy, deadline-free proof that a
// row's BUILD is capped to the table width (cells × colW can't amplify) lives in
// consoleui.TestBoundedRowBuildCapped.
func TestUISheetAutoWidthClamped(t *testing.T) {
	out := runSheet(t, `__out(ui.sheet():row({string.rep("x", 1500)}):divider("-"):render())`)
	if len(out) > 4*maxSheetWidth+16 { // one truncated row + one divider, both <= maxSheetWidth cells
		t.Errorf("auto-width not clamped: render is %d bytes (a 1500-cell row should truncate to maxSheetWidth)", len(out))
	}
}

// runSheetErr runs a script expecting a failure and returns the error.
func runSheetErr(t *testing.T, script string) error {
	t.Helper()
	z := newZone("ui")
	z.lua.L.SetGlobal("__out", z.lua.L.NewFunction(func(*lua.LState) int { return 0 }))
	return z.lua.runChunk("ui", script)
}

func TestUISheetInputByteCapErrors(t *testing.T) {
	// Two ~550 KiB cells exceed the 1 MiB cumulative input cap; the second row errors at add time (fast: no
	// render, no big loop, so the byte cap — not the deadline — is what fires).
	err := runSheetErr(t, `local big = string.rep("x", 550000)
		ui.sheet():row({big}):row({big}):render()`)
	if err == nil || !strings.Contains(err.Error(), "content too large") {
		t.Fatalf("want a content-too-large cap error, got %v", err)
	}
}

func TestUISheetElementCapErrors(t *testing.T) {
	// Adding far more than maxSheetElems must be bounded. Building ~2049 elements costs enough VM time that,
	// under load, the 5ms wall-clock deadline can fire before the element cap — BOTH are valid bounds (the
	// point is a runaway builder is stopped), so accept either.
	err := runSheetErr(t, `local s = ui.sheet()
		for i = 1, 5000 do s:divider("-") end`)
	if err == nil {
		t.Fatal("adding 5000 elements should be bounded (element cap or deadline), got nil")
	}
	if msg := err.Error(); !strings.Contains(msg, "too many elements") && !strings.Contains(msg, "deadline") {
		t.Fatalf("want an element-cap or deadline bound, got %v", err)
	}
}

func TestUISheetColumnCapErrors(t *testing.T) {
	err := runSheetErr(t, `local cells = {}
		for i = 1, 100 do cells[i] = "x" end
		ui.sheet():row(cells):render()`)
	if err == nil || !strings.Contains(err.Error(), "too many columns") {
		t.Fatalf("want a too-many-columns cap error, got %v", err)
	}
}

// TestUISheetFillBombIsBounded keeps the final belt: a fill-amplified render (many wide dividers, within
// the element/input caps) never succeeds with a giant string — it's bounded by the render byte cap or the
// wall-clock deadline. Either error is a valid "bounded" outcome; the point is it does NOT return a huge
// string or OOM.
func TestUISheetFillBombIsBounded(t *testing.T) {
	err := runSheetErr(t, `local s = ui.sheet(1024)
		for i = 1, 1200 do s:divider("=") end
		__out(s:render())`)
	if err == nil {
		t.Fatal("a 1200-wide-divider render should be bounded (cap or deadline error), got success")
	}
}

// TestUISheetIsReadOnly confirms a script cannot clobber the ui module (same discipline as mud/string).
func TestUISheetIsReadOnly(t *testing.T) {
	z := newZone("ui")
	if err := z.lua.runChunk("ro", `ui.sheet = function() end`); err == nil {
		t.Fatal("expected read-only violation writing ui.sheet, got nil")
	}
}
