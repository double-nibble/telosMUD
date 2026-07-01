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

// session.go — the broker's ONE signed cookie (Phase 15): the short-lived OAUTH-FLOW state. A brokered device
// login is one-shot — there is no persistent logged-in web session — so the only cookie carries the CSRF
// `state` + the PKCE verifier + the `device_code` being authorized across the /login -> provider -> callback
// hop. Tamper-evident (payload + HMAC-SHA256), single-use (cleared on read).

const (
	flowCookie     = "telos_oauth"
	flowCookieHost = "__Host-telos_oauth" // the __Host- prefixed name used under TLS (see flowCookieName)
	flowTTL        = 10 * time.Minute
)

// flowCookieName is the OAuth-flow cookie name. Under TLS it carries the `__Host-` prefix, which browsers
// enforce as Secure + Path=/ + no Domain — so the cookie cannot be planted over plain http or set from a
// sibling subdomain (docs/REMAINING.md §1, audit F4). The prefix REQUIRES Secure, so in dev over plain http
// (s.secureCookies == false, where Secure is off) we fall back to the unprefixed name — otherwise the
// browser would silently reject the Set-Cookie and the login would break on localhost.
func (s *Server) flowCookieName() string {
	if s.secureCookies {
		return flowCookieHost
	}
	return flowCookie
}

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
	if !hmac.Equal(sig, mac.Sum(nil)) { // constant-time
		return nil, false
	}
	return payload, true
}

// flowData is the OAuth-flow cookie payload: the CSRF state, the PKCE verifier, and the device_code this login
// authorizes (so the callback knows which waiting telnet session to mark authed).
type flowData struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Device   string `json:"device"`
	Exp      int64  `json:"exp"`
}

// setFlow stashes the OAuth CSRF state + PKCE verifier + device_code in a short-lived signed cookie.
func (s *Server) setFlow(w http.ResponseWriter, state, verifier, deviceCode string) {
	payload, _ := json.Marshal(flowData{State: state, Verifier: verifier, Device: deviceCode, Exp: time.Now().Add(flowTTL).Unix()})
	http.SetCookie(w, s.cookie(s.flowCookieName(), s.sign.sign(payload), int(flowTTL.Seconds())))
}

// takeFlow reads + clears the OAuth-flow cookie, returning its state + verifier + device_code (ok=false if
// missing/expired/tampered).
func (s *Server) takeFlow(w http.ResponseWriter, r *http.Request) (state, verifier, deviceCode string, ok bool) {
	c, err := r.Cookie(s.flowCookieName())
	if err != nil {
		return "", "", "", false
	}
	http.SetCookie(w, s.cookie(s.flowCookieName(), "", -1)) // single-use: clear it
	payload, valid := s.sign.verify(c.Value)
	if !valid {
		return "", "", "", false
	}
	var d flowData
	if json.Unmarshal(payload, &d) != nil || time.Now().Unix() >= d.Exp {
		return "", "", "", false
	}
	return d.State, d.Verifier, d.Device, true
}

// cookie builds a hardened cookie (HttpOnly, SameSite=Lax, Path=/). Secure is set when the site is served over
// TLS (s.secureCookies); in dev over plain http it is off so the cookie still works on localhost.
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
