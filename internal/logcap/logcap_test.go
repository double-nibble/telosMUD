package logcap

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValue(t *testing.T) {
	// Short value: returned unchanged.
	if got := Value("hello"); got != "hello" {
		t.Errorf("short value altered: %q", got)
	}
	// At the boundary: unchanged (no marker).
	exact := strings.Repeat("a", MaxValueBytes)
	if got := Value(exact); got != exact {
		t.Errorf("value at the cap was altered")
	}
	// One byte over: truncated, marker appended, body within the cap.
	over := strings.Repeat("a", MaxValueBytes+1)
	got := Value(over)
	if !strings.HasSuffix(got, TruncationMarker) {
		t.Errorf("truncated value must end with the marker; got tail %q", got[len(got)-20:])
	}
	if body := strings.TrimSuffix(got, TruncationMarker); len(body) > MaxValueBytes {
		t.Errorf("truncated body %d bytes exceeds cap %d", len(body), MaxValueBytes)
	}

	// A huge value (the actual threat: a ~200KB builder field) is bounded to cap+marker, nothing more.
	huge := strings.Repeat("a", 200*1024)
	g := Value(huge)
	if len(g) > MaxValueBytes+len(TruncationMarker) {
		t.Errorf("huge value not bounded: got %d bytes, want <= %d", len(g), MaxValueBytes+len(TruncationMarker))
	}

	// Multibyte: truncation never splits a rune (valid UTF-8 out).
	multi := strings.Repeat("界", MaxValueBytes) // 3 bytes each, well over the cap
	if g := Value(multi); !utf8.ValidString(g) {
		t.Errorf("Value split a multibyte rune (invalid UTF-8)")
	}
}
