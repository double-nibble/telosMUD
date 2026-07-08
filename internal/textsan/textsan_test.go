package textsan

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCleanLine(t *testing.T) {
	nel := "a" + "" + "b" //nolint:staticcheck // intentional U+0085 NEL control-rune fixture for the sanitizer
	bad := "ab\xffcd"      // 0xff is invalid UTF-8
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean passthrough", "say hello", "say hello"},
		{"strips esc and bel", "say hel\x1blo\x07", "say hello"},
		{"strips c0 controls", "a\x00b\x01c", "abc"},
		{"strips c1 control (NEL)", nel, "ab"},
		{"keeps multibyte runes", "say héllo café 😀", "say héllo café 😀"},
		{"invalid utf8 preserved on clean fast path", bad, "ab\xffcd"},
		{"invalid utf8 normalized when rewriting", "ab\xff\x07cd", "ab�cd"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CleanLine(tt.in); got != tt.want {
				t.Fatalf("CleanLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCleanMarkup proves the script-supplied-markup cleaner strips control/ESC sequences
// (terminal-injection defense) while PRESERVING legitimate markup: color tokens, act()
// '$'-referents, punctuation, and multibyte runes all survive — only control runes are dropped.
func TestCleanMarkup(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean markup passthrough", "{red}Hello{x} $n waves", "{red}Hello{x} $n waves"},
		{"strips esc sequence", "before\x1b[31mred\x1b[0mafter", "before[31mred[0mafter"},
		{"strips bare ESC", "a\x1bb", "ab"},
		{"strips bel + c0", "ding\x07\x00 done", "ding done"},
		{"preserves dollar referents", "$n says '$t' to $N", "$n says '$t' to $N"},
		{"preserves color + punctuation", "{g}+5 HP!{x} (50%)", "{g}+5 HP!{x} (50%)"},
		{"keeps multibyte", "café 😀 héllo", "café 😀 héllo"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CleanMarkup(tt.in); got != tt.want {
				t.Fatalf("CleanMarkup(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCleanMarkupCaps proves CleanMarkup caps an over-long broadcast at MaxLineBytes.
func TestCleanMarkupCaps(t *testing.T) {
	in := strings.Repeat("A", MaxLineBytes*2)
	if got := CleanMarkup(in); len(got) != MaxLineBytes {
		t.Fatalf("CleanMarkup over-long len = %d, want %d", len(got), MaxLineBytes)
	}
}

// TestCleanMarkupStripsRawC1 is the #156 regression: CleanMarkup (the OUTPUT-bound sanitizer) must DROP raw
// 8-bit C1 introducers. A lone 0x9B (CSI) / 0x9D (OSC) / 0x9C (ST) is INVALID utf-8 that decodes to a
// non-control RuneError, so a rune-level strip preserves it verbatim — letting content inject terminal
// control (screen erase / cursor move, DSR cursor-report INPUT INJECTION, OSC-52 clipboard exfil) straight
// onto the wire. CleanMarkup drops it; CleanLine (the INPUT edge-parity contract) deliberately preserves it.
func TestCleanMarkupStripsRawC1(t *testing.T) {
	for _, b := range []byte{0x9b, 0x9d, 0x9c, 0x90, 0x9f} { // samples across the C1 range 0x80-0x9F
		in := "hi" + string([]byte{b}) + "2J"
		got := CleanMarkup(in)
		if strings.IndexByte(got, b) >= 0 {
			t.Errorf("CleanMarkup must drop raw C1 byte %#x; got %q", b, got)
		}
		if got != "hi2J" {
			t.Errorf("CleanMarkup(%q) = %q, want %q (C1 dropped, rest intact)", in, got, "hi2J")
		}
	}
	// Valid multibyte text is preserved; only the raw C1 is dropped.
	if got := CleanMarkup("Élodie" + string([]byte{0x9b}) + "x"); got != "Élodiex" {
		t.Fatalf("CleanMarkup should drop only the raw C1 and keep valid multibyte text; got %q", got)
	}
	// Contrast: CleanLine (the INPUT contract) PRESERVES the same raw byte on its fast path (edge-parity) —
	// the split is deliberate, so this asymmetry is the point.
	if raw := "hi" + string([]byte{0x9b}) + "x"; CleanLine(raw) != raw {
		t.Fatalf("CleanLine should preserve a raw invalid byte (input edge-parity contract); got %q", CleanLine(raw))
	}
}

// TestCleanLineCaps proves the byte cap holds at the world's own ingress, mirroring
// the edge's MaxLineBytes — a producer that skipped the edge cannot deliver an
// unbounded line.
func TestCleanLineCaps(t *testing.T) {
	in := strings.Repeat("a", MaxLineBytes+1000)
	got := CleanLine(in)
	if len(got) != MaxLineBytes {
		t.Fatalf("CleanLine over-long: len = %d, want %d", len(got), MaxLineBytes)
	}
}

// TestIsBidiOverride pins the exact Trojan-Source override subset (#22): only U+202A–U+202E and
// U+2066–U+2069 are overrides; the block boundaries and the legitimate implicit marks/joiners are NOT.
func TestIsBidiOverride(t *testing.T) {
	override := []rune{0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069}
	for _, r := range override {
		if !IsBidiOverride(r) {
			t.Errorf("IsBidiOverride(%U) = false, want true (it is a Trojan-Source override)", r)
		}
	}
	// Boundaries just outside each block, plus the implicit marks (LRM/RLM/ALM) and joiners (ZWJ/ZWNJ)
	// that legitimate Arabic/Hebrew/emoji need — none are overrides, all must be preserved.
	notOverride := []rune{0x2029, 0x202F, 0x2065, 0x206A, 0x200E, 0x200F, 0x061C, 0x200C, 0x200D, 'a', 0x0627 /* Arabic alef */}
	for _, r := range notOverride {
		if IsBidiOverride(r) {
			t.Errorf("IsBidiOverride(%U) = true, want false (legitimate / out-of-range)", r)
		}
	}
}

// TestNeutralizeBidiDropsOverridesKeepsLegit is the #22 core: the explicit bidi-override controls are
// dropped from OUTPUT while every legitimate rune — implicit marks, zero-width joiners, actual RTL letters —
// survives, so Arabic/Hebrew/emoji still render and only the spoofing vector is neutralized.
func TestNeutralizeBidiDropsOverridesKeepsLegit(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"RLO override dropped", "user\u202eadmin", "useradmin"},
		{"LRO override dropped", "a\u202db", "ab"},
		{"isolate pair dropped", "\u2066spoof\u2069rest", "spoofrest"},
		{"all nine dropped", "\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069x", "x"},
		{"legitimate Arabic preserved", "مرحبا hello", "مرحبا hello"},
		{"implicit LRM/RLM marks preserved", "a\u200e\u200fb", "a\u200e\u200fb"},
		{"emoji ZWJ sequence preserved", "\U0001F468\u200d\U0001F469", "\U0001F468\u200d\U0001F469"},
		{"clean passthrough", "just normal text", "just normal text"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NeutralizeBidi(tt.in); got != tt.want {
				t.Fatalf("NeutralizeBidi(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestCleanMarkupStripsBidiOverride proves the OUTPUT-markup sanitizer (the content->player boundary) also
// drops the override subset — so script-supplied act() args cannot smuggle a Trojan-Source spoof — while the
// legitimate RTL text and joiners the same author might use survive.
func TestCleanMarkupStripsBidiOverride(t *testing.T) {
	if got := CleanMarkup("Guard\u202e says hi"); got != "Guard says hi" {
		t.Fatalf("CleanMarkup must drop the RLO override; got %q", got)
	}
	if got := CleanMarkup("السلام"); got != "السلام" {
		t.Fatalf("CleanMarkup must preserve legitimate Arabic; got %q", got)
	}
	if got := CleanMarkup("\U0001F468\u200d\U0001F469"); got != "\U0001F468\u200d\U0001F469" {
		t.Fatalf("CleanMarkup must preserve an emoji ZWJ sequence; got %q", got)
	}
}

// TestCleanNameCapIsRuneSafeAcrossGraphemes is the docs/REMAINING.md Track-0 (#21 item 4) guarantee for the
// rune-COUNT cap: CleanName truncates on a RUNE boundary, so its output is always valid UTF-8 even when the
// cap lands inside a multi-rune GRAPHEME cluster (a base letter + a combining mark). Cutting a grapheme is
// acceptable for a display-name cap; producing invalid UTF-8 (a split rune) is not. (CleanName strips ZWJ as
// Cf/non-graphic, so the emoji-cluster case does not apply on the NAME path — see the mail-subject cap test,
// TestMailCapRunesIsRuneSafeAcrossGraphemes, for the Cf-preserving path where it does.)
func TestCleanNameCapIsRuneSafeAcrossGraphemes(t *testing.T) {
	grapheme := "e\u0301" // e + COMBINING ACUTE ACCENT (U+0301): two runes, one grapheme
	s := grapheme + grapheme + grapheme + "tail"
	for n := 0; n <= 8; n++ {
		got := CleanName(s, n)
		if rc := utf8.RuneCountInString(got); rc > n {
			t.Fatalf("CleanName cap %d exceeded: %d runes in %q", n, rc, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("CleanName cap %d produced invalid UTF-8 (a rune was split): %q", n, got)
		}
	}
	// A cap that lands between the base 'e' and its combining mark drops the mark — a cut grapheme, still
	// valid UTF-8: the documented, acceptable behavior of a rune-count cap.
	if got := CleanName(grapheme, 1); got != "e" {
		t.Fatalf("CleanName(%q,1) = %q; want the combining mark cut (rune-safe grapheme split)", grapheme, got)
	}
}

// TestCleanLineCapNeverSplitsRune confirms the byte cap backs off to a rune boundary
// rather than slicing a multibyte rune in half (which would corrupt the tail rune).
func TestCleanLineCapNeverSplitsRune(t *testing.T) {
	// "é" is two bytes. Build a line whose cap would otherwise land mid-rune.
	in := strings.Repeat("a", MaxLineBytes-1) + "é" + "tail"
	got := CleanLine(in)
	if len(got) > MaxLineBytes {
		t.Fatalf("len = %d exceeds cap %d", len(got), MaxLineBytes)
	}
	if !strings.HasSuffix(got, "a") {
		t.Fatalf("cap split a rune; got tail %q", got[len(got)-4:])
	}
}

func TestCleanName(t *testing.T) {
	const maxLen = 20
	zwsp := "Wal" + "\u200b" + "ker" //nolint:staticcheck // intentional U+200B zero-width-space fixture for the sanitizer
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean passthrough", "Walker", "Walker"},
		{"strips control runes", "Wa\x1blker\x07", "Walker"},
		{"strips non-printable", zwsp, "Walker"},
		{"keeps accented letters", "Élodie", "Élodie"},
		{"drops raw invalid utf8 bytes (#21)", "Wa\xfflke\x80r", "Walker"},
		{"drops invalid byte, keeps adjacent multibyte (#21)", "\xffÉlodie", "Élodie"},
		{"caps length", strings.Repeat("x", 50), strings.Repeat("x", maxLen)},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CleanName(tt.in, maxLen); got != tt.want {
				t.Fatalf("CleanName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
