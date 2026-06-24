// Package telnet provides a minimal line-oriented telnet codec for the gate:
// it strips/answers IAC negotiation and yields clean lines. Full option support
// (GMCP, MCCP, NAWS, TTYPE) lands in Phase 5 (docs/GMCP.md).
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
// # Negotiation policy: refuse everything
//
// Phase 1 wants the client in plain line mode. We never *request* an option, and
// we decline every option the client offers, so the connection stays in the
// default NVT (network virtual terminal) line discipline. See refuse for why we
// only answer DO/WILL and stay silent on DONT/WONT.
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
	"io"
	"log/slog"
	"strings"
	"sync"
)

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

// Conn wraps a connection with telnet line decoding. Writes are mutex-guarded so
// negotiation answers (emitted from the reader goroutine inside ReadLine) and
// output (emitted from the gate's writer goroutine) don't interleave on the wire.
// The two-goroutine model means writeRaw can be called concurrently from both
// sides; the mutex makes each frame atomic.
type Conn struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex // guards w against concurrent reader-vs-writer writes
}

// New wraps a bidirectional connection (the common case: a net.Conn straight
// from Accept). Reads are buffered; writes go to the same underlying stream.
func New(rw io.ReadWriter) *Conn {
	return &Conn{r: bufio.NewReader(rw), w: rw}
}

// NewReadWriter is for tests with separate reader/writer halves, so a test can
// feed a scripted byte stream in and inspect the negotiation answers written out.
func NewReadWriter(r io.Reader, w io.Writer) *Conn {
	return &Conn{r: bufio.NewReader(r), w: w}
}

// ReadLine returns the next line of input with IAC sequences removed and CR/NUL
// stripped. A trailing partial line at EOF is returned with a nil error.
//
// It reads a byte at a time: an IAC byte hands off to handleIAC (which may write
// a negotiation answer back); CR and NUL are dropped so both CR LF and CR NUL
// line endings collapse cleanly; LF terminates the line. Every other byte is
// part of the line the player typed. This is the first hop of the input path:
// bytes on the socket -> a clean line -> the gate forwards it as an InputLine on
// the Play stream.
func (c *Conn) ReadLine() (string, error) {
	var line []byte
	for {
		b, err := c.r.ReadByte()
		if err != nil {
			// EOF mid-line: hand back what we have so a final unterminated
			// line isn't silently lost.
			if len(line) > 0 && err == io.EOF {
				return string(line), nil
			}
			return "", err
		}
		if b == iac {
			if err := c.handleIAC(&line); err != nil {
				return "", err
			}
			continue
		}
		switch b {
		case '\r', 0:
			// ignore CR and NUL (CR LF / CR NUL line endings)
		case '\n':
			return string(line), nil
		default:
			line = append(line, b)
		}
	}
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
		c.refuse(cmd, opt)
	case sb:
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
	switch cmd {
	case doo:
		c.writeRaw([]byte{iac, wont, opt})
	case will:
		c.writeRaw([]byte{iac, dont, opt})
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
func (c *Conn) Write(s string) error {
	if strings.IndexByte(s, iac) >= 0 {
		s = strings.ReplaceAll(s, "\xff", "\xff\xff")
	}
	return c.writeRaw([]byte(s))
}

// writeRaw writes b to the underlying connection under the write mutex, so the
// reader's negotiation answers and the writer goroutine's output never interleave
// mid-frame on the wire.
func (c *Conn) writeRaw(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.w.Write(b)
	return err
}
