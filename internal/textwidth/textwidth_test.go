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
