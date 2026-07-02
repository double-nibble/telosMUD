package telnet

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// color_test.go — the EDGE integration of the `{{TOKEN}}` color layer: the full Write path (sanitizeOutput then
// renderColor). The render/coalesce/strip and injection-safety unit tests live in internal/colormarkup.

// sgrOnly matches text in which EVERY ESC begins a well-formed SGR (`ESC [ <digits/semicolons> m`) and nothing
// else — no cursor move, erase, scroll, bell, or device query is expressible.
var sgrOnly = regexp.MustCompile("^([^\x1b]|\x1b\\[[0-9;]*m)*$")

// TestWriteColorPipeline proves the full Write path: sanitizeOutput strips a player's RAW ESC FIRST, then
// renderColor adds only SGR — so a raw screen-erase a player types can never survive, while `{{TOKEN}}` color
// renders. This is the "downstream of the control-strip" ordering that makes the token layer injection-safe.
func TestWriteColorPipeline(t *testing.T) {
	var out bytes.Buffer
	c := NewReadWriter(&bytes.Buffer{}, &out) // color on by default
	// A hostile payload: a raw ESC erase-screen the player typed, plus a legit color token.
	if err := c.Write("\x1b[2Jboo {{FG_RED}}danger{{RESET}}"); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	// The raw ESC (and thus the erase-screen CSI) was stripped by sanitizeOutput; the leftover "[2J" is inert
	// literal text. The only ESC present is the SGR from the token.
	if strings.ContainsRune(got, 0x1b) && !sgrOnly.MatchString(got) {
		t.Fatalf("raw ESC survived Write (injection): %q", got)
	}
	if !strings.Contains(got, "\x1b[31mdanger\x1b[0m") {
		t.Fatalf("color token did not render through Write: %q", got)
	}
	if strings.Contains(got, "\x1b[2J") {
		t.Fatalf("player's raw erase-screen escape survived: %q", got)
	}
}

// TestWriteColorOffStrips proves `color off` yields clean plain text (tokens removed, no ESC at all).
func TestWriteColorOffStrips(t *testing.T) {
	var out bytes.Buffer
	c := NewReadWriter(&bytes.Buffer{}, &out)
	c.SetColor(false)
	if err := c.Write("{{FG_RED}}hello{{RESET}} world"); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "hello world" {
		t.Fatalf("color off: got %q, want %q", got, "hello world")
	}
}
