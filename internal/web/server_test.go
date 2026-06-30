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
	"time"

	"github.com/double-nibble/telosmud/internal/store"
)

// server_test.go — Phase 14.7: the OAuth sign-in flow end to end against a STUBBED provider (no real GitHub),
// so CI stays hermetic. Covers: login starts the flow; the callback exchanges + creates the account + sets a
// session; the dashboard + Play work when signed in; and the CSRF state guard rejects a tampered callback.

// fakeWebStore is an in-memory web.Store.
type fakeWebStore struct {
	identities map[string]string // "provider/uid" -> account
	created    int
	chars      map[string][]store.CharacterSummary
}

func newFakeWebStore() *fakeWebStore {
	return &fakeWebStore{identities: map[string]string{}, chars: map[string][]store.CharacterSummary{}}
}

func (f *fakeWebStore) FindIdentity(_ context.Context, provider, uid string) (string, bool, error) {
	a, ok := f.identities[provider+"/"+uid]
	return a, ok, nil
}

func (f *fakeWebStore) CreateAccountWithIdentity(_ context.Context, provider, uid, _, _ string) (string, error) {
	f.created++
	acct := "acct-" + uid
	f.identities[provider+"/"+uid] = acct
	f.chars[acct] = []store.CharacterSummary{{ID: "c1", Name: "Aragorn", ZoneRef: "midgaard"}}
	return acct, nil
}

func (f *fakeWebStore) AccountDisplayName(_ context.Context, _ string) (string, bool, error) {
	return "octocat", true, nil
}

func (f *fakeWebStore) AccountCharacters(_ context.Context, acct string) ([]store.CharacterSummary, error) {
	return f.chars[acct], nil
}

type fakeMinter struct{}

func (fakeMinter) Mint(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	return "CODE1234", nil
}

// stubProvider is an httptest server standing in for GitHub's token + userinfo endpoints.
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
		_, _ = io.WriteString(w, `{"id":12345,"login":"octocat","email":"octo@example.com"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestWebsite(t *testing.T, st Store) (*httptest.Server, *http.Client) {
	t.Helper()
	stub := stubProvider(t)
	s := New(st, fakeMinter{}, Config{
		Provider: OAuthProvider{
			Name: "github", ClientID: "id", ClientSecret: "secret",
			AuthURL: "https://provider.test/authorize", TokenURL: stub.URL + "/token", UserURL: stub.URL + "/user",
			RedirectURL: "http://web.test/auth/github/callback", Scopes: []string{"read:user"},
		},
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return ts, client
}

func TestOAuthSignInFlow(t *testing.T) {
	st := newFakeWebStore()
	ts, client := newTestWebsite(t, st)

	// 1. /login starts the flow: a redirect to the provider with state + PKCE challenge, and a flow cookie.
	resp, err := client.Get(ts.URL + "/login")
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

	// 2. The provider "redirects back" to the callback with our state + a code. The flow cookie (in the jar)
	//    carries the PKCE verifier. The callback exchanges + creates the account + sets a session.
	resp, err = client.Get(ts.URL + "/auth/github/callback?code=abc123&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/dashboard" {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("callback status = %d loc = %q body = %q", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if st.created != 1 {
		t.Fatalf("a first-time sign-in should create 1 account, got %d", st.created)
	}

	// 3. The dashboard now renders for the signed-in account.
	body := getBody(t, client, ts.URL+"/dashboard")
	if !strings.Contains(body, "octocat") || !strings.Contains(body, "Aragorn") {
		t.Fatalf("dashboard missing name/characters: %q", body)
	}

	// 4. Play mints a link code.
	resp, err = client.PostForm(ts.URL+"/play", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	pbody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(pbody), "CODE1234") {
		t.Fatalf("play page missing the link code: %q", pbody)
	}

	// 5. A SECOND sign-in with the same identity logs into the SAME account (no new account).
	st2 := st.created
	resp, _ = client.Get(ts.URL + "/login")
	loc, _ = url.Parse(resp.Header.Get("Location"))
	_, _ = client.Get(ts.URL + "/auth/github/callback?code=abc123&state=" + url.QueryEscape(loc.Query().Get("state")))
	if st.created != st2 {
		t.Fatalf("a returning sign-in must NOT create a new account (created went %d -> %d)", st2, st.created)
	}
}

func TestOAuthCallbackRejectsBadState(t *testing.T) {
	ts, client := newTestWebsite(t, newFakeWebStore())
	// Start a flow (sets the flow cookie with the real state), then call back with a DIFFERENT state.
	_, _ = client.Get(ts.URL + "/login")
	resp, err := client.Get(ts.URL + "/auth/github/callback?code=abc&state=forged")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a forged callback state must be rejected (400), got %d", resp.StatusCode)
	}
}

func TestNewPanicsOnWeakSessionKey(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New must panic on a SessionKey shorter than 16 bytes (audit F3)")
		}
	}()
	New(newFakeWebStore(), fakeMinter{}, Config{SessionKey: []byte("short")})
}

func TestLogoutIsPostOnly(t *testing.T) {
	ts, client := newTestWebsite(t, newFakeWebStore())
	// A GET to /logout must NOT clear the session (it isn't a logout route) — the mux only binds POST.
	resp, err := client.Get(ts.URL + "/logout")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatalf("GET /logout should not perform a logout redirect; got %d", resp.StatusCode)
	}
}

func TestAssetsServed(t *testing.T) {
	ts, client := newTestWebsite(t, newFakeWebStore())
	for _, name := range []string{"telosmud-logo.svg", "telosmud-logo-dev.svg", "telosmud-logo.png"} {
		resp, err := client.Get(ts.URL + "/assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("asset %s status = %d, want 200", name, resp.StatusCode)
		}
	}
}

func TestDevLogoVariant(t *testing.T) {
	// A dev instance renders the -dev logo; the default (prod) renders the plain one.
	stub := stubProvider(t)
	prov := OAuthProvider{Name: "github", ClientID: "id", ClientSecret: "s", TokenURL: stub.URL, UserURL: stub.URL}
	key := []byte("0123456789abcdef0123456789abcdef")

	dev := New(newFakeWebStore(), fakeMinter{}, Config{Provider: prov, SessionKey: key, Dev: true})
	devTS := httptest.NewServer(dev.Handler())
	t.Cleanup(devTS.Close)
	if body := getBody(t, http.DefaultClient, devTS.URL+"/"); !strings.Contains(body, "telosmud-logo-dev.svg") {
		t.Fatalf("dev instance should render the -dev logo; body = %q", body)
	}

	prod := New(newFakeWebStore(), fakeMinter{}, Config{Provider: prov, SessionKey: key, Dev: false})
	prodTS := httptest.NewServer(prod.Handler())
	t.Cleanup(prodTS.Close)
	body := getBody(t, http.DefaultClient, prodTS.URL+"/")
	if !strings.Contains(body, "telosmud-logo.svg") || strings.Contains(body, "telosmud-logo-dev.svg") {
		t.Fatalf("prod instance should render the plain logo; body = %q", body)
	}
}

func getBody(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
