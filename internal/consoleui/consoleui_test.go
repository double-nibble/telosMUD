package consoleui

import (
	"strings"
	"testing"
)

// TestWhoListExample renders the design's illustrative who-list and pins the exact output, exercising
// auto-width, banner centering with fill, columnar alignment, dividers, and a spanning row together.
func TestWhoListExample(t *testing.T) {
	players := [][]string{
		{"[200]", "Alice the Warrior"},
		{"[3]", "Bob the Mage"},
		{"[5]", "Charlie the Rogue"},
	}
	s := New().Divider("-").Banner("Who List", "~").Divider("-")
	for _, p := range players {
		s.Row(p)
	}
	s.Divider("=").Span("There are 3 players online", Left).Divider("=")

	bar := strings.Repeat("-", 26)
	dbl := strings.Repeat("=", 26)
	want := strings.Join([]string{
		bar,
		"~~~~~~~~ Who List ~~~~~~~~",
		bar,
		"[200] Alice the Warrior",
		"[3]   Bob the Mage",
		"[5]   Charlie the Rogue",
		dbl,
		"There are 3 players online",
		dbl,
	}, "\n")

	if got := s.Render(); got != want {
		t.Errorf("who-list render mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestColorAwareSizing proves columns size to the VISIBLE width, ignoring {{...}} color markup: a colored cell
// and a plain cell of the same visible text pad to the same width, so the next column lines up.
func TestColorAwareSizing(t *testing.T) {
	s := New().
		Row([]string{"{{FG_RED}}AB{{RESET}}", "x"}).
		Row([]string{"ABCD", "y"})
	lines := strings.Split(s.Render(), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), lines)
	}
	// Column 0 auto-sizes to visible width 4 ("ABCD"); both rows must end up the same visible width.
	if a, b := visibleWidth(lines[0]), visibleWidth(lines[1]); a != b {
		t.Errorf("colored and plain rows misaligned: visible widths %d vs %d (%q / %q)", a, b, lines[0], lines[1])
	}
	if !strings.Contains(lines[0], "{{FG_RED}}") || !strings.Contains(lines[0], "{{RESET}}") {
		t.Errorf("color markup dropped from rendered cell: %q", lines[0])
	}
}

// TestCJKWidthSizing proves a double-width CJK cell sizes to 2 cells/rune, aligning with Latin cells.
func TestCJKWidthSizing(t *testing.T) {
	s := New().
		Row([]string{"世界", "x"}). // visible width 4
		Row([]string{"ab", "y"})  // visible width 2 -> padded to 4
	lines := strings.Split(s.Render(), "\n")
	if a, b := visibleWidth(lines[0]), visibleWidth(lines[1]); a != b {
		t.Errorf("CJK and Latin rows misaligned: %d vs %d (%q / %q)", a, b, lines[0], lines[1])
	}
}

func TestAlignment(t *testing.T) {
	s := New().
		Row([]string{"7", "a"}, Right, Left).
		Row([]string{"100", "b"}, Right, Left)
	lines := strings.Split(s.Render(), "\n")
	if lines[0] != "  7 a" { // "7" right-aligned in a width-3 column, then sep + "a"
		t.Errorf("right align: got %q, want %q", lines[0], "  7 a")
	}
	if lines[1] != "100 b" {
		t.Errorf("right align: got %q, want %q", lines[1], "100 b")
	}

	center := New().Row([]string{"x", "z"}, Center, Left).Row([]string{"12345", "z"}, Center, Left)
	cl := strings.Split(center.Render(), "\n")
	if cl[0] != "  x   z" { // "x" centered in width 5 -> "  x  ", + sep + "z"
		t.Errorf("center align: got %q, want %q", cl[0], "  x   z")
	}
}

func TestTruncateFixedWidth(t *testing.T) {
	s := NewFixed(10).Row([]string{"a very long cell here"})
	got := s.Render()
	if w := visibleWidth(got); w != 10 {
		t.Errorf("fixed width not honored: visible width %d, want 10 (%q)", w, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncation should append an ellipsis: %q", got)
	}
}

// TestTruncatePreservesColorTokens ensures truncation keeps zero-width color markup rather than slicing it.
func TestTruncatePreservesColorTokens(t *testing.T) {
	got := truncateVisible("{{FG_RED}}hello world{{RESET}}", 6)
	if w := visibleWidth(got); w != 6 {
		t.Errorf("truncated visible width %d, want 6 (%q)", w, got)
	}
	if !strings.Contains(got, "{{FG_RED}}") {
		t.Errorf("leading color token dropped: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("want trailing ellipsis: %q", got)
	}
}

func TestDividerFillsWidth(t *testing.T) {
	s := New().Span("hello", Left).Divider("=")
	lines := strings.Split(s.Render(), "\n")
	if lines[1] != "=====" {
		t.Errorf("divider should fill to content width: got %q, want %q", lines[1], "=====")
	}
}

func TestBannerNoRoomForFill(t *testing.T) {
	// A banner wider than the (fixed) sheet falls back to a centered/truncated title without fill.
	s := NewFixed(6).Banner("LongTitle", "~")
	got := s.Render()
	if w := visibleWidth(got); w != 6 {
		t.Errorf("banner over-width not clamped: visible %d, want 6 (%q)", w, got)
	}
}

func TestVisibleWidthUnclosedToken(t *testing.T) {
	// An unclosed "{{" is literal text (matches the edge renderer), so it counts toward width.
	if w := visibleWidth("{{FG_RED"); w != 8 {
		t.Errorf("unclosed token width: got %d, want 8", w)
	}
}

// TestVisibleWidthMatchesEdgeTokenizer is the regression guard for the ship-blocker the edge review caught: only
// KNOWN, closed tokens are zero-width; an unknown/typo'd token renders as LITERAL visible text at the edge, so it
// must be measured that way here too. A divergence under-sizes the cell and shifts every column to its right.
func TestVisibleWidthMatchesEdgeTokenizer(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"{{FG_RED}}hi", 2},             // known token: zero width, "hi" = 2
		{"{{FG_MAUVE}}hi", 14},          // unknown: whole "{{FG_MAUVE}}hi" is literal
		{"{{FG_RED}}{{FG_MAUVE}}x", 13}, // known prefix zero-width, "{{FG_MAUVE}}x" literal = 13
		{"{{{{FG_RED}}x", 3},            // literal "{{" (2) + known token (0) + "x" (1)
		{"a {{ b }} c", 11},             // "{{ b }}" is not a known token -> all literal
	}
	for _, tc := range cases {
		if got := visibleWidth(tc.in); got != tc.want {
			t.Errorf("visibleWidth(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestRaggedRows pins that column widths come from a FULL pass over all rows (not streaming), so a short row and
// a later wider row both align, and a row simply renders the cells it has.
func TestRaggedRows(t *testing.T) {
	short := New().Row([]string{"x"}).Row([]string{"a", "b", "c"})
	if got := strings.Split(short.Render(), "\n"); got[0] != "x" || got[1] != "a b c" {
		t.Errorf("ragged (short first) = %q, want [\"x\" \"a b c\"]", got)
	}
	long := New().Row([]string{"a", "bb", "c"}).Row([]string{"x"})
	if got := strings.Split(long.Render(), "\n"); got[0] != "a bb c" || got[1] != "x" {
		t.Errorf("ragged (long first) = %q, want [\"a bb c\" \"x\"]", got)
	}
}

func TestTruncateMaxOne(t *testing.T) {
	if got := truncateVisible("hello", 1); got != "…" {
		t.Errorf("truncateVisible(_, 1) = %q, want %q", got, "…")
	}
}

func TestExactFitNoEllipsis(t *testing.T) {
	if got := padVisible("abc", 3, Left); got != "abc" {
		t.Errorf("exact-fit cell padded/truncated: got %q, want %q", got, "abc")
	}
	if got := NewFixed(3).Row([]string{"abc"}).Render(); got != "abc" {
		t.Errorf("exact-fit fixed row: got %q, want %q", got, "abc")
	}
}

func TestRepeatToWideFillRemainder(t *testing.T) {
	// A 2-cell fill into an odd width fills whole glyphs then space-pads the 1-cell remainder.
	got := New().Span("hello", Left).Divider("<>").Render()
	div := strings.Split(got, "\n")[1]
	if div != "<><> " {
		t.Errorf("wide-fill divider = %q, want %q", div, "<><> ")
	}
	if visibleWidth(div) != 5 {
		t.Errorf("wide-fill divider width = %d, want 5", visibleWidth(div))
	}
}

func TestCombiningMarkWidth(t *testing.T) {
	// "e" + combining acute is one visible cell despite being two runes.
	if w := visibleWidth("é"); w != 1 {
		t.Errorf("combining-mark width = %d, want 1", w)
	}
	s := New().Row([]string{"é", "x"}).Row([]string{"a", "y"})
	lines := strings.Split(s.Render(), "\n")
	if a, b := visibleWidth(lines[0]), visibleWidth(lines[1]); a != b {
		t.Errorf("combining-mark row misaligned: %d vs %d", a, b)
	}
}

func TestEmptySheetAndZeroFixed(t *testing.T) {
	if got := New().Render(); got != "" {
		t.Errorf("empty sheet = %q, want empty", got)
	}
	if got := New().Row([]string{}).Render(); got != "" {
		t.Errorf("empty-row sheet = %q, want empty", got)
	}
	// NewFixed(0) is treated as AUTO (0 == "no fixed width"), a deliberate semantic callers may lean on.
	if got := NewFixed(0).Span("hi", Left).Divider("-").Render(); got != "hi\n--" {
		t.Errorf("NewFixed(0) not auto: got %q, want %q", got, "hi\n--")
	}
}

// TestRTLWidthAligns pins that RTL text is measured by DISPLAY width (Arabic base letters = 1 cell, harakat
// diacritics = 0), so an Arabic cell aligns by width with a Latin cell — the CJK guarantee, for RTL. (Visual
// bidi ORDERING via isolates is a separate, later commit; this pins the width layer, which works today.)
func TestRTLWidthAligns(t *testing.T) {
	if w := visibleWidth("سلام"); w != 4 { // 4 Arabic base letters, each 1 cell
		t.Errorf("Arabic width = %d, want 4", w)
	}
	if w := visibleWidth("سَلام"); w != 4 { // a fatha diacritic (U+064E) is a combining mark -> 0
		t.Errorf("Arabic-with-harakat width = %d, want 4", w)
	}
	s := New().Row([]string{"سلام", "x"}).Row([]string{"abcd", "y"})
	lines := strings.Split(s.Render(), "\n")
	if a, b := visibleWidth(lines[0]), visibleWidth(lines[1]); a != b {
		t.Errorf("RTL and Latin rows misaligned: %d vs %d", a, b)
	}
}
