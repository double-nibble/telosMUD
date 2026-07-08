//go:build !telos_devauth

package gate

// This file is compiled into the DEFAULT (release) build — any build WITHOUT `-tags telos_devauth`. It
// replaces the TELOS_DEV_AUTOAUTH bypass with a hard-refuse stub so the bare-name login that bypasses OAuth is
// PHYSICALLY ABSENT from a production binary (#96). A runtime env / config / compromised orchestrator cannot
// re-enable it: WithDevAutoAuth never sets the flag, so devAuthActive() is always false, login() always takes
// the OAuth path, and the bypass never engages. The dev/test counterpart lives in devauth_dev.go.

import "log/slog"

// WithDevAutoAuth is a hard-refuse no-op in a release build: the bypass is not compiled in, so there is
// nothing to enable. A caller that asks for it (a stale config / env in production) gets a loud warning and
// OAuth stays enforced — s.devAutoAuth is deliberately left false. Returns the Server unchanged for chaining.
// The dev-tagged build (devauth_dev.go) carries the real setter.
func (s *Server) WithDevAutoAuth(on bool) *Server {
	if on {
		slog.Warn("TELOS_DEV_AUTOAUTH requested but this is a RELEASE build (compiled without -tags telos_devauth) — " +
			"the no-OAuth bypass is absent; OAuth stays ENFORCED. This is the intended production posture (#96).")
	}
	return s
}

// devAuthActive reports whether the dev-autoauth bypass is engaged. In a release build s.devAutoAuth is never
// set true (WithDevAutoAuth above refuses to set it), so this is always false — the bypass branch in login()
// is unreachable in practice. The dev-tagged build's setter is the only path that can make it true.
func (s *Server) devAuthActive() bool {
	return s.devAutoAuth
}
