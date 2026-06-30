// Package web is telos-account's website + OAuth front door (docs/ACCOUNT.md §2/§3, Phase 14.7): server-
// rendered sign-in (GitHub OAuth, Authorization Code + PKCE), the account dashboard, and the "Play" bridge
// that mints a link code. It runs IN the telos-account process (alongside the gRPC API), so it talks to the
// store + the account service in-process — no provider tokens ever reach the gate or world.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// OAuthProvider is one OAuth provider's config. The endpoint URLs are fields (not hardcoded) so a test can
// point them at a stub server — the whole flow runs hermetically in CI without hitting real GitHub.
type OAuthProvider struct {
	Name         string
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	UserURL      string
	RedirectURL  string
	Scopes       []string
	HTTP         *http.Client // injectable; defaults to http.DefaultClient
}

// GitHubProvider builds the GitHub provider with the standard endpoints; only the credentials + redirect vary.
func GitHubProvider(clientID, clientSecret, redirectURL string) OAuthProvider {
	//nolint:gosec // G101 false positive: the "access_token" in GitHub's public token URL is an endpoint path, not a credential.
	return OAuthProvider{
		Name:         "github",
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthURL:      "https://github.com/login/oauth/authorize",
		TokenURL:     "https://github.com/login/oauth/access_token",
		UserURL:      "https://api.github.com/user",
		RedirectURL:  redirectURL,
		Scopes:       []string{"read:user", "user:email"},
	}
}

// Configured reports whether the provider has credentials (without them, sign-in is unavailable).
func (p OAuthProvider) Configured() bool { return p.ClientID != "" && p.ClientSecret != "" }

func (p OAuthProvider) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

// AuthCodeURL builds the provider authorize URL for a sign-in, carrying the CSRF `state` and the PKCE
// `challenge` (S256). The user is redirected here to approve.
func (p OAuthProvider) AuthCodeURL(state, challenge string) string {
	q := url.Values{
		"client_id":             {p.ClientID},
		"redirect_uri":          {p.RedirectURL},
		"scope":                 {strings.Join(p.Scopes, " ")},
		"state":                 {state},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	return p.AuthURL + "?" + q.Encode()
}

// Identity is the minimal provider profile we persist: a STABLE provider user id + an informational email.
type Identity struct {
	ProviderUID string
	Login       string
	Email       string
}

// Exchange swaps an authorization `code` (with the PKCE `verifier`) for an access token.
func (p OAuthProvider) Exchange(ctx context.Context, code, verifier string) (string, error) {
	form := url.Values{
		"client_id":     {p.ClientID},
		"client_secret": {p.ClientSecret},
		"code":          {code},
		"redirect_uri":  {p.RedirectURL},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json") // GitHub returns form-encoded by default; ask for JSON
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth exchange: status %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("oauth exchange decode: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("oauth exchange: no access_token (%s)", tok.Error)
	}
	return tok.AccessToken, nil
}

// FetchIdentity fetches the provider profile with the access token. The provider_uid is the STABLE numeric id
// (never the login/email, which can change).
func (p OAuthProvider) FetchIdentity(ctx context.Context, token string) (Identity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserURL, nil)
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := p.client().Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("oauth userinfo: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("oauth userinfo: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var u struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return Identity{}, fmt.Errorf("oauth userinfo decode: %w", err)
	}
	if u.ID == 0 {
		return Identity{}, fmt.Errorf("oauth userinfo: missing user id")
	}
	return Identity{ProviderUID: strconv.FormatInt(u.ID, 10), Login: u.Login, Email: u.Email}, nil
}

// --- PKCE + random helpers ------------------------------------------------------------------------------

// newVerifier returns a high-entropy PKCE code verifier (base64url, ~43 chars).
func newVerifier() (string, error) { return randB64(32) }

// challengeFor returns the S256 PKCE challenge for a verifier.
func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randB64 returns n random bytes as a base64url string (CSRF state, PKCE verifier).
func randB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
