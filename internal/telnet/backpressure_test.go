package telnet

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestWriteTimeoutFiresOnWedgedClient is the Phase-16.3 enforcement primitive: a client whose peer never
// reads (a wedged/dead socket) must not pin a write — and thus the gate's writer goroutine — forever. With a
// write timeout set, Write returns an error once the deadline elapses, which is the signal the gate uses to
// close the socket and reclaim the slot.
func TestWriteTimeoutFiresOnWedgedClient(t *testing.T) {
	cli, srv := net.Pipe() // unbuffered: a Write blocks until the peer Reads
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })

	c := New(srv)
	c.SetWriteTimeout(50 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- c.Write("output to a client that never reads") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a write-deadline error when the client never reads")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not return after its deadline; it is pinned on the wedged client")
	}
}

// TestWriteSucceedsWhenClientDrains is the positive control: the deadline only bites a STALLED peer. A client
// that keeps reading sees its writes complete normally even with a timeout configured.
func TestWriteSucceedsWhenClientDrains(t *testing.T) {
	cli, srv := net.Pipe()
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })
	go func() { _, _ = io.Copy(io.Discard, cli) }() // drain forever

	c := New(srv)
	c.SetWriteTimeout(time.Second)
	if err := c.Write("hello, attentive client"); err != nil {
		t.Fatalf("write to a draining client failed: %v", err)
	}
}

// TestNoWriteTimeoutLeavesWritesUnbounded confirms the default (0) preserves the pre-16.3 behavior: with no
// timeout set, a write to a draining client still works and no deadline is imposed (tests on a plain pipe
// rely on this).
func TestNoWriteTimeoutLeavesWritesUnbounded(t *testing.T) {
	cli, srv := net.Pipe()
	t.Cleanup(func() { _ = cli.Close(); _ = srv.Close() })
	go func() { _, _ = io.Copy(io.Discard, cli) }()

	c := New(srv) // no SetWriteTimeout
	if err := c.Write("unbounded write"); err != nil {
		t.Fatalf("unbounded write failed: %v", err)
	}
}
