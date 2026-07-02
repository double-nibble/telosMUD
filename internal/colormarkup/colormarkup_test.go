package colormarkup

import (
	"regexp"
	"strings"
	"testing"
)

// colormarkup_test.go — pins the `{{TOKEN}}` render/strip/coalesce behavior AND the security guarantee: no token
// combination can produce anything but a well-formed SGR. (The Write-path integration lives in internal/telnet.)

func TestRenderBasics(t *testing.T) {
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
		{"color span across CRLF in one frame", "{{FG_GREEN}}line1\r\nline2{{RESET}}", true, "\x1b[32mline1\r\nline2\x1b[0m"},
		{"multibyte around a token", "世{{FG_RED}}界", true, "世\x1b[31m界\x1b[0m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Render(tc.in, tc.enabled); got != tc.want {
				t.Fatalf("Render(%q, %v) = %q, want %q", tc.in, tc.enabled, got, tc.want)
			}
		})
	}
}

func TestStripIsDisabledRender(t *testing.T) {
	if got := Strip("{{FG_RED}}hi{{RESET}} world"); got != "hi world" {
		t.Fatalf("Strip dropped/kept wrong: got %q", got)
	}
	if got := Strip("{{FG_MAUVE}}hi"); got != "{{FG_MAUVE}}hi" {
		t.Fatalf("Strip should leave unknown tokens literal: got %q", got)
	}
}

// sgrOnly matches text in which EVERY ESC begins a well-formed SGR (`ESC [ <digits/semicolons> m`) and nothing
// else — no cursor move, erase, scroll, bell, or device query is expressible.
var sgrOnly = regexp.MustCompile("^([^\x1b]|\x1b\\[[0-9;]*m)*$")

// TestRenderIsInjectionSafe is the security proof: rendering EVERY token (and adjacent combinations) produces
// only well-formed SGR escapes — never a terminal-control sequence.
func TestRenderIsInjectionSafe(t *testing.T) {
	var sb strings.Builder
	for name := range sgrTokens {
		sb.WriteString("{{" + name + "}}")
		sb.WriteString("x{{" + name + "}}")
	}
	out := Render(sb.String(), true)

	if !sgrOnly.MatchString(out) {
		t.Fatalf("rendered output contains a NON-SGR escape (injection!): %q", out)
	}
	for _, bad := range []string{"\x07" /*BEL*/, "\x08" /*BS*/, "\x1b[2J" /*erase*/, "\x1b[H" /*home*/, "\x1b[6n" /*query*/, "\x1b]" /*OSC*/} {
		if strings.Contains(out, bad) {
			t.Fatalf("rendered output contains dangerous sequence %q: %q", bad, out)
		}
	}
	for i := 0; i < len(out); i++ {
		if out[i] == 0x1b {
			if i+1 >= len(out) || out[i+1] != '[' {
				t.Fatalf("ESC not starting a CSI at %d: %q", i, out)
			}
			if strings.IndexByte(out[i:], 'm') < 0 {
				t.Fatalf("SGR not terminated by 'm' at %d: %q", i, out)
			}
		}
	}
}
