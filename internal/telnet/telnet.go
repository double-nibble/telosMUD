// Package telnet provides a minimal line-oriented telnet codec for the gate:
// it strips/answers IAC negotiation and yields clean lines, and (Phase 9) speaks
// GMCP — option 201, the one option it OFFERS and accepts (gmcp.go, docs/GMCP.md).
// The remaining rich options (MCCP2, NAWS, TTYPE, CHARSET) are still skipped/refused
// and land in later Phase 9 sub-slices.
//
// # What it does
//
// A raw telnet client interleaves two things on the socket: the actual line the
// player typed, and in-band telnet control sequences introduced by the IAC byte
// (0xFF, "Interpret As Command"). This codec sits directly on the net.Conn and
// presents a clean, line-oriented view to the gate:
//
//   - ReadLine accumulates input bytes into a line, transparently consuming any
//     IAC sequences it encounters, and returns at the first newline.
//   - Write sends a string to the client, escaping any literal 0xFF so it is not
//     mistaken for the start of a command.
//
// # Negotiation policy: refuse everything except GMCP
//
// The client stays in plain line mode for input. We never *request* a client
// option and decline every option the client offers (so the connection keeps the
// default NVT line discipline), with ONE exception: we offer GMCP (IAC WILL 201)
// and accept the client's IAC DO 201, so a rich client can receive structured
// out-of-band data alongside the text stream (gmcp.go). See refuse for why we only
// answer DO/WILL and stay silent on DONT/WONT.
//
// # Debug logging
//
// This package logs negotiation and subnegotiation *control* events at
// slog.Debug (off unless DEBUG=1; see internal/obs) so an operator can watch the
// option dance. It deliberately does NOT log the content of input or output
// lines — that would be both noisy and a privacy/secret hazard (passwords). Each
// record carries "component"="telnet". The codec has no config dependency; it
// uses the slog default logger installed by obs.Init.
package telnet

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxLineBytes caps how many bytes ReadLine will accumulate for a single line
// before it stops appending and discards the rest of the over-long line up to
// the next newline. This bounds the per-connection read buffer (a malicious or
// buggy client cannot grow it without limit by withholding the newline) and, in
// turn, bounds the size of anything that fans out per room occupant downstream.
// 4 KiB comfortably exceeds any legitimate command or multibyte say while still
// being a hard ceiling. It lives here (not in config) because the codec has no
// config dependency by design and this is a protocol-level safety limit, not an
// operator-tunable knob.
const MaxLineBytes = 4096

// Telnet command bytes (RFC 854). These are the second byte of an IAC sequence
// (IAC <cmd> [opt]); option negotiation uses WILL/WONT/DO/DONT, and SB..SE
// brackets a subnegotiation payload.
const (
	iac  = 255
	dont = 254
	doo  = 253 // "DO" (do is a Go keyword)
	wont = 252
	will = 251
	sb   = 250
	se   = 240
)

// optGMCP is telnet option 201 (0xC9), the Generic MUD Communication Protocol (docs/GMCP.md). Unlike
// every other option (which `refuse` declines), the gate OFFERS GMCP (IAC WILL 201) and a capable
// client accepts with IAC DO 201; thereafter structured messages ride IAC SB 201 <pkg> SP <json> IAC SE.
const optGMCP = 201

// maxGMCPInBytes caps an inbound GMCP subnegotiation payload (Core.Hello / Core.Supports are small; a
// hostile client must not grow the read buffer without bound by withholding IAC SE). An over-cap
// subneg is drained to its terminator and dropped (fail-closed), never delivered.
const maxGMCPInBytes = 8192

// Conn wraps a connection with telnet line decoding. Writes are mutex-guarded so
// negotiation answers (emitted from the reader goroutine inside ReadLine) and
// output (emitted from the gate's writer goroutine) don't interleave on the wire.
// The two-goroutine model means writeRaw can be called concurrently from both
// sides; the mutex makes each frame atomic.
type Conn struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex // guards w against concurrent reader-vs-writer writes

	// GMCP (option 201, docs/GMCP.md). gmcpOn flips true when the client accepts our offer (IAC DO
	// 201) and false on IAC DONT 201 — read by WriteGmcp on the writer goroutine, set by handleIAC on
	// the reader goroutine, so it is atomic. onGmcp is the inbound sink for a parsed IAC SB 201 message;
	// it is set ONCE by the gate during connection setup, before the read loop starts (so a plain field
	// is race-free), and nil until then (a GMCP message that arrives before it is set is dropped).
	gmcpOn atomic.Bool
	onGmcp func(pkg string, json []byte)

	// writeTimeout bounds how long a single outbound write may block on a slow/wedged client before it
	// errors (Phase 16.3 backpressure). 0 (the default) means no deadline — writes block indefinitely, the
	// pre-16.3 behavior. Set ONCE by the gate at connection setup, before the read/write goroutines start, so
	// a plain field is race-free (like onGmcp). Only effective when the underlying writer is a net.Conn
	// (exposes SetWriteDeadline); a plain io.Writer (tests) silently ignores it.
	writeTimeout time.Duration

	// colorEnabled gates the `{{TOKEN}}` -> SGR rendering in Write (color.go). Default TRUE (MUD convention;
	// a non-ANSI client types `color off`). Read on the writer goroutine, flipped by the `color on/off`
	// command from the reader/dispatch goroutine, so it is atomic (like gmcpOn). When false, Write STRIPS the
	// tokens to clean text instead of rendering them.
	colorEnabled atomic.Bool
}

// SetWriteTimeout bounds how long a single Write may block on a slow/wedged client before returning a
// deadline error, so a wedged socket can't pin the gate's writer goroutine indefinitely (Phase 16.3). The
// gate closes the connection on such an error, reclaiming the slot. 0 disables the deadline. Call before the
// read loop starts (connection setup); not safe to change concurrently with writes.
func (c *Conn) SetWriteTimeout(d time.Duration) { c.writeTimeout = d }

// New wraps a bidirectional connection (the common case: a net.Conn straight
// from Accept). Reads are buffered; writes go to the same underlying stream.
func New(rw io.ReadWriter) *Conn {
	c := &Conn{r: bufio.NewReader(rw), w: rw}
	c.colorEnabled.Store(true) // color on by default (MUD convention); `color off` disables
	return c
}

// NewReadWriter is for tests with separate reader/writer halves, so a test can
// feed a scripted byte stream in and inspect the negotiation answers written out.
func NewReadWriter(r io.Reader, w io.Writer) *Conn {
	c := &Conn{r: bufio.NewReader(r), w: w}
	c.colorEnabled.Store(true)
	return c
}

// SetColor toggles `{{TOKEN}}` -> ANSI SGR rendering for this connection (the `color on/off` command). When
// off, Write strips the tokens to clean plain text. Race-safe (atomic): the writer goroutine reads it in Write.
func (c *Conn) SetColor(on bool) { c.colorEnabled.Store(on) }

// ColorEnabled reports whether ANSI color rendering is on for this connection.
func (c *Conn) ColorEnabled() bool { return c.colorEnabled.Load() }

// ReadLine returns the next line of input with IAC sequences removed, CR/NUL
// stripped, and control characters filtered out (sanitizeLine). A trailing
// partial line at EOF is returned with a nil error.
//
// It reads a byte at a time: an IAC byte hands off to handleIAC (which may write
// a negotiation answer back); CR and NUL are dropped so both CR LF and CR NUL
// line endings collapse cleanly; LF terminates the line. Every other byte is
// part of the line the player typed. This is the first hop of the input path:
// bytes on the socket -> a clean line -> the gate forwards it as an InputLine on
// the Play stream.
//
// # Length cap (DoS)
//
// Accumulation is bounded by MaxLineBytes. Once the line reaches that size we
// stop appending and quietly drain the rest of the over-long line up to the next
// newline (the connection is NOT dropped, and the next line still parses
// cleanly). The truncated prefix — not a torn-in-half tail — is what we return,
// after telling the user their line was too long. IAC sequences are still
// consumed while draining so negotiation is never corrupted by the cap.
//
// # Sanitization (terminal injection)
//
// The assembled line is filtered through sanitizeLine BEFORE return. Crucially
// this runs on the FINAL user-text bytes, AFTER every IAC/option sequence has
// already been consumed by handleIAC — so it never sees, and cannot corrupt,
// telnet negotiation bytes. It strips control runes (ESC, BEL, backspace, C1,
// ...) that would otherwise reach a bystander's terminal verbatim.
func (c *Conn) ReadLine() (string, error) {
	var line []byte
	truncated := false
	for {
		b, err := c.r.ReadByte()
		if err != nil {
			// EOF mid-line: hand back what we have so a final unterminated
			// line isn't silently lost.
			if len(line) > 0 && err == io.EOF {
				return sanitizeLine(line), nil
			}
			return "", err
		}
		if b == iac {
			// Consume negotiation even while draining an over-long line, so the
			// option dance and IAC-escaping stay correct. While truncated we
			// discard any literal byte handleIAC would append.
			before := len(line)
			if err := c.handleIAC(&line); err != nil {
				return "", err
			}
			if truncated && len(line) > before {
				line = line[:before]
			}
			continue
		}
		switch b {
		case '\r', 0:
			// ignore CR and NUL (CR LF / CR NUL line endings)
		case '\n':
			return sanitizeLine(line), nil
		default:
			if len(line) >= MaxLineBytes {
				// Over the cap: stop appending, start draining to the next LF.
				if !truncated {
					truncated = true
					slog.Debug("telnet input line truncated",
						"component", "telnet", "max", MaxLineBytes)
					_ = c.Write("\r\n[Your input was too long and has been truncated.]\r\n")
				}
				continue
			}
			line = append(line, b)
		}
	}
}

// sanitizeLine renders an assembled user-input line safe to forward to the world
// and, ultimately, to other players' terminals. It is UTF-8 aware: the bytes are
// decoded rune-by-rune and any rune for which unicode.IsControl reports true is
// dropped — this covers the C0 controls (ESC, BEL, backspace, ...) AND the C1
// control CODEPOINTS U+0080-U+009F, without raw-stripping the 0x80-0x9F byte
// range (those bytes are valid UTF-8 continuation bytes inside multibyte runes,
// so byte-stripping them would corrupt emoji / accented text). Invalid UTF-8
// decodes to utf8.RuneError, which is not a control rune and is kept as the
// replacement character rather than panicking.
//
// Tab decision: tab (U+0009) is a control rune and is therefore DROPPED here. The
// world's command parser treats whitespace generically and never needs an
// embedded tab; dropping it (rather than converting to a space) keeps targeting
// keywords clean and avoids smuggling layout control into rendered output. The
// common case — a line with no control runes — returns its bytes unchanged.
//
// SIZE NOTE: the slow path's WriteRune re-encodes each invalid-UTF-8 byte to the
// 3-byte U+FFFD, so for an invalid-bytes-plus-control line the returned string can
// be up to ~3× MaxLineBytes (the read loop bounds INPUT bytes, not output). This is
// benign at the edge — the line is forwarded once over this one connection's Play
// stream and is NOT fanned out per room occupant here; the world re-caps it with
// textsan.CleanLine at its gRPC ingress (the post-fan-out byte bound lives THERE, by
// the edge/world trust split). A future refactor that adds edge-side fan-out or trusts
// the edge line length must re-cap here (see textsan.CleanLine's double-cap).
func sanitizeLine(line []byte) string {
	if !hasControl(line) {
		return string(line)
	}
	var b strings.Builder
	b.Grow(len(line))
	for _, r := range string(line) {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// hasControl reports whether the UTF-8 text contains any control rune, so the
// common (clean) line can be returned without allocating a builder. It decodes
// runes rather than scanning bytes so it does not misread a multibyte
// continuation byte (0x80-0x9F) as a C1 control.
func hasControl(line []byte) bool {
	for i := 0; i < len(line); {
		r, size := utf8.DecodeRune(line[i:])
		if r != utf8.RuneError && unicode.IsControl(r) {
			return true
		}
		i += size
	}
	return false
}

// handleIAC consumes one IAC sequence, already having read the leading IAC byte.
// IAC IAC is an escaped literal 0xFF and appends one to line; WILL/WONT/DO/DONT
// are option negotiations and get refused; SB starts a subnegotiation we skip to
// its SE; any other 2-byte command (NOP, AYT, ...) is consumed and ignored.
func (c *Conn) handleIAC(line *[]byte) error {
	cmd, err := c.r.ReadByte()
	if err != nil {
		return err
	}
	switch cmd {
	case iac:
		// Escaped 0xFF in the data stream -> one literal byte in the line.
		*line = append(*line, iac)
	case doo, dont, will, wont:
		opt, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		// GMCP (201) is the one option we WANT: the client's DO enables it, DONT disables it. We
		// initiated with WILL, so no reply is owed. Every other option (and a stray WILL/WONT on 201)
		// is refused as before.
		if opt == optGMCP && (cmd == doo || cmd == dont) {
			c.gmcpOn.Store(cmd == doo)
			return nil
		}
		c.refuse(cmd, opt)
	case sb:
		opt, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		if opt == optGMCP {
			return c.readGMCPSubneg()
		}
		// Any other subnegotiation (NAWS, TTYPE, …) is still skipped wholesale to its IAC SE; the opt
		// byte we just consumed is part of that skipped block.
		return c.skipSubneg()
	default:
		// Other 2-byte commands (NOP, AYT, ...) — already consumed; ignore.
	}
	return nil
}

// refuse declines every negotiation so the client stays in line mode. Replying
// only to DO/WILL (never to DONT/WONT) avoids negotiation loops: a peer's DO is
// answered with WONT and a peer's WILL with DONT, while their DONT/WONT need no
// reply (they already match our "no options" stance). The refusal is logged at
// Debug with the command and option so the option dance is visible under DEBUG=1.
func (c *Conn) refuse(cmd, opt byte) {
	slog.Debug("telnet negotiation refused",
		"component", "telnet",
		"cmd", cmd,
		"opt", opt,
	)
	// A failed option-refusal write is best-effort and non-fatal: we deliberately discard the
	// error. A genuinely dead socket surfaces on the next real write via normal teardown, so
	// there is nothing useful to do with a negotiation-write failure here.
	switch cmd {
	case doo:
		_ = c.writeRaw([]byte{iac, wont, opt})
	case will:
		_ = c.writeRaw([]byte{iac, dont, opt})
	}
}

// skipSubneg discards a subnegotiation payload up to and including IAC SE. We
// don't support any options yet, so any SB..SE block (GMCP, NAWS, TTYPE, ...) is
// dropped wholesale. An IAC inside the payload is only the terminator when
// immediately followed by SE. The skip is logged at Debug as a control event.
func (c *Conn) skipSubneg() error {
	slog.Debug("telnet subnegotiation skipped", "component", "telnet")
	for {
		b, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		if b == iac {
			b2, err := c.r.ReadByte()
			if err != nil {
				return err
			}
			if b2 == se {
				return nil
			}
		}
	}
}

// Write sends s, escaping any literal 0xFF bytes as IAC IAC so player-visible
// text containing 0xFF can't be misread as the start of a telnet command. This
// is the last hop of the output path: a rendered ServerFrame -> bytes on the
// socket.
//
// Defense-in-depth: it also strips control runes (sanitizeOutput) so a control
// character that somehow reaches output — e.g. one that slipped past inbound
// sanitization on a different code path, or future GMCP-bearing rendering — can
// never hit a terminal as an injection. CR and LF are preserved because the gate
// frames output with "\r\n"; everything else unicode.IsControl flags is dropped.
// Inbound ReadLine sanitization remains the primary fix; this is belt-and-braces.
//
// ANSI color: renderColor (color.go) runs AFTER sanitizeOutput has stripped every raw control rune, so it is
// the SOLE source of ESC in the output and that ESC is always a well-formed SGR (`ESC [ <params> m`). This is
// the "produce SGR downstream of, not through, the control-strip" ordering: a player's raw ESC never survives
// (sanitizeOutput ate it), while the allowlisted `{{TOKEN}}` markup renders to color here. IAC-escaping runs
// last on the final bytes; SGR contains no 0xFF, so it is untouched by that step.
func (c *Conn) Write(s string) error {
	s = sanitizeOutput(s)
	s = renderColor(s, c.colorEnabled.Load())
	if strings.IndexByte(s, iac) >= 0 {
		s = strings.ReplaceAll(s, "\xff", "\xff\xff")
	}
	return c.writeRaw([]byte(s))
}

// sanitizeOutput strips control runes from outbound text while PRESERVING the
// CR/LF line framing the gate relies on. It is UTF-8 aware (rune-level, so
// multibyte runes survive) and short-circuits the clean common case. It walks the
// bytes with utf8.DecodeRuneInString so that bytes which are NOT a valid rune
// (e.g. a lone 0xFF) are copied through verbatim rather than rewritten to U+FFFD —
// that keeps Write's downstream 0xFF -> IAC IAC escaping intact. It runs before
// that escaping step.
func sanitizeOutput(s string) string {
	// The fast path REQUIRES valid UTF-8: a lone raw 8-bit C1 introducer (0x9B CSI / 0x9D OSC / 0x9C ST) is
	// invalid utf-8 that decodes to a NON-control RuneError, so an IsControl-only scan would wrongly judge it
	// clean and return it verbatim onto the wire — a sanitizer-bypassing terminal-control-injection vector
	// (#156). Fall through to the byte-aware rewrite when the string is not valid UTF-8.
	clean := utf8.ValidString(s)
	if clean {
		for _, r := range s {
			if (r != '\r' && r != '\n' && unicode.IsControl(r)) || isBidiOverride(r) {
				clean = false
				break
			}
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// A lone invalid byte. Preserve 0xFF (IAC) so downstream telnet escaping still doubles it, but
			// DROP raw 8-bit C1 introducers (0x80-0x9F, e.g. CSI 0x9B / OSC 0x9D / ST 0x9C) and any other
			// invalid byte — they are sanitizer-bypassing terminal control and must never reach the client
			// verbatim (#156). This is the universal last-line defense; the world's textsan.CleanMarkup
			// closes the same hole at the content-output boundary.
			if s[i] == 0xFF {
				b.WriteByte(s[i])
			}
			i++
			continue
		}
		if (r != '\r' && r != '\n' && unicode.IsControl(r)) || isBidiOverride(r) {
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// isBidiOverride reports whether r is an explicit Unicode bidi-override control — the "Trojan Source"
// visual-spoofing subset (CVE-2021-42574, #22): the embedding/override block U+202A–U+202E (LRE/RLE/PDF/
// LRO/RLO) and the isolate block U+2066–U+2069 (LRI/RLI/FSI/PDI). These are category Cf, so unicode.IsControl
// (Cc only) does NOT catch them and sanitizeOutput would otherwise pass them verbatim onto the terminal,
// letting a player name/message DISPLAY as something other than its bytes. It is a NARROW subset: legitimate
// implicit bidi marks (LRM/RLM/ALM) and the zero-width joiners (ZWNJ/ZWJ) are Cf too but preserved. The edge
// keeps its own copy of this predicate rather than importing internal/textsan.IsBidiOverride, so the edge and
// world trust domains stay independently defensible (the same reason sanitizeLine mirrors, not shares).
func isBidiOverride(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069)
}

// WriteScreen sends pre-formed raw terminal bytes (a TRUSTED full-screen/ANSI sequence: cursor
// positioning, erase, scroll regions, a clear) STRAIGHT to the socket, BYPASSING sanitizeOutput and the
// color-token renderer — so the terminal control survives instead of being stripped (the whole point of
// #31). It is the trusted output mode: the CALLER guarantees provenance (engine-owned output, or a
// trust-gated screen.* capability); player-authored text must NEVER reach here, because sanitizeOutput —
// the guard that neutralizes hostile ESC/cursor injection — is exactly what this skips. IAC (0xFF) IS still
// escaped: that is a telnet framing requirement (a raw 0xFF must not be misread as a telnet command), NOT
// terminal sanitization, so it does not defeat the raw ANSI. Mutex + write deadline are honored like Write.
func (c *Conn) WriteScreen(b []byte) error {
	if bytes.IndexByte(b, iac) >= 0 {
		b = bytes.ReplaceAll(b, []byte{iac}, []byte{iac, iac})
	}
	return c.writeRaw(b)
}

// writeRaw writes b to the underlying connection under the write mutex, so the
// reader's negotiation answers and the writer goroutine's output never interleave
// mid-frame on the wire.
func (c *Conn) writeRaw(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Phase 16.3: bound a single write so a wedged client (TCP recv window full, never draining) can't pin
	// this write — and thus the gate's writer goroutine — indefinitely. The deadline is per-write: set fresh,
	// cleared after, so it never leaks onto a later write. Only a net.Conn-backed writer honors it.
	if c.writeTimeout > 0 {
		if d, ok := c.w.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = d.SetWriteDeadline(time.Now().Add(c.writeTimeout))
			defer func() { _ = d.SetWriteDeadline(time.Time{}) }()
		}
	}
	_, err := c.w.Write(b)
	return err
}
