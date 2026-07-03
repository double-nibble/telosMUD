package gate

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// account.go — the gate's seam to telos-account (docs/ACCOUNT.md). Auth is OAuth-only (Phase 15): the gate
// runs the terminal device flow, then prompt-driven character select/create, and issues the session assertion
// it carries in Attach. A stub backs it when no account service is configured (the bare dev "type a name"
// login); a gRPC client backs it when cfg.AccountTarget is set.

// AccountClient is the gate-side account API (OAuth device flow + chargen + the session assertion).
type AccountClient interface {
	ListCharacters(ctx context.Context, accountID string) ([]CharacterInfo, error)
	// IssueSessionAssertion mints the signed assertion the gate carries in Attach (Phase 14.3). An empty
	// string is returned (no error) when account has no signing key — the world then runs unverified.
	IssueSessionAssertion(ctx context.Context, accountID, characterID, sessionID string) (string, error)
	// StartDeviceAuth begins a browser OAuth login (Phase 15), returning the device_code, the one-click link
	// to show the player, and the suggested poll interval.
	StartDeviceAuth(ctx context.Context, connInfo string) (deviceCode, verificationURI string, interval time.Duration, err error)
	// PollDeviceAuth reports the device-login status ("pending" | "authed" | "expired"); on "authed" it
	// returns the account + its characters.
	PollDeviceAuth(ctx context.Context, deviceCode string) (status, accountID string, characters []CharacterInfo, err error)
	// GetChargenFlow returns the content chargen flow the gate walks as prompts + the per-account character cap
	// (Phase 15.4). configured=false => no chargen (the gate offers no create).
	GetChargenFlow(ctx context.Context) (configured bool, steps []ChargenStep, options []ChargenBundleOption, maxCharacters int, err error)
	// CreateChargenCharacter validates a prompt-driven submission + creates the character. reason is a
	// non-empty user-facing message on a validation failure; atCapacity=true means the account is full (the
	// gate returns to character SELECT rather than re-running chargen).
	CreateChargenCharacter(ctx context.Context, accountID, name string, picks map[string]string, allocs map[string]map[string]int) (characterID, reason string, atCapacity bool, err error)
	// SetAccountTier is the promote/demote call (#27): change targetCharacter's account tier. Authz is
	// enforced at the account service (the actor must be an admin per the store), so the gate just passes the
	// actor's account id. ok=false with a user-facing reason on refusal; oldTier is the prior tier on success.
	SetAccountTier(ctx context.Context, actorAccountID, targetCharacter, newTier string) (ok bool, reason, oldTier string, err error)
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

// stubAccountClient is the no-service fallback. It returns a single character whose name is the
// connection-chosen name carried as the accountID, preserving the bare "By what name shall you be known?"
// dev login used when no account service is wired.
type stubAccountClient struct{}

func (stubAccountClient) ListCharacters(_ context.Context, accountID string) ([]CharacterInfo, error) {
	return []CharacterInfo{{ID: accountID, Name: accountID}}, nil
}

// IssueSessionAssertion on the stub returns no token (the stub is the no-auth fallback).
func (stubAccountClient) IssueSessionAssertion(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}

// SetAccountTier on the stub refuses: without an account service there is no tier authority (the dev/no-auth
// path has no accounts).
func (stubAccountClient) SetAccountTier(_ context.Context, _, _, _ string) (bool, string, string, error) {
	return false, "Trust tiers require an account service.", "", nil
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
func (stubAccountClient) GetChargenFlow(context.Context) (bool, []ChargenStep, []ChargenBundleOption, int, error) {
	return false, nil, nil, 0, nil
}

func (stubAccountClient) CreateChargenCharacter(context.Context, string, string, map[string]string, map[string]map[string]int) (string, string, bool, error) {
	return "", "character creation is unavailable", false, nil
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

func (g *grpcAccountClient) IssueSessionAssertion(ctx context.Context, accountID, characterID, sessionID string) (string, error) {
	resp, err := g.cli.IssueSessionAssertion(ctx, &accountv1.IssueSessionAssertionRequest{
		AccountId: accountID, CharacterId: characterID, SessionId: sessionID,
	})
	if err != nil {
		return "", err
	}
	return resp.GetAssertion(), nil
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

func (g *grpcAccountClient) GetChargenFlow(ctx context.Context) (bool, []ChargenStep, []ChargenBundleOption, int, error) {
	resp, err := g.cli.GetChargenFlow(ctx, &accountv1.GetChargenFlowRequest{})
	if err != nil {
		return false, nil, nil, 0, err
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
	return resp.GetConfigured(), steps, opts, int(resp.GetMaxCharacters()), nil
}

func (g *grpcAccountClient) CreateChargenCharacter(ctx context.Context, accountID, name string, picks map[string]string, allocs map[string]map[string]int) (string, string, bool, error) {
	pa := make(map[string]*accountv1.AttrAlloc, len(allocs))
	for stepID, m := range allocs {
		vals := make(map[string]int32, len(m))
		for attr, v := range m {
			// Chargen point-buy values are small content-bounded ints (telos-account re-validates
			// against the flow), but saturate the wire conversion anyway so an absurd value can't
			// wrap (CodeQL go/incorrect-integer-conversion).
			vals[attr] = clampInt32(v)
		}
		pa[stepID] = &accountv1.AttrAlloc{Values: vals}
	}
	resp, err := g.cli.CreateChargenCharacter(ctx, &accountv1.CreateChargenCharacterRequest{
		AccountId: accountID, Name: name, Picks: picks, Allocs: pa,
	})
	if err != nil {
		return "", "", false, err
	}
	return resp.GetCharacterId(), resp.GetReason(), resp.GetAtCapacity(), nil
}

// SetAccountTier calls the account service's promote/demote RPC (#27). Authz is enforced service-side.
func (g *grpcAccountClient) SetAccountTier(ctx context.Context, actorAccountID, targetCharacter, newTier string) (bool, string, string, error) {
	resp, err := g.cli.SetAccountTier(ctx, &accountv1.SetAccountTierRequest{
		ActorAccountId: actorAccountID, TargetCharacter: targetCharacter, NewTier: newTier,
	})
	if err != nil {
		return false, "", "", err
	}
	return resp.GetOk(), resp.GetReason(), resp.GetOldTier(), nil
}

// Close releases the gRPC connection.
func (g *grpcAccountClient) Close() error { return g.cc.Close() }

// --- login flow (Phase 15 OAuth device flow) -------------------------------------------------------------

// login resolves the character name + account id to enter the world with. When a real account service is
// wired it runs the Phase-15 terminal-native OAuth device flow; otherwise it falls back to the bare "type a
// name" login so a dev gate with no account service still works. The accountID is "" on the legacy path.
// Returns ok=false when the connection drops or login aborts.
func (s *Server) login(ctx context.Context, tc *telnet.Conn, log *slog.Logger, remote string, _ bool) (name, accountID string, ok bool) {
	// No account service, or the dev/test bypass (TELOS_DEV_AUTOAUTH): the bare name login, no OAuth.
	if !s.accountConfigured || s.devAutoAuth {
		name, ok = loginByName(tc, log)
		return name, "", ok
	}
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

// clampInt32 saturates an int to the int32 range for a wire conversion (rather than wrapping).
func clampInt32(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int32(v)
}
