// Package textwidth measures the terminal DISPLAY width of text — columns/cells, not bytes or rune count —
// so column-framed output (the score sheet, any aligned table) lines up regardless of CJK-wide characters,
// combining marks, or multibyte encoding. It is the display-width measure docs/REMAINING.md Track 0 calls
// for and the Track 1 `score` framing (and any future ANSI-aware framing) must use instead of len(s).
//
// Width rules (the common, terminal-portable subset): a combining mark occupies 0 cells, an East Asian WIDE
// or FULLWIDTH rune occupies 2, a control rune 0, everything else 1. East Asian AMBIGUOUS is treated as 1
// (the modern-terminal convention). Emoji/ZWJ width is genuinely terminal-dependent and out of scope beyond
// what the East Asian width property already classifies as wide.
//
// These helpers measure a SINGLE line: CR/LF and other control runes score 0, so a caller framing multi-line
// output must split on "\n" first, and must pre-expand "\t" (tab width is cursor-position-dependent, which a
// context-free rune measure cannot answer). The control-rune-as-0 rule matches the edge's sanitizeOutput
// (internal/telnet), which strips the same unicode.IsControl set (except CR/LF) before the socket — so a
// width computed in the world stays valid after the edge strips.
package textwidth

import (
	"strings"
	"unicode"

	"golang.org/x/text/width"
)

// RuneWidth returns the display width of a single rune in terminal cells: 0 for a combining mark or a
// non-printing control rune, 2 for an East Asian wide/fullwidth rune, 1 otherwise.
func RuneWidth(r rune) int {
	// Combining marks (Mn = non-spacing, Me = enclosing) render ON the previous cell — 0 columns.
	if unicode.In(r, unicode.Mn, unicode.Me) {
		return 0
	}
	// C0/C1 control runes are non-printing (the edge's sanitizeOutput strips them; this is defensive so a
	// width measure of raw text never over-counts an escape). \t is width-ambiguous — treated as 0 here;
	// callers that keep tabs should expand them before measuring.
	if r < 0x20 || (r >= 0x7f && r < 0xa0) {
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	default:
		return 1
	}
}

// Width returns the total display width of s (the sum of each rune's cell width).
func Width(s string) int {
	w := 0
	for _, r := range s {
		w += RuneWidth(r)
	}
	return w
}

// Truncate returns the longest prefix of s whose display width is <= maxCells, never splitting a rune and
// never leaving a dangling half of a wide rune (a 2-cell rune that would cross the boundary is dropped
// whole). maxCells <= 0 returns "".
func Truncate(s string, maxCells int) string {
	if maxCells <= 0 {
		return ""
	}
	w := 0
	for i, r := range s {
		rw := RuneWidth(r)
		if w+rw > maxCells {
			return s[:i]
		}
		w += rw
	}
	return s
}

// Pad returns s right-padded with spaces to a display width of at least cells, for left-aligned column
// framing. If s already meets or exceeds cells it is returned unchanged (never truncated — use Truncate
// first to clip). Padding is measured in DISPLAY cells, so a CJK-wide column aligns with an ASCII one.
func Pad(s string, cells int) string {
	if pad := cells - Width(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}
