package textsan

import (
	"testing"
	"unicode"
	"unicode/utf8"
)

// textsan_fuzz_test.go is the first fuzz target in the repo (W6). textsan is the ideal seed: it is the
// world's gRPC-ingress text-safety boundary (a buggy/compromised gate could deliver un-sanitized text),
// it is pure and dependency-free, and it DELIBERATELY handles adversarial input — invalid UTF-8, control
// runes, over-length lines, multibyte runes — so its contract is exactly a set of properties a fuzzer
// can hammer. The seed corpus runs hermetically under `go test` every commit; the long `-fuzz` run goes
// to the nightly tier.
//
// The invariants pinned for ALL three Clean entrypoints, on ANY input:
//   - NO PANIC (the headline — a single malformed line must never crash the zone goroutine at ingress).
//   - CAPS respected: CleanLine/CleanMarkup ≤ MaxLineBytes; CleanName ≤ maxRunes runes.
//   - SANITIZED: CleanLine/CleanMarkup output is control-rune-free; CleanName output is graphic-only
//     (no control, no non-print).
//   - IDEMPOTENT: Clean(Clean(x)) == Clean(x) — the output is a fixed point (re-cleaning is a no-op),
//     the property that lets the world re-apply the cap+strip at its boundary without drift.
//
// NOTE on invalid UTF-8: the two paths diverge (#119/#156). CleanLine (the INPUT edge-parity path) deliberately
// PRESERVES a lone invalid byte verbatim, so its output is NOT guaranteed valid UTF-8 — we do not assert that.
// CleanMarkup (the OUTPUT path) DROPS raw invalid bytes, because a lone 8-bit C1 introducer (0x9B CSI / 0x9D OSC
// / 0x9C ST) is invalid UTF-8 that a rune-level scan judges non-control (it decodes to U+FFFD) yet is a live
// terminal-injection vector on the wire. The rune-level control-free assertion below is BLIND to that survival
// (U+FFFD is printable, non-control), so CleanMarkup additionally gets a byte-level utf8.ValidString invariant —
// the property that a raw C1 byte can never reach a player client through the normal output path.
func FuzzTextsan(f *testing.F) {
	seeds := []struct {
		s string
		n int
	}{
		{"hello world", 16},
		{"name\x1b[31mwith an ANSI escape", 8},
		{"tab\tand\nnewline\rand a \x00 NUL", 32},
		{"emoji 🜂 and accents café résumé", 4},
		{"\xff\xfe invalid utf8 bytes \x80", 10},
		{"zero\u200bwidth\u200band\ufefffmt", 12}, // non-print, non-control format runes (ZWSP, BOM)
		{"$n waves at $N with {color} markup", 64},
		{"", 0},
		{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 3},
	}
	for _, sd := range seeds {
		f.Add(sd.s, sd.n)
	}

	f.Fuzz(func(t *testing.T, s string, n int) {
		// Bound maxRunes to a sane non-negative range (capRunes handles n<=0 as ""; the rune-cap
		// assertion below needs n>=0 to be meaningful).
		if n < 0 {
			n = -n
		}
		n %= MaxLineBytes

		line := CleanLine(s)
		markup := CleanMarkup(s)
		name := CleanName(s, n)

		// CAPS.
		if len(line) > MaxLineBytes {
			t.Fatalf("CleanLine exceeded the byte cap: %d > %d", len(line), MaxLineBytes)
		}
		if len(markup) > MaxLineBytes {
			t.Fatalf("CleanMarkup exceeded the byte cap: %d > %d", len(markup), MaxLineBytes)
		}
		if rc := utf8.RuneCountInString(name); rc > n {
			t.Fatalf("CleanName exceeded the rune cap: %d runes > %d", rc, n)
		}

		// SANITIZED.
		for _, r := range line {
			if unicode.IsControl(r) {
				t.Fatalf("CleanLine left a control rune %U in %q", r, line)
			}
		}
		for _, r := range markup {
			if unicode.IsControl(r) {
				t.Fatalf("CleanMarkup left a control rune %U in %q", r, markup)
			}
		}
		// C1 SURVIVAL (#119/#156): the rune-range control check above is blind to a raw 8-bit C1 byte (it
		// decodes to non-control U+FFFD), so assert at the byte level that CleanMarkup output is valid UTF-8 —
		// stripOutputControl drops every raw invalid byte, so no 0x80-0x9F C1 introducer can survive to the wire.
		if !utf8.ValidString(markup) {
			t.Fatalf("CleanMarkup left a raw invalid byte (C1-survival vector) in %q", markup)
		}
		for _, r := range name {
			if unicode.IsControl(r) || !unicode.IsPrint(r) {
				t.Fatalf("CleanName left a non-graphic rune %U in %q", r, name)
			}
		}

		// IDEMPOTENT (fixed point).
		if got := CleanLine(line); got != line {
			t.Fatalf("CleanLine not idempotent: %q -> %q", line, got)
		}
		if got := CleanMarkup(markup); got != markup {
			t.Fatalf("CleanMarkup not idempotent: %q -> %q", markup, got)
		}
		if got := CleanName(name, n); got != name {
			t.Fatalf("CleanName not idempotent: %q -> %q", name, got)
		}
	})
}
