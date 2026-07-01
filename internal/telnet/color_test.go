package telnet

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// color_test.go — the `{{TOKEN}}` ANSI color layer (docs/REMAINING.md Track 1). Pins the render/strip/coalesce
// behavior AND the security guarantee: no token combination can produce anything but a well-formed SGR.

func TestRenderColorBasics(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		enabled bool
		want    string
	}{
		{"single fg", "{{FG_RED}}hi", true, "\x1b[31mhi\x1b[0m"},
		{"explicit reset, no auto double", "{{FG_RED}}hi{{RESET}}", true, "\x1b[31mhi\x1b[0m"},
		{"coalesce adjacent", "{{FG_RED}}{{BOLD}}{{BG_YELLOW}}X{{RESET}}", true, "\x1b[31;1;43mX\x1b[0m"},
		{"text between tokens flushes separate SGR", "{{FG_RED}}a{{BOLD}}b", true, "\x1b[31ma\x1b[1mb\x1b[0m"},
		{"case-insensitive + whitespace", "{{ fg_red }}hi", true, "\x1b[31mhi\x1b[0m"},
		{"color OFF strips known tokens", "{{FG_RED}}{{BOLD}}hi{{RESET}}", false, "hi"},
		{"unknown token passes through literally", "{{FG_MAUVE}}hi", true, "{{FG_MAUVE}}hi"},
		{"unknown token passes through when off too", "{{FG_MAUVE}}hi", false, "{{FG_MAUVE}}hi"},
		{"unclosed brace is literal", "a {{FG_RED text", true, "a {{FG_RED text"},
		{"no tokens is identity (fast path)", "plain 世界 text", true, "plain 世界 text"},
		{"bright + bg", "{{FG_BRIGHT_CYAN}}{{BG_BLACK}}x", true, "\x1b[96;40mx\x1b[0m"},
		// A realistic multi-line frame: the color span crosses the CRLF inside one Write; CR/LF are
		// ordinary non-token bytes copied through, and only ONE trailing reset is appended at frame end.
		{"color span across CRLF in one frame", "{{FG_GREEN}}line1\r\nline2{{RESET}}", true, "\x1b[32mline1\r\nline2\x1b[0m"},
		{"multibyte around a token", "世{{FG_RED}}界", true, "世\x1b[31m界\x1b[0m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderColor(tc.in, tc.enabled); got != tc.want {
				t.Fatalf("renderColor(%q, %v) = %q, want %q", tc.in, tc.enabled, got, tc.want)
			}
		})
	}
}

// sgrOnly matches text in which EVERY ESC begins a well-formed SGR (`ESC [ <digits/semicolons> m`) and nothing
// else — no cursor move, erase, scroll, bell, or device query is expressible.
var sgrOnly = regexp.MustCompile("^([^\x1b]|\x1b\\[[0-9;]*m)*$")

// TestRenderColorIsInjectionSafe is the security proof: rendering EVERY token (and adjacent combinations)
// produces only well-formed SGR escapes — never a terminal-control sequence. This is what lets a hostile
// player/builder use color freely without being able to send a bell, backspace, cursor move, or screen clear.
func TestRenderColorIsInjectionSafe(t *testing.T) {
	// Concatenate every token twice (to exercise coalescing) plus interleaved text.
	var sb strings.Builder
	for name := range sgrTokens {
		sb.WriteString("{{" + name + "}}")
		sb.WriteString("x{{" + name + "}}")
	}
	out := renderColor(sb.String(), true)

	if !sgrOnly.MatchString(out) {
		t.Fatalf("rendered output contains a NON-SGR escape (injection!): %q", out)
	}
	// Belt-and-braces: none of the dangerous control bytes/sequences can appear.
	for _, bad := range []string{"\x07" /*BEL*/, "\x08" /*BS*/, "\x1b[2J" /*erase screen*/, "\x1b[H" /*cursor home*/, "\x1b[6n" /*device query*/, "\x1b]" /*OSC*/} {
		if strings.Contains(out, bad) {
			t.Fatalf("rendered output contains dangerous sequence %q: %q", bad, out)
		}
	}
	// Every ESC must be immediately followed by '[' and terminated by 'm'.
	for i := 0; i < len(out); i++ {
		if out[i] == 0x1b {
			if i+1 >= len(out) || out[i+1] != '[' {
				t.Fatalf("ESC not starting a CSI at %d: %q", i, out)
			}
			end := strings.IndexByte(out[i:], 'm')
			if end < 0 {
				t.Fatalf("SGR not terminated by 'm' at %d: %q", i, out)
			}
		}
	}
}

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
