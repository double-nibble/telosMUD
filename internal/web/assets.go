package web

import (
	"embed"
	"net/http"
)

// assets.go — the website's static files (the TelosMUD logo), embedded into the binary so telos-account ships
// self-contained (no sidecar files to deploy). Served read-only under /assets/ by the route mux (server.go).

// Embed the prod + dev + staging logo variants by EXACT filename (not assets/*, so a stray file can't slip
// into the public /assets/ tree — security audit F7). The site picks the variant by env (New): the -dev /
// -staging badge renders on non-prod so an operator can tell a dev or staging instance from prod at a glance.
//
//go:embed assets/telosmud-logo.svg assets/telosmud-logo.png assets/telosmud-logo-dev.svg assets/telosmud-logo-dev.png assets/telosmud-logo-staging.svg assets/telosmud-logo-staging.png
var assetsFS embed.FS

// assetsHandler serves the embedded static files under /assets/ (e.g. /assets/telosmud-logo.svg).
func assetsHandler() http.Handler {
	return http.FileServerFS(assetsFS)
}
