package world

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	lua "github.com/yuin/gopher-lua"
)

// luascreen.go — the `screen` sandbox module (#31 Slice 5): a curated, SAFE-BY-CONSTRUCTION binding for the
// trusted full-screen/ANSI output path (screen.go / the Screen frame). A content author builds an intro
// animation or HUD by chaining PRIMITIVES on a builder — clear / home / at(row,col) / color(name) /
// write(text) — that the ENGINE renders into a bounded subset of ANSI (erase, cursor move, SGR color), then
// flushes to a player with show(target) as ONE Screen frame.
//
// There is deliberately NO raw-bytes primitive. Because the author can never supply an arbitrary escape,
// the DANGEROUS ANSI classes the security review named are UNREACHABLE by construction — the allowlist IS
// the API surface, which is stronger than a post-hoc filter:
//   - report/query sequences (DSR ESC[…n, DA ESC[c, DECRQSS) that make the terminal transmit back INTO the
//     input stream — not expressible;
//   - OSC (clipboard OSC 52, window-title set/report) and bare ENQ (exfiltration) — not expressible;
//   - cursor args are numeric and CLAMPED; color is a fixed name→SGR allowlist; write() text is run through
//     sanitizeScreenText (BYTE-AWARE printable-only — strips C0 AND the raw 8-bit C1 controls 0x9B/0x9D/0x9C
//     a rune-level cleaner misses), so no author byte ever becomes terminal control.
//
// Trust model (mirrors the existing handle ops send/act/damage): the capability is exposed to CONTENT, and
// content-authorship is the trust boundary — a builder wrote the Lua. Unlike a raw-bytes path there is no
// dangerous primitive to additionally tier-gate: the worst a script can do is redraw a player's screen
// (bounded + resettable — the same "annoying content" risk as h:send spam), and the per-VM instruction
// budget plus the caps below bound the volume. The GATE additionally drops a Screen frame for a client that
// disabled ANSI (color off / non-ANSI terminal), so the raw path never garbles a plain client.

// luaScreenTypeName is the metatable registry key for the screen-builder userdata type.
const luaScreenTypeName = "telos.screen"

const (
	// maxScreenBytes caps the accumulated sequence (one animation frame) — an alloc/output bound mirroring
	// the sandbox string cap scale. maxScreenOps caps the number of primitive calls per builder (the op
	// multiplier the per-instruction budget can't see cheaply). maxScreenCoord clamps a cursor row/col to a
	// sane terminal bound so `at` can't emit an absurd ESC[N;NH.
	maxScreenBytes = 32 * 1024
	maxScreenOps   = 4096
	maxScreenCoord = 999
)

// screenColors is the fixed name→SGR allowlist the color() primitive maps — no author-supplied byte ever
// reaches the SGR. "reset" clears all attributes; "bold" is the one intensity attribute.
var screenColors = map[string]string{
	"reset": "\x1b[0m", "bold": "\x1b[1m",
	"black": "\x1b[30m", "red": "\x1b[31m", "green": "\x1b[32m", "yellow": "\x1b[33m",
	"blue": "\x1b[34m", "magenta": "\x1b[35m", "cyan": "\x1b[36m", "white": "\x1b[37m",
}

// luaScreen is the Go payload of a screen-builder userdata: the accumulated SAFE ANSI + op/byte accounting.
// Zone-goroutine-owned, so no lock.
type luaScreen struct {
	buf []byte
	ops int
}

// admit charges one primitive carrying n content bytes against the caps, raising a clean script error
// (fail-closed) if either the op count or the buffer would overflow. Called by every primitive BEFORE it
// appends, so a pathological builder errors at add time rather than allocating unbounded.
func (s *luaScreen) admit(l *lua.LState, n int) bool {
	if s.ops+1 > maxScreenOps {
		l.RaiseError("screen: too many operations (max %d)", maxScreenOps)
		return false
	}
	if len(s.buf)+n > maxScreenBytes {
		l.RaiseError("screen: sequence too large (cap %d bytes)", maxScreenBytes)
		return false
	}
	s.ops++
	return true
}

// installScreenTable registers the screen-builder userdata type and exposes the read-only `screen` global
// with the frame() constructor. Called once at sandbox build (luart.go), after the allowlist env is set.
func (rt *luaRuntime) installScreenTable() {
	L := rt.L

	smt := L.NewTypeMetatable(luaScreenTypeName)
	L.SetField(smt, "__tostring", L.NewFunction(func(l *lua.LState) int {
		l.Push(lua.LString("<screen>"))
		return 1
	}))
	L.SetField(smt, "__metatable", lua.LString("locked"))
	L.SetField(smt, "__index", L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"clear": rt.screenClear,
		"home":  rt.screenHome,
		"at":    rt.screenAt,
		"color": rt.screenColor,
		"write": rt.screenWrite,
		"show":  rt.screenShow,
	}))

	screen := L.NewTable()
	L.SetFuncs(screen, map[string]lua.LGFunction{"frame": rt.screenNew})

	g := L.Get(lua.GlobalsIndex).(*lua.LTable)
	g.RawSetString("screen", rt.readOnly(screen))
}

// screenNew is the `screen.frame()` constructor: a fresh empty builder.
func (rt *luaRuntime) screenNew(l *lua.LState) int {
	ud := l.NewUserData()
	ud.Value = &luaScreen{}
	l.SetMetatable(ud, l.GetTypeMetatable(luaScreenTypeName))
	l.Push(ud)
	return 1
}

// checkScreen extracts the *luaScreen payload from the userdata at stack index n, or nil.
func checkScreen(l *lua.LState, n int) *luaScreen {
	ud, ok := l.Get(n).(*lua.LUserData)
	if !ok {
		return nil
	}
	s, ok := ud.Value.(*luaScreen)
	if !ok {
		return nil
	}
	return s
}

// screenClear appends erase-screen + cursor-home.
func (rt *luaRuntime) screenClear(l *lua.LState) int {
	s := checkScreen(l, 1)
	if s == nil || !s.admit(l, len(ansiClearScreen)+len(ansiCursorHome)) {
		return 0
	}
	s.buf = append(s.buf, ansiClearScreen...)
	s.buf = append(s.buf, ansiCursorHome...)
	return returnSelf(l)
}

// screenHome appends cursor-home (row 1, col 1).
func (rt *luaRuntime) screenHome(l *lua.LState) int {
	s := checkScreen(l, 1)
	if s == nil || !s.admit(l, len(ansiCursorHome)) {
		return 0
	}
	s.buf = append(s.buf, ansiCursorHome...)
	return returnSelf(l)
}

// screenAt appends a cursor-position sequence ESC[{row};{col}H; row/col are clamped to [1, maxScreenCoord],
// so a script can never emit an absurd coordinate.
func (rt *luaRuntime) screenAt(l *lua.LState) int {
	s := checkScreen(l, 1)
	if s == nil {
		return 0
	}
	seq := fmt.Sprintf("\x1b[%d;%dH", clampCoord(l.CheckInt(2)), clampCoord(l.CheckInt(3)))
	if !s.admit(l, len(seq)) {
		return 0
	}
	s.buf = append(s.buf, seq...)
	return returnSelf(l)
}

// screenColor appends the SGR for a named color from the fixed allowlist; an unknown name is a soft no-op
// (fail-soft, no error mid-build).
func (rt *luaRuntime) screenColor(l *lua.LState) int {
	s := checkScreen(l, 1)
	if s == nil {
		return 0
	}
	code, ok := screenColors[strings.ToLower(strings.TrimSpace(l.CheckString(2)))]
	if !ok {
		return returnSelf(l)
	}
	if !s.admit(l, len(code)) {
		return 0
	}
	s.buf = append(s.buf, code...)
	return returnSelf(l)
}

// screenWrite appends literal text at the cursor. The text is run through sanitizeScreenText (below), which
// keeps ONLY printable runes, so an author can never smuggle an escape sequence through write(). NOTE:
// write() is SINGLE-LINE — control runes including newline/tab are stripped, so an embedded "\n" is removed,
// not a line break. Full-screen layout is by ABSOLUTE POSITIONING: call at(row, col) before each line's
// write(). (This keeps the frame free of the scroll side effects a bare newline would cause.)
func (rt *luaRuntime) screenWrite(l *lua.LState) int {
	s := checkScreen(l, 1)
	if s == nil {
		return 0
	}
	text := sanitizeScreenText(l.CheckString(2))
	if !s.admit(l, len(text)) {
		return 0
	}
	s.buf = append(s.buf, text...)
	return returnSelf(l)
}

// sanitizeScreenText returns text safe to place VERBATIM on the sanitizer-BYPASSING screen path: only
// printable runes survive. It is deliberately BYTE-AWARE where it must be. textsan.CleanMarkup is rune-level
// and, being blind to invalid-UTF-8 bytes, would let a RAW 8-BIT C1 CONTROL through — 0x9B (8-bit CSI),
// 0x9D (8-bit OSC), 0x9C (8-bit ST) are all invalid UTF-8, decode to U+FFFD (for which unicode.IsControl is
// false), and so survive a rune-level filter. On this raw path those bytes are LIVE escape introducers (a
// DSR/DA cursor-report that injects back into the input stream, or an OSC-52 clipboard exfil) — the exact
// classes #31's security guardrail said must be unreachable. So here EVERY byte that is not a valid,
// printable, non-control rune is DROPPED: an invalid/lone byte (a raw C1 like 0x9B, a stray 0xFF) and every
// control rune (C0 incl. ESC/BEL, DEL, and the U+0080–U+009F C1 runes) are removed; valid multibyte UTF-8
// printables (including combining marks/emoji) survive. This makes write() safe by CONSTRUCTION, not by a
// downstream filter the raw path skips.
func sanitizeScreenText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++ // an invalid byte (e.g. a raw 8-bit C1 introducer 0x9B/0x9D/0x9C, or 0xFF) — drop it
			continue
		}
		if !unicode.IsControl(r) {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// screenShow flushes the accumulated sequence to a target player as ONE Screen frame (atomic — no flicker
// or interleave with other output), followed by an SGR reset so color never bleeds past the frame. The
// target is an entity handle; a non-player / session-less target is a silent no-op.
func (rt *luaRuntime) screenShow(l *lua.LState) int {
	s := checkScreen(l, 1)
	if s == nil {
		return 0
	}
	target := resolveHandle(l, 2)
	if target == nil {
		return 0
	}
	if pc, ok := sessionOf(target); ok {
		out := make([]byte, 0, len(s.buf)+4)
		out = append(out, s.buf...)
		out = append(out, "\x1b[0m"...) // trailing reset
		pc.send(screenFrame(out))
	}
	return 0
}

// clampCoord bounds a cursor row/col to [1, maxScreenCoord].
func clampCoord(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxScreenCoord {
		return maxScreenCoord
	}
	return n
}
