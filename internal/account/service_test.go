package account

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	accountv1 "github.com/double-nibble/telosmud/api/gen/telosmud/account/v1"
	"github.com/double-nibble/telosmud/internal/store"
)

// service_test.go — Phase 14.1: the Account service RPC shapes over an in-memory fake store (no Postgres).

// fakeStore is an in-memory CharStore for the service tests.
type fakeStore struct {
	chars       map[string][]store.CharacterSummary
	taken       map[string]bool
	nameAccount map[string]string               // character name -> account id (Phase 14.5)
	auth        map[string]store.PassphraseAuth // account id -> auth row (Phase 14.5)
	sshKeys     map[string]string               // fingerprint -> account id (Phase 14.6)
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		chars:       map[string][]store.CharacterSummary{},
		taken:       map[string]bool{},
		nameAccount: map[string]string{},
		auth:        map[string]store.PassphraseAuth{},
		sshKeys:     map[string]string{},
	}
}

func (f *fakeStore) ResolveSSHKey(_ context.Context, fingerprint string) (string, bool, error) {
	acct, ok := f.sshKeys[fingerprint]
	return acct, ok, nil
}

func (f *fakeStore) CharacterAccount(_ context.Context, name string) (string, bool, error) {
	acct, ok := f.nameAccount[name]
	return acct, ok, nil
}

func (f *fakeStore) AccountAuth(_ context.Context, accountID string) (store.PassphraseAuth, bool, error) {
	a, ok := f.auth[accountID]
	return a, ok, nil
}

func (f *fakeStore) SetPassphraseHash(_ context.Context, accountID, hash string) error {
	f.auth[accountID] = store.PassphraseAuth{Hash: hash}
	return nil
}

func (f *fakeStore) RecordAuthFailure(_ context.Context, accountID string, lockAfter int, lockFor time.Duration) (int, error) {
	a := f.auth[accountID]
	a.FailedAttempts++
	if a.FailedAttempts >= lockAfter {
		a.LockedUntil = time.Now().Add(lockFor)
	}
	f.auth[accountID] = a
	return a.FailedAttempts, nil
}

func (f *fakeStore) ResetAuthFailures(_ context.Context, accountID string) error {
	a := f.auth[accountID]
	a.FailedAttempts, a.LockedUntil = 0, time.Time{}
	f.auth[accountID] = a
	return nil
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
		{"bad\tname", "invalid_char", false},
	}
	for _, tc := range cases {
		reason, ok := ValidateCharacterName(tc.name)
		if ok != tc.ok || reason != tc.reason {
			t.Errorf("ValidateCharacterName(%q) = (%q,%v), want (%q,%v)", tc.name, reason, ok, tc.reason, tc.ok)
		}
	}
}
