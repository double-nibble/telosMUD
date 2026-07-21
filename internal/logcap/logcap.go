// Package logcap bounds the byte length of a builder-authored value before it is interpolated into a
// log line or an error message. Semi-trusted content — Lua bodies (#456) and the content DTOs the
// pack loader parses (#481) — can otherwise drive an arbitrarily long string into the process log
// (and, once container stdout ships into Loki, into a durable, indexed observability store). Left
// unbounded that is a log-poisoning / disk-fill DoS against the whole node (the game shares it) and a
// way to bury real signal during an incident.
//
// This is a ZERO-DEPENDENCY leaf so the two very different callers can share ONE bound without a
// dependency edge between them: the low-level content loader (internal/content) must not pull in the
// Lua sandbox (and its gopher-lua fork) just to cap a string, and the sandbox must not pull in
// content. luasandbox.CapLogMsg delegates here so "the bound is identical everywhere" is enforced by
// shared code, not by convention.
package logcap

import "unicode/utf8"

// MaxValueBytes caps a single builder-authored value. ~1KB is generous for a human-readable
// diagnostic (a ref, a dice string, a formula head, one parse error); anything larger is a report the
// log is the wrong channel for. It is the same bound the Lua log sinks use (#456) so a content author
// meets one rule regardless of which subsystem rejected their input.
const MaxValueBytes = 1024

// TruncationMarker is appended to a value clipped by Value so a reader can tell the line was cut
// rather than genuinely ending there. Exported so a delegating caller (luasandbox) can assert on it.
const TruncationMarker = "…[truncated]"

// Value clamps s to at most MaxValueBytes bytes (plus the truncation marker), never splitting a UTF-8
// rune. A string already within the bound is returned unchanged (no allocation).
func Value(s string) string {
	if len(s) <= MaxValueBytes {
		return s
	}
	cut := MaxValueBytes
	// Back up to a rune boundary so a multibyte rune is never sliced mid-sequence.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + TruncationMarker
}
