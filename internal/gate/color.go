package gate

import (
	"strings"

	"github.com/double-nibble/telosmud/internal/telnet"
)

// color.go — the edge-local `color` command (Track 1 slice 2). Color is a TERMINAL concern: it toggles the
// gate's `{{TOKEN}}` -> SGR rendering (internal/telnet/color.go), not any game state, so the gate handles it
// INLINE in the line pump and does NOT forward it to the world — the gate is stateless beyond its socket and
// the world never needs to know a player's terminal color preference. Session-scoped: the toggle resets to the
// default (on) on a new connection; persisting it across sessions (via the account) is a follow-up.
//
// RESERVED WORD: because the gate intercepts `color` before the world sees it, `color` is a reserved edge verb
// — a content-registered custom Lua verb named `color` would be silently shadowed here (the edge and world
// command registries don't know about each other). Any future edge-local verbs are reserved the same way.

// handleColorCommand intercepts `color`, `color on`, and `color off` (case-insensitive). It returns true when
// the line was a color command — the caller then does NOT forward it to the world. Confirmations are written
// straight to the telnet conn; telnet.Conn.Write is mutex-guarded, so writing from the line-pump goroutine is
// race-safe against the world-frame writer goroutine.
func handleColorCommand(tc *telnet.Conn, line string) bool {
	f := strings.Fields(strings.ToLower(strings.TrimSpace(line)))
	if len(f) == 0 || f[0] != "color" {
		return false
	}
	switch {
	case len(f) == 1:
		state := "off"
		if tc.ColorEnabled() {
			state = "on"
		}
		_ = tc.Write("Color is currently " + state + ". Use `color on` or `color off`.\r\n")
	case f[1] == "on":
		tc.SetColor(true)
		// Rendered with color now ON, so the confirmation itself shows it working.
		_ = tc.Write("{{FG_GREEN}}Color is now ON.{{RESET}}\r\n")
	case f[1] == "off":
		tc.SetColor(false)
		_ = tc.Write("Color is now OFF (plain text).\r\n")
	default:
		_ = tc.Write("Usage: color [on|off]\r\n")
	}
	return true
}
