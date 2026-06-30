package gate

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// account.go — the gate's seam to telos-account (Phase 14, docs/ACCOUNT.md). The gate calls the account
// service to list an account's characters (the select menu) and — in later slices — redeem link codes,
// verify passphrases, and resolve SSH keys. A stub backs it when no account service is configured, so the
// pre-Phase-14 "type a name" login keeps working; a gRPC client backs it when cfg.AccountTarget is set.

// AccountClient is the gate-side account API. It grows per slice (14.5 VerifyPassphrase, 14.6 ResolveSSHKey);
// 14.1 landed ListCharacters, 14.2 adds RedeemLinkCode.
type AccountClient interface {
	ListCharacters(ctx context.Context, accountID string) ([]CharacterInfo, error)
	// RedeemLinkCode atomically consumes a link code, returning the account + its characters. found=false is
	// the clean "invalid/expired/already-redeemed" case (not an error).
	RedeemLinkCode(ctx context.Context, code, connInfo string) (accountID string, characters []CharacterInfo, found bool, err error)
	// IssueSessionAssertion mints the signed assertion the gate carries in Attach (Phase 14.3). An empty
	// string is returned (no error) when account has no signing key — the world then runs unverified.
	IssueSessionAssertion(ctx context.Context, accountID, characterID, sessionID string) (string, error)
	// VerifyPassphrase checks a name+passphrase login (Phase 14.5). ok=false is a clean auth failure (bad
	// credentials or locked out — reason carries which); the account id is returned on success.
	VerifyPassphrase(ctx context.Context, name, pass, connInfo string) (ok bool, accountID, reason string, err error)
	// Close releases any underlying connection (a no-op for the stub).
	Close() error
}

// CharacterInfo is the gate-side summary of a character returned by the account service.
type CharacterInfo struct {
	ID      string
	Name    string
	ZoneRef string
	RoomRef string
}

// stubAccountClient is the no-service fallback. It returns a single character whose name is the (legacy)
// connection-chosen name carried as the accountID, preserving today's "By what name shall you be known?"
// login until link codes (14.2) replace it.
type stubAccountClient struct{}

func (stubAccountClient) ListCharacters(_ context.Context, accountID string) ([]CharacterInfo, error) {
	return []CharacterInfo{{ID: accountID, Name: accountID}}, nil
}

// RedeemLinkCode is never reached on the stub (the gate only enters the link-code flow when a REAL account
// client is wired), but it satisfies the interface — and refuses cleanly if ever called.
func (stubAccountClient) RedeemLinkCode(_ context.Context, _, _ string) (string, []CharacterInfo, bool, error) {
	return "", nil, false, nil
}

// IssueSessionAssertion on the stub returns no token (the stub is the no-auth fallback).
func (stubAccountClient) IssueSessionAssertion(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}

// VerifyPassphrase on the stub always fails (the stub login is name-only).
func (stubAccountClient) VerifyPassphrase(_ context.Context, _, _, _ string) (bool, string, string, error) {
	return false, "", "bad_credentials", nil
}

func (stubAccountClient) Close() error { return nil }

// grpcAccountClient wraps the generated Account gRPC client.
type grpcAccountClient struct {
	cc  *grpc.ClientConn
	cli accountv1.AccountClient
}

// DialAccount opens a gRPC client to telos-account at target. The in-cluster hop is insecure transport — the
// world's trust comes from the signed session assertion (Phase 14.3), not this link; a cluster mTLS posture
// is a deployment concern, not the gate's.
func DialAccount(target string) (AccountClient, error) {
	cc, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	return &grpcAccountClient{cc: cc, cli: accountv1.NewAccountClient(cc)}, nil
}

func (g *grpcAccountClient) ListCharacters(ctx context.Context, accountID string) ([]CharacterInfo, error) {
	resp, err := g.cli.ListCharacters(ctx, &accountv1.ListCharactersRequest{AccountId: accountID})
	if err != nil {
		return nil, err
	}
	out := make([]CharacterInfo, 0, len(resp.GetCharacters()))
	for _, c := range resp.GetCharacters() {
		out = append(out, CharacterInfo{
			ID: c.GetId(), Name: c.GetName(), ZoneRef: c.GetZoneRef(), RoomRef: c.GetRoomRef(),
		})
	}
	return out, nil
}

func (g *grpcAccountClient) RedeemLinkCode(ctx context.Context, code, connInfo string) (string, []CharacterInfo, bool, error) {
	resp, err := g.cli.RedeemLinkCode(ctx, &accountv1.RedeemLinkCodeRequest{Code: code, ConnInfo: connInfo})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return "", nil, false, nil // invalid / expired / already redeemed — a clean miss, not an error
		}
		return "", nil, false, err
	}
	out := make([]CharacterInfo, 0, len(resp.GetCharacters()))
	for _, c := range resp.GetCharacters() {
		out = append(out, CharacterInfo{
			ID: c.GetId(), Name: c.GetName(), ZoneRef: c.GetZoneRef(), RoomRef: c.GetRoomRef(),
		})
	}
	return resp.GetAccountId(), out, true, nil
}

func (g *grpcAccountClient) IssueSessionAssertion(ctx context.Context, accountID, characterID, sessionID string) (string, error) {
	resp, err := g.cli.IssueSessionAssertion(ctx, &accountv1.IssueSessionAssertionRequest{
		AccountId: accountID, CharacterId: characterID, SessionId: sessionID,
	})
	if err != nil {
		return "", err
	}
	return resp.GetAssertion(), nil
}

func (g *grpcAccountClient) VerifyPassphrase(ctx context.Context, name, pass, connInfo string) (bool, string, string, error) {
	resp, err := g.cli.VerifyPassphrase(ctx, &accountv1.VerifyPassphraseRequest{Name: name, Passphrase: pass, ConnInfo: connInfo})
	if err != nil {
		return false, "", "", err
	}
	return resp.GetOk(), resp.GetAccountId(), resp.GetReason(), nil
}

// Close releases the gRPC connection.
func (g *grpcAccountClient) Close() error { return g.cc.Close() }

// --- login flow (Phase 14.2) ---------------------------------------------------------------------------

// login resolves the character name + account id to enter the world with. When a real account service is
// wired it runs the LINK-CODE bridge (ACCOUNT.md §4); otherwise it falls back to the legacy "type a name"
// prompt so a bare dev gate (no account service) still works. The accountID is "" on the legacy path. Returns
// ok=false when the connection drops or login aborts.
func (s *Server) login(tc *telnet.Conn, log *slog.Logger, remote string, encrypted bool) (name, accountID string, ok bool) {
	if !s.accountConfigured {
		name, ok = loginByName(tc, log)
		return name, "", ok
	}
	return s.loginAuthenticated(tc, log, remote, encrypted)
}

// loginByName is the pre-Phase-14 stand-in: read a name that is safe to render + safe as a targeting
// keyword, re-prompting on a bad one. Used only when no account service is configured.
func loginByName(tc *telnet.Conn, log *slog.Logger) (string, bool) {
	for {
		_ = tc.Write("By what name shall you be known? ")
		line, err := tc.ReadLine()
		if err != nil {
			log.Debug("connection closed before login", "err", err)
			return "", false
		}
		candidate := strings.TrimSpace(line)
		if reason, ok := validateName(candidate); !ok {
			log.Debug("login name rejected", "reason", reason)
			_ = tc.Write("\r\nThat name won't do: " + reason + "\r\n")
			continue
		}
		return candidate, true
	}
}

// loginAuthenticated prompts for a LINK CODE (bare or "connect <code>") or a PASSPHRASE login
// ("connect <name> <passphrase>"), and dispatches to the right backend. A failed attempt re-prompts; a
// dropped connection returns ok=false.
func (s *Server) loginAuthenticated(tc *telnet.Conn, log *slog.Logger, remote string, encrypted bool) (string, string, bool) {
	for {
		_ = tc.Write("Enter your link code, or 'connect <name> <passphrase>': ")
		line, err := tc.ReadLine()
		if err != nil {
			log.Debug("connection closed before login", "err", err)
			return "", "", false
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// Passphrase login: connect <name> <passphrase>.
		if strings.EqualFold(fields[0], "connect") && len(fields) == 3 {
			if name, account, ok := s.loginByPassphrase(tc, log, remote, fields[1], fields[2], encrypted); ok {
				return name, account, true
			}
			continue
		}
		// Otherwise a link code — the LAST token (covers bare "<code>" and "connect <code>").
		code := strings.ToUpper(fields[len(fields)-1])
		if name, account, ok := s.loginByCode(tc, log, remote, code); ok {
			return name, account, true
		}
		// loginByCode wrote the reason; loop to re-prompt. A dropped connection is caught by the next
		// ReadLine at the top of the loop, which returns ok=false.
	}
}

// loginByCode redeems a link code + selects a character. ok=false on a bad code (a reason was written) or a
// dropped connection.
func (s *Server) loginByCode(tc *telnet.Conn, log *slog.Logger, remote, code string) (string, string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	accountID, chars, found, err := s.account.RedeemLinkCode(ctx, code, remote)
	cancel()
	if err != nil {
		log.Warn("redeem link code failed", "err", err)
		_ = tc.Write("\r\nThe login service is unavailable right now. Please try again shortly.\r\n")
		return "", "", false
	}
	if !found {
		_ = tc.Write("\r\nThat code is invalid or has expired. Get a fresh one from the website.\r\n")
		return "", "", false
	}
	name, ok := selectCharacter(tc, chars)
	if !ok {
		return "", "", false
	}
	log.Debug("login via link code", "account", accountID, "character", name)
	return name, accountID, true
}

// loginByPassphrase authenticates `connect <name> <passphrase>`: the named character must exist + the
// passphrase verify. On success the player plays that character. A cleartext warning is shown (the passphrase
// just crossed a possibly-unencrypted wire); the transport-aware version lands with TLS/SSH (Phase 14.6).
func (s *Server) loginByPassphrase(tc *telnet.Conn, log *slog.Logger, remote, name, pass string, encrypted bool) (string, string, bool) {
	if reason, ok := validateName(name); !ok {
		_ = tc.Write("\r\nThat name won't do: " + reason + "\r\n")
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	authOK, account, reason, err := s.account.VerifyPassphrase(ctx, name, pass, remote)
	cancel()
	if err != nil {
		log.Warn("verify passphrase failed", "err", err)
		_ = tc.Write("\r\nThe login service is unavailable right now. Please try again shortly.\r\n")
		return "", "", false
	}
	if !authOK {
		if reason == "locked" {
			_ = tc.Write("\r\nToo many failed attempts. Please wait and try again later.\r\n")
		} else {
			_ = tc.Write("\r\nIncorrect name or passphrase.\r\n")
		}
		return "", "", false
	}
	if !encrypted {
		_ = tc.Write("\r\n(Note: a passphrase is visible on this UNENCRYPTED connection — prefer a link code, TLS, or SSH.)\r\n")
	}
	log.Debug("login via passphrase", "account", account, "character", name, "encrypted", encrypted)
	return name, account, true
}

// selectCharacter picks which character to play from the account's list: zero => a prompt to create one on
// the website (chargen-over-telnet lands later) and ok=false; one => that character; many => a numbered menu.
func selectCharacter(tc *telnet.Conn, chars []CharacterInfo) (string, bool) {
	switch len(chars) {
	case 0:
		_ = tc.Write("\r\nThis account has no characters yet. Create one on the website, then reconnect.\r\n")
		return "", false
	case 1:
		return chars[0].Name, true
	default:
		for {
			_ = tc.Write("\r\nChoose a character:\r\n")
			for i, c := range chars {
				_ = tc.Write(fmt.Sprintf("  %d) %s\r\n", i+1, c.Name))
			}
			_ = tc.Write("> ")
			line, err := tc.ReadLine()
			if err != nil {
				return "", false
			}
			n, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil || n < 1 || n > len(chars) {
				_ = tc.Write("\r\nPick a number from the list.\r\n")
				continue
			}
			return chars[n-1].Name, true
		}
	}
}
