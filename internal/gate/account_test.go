package gate

import (
	"context"
	"testing"
	"time"
)

// account_test.go — Phase 14.1: the gate's account seam. The stub backs the gate until a real account
// service is wired, preserving the legacy name login; a real client is exercised end-to-end in 14.2+.

func TestStubAccountClientListCharacters(t *testing.T) {
	var ac AccountClient = stubAccountClient{}
	chars, err := ac.ListCharacters(context.Background(), "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(chars) != 1 || chars[0].Name != "Alice" {
		t.Fatalf("stub characters = %+v, want one named Alice", chars)
	}
}

func TestNewServerDefaultsToStubAccount(t *testing.T) {
	s := newServer(":0", nil, newPool(), nil)
	if _, ok := s.account.(stubAccountClient); !ok {
		t.Fatalf("a fresh gate should default to the stub account client, got %T", s.account)
	}
	// WithAccountClient ignores nil (keeps the stub) and accepts a real one.
	s.WithAccountClient(nil)
	if _, ok := s.account.(stubAccountClient); !ok {
		t.Fatal("WithAccountClient(nil) should keep the stub")
	}
}

// deviceAuthUnsupported gives the pre-Phase-15 journey fakes the device-auth methods as no-ops — those tests
// exercise the link-code/passphrase/SSH paths, not the OAuth device flow (which has its own fake).
type deviceAuthUnsupported struct{}

func (deviceAuthUnsupported) StartDeviceAuth(context.Context, string) (string, string, time.Duration, error) {
	return "", "", 0, nil
}

func (deviceAuthUnsupported) PollDeviceAuth(context.Context, string) (string, string, []CharacterInfo, error) {
	return "expired", "", nil, nil
}

func (deviceAuthUnsupported) GetChargenFlow(context.Context) (bool, []ChargenStep, []ChargenBundleOption, int, error) {
	return false, nil, nil, 0, nil
}

func (deviceAuthUnsupported) CreateChargenCharacter(context.Context, string, string, map[string]string, map[string]map[string]int) (string, string, bool, error) {
	return "", "unavailable", false, nil
}
