package gate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

// ssh.go — Phase-14.6b SSH transport (docs/ACCOUNT.md §6): the cleanest credential model (encrypted +
// keypair = identity). The gate runs an SSH server; on connect it maps the client's public-key fingerprint
// to an account (ResolveSSHKey). A KNOWN key pre-authenticates the account (login skips straight to character
// select); an UNKNOWN key still gets an encrypted channel and falls back to interactive login (link code /
// passphrase). The telnet/GMCP byte stream runs over the SSH session channel exactly as over TLS/plain —
// only the encryption layer differs (ACCOUNT.md §8).

// sshHostKey loads the configured host key, or generates an EPHEMERAL ed25519 one (dev) with a warning — a
// changing host key trips client warnings, so production should configure a stable file.
func sshHostKey(path string, log loggerLike) (ssh.Signer, error) {
	if path != "" {
		pem, err := os.ReadFile(path) //nolint:gosec // operator-provided host key path
		if err != nil {
			return nil, fmt.Errorf("gate: read ssh host key: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			return nil, fmt.Errorf("gate: parse ssh host key: %w", err)
		}
		return signer, nil
	}
	log.Warn("no SSH host key configured — generating an EPHEMERAL one (clients will see a changed-key warning across restarts)")
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// loggerLike is the tiny slice of *slog.Logger sshHostKey needs (kept narrow for testability).
type loggerLike interface{ Warn(string, ...any) }

// serveSSH runs the SSH accept loop. Each accepted TCP connection is handed an SSH handshake; the pubkey
// callback resolves the account (or leaves it empty for an unknown key) and the session channel becomes the
// player's I/O. The listener closes on ctx cancellation.
func (s *Server) serveSSH(ctx context.Context, ln net.Listener) {
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		nc, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				s.log.Warn("ssh accept error", "err", err)
				return
			}
		}
		go s.handleSSH(ctx, nc)
	}
}

// handleSSH performs the SSH handshake (resolving the key->account), accepts the session channel, and runs
// the player session over it (encrypted, pre-authenticated to the resolved account if the key was known).
func (s *Server) handleSSH(ctx context.Context, nc net.Conn) {
	cfg := &ssh.ServerConfig{
		// Accept ANY public key (so an unknown key still gets an encrypted channel for interactive login),
		// but record the resolved account in the permissions extensions for the pre-auth path. The key is a
		// transport identity here, not the sole gate — auth completes via the resolved account or the
		// interactive login over the encrypted channel.
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			fp := ssh.FingerprintSHA256(key)
			account := ""
			rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			found, acct, err := s.account.ResolveSSHKey(rctx, fp)
			cancel()
			if err != nil {
				s.log.Warn("ssh key resolve failed (continuing unauthenticated)", "err", err)
			} else if found {
				account = acct
			}
			return &ssh.Permissions{Extensions: map[string]string{"account": account}}, nil
		},
	}
	cfg.AddHostKey(s.sshSigner)

	sconn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		s.log.Debug("ssh handshake failed", "err", err)
		_ = nc.Close()
		return
	}
	defer func() { _ = sconn.Close() }()
	go ssh.DiscardRequests(reqs)

	account := ""
	if sconn.Permissions != nil {
		account = sconn.Permissions.Extensions["account"]
	}

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		ch, requests, err := newChan.Accept()
		if err != nil {
			s.log.Debug("ssh channel accept failed", "err", err)
			return
		}
		// Accept shell/pty requests so a MUD/SSH client gets its interactive terminal; ignore the rest.
		go func() {
			for req := range requests {
				switch req.Type {
				case "shell", "pty-req", "env", "window-change":
					_ = req.Reply(true, nil)
				default:
					_ = req.Reply(false, nil)
				}
			}
		}()
		// Run the player session over the SSH channel (encrypted; pre-authenticated if the key was known).
		s.handle(ctx, sshConn{Channel: ch, local: nc.LocalAddr(), remote: nc.RemoteAddr()}, true, account)
		return // one player session per SSH connection
	}
}

// sshConn adapts an ssh.Channel to net.Conn (the gate's telnet/GMCP stack reads/writes a net.Conn). The SSH
// layer has no per-op deadlines, so the deadline setters are no-ops; the connection ends when the channel or
// the underlying TCP conn closes.
type sshConn struct {
	ssh.Channel
	local  net.Addr
	remote net.Addr
}

func (c sshConn) LocalAddr() net.Addr            { return c.local }
func (c sshConn) RemoteAddr() net.Addr           { return c.remote }
func (sshConn) SetDeadline(time.Time) error      { return nil }
func (sshConn) SetReadDeadline(time.Time) error  { return nil }
func (sshConn) SetWriteDeadline(time.Time) error { return nil }
