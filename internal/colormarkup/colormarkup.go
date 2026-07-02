// Package colormarkup is the SINGLE SOURCE OF TRUTH for the `{{TOKEN}}` ANSI color markup: the vocabulary
// (FG_RED, BOLD, …), the tokenizer, and the render/strip. Color is expressed as TEXT tokens (`{{FG_RED}}`,
// `{{BOLD}}`, …) that ride the whole pipeline as ordinary text.
//
// Two subsystems consume this and MUST agree on which `{{...}}` is a zero-width token vs. literal text:
//   - the edge (internal/telnet) calls Render to turn ALLOWLISTED tokens into SGR at Write time; and
//   - the layout engine (internal/consoleui) calls ScanTokenRun to measure the VISIBLE width PAST those tokens.
//
// If those two used independent tokenizers they would drift on unknown/typo'd tokens (which the edge emits as
// literal visible text, not zero-width) and columns would misalign — exactly the bug this package prevents by
// owning the tokenizer once.
//
// Safety guarantee: a token can produce color and NOTHING else. Every rendered escape is exactly `ESC[<params>m`
// — the terminator is the hardcoded `m` (SGR) and every param is a fixed numeric code from sgrTokens (0–9
// styles, 30–37/90–97 fg, 40–47/100–107 bg). There is no token for cursor movement, erase, bell, or a device
// query, so a hostile player/builder cannot inject a terminal-control sequence. (The edge additionally strips
// raw ESC/BEL/BS via sanitizeOutput BEFORE calling Render.)
//
// The vocabulary is FIXED and universal (red == SGR 31 everywhere) — NOT content-defined: the flavor is which
// text a builder colors, not what "red" means. Case-insensitive; unknown/unclosed `{{...}}` passes through
// literally (transparent, never a silent drop); adjacent known tokens COALESCE into one SGR.
package colormarkup

import "strings"

// sgrTokens maps a color token name (uppercased) to its SGR parameter code. This is the entire ANSI capability
// exposed to content/players — all color/style, no terminal-state control.
var sgrTokens = map[string]string{
	"RESET": "0",
	// Styles (BLINK/ITALIC/STRIKE are spottier across terminals but harmless when unsupported).
	"BOLD": "1", "DIM": "2", "ITALIC": "3", "UNDERLINE": "4", "BLINK": "5", "REVERSE": "7", "STRIKE": "9",
	// Foreground 30–37.
	"FG_BLACK": "30", "FG_RED": "31", "FG_GREEN": "32", "FG_YELLOW": "33",
	"FG_BLUE": "34", "FG_MAGENTA": "35", "FG_CYAN": "36", "FG_WHITE": "37",
	// Bright foreground 90–97.
	"FG_BRIGHT_BLACK": "90", "FG_BRIGHT_RED": "91", "FG_BRIGHT_GREEN": "92", "FG_BRIGHT_YELLOW": "93",
	"FG_BRIGHT_BLUE": "94", "FG_BRIGHT_MAGENTA": "95", "FG_BRIGHT_CYAN": "96", "FG_BRIGHT_WHITE": "97",
	// Background 40–47.
	"BG_BLACK": "40", "BG_RED": "41", "BG_GREEN": "42", "BG_YELLOW": "43",
	"BG_BLUE": "44", "BG_MAGENTA": "45", "BG_CYAN": "46", "BG_WHITE": "47",
	// Bright background 100–107.
	"BG_BRIGHT_BLACK": "100", "BG_BRIGHT_RED": "101", "BG_BRIGHT_GREEN": "102", "BG_BRIGHT_YELLOW": "103",
	"BG_BRIGHT_BLUE": "104", "BG_BRIGHT_MAGENTA": "105", "BG_BRIGHT_CYAN": "106", "BG_BRIGHT_WHITE": "107",
}

// sgrParam resolves a token name (case-insensitive, whitespace-trimmed) to its SGR param, or ok=false.
func sgrParam(name string) (string, bool) {
	p, ok := sgrTokens[strings.ToUpper(strings.TrimSpace(name))]
	return p, ok
}

// sgrReset is the reset sequence appended at a frame's end so an unclosed color can't bleed into the prompt
// or the next frame.
const sgrReset = "\x1b[0m"

// Render translates `{{TOKEN}}` markup in s. When enabled, each maximal run of ADJACENT known tokens becomes one
// coalesced SGR (`{{FG_RED}}{{BOLD}}` -> ESC[31;1m); when disabled, known tokens are DROPPED (clean plain text).
// An unknown or unclosed `{{…}}` is emitted verbatim either way. If any color was emitted, a trailing reset is
// appended (idempotent) so color never leaks past this frame. The edge runs this AFTER sanitizeOutput, so it is
// the SOLE source of ESC in the output and that ESC is always a well-formed SGR.
func Render(s string, enabled bool) string {
	if !strings.Contains(s, "{{") { // fast path: the common, token-free frame
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	emitted := false
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "{{") {
			// Coalesce a run of adjacent KNOWN tokens starting at i.
			params, next := ScanTokenRun(s, i)
			if len(params) > 0 {
				if enabled {
					b.WriteString("\x1b[")
					b.WriteString(strings.Join(params, ";"))
					b.WriteByte('m')
					emitted = true
				} // disabled: drop the run
				i = next
				continue
			}
			// Not a known token (unknown name or no closing "}}"): emit "{{" literally and rescan the rest.
			b.WriteString("{{")
			i += 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	out := b.String()
	if emitted && !strings.HasSuffix(out, sgrReset) {
		out += sgrReset
	}
	return out
}

// Strip returns s as plain text: known color tokens removed, unknown/unclosed `{{...}}` left literal (matching
// how the edge renders them). It is Render's disabled projection — for `color off`, GMCP text, and any consumer
// that wants the visible string without SGR.
func Strip(s string) string { return Render(s, false) }

// ScanTokenRun reads a maximal run of adjacent known `{{TOKEN}}` tokens starting at s[i] (which begins "{{"),
// returning their SGR params and the index just past the run. An empty result (next == i) means s[i] does not
// begin a known token (unknown name or unclosed), so the caller treats "{{" as literal text. This is the shared
// primitive: the edge renders these runs to SGR, and consoleui zero-widths them for measurement.
func ScanTokenRun(s string, i int) (params []string, next int) {
	j := i
	for strings.HasPrefix(s[j:], "{{") {
		end := strings.Index(s[j:], "}}")
		if end < 0 {
			break // unclosed
		}
		param, ok := sgrParam(s[j+2 : j+end])
		if !ok {
			break // unknown token ends the run
		}
		params = append(params, param)
		j += end + 2
	}
	return params, j
}
