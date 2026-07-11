//go:build nofixture

package content

import "embed"

// embed_release.go — the RELEASE content embed (build tag `nofixture`): ONLY packs/core, the minimal
// bootstrap pack a fresh/empty shard needs to boot a login lobby before real content is pulled. The `demo`
// test fixture is deliberately NOT embedded, so a shipped GHCR image does not carry it — the deployable
// world comes from the external content store (telosMUD-content) via telos-pull into Postgres. A
// LoadPack("demo") / EmbeddedSource for "demo" simply finds nothing here (found=false), which is the
// intended bare-engine behaviour. The gate/world/account/director/migrate/pull release images build with
// this tag; telos-seed (the demo-seeder tool) keeps the full embed.
//
//go:embed packs/core
var packFS embed.FS
