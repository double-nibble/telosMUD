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
