package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// server_test.go — the Phase-15 OAuth BROKER device flow, end to end against a STUBBED provider (hermetic).
// /login/<device> starts OAuth; the callback exchanges + resolves the account + marks the device authed.

// fakeStore is an in-memory web.Store (OAuth identity resolution only).
type fakeStore struct {
	identities    map[string]string // "provider/uid" -> account
	created       int
	lastBootstrap bool // the bootstrapAdmin flag passed to the most recent CreateAccountWithIdentity
}

func newFakeStore() *fakeStore { return &fakeStore{identities: map[string]string{}} }

func (f *fakeStore) FindIdentity(_ context.Context, provider, uid string) (string, bool, error) {
	a, ok := f.identities[provider+"/"+uid]
	return a, ok, nil
}

func (f *fakeStore) CreateAccountWithIdentity(_ context.Context, provider, uid, _, _ string, bootstrapAdmin bool) (string, error) {
	f.created++
	f.lastBootstrap = bootstrapAdmin
	acct := "acct-" + uid
	f.identities[provider+"/"+uid] = acct
	return acct, nil
}

// fakeAuthorizer records AuthorizeDevice calls; ok controls whether the device session is still live.
type fakeAuthorizer struct {
	ok         bool
	gotDevice  string
	gotAccount string
	callCount  int
}

func (f *fakeAuthorizer) AuthorizeDevice(_ context.Context, device, account string) (bool, error) {
	f.callCount++
	f.gotDevice, f.gotAccount = device, account
	return f.ok, nil
}

// stubProvider stands in for GitHub's token + userinfo endpoints.
func stubProvider(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.FormValue("code") == "" || r.FormValue("code_verifier") == "" {
			http.Error(w, "missing code/verifier", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"stub-token"}`)
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer stub-token" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":4242,"login":"octocat","email":"octo@example.com"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestBroker(t *testing.T, auth *fakeAuthorizer, st Store) (*httptest.Server, *http.Client) {
	return newTestBrokerAdmin(t, auth, st, "")
}

func newTestBrokerAdmin(t *testing.T, auth *fakeAuthorizer, st Store, bootstrapAdmin string) (*httptest.Server, *http.Client) {
	t.Helper()
	stub := stubProvider(t)
	s := New(st, Config{
		Provider: OAuthProvider{
			Name: "github", ClientID: "id", ClientSecret: "secret",
			AuthURL: "https://provider.test/authorize", TokenURL: stub.URL + "/token", UserURL: stub.URL + "/user",
			RedirectURL: "http://broker.test/auth/github/callback", Scopes: []string{"read:user"},
		},
		Authorizer:     auth,
		SessionKey:     []byte("0123456789abcdef0123456789abcdef"),
		BootstrapAdmin: bootstrapAdmin,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return ts, client
}

func TestDeviceLoginFlow(t *testing.T) {
	auth := &fakeAuthorizer{ok: true}
	st := newFakeStore()
	ts, client := newTestBroker(t, auth, st)

	// 1. /login/<device> starts OAuth: a redirect to the provider with state + PKCE challenge + a flow cookie.
	resp, err := client.Get(ts.URL + "/login/DEVCODE123")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("/login status = %d, want 303", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	state := loc.Query().Get("state")
	if state == "" || loc.Query().Get("code_challenge") == "" {
		t.Fatalf("/login redirect missing state/challenge: %s", resp.Header.Get("Location"))
	}

	// 2. The provider "redirects back" with our state + a code; the callback exchanges, resolves the account,
	//    and marks the device authed.
	resp, err = client.Get(ts.URL + "/auth/github/callback?code=abc&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Signed in") {
		t.Fatalf("callback status=%d body=%q, want a 200 success page", resp.StatusCode, body)
	}
	if st.created != 1 {
		t.Fatalf("a first-time sign-in should create 1 account, got %d", st.created)
	}
	if auth.callCount != 1 || auth.gotDevice != "DEVCODE123" || auth.gotAccount != "acct-4242" {
		t.Fatalf("AuthorizeDevice got (device=%q account=%q calls=%d), want (DEVCODE123, acct-4242, 1)", auth.gotDevice, auth.gotAccount, auth.callCount)
	}
}

// TestIsBootstrapAdmin covers the config-pin match (#27): case-insensitive on the LOGIN only; empty config
// (the default) matches nothing; the email is NEVER matched (login-only by design — the email is an
// unverified, user-settable provider field).
func TestIsBootstrapAdmin(t *testing.T) {
	mk := func(pin string) *Server {
		return New(newFakeStore(), Config{SessionKey: []byte("0123456789abcdef0123456789abcdef"), Authorizer: &fakeAuthorizer{}, BootstrapAdmin: pin})
	}
	if mk("").isBootstrapAdmin("octocat") {
		t.Error("empty bootstrap config must match nothing")
	}
	if !mk("octocat").isBootstrapAdmin("OctoCat") {
		t.Error("should match the login case-insensitively")
	}
	if mk("octo@example.com").isBootstrapAdmin("octocat") {
		t.Error("a pin must NOT match the login as an email (login-only)")
	}
	if mk("theboss").isBootstrapAdmin("octocat") {
		t.Error("a non-matching login must not be bootstrap admin")
	}
}

// TestCallbackGrantsBootstrapAdmin proves the pin flows end-to-end: the callback passes bootstrapAdmin=true
// to the store ONLY when the configured pin matches the OAuth LOGIN (stub user login "octocat" / email
// "octo@example.com"). An email pin must NOT grant admin (login-only by design).
func TestCallbackGrantsBootstrapAdmin(t *testing.T) {
	for _, tc := range []struct {
		pin  string
		want bool
	}{
		{"", false},
		{"octocat", true},
		{"octo@example.com", false}, // the email must not match — login-only
		{"someoneelse", false},
	} {
		auth := &fakeAuthorizer{ok: true}
		st := newFakeStore()
		ts, client := newTestBrokerAdmin(t, auth, st, tc.pin)
		resp, err := client.Get(ts.URL + "/login/DEV")
		if err != nil {
			t.Fatal(err)
		}
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if _, err := client.Get(ts.URL + "/auth/github/callback?code=abc&state=" + url.QueryEscape(loc.Query().Get("state"))); err != nil {
			t.Fatal(err)
		}
		if st.created != 1 {
			t.Fatalf("pin=%q: expected 1 account created, got %d", tc.pin, st.created)
		}
		if st.lastBootstrap != tc.want {
			t.Errorf("pin=%q: store got bootstrapAdmin=%v, want %v", tc.pin, st.lastBootstrap, tc.want)
		}
	}
}

func TestCallbackRejectsBadState(t *testing.T) {
	auth := &fakeAuthorizer{ok: true}
	ts, client := newTestBroker(t, auth, newFakeStore())
	_, _ = client.Get(ts.URL + "/login/DEV") // sets the flow cookie with the real state
	resp, err := client.Get(ts.URL + "/auth/github/callback?code=abc&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	if body := readBody(t, resp); !strings.Contains(body, "Invalid sign-in state") {
		t.Fatalf("a forged state should render the invalid-state notice; body=%q", body)
	}
	if auth.callCount != 0 {
		t.Fatal("a forged callback must not authorize any device")
	}
}

func TestCallbackExpiredDevice(t *testing.T) {
	auth := &fakeAuthorizer{ok: false} // the device session expired before the browser finished
	ts, client := newTestBroker(t, auth, newFakeStore())
	resp, _ := client.Get(ts.URL + "/login/DEV")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp, err := client.Get(ts.URL + "/auth/github/callback?code=abc&state=" + url.QueryEscape(loc.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	if body := readBody(t, resp); !strings.Contains(body, "expired") {
		t.Fatalf("an expired device should render the expired notice; body=%q", body)
	}
}

func TestNewPanicsOnWeakSessionKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New must panic on a SessionKey shorter than 16 bytes")
		}
	}()
	New(newFakeStore(), Config{SessionKey: []byte("short"), Authorizer: &fakeAuthorizer{}})
}

func TestAssetsServed(t *testing.T) {
	ts, client := newTestBroker(t, &fakeAuthorizer{}, newFakeStore())
	resp, err := client.Get(ts.URL + "/assets/telosmud-logo.svg")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("logo asset status = %d, want 200", resp.StatusCode)
	}
}

func TestLogoURLForEnv(t *testing.T) {
	cases := []struct {
		env  string
		want string
	}{
		{"prod", "/assets/telosmud-logo.svg"},
		{"", "/assets/telosmud-logo.svg"},
		{"anything-else", "/assets/telosmud-logo.svg"},
		{"dev", "/assets/telosmud-logo-dev.svg"},
		{"staging", "/assets/telosmud-logo-staging.svg"},
		{" Staging ", "/assets/telosmud-logo-staging.svg"}, // case- and space-insensitive
		{"DEV", "/assets/telosmud-logo-dev.svg"},
	}
	for _, tc := range cases {
		if got := logoURLForEnv(tc.env); got != tc.want {
			t.Errorf("logoURLForEnv(%q) = %q, want %q", tc.env, got, tc.want)
		}
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
