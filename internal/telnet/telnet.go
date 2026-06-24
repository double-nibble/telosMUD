// Package telnet provides a minimal line-oriented telnet codec for the gate:
// it strips/answers IAC negotiation and yields clean lines. Full option support
// (GMCP, MCCP, NAWS, TTYPE) lands in Phase 5 (docs/GMCP.md).
package telnet

import (
	"bufio"
	"io"
	"strings"
	"sync"
)

// Telnet command bytes (RFC 854).
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
// negotiation answers (from the reader) and output (from a writer goroutine) don't
// interleave on the wire.
type Conn struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex
}

func New(rw io.ReadWriter) *Conn {
	return &Conn{r: bufio.NewReader(rw), w: rw}
}

// NewReadWriter is for tests with separate reader/writer halves.
func NewReadWriter(r io.Reader, w io.Writer) *Conn {
	return &Conn{r: bufio.NewReader(r), w: w}
}

// ReadLine returns the next line of input with IAC sequences removed and CR/NUL
// stripped. A trailing partial line at EOF is returned with a nil error.
func (c *Conn) ReadLine() (string, error) {
	var line []byte
	for {
		b, err := c.r.ReadByte()
		if err != nil {
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

// handleIAC consumes one IAC sequence. IAC IAC appends a literal 0xFF to line;
// negotiations are refused; subnegotiations are skipped.
func (c *Conn) handleIAC(line *[]byte) error {
	cmd, err := c.r.ReadByte()
	if err != nil {
		return err
	}
	switch cmd {
	case iac:
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
// only to DO/WILL (never to DONT/WONT) avoids negotiation loops.
func (c *Conn) refuse(cmd, opt byte) {
	switch cmd {
	case doo:
		c.writeRaw([]byte{iac, wont, opt})
	case will:
		c.writeRaw([]byte{iac, dont, opt})
	}
}

func (c *Conn) skipSubneg() error {
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

// Write sends s, escaping any literal 0xFF bytes as IAC IAC.
func (c *Conn) Write(s string) error {
	if strings.IndexByte(s, iac) >= 0 {
		s = strings.ReplaceAll(s, "\xff", "\xff\xff")
	}
	return c.writeRaw([]byte(s))
}

func (c *Conn) writeRaw(b []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.w.Write(b)
	return err
}
