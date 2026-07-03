// Package account implements telos-account: the accounts/auth service (docs/ACCOUNT.md). It is the only
// service that touches OAuth providers + identities; the gate reaches it over the Account gRPC API. Auth is
// OAuth-only (Phase 15): the device-auth RPCs (StartDeviceAuth/PollDeviceAuth) drive the browser login, the
// character RPCs (list/reserve/create) and the chargen RPCs back the prompt-driven flow, and
// IssueSessionAssertion signs the gate->world session assertion.
package account

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/assertion"
	"github.com/double-nibble/telosmud/internal/content"
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
	// AccountTier (#27) returns the account's trust tier — signed into the session assertion.
	AccountTier(ctx context.Context, accountID string) (string, bool, error)
	// AccountByCharacterName (#27) resolves a character name to its owning account (the promote target).
	AccountByCharacterName(ctx context.Context, name string) (string, bool, error)
	// SetAccountTier (#27) writes the target's new tier + an audit row; returns the previous tier.
	SetAccountTier(ctx context.Context, actorAccountID, targetAccountID, newTier string) (string, error)
}

// Service implements the Account gRPC server. It is transport-thin: validation + a store call + a mapping to
// the proto types. Auth is OAuth-only (Phase 15): the browser device flow + signed session assertions.
type Service struct {
	accountv1.UnimplementedAccountServer
	store   CharStore
	signKey ed25519.PrivateKey // Phase 14.3 assertion signing key; nil => IssueSessionAssertion returns ""
	now     func() time.Time   // injectable clock (assertion expiry); defaults to time.Now
	log     *slog.Logger
	// startZone/startRoom are the default spawn location a freshly-created character lands in (the demo
	// pack's start room). Config-supplied; later chargen (14.8) may let content choose a starting zone.
	startZone string
	startRoom string
	// Chargen (Phase 14.8): the content flow + selectable bundle options the gate's prompt-driven chargen
	// renders + validates against. Empty (chargenOK=false) => GetChargenFlow reports unconfigured and the
	// gate falls back to a bare name-only create.
	chargenFlow    content.ChargenDTO
	chargenOptions []content.ChargenBundleOption
	chargenKind    map[string]string // bundle ref -> kind (the bundle_choice legality check)
	chargenOK      bool
	// Device auth (Phase 15): the device-session store + the verification base URL the gate's one-click link
	// points at. nil store => StartDeviceAuth/PollDeviceAuth are Unavailable.
	deviceAuth    DeviceAuthStore
	verifyBaseURL string
	// maxCharacters caps how many characters an account may own (Phase 15.4, configurable; default below).
	maxCharacters int
}

// defaultMaxCharacters is the per-account character cap when none is configured.
const defaultMaxCharacters = 3

// New builds the Account service over a character store.
func New(cs CharStore, log *slog.Logger, startZone, startRoom string) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		store: cs, log: log, now: time.Now,
		startZone: startZone, startRoom: startRoom,
		maxCharacters: defaultMaxCharacters,
	}
}

// WithMaxCharacters sets the per-account character cap (Phase 15.4); a value <= 0 keeps the default.
func (s *Service) WithMaxCharacters(n int) *Service {
	if n > 0 {
		s.maxCharacters = n
	}
	return s
}

// WithChargen wires the content chargen flow + selectable bundle options (Phase 14.8). The gate reads them
// (via GetChargenFlow) to drive the prompt-driven chargen. Without it, the gate offers a bare name-only create.
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

// ChargenFlow returns the content chargen flow + bundle options + whether chargen is configured (the gate
// renders its chargen prompts from these).
func (s *Service) ChargenFlow() (content.ChargenDTO, []content.ChargenBundleOption, bool) {
	return s.chargenFlow, s.chargenOptions, s.chargenOK
}

// BuildCharacter validates a chargen submission (name + the flow's picks/allocations) and creates the
// character with its first-spawn marker. It returns a non-empty user-facing reason for a validation failure
// (err nil); err is reserved for an internal failure. CreateChargenCharacter calls it in-process on behalf
// of the gate's telnet chargen.
func (s *Service) BuildCharacter(ctx context.Context, accountID, name string, picks map[string]string, allocs map[string]map[string]int) (id string, reason string, err error) {
	if accountID == "" {
		return "", "", status.Error(codes.InvalidArgument, "account_id required")
	}
	if r, ok := ValidateCharacterName(name); !ok {
		return "", r, nil
	}
	// Enforce the per-account character cap (Phase 15.4).
	if existing, err := s.store.AccountCharacters(ctx, accountID); err == nil && len(existing) >= s.maxCharacters {
		return "", capMessage(s.maxCharacters), nil
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

// capMessage is the user-facing "you're at the character limit" message.
func capMessage(n int) string {
	return fmt.Sprintf("You've reached the limit of %d characters.", n)
}

// atCapacity reports whether the account already holds the maximum number of characters.
func (s *Service) atCapacity(ctx context.Context, accountID string) bool {
	existing, err := s.store.AccountCharacters(ctx, accountID)
	return err == nil && len(existing) >= s.maxCharacters
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

// GetChargenFlow returns the content chargen flow + bundle options the gate renders as prompts (Phase 15.4).
func (s *Service) GetChargenFlow(_ context.Context, _ *accountv1.GetChargenFlowRequest) (*accountv1.GetChargenFlowResponse, error) {
	if !s.chargenOK {
		return &accountv1.GetChargenFlowResponse{Configured: false}, nil
	}
	steps := make([]*accountv1.ChargenStep, 0, len(s.chargenFlow.Steps))
	for _, st := range s.chargenFlow.Steps {
		steps = append(steps, &accountv1.ChargenStep{
			Kind: st.Kind, Id: st.ID, Prompt: st.Prompt, BundleKind: st.BundleKind,
			Attributes: st.Attributes,
			//nolint:gosec // chargen point-buy bounds are small content-authored ints; no overflow.
			Points: int32(st.Points), Base: int32(st.Base), Min: int32(st.Min), Max: int32(st.Max),
		})
	}
	opts := make([]*accountv1.ChargenBundleOption, 0, len(s.chargenOptions))
	for _, o := range s.chargenOptions {
		opts = append(opts, &accountv1.ChargenBundleOption{Ref: o.Ref, Kind: o.Kind, Label: o.Label})
	}
	return &accountv1.GetChargenFlowResponse{
		Configured: true, Steps: steps, Options: opts,
		MaxCharacters: int32(s.maxCharacters), //nolint:gosec // a small content-bounded cap.
	}, nil
}

// CreateChargenCharacter validates a prompt-driven chargen submission + creates the character (Phase 15.4).
func (s *Service) CreateChargenCharacter(ctx context.Context, req *accountv1.CreateChargenCharacterRequest) (*accountv1.CreateChargenCharacterResponse, error) {
	// At-capacity is a distinct signal so the gate returns to character SELECT rather than re-running chargen.
	if s.atCapacity(ctx, req.GetAccountId()) {
		return &accountv1.CreateChargenCharacterResponse{Reason: capMessage(s.maxCharacters), AtCapacity: true}, nil
	}
	allocs := make(map[string]map[string]int, len(req.GetAllocs()))
	for stepID, a := range req.GetAllocs() {
		m := make(map[string]int, len(a.GetValues()))
		for attr, v := range a.GetValues() {
			m[attr] = int(v)
		}
		allocs[stepID] = m
	}
	id, reason, err := s.BuildCharacter(ctx, req.GetAccountId(), req.GetName(), req.GetPicks(), allocs)
	if err != nil {
		return nil, err
	}
	return &accountv1.CreateChargenCharacterResponse{CharacterId: id, Reason: reason}, nil
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
func (s *Service) IssueSessionAssertion(ctx context.Context, req *accountv1.IssueSessionAssertionRequest) (*accountv1.IssueSessionAssertionResponse, error) {
	if req.GetAccountId() == "" || req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id and session_id required")
	}
	if s.signKey == nil {
		return &accountv1.IssueSessionAssertionResponse{}, nil // assertions disabled
	}
	// The trust tier (#27) is signed INTO the assertion so the world trusts it offline (no per-connect RPC).
	// FAIL SAFE: a read error / unknown account degrades to player — an error must never elevate a tier.
	tier := store.TierPlayer
	if t, found, err := s.store.AccountTier(ctx, req.GetAccountId()); err != nil {
		s.log.Error("IssueSessionAssertion: tier read (defaulting to player)", "account", req.GetAccountId(), "err", err)
	} else if found {
		tier = t
	}
	tok, err := assertion.Sign(s.signKey, assertion.Claims{
		Account:   req.GetAccountId(),
		Character: req.GetCharacterId(),
		Session:   req.GetSessionId(),
		Expires:   s.now().Add(assertionTTL).Unix(),
		Tier:      tier,
	})
	if err != nil {
		s.log.Error("IssueSessionAssertion: sign", "account", req.GetAccountId(), "err", err)
		return nil, status.Error(codes.Internal, "sign failed")
	}
	return &accountv1.IssueSessionAssertionResponse{Assertion: tok}, nil
}

// validTiers is the set of assignable trust tiers (#27) — mirrors the accounts.tier CHECK (migration 00019).
var validTiers = map[string]bool{store.TierPlayer: true, store.TierBuilder: true, store.TierAdmin: true}

// SetAccountTier is the promote/demote authority (#27). AUTHZ IS HERE, not the edge: it reads the ACTOR's
// tier from the store and refuses unless the actor is an admin — so a compromised/forged edge request from a
// non-admin account cannot elevate anyone. It resolves the target character to its account, writes the new
// tier + an audit row (actor recorded), and returns the previous tier. The change takes effect on the
// target's NEXT login (the assertion re-reads the tier). A user-facing refusal rides ok=false + reason (the
// gate prints it); only unexpected I/O is a gRPC error.
func (s *Service) SetAccountTier(ctx context.Context, req *accountv1.SetAccountTierRequest) (*accountv1.SetAccountTierResponse, error) {
	if req.GetActorAccountId() == "" || req.GetTargetCharacter() == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_account_id and target_character required")
	}
	// AUTHZ FIRST (before any other validation): the actor must be an admin, per the authoritative store
	// (never the edge's word). Checking this before tier/target validation avoids disclosing the tier
	// vocabulary or probing character existence to a non-admin.
	actorTier, found, err := s.store.AccountTier(ctx, req.GetActorAccountId())
	if err != nil {
		s.log.Error("SetAccountTier: actor tier", "actor", req.GetActorAccountId(), "err", err)
		return nil, status.Error(codes.Internal, "tier lookup failed")
	}
	if !found || actorTier != store.TierAdmin {
		s.log.Warn("SetAccountTier refused: actor not admin", "actor", req.GetActorAccountId(), "actor_tier", actorTier)
		return &accountv1.SetAccountTierResponse{Reason: "You are not authorized to change trust tiers."}, nil
	}
	if !validTiers[req.GetNewTier()] {
		return &accountv1.SetAccountTierResponse{Reason: "Unknown tier (use player, builder, or admin)."}, nil
	}
	// Resolve the target character to its owning account.
	target, found, err := s.store.AccountByCharacterName(ctx, req.GetTargetCharacter())
	if err != nil {
		s.log.Error("SetAccountTier: resolve target", "target", req.GetTargetCharacter(), "err", err)
		return nil, status.Error(codes.Internal, "target lookup failed")
	}
	if !found {
		return &accountv1.SetAccountTierResponse{Reason: "No such character."}, nil
	}
	old, err := s.store.SetAccountTier(ctx, req.GetActorAccountId(), target, req.GetNewTier())
	if err != nil {
		s.log.Error("SetAccountTier: write", "target", target, "err", err)
		return nil, status.Error(codes.Internal, "set tier failed")
	}
	s.log.Info("account tier changed", "actor", req.GetActorAccountId(), "target_character", req.GetTargetCharacter(),
		"target_account", target, "old_tier", old, "new_tier", req.GetNewTier())
	return &accountv1.SetAccountTierResponse{Ok: true, OldTier: old}, nil
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

// CreateCharacter creates a character on an account (name + location write + records the chosen chargen
// bundles). By design (Model A, Phase 14.8) the account service only VALIDATES + RECORDS the bundles; the
// WORLD applies their grants into the entity on FIRST SPAWN (via the characters.chargen marker), so there is
// deliberately no grant application here — the account tier stays content-reading + validating, never a
// world mutator.
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

// toProtoChars maps store summaries onto the proto Character list.
func toProtoChars(chars []store.CharacterSummary) []*accountv1.Character {
	out := make([]*accountv1.Character, 0, len(chars))
	for _, c := range chars {
		out = append(out, &accountv1.Character{Id: c.ID, Name: c.Name, ZoneRef: c.ZoneRef, RoomRef: c.RoomRef})
	}
	return out
}
