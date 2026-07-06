package content

import (
	"context"
	"embed"
	"fmt"

	"gopkg.in/yaml.v3"
)

// DemoPack is the name of the demo content pack. It is deliberately NOT "stdlib": the demo
// world is kept visibly separate from a future curated standard library, so the bare-engine
// strip story (DELETE WHERE pack=...) and the demo are independent.
const DemoPack = "demo"

// The embed covers the whole packs/ tree, so a pack may be a single packs/<name>.yaml file OR a
// directory packs/<name>/**/*.yaml (the #53 tree layout). `all:` keeps files the default embed
// would skip (those beginning with `.`/`_`) out of scope only where intended; here the plain form
// is enough — we only read *.yaml/*.yml.
//
//go:embed packs
var packFS embed.FS

// EmbeddedSource serves content packs compiled into the binary from the packs/ tree. It is the
// Source unit tests and a bare dev run use, so the test suite has NO Postgres dependency.
// Production loads from internal/store (Postgres) instead; `make seed` imports these same
// YAML files into the DB so the two sources agree.
type EmbeddedSource struct{}

// LoadPacks parses the embedded content for each enabled pack name and returns them. Each pack is
// resolved as either the single-file layout (packs/<name>.yaml) or the directory-tree layout
// (packs/<name>/**/*.yaml), merged into one Pack (loadPackFS). A name with no embedded file OR
// directory is skipped (not an error): enabling a pack the binary doesn't carry just contributes
// nothing, preserving the bare-engine boot.
func (EmbeddedSource) LoadPacks(_ context.Context, enabled []string) ([]Pack, error) {
	var out []Pack
	for _, name := range enabled {
		p, found, err := loadPackFS(packFS, name)
		if err != nil {
			return nil, err
		}
		if !found {
			continue // not embedded: nothing to contribute (a Postgres source would 404 the same way)
		}
		out = append(out, p)
	}
	return out, nil
}

// LoadDemoPack is a convenience for tests and the parity check: load the embedded demo pack
// straight into a LoadedContent.
func LoadDemoPack() (*LoadedContent, error) {
	return Load(context.Background(), EmbeddedSource{}, []string{DemoPack})
}

// LoadPack resolves and merges one embedded pack by name (single-file OR directory-tree layout)
// into a single Pack, for `make seed` / store import to push into Postgres. It is the tree-aware
// replacement for the old raw-bytes read: a pack authored as a directory is merged here, so the
// importer always receives one complete Pack regardless of on-disk shape. found=false means the
// name is not embedded.
func LoadPack(name string) (pack Pack, found bool, err error) {
	return loadPackFS(packFS, name)
}

// ParsePack parses one pack's YAML bytes (used by the store importer for `make seed`).
func ParsePack(data []byte) (Pack, error) {
	var p Pack
	if err := yaml.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("content: parse pack: %w", err)
	}
	return p, nil
}
