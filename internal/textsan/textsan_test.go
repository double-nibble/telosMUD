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
