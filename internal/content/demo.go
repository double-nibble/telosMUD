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

//go:embed packs/*.yaml
var packFS embed.FS

// EmbeddedSource serves content packs compiled into the binary from packs/*.yaml. It is the
// Source unit tests and a bare dev run use, so the test suite has NO Postgres dependency.
// Production loads from internal/store (Postgres) instead; `make seed` imports these same
// YAML files into the DB so the two sources agree.
type EmbeddedSource struct{}

// LoadPacks parses the embedded YAML for each enabled pack name (packs/<name>.yaml) and
// returns them. A name with no embedded file is skipped (not an error): enabling a pack the
// binary doesn't carry just contributes nothing, preserving the bare-engine boot.
func (EmbeddedSource) LoadPacks(_ context.Context, enabled []string) ([]Pack, error) {
	var out []Pack
	for _, name := range enabled {
		data, err := packFS.ReadFile("packs/" + name + ".yaml")
		if err != nil {
			// Not embedded: nothing to contribute. (A Postgres source would 404 the same way.)
			continue
		}
		var p Pack
		if err := yaml.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("content: parse embedded pack %q: %w", name, err)
		}
		if p.Pack == "" {
			p.Pack = name
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

// DemoPackBytes returns the raw embedded demo YAML, for `make seed` / store import to push
// into Postgres without re-reading the file from disk.
func DemoPackBytes() ([]byte, error) { return packFS.ReadFile("packs/" + DemoPack + ".yaml") }

// ParsePack parses one pack's YAML bytes (used by the store importer for `make seed`).
func ParsePack(data []byte) (Pack, error) {
	var p Pack
	if err := yaml.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("content: parse pack: %w", err)
	}
	return p, nil
}
