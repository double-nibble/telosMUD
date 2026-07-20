package world

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// rawinput_log_test.go — #454. Verbatim player input (tells, chat, a mistyped link code) must not
// escape into the logs by default. The three sites that carry the raw line — the pre-dispatch
// "inbox: input" (zone.go), the resolved-verb "dispatch" (parser.go), and the panic-recovery
// Error line (zone.go) — attach the line ONLY under the explicit TELOS_LOG_RAW_INPUT opt-in, which
// is deliberately separate from DEBUG. These tests drive the real dispatch path and assert the
// secret is absent by default and present under the opt-in.

const rawSecret = "my-link-code-8F3K2" // stands in for a tell body / a mistyped credential

// captureZoneLog swaps z.log for a Debug-level text handler writing to buf, so every line the zone
// emits is observable. It returns buf for inspection.
func captureZoneLog(z *Zone) *bytes.Buffer {
	var buf bytes.Buffer
	z.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

// TestRawInputRedactedByDefault: with logRawInput off (the default), neither the pre-dispatch site
// nor the resolved-verb dispatch site may emit the raw line — for an UNKNOWN verb (only the
// pre-dispatch site fires) or a RESOLVED verb (both fire).
func TestRawInputRedactedByDefault(t *testing.T) {
	for _, line := range []string{
		"frobnicate " + rawSecret, // unknown verb -> pre-dispatch site only
		"look " + rawSecret,       // resolved verb -> pre-dispatch + dispatch sites
		"say " + rawSecret,        // resolved verb with a body (the tell/chat case)
	} {
		z, _, room := harmZone(t)
		z.logRawInput = false
		buf := captureZoneLog(z)
		harmPlayer(z, room, "Alice")

		z.handleInput(inputMsg{id: "Alice", line: line})

		if got := buf.String(); strings.Contains(got, rawSecret) {
			t.Errorf("raw input leaked into logs for %q (opt-in OFF):\n%s", line, got)
		}
		// The flow markers must still be present — redaction keeps the diagnostic, drops the body.
		if got := buf.String(); !strings.Contains(got, "inbox: input") {
			t.Errorf("pre-dispatch flow log missing for %q; redaction dropped too much:\n%s", line, got)
		}
	}
}

// TestRawInputLoggedUnderOptIn: with logRawInput on, the raw line reappears at both sites — the
// escape hatch for a local bug hunt still works.
func TestRawInputLoggedUnderOptIn(t *testing.T) {
	// Unknown verb: only the pre-dispatch site carries the line.
	z, _, room := harmZone(t)
	z.logRawInput = true
	buf := captureZoneLog(z)
	harmPlayer(z, room, "Alice")
	z.handleInput(inputMsg{id: "Alice", line: "frobnicate " + rawSecret})
	if got := buf.String(); !strings.Contains(got, rawSecret) {
		t.Errorf("opt-in ON: pre-dispatch site should log the raw line, got:\n%s", got)
	}

	// Resolved verb: the dispatch site carries the line too.
	z2, _, room2 := harmZone(t)
	z2.logRawInput = true
	buf2 := captureZoneLog(z2)
	harmPlayer(z2, room2, "Bob")
	z2.handleInput(inputMsg{id: "Bob", line: "look " + rawSecret})
	if got := buf2.String(); !strings.Contains(got, rawSecret) {
		t.Errorf("opt-in ON: dispatch site should log the raw line, got:\n%s", got)
	}
}

// TestNewZoneWiresRawInputFromEnv: construction reads the opt-in from TELOS_LOG_RAW_INPUT (and
// nothing else — DEBUG must not enable it). This is what wires obs.LogRawInput() onto the zone.
func TestNewZoneWiresRawInputFromEnv(t *testing.T) {
	t.Setenv("DEBUG", "1")
	t.Setenv("TELOS_LOG_RAW_INPUT", "")
	if z := newZone("wire-off"); z.logRawInput {
		t.Error("DEBUG=1 alone must not enable logRawInput at construction")
	}

	t.Setenv("TELOS_LOG_RAW_INPUT", "1")
	if z := newZone("wire-on"); !z.logRawInput {
		t.Error("TELOS_LOG_RAW_INPUT=1 should enable logRawInput at construction")
	}
}

// panicVerb454 is a hidden test-only command that panics, used to drive dispatchSafe's recovery
// path. Registered once into the base table (guarded so a repeated in-process test run is a no-op).
const panicVerb454 = "panicnow454"

func init() {
	if _, ok := baseTable.byExact[panicVerb454]; !ok {
		baseTable.register(&Command{
			Name:  panicVerb454,
			Flags: CmdHidden,
			Run:   func(*Context) error { panic("boom-" + rawSecret) },
		})
	}
}

// TestPanicPathRedactsRawInput: when a command handler panics, dispatchSafe logs the crash at
// Error. The raw input line (verbatim player text) must be gated behind the opt-in, but the panic
// must remain fully diagnosable (player + panic value + stack) regardless.
func TestPanicPathRedactsRawInput(t *testing.T) {
	// The panic value itself embeds rawSecret so we can prove the *line* redaction independently:
	// we search for the input-line marker, not the panic string. Use a distinct token for the line.
	const lineSecret = "line-body-QQ9"

	// Opt-in OFF: the line must not appear, but the stack/player must.
	z, _, room := harmZone(t)
	z.logRawInput = false
	buf := captureZoneLog(z)
	harmPlayer(z, room, "Alice")
	z.dispatchSafe(z.players["Alice"], panicVerb454+" "+lineSecret)
	off := buf.String()
	if strings.Contains(off, lineSecret) {
		t.Errorf("panic path leaked the raw input line (opt-in OFF):\n%s", off)
	}
	if !strings.Contains(off, "panicked") || !strings.Contains(off, "stack") {
		t.Errorf("panic path dropped the diagnostic (need panic+stack):\n%s", off)
	}

	// Opt-in ON: the line reappears.
	z2, _, room2 := harmZone(t)
	z2.logRawInput = true
	buf2 := captureZoneLog(z2)
	harmPlayer(z2, room2, "Bob")
	z2.dispatchSafe(z2.players["Bob"], panicVerb454+" "+lineSecret)
	if on := buf2.String(); !strings.Contains(on, lineSecret) {
		t.Errorf("panic path should log the raw line under the opt-in:\n%s", on)
	}
}
