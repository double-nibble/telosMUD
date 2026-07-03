package world

// screen.go — the TRUSTED full-screen / ANSI output path (#31, tier 2 of the color design). The Track-1
// color layer is SGR-only (safe for anyone, but it can't move the cursor or redraw the screen); THIS is a
// distinct, trusted output mode that writes raw ANSI — cursor positioning, erase, scroll regions — STRAIGHT
// to the socket via the Screen frame (frames.go), BYPASSING the gate's output sanitizer (which strips ESC)
// and the color-token renderer. Safety is by PROVENANCE, not an allowlist: only trusted producers emit a
// Screen frame, so cursor/screen control stays out of untrusted hands.
//
// This slice ships the PROTOCOL SEAM plus ONE engine-owned producer to prove it end-to-end: `clear`, whose
// bytes are the engine-CONSTANT clear-screen sequence (never player input), so it is safe for ANY player to
// invoke. A sandboxed, trust-gated screen.* CONTENT capability (a builder-authored intro animation) is the
// next slice — it will emit through this same screenFrame, gated so player text never reaches the raw path.

// ANSI screen-control sequences the engine emits over the trusted raw path. CSI is ESC[ (0x1b 0x5b); the
// gate's sanitizer would strip the ESC on the normal Output path, which is exactly why these ride a Screen
// frame instead.
const (
	ansiClearScreen = "\x1b[2J" // erase the entire screen
	ansiCursorHome  = "\x1b[H"  // move the cursor to row 1, column 1
)

// screenCommands returns the engine-owned raw-screen verbs (#31). `clear` is UNIVERSAL — its bytes are an
// engine constant (not player-authored), so there is nothing untrusted to gate; it is not rank-gated (unlike
// the later screen.* content capability). Registered low-priority so `cl` still abbreviates to `close`.
func screenCommands() []*Command {
	return []*Command{
		{Name: "clear", Aliases: []string{"cls"}, Run: cmdClear},
	}
}

// cmdClear wipes the player's terminal via the trusted raw path: the engine-constant clear-screen +
// cursor-home sequence, sent as a Screen frame so it bypasses the sanitizer that would otherwise strip the
// ESC. Provenance is trivially safe here — the bytes are a compile-time constant, never player input.
func cmdClear(c *Context) error {
	c.s.send(screenFrame([]byte(ansiClearScreen + ansiCursorHome)))
	return nil
}
