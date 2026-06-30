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

	"github.com/double-nibble/telosmud/internal/content"
)

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// chargen_test.go — Phase 14.8b: the website's create-character flow against a fake ChargenService (hermetic).

type fakeChargen struct {
	flow    content.ChargenDTO
	options []content.ChargenBundleOption
	created []string // names passed to BuildCharacter that validated
	reason  string   // when non-empty, BuildCharacter returns it (a forced validation failure)
}

func (f *fakeChargen) ChargenFlow() (content.ChargenDTO, []content.ChargenBundleOption, bool) {
	return f.flow, f.options, len(f.flow.Steps) > 0
}

func (f *fakeChargen) BuildCharacter(_ context.Context, _, name string, _ map[string]string, _ map[string]map[string]int) (string, string, error) {
	if f.reason != "" {
		return "", f.reason, nil
	}
	f.created = append(f.created, name)
	return "id-" + name, "", nil
}

func newDemoChargen() *fakeChargen {
	return &fakeChargen{
		flow: content.ChargenDTO{Steps: []content.ChargenStepDTO{
			{Kind: "bundle_choice", ID: "race", Prompt: "Choose your race", BundleKind: "race", Pick: 1},
			{Kind: "bundle_choice", ID: "class", Prompt: "Choose your class", BundleKind: "class", Pick: 1},
			{Kind: "point_buy", ID: "attrs", Prompt: "Allocate", Attributes: []string{"strength", "intellect"}, Points: 27, Base: 8, Min: 8, Max: 15},
		}},
		options: []content.ChargenBundleOption{
			{Ref: "elf", Kind: "race", Label: "Elf"},
			{Ref: "dwarf", Kind: "race", Label: "Dwarf"},
			{Ref: "fighter", Kind: "class", Label: "Fighter"},
		},
	}
}

// signedInClient builds a website with the given chargen service, runs the OAuth stub flow to establish a
// session, and returns the test server + an authenticated client.
func signedInClient(t *testing.T, cg ChargenService) (*httptest.Server, *http.Client) {
	t.Helper()
	stub := stubProvider(t)
	s := New(newFakeWebStore(), fakeMinter{}, Config{
		Provider: OAuthProvider{
			Name: "github", ClientID: "id", ClientSecret: "secret",
			AuthURL: "https://provider.test/authorize", TokenURL: stub.URL + "/token", UserURL: stub.URL + "/user",
		},
		SessionKey: []byte("0123456789abcdef0123456789abcdef"),
		Chargen:    cg,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Sign in via the stub OAuth flow.
	resp, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	_, err = client.Get(ts.URL + "/auth/github/callback?code=abc&state=" + url.QueryEscape(loc.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	return ts, client
}

func TestChargenFormRenders(t *testing.T) {
	ts, client := signedInClient(t, newDemoChargen())
	body := getBody(t, client, ts.URL+"/chargen")
	for _, want := range []string{"Choose your race", "Elf", "Dwarf", "Fighter", "Choose your class", "Allocate", "strength", "intellect"} {
		if !strings.Contains(body, want) {
			t.Fatalf("chargen form missing %q; body = %q", want, body)
		}
	}
	// The dashboard offers the create link when chargen is configured.
	if d := getBody(t, client, ts.URL+"/dashboard"); !strings.Contains(d, "/chargen") {
		t.Fatalf("dashboard missing the create-character link; body = %q", d)
	}
}

func TestChargenCreateValid(t *testing.T) {
	cg := newDemoChargen()
	ts, client := signedInClient(t, cg)
	form := url.Values{
		"name": {"Aragorn"}, "race": {"elf"}, "class": {"fighter"},
		"attrs_strength": {"15"}, "attrs_intellect": {"13"},
	}
	resp, err := client.PostForm(ts.URL+"/chargen", form)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/dashboard" {
		t.Fatalf("valid create status = %d loc = %q, want 303 -> /dashboard", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(cg.created) != 1 || cg.created[0] != "Aragorn" {
		t.Fatalf("BuildCharacter not called with the submitted name: %v", cg.created)
	}
}

func TestChargenCreateInvalidReRenders(t *testing.T) {
	cg := newDemoChargen()
	cg.reason = "That name is already taken."
	ts, client := signedInClient(t, cg)
	form := url.Values{"name": {"Taken"}, "race": {"elf"}, "class": {"fighter"}, "attrs_strength": {"15"}, "attrs_intellect": {"13"}}
	resp, err := client.PostForm(ts.URL+"/chargen", form)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("invalid create status = %d, want 200 (re-render)", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "That name is already taken.") || !strings.Contains(body, `value="Taken"`) {
		t.Fatalf("re-render should show the error + preserve the name; body = %q", body)
	}
}

func TestChargenRequiresSession(t *testing.T) {
	ts, _ := signedInClient(t, newDemoChargen())
	// A fresh, unauthenticated client is redirected away from /chargen.
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := anon.Get(ts.URL + "/chargen")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anonymous /chargen status = %d, want a 303 redirect to sign-in", resp.StatusCode)
	}
}
