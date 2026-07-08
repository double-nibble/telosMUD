package world

import (
	"bytes"
	"strings"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
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

// TestScreenProvenanceNormalSendCannotEmitRawANSI pins the OTHER half of the raw-ANSI provenance boundary
// (#31): the trusted screen.* path (screenShow -> screenFrame) is the ONLY producer of a raw Screen frame,
// the one output the gate writes verbatim without sanitizing. The sibling tests prove the screen.* API
// itself is safe-by-construction; this guards the complementary seam — that UNTRUSTED content routing text
// through the NORMAL output op (self:send) cannot smuggle raw terminal control. Whether the payload carries
// a 7-bit ESC CSI or a RAW 8-BIT C1 introducer (0x9B CSI / 0x9D OSC — the #156 vector that a rune-level
// CleanMarkup used to pass verbatim), self:send must go through textsan.CleanMarkup and arrive as an
// ordinary Output frame with the control stripped — never a Screen frame.
func TestScreenProvenanceNormalSendCannotEmitRawANSI(t *testing.T) {
	cases := map[string]string{
		// string.char(155)=0x9B (8-bit CSI), 157=0x9D (8-bit OSC) — raw invalid-utf8 C1 introducers.
		"7-bit ESC CSI":  `self:send("\x1b[2J danger \x1b[0m")`,
		"8-bit C1 CSI":   `self:send(string.char(155).."2J danger "..string.char(155).."m")`,
		"8-bit C1 OSC52": `self:send(string.char(157).."52;c;ZXZpbA==".."\x07")`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			z := newZone("screen")
			player := makeRoomPlayer(z, "Viewer")
			room := player.entity.location
			z.rooms[room.proto] = room

			if err := z.lua.runChunkWithSelf(t.Name(), script, player.entity); err != nil {
				t.Fatalf("script errored: %v", err)
			}

			select {
			case f := <-player.out:
				if sc := f.GetScreen(); sc != nil {
					t.Fatalf("content self:send produced a raw Screen frame — provenance boundary breached: %q", sc.GetData())
				}
				out := f.GetOutput()
				if out == nil {
					t.Fatalf("content self:send should produce an ordinary Output frame, got payload %T", f.GetPayload())
				}
				// BYTE-level checks: a raw 8-bit C1 (0x9B/0x9D/0x9C) is INVALID utf-8, so strings.ContainsRune
				// would look for the 2-byte U+00xx encoding and MISS the raw byte. Assert on the bytes.
				markup := out.GetMarkup()
				for _, bad := range []byte{0x1b, 0x9b, 0x9d, 0x9c, 0x07} { // ESC, CSI, OSC, ST, BEL
					if strings.IndexByte(markup, bad) >= 0 {
						t.Fatalf("content output must be sanitized (only the trusted screen path emits raw control); leaked %#x: %q", bad, markup)
					}
				}
			case <-time.After(time.Second):
				t.Fatal("no frame emitted from self:send")
			}
		})
	}
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

// TestScreenWriteStripsBidiOverride is the #22 regression on the sanitizer-BYPASSING screen path: write()
// runs sanitizeScreenText, which (like sanitizeOutput) gated only on Cc controls and so let the Cf
// bidi-override subset (RLO U+202E = UTF-8 E2 80 AE, and the isolates) through — a Trojan-Source spoof on a
// full-screen surface. It must now drop the override subset while preserving legitimate RTL text (Arabic
// alef U+0627 = D8 A7).
func TestScreenWriteStripsBidiOverride(t *testing.T) {
	out := runScreen(t, `screen.frame():write("adm\226\128\174in \216\167"):show(self)`)
	if strings.ContainsRune(out, 0x202E) {
		t.Fatalf("write() must strip the RLO bidi-override; got %q", out)
	}
	if !strings.Contains(out, "admin") {
		t.Fatalf("surrounding text should remain (override dropped, not the letters); got %q", out)
	}
	if !strings.ContainsRune(out, 0x0627) {
		t.Fatalf("legitimate Arabic letter stripped (only the OVERRIDE subset should go); got %q", out)
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

// countScreenFrames drains s.out and returns how many Screen frames were emitted (non-blocking).
func countScreenFrames(s *session) int {
	n := 0
	for {
		select {
		case f := <-s.out:
			if f.GetScreen() != nil {
				n++
			}
		default:
			return n
		}
	}
}

// TestScreenShowSameRoomScope (#120/#147): screen:show(target) may paint only a target in the SAME ROOM as
// the script's subject (self), not merely the same zone — the cheapest bound on "who a script can redraw".
func TestScreenShowSameRoomScope(t *testing.T) {
	z := newZone("screen")
	actor := makeRoomPlayer(z, "Actor")
	z.rooms[actor.entity.location.proto] = actor.entity.location

	// A co-located victim IS paintable.
	near := makePlayerTargetInRoom(z, actor.entity, "Near")
	z.lua.L.SetGlobal("near", z.lua.newHandle(near.entity))
	if err := z.lua.runChunkWithSelf(t.Name(), `screen.frame():write("hi"):show(near)`, actor.entity); err != nil {
		t.Fatalf("script errored: %v", err)
	}
	if got := countScreenFrames(near); got != 1 {
		t.Fatalf("a co-located target must receive the frame, got %d", got)
	}

	// A victim in a DIFFERENT room (same zone) must NOT be painted — the same-zone-but-not-same-room case #120
	// tightens.
	otherRoom := z.newEntity(ProtoRef("test:room2"))
	Add(otherRoom, &Room{})
	z.rooms[otherRoom.proto] = otherRoom
	far := &session{character: "Far", out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(far, "Far")
	Move(far.entity, otherRoom)
	z.players["Far"] = far
	z.lua.L.SetGlobal("far", z.lua.newHandle(far.entity))
	if err := z.lua.runChunkWithSelf(t.Name(), `screen.frame():write("hi"):show(far)`, actor.entity); err != nil {
		t.Fatalf("script errored: %v", err)
	}
	if got := countScreenFrames(far); got != 0 {
		t.Fatalf("a target in a DIFFERENT room must NOT be painted, got %d frames", got)
	}
}

// TestScreenShowRateLimit (#120): a receiver accepts at most maxScreenFramesPerPulse frames per zone pulse, so
// a tight repaint loop can't flood a co-located player. All show()s here run in one script invocation (one
// pulse), so only the first maxScreenFramesPerPulse land.
func TestScreenShowRateLimit(t *testing.T) {
	z := newZone("screen")
	actor := makeRoomPlayer(z, "Actor")
	z.rooms[actor.entity.location.proto] = actor.entity.location

	// Five paints of self within one invocation (one pulse) => only maxScreenFramesPerPulse accepted.
	if err := z.lua.runChunkWithSelf(t.Name(),
		`for i=1,5 do screen.frame():write("x"):show(self) end`, actor.entity); err != nil {
		t.Fatalf("script errored: %v", err)
	}
	if got := countScreenFrames(actor); got != maxScreenFramesPerPulse {
		t.Fatalf("the per-pulse rate limit must cap frames at %d, got %d", maxScreenFramesPerPulse, got)
	}

	// After the pulse advances, the budget resets — a later frame is accepted again.
	z.pulses.pulse++
	if err := z.lua.runChunkWithSelf(t.Name(), `screen.frame():write("y"):show(self)`, actor.entity); err != nil {
		t.Fatalf("script errored: %v", err)
	}
	if got := countScreenFrames(actor); got != 1 {
		t.Fatalf("the budget must reset on a new pulse, got %d", got)
	}
}
