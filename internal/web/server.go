package web

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/double-nibble/telosmud/internal/store"
)

// server.go — the telos-account website (Phase 14.7): GitHub OAuth sign-in, the account dashboard, and the
// Play bridge. Server-rendered html/template; sessions are signed cookies (session.go); the OAuth flow is
// Authorization Code + PKCE (oauth.go). It depends on the store + a link-code minter through narrow
// interfaces so the handler tests run with in-memory fakes + a stubbed OAuth provider (hermetic CI).

// Store is the persistence surface the website needs.
type Store interface {
	FindIdentity(ctx context.Context, provider, providerUID string) (string, bool, error)
	CreateAccountWithIdentity(ctx context.Context, provider, providerUID, email, displayName string) (string, error)
	AccountDisplayName(ctx context.Context, accountID string) (string, bool, error)
	AccountCharacters(ctx context.Context, accountID string) ([]store.CharacterSummary, error)
}

// LinkCodeMinter mints a link code for the Play button (account.LinkCodeStore satisfies this).
type LinkCodeMinter interface {
	Mint(ctx context.Context, accountID, characterID string, ttl time.Duration) (string, error)
}

// Server is the website. Construct with New, then ServeHTTP / Handler().
type Server struct {
	store         Store
	codes         LinkCodeMinter
	provider      OAuthProvider
	sign          signer
	secureCookies bool
	linkCodeTTL   time.Duration
	gateHint      string // shown on the Play page ("connect to <host> and type: connect <code>")
	tmpl          *template.Template
	log           *slog.Logger
}

// Config carries the website's wiring.
type Config struct {
	Provider      OAuthProvider
	SessionKey    []byte // HMAC key for signed cookies (a stable random key in prod)
	SecureCookies bool   // set Secure on cookies (true when served over TLS)
	LinkCodeTTL   time.Duration
	GateHint      string
	Dev           bool // dev instance: render the -dev logo variant so operators can tell it from prod
	Log           *slog.Logger
}

// New builds the website Server. It PANICS on a SessionKey shorter than 16 bytes: that key is the sole secret
// underwriting every signed cookie, and an empty/weak key makes session cookies universally forgeable (account
// impersonation). Failing loud here is the guardrail against a caller misconfig (security audit F3).
func New(st Store, codes LinkCodeMinter, cfg Config) *Server {
	if len(cfg.SessionKey) < 16 {
		panic("web: SessionKey must be at least 16 bytes (it signs all session cookies)")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	ttl := cfg.LinkCodeTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	// The logo URL is fixed at construction (by env), exposed to every template via the logoURL func so the
	// shared {{template "head"}} can reference it without threading data through each page's pipeline.
	logoURL := "/assets/telosmud-logo.svg"
	if cfg.Dev {
		logoURL = "/assets/telosmud-logo-dev.svg"
	}
	tmpl := template.Must(template.New("web").
		Funcs(template.FuncMap{"logoURL": func() string { return logoURL }}).
		Parse(pageTemplates))
	return &Server{
		store:         st,
		codes:         codes,
		provider:      cfg.Provider,
		sign:          signer{key: cfg.SessionKey},
		secureCookies: cfg.SecureCookies,
		linkCodeTTL:   ttl,
		gateHint:      cfg.GateHint,
		tmpl:          tmpl,
		log:           log,
	}
}

// Handler returns the website's HTTP handler (the route mux).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /assets/", assetsHandler())
	mux.HandleFunc("GET /", s.handleHome)
	mux.HandleFunc("GET /login", s.handleLogin)
	mux.HandleFunc("GET /auth/github/callback", s.handleCallback)
	mux.HandleFunc("GET /dashboard", s.handleDashboard)
	mux.HandleFunc("POST /play", s.handlePlay)
	mux.HandleFunc("POST /logout", s.handleLogout) // POST (not GET): logout is state-changing; a GET enables logout-CSRF
	return mux
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if s.sessionAccount(r) != "" {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.render(w, "home", map[string]any{"Configured": s.provider.Configured()})
}

// handleLogin starts the OAuth flow: generate the CSRF state + PKCE verifier, stash them in a signed flow
// cookie, and redirect to the provider's authorize URL.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.provider.Configured() {
		http.Error(w, "sign-in is not configured", http.StatusServiceUnavailable)
		return
	}
	state, err := randB64(24)
	if err != nil {
		s.fail(w, "login", err)
		return
	}
	verifier, err := newVerifier()
	if err != nil {
		s.fail(w, "login", err)
		return
	}
	s.setFlow(w, state, verifier)
	http.Redirect(w, r, s.provider.AuthCodeURL(state, challengeFor(verifier)), http.StatusSeeOther)
}

// handleCallback completes the OAuth flow: verify state (CSRF), exchange the code (with the PKCE verifier),
// fetch the identity, look up or create the account, and set the session cookie.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	// Always burn the single-use flow cookie first, even on the provider-error path, so a repeated
	// ?error= callback can't keep a live flow cookie around for its full TTL (security audit F9).
	wantState, verifier, ok := s.takeFlow(w, r)
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "sign-in was cancelled or denied", http.StatusBadRequest)
		return
	}
	if !ok {
		http.Error(w, "the sign-in session expired; please try again", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("state") != wantState {
		http.Error(w, "invalid sign-in state", http.StatusBadRequest) // CSRF guard
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	token, err := s.provider.Exchange(ctx, code, verifier)
	if err != nil {
		s.fail(w, "callback exchange", err)
		return
	}
	id, err := s.provider.FetchIdentity(ctx, token)
	if err != nil {
		s.fail(w, "callback identity", err)
		return
	}

	account, found, err := s.store.FindIdentity(ctx, s.provider.Name, id.ProviderUID)
	if err != nil {
		s.fail(w, "callback find", err)
		return
	}
	if !found {
		account, err = s.store.CreateAccountWithIdentity(ctx, s.provider.Name, id.ProviderUID, id.Email, id.Login)
		if err != nil {
			s.fail(w, "callback create", err)
			return
		}
		s.log.Info("new account created via oauth", "provider", s.provider.Name, "login", id.Login)
	}
	s.setSession(w, account)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	account := s.sessionAccount(r)
	if account == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	name, _, _ := s.store.AccountDisplayName(ctx, account)
	chars, err := s.store.AccountCharacters(ctx, account)
	if err != nil {
		s.fail(w, "dashboard", err)
		return
	}
	s.render(w, "dashboard", map[string]any{"Name": name, "Characters": chars})
}

// handlePlay mints a single-use link code for the account and shows the connect instructions.
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	account := s.sessionAccount(r)
	if account == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	code, err := s.codes.Mint(ctx, account, r.FormValue("character_id"), s.linkCodeTTL)
	if err != nil {
		s.fail(w, "play", err)
		return
	}
	s.render(w, "play", map[string]any{"Code": code, "GateHint": s.gateHint, "TTLMin": int(s.linkCodeTTL.Minutes())})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// render executes the named template into a full page.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("template render", "name", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, where string, err error) {
	s.log.Error("web handler", "where", where, "err", err)
	http.Error(w, "something went wrong; please try again", http.StatusInternalServerError)
}
