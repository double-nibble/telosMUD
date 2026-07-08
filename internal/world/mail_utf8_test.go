package world

import (
	"testing"
	"unicode/utf8"
)

// mail_utf8_test.go — docs/REMAINING.md Track-0 (#21 item 4): the mail SUBJECT rune cap (mailcmds.capRunes,
// applied after textsan.CleanLine, which strips control runes but PRESERVES Cf joiners) must be RUNE-safe —
// it truncates on a rune boundary, never splitting a multibyte rune — even when the cap lands mid-GRAPHEME,
// including inside an emoji ZWJ cluster (which, unlike the name path, survives CleanLine and reaches capRunes).
// A cut grapheme is acceptable; invalid UTF-8 is not.
func TestMailCapRunesIsRuneSafeAcrossGraphemes(t *testing.T) {
	combining := "e\u0301"                                 // é: base + combining mark (2 runes, 1 grapheme)
	family := "\U0001F468\u200d\U0001F469\u200d\U0001F467" // man ZWJ woman ZWJ girl (5 runes, 1 grapheme)
	s := combining + family + " subject"
	for n := 0; n <= 10; n++ {
		got := capRunes(s, n)
		if rc := utf8.RuneCountInString(got); rc > n {
			t.Fatalf("capRunes(%d) exceeded rune cap: %d runes in %q", n, rc, got)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("capRunes(%d) produced invalid UTF-8 (a rune was split): %q", n, got)
		}
	}
	// Cutting INSIDE the ZWJ family cluster is rune-safe (valid UTF-8) even though it splits the grapheme:
	// two runes out (man + ZWJ), a truncated-but-valid cluster.
	if got := capRunes(family, 2); !utf8.ValidString(got) || utf8.RuneCountInString(got) != 2 {
		t.Fatalf("capRunes(family,2) = %q; want exactly 2 valid runes (grapheme split acceptable)", got)
	}
}
