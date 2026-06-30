package web

import (
	"embed"
	"net/http"
)

// assets.go — the website's static files (the TelosMUD logo), embedded into the binary so telos-account ships
// self-contained (no sidecar files to deploy). Served read-only under /assets/ by the route mux (server.go).

// Embed only the shipped (production) logo — the assets/ dir also holds *-dev variants that should NOT be
// publicly reachable (security audit F7), so the glob is the exact filenames, not assets/*.
//
//go:embed assets/telosmud-logo.svg assets/telosmud-logo.png
var assetsFS embed.FS

// assetsHandler serves the embedded static files under /assets/ (e.g. /assets/telosmud-logo.svg).
func assetsHandler() http.Handler {
	return http.FileServerFS(assetsFS)
}
