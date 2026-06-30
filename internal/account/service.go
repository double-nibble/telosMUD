// Package account implements telos-account: the accounts/auth service (docs/ACCOUNT.md). It is the only
// service that touches OAuth providers + credentials; the gate reaches it over the Account gRPC API. Phase
// 14.1 lands the service skeleton + the character RPCs (list/reserve/create); the auth-backend RPCs
// (link codes, passphrase, SSH) return Unimplemented until their slices (14.2/14.5/14.6).
package account

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/assertion"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/passphrase"
	"github.com/double-nibble/telosmud/internal/store"
)

// assertionTTL is how long an issued session assertion is valid (short-lived, ACCOUNT.md §9). It only needs
// to cover the connect handshake (gate -> world Attach), so a few minutes is generous.
const assertionTTL = 5 * time.Minute

// CharStore is the persistence surface the service needs (the subset of store.Pool it calls). An interface
// so tests can drive the service with an in-memory fake — no Postgres required for the RPC-shape tests.
type CharStore interface {
	AccountCharacters(ctx context.Context, accountID string) ([]store.CharacterSummary, error)
	NameAvailable(ctx context.Context, name string) (bool, error)
	CreateAccountCharacter(ctx context.Context, accountID, name, zoneRef, roomRef string, state, chargen []byte) (string, error)
	// CreateCharacterWithChargen (Phase 14.8) creates a character carrying the first-spawn chargen marker.
	CreateCharacterWithChargen(ctx context.Context, accountID, name, zoneRef, roomRef string, bundles []string, attrs map[string]float64) (string, error)
	// Phase 14.5 passphrase auth.
	CharacterAccount(ctx context.Context, name string) (string, bool, error)
	AccountAuth(ctx context.Context, accountID string) (store.PassphraseAuth, bool, error)
	SetPassphraseHash(ctx context.Context, accountID, hash string) error
	RecordAuthFailure(ctx context.Context, accountID string, lockAfter int, lockFor time.Duration) (int, error)
	ResetAuthFailures(ctx context.Context, accountID string) error
	// Phase 14.6 SSH key auth.
	ResolveSSHKey(ctx context.Context, fingerprint string) (string, bool, error)
}

// Passphrase-auth tuning (Phase 14.5): lock an account after this many consecutive failures, for this long.
const (
	passphraseLockAfter = 5
	passphraseLockFor   = 5 * time.Minute
)

// Service implements the Account gRPC server. It is transport-thin: validation + a store call + a mapping to
// the proto types. The auth backends (Redis link codes, Argon2id, SSH) attach to it in later slices.
type Service struct {
	accountv1.UnimplementedAccountServer
	store      CharStore
	codes      LinkCodeStore      // Phase 14.2 link codes; nil => Mint/RedeemLinkCode are Unavailable
	signKey    ed25519.PrivateKey // Phase 14.3 assertion signing key; nil => IssueSessionAssertion returns ""
	hashParams passphrase.Params  // Phase 14.5 Argon2id cost
	ipThrottle *ipThrottle        // Phase 14.5 per-IP login throttle
	now        func() time.Time   // injectable clock (assertion expiry, lockout); defaults to time.Now
	log        *slog.Logger
	// startZone/startRoom are the default spawn location a freshly-created character lands in (the demo
	// pack's start room). Config-supplied; later chargen (14.8) may let content choose a starting zone.
	startZone string
	startRoom string
	// Chargen (Phase 14.8): the content flow + selectable bundle options the website renders + validates
	// against. Empty (chargenOK=false) => the website falls back to a bare name-only create.
	chargenFlow    content.ChargenDTO
	chargenOptions []content.ChargenBundleOption
	chargenKind    map[string]string // bundle ref -> kind (the bundle_choice legality check)
	chargenOK      bool
	// Device auth (Phase 15): the device-session store + the verification base URL the gate's one-click link
	// points at. nil store => StartDeviceAuth/PollDeviceAuth are Unavailable.
	deviceAuth    DeviceAuthStore
	verifyBaseURL string
}

// New builds the Account service over a character store.
func New(cs CharStore, log *slog.Logger, startZone, startRoom string) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store: cs, log: log, now: time.Now,
		hashParams: passphrase.DefaultParams,
		ipThrottle: newIPThrottle(20, time.Minute), // 20 passphrase attempts/min per source IP
		startZone:  startZone, startRoom: startRoom,
	}
}

// WithChargen wires the content chargen flow + selectable bundle options (Phase 14.8). The website reads them
// to render + validate the signup form. Without it, the website offers a bare name-only create.
func (s *Service) WithChargen(flow content.ChargenDTO, options []content.ChargenBundleOption) *Service {
	s.chargenFlow = flow
	s.chargenOptions = options
	s.chargenKind = make(map[string]string, len(options))
	for _, o := range options {
		s.chargenKind[o.Ref] = o.Kind
	}
	s.chargenOK = len(flow.Steps) > 0
	return s
}

// ChargenFlow returns the content chargen flow + bundle options + whether chargen is configured (the website
// renders the form from these).
func (s *Service) ChargenFlow() (content.ChargenDTO, []content.ChargenBundleOption, bool) {
	return s.chargenFlow, s.chargenOptions, s.chargenOK
}

// BuildCharacter validates a chargen submission (name + the flow's picks/allocations) and creates the
// character with its first-spawn marker. It returns a non-empty user-facing reason for a validation failure
// (err nil); err is reserved for an internal failure. The website calls it in-process (the gate has no
// telnet chargen yet).
func (s *Service) BuildCharacter(ctx context.Context, accountID, name string, picks map[string]string, allocs map[string]map[string]int) (id string, reason string, err error) {
	if accountID == "" {
		return "", "", status.Error(codes.InvalidArgument, "account_id required")
	}
	if r, ok := ValidateCharacterName(name); !ok {
		return "", r, nil
	}
	bundles, attrs, r := content.ValidateChargen(s.chargenFlow, picks, allocs, s.chargenKind)
	if r != "" {
		return "", r, nil
	}
	cid, err := s.store.CreateCharacterWithChargen(ctx, accountID, name, s.startZone, s.startRoom, bundles, attrs)
	if err != nil {
		if errors.Is(err, store.ErrNameTaken) {
			return "", "That name is already taken.", nil
		}
		s.log.Error("BuildCharacter", "account", accountID, "name", name, "err", err)
		return "", "", status.Error(codes.Internal, "create character failed")
	}
	return cid, "", nil
}

// WithLinkCodes wires the link-code store (Phase 14.2). Without it, Mint/RedeemLinkCode return Unavailable.
func (s *Service) WithLinkCodes(codes LinkCodeStore) *Service {
	s.codes = codes
	return s
}

// WithDeviceAuth wires the device-auth store + the verification base URL the gate's one-click link points at
// (Phase 15). Without it, StartDeviceAuth/PollDeviceAuth return Unavailable.
func (s *Service) WithDeviceAuth(store DeviceAuthStore, verifyBaseURL string) *Service {
	s.deviceAuth = store
	s.verifyBaseURL = strings.TrimRight(verifyBaseURL, "/")
	return s
}

// AuthorizeDevice is the BROKER-facing callback (in-process, called from the web auth bridge): once the
// browser completes OAuth and the identity resolves to an account, it flips the pending device session to
// authed so the gate's poll picks it up. ok=false means the device code is unknown/expired (stale/forged link).
func (s *Service) AuthorizeDevice(ctx context.Context, deviceCode, accountID string) (bool, error) {
	if s.deviceAuth == nil {
		return false, status.Error(codes.Unavailable, "device auth not configured")
	}
	return s.deviceAuth.Authorize(ctx, deviceCode, accountID)
}

// StartDeviceAuth mints a device_code + the one-click verification link the gate shows the player (Phase 15).
func (s *Service) StartDeviceAuth(ctx context.Context, _ *accountv1.StartDeviceAuthRequest) (*accountv1.StartDeviceAuthResponse, error) {
	if s.deviceAuth == nil {
		return nil, status.Error(codes.Unavailable, "device auth not configured")
	}
	code, err := s.deviceAuth.Start(ctx, deviceCodeTTL)
	if err != nil {
		s.log.Error("StartDeviceAuth", "err", err)
		return nil, status.Error(codes.Internal, "start device auth failed")
	}
	return &accountv1.StartDeviceAuthResponse{
		DeviceCode:      code,
		VerificationUri: s.verifyBaseURL + "/login/" + code,
		ExpiresIn:       int32(deviceCodeTTL.Seconds()),
		Interval:        int32(devicePollInterval.Seconds()),
	}, nil
}

// PollDeviceAuth reports whether the browser has completed OAuth for device_code; once authed it returns the
// account + its characters (the gate then runs character select). status is "pending" | "authed" | "expired".
func (s *Service) PollDeviceAuth(ctx context.Context, req *accountv1.PollDeviceAuthRequest) (*accountv1.PollDeviceAuthResponse, error) {
	if s.deviceAuth == nil {
		return nil, status.Error(codes.Unavailable, "device auth not configured")
	}
	if req.GetDeviceCode() == "" {
		return nil, status.Error(codes.InvalidArgument, "device_code required")
	}
	st, account, found, err := s.deviceAuth.Poll(ctx, req.GetDeviceCode())
	if err != nil {
		s.log.Error("PollDeviceAuth", "err", err)
		return nil, status.Error(codes.Internal, "poll device auth failed")
	}
	if !found {
		return &accountv1.PollDeviceAuthResponse{Status: "expired"}, nil
	}
	if st != DeviceAuthed {
		return &accountv1.PollDeviceAuthResponse{Status: "pending"}, nil
	}
	chars, err := s.store.AccountCharacters(ctx, account)
	if err != nil {
		s.log.Error("PollDeviceAuth: list characters", "account", account, "err", err)
		return nil, status.Error(codes.Internal, "list characters failed")
	}
	return &accountv1.PollDeviceAuthResponse{Status: "authed", AccountId: account, Characters: toProtoChars(chars)}, nil
}

// WithSigningKey wires the Ed25519 assertion-signing key (Phase 14.3). Without it, IssueSessionAssertion
// returns an empty assertion (the world then runs unverified — dev / pre-14.3).
func (s *Service) WithSigningKey(priv ed25519.PrivateKey) *Service {
	s.signKey = priv
	return s
}

// IssueSessionAssertion mints a short-lived signed assertion binding {account, character, session} (Phase
// 14.3). The gate calls it after login; the world verifies it offline on Attach. With no signing key the
// assertion is empty (auth disabled) — the response is still OK so the gate's flow is unconditional.
func (s *Service) IssueSessionAssertion(_ context.Context, req *accountv1.IssueSessionAssertionRequest) (*accountv1.IssueSessionAssertionResponse, error) {
	if req.GetAccountId() == "" || req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id and session_id required")
	}
	if s.signKey == nil {
		return &accountv1.IssueSessionAssertionResponse{}, nil // assertions disabled
	}
	tok, err := assertion.Sign(s.signKey, assertion.Claims{
		Account:   req.GetAccountId(),
		Character: req.GetCharacterId(),
		Session:   req.GetSessionId(),
		Expires:   s.now().Add(assertionTTL).Unix(),
	})
	if err != nil {
		s.log.Error("IssueSessionAssertion: sign", "account", req.GetAccountId(), "err", err)
		return nil, status.Error(codes.Internal, "sign failed")
	}
	return &accountv1.IssueSessionAssertionResponse{Assertion: tok}, nil
}

// ListCharacters returns the characters owned by an account (the select menu).
func (s *Service) ListCharacters(ctx context.Context, req *accountv1.ListCharactersRequest) (*accountv1.ListCharactersResponse, error) {
	if req.GetAccountId() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id required")
	}
	chars, err := s.store.AccountCharacters(ctx, req.GetAccountId())
	if err != nil {
		s.log.Error("ListCharacters", "account", req.GetAccountId(), "err", err)
		return nil, status.Error(codes.Internal, "list characters failed")
	}
	return &accountv1.ListCharactersResponse{Characters: toProtoChars(chars)}, nil
}

// ReserveName checks a candidate character name: the format rules + current availability. It does not write a
// row (creation reserves via the unique constraint); it is the pre-commit check the chargen UI shows.
func (s *Service) ReserveName(ctx context.Context, req *accountv1.ReserveNameRequest) (*accountv1.ReserveNameResponse, error) {
	if reason, ok := ValidateCharacterName(req.GetName()); !ok {
		return &accountv1.ReserveNameResponse{Ok: false, Reason: reason}, nil
	}
	free, err := s.store.NameAvailable(ctx, req.GetName())
	if err != nil {
		s.log.Error("ReserveName", "name", req.GetName(), "err", err)
		return nil, status.Error(codes.Internal, "name check failed")
	}
	if !free {
		return &accountv1.ReserveNameResponse{Ok: false, Reason: "taken"}, nil
	}
	return &accountv1.ReserveNameResponse{Ok: true}, nil
}

// CreateCharacter creates a character on an account. 14.1 lands the name + location write; applying the
// chosen content bundles' grants into the initial state is wired in 14.8 (the chargen front-end), so the
// bundles field is validated + recorded but the grant application is a TODO until then.
func (s *Service) CreateCharacter(ctx context.Context, req *accountv1.CreateCharacterRequest) (*accountv1.CreateCharacterResponse, error) {
	if req.GetAccountId() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id required")
	}
	if reason, ok := ValidateCharacterName(req.GetName()); !ok {
		return nil, status.Errorf(codes.InvalidArgument, "invalid name: %s", reason)
	}
	// Phase 14.8b wires the validated chargen marker (chosen bundles + bought attributes) here; until then a
	// new character starts with empty state + no pending chargen (the established backward-compat default).
	id, err := s.store.CreateAccountCharacter(ctx, req.GetAccountId(), req.GetName(), s.startZone, s.startRoom, nil, nil)
	if err != nil {
		if errors.Is(err, store.ErrNameTaken) {
			return nil, status.Error(codes.AlreadyExists, "name taken")
		}
		s.log.Error("CreateCharacter", "account", req.GetAccountId(), "name", req.GetName(), "err", err)
		return nil, status.Error(codes.Internal, "create character failed")
	}
	return &accountv1.CreateCharacterResponse{Character: &accountv1.Character{
		Id: id, Name: req.GetName(), ZoneRef: s.startZone, RoomRef: s.startRoom,
	}}, nil
}

// MintLinkCode mints a single-use link code for an authenticated account (the website's "Play" button,
// Phase 14.2). The code is consumed at the gate by RedeemLinkCode.
func (s *Service) MintLinkCode(ctx context.Context, req *accountv1.MintLinkCodeRequest) (*accountv1.MintLinkCodeResponse, error) {
	if s.codes == nil {
		return nil, status.Error(codes.Unavailable, "link codes not configured")
	}
	if req.GetAccountId() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id required")
	}
	code, err := s.codes.Mint(ctx, req.GetAccountId(), req.GetCharacterId(), linkCodeTTL)
	if err != nil {
		s.log.Error("MintLinkCode", "account", req.GetAccountId(), "err", err)
		return nil, status.Error(codes.Internal, "mint link code failed")
	}
	//nolint:gosec // linkCodeTTL is a positive constant (5 min); the ms value can't be negative
	return &accountv1.MintLinkCodeResponse{Code: code, TtlMs: uint64(linkCodeTTL.Milliseconds())}, nil
}

// RedeemLinkCode atomically consumes a link code at the gate and returns the account + its characters. The
// session assertion (signed proof the world verifies offline) is filled in by Phase 14.3; for now it is
// empty (the gate trusts the account_id directly over the in-cluster link this slice). A bad/expired/already-
// redeemed code is NotFound — the gate shows "invalid or expired code" without leaking which.
func (s *Service) RedeemLinkCode(ctx context.Context, req *accountv1.RedeemLinkCodeRequest) (*accountv1.RedeemLinkCodeResponse, error) {
	if s.codes == nil {
		return nil, status.Error(codes.Unavailable, "link codes not configured")
	}
	if req.GetCode() == "" {
		return nil, status.Error(codes.InvalidArgument, "code required")
	}
	accountID, _, found, err := s.codes.Redeem(ctx, req.GetCode())
	if err != nil {
		s.log.Error("RedeemLinkCode", "err", err)
		return nil, status.Error(codes.Internal, "redeem failed")
	}
	if !found {
		return nil, status.Error(codes.NotFound, "invalid or expired code")
	}
	chars, err := s.store.AccountCharacters(ctx, accountID)
	if err != nil {
		s.log.Error("RedeemLinkCode: list characters", "account", accountID, "err", err)
		return nil, status.Error(codes.Internal, "load characters failed")
	}
	s.log.Info("link code redeemed", "account", accountID, "conn", req.GetConnInfo(), "characters", len(chars))
	return &accountv1.RedeemLinkCodeResponse{
		AccountId:  accountID,
		Characters: toProtoChars(chars),
		// SessionAssertion left empty until Phase 14.3 (signed assertions).
	}, nil
}

// toProtoChars maps store summaries onto the proto Character list.
func toProtoChars(chars []store.CharacterSummary) []*accountv1.Character {
	out := make([]*accountv1.Character, 0, len(chars))
	for _, c := range chars {
		out = append(out, &accountv1.Character{Id: c.ID, Name: c.Name, ZoneRef: c.ZoneRef, RoomRef: c.RoomRef})
	}
	return out
}
