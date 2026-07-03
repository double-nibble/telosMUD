package account

import (
	"context"
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
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		chars:       map[string][]store.CharacterSummary{},
		taken:       map[string]bool{},
		tiers:       map[string]string{},
		charAccount: map[string]string{},
	}
}

func (f *fakeStore) AccountTier(_ context.Context, accountID string) (string, bool, error) {
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
