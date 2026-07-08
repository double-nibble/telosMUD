package account

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/store"
)

// service_test.go — Phase 14.1: the Account service RPC shapes over an in-memory fake store (no Postgres).

// fakeStore is an in-memory CharStore for the service tests.
type fakeStore struct {
	chars       map[string][]store.CharacterSummary
	taken       map[string]bool
	tiers       map[string]string // accountID -> tier (#27); absent => not found
	charAccount map[string]string // character name -> owning accountID (#27)
	tierErr     error             // when set, AccountTier returns this error (fail-safe tests, #27)
	colorPref   map[string]bool   // accountID -> color preference (#23); absent => never set
	colorGetErr error             // when set, AccountColorPref returns this error (#23)
	colorSetErr error             // when set, SetAccountColorPref returns this error (#23)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		chars:       map[string][]store.CharacterSummary{},
		taken:       map[string]bool{},
		tiers:       map[string]string{},
		charAccount: map[string]string{},
		colorPref:   map[string]bool{},
	}
}

func (f *fakeStore) AccountColorPref(_ context.Context, accountID string) (bool, bool, error) {
	if f.colorGetErr != nil {
		return false, false, f.colorGetErr
	}
	v, ok := f.colorPref[accountID]
	return v, ok, nil
}

func (f *fakeStore) SetAccountColorPref(_ context.Context, accountID string, enabled bool) error {
	if f.colorSetErr != nil {
		return f.colorSetErr
	}
	f.colorPref[accountID] = enabled
	return nil
}

func (f *fakeStore) AccountTier(_ context.Context, accountID string) (string, bool, error) {
	if f.tierErr != nil {
		return "", false, f.tierErr
	}
	t, ok := f.tiers[accountID]
	return t, ok, nil
}

func (f *fakeStore) AccountByCharacterName(_ context.Context, name string) (string, bool, error) {
	a, ok := f.charAccount[name]
	return a, ok, nil
}

func (f *fakeStore) SetAccountTier(_ context.Context, _, targetAccountID, newTier string) (string, error) {
	old := f.tiers[targetAccountID]
	f.tiers[targetAccountID] = newTier
	return old, nil
}

func (f *fakeStore) AccountCharacters(_ context.Context, accountID string) ([]store.CharacterSummary, error) {
	return f.chars[accountID], nil
}

func (f *fakeStore) NameAvailable(_ context.Context, name string) (bool, error) {
	return !f.taken[name], nil
}

func (f *fakeStore) CreateAccountCharacter(_ context.Context, accountID, name, _, _ string, _, _ []byte) (string, error) {
	if f.taken[name] {
		return "", store.ErrNameTaken
	}
	f.taken[name] = true
	f.chars[accountID] = append(f.chars[accountID], store.CharacterSummary{ID: "id-" + name, Name: name})
	return "id-" + name, nil
}

func (f *fakeStore) CreateCharacterWithChargen(ctx context.Context, accountID, name, zoneRef, roomRef string, _ []string, _ map[string]float64) (string, error) {
	return f.CreateAccountCharacter(ctx, accountID, name, zoneRef, roomRef, nil, nil)
}

func newTestService(fs *fakeStore) *Service {
	return New(fs, nil, "midgaard", "midgaard:room:temple")
}

func TestListCharacters(t *testing.T) {
	fs := newFakeStore()
	fs.chars["acct-1"] = []store.CharacterSummary{{ID: "c1", Name: "Aragorn"}, {ID: "c2", Name: "Gimli"}}
	svc := newTestService(fs)

	resp, err := svc.ListCharacters(context.Background(), &accountv1.ListCharactersRequest{AccountId: "acct-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetCharacters()) != 2 || resp.GetCharacters()[0].GetName() != "Aragorn" {
		t.Fatalf("characters = %+v, want Aragorn + Gimli", resp.GetCharacters())
	}

	// A missing account_id is an InvalidArgument.
	if _, err := svc.ListCharacters(context.Background(), &accountv1.ListCharactersRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty account_id should be InvalidArgument, got %v", err)
	}
}

func TestReserveName(t *testing.T) {
	fs := newFakeStore()
	fs.taken["Aragorn"] = true
	svc := newTestService(fs)

	// Free + valid -> ok.
	if r, _ := svc.ReserveName(context.Background(), &accountv1.ReserveNameRequest{Name: "Boromir"}); !r.GetOk() {
		t.Fatal("a free, valid name should reserve ok")
	}
	// Taken -> not ok, reason "taken".
	if r, _ := svc.ReserveName(context.Background(), &accountv1.ReserveNameRequest{Name: "Aragorn"}); r.GetOk() || r.GetReason() != "taken" {
		t.Fatalf("a taken name should be refused with reason=taken, got %+v", r)
	}
	// Invalid -> not ok, a format reason.
	if r, _ := svc.ReserveName(context.Background(), &accountv1.ReserveNameRequest{Name: "9bad"}); r.GetOk() || r.GetReason() != "leading_digit" {
		t.Fatalf("a digit-leading name should be refused with reason=leading_digit, got %+v", r)
	}
}

func TestCreateCharacter(t *testing.T) {
	fs := newFakeStore()
	svc := newTestService(fs)

	resp, err := svc.CreateCharacter(context.Background(), &accountv1.CreateCharacterRequest{AccountId: "acct-1", Name: "Frodo"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCharacter().GetName() != "Frodo" || resp.GetCharacter().GetZoneRef() != "midgaard" {
		t.Fatalf("created character = %+v, want Frodo in midgaard", resp.GetCharacter())
	}
	// A second create with the same name is AlreadyExists.
	if _, err := svc.CreateCharacter(context.Background(), &accountv1.CreateCharacterRequest{AccountId: "acct-1", Name: "Frodo"}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate name should be AlreadyExists, got %v", err)
	}
	// An invalid name is InvalidArgument.
	if _, err := svc.CreateCharacter(context.Background(), &accountv1.CreateCharacterRequest{AccountId: "acct-1", Name: ".bad"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid name should be InvalidArgument, got %v", err)
	}
}

func TestValidateCharacterName(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		ok     bool
	}{
		{"Aragorn", "", true},
		{"", "required", false},
		{"123456789012345678901", "too_long", false},
		{".dot", "leading_dot", false},
		{"9nine", "leading_digit", false},
		{"a.b", "contains_dot", false},
		{"{{FG_RED}}Bob", "contains_brace", false},
		{"Bo}b", "contains_brace", false},
		{"bad\tname", "invalid_char", false},
	}
	for _, tc := range cases {
		reason, ok := ValidateCharacterName(tc.name)
		if ok != tc.ok || reason != tc.reason {
			t.Errorf("ValidateCharacterName(%q) = (%q,%v), want (%q,%v)", tc.name, reason, ok, tc.reason, tc.ok)
		}
	}
}

// TestBuildCharacter (Phase 14.8b) covers the chargen create path: a valid submission creates the character,
// and validation failures (wrong-kind pick, bad name) return a user-facing reason without an error.
func TestBuildCharacter(t *testing.T) {
	svc := New(newFakeStore(), nil, "midgaard", "midgaard:room:temple")
	pb := content.ChargenStepDTO{Kind: "point_buy", ID: "attrs", Attributes: []string{"strength"}, Points: 9, Base: 8, Min: 8, Max: 15, Cost: map[string]int{"8": 0, "15": 9}}
	svc.WithChargen(content.ChargenDTO{Steps: []content.ChargenStepDTO{
		{Kind: "bundle_choice", ID: "race", BundleKind: "race", Pick: 1},
		pb,
	}}, []content.ChargenBundleOption{{Ref: "elf", Kind: "race", Label: "Elf"}})
	ctx := context.Background()

	// Valid: creates the character.
	id, reason, err := svc.BuildCharacter(ctx, "acct1", "Legolas",
		map[string]string{"race": "elf"}, map[string]map[string]int{"attrs": {"strength": 15}})
	if err != nil || reason != "" || id == "" {
		t.Fatalf("valid build: id=%q reason=%q err=%v", id, reason, err)
	}

	// Wrong-kind pick: a user-facing reason, no error, no create.
	if _, reason, err := svc.BuildCharacter(ctx, "acct1", "Bad",
		map[string]string{"race": "nonexistent"}, map[string]map[string]int{"attrs": {"strength": 8}}); err != nil || reason == "" {
		t.Fatalf("wrong-kind pick: want a reason, got reason=%q err=%v", reason, err)
	}

	// Empty name: rejected with a reason.
	if _, reason, err := svc.BuildCharacter(ctx, "acct1", "",
		map[string]string{"race": "elf"}, map[string]map[string]int{"attrs": {"strength": 15}}); err != nil || reason == "" {
		t.Fatalf("empty name: want a reason, got reason=%q err=%v", reason, err)
	}

	// Duplicate name: ErrNameTaken mapped to a friendly reason.
	if _, reason, _ := svc.BuildCharacter(ctx, "acct1", "Legolas",
		map[string]string{"race": "elf"}, map[string]map[string]int{"attrs": {"strength": 15}}); reason == "" {
		t.Fatal("duplicate name should return a 'name taken' reason")
	}
}

// TestDeviceAuthService (Phase 15) covers the gate-facing StartDeviceAuth/PollDeviceAuth + the broker-facing
// AuthorizeDevice over the in-memory device store.
func TestDeviceAuthService(t *testing.T) {
	fs := newFakeStore()
	fs.chars["acct-1"] = []store.CharacterSummary{{ID: "c1", Name: "Aragorn"}}
	svc := New(fs, nil, "midgaard", "midgaard:room:temple").WithDeviceAuth(NewMemDeviceAuth(), "http://localhost:8080/")
	ctx := context.Background()

	start, err := svc.StartDeviceAuth(ctx, &accountv1.StartDeviceAuthRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if start.GetDeviceCode() == "" || start.GetExpiresIn() <= 0 || start.GetInterval() <= 0 {
		t.Fatalf("StartDeviceAuth response incomplete: %+v", start)
	}
	if want := "http://localhost:8080/login/" + start.GetDeviceCode(); start.GetVerificationUri() != want {
		t.Fatalf("verification_uri = %q, want %q", start.GetVerificationUri(), want)
	}

	// Pending until the broker authorizes.
	if p, _ := svc.PollDeviceAuth(ctx, &accountv1.PollDeviceAuthRequest{DeviceCode: start.GetDeviceCode()}); p.GetStatus() != "pending" {
		t.Fatalf("poll before auth status = %q, want pending", p.GetStatus())
	}

	// Broker callback authorizes; the next poll returns authed + the account's characters.
	if ok, err := svc.AuthorizeDevice(ctx, start.GetDeviceCode(), "acct-1"); err != nil || !ok {
		t.Fatalf("AuthorizeDevice: ok=%v err=%v", ok, err)
	}
	p, err := svc.PollDeviceAuth(ctx, &accountv1.PollDeviceAuthRequest{DeviceCode: start.GetDeviceCode()})
	if err != nil {
		t.Fatal(err)
	}
	if p.GetStatus() != "authed" || p.GetAccountId() != "acct-1" || len(p.GetCharacters()) != 1 || p.GetCharacters()[0].GetName() != "Aragorn" {
		t.Fatalf("authed poll = %+v, want authed/acct-1/[Aragorn]", p)
	}

	// An unknown device code polls as expired.
	if p, _ := svc.PollDeviceAuth(ctx, &accountv1.PollDeviceAuthRequest{DeviceCode: "nope"}); p.GetStatus() != "expired" {
		t.Fatalf("unknown device poll status = %q, want expired", p.GetStatus())
	}

	// With no device store wired, the RPCs are Unavailable.
	bare := New(newFakeStore(), nil, "midgaard", "midgaard:room:temple")
	if _, err := bare.StartDeviceAuth(ctx, &accountv1.StartDeviceAuthRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("StartDeviceAuth without a store should be Unavailable, got %v", err)
	}
}

// setTierReq is a small helper for the SetAccountTier authz tests.
func setTierReq(actor, target, tier string) *accountv1.SetAccountTierRequest {
	return &accountv1.SetAccountTierRequest{ActorAccountId: actor, TargetCharacter: target, NewTier: tier}
}

// TestSetAccountTierDefaultLadder pins that with no content ladder wired, promote authz is byte-for-byte
// round-8: an admin actor may set any of player/builder/admin on anyone; a non-admin (builder/player) is
// refused (its tier does not grant the manage-tiers capability).
func TestSetAccountTierDefaultLadder(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	fs.tiers["a-admin"] = "admin"
	fs.tiers["a-bob"] = "player"
	fs.charAccount["Bob"] = "a-bob"
	svc := newTestService(fs) // no WithTrustLadder => default ladder

	// admin promotes Bob player -> builder.
	resp, err := svc.SetAccountTier(ctx, setTierReq("a-admin", "Bob", "builder"))
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetOk() || resp.GetOldTier() != "player" || fs.tiers["a-bob"] != "builder" {
		t.Fatalf("admin promote should succeed player->builder, got ok=%v old=%q now=%q", resp.GetOk(), resp.GetOldTier(), fs.tiers["a-bob"])
	}

	// A builder actor (no manage-tiers capability in the default ladder) is refused.
	fs.tiers["a-builder"] = "builder"
	resp, err = svc.SetAccountTier(ctx, setTierReq("a-builder", "Bob", "player"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOk() || resp.GetReason() == "" {
		t.Fatalf("a builder must not be authorized to change tiers, got %+v", resp)
	}
}

// TestSetAccountTierUnknownTier: an authorized actor requesting a tier not in the ladder is refused with the
// known-tier list (authz runs first, so only an authorized actor ever sees the vocabulary).
func TestSetAccountTierUnknownTier(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	fs.tiers["a-admin"] = "admin"
	fs.tiers["a-bob"] = "player"
	fs.charAccount["Bob"] = "a-bob"
	svc := newTestService(fs)

	resp, err := svc.SetAccountTier(ctx, setTierReq("a-admin", "Bob", "wizard"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOk() || !strings.Contains(resp.GetReason(), "known:") {
		t.Fatalf("unknown tier should be refused with the known-tier list, got %+v", resp)
	}
	if fs.tiers["a-bob"] != "player" {
		t.Error("a refused promote must not write the tier")
	}
}

// TestSetAccountTierRankCeiling: with a content ladder that has an admin-capable mid tier (architect grants
// the manage-tiers flag at rank 30, below admin=40), an architect may promote up to architect but NOT to
// admin (a tier above its own standing), and may not change an account that outranks it.
func TestSetAccountTierRankCeiling(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	fs.tiers["a-arch"] = "architect"
	fs.tiers["a-bob"] = "player"
	fs.tiers["a-boss"] = "admin"
	fs.charAccount["Bob"] = "a-bob"
	fs.charAccount["Boss"] = "a-boss"
	svc := newTestService(fs).WithTrustLadder([]content.TrustTierDTO{
		{Name: "player", Rank: 0},
		{Name: "architect", Rank: 30, Flags: []string{content.FlagAdmin}},
		{Name: "admin", Rank: 40, Flags: []string{content.FlagAdmin}},
	})

	// architect -> architect on Bob: allowed (rank 30 <= actor 30).
	if resp, err := svc.SetAccountTier(ctx, setTierReq("a-arch", "Bob", "architect")); err != nil || !resp.GetOk() {
		t.Fatalf("architect should be able to grant architect (<= own rank), got resp=%+v err=%v", resp, err)
	}

	// architect -> admin on Bob: refused (rank 40 > actor 30).
	if resp, _ := svc.SetAccountTier(ctx, setTierReq("a-arch", "Bob", "admin")); resp.GetOk() {
		t.Fatal("architect must not grant admin (a tier above its own standing)")
	}

	// architect changing Boss (an admin, rank 40 > 30): refused (target outranks the actor).
	if resp, _ := svc.SetAccountTier(ctx, setTierReq("a-arch", "Boss", "player")); resp.GetOk() {
		t.Fatal("architect must not change the tier of an account that outranks it")
	}
}

// TestSetAccountTierUnverifiedActorRefused: an actor whose account has no tier (empty / not found — the
// unverified/dev baseline) grants no capability and is refused. Guards the "positive capability required,
// baseline rank-0 cannot manage" posture.
func TestSetAccountTierUnverifiedActorRefused(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	fs.tiers["a-bob"] = "player"
	fs.charAccount["Bob"] = "a-bob"
	svc := newTestService(fs)

	if resp, _ := svc.SetAccountTier(ctx, setTierReq("a-nobody", "Bob", "builder")); resp.GetOk() {
		t.Fatal("an actor with no tier (baseline) must not be authorized to change tiers")
	}
}

// TestAccountPrefsGetSet (#23): GetAccountPrefs reports the tri-state (absent when never set), SetAccountPrefs
// persists a present field, and an ABSENT field is a NO-OP (it does not clear a stored value).
func TestAccountPrefsGetSet(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	svc := newTestService(fs)

	// Never set: the color pref is ABSENT (nil optional), so the gate keeps its default.
	got, err := svc.GetAccountPrefs(ctx, &accountv1.GetAccountPrefsRequest{AccountId: "acct-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ColorEnabled != nil {
		t.Fatalf("an unset color pref must be absent, got %v", got.GetColorEnabled())
	}

	// Set color OFF: the field is present + false on read-back.
	off := false
	if _, err := svc.SetAccountPrefs(ctx, &accountv1.SetAccountPrefsRequest{AccountId: "acct-1", ColorEnabled: &off}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.GetAccountPrefs(ctx, &accountv1.GetAccountPrefsRequest{AccountId: "acct-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ColorEnabled == nil || got.GetColorEnabled() {
		t.Fatalf("color pref should be present+false after `color off`, got %v", got.ColorEnabled)
	}

	// Set color ON: overwrites to present + true.
	on := true
	if _, err := svc.SetAccountPrefs(ctx, &accountv1.SetAccountPrefsRequest{AccountId: "acct-1", ColorEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetAccountPrefs(ctx, &accountv1.GetAccountPrefsRequest{AccountId: "acct-1"})
	if got.ColorEnabled == nil || !got.GetColorEnabled() {
		t.Fatalf("color pref should be present+true after `color on`, got %v", got.ColorEnabled)
	}

	// A SetAccountPrefs with NO fields present is a no-op: the stored value is untouched.
	if _, err := svc.SetAccountPrefs(ctx, &accountv1.SetAccountPrefsRequest{AccountId: "acct-1"}); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.GetAccountPrefs(ctx, &accountv1.GetAccountPrefsRequest{AccountId: "acct-1"})
	if got.ColorEnabled == nil || !got.GetColorEnabled() {
		t.Fatalf("an empty SetAccountPrefs must not clear the stored value, got %v", got.ColorEnabled)
	}
}

// TestAccountPrefsValidation (#23): a missing account_id is an InvalidArgument on both RPCs; a store error is
// surfaced as an Internal gRPC error (not swallowed at the service).
func TestAccountPrefsValidation(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(newFakeStore())

	if _, err := svc.GetAccountPrefs(ctx, &accountv1.GetAccountPrefsRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("GetAccountPrefs with no account_id should be InvalidArgument, got %v", err)
	}
	if _, err := svc.SetAccountPrefs(ctx, &accountv1.SetAccountPrefsRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("SetAccountPrefs with no account_id should be InvalidArgument, got %v", err)
	}

	// A store read error surfaces as Internal.
	fsErr := newFakeStore()
	fsErr.colorGetErr = errors.New("prefs store unavailable")
	svcErr := newTestService(fsErr)
	if _, err := svcErr.GetAccountPrefs(ctx, &accountv1.GetAccountPrefsRequest{AccountId: "a"}); status.Code(err) != codes.Internal {
		t.Fatalf("a store read error should surface as Internal, got %v", err)
	}
	// A store write error surfaces as Internal.
	fsErr.colorSetErr = errors.New("prefs store unavailable")
	on := true
	if _, err := svcErr.SetAccountPrefs(ctx, &accountv1.SetAccountPrefsRequest{AccountId: "a", ColorEnabled: &on}); status.Code(err) != codes.Internal {
		t.Fatalf("a store write error should surface as Internal, got %v", err)
	}
}
