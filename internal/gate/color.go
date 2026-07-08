package gate

import (
	"context"
	"log/slog"
	"strings"
	"time"

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
//
// PERSISTENCE (#23): a `color on`/`color off` MUTATION is persisted to the account via ac.SetColorPref so the
// preference survives across sessions — the write stays on the gate<->account seam (color never touches the
// world). Persist only on an ACTUAL state change (requested != current tc.ColorEnabled()): a client that spams
// `color on;color on;…` collapses to a single write, closing the UPDATE-amplification vector an unauthenticated
// player could otherwise trigger on their own account row. The bare `color` STATUS query is not persisted (it
// changes nothing). accountID=="" (the stub / dev-autoauth path) is a no-op write. A persist failure is logged
// and swallowed: the in-session toggle has already applied, so a flaky account service must not break the command.
func handleColorCommand(ctx context.Context, tc *telnet.Conn, ac AccountClient, accountID, line string, log *slog.Logger) bool {
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
		changed := !tc.ColorEnabled() // persist only a real state change (below)
		tc.SetColor(true)
		// Rendered with color now ON, so the confirmation itself shows it working.
		_ = tc.Write("{{FG_GREEN}}Color is now ON.{{RESET}}\r\n")
		if changed {
			persistColorPref(ctx, ac, accountID, true, log)
		}
	case f[1] == "off":
		changed := tc.ColorEnabled()
		tc.SetColor(false)
		_ = tc.Write("Color is now OFF (plain text).\r\n")
		if changed {
			persistColorPref(ctx, ac, accountID, false, log)
		}
	default:
		_ = tc.Write("Usage: color [on|off]\r\n")
	}
	return true
}

// persistColorPref writes the toggled preference to the account (#23), bounded by a short timeout so a hung
// account service can't wedge the input pump. accountID=="" (stub / dev-autoauth) skips the write; any error
// is logged and swallowed (the session toggle already applied — persistence is best-effort).
func persistColorPref(ctx context.Context, ac AccountClient, accountID string, enabled bool, log *slog.Logger) {
	if accountID == "" {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ac.SetColorPref(wctx, accountID, enabled); err != nil {
		log.Warn("persist color pref failed (session toggle still applied)", "enabled", enabled, "err", err)
	}
}
