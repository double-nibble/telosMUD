//go:build telos_devauth

package gate

// This file is compiled ONLY into a dev/test build (`go build -tags telos_devauth`). It carries the real
// TELOS_DEV_AUTOAUTH bypass — the bare-name login that replaces the browser OAuth flow so headless smoke/e2e
// and local dev work without a browser. #96: the bypass is PHYSICALLY ABSENT from a default (release) build,
// where devauth_release.go compiles a hard-refuse stub in its place, so no environment / config / compromised
// orchestrator can enable it in production. CI builds the smoke/e2e/botswarm images with `-tags telos_devauth`
// (deploy/Dockerfile BUILD_TAGS, deploy/docker-compose.yml gate service); the release build omits the tag.

// WithDevAutoAuth enables the Phase-15.6 dev/test bypass on a dev-tagged build: an account-backed gate accepts
// the bare name login instead of the browser OAuth flow. INSECURE — the ListenAndServe bind guard keeps it
// loopback-only; never enable it on a reachable host. Returns the Server for chaining. In a release build the
// same call is a hard-refuse no-op (devauth_release.go).
func (s *Server) WithDevAutoAuth(on bool) *Server {
	s.devAutoAuth = on
	return s
}

// devAuthActive reports whether the dev-autoauth bypass is engaged for this Server. On a dev-tagged build it
// tracks the WithDevAutoAuth setting; on a release build the bypass is never enabled.
func (s *Server) devAuthActive() bool {
	return s.devAutoAuth
}
