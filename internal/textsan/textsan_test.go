package textsan

import (
	"strings"
	"testing"
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
	zwsp := "Wal" + "​" + "ker" //nolint:staticcheck // intentional U+200B zero-width-space fixture for the sanitizer
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean passthrough", "Walker", "Walker"},
		{"strips control runes", "Wa\x1blker\x07", "Walker"},
		{"strips non-printable", zwsp, "Walker"},
		{"keeps accented letters", "Élodie", "Élodie"},
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
