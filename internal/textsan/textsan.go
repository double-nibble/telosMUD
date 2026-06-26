// Package textsan holds the small, dependency-free text-safety primitives the world
// applies to externally-sourced text at its gRPC trust boundary. The canonical
// chokepoint for player input is the edge (internal/telnet sanitizes+caps every
// line; internal/gate validates the login name), but the world is a *separate trust
// domain* reachable over gRPC: a compromised/buggy gate, or a future direct-shard
// client, could deliver un-sanitized text. These helpers let the world re-apply a
// cheap cap + control-rune strip at ingress without reaching across into the edge
// package (internal/world must NOT import internal/telnet — that would invert the
// edge/world layering). They mirror, but do not share, the edge's sanitizeLine /
// validateName so the two trust domains stay independently defensible.
//
// Everything here is defense-in-depth: behavior-preserving for legitimate input
// (the common clean case is returned unchanged and unallocated) and only visibly
// active against malformed/hostile text.
package textsan

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLineBytes mirrors internal/telnet.MaxLineBytes: the hard ceiling on a single
// input line. It is duplicated here (rather than imported) because the world must
// not depend on the edge; it is the same protocol-level safety limit applied a
// second time at the world's own boundary. 4 KiB comfortably exceeds any legitimate
// command while bounding anything that fans out per room occupant downstream.
const MaxLineBytes = 4096

// CleanLine makes one externally-sourced input line safe to post to a zone inbox:
// it caps the line at MaxLineBytes (on a rune boundary) and strips control runes.
// This is the world-side mirror of the edge's telnet sanitizeLine, applied at the
// gRPC ingress so a producer that skipped the edge cannot deliver an unbounded or
// control-laden line. A clean, in-bounds line is returned unchanged.
func CleanLine(s string) string {
	return stripControl(capBytes(s, MaxLineBytes))
}

// CleanName makes an externally-sourced display name (e.g. a cross-shard handoff
// snapshot's Name) safe to render and to use as a targeting keyword: it drops every
// control and non-graphic rune and caps the result at maxRunes. It is the world-side
// mirror of the gate's validateName, but it *sanitizes* rather than *rejects* — at a
// handoff there is no user to re-prompt, so dropping the offending runes is the
// behavior-preserving choice. Grammar-level rules the gate also enforces (no leading
// dot/digit, no embedded dot) are deliberately NOT replicated: those guard targeting
// disambiguation, not terminal injection, and silently rewriting them would alter a
// legitimate-but-unusual name more than it protects anything. A clean, in-bounds name
// is returned unchanged.
func CleanName(s string, maxRunes int) string {
	return capRunes(stripNonGraphic(s), maxRunes)
}

// capBytes truncates s to at most max bytes, backing off to the nearest rune
// boundary so a multibyte rune is never split. A string already within the limit is
// returned unchanged.
func capBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// capRunes truncates s to at most max runes. A string already within the limit is
// returned unchanged.
func capRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}

// stripControl drops every control rune from s. It is UTF-8 aware (rune-level, so
// multibyte runes survive) and short-circuits the clean common case without
// allocating. Invalid UTF-8 is never a control rune, so it never triggers a rewrite
// on its own: a lone invalid byte is preserved verbatim on the fast path. If some
// other control rune does trigger the rewrite, the range-decode normalizes any
// invalid byte in s to the U+FFFD replacement character. Either way an invalid byte
// is never dropped and never panics. (This matches the edge's telnet.sanitizeLine.)
func stripControl(s string) string {
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// stripNonGraphic drops every control OR non-printable rune from s — the same reject
// set the gate's validateName uses (unicode.IsControl || !unicode.IsPrint), applied
// here as a filter. UTF-8 aware; the clean common case is returned unallocated.
func stripNonGraphic(s string) string {
	bad := func(r rune) bool { return unicode.IsControl(r) || !unicode.IsPrint(r) }
	if !strings.ContainsFunc(s, bad) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if bad(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
