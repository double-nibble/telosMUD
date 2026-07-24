package gate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/directory"
)

// transport_test.go — Phase 14.6a: the gate's transport posture. TLS terminates + serves the login banner;
// plain telnet is off by default; with no transport configured ListenAndServe errors rather than silently
// serving nothing.

// writeSelfSignedCert writes a throwaway ECDSA self-signed cert + key to temp files and returns their paths.
func writeSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// freePort grabs a free localhost address (binds + closes to learn the port).
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestTLSTransportServesLogin(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)
	addr := freePort(t)
	s := newServer(":0", directory.Static{Addr: "unused"}, newPool(), nil)
	s.WithTransports(false, addr, certFile, keyFile) // plain OFF, TLS ON

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.ListenAndServe(ctx) }()

	// Dial TLS and read until the welcome banner appears — proving the TLS listener terminates + handle runs.
	var conn *tls.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // self-signed test cert
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not dial TLS gate: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var acc strings.Builder
	buf := make([]byte, 256)
	for !strings.Contains(acc.String(), "Welcome to TelosMUD") {
		n, err := conn.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
		}
		if err != nil {
			t.Fatalf("reading TLS banner: %v (got %q)", err, acc.String())
		}
	}
}

func TestNoTransportConfiguredErrors(t *testing.T) {
	s := newServer(":0", nil, newPool(), nil) // plain off (default), no TLS
	err := s.ListenAndServe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no transport") {
		t.Fatalf("expected a no-transport error, got %v", err)
	}
}

// startTLSGate stands up a TLS-only gate on a free port and returns its address; the test's ctx tears it down.
func startTLSGate(ctx context.Context, t *testing.T) string {
	t.Helper()
	certFile, keyFile := writeSelfSignedCert(t)
	addr := freePort(t)
	s := newServer(":0", directory.Static{Addr: "unused"}, newPool(), nil)
	s.WithTransports(false, addr, certFile, keyFile) // plain OFF, TLS ON — the production posture
	go func() { _ = s.ListenAndServe(ctx) }()
	// Wait until the listener is actually accepting before returning (ListenAndServe binds asynchronously).
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("gate never came up at %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// drainToClose reads from conn under a deadline until the socket closes/errs, returning everything read plus
// the terminating error. Draining to close (vs stopping at a substring) is what lets a caller distinguish a
// real server close (io.EOF) from a leaked, never-closed socket (a deadline timeout).
func drainToClose(t *testing.T, conn net.Conn, within time.Duration) (string, error) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	var acc strings.Builder
	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
		}
		if err != nil {
			return acc.String(), err
		}
	}
}

// readAll accumulates from conn under a deadline until it either sees `want` or the socket closes/errs.
func readUntilOrClose(t *testing.T, conn net.Conn, want string) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	var acc strings.Builder
	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			acc.Write(buf[:n])
			if strings.Contains(acc.String(), want) {
				return acc.String()
			}
		}
		if err != nil {
			return acc.String()
		}
	}
}

// #486: a PLAINTEXT client that speaks first (a raw telnet/nc sending bytes) on the TLS-only port must get the
// "use TLS" line and a close — not a silent handshake-failure hang.
func TestPlaintextSpeakerOnTLSPortGetsMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := startTLSGate(ctx, t)

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Send plaintext bytes (as a bare telnet client / a `look` would) — first byte is not 0x16.
	if _, err := conn.Write([]byte("look\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Drain to CLOSE, accumulating the whole message. Draining to close — rather than reading until a
	// substring and then probing for "any non-nil error" — is what makes this non-vacuous: a leaked (never
	// closed) socket would block until the read deadline and yield a TIMEOUT, whereas a real server close is a
	// non-timeout error (io.EOF, or connection-reset because our unread `look\r\n` makes the OS send RST). So
	// deleting the reject-path nc.Close() turns this into a timeout and fails the test.
	got, readErr := drainToClose(t, conn, 4*time.Second)
	if !strings.Contains(got, "requires a secure (TLS/SSL) connection") {
		t.Fatalf("expected the use-TLS message, got %q", got)
	}
	if isTimeout(readErr) {
		t.Fatalf("expected the server to CLOSE the connection after the message; got a read timeout (leaked socket?): %v", readErr)
	}
}

// isTimeout reports whether err is a network deadline timeout — the signature of a socket the server never
// closed (as opposed to io.EOF / connection-reset, which are real closes).
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// #486: a client whose first byte IS 0x16 but which then sends non-TLS garbage must fail the handshake and be
// closed WITHOUT ever reaching the login path — it must never see the welcome banner. Guards that the sniff's
// 0x16 branch still terminates TLS properly (no plaintext-claiming-TLS client reaches player state).
func TestGarbageAfter0x16OnTLSPortNeverReachesLogin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := startTLSGate(ctx, t)

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Claim TLS (0x16) then send junk that is not a valid ClientHello — the handshake must fail.
	if _, err := conn.Write(append([]byte{0x16}, []byte("not a real ClientHello, just garbage bytes")...)); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := drainToClose(t, conn, 5*time.Second)
	if strings.Contains(got, "Welcome to TelosMUD") {
		t.Fatalf("a garbage-after-0x16 client reached the login banner — TLS was not enforced: %q", got)
	}
}

// #486: a SILENT plaintext client (connects and waits for a server-first banner, as telnet does) must also get
// the message after the sniff deadline — not hang forever. tlsSniffTimeout is shortened so the test is fast.
func TestSilentPlaintextClientOnTLSPortGetsMessage(t *testing.T) {
	orig := tlsSniffTimeout
	tlsSniffTimeout = 200 * time.Millisecond
	defer func() { tlsSniffTimeout = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := startTLSGate(ctx, t)

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	// Send NOTHING: the server must break the silence with the message after tlsSniffTimeout, then close.
	got := readUntilOrClose(t, conn, "requires a secure")
	if !strings.Contains(got, "requires a secure (TLS/SSL) connection") {
		t.Fatalf("expected the use-TLS message on a silent client, got %q", got)
	}
}

// #486 regression guard: a real TLS client still handshakes and reaches the login banner through the sniffing
// listener (the peeked first byte is replayed, so the ClientHello is intact). Mirrors TestTLSTransportServesLogin
// but asserts explicitly that the sniff path preserves the encrypted transport.
func TestTLSStillHandshakesThroughSniff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := startTLSGate(ctx, t)

	var conn *tls.Conn
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // self-signed test cert
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not dial TLS gate through the sniff: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer conn.Close()
	if got := readUntilOrClose(t, conn, "Welcome to TelosMUD"); !strings.Contains(got, "Welcome to TelosMUD") {
		t.Fatalf("TLS client did not reach the welcome banner through the sniff, got %q", got)
	}
}
