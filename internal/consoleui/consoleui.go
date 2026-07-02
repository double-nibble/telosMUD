// Package consoleui is a terminal LAYOUT toolkit — "Bootstrap for console output": a builder that assembles
// aligned columnar rows, spanning lines, dividers, and centered banners into a framed text block, so content
// authors describe WHAT a sheet is (a who-list, a score sheet) and never compute widths or padding.
//
// All sizing is DISPLAY-width aware and COLOR-token aware: it measures the VISIBLE width of each cell via
// internal/textwidth (CJK-wide = 2 cells, combining marks = 0) while IGNORING `{{...}}` color markup (which
// renders to zero-width SGR at the edge). So a `{{FG_RED}}200{{RESET}}` cell aligns with a plain `200`, and a
// CJK name lines up with a Latin one — the alignment survives the edge's color render and multibyte text.
//
// The Go engine here is pure + fully unit-testable; a later slice exposes it to the Lua sandbox (the `ui`
// module display templates use) and resolves "full" width from the client's negotiated terminal size.
package consoleui

import (
	"strings"
	"unicode/utf8"

	"github.com/double-nibble/telosmud/internal/colormarkup"
	"github.com/double-nibble/telosmud/internal/textwidth"
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
	elems      []element
}

// New returns an auto-width sheet: the total width fits the widest row/span/banner.
func New() *Sheet { return &Sheet{} }

// NewFixed returns a sheet whose total width is exactly width cells; content wider than that is truncated
// with an ellipsis. (The Lua layer maps a "full" request to the client's terminal width via this.)
func NewFixed(width int) *Sheet { return &Sheet{fixedWidth: width} }

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
	if tableW < 0 {
		tableW = 0
	}

	lines := make([]string, 0, len(s.elems))
	for _, e := range s.elems {
		switch e.kind {
		case elemRow:
			lines = append(lines, s.renderRow(e, colW, tableW))
		case elemSpan:
			lines = append(lines, padVisible(e.text, tableW, e.align))
		case elemDivider:
			lines = append(lines, repeatTo(e.fill, tableW))
		case elemBanner:
			lines = append(lines, renderBanner(e.text, e.fill, tableW))
		}
	}
	return strings.Join(lines, "\n")
}

func (s *Sheet) fixed() bool { return s.fixedWidth > 0 }

// columnWidths computes the max visible width of each column across all rows.
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
			colW[i] = max(colW[i], visibleWidth(c))
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
// line to the sheet width in fixed mode. Trailing padding on the final column is trimmed for clean output.
func (s *Sheet) renderRow(e element, colW []int, tableW int) string {
	parts := make([]string, len(e.cells))
	for i, c := range e.cells {
		a := Left
		if i < len(e.aligns) {
			a = e.aligns[i]
		}
		w := 0
		if i < len(colW) {
			w = colW[i]
		}
		parts[i] = padVisible(c, w, a)
	}
	line := strings.TrimRight(strings.Join(parts, colSep), " ")
	if s.fixed() && visibleWidth(line) > tableW {
		line = truncateVisible(line, tableW)
	}
	return line
}

// renderBanner centers text with fill on both sides to tableW: `<fill> text <fill>`.
func renderBanner(text, fill string, tableW int) string {
	if visibleWidth(text)+2 >= tableW || fill == "" {
		return padVisible(text, tableW, Center) // no room for fill; just center
	}
	gap := tableW - (visibleWidth(text) + 2) // space consumed by the two spaces around text
	l := gap / 2
	return repeatTo(fill, l) + " " + text + " " + repeatTo(fill, gap-l)
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
// an ellipsis "…" (1 cell) when content is dropped. maxCells<=0 yields "".
//
// Known limitation: if truncation cuts before a cell's closing reset token, that token is dropped and the color
// can bleed rightward until the edge's end-of-payload reset. This only bites fixed-width overflow of a colored
// cell; a color-vocabulary-aware reset on truncation is a follow-up (the package intentionally knows the token
// FORMAT, not the token NAMES, to stay decoupled from the edge's SGR map).
func truncateVisible(s string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	limit := maxCells - 1 // reserve one cell for the ellipsis
	var b strings.Builder
	vw := 0
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "{{") {
			if params, next := colormarkup.ScanTokenRun(s, i); len(params) > 0 {
				b.WriteString(s[i:next]) // keep the whole (zero-width) known token run
				i = next
				continue
			}
			if vw+2 > limit { // literal "{{" is two visible cells (matches the edge)
				return b.String() + "…"
			}
			b.WriteString("{{")
			vw += 2
			i += 2
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		rw := textwidth.RuneWidth(r)
		if vw+rw > limit {
			return b.String() + "…"
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
