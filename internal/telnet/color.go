package telnet

import "github.com/double-nibble/telosmud/internal/colormarkup"

// color.go — the edge side of the `{{TOKEN}}` ANSI color layer (docs/REMAINING.md Track 1). The vocabulary,
// tokenizer, and render/strip live in internal/colormarkup (the single source of truth, shared with the layout
// engine's width measurement so the two can't drift). HERE we only wire it into the edge: renderColor runs inside
// Write, AFTER sanitizeOutput has stripped every raw control rune, so it is the SOLE source of ESC in the output
// and that ESC is always a well-formed SGR — a hostile player/builder cannot inject cursor/erase/bell/query
// sequences. Cursor/screen control ("towel"-style full-screen ANSI) is a SEPARATE trusted output path, never
// this token layer.

// renderColor is the edge's entry point into the shared color renderer (see internal/colormarkup.Render).
func renderColor(s string, enabled bool) string { return colormarkup.Render(s, enabled) }
