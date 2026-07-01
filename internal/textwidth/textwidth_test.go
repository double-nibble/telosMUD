package textwidth

import "testing"

// textwidth_test.go — the display-width measure (docs/REMAINING.md Track 0): pins that width is counted in
// terminal CELLS (not bytes/runes) so CJK-wide, combining marks, RTL, and multibyte encoding frame correctly.
// Combining / decomposed / control runes are built from EXPLICIT code points (rune(0x...)) rather than source
// literals, so the source encoding can't make an assertion pass for the wrong reason — a literal "é" is
// precomposed U+00E9, NOT a base+combining sequence, and a literal combining mark is invisible in a diff.

// Explicit code points used by the tests (unambiguous vs. source literals).
const (
	cpAcute      = rune(0x0301) // COMBINING ACUTE ACCENT (Mn) — 0 cells
	cpTanwin     = rune(0x064B) // ARABIC FATHATAN (Mn) — 0 cells
	cpEnclosing  = rune(0x20DD) // COMBINING ENCLOSING CIRCLE (Me) — 0 cells
	cpDevaSignAA = rune(0x093E) // DEVANAGARI VOWEL SIGN AA (Mc, SPACING) — 1 cell (must NOT be forced to 0)
	cpMeem       = rune(0x0645) // ARABIC LETTER MEEM (base) — 1 cell
	cpPrecompE   = rune(0x00E9) // é precomposed — 1 cell

	// Zero-width FORMAT chars (Cf) — must measure 0 (the user requirement for grapheme/bidi handling).
	cpZWJ        = rune(0x200D) // ZERO WIDTH JOINER (grapheme-cluster glue for emoji sequences)
	cpZWNJ       = rune(0x200C) // ZERO WIDTH NON-JOINER
	cpZWSP       = rune(0x200B) // ZERO WIDTH SPACE
	cpLRM        = rune(0x200E) // LEFT-TO-RIGHT MARK (implicit-bidi hint)
	cpRLM        = rune(0x200F) // RIGHT-TO-LEFT MARK
	cpWordJoiner = rune(0x2060) // WORD JOINER
	cpBOM        = rune(0xFEFF) // ZERO WIDTH NO-BREAK SPACE / BOM
	cpVS16       = rune(0xFE0F) // VARIATION SELECTOR-16 (Mn) — 0 cells

	// Wide / East Asian.
	cpFullwidthA = rune(0xFF21) // FULLWIDTH LATIN CAPITAL A — 2 cells
	cpHalfKana   = rune(0xFF71) // HALFWIDTH KATAKANA A — 1 cell

	// Emoji grapheme-cluster parts (each a wide rune; the cluster is rune-summed, not glyph-measured).
	cpManEmoji   = rune(0x1F468) // 👨 — 2 cells
	cpWomanEmoji = rune(0x1F469) // 👩 — 2 cells
	cpGirlEmoji  = rune(0x1F467) // 👧 — 2 cells
)

func TestRuneWidth(t *testing.T) {
	cases := []struct {
		name string
		r    rune
		want int
	}{
		{"ascii", 'A', 1},
		{"space", ' ', 1},
		{"precomposed accented U+00E9", cpPrecompE, 1},
		{"CJK han (wide)", '世', 2},
		{"hiragana (wide)", 'あ', 2},
		{"fullwidth digit", '３', 2},
		{"halfwidth katakana (narrow)", 'ｱ', 1},
		{"arabic base letter meem (narrow)", cpMeem, 1},
		{"arabic combining tanwin U+064B (0 cells)", cpTanwin, 0},
		{"combining acute Mn U+0301 (0 cells)", cpAcute, 0},
		{"combining enclosing Me U+20DD (0 cells)", cpEnclosing, 0},
		{"devanagari SPACING-combining Mc U+093E (1 cell, NOT 0)", cpDevaSignAA, 1},
		{"C0 control BEL U+0007", rune(0x0007), 0},
		{"C1 control NEL U+0085", rune(0x0085), 0},
		{"DEL U+007F", rune(0x007F), 0},
		// Zero-width format chars (Cf) — the user's zero-width-modifier requirement.
		{"ZWJ U+200D (0 cells)", cpZWJ, 0},
		{"ZWNJ U+200C (0 cells)", cpZWNJ, 0},
		{"ZWSP U+200B (0 cells)", cpZWSP, 0},
		{"LRM U+200E bidi mark (0 cells)", cpLRM, 0},
		{"RLM U+200F bidi mark (0 cells)", cpRLM, 0},
		{"word joiner U+2060 (0 cells)", cpWordJoiner, 0},
		{"BOM/ZWNBSP U+FEFF (0 cells)", cpBOM, 0},
		{"variation selector-16 U+FE0F (0 cells)", cpVS16, 0},
		// Fullwidth vs halfwidth (CJK).
		{"fullwidth latin A U+FF21 (2 cells)", cpFullwidthA, 2},
		{"halfwidth katakana U+FF71 (1 cell)", cpHalfKana, 1},
		{"astral emoji U+1F468 (wide, 2 cells)", cpManEmoji, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RuneWidth(tc.r); got != tc.want {
				t.Fatalf("RuneWidth(%U) = %d, want %d", tc.r, got, tc.want)
			}
		})
	}
}

func TestWidth(t *testing.T) {
	decomposed := "cafe" + string(cpAcute)                    // c a f e + combining acute → 4 cells, 6 bytes
	allCombining := string([]rune{cpAcute, cpAcute, cpAcute}) // three 0-width marks → 0 cells
	arabic := "مرحبا" + string(cpTanwin) + " يا عالم"         // Arabic "Hello world" with an explicit tanwin
	cases := []struct {
		name string
		s    string
		want int
	}{
		{"ascii", "score", 5},
		{"empty", "", 0},
		{"mixed ascii + CJK", "HP 世界", 3 + 2 + 2}, // "HP " = 3 cells, 世界 = 4
		{"precomposed accented (caf + U+00E9)", "caf" + string(cpPrecompE), 4},
		{"decomposed base+combining (cafe + U+0301)", decomposed, 4},
		{"all combining marks (0 cells)", allCombining, 0},
		{"emoji-free CJK sentence", "你好", 4},
		// 12 base letters + 2 spaces (the tanwin is 0-width) = 13 display cells, though it is 26 bytes / 14 runes.
		{"RTL arabic with a combining tanwin", arabic, 13},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Width(tc.s); got != tc.want {
				t.Fatalf("Width(%q) = %d, want %d (byte len = %d)", tc.s, got, tc.want, len(tc.s))
			}
		})
	}
	// Sanity-pin the decomposed grapheme is genuinely a base+combining sequence (2 runes, 6 bytes), not the
	// precomposed single rune — so the case above tests what it claims.
	if r := []rune(decomposed); len(r) != 5 || r[4] != cpAcute {
		t.Fatalf("decomposed café must end in a base+combining sequence, got runes %U", r)
	}
	// The whole point: display width != byte length for multibyte text — asserted for a CJK and an RTL
	// string, so a caller framing columns can never silently fall back to len(s).
	for _, s := range []string{"世界", arabic} {
		if Width(s) == len(s) {
			t.Fatalf("Width must measure cells, not bytes: %q Width=%d, bytes=%d", s, Width(s), len(s))
		}
	}
}

// TestGraphemeClusterAndBidiWidth pins the two behaviors the user called out: a ZWJ grapheme cluster is
// measured as the RUNE-SUM (documented limitation — not a single composed glyph), and bidirectional text is
// measured in LOGICAL order so width is order-independent (however the terminal visually reorders LTR runs
// inside RTL, the cell count is the same).
func TestGraphemeClusterAndBidiWidth(t *testing.T) {
	// Family ZWJ sequence 👨‍👩‍👧 = man ZWJ woman ZWJ girl. Rune-sum = 2+0+2+0+2 = 6 (a terminal that composes
	// the glyph shows 2; rune-sum is the pragmatic, portable measure and this test pins that intent).
	family := string([]rune{cpManEmoji, cpZWJ, cpWomanEmoji, cpZWJ, cpGirlEmoji})
	if got := Width(family); got != 6 {
		t.Fatalf("ZWJ family cluster width = %d, want 6 (rune-sum: 2+0+2+0+2)", got)
	}
	// The ZWJ itself contributes nothing: the same base runes WITHOUT the joiners have the same width.
	if got := Width(string([]rune{cpManEmoji, cpWomanEmoji, cpGirlEmoji})); got != 6 {
		t.Fatalf("three wide emoji without ZWJ = %d, want 6 (ZWJ is 0-width)", got)
	}

	// Bidi: English/URL embedded in Arabic. Width is order-independent — reversing the rune order (a crude
	// stand-in for the terminal's visual bidi reordering) yields the SAME cell count.
	bidi := "قال hello world " + "http://example.com/x " + "للعالم"
	rev := []rune(bidi)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if Width(bidi) != Width(string(rev)) {
		t.Fatalf("bidi width must be order-independent: logical=%d reversed=%d", Width(bidi), Width(string(rev)))
	}
	// And an LTR run embedded in RTL adds exactly its own cells (the embedded ASCII "hello" = 5).
	if a, b := Width("قال hello"), Width("قال")+Width(" hello"); a != b {
		t.Fatalf("embedded LTR run width mismatch: whole=%d parts=%d", a, b)
	}
}

func TestTruncate(t *testing.T) {
	eAcute := "e" + string(cpAcute) // base+combining, width 1
	arabic := "مرحبا" + string(cpTanwin) + " يا عالم"
	arabicClip5 := "مرحبا" + string(cpTanwin) // 5 base cells + the 0-width tanwin
	cases := []struct {
		name     string
		s        string
		maxCells int
		want     string
	}{
		{"no clip", "score", 10, "score"},
		{"exact", "score", 5, "score"},
		{"clip ascii", "score", 3, "sco"},
		{"zero", "score", 0, ""},
		{"negative", "score", -1, ""},
		{"empty string", "", 5, ""},
		{"wide rune dropped whole at boundary", "A世B", 2, "A"}, // 世 is 2 cells; only 1 left → drop it whole
		{"wide rune fits", "A世B", 3, "A世"},
		{"never splits a multibyte rune", "café", 3, "caf"},
		// A 0-width combining mark following a fitting base rides ALONG (not dropped): base+U+0301 is width 1,
		// so Truncate(...,1) keeps the whole grapheme, then drops the trailing "x".
		{"combining mark rides along at the boundary", eAcute + "x", 1, eAcute},
		{"RTL clip keeps base+combining", arabic, 5, arabicClip5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.s, tc.maxCells); got != tc.want {
				t.Fatalf("Truncate(%q, %d) = %q, want %q", tc.s, tc.maxCells, got, tc.want)
			}
		})
	}
}

func TestPad(t *testing.T) {
	// A CJK-wide column, an ASCII column, and an RTL column all padded to 6 cells must have EQUAL display
	// width, even though their byte lengths differ — the alignment property score's grid needs.
	for _, s := range []string{"HP", "世界", "مرحبا"} {
		if got := Width(Pad(s, 6)); got != 6 {
			t.Fatalf("Pad(%q, 6) has display width %d, want 6 (byte len %d)", s, got, len(Pad(s, 6)))
		}
	}
	// Already-wide input is returned unchanged (never truncated).
	if got := Pad("toolongvalue", 4); got != "toolongvalue" {
		t.Fatalf("Pad must not truncate: got %q", got)
	}
	// Exact-width input gets no padding (the pad>0 guard boundary).
	if got := Pad("abc", 3); got != "abc" {
		t.Fatalf("Pad at exact width must not add spaces: got %q", got)
	}
	// Empty input pads to full width.
	if got := Pad("", 3); got != "   " {
		t.Fatalf("Pad(\"\", 3) = %q, want 3 spaces", got)
	}
}
