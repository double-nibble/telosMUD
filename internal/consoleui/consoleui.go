// Package consoleui is a terminal LAYOUT toolkit — "Bootstrap for console output": a builder that assembles
// aligned columnar rows, spanning lines, dividers, and centered banners into a framed text block, so content
// authors describe WHAT a sheet is (a who-list, a score sheet) and never compute widths or padding.
//
// All sizing is DISPLAY-width aware and COLOR-token aware: it measures the VISIBLE width of each cell via
// internal/textwidth (CJK-wide = 2 cells, combining marks = 0) while IGNORING `{{...}}` color markup (which
// renders to zero-width SGR at the edge). So a `{{FG_RED}}200{{RESET}}` cell aligns with a plain `200`, and a
// CJK name lines up with a Latin one — the alignment survives the edge's color render and multibyte text.
//
// RTL text is handled three ways: its width is measured correctly (Arabic base letters = 1 cell, harakat = 0);
// any cell containing RTL is auto-wrapped in a bidi isolate (FSI…PDI) so a bidi terminal can't reorder it across
// column boundaries; and a line carrying RTL is prefixed with an LRM to pin its base direction to LTR (so an
// RTL column 0 can't flip the whole grid). Pure-LTR content is untouched.
//
// These controls (U+2068/U+2069/U+200E) are zero-width on a UTF-8, bidi-aware or Cf-suppressing client — the
// modern common case. On a non-UTF-8 client they'd be garbage bytes, but such a client already can't render the
// RTL runes that trigger them. Gating isolate emission on the edge's negotiated CHARSET (like ANSI is gated on
// color capability) is a follow-up for the edge, which — unlike this pure layout engine — knows the client's
// encoding.
//
// The Go engine here is pure + fully unit-testable; a later slice exposes it to the Lua sandbox (the `ui`
// module display templates use) and resolves "full" width from the client's negotiated terminal size.
package consoleui

import (
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/bidi"

	"github.com/double-nibble/telosmud/internal/colormarkup"
	"github.com/double-nibble/telosmud/internal/textwidth"
)

// Bidi isolate controls (Unicode 6.3). A cell containing RTL text is wrapped in FSI…PDI so a bidi-capable
// terminal treats it as a self-contained unit that can't reorder relative to neighboring columns — keeping the
// monospace grid stable. FSI auto-detects the cell's direction; both controls are zero-width (Cf), so non-bidi
// terminals ignore them and the width math is unaffected.
const (
	fsi = "\u2068" // FIRST STRONG ISOLATE
	pdi = "\u2069" // POP DIRECTIONAL ISOLATE
	lrm = "\u200e" // LEFT-TO-RIGHT MARK (pins a line's base direction to LTR)
)

// Align controls a column's or a spanning line's horizontal alignment.
type Align int

// Alignment options for columns and spanning lines.
const (
	Left Align = iota
	Right
	Center
)

const colSep = " " // one space between columns

type elemKind int

const (
	elemRow elemKind = iota
	elemSpan
	elemDivider
	elemBanner
)

type element struct {
	kind   elemKind
	cells  []string // elemRow: the column cells
	aligns []Align  // elemRow: per-column alignment (default Left)
	text   string   // elemSpan / elemBanner
	align  Align    // elemSpan
	fill   string   // elemDivider / elemBanner fill char(s)
}

// Sheet accumulates layout elements and renders them to one aligned text block. Build with New (auto width)
// or NewFixed (a fixed column count of cells wide); the chainable add-methods return the Sheet.
type Sheet struct {
	fixedWidth int // 0 => auto (fit the widest content)
	maxWidth   int // 0 => unbounded; else a hard ceiling on every column AND the total width
	elems      []element
}

// New returns an auto-width sheet: the total width fits the widest row/span/banner.
func New() *Sheet { return &Sheet{} }

// NewFixed returns a sheet whose total width is exactly width cells; content wider than that is truncated
// with an ellipsis. (The Lua layer maps a "full" request to the client's terminal width via this.)
func NewFixed(width int) *Sheet { return &Sheet{fixedWidth: width} }

// MaxWidth sets a hard ceiling (in cells) on every column width AND the total table width, so an untrusted
// caller can't drive an unbounded allocation via a huge AUTO width (one giant cell, or a divider/pad filling to
// it). Content wider than the ceiling is truncated with an ellipsis, exactly like fixed mode. n<=0 clears it.
// Returns the sheet for chaining.
func (s *Sheet) MaxWidth(n int) *Sheet {
	s.maxWidth = n
	return s
}

// Row adds a columnar row. aligns[i] sets column i's alignment (default Left when omitted).
func (s *Sheet) Row(cells []string, aligns ...Align) *Sheet {
	s.elems = append(s.elems, element{kind: elemRow, cells: cells, aligns: aligns})
	return s
}

// Span adds a single full-width line (not columnar), aligned within the sheet width.
func (s *Sheet) Span(text string, align Align) *Sheet {
	s.elems = append(s.elems, element{kind: elemSpan, text: text, align: align})
	return s
}

// Divider adds a full-width line of the fill char(s).
func (s *Sheet) Divider(fill string) *Sheet {
	s.elems = append(s.elems, element{kind: elemDivider, fill: fill})
	return s
}

// Banner adds a centered title with fill on both sides (e.g. "~~~~ Who List ~~~~").
func (s *Sheet) Banner(text, fill string) *Sheet {
	s.elems = append(s.elems, element{kind: elemBanner, text: text, fill: fill})
	return s
}

// Render assembles the sheet into a newline-joined string (with any color markup preserved for the edge).
func (s *Sheet) Render() string {
	colW := s.columnWidths()
	content := colsTotal(colW)
	tableW := content
	if !s.fixed() { // auto: also grow to fit spans/banners
		for _, e := range s.elems {
			switch e.kind {
			case elemSpan:
				tableW = max(tableW, visibleWidth(e.text))
			case elemBanner:
				tableW = max(tableW, visibleWidth(e.text)+4) // text + 2 spaces + min 2 fill
			}
		}
	} else {
		tableW = s.fixedWidth
	}
	if s.maxWidth > 0 && tableW > s.maxWidth { // hard ceiling on the total width (auto or fixed)
		tableW = s.maxWidth
	}
	if tableW < 0 {
		tableW = 0
	}

	lines := make([]string, 0, len(s.elems))
	for _, e := range s.elems {
		switch e.kind {
		case elemRow:
			lines = append(lines, pinBaseLTR(s.renderRow(e, colW, tableW)))
		case elemSpan:
			lines = append(lines, pinBaseLTR(fitCell(e.text, tableW, e.align)))
		case elemDivider:
			lines = append(lines, repeatTo(e.fill, tableW))
		case elemBanner:
			lines = append(lines, pinBaseLTR(renderBanner(e.text, e.fill, tableW)))
		}
	}
	return strings.Join(lines, "\n")
}

func (s *Sheet) fixed() bool { return s.fixedWidth > 0 }

// bounded reports whether the sheet has a hard total width (fixed or a max ceiling) that a too-wide row must be
// truncated to.
func (s *Sheet) bounded() bool { return s.fixedWidth > 0 || s.maxWidth > 0 }

// columnWidths computes the max visible width of each column across all rows, capping each at maxWidth so no
// single column can drive an unbounded per-cell pad allocation.
func (s *Sheet) columnWidths() []int {
	var colW []int
	for _, e := range s.elems {
		if e.kind != elemRow {
			continue
		}
		for i, c := range e.cells {
			for len(colW) <= i {
				colW = append(colW, 0)
			}
			w := max(colW[i], visibleWidth(c))
			if s.maxWidth > 0 && w > s.maxWidth {
				w = s.maxWidth
			}
			colW[i] = w
		}
	}
	return colW
}

func colsTotal(colW []int) int {
	total := 0
	for _, w := range colW {
		total += w
	}
	if len(colW) > 1 {
		total += (len(colW) - 1) * visibleWidth(colSep)
	}
	return total
}

// renderRow pads each cell to its column width (per align), joins with the separator, and truncates the whole
// line to the sheet width when bounded. Trailing padding on the final column is trimmed for clean output.
//
// When bounded, it stops building once the accumulated width reaches tableW and caps the straddling cell's pad
// to what still fits: everything past tableW is truncated away anyway, so building it is pure waste — and,
// crucially, that waste is the render-time DoS amplifier (cells × colW can be far larger than tableW). Capping
// the BUILD to ~tableW bounds a row's allocation to the line width regardless of column count or column widths.
// For fitting content (or an unbounded sheet) the accumulator never reaches tableW, so this is a no-op and the
// layout is byte-identical.
func (s *Sheet) renderRow(e element, colW []int, tableW int) string {
	var b strings.Builder
	acc := 0 // visible cells built so far
	for i, c := range e.cells {
		if s.bounded() && acc >= tableW {
			break // the rest of the row would be truncated away — don't allocate it (DoS guard)
		}
		if i > 0 {
			b.WriteString(colSep)
			acc += visibleWidth(colSep)
		}
		a := Left
		if i < len(e.aligns) {
			a = e.aligns[i]
		}
		w := 0
		if i < len(colW) {
			w = colW[i]
		}
		if s.bounded() && acc+w > tableW { // cap the straddling cell to the remaining width
			w = tableW - acc
		}
		b.WriteString(fitCell(c, w, a))
		acc += w
	}
	line := strings.TrimRight(b.String(), " ")
	if s.bounded() && visibleWidth(line) > tableW {
		// Bounded-width overflow: truncate the assembled row. This can cut mid-cell and drop a trailing PDI, but a
		// bidi terminal auto-closes an open isolate at the line boundary, so the blast radius is one line.
		line = truncateVisible(line, tableW)
	}
	return line
}

// renderBanner centers text with fill on both sides to tableW: `<fill> text <fill>`.
func renderBanner(text, fill string, tableW int) string {
	text = bidiIsolate(text) // keep an RTL title from reordering with the fill
	if visibleWidth(text)+2 >= tableW || fill == "" {
		return padVisible(text, tableW, Center) // no room for fill; just center
	}
	gap := tableW - (visibleWidth(text) + 2) // space consumed by the two spaces around text
	l := gap / 2
	return repeatTo(fill, l) + " " + text + " " + repeatTo(fill, gap-l)
}

// fitCell lays a cell into a fixed-width slot: it first TRUNCATES the raw content to fit (so any closing color
// token / ellipsis is decided on the real text), then wraps it in a bidi isolate if it holds RTL text (applied
// AFTER truncation so the closing PDI is never cut), then pads to width. The isolate is zero-width, so padding
// (grid structure, in the line's base direction) lands outside it and the width math is unchanged.
func fitCell(content string, width int, align Align) string {
	if visibleWidth(content) > width {
		content = truncateVisible(content, width)
	}
	return padVisible(bidiIsolate(content), width, align)
}

// bidiIsolate wraps s in FSI…PDI when it contains any strong RTL character (bidi class R or AL); pure-LTR/ASCII
// content is returned unchanged so the common case stays byte-for-byte identical.
func bidiIsolate(s string) string {
	if !hasRTL(s) {
		return s
	}
	return fsi + s + pdi
}

// pinBaseLTR pins a rendered line's BASE paragraph direction to LTR (via a leading zero-width LRM) when the line
// carries an RTL isolate. Without it, a terminal that first-strong-detects the line's direction would flip the
// whole grid — padding, column order, and all — to RTL when column 0 happens to be RTL. LRM needs no closer, so
// it doesn't perturb the FSI/PDI balance. No-op for the pure-LTR common case (and for dividers, which never
// contain an isolate).
func pinBaseLTR(line string) string {
	if strings.Contains(line, fsi) {
		return lrm + line
	}
	return line
}

// hasRTL reports whether s contains a right-to-left character: strong RTL (Hebrew = R, Arabic = AL) or an
// Arabic-Indic NUMBER (AN) — AN runs also reorder under the bidi algorithm, so an all-Arabic-digits cell still
// needs isolation even with no strong-RTL letter.
func hasRTL(s string) bool {
	for _, r := range s {
		p, _ := bidi.LookupRune(r)
		switch p.Class() {
		case bidi.R, bidi.AL, bidi.AN:
			return true
		}
	}
	return false
}

// --- visible-width helpers (color-token + display-width aware) ----------------------------------

// visibleWidth returns the display width of s in terminal cells. It uses the SHARED colormarkup tokenizer so it
// agrees with the edge exactly: a run of KNOWN, closed `{{TOKEN}}` markup renders to zero-width SGR and is not
// counted, but an unknown/typo'd/unclosed `{{...}}` is emitted LITERALLY by the edge, so its `{{` counts as two
// visible cells (and the remainder is measured as ordinary text). Getting this wrong under-measures a cell with a
// bad token and shifts every column to its right — hence one tokenizer, not two.
func visibleWidth(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "{{") {
			if params, next := colormarkup.ScanTokenRun(s, i); len(params) > 0 {
				i = next // known token run: zero width
				continue
			}
			w += 2 // literal "{{" (edge emits it verbatim); rescan the rest as ordinary text
			i += 2
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += textwidth.RuneWidth(r)
		i += size
	}
	return w
}

// padVisible pads (or truncates) s to exactly width visible cells, applying align. Color tokens are preserved
// and uncounted; an over-wide s is truncated with an ellipsis before padding.
func padVisible(s string, width int, align Align) string {
	vw := visibleWidth(s)
	if vw > width {
		s = truncateVisible(s, width)
		vw = visibleWidth(s)
	}
	pad := width - vw
	if pad <= 0 {
		return s
	}
	switch align {
	case Right:
		return strings.Repeat(" ", pad) + s
	case Center:
		l := pad / 2
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", pad-l)
	default:
		return s + strings.Repeat(" ", pad)
	}
}

// truncateVisible truncates s to at most maxCells visible cells, PRESERVING `{{...}}` color tokens and appending
// an ellipsis "…" (1 cell) when content is dropped. It also tracks bidi-isolate nesting and CLOSES any isolate
// still open at the cut (before the ellipsis), so truncating an RTL cell never leaves a dangling FSI. maxCells<=0
// yields "".
//
// Known limitation: if truncation cuts before a cell's closing COLOR reset token, that token is dropped and the
// color can bleed rightward until the edge's end-of-payload reset. This only bites fixed-width overflow of a
// colored cell; a color-vocabulary-aware reset on truncation is a follow-up (the package intentionally knows the
// token FORMAT, not the token NAMES, to stay decoupled from the edge's SGR map — unlike the isolate pair, which
// is this package's own).
func truncateVisible(s string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	limit := maxCells - 1 // reserve one cell for the ellipsis
	var b strings.Builder
	vw := 0
	isolateDepth := 0
	cut := func() string { return b.String() + "…" + strings.Repeat(pdi, isolateDepth) }
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "{{") {
			if params, next := colormarkup.ScanTokenRun(s, i); len(params) > 0 {
				b.WriteString(s[i:next]) // keep the whole (zero-width) known token run
				i = next
				continue
			}
			if vw+2 > limit { // literal "{{" is two visible cells (matches the edge)
				return cut()
			}
			b.WriteString("{{")
			vw += 2
			i += 2
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := textwidth.RuneWidth(r)
		if vw+rw > limit {
			return cut()
		}
		switch s[i : i+size] { // track isolate nesting so a mid-isolate cut still closes it
		case fsi:
			isolateDepth++
		case pdi:
			if isolateDepth > 0 {
				isolateDepth--
			}
		}
		b.WriteString(s[i : i+size])
		vw += rw
		i += size
	}
	return b.String() // fit without truncation (defensive; caller only truncates when over-wide)
}

// repeatTo builds a line of the fill char(s) exactly width cells wide (a remainder narrower than a wide fill
// char is space-padded).
func repeatTo(fill string, width int) string {
	fw := visibleWidth(fill)
	if width <= 0 || fw <= 0 {
		return ""
	}
	var b strings.Builder
	w := 0
	for w+fw <= width {
		b.WriteString(fill)
		w += fw
	}
	if w < width {
		b.WriteString(strings.Repeat(" ", width-w))
	}
	return b.String()
}
