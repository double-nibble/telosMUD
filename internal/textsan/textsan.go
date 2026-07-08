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
//
// The cap is applied TWICE on purpose. The inner cap bounds the WORK (stripControl
// never decodes more than MaxLineBytes of input). The outer cap bounds the OUTPUT:
// stripControl's slow path range-decodes invalid UTF-8 to the 3-byte U+FFFD, so a
// capped-but-hostile line of invalid bytes (e.g. a control rune followed by 4 KiB of
// 0xDA) would otherwise EXPAND past MaxLineBytes — a fuzzer (FuzzTextsan) found this.
// Re-capping after the strip restores the documented byte bound. Cost is nil on the
// clean fast path (both caps and the strip are unallocated no-ops for in-bounds text).
//
// INVARIANT (#156): CleanLine deliberately PRESERVES a raw invalid byte (e.g. an 8-bit C1) on its fast path
// for edge-parity. That is safe ONLY because every cross-player egress of input-derived text goes through the
// gate's sanitizeOutput (the universal last-line drop), never a verbatim sink — a future path that writes an
// input-derived string straight to a socket (WriteScreen or any non-Write path) would reopen the C1 hole.
func CleanLine(s string) string {
	return capBytes(stripControl(capBytes(s, MaxLineBytes)), MaxLineBytes)
}

// CleanName makes an externally-sourced display name (e.g. a cross-shard handoff
// snapshot's Name) safe to render and to use as a targeting keyword: it drops every
// control and non-graphic rune AND every raw invalid UTF-8 byte (#21 — so the name is
// always valid UTF-8 for its render/targeting/NATS-subject sinks) and caps the result
// at maxRunes on a rune boundary (a multi-rune grapheme may be cut, a rune never is). It is the world-side
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

// CleanMarkup makes SCRIPT/content-SUPPLIED outbound markup safe to deliver to a player client (builder Lua
// is a separate trust boundary). It strips every control rune AND every raw invalid byte via stripOutputControl
// — the terminal-injection vector a non-telnet sink (GMCP, the ANSI renderer that stops stripping ESC) would
// otherwise pass through to other players' clients — while PRESERVING all printable runes, so the engine's
// markup survives intact: color tokens, the act() '$'-referents ($n/$N/$t/...), and ordinary punctuation are
// printable characters, never control. It also caps the result at MaxLineBytes (defense in depth against an
// over-long broadcast fanning out per room occupant). Apply this ONLY to script-supplied args; engine-generated
// text is already safe. A clean, in-bounds string is returned unchanged and unallocated.
//
// Unlike CleanLine — the INPUT edge-parity contract, which preserves/normalizes invalid bytes — the OUTPUT
// path DROPS raw invalid bytes: a lone 8-bit C1 introducer (0x9B CSI / 0x9D OSC / 0x9C ST) is invalid UTF-8
// that a rune-level strip would pass verbatim onto a terminal, a control-injection vector (#156). Because
// stripOutputControl only ever drops or copies whole runes (it never expands invalid bytes to U+FFFD the way
// the input-side stripControl does), the outer cap is now merely belt-and-suspenders here.
func CleanMarkup(s string) string {
	return capBytes(stripOutputControl(capBytes(s, MaxLineBytes)), MaxLineBytes)
}

// capBytes truncates s to at most max bytes, backing off to the nearest rune
// boundary so a multibyte rune is never split. A string already within the limit is
// returned unchanged.
func capBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit]
}

// capRunes truncates s to at most max runes. A string already within the limit is
// returned unchanged.
func capRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	n := 0
	for i := range s {
		if n == limit {
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
// is never dropped and never panics. This is the INPUT contract (mirrors the edge's
// telnet.sanitizeLine); OUTPUT-bound markup uses the byte-aware stripOutputControl,
// which DROPS raw invalid bytes because a raw 8-bit C1 introducer is a wire-injection
// vector there (#156).
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

// IsBidiOverride reports whether r is one of the explicit Unicode bidirectional FORMATTING controls that
// enable "Trojan Source"-style visual spoofing (CVE-2021-42574, #22): the embedding/override block
// U+202A–U+202E (LRE/RLE/PDF/LRO/RLO) and the isolate block U+2066–U+2069 (LRI/RLI/FSI/PDI). They reorder
// the VISUAL run of surrounding text independently of its logical byte order, so a name or message can be
// made to DISPLAY as something other than what it is. This is deliberately a NARROW subset of category Cf:
// the IMPLICIT marks legitimate mixed-direction text needs (LRM U+200E, RLM U+200F, ALM U+061C) and the
// zero-width joiners scripts/emoji need (ZWNJ U+200C, ZWJ U+200D) are Cf too but are NOT spoofing controls,
// so they are PRESERVED — the fix neutralizes the abuse without breaking legitimate Arabic/Hebrew/emoji.
func IsBidiOverride(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// NeutralizeBidi drops every explicit bidi-override control (IsBidiOverride) from s, leaving all other runes
// — including legitimate implicit bidi marks and zero-width joiners — intact. It is for OUTPUT sinks that
// BYPASS the control-strip chokepoints, notably GMCP JSON payloads: JSON-escaping makes a leaked bidi control
// wire-safe but does NOT strip its DISPLAY effect, so a rich client rendering Comm.Channel text/talker would
// still be spoofable (#22). The clean common case is returned unchanged and unallocated. NOTE: when an
// override IS present (slow path), the rune-range rebuild normalizes any lone invalid UTF-8 byte in s to
// U+FFFD, so output is not strictly byte-preserving for malformed input — a non-issue for its GMCP callers,
// whose fields are valid Go strings that json.Marshal normalizes downstream.
func NeutralizeBidi(s string) string {
	if !strings.ContainsFunc(s, IsBidiOverride) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if IsBidiOverride(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// badOutputRune is the OUTPUT drop-set: control runes AND the explicit bidi-override subset. Raw invalid
// bytes are handled separately (byte-level) by stripOutputControl.
func badOutputRune(r rune) bool { return unicode.IsControl(r) || IsBidiOverride(r) }

// stripOutputControl is the OUTPUT-side counterpart of stripControl: it drops every control rune, every
// explicit bidi-override control (#22), AND every raw invalid byte. It is BYTE-aware, not merely rune-aware:
// a lone invalid UTF-8 byte — in particular a raw 8-bit C1 introducer (0x9B CSI / 0x9D OSC / 0x9C ST) —
// decodes to a NON-control RuneError, so a rune-level unicode.IsControl check (as stripControl uses) would
// let it pass verbatim onto the OUTPUT wire, a sanitizer-bypassing terminal-control-injection vector (#156:
// 8-bit CSI erase/cursor, DSR/DA cursor-report input-injection, OSC-52 clipboard exfil). The fast path
// therefore also requires utf8.ValidString, and the rewrite DROPS invalid bytes outright (mirroring
// world/luascreen.sanitizeScreenText, the byte-aware model on the trusted screen path). The clean common
// case (valid UTF-8, no control/bidi-override rune) is returned unallocated.
func stripOutputControl(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, badOutputRune) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++ // a raw invalid byte (8-bit C1 introducer / 0xFF) — drop it; never onto the wire
			continue
		}
		if !badOutputRune(r) {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// stripNonGraphic drops every control OR non-printable rune from s — the same reject set the gate's
// validateName uses (unicode.IsControl || !unicode.IsPrint) — AND every raw invalid UTF-8 byte. It is
// BYTE-aware, not merely rune-aware (the #156 split, applied to the name path): a lone invalid byte decodes
// to U+FFFD, which IS printable, so a rune-level filter (`for _, r := range s`) would judge the string clean
// and PRESERVE the raw byte verbatim (#21 found this via the fuzz). A display name feeds output sinks that
// must be valid UTF-8 — the telnet/GMCP render and the tell NATS-subject token — so the fast path now
// requires utf8.ValidString and the rewrite drops raw invalid bytes outright. The clean common case (valid
// UTF-8, all-graphic) is returned unallocated.
func stripNonGraphic(s string) string {
	bad := func(r rune) bool { return unicode.IsControl(r) || !unicode.IsPrint(r) }
	if utf8.ValidString(s) && !strings.ContainsFunc(s, bad) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++ // a raw invalid byte — drop it; a display name must be valid UTF-8
			continue
		}
		if !bad(r) {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}
