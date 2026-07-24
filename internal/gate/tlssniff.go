package gate

import (
	"context"
	"crypto/tls"
	"net"
	"time"
)

// tlssniff.go — #486: on the TLS-only port, TELL a plaintext client to use TLS instead of hanging.
//
// In production the gate binds the encrypted port ONLY (no plaintext listener). A client that connects
// without TLS — wrong port, or "TLS/SSL" left unchecked in the MUD client — sends bytes that aren't a valid
// TLS ClientHello, so a tls.Listen handshake fails on the first read and the socket is dropped with NO
// application data ever sent: the player sees an open connection that just hangs, no banner, no prompt. That
// was the top false "prod is down" alarm at the 2026-07-21 bring-up (the server is healthy; the client isn't
// speaking TLS). Staging runs plaintext, so the SAME client works there, compounding the confusion.
//
// The fix is a protocol sniff: bind a PLAIN net.Listener on the TLS address and peek the first byte of each
// connection. A TLS record always opens with 0x16 (the handshake content type) → wrap the connection back
// into a tls.Server and continue exactly as before (the peeked byte is replayed, so the handshake is
// byte-identical to tls.Listen's). Anything else → write one plaintext line telling the operator/player to
// enable TLS, and close. Only the TLS listener sniffs; the optional plaintext listener (staging) is untouched.
//
// The wrinkle is that TLS is client-speaks-first but telnet against this gate is SERVER-speaks-first (the gate
// offers GMCP + the welcome before the client sends anything). So a mis-configured plaintext client may send
// IAC negotiation first (non-0x16 → immediate reject) OR send nothing and wait. The peek therefore runs under
// a short read deadline: a real TLS client's ClientHello is immediate (zero added latency), while a silent
// telnet client waits tlsSniffTimeout for the "use TLS" line instead of hanging forever.

// tlsSniffTimeout bounds the first-byte peek. A TLS ClientHello arrives immediately, so this only delays the
// reject sent to a SILENT plaintext client (one that connects and waits for a server-first banner). Kept
// short so such a client isn't left waiting long, but long enough to absorb ordinary network jitter. A var,
// not a const, so the silent-client test can shorten it without a multi-second wait — safe because no test in
// this package runs t.Parallel(), so its mutation is fully serialized (thread it through the Server struct if
// that ever changes).
var tlsSniffTimeout = 4 * time.Second

// tlsHandshakeTimeout bounds the TLS handshake AFTER the sniff. A bare tls.Server handshakes LAZILY on the
// first I/O with NO read deadline (the sniff's deadline is cleared before the handshake, and telnet sets only
// a WRITE deadline), so a client that sends the single byte 0x16 then stalls would pin this goroutine + fd
// indefinitely on the blocked handshake read. Driving the handshake explicitly under this deadline bounds
// that case, closing a goroutine/fd-exhaustion vector and making the "a short deadline bounds a silent
// client" property hold for the 0x16-prefixed case too. Generous: a real handshake is sub-second; this only
// reaps a stalled or malicious one.
var tlsHandshakeTimeout = 15 * time.Second

// tlsRequiredMsg is the single line sent to a plaintext client on the TLS-only port before the socket closes.
// Plain ASCII + CRLF so it renders in a raw telnet/nc session (the exact client that hit this symptom).
const tlsRequiredMsg = "\r\nThis server requires a secure (TLS/SSL) connection. " +
	"Enable TLS/SSL in your MUD client and reconnect.\r\n"

// serveTLSListener runs the TLS port's accept loop over a PLAIN listener, sniffing each connection for a TLS
// ClientHello (see the file header). Mirrors serveListener's ctx-cancel-closes-the-listener shutdown, but
// each accepted connection is sniffed in its OWN goroutine so a slow/silent client can never stall the accept
// loop (the peek blocks up to tlsSniffTimeout).
func (s *Server) serveTLSListener(ctx context.Context, ln net.Listener, cfg *tls.Config) {
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				s.log.Warn("accept error", "err", err)
				return
			}
		}
		go s.sniffTLSAndHandle(ctx, conn, cfg)
	}
}

// sniffTLSAndHandle peeks the first byte of nc: a TLS handshake (0x16) is wrapped into a tls.Server and run
// through the normal handle() path (encrypted=true); anything else — a plaintext byte OR silence past the
// deadline — gets the one-line "use TLS" message and a close. Runs one-per-connection off the accept loop.
func (s *Server) sniffTLSAndHandle(ctx context.Context, nc net.Conn, cfg *tls.Config) {
	first, ok := peekFirstByte(nc, tlsSniffTimeout)
	if !ok || first != 0x16 {
		// Not a TLS ClientHello (plaintext bytes, or the client sent nothing and waited): tell them to use
		// TLS, then close. Debug-level: port scanners hit this port too, so it must not flood the logs.
		s.log.Debug("rejected plaintext connection on TLS port", "remote", nc.RemoteAddr().String())
		_ = nc.SetWriteDeadline(time.Now().Add(tlsSniffTimeout))
		_, _ = nc.Write([]byte(tlsRequiredMsg))
		_ = nc.Close()
		return
	}
	// A TLS client: replay the peeked byte into a fresh tls.Server and DRIVE the handshake under a deadline
	// before handing off. A bare tls.Server would handshake lazily on the first I/O with no read deadline, so
	// a client that sends 0x16 then stalls would pin this goroutine + fd forever; bounding it here reaps that
	// case. HandshakeContext also honors ctx, so a shard shutdown aborts an in-flight handshake.
	tc := tls.Server(&peekConn{Conn: nc, peeked: []byte{first}}, cfg)
	_ = tc.SetDeadline(time.Now().Add(tlsHandshakeTimeout))
	if err := tc.HandshakeContext(ctx); err != nil {
		s.log.Debug("TLS handshake failed on TLS port", "remote", nc.RemoteAddr().String(), "err", err)
		_ = tc.Close()
		return
	}
	_ = tc.SetDeadline(time.Time{}) // clear the handshake deadline; handle()/telnet install their own timeouts
	// handle owns the close (its deferred nc.Close), symmetric to serveListener.
	s.handle(ctx, tc, true)
}

// peekFirstByte reads exactly one byte from nc under a read deadline, returning (byte, true) on success or
// (0, false) on timeout/EOF/error (all treated as "not TLS" by the caller — a client that sends nothing or
// drops is not speaking TLS). The deadline is cleared before returning so the subsequent tls.Server
// handshake / handle path runs with no residual deadline.
func peekFirstByte(nc net.Conn, timeout time.Duration) (byte, bool) {
	_ = nc.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = nc.SetReadDeadline(time.Time{}) }()
	var b [1]byte
	// A byte may arrive alongside a non-nil err (e.g. a single-byte write then close); n==1 is authoritative.
	if n, _ := nc.Read(b[:]); n == 1 {
		return b[0], true
	}
	return 0, false
}

// peekConn replays bytes consumed during the TLS sniff before delegating to the underlying connection, so
// tls.Server sees the ClientHello from its true first byte. Only Read is overridden; every other net.Conn
// method (Write, Close, deadlines, addrs) passes straight through to the embedded conn.
type peekConn struct {
	net.Conn
	peeked []byte
}

func (p *peekConn) Read(b []byte) (int, error) {
	if len(p.peeked) > 0 {
		n := copy(b, p.peeked)
		p.peeked = p.peeked[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}
