package helpers

import (
	"bytes"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TelnetClient is a minimal scripted telnet/TCP client for the e2e tier: connect
// to the live gate, send lines, and poll the received stream for an expected
// substring with a deadline (NEVER a fixed sleep-and-hope, per the TEST STANDARD).
//
// It is the shared harness for tests/e2e (per docs/TESTING.md) — a black-box
// player driver that speaks the same plain-text protocol a human telnet user does.
// A background reader continuously drains the socket into an in-memory buffer with
// telnet IAC (0xFF) command sequences stripped, so a test asserts on the visible
// game text exactly as a player would see it. The buffer is append-only and
// mutex-guarded; Expect / ExpectFrom read snapshots of it.
type TelnetClient struct {
	conn net.Conn

	mu  sync.Mutex
	buf bytes.Buffer

	closeOnce sync.Once
	done      chan struct{}
}

// Dial connects to a live gate at addr (e.g. "localhost:4000") and starts the
// background reader. It returns an error rather than failing the test so the
// caller can decide whether an unreachable gate is a SKIP (e2e default) or a
// failure. The connection and reader are torn down via t.Cleanup.
func Dial(t *testing.T, addr string) (*TelnetClient, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c := &TelnetClient{conn: conn, done: make(chan struct{})}
	go c.readLoop()
	t.Cleanup(c.Close)
	return c, nil
}

// readLoop drains the socket into the buffer until the connection closes or Close
// is called. It strips telnet IAC negotiation: a 0xFF (IAC) byte and the two bytes
// that follow it (command + option) are dropped, so the buffer holds only the
// game's rendered text. This is a deliberately crude strip — the gate only ever
// sends a small, fixed set of negotiation sequences at connect — and it is enough
// to keep substring assertions clean.
func (c *TelnetClient) readLoop() {
	b := make([]byte, 4096)
	for {
		select {
		case <-c.done:
			return
		default:
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, err := c.conn.Read(b)
		if n > 0 {
			c.mu.Lock()
			for i := 0; i < n; i++ {
				if b[i] == 0xFF { // IAC: skip it and the next two bytes (command + option)
					i += 2
					continue
				}
				c.buf.WriteByte(b[i])
			}
			c.mu.Unlock()
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue // idle read window elapsed; keep draining
			}
			return // real read error (closed connection): stop the loop
		}
	}
}

// Send writes one line to the gate, appending the CRLF the line protocol expects.
// It is non-fatal on a write error (a closed connection surfaces as a failed
// Expect downstream, with the captured transcript for context).
func (c *TelnetClient) Send(line string) {
	_, _ = c.conn.Write([]byte(line + "\r\n"))
}

// Transcript returns a snapshot of everything received so far (IAC-stripped). Use
// it to build a failure message that shows the player what was actually rendered.
func (c *TelnetClient) Transcript() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Len returns the current received-byte count; pair it with ExpectFrom to assert
// only on output produced AFTER a command (so a substring already on screen from
// an earlier render does not produce a false positive).
func (c *TelnetClient) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Len()
}

// Expect polls the FULL received buffer for sub until it appears or timeout
// elapses. It returns true on success; the caller decides whether a miss is fatal.
// Polling (not sleeping) keeps the e2e deterministic against variable gate/handoff
// latency: it returns as soon as the text arrives.
func (c *TelnetClient) Expect(sub string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		found := bytes.Contains(c.buf.Bytes(), []byte(sub))
		c.mu.Unlock()
		if found {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ExpectFrom is Expect restricted to output received at or after byte offset `from`
// (typically a Len() captured just before the triggering Send). It scopes the
// assertion to the response of one command — the right tool for re-issuing `look`
// after a state change, where the same room name appears in both renders.
func (c *TelnetClient) ExpectFrom(from int, sub string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		var window []byte
		if from <= c.buf.Len() {
			window = c.buf.Bytes()[from:]
		}
		found := bytes.Contains(window, []byte(sub))
		c.mu.Unlock()
		if found {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ExpectAny polls for the FIRST of several substrings to appear, returning the one
// that matched (or "" on timeout). Use it to assert a contract with alternatives —
// e.g. the goblin died OR the player was slain — so a chaos/death test can branch
// on the observed outcome instead of hanging on a single expected string.
func (c *TelnetClient) ExpectAny(subs []string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		c.mu.Lock()
		body := c.buf.String()
		c.mu.Unlock()
		for _, s := range subs {
			if strings.Contains(body, s) {
				return s
			}
		}
		if time.Now().After(deadline) {
			return ""
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Close stops the reader and closes the connection. Idempotent; registered with
// t.Cleanup by Dial, so a test rarely calls it directly.
func (c *TelnetClient) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}
