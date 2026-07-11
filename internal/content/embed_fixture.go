//go:build !nofixture

package content

import "embed"

// embed_fixture.go — the DEFAULT content embed: the WHOLE packs/ tree (the minimal `core` bootstrap pack
// PLUS the `demo` test fixture). This is what `go test`, `go build`, and a dev image compile in, so the
// unit tests + telos-seed + a local dev run have the demo world available. A release image is built with
// `-tags nofixture` and instead compiles embed_release.go (core only) — see that file.
//
//go:embed packs
var packFS embed.FS
