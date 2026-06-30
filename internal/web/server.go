package web

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"time"
)

// server.go — the telos-account OAUTH BROKER (Phase 15). It is no longer a website: it is a bare auth bridge
// for the terminal-native device flow. The gate mints a device_code and shows the player a one-click link to
// `/login/<device_code>`; the broker runs OAuth (Authorization Code + PKCE), resolves-or-creates the account,
// and marks the device session AUTHED so the gate's poll proceeds. There is no dashboard, no form, no Play, no
// persistent session — a device login is one-shot. Narrow interfaces keep the handler tests hermetic.

// Store is the persistence surface the broker needs: just the OAuth identity resolution.
type Store interface {
	FindIdentity(ctx context.Context, provider, providerUID string) (string, bool, error)
	CreateAccountWithIdentity(ctx context.Context, provider, providerUID, email, displayName string) (string, error)
}

// DeviceAuthorizer flips a pending device session to authed once the browser completes OAuth (the account
// Service satisfies it in-process). ok=false means the device code is unknown/expired (a stale/forged link).
type DeviceAuthorizer interface {
	AuthorizeDevice(ctx context.Context, deviceCode, accountID string) (bool, error)
}

// Server is the broker. Construct with New, then Handler().
type Server struct {
	store         Store
	authorizer    DeviceAuthorizer
	provider      OAuthProvider
	sign          signer
	secureCookies bool
	tmpl          *template.Template
	log           *slog.Logger
}

// Config carries the broker's wiring.
type Config struct {
	Provider      OAuthProvider
	Authorizer    DeviceAuthorizer
	SessionKey    []byte // HMAC key for the signed flow cookie (a stable random key in prod)
	SecureCookies bool   // set Secure on the cookie (true when served over TLS)
	Dev           bool   // dev instance: render the -dev logo variant
	Log           *slog.Logger
}

// New builds the broker. It PANICS on a SessionKey shorter than 16 bytes — that key signs the OAuth-flow
// cookie (the CSRF state + PKCE verifier + device_code), so a weak key makes the flow forgeable.
func New(st Store, cfg Config) *Server {
	if len(cfg.SessionKey) < 16 {
		panic("web: SessionKey must be at least 16 bytes (it signs the OAuth flow cookie)")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	logoURL := "/assets/telosmud-logo.svg"
	if cfg.Dev {
		logoURL = "/assets/telosmud-logo-dev.svg"
	}
	tmpl := template.Must(template.New("web").
		Funcs(template.FuncMap{"logoURL": func() string { return logoURL }}).
		Parse(pageTemplates))
	return &Server{
		store:         st,
		authorizer:    cfg.Authorizer,
		provider:      cfg.Provider,
		sign:          signer{key: cfg.SessionKey},
		secureCookies: cfg.SecureCookies,
		tmpl:          tmpl,
		log:           log,
	}
}

// Handler returns the broker's HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /assets/", assetsHandler())
	mux.HandleFunc("GET /", s.handleHome)
	mux.HandleFunc("GET /login/{device}", s.handleDeviceLogin)
	mux.HandleFunc("GET /auth/github/callback", s.handleCallback)
	return mux
}

func (s *Server) handleHome(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "home", map[string]any{"Configured": s.provider.Configured()})
}

// handleDeviceLogin starts the OAuth flow for a device_code: generate the CSRF state + PKCE verifier, stash
// them + the device_code in a signed flow cookie, and redirect to the provider. The device_code is NOT
// validated here (an unknown one simply fails at the callback's AuthorizeDevice) — the broker never reveals
// whether a code is live.
func (s *Server) handleDeviceLogin(w http.ResponseWriter, r *http.Request) {
	if !s.provider.Configured() {
		s.page(w, "Sign-in is not configured on this server.")
		return
	}
	device := r.PathValue("device")
	if device == "" {
		s.page(w, "That sign-in link is missing its code. Reconnect to get a fresh link.")
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
	s.setFlow(w, state, verifier, device)
	http.Redirect(w, r, s.provider.AuthCodeURL(state, challengeFor(verifier)), http.StatusSeeOther)
}

// handleCallback completes OAuth: verify state (CSRF), exchange the code (with the PKCE verifier), fetch the
// identity, resolve-or-create the account, and mark the device session authed so the gate's poll proceeds.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	wantState, verifier, device, ok := s.takeFlow(w, r) // always burn the single-use flow cookie first
	if e := r.URL.Query().Get("error"); e != "" {
		s.page(w, "Sign-in was cancelled. Reconnect and try again.")
		return
	}
	if !ok {
		s.page(w, "The sign-in link expired. Reconnect to get a fresh one.")
		return
	}
	if r.URL.Query().Get("state") != wantState {
		s.page(w, "Invalid sign-in state.") // CSRF guard
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.page(w, "Missing authorization code.")
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

	authed, err := s.authorizer.AuthorizeDevice(ctx, device, account)
	if err != nil {
		s.fail(w, "callback authorize", err)
		return
	}
	if !authed {
		s.page(w, "This sign-in link has expired. Reconnect to get a fresh one.")
		return
	}
	s.render(w, "success", map[string]any{"Login": id.Login})
}

// render executes the named template into a full page.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("template render", "name", name, "err", err)
	}
}

// page renders a one-line message page (the broker's generic notice — expired link, cancelled, misconfig).
func (s *Server) page(w http.ResponseWriter, msg string) {
	s.render(w, "notice", map[string]any{"Message": msg})
}

func (s *Server) fail(w http.ResponseWriter, where string, err error) {
	s.log.Error("broker handler", "where", where, "err", err)
	http.Error(w, "something went wrong; please reconnect and try again", http.StatusInternalServerError)
}
