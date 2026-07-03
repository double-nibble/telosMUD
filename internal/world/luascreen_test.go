package world

import (
	"bytes"
	"strings"
	"testing"
)

// luascreen_test.go — #31 Slice 5: the `screen` sandbox capability (luascreen.go). Covers the builder API
// producing SAFE ANSI, the safe-by-construction guarantees (write strips raw ESC; at clamps coords; no
// raw-bytes surface), the DoS caps, and the read-only global.

// runScreen runs a screen script with `self` bound to a player and returns the emitted Screen frame's bytes.
func runScreen(t *testing.T, script string) string {
	t.Helper()
	z := newZone("screen")
	player := makeRoomPlayer(z, "Viewer")
	// Register the player's room so the handle-resolve walk (entityByRID over z.rooms) finds self.
	room := player.entity.location
	z.rooms[room.proto] = room
	if err := z.lua.runChunkWithSelf(t.Name(), script, player.entity); err != nil {
		t.Fatalf("script errored: %v", err)
	}
	return drainForScreen(t, player)
}

// TestScreenBuildsSafeANSI: the primitives render to the expected bounded ANSI (erase, cursor move, SGR
// color, text), flushed as one Screen frame with a trailing reset.
func TestScreenBuildsSafeANSI(t *testing.T) {
	out := runScreen(t, `screen.frame():clear():at(2,3):color("red"):write("Hi"):show(self)`)
	for _, want := range []string{
		"\x1b[2J",   // clear
		"\x1b[2;3H", // at(2,3)
		"\x1b[31m",  // color red
		"Hi",        // write
		"\x1b[0m",   // trailing reset
	} {
		if !strings.Contains(out, want) {
			t.Errorf("screen output missing %q\n---\n%q", want, out)
		}
	}
}

// TestScreenWriteStripsRawEscape: an author cannot smuggle a raw escape (here a 7-bit OSC 52 clipboard-write
// attempt) through write() — sanitizeScreenText strips the ESC/BEL, so the OSC introducer never reaches the
// wire. (The 8-bit-C1 form is covered by TestScreenWriteStripsC1Bytes.)
func TestScreenWriteStripsRawEscape(t *testing.T) {
	// write an OSC-52 clipboard sequence: ESC ] 52 ; c ; <b64> BEL
	out := runScreen(t, `screen.frame():write("\x1b]52;c;ZXZpbA==\x07"):show(self)`)
	if strings.Contains(out, "\x1b]") {
		t.Fatalf("write() must strip the raw ESC so no OSC reaches the wire; got %q", out)
	}
	// The stripped remnant is harmless literal text (no leading ESC), and the only ESC present is the
	// engine's trailing reset.
	if strings.Count(out, "\x1b") != 1 || !strings.HasSuffix(out, "\x1b[0m") {
		t.Fatalf("the only ESC should be the trailing reset; got %q", out)
	}
}

// TestScreenWriteIsSingleLine documents the contract: write() strips control runes including newline (so
// no scroll side effects); multi-line layout is via at() per line.
func TestScreenWriteIsSingleLine(t *testing.T) {
	out := runScreen(t, `screen.frame():write("Line1\nLine2"):show(self)`)
	if !strings.Contains(out, "Line1Line2") {
		t.Fatalf("write() should strip the newline (single-line contract); got %q", out)
	}
	if strings.Contains(out, "\n") {
		t.Fatalf("write() must not emit a raw newline; got %q", out)
	}
}

// TestScreenWriteStripsC1Bytes is the regression for the security-auditor's HIGH finding: write() must strip
// RAW 8-BIT C1 controls (0x9B CSI, 0x9D OSC, 0x9C ST) — invalid UTF-8 that a rune-level cleaner let through
// onto the sanitizer-bypassing wire, making DSR/DA cursor-report (input injection) and OSC-52 (clipboard
// exfil) reachable. The only ESC/control that may remain is the engine's trailing reset.
func TestScreenWriteStripsC1Bytes(t *testing.T) {
	cases := map[string]string{
		"8-bit CSI DSR (6n)":     `screen.frame():write("\155".."6n"):show(self)`,
		"8-bit CSI DA (c)":       `screen.frame():write("\155".."c"):show(self)`,
		"8-bit CSI window (18t)": `screen.frame():write("\155".."18t"):show(self)`,
		"8-bit OSC 52 clipboard": `screen.frame():write("\157".."52;c;ZXZpbA==".."\156"):show(self)`,
	}
	for name, src := range cases {
		outB := []byte(runScreen(t, src))
		for _, bad := range []byte{0x9b, 0x9d, 0x9c, 0x07} { // CSI, OSC, ST, BEL
			if bytes.IndexByte(outB, bad) >= 0 {
				t.Errorf("%s: byte %#x survived write() onto the raw path; got % x", name, bad, outB)
			}
		}
		// The only ESC (0x1b) permitted is the engine's trailing reset — never one injected via write().
		if bytes.Count(outB, []byte{0x1b}) != 1 || !bytes.HasSuffix(outB, []byte("\x1b[0m")) {
			t.Errorf("%s: the only ESC must be the trailing reset; got % x", name, outB)
		}
	}
}

// TestScreenAtClampsCoords: out-of-range cursor coordinates are clamped to [1, maxScreenCoord], so a script
// can never emit an absurd ESC[N;NH.
func TestScreenAtClampsCoords(t *testing.T) {
	out := runScreen(t, `screen.frame():at(-5, 100000):show(self)`)
	if !strings.Contains(out, "\x1b[1;999H") {
		t.Fatalf("at(-5,100000) should clamp to ESC[1;999H; got %q", out)
	}
}

// TestScreenNoRawSurface: there is no raw-bytes method — the security guarantee is the API shape itself.
func TestScreenNoRawSurface(t *testing.T) {
	z := newZone("screen")
	for _, method := range []string{"raw", "bytes", "esc", "ansi", "osc"} {
		src := `local ok = pcall(function() return screen.frame():` + method + `("x") end); assert(not ok, "` + method + ` must not exist")`
		if err := z.lua.runChunk(t.Name(), src); err != nil {
			t.Fatalf("screen builder must expose no raw-bytes method %q: %v", method, err)
		}
	}
}

// TestScreenIsReadOnly: the `screen` global can't be reassigned/monkeypatched by a script.
func TestScreenIsReadOnly(t *testing.T) {
	z := newZone("screen")
	if err := z.lua.runChunk(t.Name(), `screen.frame = function() end`); err == nil {
		t.Fatal("the screen global must be read-only")
	}
}

// TestScreenOpCapErrors: a builder that exceeds the per-frame op cap raises a clean script error.
func TestScreenOpCapErrors(t *testing.T) {
	z := newZone("screen")
	src := `local s = screen.frame(); for i=1,` + itoa(maxScreenOps+10) + ` do s:home() end`
	if err := z.lua.runChunk(t.Name(), src); err == nil {
		t.Fatal("exceeding the screen op cap should error")
	}
}
