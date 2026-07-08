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
	"sort"
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
	// SetAccountTier (#27) writes the target's new tier + an audit row; returns the previous tier. The write
	// is a COMPARE-AND-SET against expectedOldTier (the tier this service's ceilings were evaluated against),
	// returning store.ErrTierConflict when a concurrent change moved the base (#165).
	SetAccountTier(ctx context.Context, actorAccountID, targetAccountID, newTier, expectedOldTier string) (string, error)
	// AccountColorPref (#23) returns the persisted terminal color preference; set=false => never chosen.
	AccountColorPref(ctx context.Context, accountID string) (enabled bool, set bool, err error)
	// SetAccountColorPref (#23) persists the terminal color preference (true/false).
	SetAccountColorPref(ctx context.Context, accountID string, enabled bool) error
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
	// trustTiers is the resolved content trust ladder (#27/#29 Slice 0b): SetAccountTier validates the
	// requested tier and authorizes the actor against it (rank + the manage-tiers capability). nil until
	// WithTrustLadder; trustLadder() substitutes the default ladder (player/builder/admin) so a service
	// built without content authorizes promotes exactly as round-8 did.
	trustTiers *content.TrustLadder
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

// WithTrustLadder wires the content-defined trust ladder (#27/#29 Slice 0b): SetAccountTier validates the
// requested tier + authorizes the actor against it. An empty/absent ladder falls back to the built-in
// player/builder/admin ladder, preserving round-8 promote authz.
func (s *Service) WithTrustLadder(tiers []content.TrustTierDTO) *Service {
	s.trustTiers = content.NewTrustLadder(tiers)
	return s
}

// trustLadder returns the resolved ladder, defaulting to the built-in ladder when none was wired (so a
// service constructed without content still authorizes promotes as round-8 did).
func (s *Service) trustLadder() *content.TrustLadder {
	if s.trustTiers != nil {
		return s.trustTiers
	}
	return content.NewTrustLadder(nil)
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

// SetAccountTier is the promote/demote authority (#27; content-tier-aware since #29 Slice 0b). AUTHZ IS
// HERE, not the edge: it reads the ACTOR's tier from the store and resolves it against the content trust
// ladder, so a forged edge request from an under-privileged account cannot elevate anyone. (CAVEAT: the
// acting principal is taken from req.ActorAccountId, and the gRPC listener has no transport authentication —
// anyone who can dial it may assert any account id. Every ceiling below therefore assumes a trusted network
// between gate and account service. Tracked in #247.) The model, replacing the round-8 hardcoded
// actor==admin gate:
//
//   - the actor's current tier must grant the manage-tiers capability (content.FlagAdmin) — content decides
//     which tiers are admin-capable (the default ladder: only "admin");
//   - the requested tier must be a DEFINED tier in the ladder;
//   - CEILING (rank): the actor may not grant a tier ranked ABOVE its own standing, nor change the tier of
//     an account that OUTRANKS it — so no one can mint a peer/superior or demote someone above them;
//   - CEILING (capability, #165): the actor may not GRANT a tier holding a capability flag
//     (holylight/builder/admin) its own tier lacks, nor change the tier of a SAME-RANK account holding one.
//     Rank and capability are independent axes (see content.TierDominates), so the rank ceiling alone lets
//     both a same-rank and a lower-rank richer-flagged tier through.
//
// Why the capability ceiling is asymmetric — grant-side unconditional, target-side only at EQUAL rank: the
// grant side is the escalation path (writing a tier mints its capabilities) and must hold universally. The
// target side only ever STRIPS capability, so it is policy, not security; applying it across ranks would make
// accounts permanently unmanageable under a ladder whose top tier is not a capability superset (an
// archon(50,{admin}) could never demote a builder(20,{holylight,builder}), and the gate offers no other
// revocation verb). Scoping it to equal rank keeps the classic "a strictly higher rank may always manage a
// strictly lower one" rule — so revocation is never lost — while still stopping a gm(30,{admin}) from
// stripping a peer warden(30,{admin,holylight})'s see-all, which is where rank gives no separation at all.
// A non-nested ladder still cannot CREATE the richer tier (the grant ceiling is unconditional); that is an
// authoring smell LintTrustLadder warns on, not a lockout.
//
// For the default ladder this is byte-for-byte round-8: admin is the top rank AND holds every capability, so
// both ceilings are vacuous for it and it may set any of the three tiers on anyone.
//
// It resolves the target character to its account, writes the new tier + an audit row, and returns the
// previous tier. The write is a COMPARE-AND-SET against the tier the ceilings were evaluated against, so a
// concurrent promote cannot land between the check and the write (the ceilings read outside the row lock).
// Effect on the target's NEXT login. A user-facing refusal rides ok=false + reason; only unexpected I/O is a
// gRPC error. Authz runs BEFORE tier/target validation so the tier vocabulary and character existence are
// never disclosed to an unauthorized actor.
//
// Refusals are logged at Warn with the same field set as the success line — an attempted escalation by an
// already-privileged actor is the signal a role-audit trail exists for. They are deliberately NOT persisted:
// the first refusal branch is reachable by any logged-in mortal, so a refusal table would be a
// player-controlled unbounded INSERT, and account_role_audit's schema (NOT NULL FK on target_account) cannot
// represent a refusal that fires before target resolution. See #108 for the persisted-trail discussion.
func (s *Service) SetAccountTier(ctx context.Context, req *accountv1.SetAccountTierRequest) (*accountv1.SetAccountTierResponse, error) {
	if req.GetActorAccountId() == "" || req.GetTargetCharacter() == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_account_id and target_character required")
	}
	ladder := s.trustLadder()
	// AUTHZ FIRST: the actor's current tier must grant the manage-tiers capability, per the authoritative
	// store (never the edge's word).
	actorTier, found, err := s.store.AccountTier(ctx, req.GetActorAccountId())
	if err != nil {
		s.log.Error("SetAccountTier: actor tier", "actor", req.GetActorAccountId(), "err", err)
		return nil, status.Error(codes.Internal, "tier lookup failed")
	}
	if !found || !ladder.GrantsFlag(actorTier, content.FlagAdmin) {
		s.log.Warn("SetAccountTier refused: actor lacks manage-tiers capability",
			"actor", req.GetActorAccountId(), "actor_tier", actorTier)
		return &accountv1.SetAccountTierResponse{Reason: "You are not authorized to change trust tiers."}, nil
	}
	// Resolve the requested tier. An EMPTY new_tier is the demote-to-BASELINE sentinel (#112): the edge's
	// `demote <char>` sends "" rather than hardcoding "player", which a content pack may rename or omit (making
	// every demote fail closed with "Unknown tier"). The ladder — which the account service owns — resolves the
	// baseline (lowest-rank tier). A non-empty tier passes through unchanged. All checks below use this local.
	newTier := req.GetNewTier()
	if newTier == "" {
		newTier = ladder.Baseline()
	}
	// refuse logs a refused tier change at Warn with the success line's field set, then renders the reason.
	// Every ceiling goes through it, so a probe of ANY branch leaves the same shape of trail (M-1/M-2).
	refuse := func(why, reason string, kv ...any) (*accountv1.SetAccountTierResponse, error) {
		s.log.Warn("SetAccountTier refused: "+why, append([]any{
			"actor", req.GetActorAccountId(), "actor_tier", actorTier,
			"target_character", req.GetTargetCharacter(), "new_tier", newTier,
		}, kv...)...)
		return &accountv1.SetAccountTierResponse{Reason: reason}, nil
	}
	// The requested tier must be a defined tier in the content ladder. (Baseline() always is, so the demote
	// sentinel never trips this — unless the ladder is somehow empty, which NewTrustLadder prevents.)
	if !ladder.Has(newTier) {
		names := ladder.Names()
		sort.Strings(names)
		return &accountv1.SetAccountTierResponse{Reason: "Unknown tier (known: " + strings.Join(names, ", ") + ")."}, nil
	}
	// Ceiling (rank): the actor may not grant a tier ranked above its own standing.
	if ladder.Rank(newTier) > ladder.Rank(actorTier) {
		return refuse("requested tier outranks the actor", "You cannot grant a tier above your own standing.")
	}
	// Ceiling (capability, #165): the actor may not grant a tier holding a capability its OWN tier lacks.
	// Unconditional — this is the escalation path (writing a tier mints its capabilities on the next login).
	// Naming the missing capability is not a disclosure: the actor already passed the FlagAdmin gate above,
	// and the Unknown-tier branch already hands an authorized actor the whole tier vocabulary.
	if !ladder.TierDominates(actorTier, newTier) {
		return refuse("requested tier grants a capability the actor lacks",
			fmt.Sprintf("%s grants %s, which your tier (%s) does not hold.",
				newTier, joinCaps(ladder.MissingCapabilities(actorTier, newTier)), actorTier))
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
	// The target's CURRENT tier: both the remaining ceilings' input and the compare-and-set base for the write.
	// accounts.tier is NOT NULL DEFAULT 'player' (migration 00019) and `target` came from a resolved character
	// row, so found is always true here; the flag is ignored deliberately, and Rank("")/TierDominates degrade
	// to the rank-0 baseline rather than skipping a check.
	targetTier, _, err := s.store.AccountTier(ctx, target)
	if err != nil {
		s.log.Error("SetAccountTier: target tier", "target", target, "err", err)
		return nil, status.Error(codes.Internal, "tier lookup failed")
	}
	// FAIL CLOSED on a stored tier the loaded ladder does not define (#165 F6). accounts.tier is free-form TEXT
	// since migration 00021 dropped the CHECK, so a renamed/removed rung — or telos-account loading a different
	// pack set than the world (#246) — leaves rows carrying a tier this service cannot reason about. Rank() and
	// TierDominates() would both read it as the baseline, silently voiding BOTH ceilings for that account.
	// An unrecognized stored tier means the operator's ladder is wrong; refuse rather than guess.
	if targetTier != "" && !ladder.Has(targetTier) {
		s.log.Error("SetAccountTier refused: target's stored tier is not in the loaded ladder",
			"actor", req.GetActorAccountId(), "target_account", target, "target_tier", targetTier, "known", ladder.Names())
		return &accountv1.SetAccountTierResponse{Reason: "That account's current tier is not defined in the loaded content ladder; the server's ladder needs attention."}, nil
	}
	// Ceiling (rank): the actor may not change the tier of an account that outranks it.
	if ladder.Rank(targetTier) > ladder.Rank(actorTier) {
		return refuse("target outranks the actor", "You cannot change the tier of someone above your own standing.",
			"target_account", target, "target_tier", targetTier)
	}
	// Ceiling (capability, #165) — EQUAL RANK ONLY. At equal rank the rank ceiling gives no separation, so
	// without this a gm(30,{admin}) could strip a peer warden(30,{admin,holylight})'s see-all. Above equal rank
	// the actor already dominates by rank and stripping capability is revocation, not escalation; refusing there
	// would strand accounts no principal can demote (see the asymmetry note in the doc comment).
	if ladder.Rank(targetTier) == ladder.Rank(actorTier) && !ladder.TierDominates(actorTier, targetTier) {
		return refuse("same-rank target holds a capability the actor lacks",
			fmt.Sprintf("%s holds %s, which your tier (%s) does not; you cannot change their tier.",
				req.GetTargetCharacter(), joinCaps(ladder.MissingCapabilities(actorTier, targetTier)), actorTier),
			"target_account", target, "target_tier", targetTier)
	}
	// COMPARE-AND-SET on targetTier: the two target-side ceilings above read the tier OUTSIDE the row lock, so
	// a concurrent promote could otherwise land between the check and the write — e.g. a gm passes the ceiling
	// against Bob=player, an admin promotes Bob to warden, and the gm's write then strips a warden's holylight.
	// The store re-reads under FOR UPDATE and refuses when the base moved (#165 F3).
	old, err := s.store.SetAccountTier(ctx, req.GetActorAccountId(), target, newTier, targetTier)
	if errors.Is(err, store.ErrTierConflict) {
		// old carries the tier the store OBSERVED under the row lock (what a concurrent writer set) — the more
		// useful forensic value than the stale base we expected, so log both.
		return refuse("target tier changed under the check (CAS conflict)",
			"That account's tier changed while you were acting on it; try again.",
			"target_account", target, "expected_tier", targetTier, "observed_tier", old)
	}
	if err != nil {
		s.log.Error("SetAccountTier: write", "target", target, "err", err)
		return nil, status.Error(codes.Internal, "set tier failed")
	}
	s.log.Info("account tier changed", "actor", req.GetActorAccountId(), "target_character", req.GetTargetCharacter(),
		"target_account", target, "old_tier", old, "new_tier", newTier)
	return &accountv1.SetAccountTierResponse{Ok: true, OldTier: old, NewTier: newTier}, nil
}

// joinCaps renders a capability-name list for a refusal message ("holylight" / "holylight and builder" /
// "holylight, builder and admin"). Never empty at the call sites: both are reached only when TierDominates
// returned false, which means at least one capability is missing.
func joinCaps(caps []string) string {
	switch len(caps) {
	case 0:
		return "capabilities you do not hold" // unreachable; keeps the sentence grammatical
	case 1:
		return caps[0]
	default:
		return strings.Join(caps[:len(caps)-1], ", ") + " and " + caps[len(caps)-1]
	}
}

// GetAccountPrefs returns an account's persisted EDGE preferences (#23). Each pref is an optional proto field
// so "never set" (absent) is distinguishable from an explicit false — the gate keeps its default when absent.
// Color is a purely edge concern, so only the gate reads this; the world never sees it.
func (s *Service) GetAccountPrefs(ctx context.Context, req *accountv1.GetAccountPrefsRequest) (*accountv1.GetAccountPrefsResponse, error) {
	if req.GetAccountId() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id required")
	}
	resp := &accountv1.GetAccountPrefsResponse{}
	enabled, set, err := s.store.AccountColorPref(ctx, req.GetAccountId())
	if err != nil {
		s.log.Error("GetAccountPrefs: color", "account", req.GetAccountId(), "err", err)
		return nil, status.Error(codes.Internal, "read prefs failed")
	}
	if set {
		resp.ColorEnabled = &enabled // absent stays absent => the gate keeps its default
	}
	return resp, nil
}

// SetAccountPrefs persists the edge preferences PRESENT in the request (#23). An absent field is a NO-OP (it
// does not clear the stored value), so a caller updating one pref never disturbs another.
func (s *Service) SetAccountPrefs(ctx context.Context, req *accountv1.SetAccountPrefsRequest) (*accountv1.SetAccountPrefsResponse, error) {
	if req.GetAccountId() == "" {
		return nil, status.Error(codes.InvalidArgument, "account_id required")
	}
	if req.ColorEnabled != nil {
		if err := s.store.SetAccountColorPref(ctx, req.GetAccountId(), req.GetColorEnabled()); err != nil {
			s.log.Error("SetAccountPrefs: color", "account", req.GetAccountId(), "err", err)
			return nil, status.Error(codes.Internal, "write prefs failed")
		}
	}
	return &accountv1.SetAccountPrefsResponse{}, nil
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
