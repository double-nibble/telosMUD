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

// TestScanWindowBoundsAdversarialInput pins the tokenScanWindow hardening: (a) a "{{...}}" pair wider
// than the window is LITERAL even if a distant "}}" exists (before the window, each such "{{" rescanned
// to that distant close — O(n²) on adversarial input); (b) generously-padded KNOWN tokens still render;
// (c) a huge token-less "{{" run round-trips verbatim in time only a linear scan allows (a quadratic
// scan of this input would visibly hang the suite).
func TestScanWindowBoundsAdversarialInput(t *testing.T) {
	// (a) The "}}" sits past the window → literal, both enabled and disabled.
	wide := "{{" + strings.Repeat(" ", tokenScanWindow+4) + "FG_RED}} tail"
	if got := Render(wide, true); got != wide {
		t.Fatalf("over-window pair should be literal: got %q", got)
	}
	if got := Strip(wide); got != wide {
		t.Fatalf("Strip of over-window pair should be identity: got %q", got)
	}
	// (b) Padding within the window still renders (the trim semantics are unchanged for sane input).
	padded := "{{  " + "FG_RED" + "  }}hi"
	if got := Render(padded, true); got != "\x1b[31mhi\x1b[0m" {
		t.Fatalf("padded known token should render: got %q", got)
	}
	// Exact boundary: a name region of exactly tokenScanWindow bytes renders; one byte more is
	// literal (pins the limit arithmetic, not just the far-over case).
	atLimit := "{{" + strings.Repeat(" ", tokenScanWindow-6) + "FG_RED}}x"
	if got := Render(atLimit, true); got != "\x1b[31mx\x1b[0m" {
		t.Fatalf("exactly-at-window token should render: got %q", got)
	}
	pastLimit := "{{" + strings.Repeat(" ", tokenScanWindow-5) + "FG_RED}}x"
	if got := Render(pastLimit, true); got != pastLimit {
		t.Fatalf("one-past-window pair should be literal: got %q", got)
	}
	// Vocabulary-growth guard: every known token name must fit the window with room to spare, so
	// adding a long name can never silently make a bare (unpadded) token unscannable.
	for name := range sgrTokens {
		if len(name) > tokenScanWindow {
			t.Errorf("token %q (%d bytes) exceeds tokenScanWindow (%d)", name, len(name), tokenScanWindow)
		}
	}
	// (c) 200k token-less "{{" bytes: identity, and fast only if the scan is linear.
	run := strings.Repeat("{{", 100_000)
	if got := Strip(run); got != run {
		t.Fatal("token-less {{ run must round-trip verbatim")
	}
	if got := Render(run+"{{FG_RED}}x", true); !strings.HasSuffix(got, "\x1b[31mx\x1b[0m") || !strings.HasPrefix(got, run) {
		t.Fatalf("trailing real token after a literal run must still render: got tail %q", got[len(got)-20:])
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
			// Parse THIS sequence to its own terminator: after ESC[ only digits/semicolons may
			// appear, and the very next byte must be 'm'. (A bare substring search for 'm' could
			// be satisfied by a LATER sequence's terminator, silently passing a malformed one —
			// ai-finding #10. The sgrOnly regex above already enforces this; this loop is the
			// independent second proof, so it must be just as strict.)
			j := i + 2
			for j < len(out) && (out[j] == ';' || (out[j] >= '0' && out[j] <= '9')) {
				j++
			}
			if j >= len(out) || out[j] != 'm' {
				t.Fatalf("SGR at %d not terminated by 'm' after its parameter bytes: %q", i, out)
			}
		}
	}
}
