package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// session.go — HMAC-signed cookies (Phase 14.7). Two cookies: the logged-in SESSION (account id) and the
// short-lived OAUTH-FLOW state (the CSRF `state` + the PKCE verifier carried between /login and the callback).
// Both are tamper-evident (a payload + HMAC-SHA256), so the server holds no session table for the web layer.
//
// KNOWN LIMITATION (stateless sessions, security audit F2): the SESSION cookie is a self-contained bearer
// token valid until its embedded Exp. There is no server-side revocation list, so /logout only clears the
// cookie in the caller's own browser — a leaked cookie stays valid until expiry, and the only global kill
// switch is rotating the HMAC key (which drops every session). Acceptable for now; a per-account session
// "version" (bumped on sign-out-everywhere) is the fix — tracked in docs/FOLLOW-UPS.md for Phase 15.

const (
	sessionCookie = "telos_session"
	flowCookie    = "telos_oauth"
	sessionTTL    = 24 * time.Hour
	flowTTL       = 10 * time.Minute
)

// signer signs + verifies cookie payloads with an HMAC key.
type signer struct{ key []byte }

func (s signer) sign(payload []byte) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verify checks the value's signature and returns the payload bytes, or ok=false on tamper/malformed.
func (s signer) verify(value string) ([]byte, bool) {
	payloadB64, sigB64, ok := strings.Cut(value, ".")
	if !ok {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, false
	}
	return payload, true
}

type sessionData struct {
	Account string `json:"acct"`
	Exp     int64  `json:"exp"`
}

type flowData struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Exp      int64  `json:"exp"`
}

// setSession writes the logged-in session cookie for accountID.
func (s *Server) setSession(w http.ResponseWriter, accountID string) {
	payload, _ := json.Marshal(sessionData{Account: accountID, Exp: time.Now().Add(sessionTTL).Unix()})
	http.SetCookie(w, s.cookie(sessionCookie, s.sign.sign(payload), int(sessionTTL.Seconds())))
}

// sessionAccount returns the authenticated account id from the request, or "" if not signed in / expired.
func (s *Server) sessionAccount(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	payload, ok := s.sign.verify(c.Value)
	if !ok {
		return ""
	}
	var d sessionData
	if json.Unmarshal(payload, &d) != nil || time.Now().Unix() >= d.Exp {
		return ""
	}
	return d.Account
}

// clearSession expires the session cookie (logout).
func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, s.cookie(sessionCookie, "", -1))
}

// setFlow stashes the OAuth CSRF state + PKCE verifier in a short-lived signed cookie.
func (s *Server) setFlow(w http.ResponseWriter, state, verifier string) {
	payload, _ := json.Marshal(flowData{State: state, Verifier: verifier, Exp: time.Now().Add(flowTTL).Unix()})
	http.SetCookie(w, s.cookie(flowCookie, s.sign.sign(payload), int(flowTTL.Seconds())))
}

// takeFlow reads + clears the OAuth-flow cookie, returning its state + verifier (ok=false if missing/expired).
func (s *Server) takeFlow(w http.ResponseWriter, r *http.Request) (state, verifier string, ok bool) {
	c, err := r.Cookie(flowCookie)
	if err != nil {
		return "", "", false
	}
	http.SetCookie(w, s.cookie(flowCookie, "", -1)) // single-use: clear it
	payload, valid := s.sign.verify(c.Value)
	if !valid {
		return "", "", false
	}
	var d flowData
	if json.Unmarshal(payload, &d) != nil || time.Now().Unix() >= d.Exp {
		return "", "", false
	}
	return d.State, d.Verifier, true
}

// cookie builds a hardened cookie (HttpOnly, SameSite=Lax, Path=/). Secure is set when the site is served
// over TLS (s.secureCookies); in dev over plain http it is off so the cookie still works on localhost.
func (s *Server) cookie(name, value string, maxAge int) *http.Cookie {
	//nolint:gosec // G124: Secure is intentionally conditional (s.secureCookies) so dev over plain http works; prod sets it on.
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.secureCookies,
	}
}
