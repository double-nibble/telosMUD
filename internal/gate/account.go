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
	// ResolveSSHKey maps an SSH key fingerprint to an account (Phase 14.6). found=false for an unknown key.
	ResolveSSHKey(ctx context.Context, fingerprint string) (found bool, accountID string, err error)
	// StartDeviceAuth begins a browser OAuth login (Phase 15), returning the device_code, the one-click link
	// to show the player, and the suggested poll interval.
	StartDeviceAuth(ctx context.Context, connInfo string) (deviceCode, verificationURI string, interval time.Duration, err error)
	// PollDeviceAuth reports the device-login status ("pending" | "authed" | "expired"); on "authed" it
	// returns the account + its characters.
	PollDeviceAuth(ctx context.Context, deviceCode string) (status, accountID string, characters []CharacterInfo, err error)
	// GetChargenFlow returns the content chargen flow the gate walks as prompts (Phase 15.4). configured=false
	// => no chargen (the gate offers no create).
	GetChargenFlow(ctx context.Context) (configured bool, steps []ChargenStep, options []ChargenBundleOption, err error)
	// CreateChargenCharacter validates a prompt-driven submission + creates the character. reason is a
	// non-empty user-facing message on a validation failure (name taken, over budget, at the cap, …).
	CreateChargenCharacter(ctx context.Context, accountID, name string, picks map[string]string, allocs map[string]map[string]int) (characterID, reason string, err error)
	// Close releases any underlying connection (a no-op for the stub).
	Close() error
}

// ChargenStep is the gate-side form of a content chargen step (a tagged union over Kind).
type ChargenStep struct {
	Kind, ID, Prompt, BundleKind string
	Attributes                   []string
	Points, Base, Min, Max       int
}

// ChargenBundleOption is a selectable bundle the gate lists for a bundle_choice step.
type ChargenBundleOption struct{ Ref, Kind, Label string }

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

// ResolveSSHKey on the stub never resolves (no account service).
func (stubAccountClient) ResolveSSHKey(_ context.Context, _ string) (bool, string, error) {
	return false, "", nil
}

// StartDeviceAuth/PollDeviceAuth are never reached on the stub (the gate only runs device login when a real
// account client is wired); they satisfy the interface.
func (stubAccountClient) StartDeviceAuth(_ context.Context, _ string) (string, string, time.Duration, error) {
	return "", "", 0, nil
}

func (stubAccountClient) PollDeviceAuth(_ context.Context, _ string) (string, string, []CharacterInfo, error) {
	return "expired", "", nil, nil
}

// GetChargenFlow/CreateChargenCharacter are never reached on the stub (no account service => no chargen).
func (stubAccountClient) GetChargenFlow(context.Context) (bool, []ChargenStep, []ChargenBundleOption, error) {
	return false, nil, nil, nil
}

func (stubAccountClient) CreateChargenCharacter(context.Context, string, string, map[string]string, map[string]map[string]int) (string, string, error) {
	return "", "character creation is unavailable", nil
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

func (g *grpcAccountClient) ResolveSSHKey(ctx context.Context, fingerprint string) (bool, string, error) {
	resp, err := g.cli.ResolveSSHKey(ctx, &accountv1.ResolveSSHKeyRequest{Fingerprint: fingerprint})
	if err != nil {
		return false, "", err
	}
	return resp.GetFound(), resp.GetAccountId(), nil
}

func (g *grpcAccountClient) StartDeviceAuth(ctx context.Context, connInfo string) (string, string, time.Duration, error) {
	resp, err := g.cli.StartDeviceAuth(ctx, &accountv1.StartDeviceAuthRequest{ConnInfo: connInfo})
	if err != nil {
		return "", "", 0, err
	}
	return resp.GetDeviceCode(), resp.GetVerificationUri(), time.Duration(resp.GetInterval()) * time.Second, nil
}

func (g *grpcAccountClient) PollDeviceAuth(ctx context.Context, deviceCode string) (string, string, []CharacterInfo, error) {
	resp, err := g.cli.PollDeviceAuth(ctx, &accountv1.PollDeviceAuthRequest{DeviceCode: deviceCode})
	if err != nil {
		return "", "", nil, err
	}
	out := make([]CharacterInfo, 0, len(resp.GetCharacters()))
	for _, c := range resp.GetCharacters() {
		out = append(out, CharacterInfo{ID: c.GetId(), Name: c.GetName(), ZoneRef: c.GetZoneRef(), RoomRef: c.GetRoomRef()})
	}
	return resp.GetStatus(), resp.GetAccountId(), out, nil
}

func (g *grpcAccountClient) GetChargenFlow(ctx context.Context) (bool, []ChargenStep, []ChargenBundleOption, error) {
	resp, err := g.cli.GetChargenFlow(ctx, &accountv1.GetChargenFlowRequest{})
	if err != nil {
		return false, nil, nil, err
	}
	steps := make([]ChargenStep, 0, len(resp.GetSteps()))
	for _, s := range resp.GetSteps() {
		steps = append(steps, ChargenStep{
			Kind: s.GetKind(), ID: s.GetId(), Prompt: s.GetPrompt(), BundleKind: s.GetBundleKind(),
			Attributes: s.GetAttributes(),
			Points:     int(s.GetPoints()), Base: int(s.GetBase()), Min: int(s.GetMin()), Max: int(s.GetMax()),
		})
	}
	opts := make([]ChargenBundleOption, 0, len(resp.GetOptions()))
	for _, o := range resp.GetOptions() {
		opts = append(opts, ChargenBundleOption{Ref: o.GetRef(), Kind: o.GetKind(), Label: o.GetLabel()})
	}
	return resp.GetConfigured(), steps, opts, nil
}

func (g *grpcAccountClient) CreateChargenCharacter(ctx context.Context, accountID, name string, picks map[string]string, allocs map[string]map[string]int) (string, string, error) {
	pa := make(map[string]*accountv1.AttrAlloc, len(allocs))
	for stepID, m := range allocs {
		vals := make(map[string]int32, len(m))
		for attr, v := range m {
			vals[attr] = int32(v) //nolint:gosec // chargen point-buy values are small content-bounded ints.
		}
		pa[stepID] = &accountv1.AttrAlloc{Values: vals}
	}
	resp, err := g.cli.CreateChargenCharacter(ctx, &accountv1.CreateChargenCharacterRequest{
		AccountId: accountID, Name: name, Picks: picks, Allocs: pa,
	})
	if err != nil {
		return "", "", err
	}
	return resp.GetCharacterId(), resp.GetReason(), nil
}

// Close releases the gRPC connection.
func (g *grpcAccountClient) Close() error { return g.cc.Close() }

// --- login flow (Phase 14.2) ---------------------------------------------------------------------------

// login resolves the character name + account id to enter the world with. When a real account service is
// wired it runs the LINK-CODE bridge (ACCOUNT.md §4); otherwise it falls back to the legacy "type a name"
// prompt so a bare dev gate (no account service) still works. The accountID is "" on the legacy path. Returns
// ok=false when the connection drops or login aborts.
func (s *Server) login(ctx context.Context, tc *telnet.Conn, log *slog.Logger, remote string, _ bool, preAuth string) (name, accountID string, ok bool) {
	if !s.accountConfigured {
		name, ok = loginByName(tc, log)
		return name, "", ok
	}
	// Phase 14.6: an SSH key already authenticated the account — skip straight to character select. (SSH is
	// removed in 15.5; until then this path stays.)
	if preAuth != "" {
		return s.loginPreAuthenticated(tc, log, preAuth)
	}
	// Phase 15: the terminal-native OAuth device flow — show a one-click link, poll until the browser auths.
	return s.loginViaDevice(ctx, tc, log, remote)
}

// deviceLoginTimeout bounds how long the gate waits for the player to complete the browser sign-in before
// giving up (matched to the device_code TTL).
const deviceLoginTimeout = 10 * time.Minute

// loginViaDevice runs the Phase-15 terminal OAuth flow: ask account for a device_code + a one-click link,
// show it, and poll until the browser completes OAuth (or the link expires / the player disconnects). On
// success it returns the account + the selected character. The connection ctx cancels the poll on disconnect.
func (s *Server) loginViaDevice(ctx context.Context, tc *telnet.Conn, log *slog.Logger, remote string) (string, string, bool) {
	ctx, cancel := context.WithTimeout(ctx, deviceLoginTimeout)
	defer cancel()

	startCtx, c := context.WithTimeout(ctx, 5*time.Second)
	device, uri, interval, err := s.account.StartDeviceAuth(startCtx, remote)
	c()
	if err != nil {
		log.Warn("StartDeviceAuth failed", "err", err)
		_ = tc.Write("\r\nThe login service is unavailable right now. Please try again later.\r\n")
		return "", "", false
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	// A bare URL on its own line: copy-pasteable everywhere, and auto-clickable in modern terminals that
	// linkify URLs. (OSC-8 hyperlinks are deferred — too many MUD clients render the escapes as junk.)
	_ = tc.Write("\r\nTo sign in, open this link in your browser:\r\n\r\n    " + uri + "\r\n\r\nWaiting for you to sign in (this page will tell you when to return)...\r\n")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = tc.Write("\r\nThe sign-in link expired. Please reconnect to get a new one.\r\n")
			return "", "", false
		case <-ticker.C:
			pollCtx, pc := context.WithTimeout(ctx, 5*time.Second)
			st, account, chars, err := s.account.PollDeviceAuth(pollCtx, device)
			pc()
			if err != nil {
				log.Debug("poll device auth (transient)", "err", err)
				continue // keep polling — a transient blip shouldn't drop the login
			}
			switch st {
			case "authed":
				name, ok := s.selectOrCreateCharacter(ctx, tc, log, account, chars)
				if !ok {
					return "", "", false
				}
				return name, account, true
			case "expired":
				_ = tc.Write("\r\nThe sign-in link expired. Please reconnect to get a new one.\r\n")
				return "", "", false
			}
			// "pending": keep waiting.
		}
	}
}

// loginPreAuthenticated handles an SSH-key login (the account is already proven by the key): list the
// account's characters and select one — no code/passphrase prompt.
func (s *Server) loginPreAuthenticated(tc *telnet.Conn, log *slog.Logger, account string) (string, string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	chars, err := s.account.ListCharacters(ctx, account)
	cancel()
	if err != nil {
		log.Warn("list characters (ssh) failed", "err", err)
		_ = tc.Write("\r\nThe login service is unavailable right now. Goodbye.\r\n")
		return "", "", false
	}
	name, ok := selectCharacter(tc, chars)
	if !ok {
		return "", "", false
	}
	log.Debug("login via ssh key", "account", account, "character", name)
	return name, account, true
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
