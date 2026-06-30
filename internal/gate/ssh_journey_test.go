package gate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/double-nibble/telosmud/internal/directory"
)

// ssh_journey_test.go — Phase 14.6b: a real SSH client connects with a public key over the gate's SSH
// transport. A KNOWN key fingerprint pre-authenticates its account (login goes straight to character select,
// no prompt); an UNKNOWN key still gets the encrypted channel and falls back to interactive login. (The
// post-login world spawn is the SAME handle path the other journey tests cover.)

// sshFakeAccount resolves one fingerprint to an account; ListCharacters returns `chars` for that account.
type sshFakeAccount struct {
	fingerprint string
	account     string
	chars       []CharacterInfo
}

func (f sshFakeAccount) ListCharacters(_ context.Context, account string) ([]CharacterInfo, error) {
	if account == f.account {
		return f.chars, nil
	}
	return nil, nil
}

func (f sshFakeAccount) ResolveSSHKey(_ context.Context, fingerprint string) (bool, string, error) {
	if fingerprint == f.fingerprint {
		return true, f.account, nil
	}
	return false, "", nil
}

func (sshFakeAccount) RedeemLinkCode(context.Context, string, string) (string, []CharacterInfo, bool, error) {
	return "", nil, false, nil
}

func (sshFakeAccount) IssueSessionAssertion(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (sshFakeAccount) VerifyPassphrase(context.Context, string, string, string) (bool, string, string, error) {
	return false, "", "bad_credentials", nil
}
func (sshFakeAccount) Close() error { return nil }

// sshClientKey makes a client signer + its SHA256 fingerprint.
func sshClientKey(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return signer, ssh.FingerprintSHA256(sshPub)
}

// dialSSHShell starts the gate's SSH server (with the given account fake), dials it with `signer`, opens a
// shell, and returns the session's stdout. The gate + a midgaard shard are wired via the harness.
func dialSSHShell(t *testing.T, signer ssh.Signer, acct AccountClient) interface{ Read([]byte) (int, error) } {
	t.Helper()
	const addr = "addr-a"
	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})
	h.srv.WithAccountClient(acct)

	sshAddr := freePort(t)
	h.srv.WithSSH(sshAddr, "") // ephemeral host key

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = h.srv.ListenAndServe(ctx) }()

	var client *ssh.Client
	deadline := time.Now().Add(8 * time.Second)
	for {
		c, derr := ssh.Dial("tcp", sshAddr, &ssh.ClientConfig{
			User:            "player",
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test: ephemeral host key
			Timeout:         2 * time.Second,
		})
		if derr == nil {
			client = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not SSH-dial the gate: %v", derr)
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Cleanup(func() { _ = client.Close() })

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	return stdout
}

// TestSSHKnownKeyPreAuthenticates: a registered key resolves its account and the login goes STRAIGHT to
// character select — no link-code/passphrase prompt. With no characters, the account-has-none notice proves
// the pre-auth path ran (it listed the resolved account's characters).
func TestSSHKnownKeyPreAuthenticates(t *testing.T) {
	signer, fp := sshClientKey(t)
	// Two characters so the gate presents the SELECT MENU and waits (holding the channel open) — a reliable
	// read point that proves the pre-auth path listed the resolved account's characters.
	chars := []CharacterInfo{{ID: "c1", Name: "Aragorn"}, {ID: "c2", Name: "Legolas"}}
	stdout := dialSSHShell(t, signer, sshFakeAccount{fingerprint: fp, account: "acct-1", chars: chars})

	got := readUntil(t, stdout, "Choose a character", 8*time.Second)
	if !strings.Contains(got, "Choose a character") || !strings.Contains(got, "Aragorn") {
		t.Fatalf("a known SSH key should pre-authenticate + reach character select; got %q", got)
	}
	if strings.Contains(got, "Enter your link code") {
		t.Fatalf("a known SSH key must NOT fall back to the link-code prompt; got %q", got)
	}
}

// TestSSHUnknownKeyFallsBackToInteractive: an unregistered key still gets the encrypted channel, and login
// falls back to the interactive prompt (link code / passphrase).
func TestSSHUnknownKeyFallsBackToInteractive(t *testing.T) {
	signer, _ := sshClientKey(t) // its fingerprint is NOT the one the fake knows
	stdout := dialSSHShell(t, signer, sshFakeAccount{fingerprint: "SHA256:bogus", account: "acct-1"})

	got := readUntil(t, stdout, "Enter your link code", 8*time.Second)
	if !strings.Contains(got, "Enter your link code") {
		t.Fatalf("an unknown SSH key should fall back to the interactive login prompt; got %q", got)
	}
}

// readUntil drains r in a background goroutine and polls the accumulated output until substr appears or the
// deadline elapses — robust to partial reads and a blocking final read.
func readUntil(t *testing.T, r interface{ Read([]byte) (int, error) }, substr string, within time.Duration) string {
	t.Helper()
	var mu sync.Mutex
	var acc strings.Builder
	go func() {
		buf := make([]byte, 256)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				mu.Lock()
				acc.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	deadline := time.After(within)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			mu.Lock()
			defer mu.Unlock()
			return acc.String()
		case <-tick.C:
			mu.Lock()
			s := acc.String()
			mu.Unlock()
			if strings.Contains(s, substr) {
				return s
			}
		}
	}
}
