package content

import (
	"context"
	"fmt"
	"log/slog"
)

// Source is where content comes from. Two implementations exist:
//
//   - the embedded YAML demo pack (EmbeddedSource, demo.go) — used by unit tests and a
//     bare dev run, so the test suite needs NO live Postgres;
//   - Postgres (internal/store implements this) — the production path.
//
// Both return the same neutral []Pack, filtered to the enabled pack names, so the loader
// and the world-side mapper are source-agnostic.
type Source interface {
	// LoadPacks returns the definition data for exactly the named packs (each as one Pack).
	// An enabled pack with no rows yields an empty/absent Pack; unknown names are skipped.
	// A nil/empty enabled list means "load nothing" (bare-engine boot).
	LoadPacks(ctx context.Context, enabled []string) ([]Pack, error)
}

// LoadedContent is the result of a load: the zones to build, keyed by zone ref, in a
// neutral form the world package consumes (it builds prototypes and runs resets from this).
// It carries no world types, so this package stays free of an import cycle.
type LoadedContent struct {
	// Zones is every loaded zone, in stable load order (pack order, then file order) so a
	// build is deterministic.
	Zones []ZoneDTO
	// byRef indexes Zones by ref for O(1) lookup.
	byRef map[string]*ZoneDTO
}

// Zone returns the loaded zone with the given ref, or nil.
func (lc *LoadedContent) Zone(ref string) *ZoneDTO {
	if lc == nil {
		return nil
	}
	return lc.byRef[ref]
}

// Empty reports whether nothing was loaded (the bare-engine boot case).
func (lc *LoadedContent) Empty() bool { return lc == nil || len(lc.Zones) == 0 }

// Load reads the enabled packs from src and returns the assembled LoadedContent. It is the
// boot-time content read; it runs synchronously on the shard-construction goroutine (never
// on a zone goroutine), so blocking I/O here is fine (docs/PHASE4-PLAN.md §3). With no
// enabled packs or an unreachable source returning nothing, it returns an empty
// LoadedContent — the bare-engine invariant: the engine boots with zero content.
func Load(ctx context.Context, src Source, enabled []string) (*LoadedContent, error) {
	lc := &LoadedContent{byRef: map[string]*ZoneDTO{}}
	if src == nil || len(enabled) == 0 {
		slog.Debug("content load: no source or no enabled packs; booting empty",
			"enabled", enabled)
		return lc, nil
	}
	packs, err := src.LoadPacks(ctx, enabled)
	if err != nil {
		return nil, fmt.Errorf("content: load packs %v: %w", enabled, err)
	}
	// Materialize the deduped zone set: a later pack shipping the same zone ref overrides the
	// earlier one IN PLACE (last write wins; content-lint catches accidental collisions).
	// Track positions by index, NOT by pointer — appending to lc.Zones reallocates the backing
	// array, so any &lc.Zones[i] taken here would dangle.
	idxByRef := make(map[string]int)
	for _, p := range packs {
		for i := range p.Zones {
			z := p.Zones[i]
			if idx, ok := idxByRef[z.Ref]; ok {
				slog.Debug("content load: zone overridden by later pack", "zone", z.Ref, "pack", p.Pack)
				lc.Zones[idx] = z
			} else {
				idxByRef[z.Ref] = len(lc.Zones)
				lc.Zones = append(lc.Zones, z)
			}
		}
	}
	// Build byRef and the counts from the FINAL zone set (the backing array no longer grows).
	var rooms, protos, resets int
	lc.byRef = make(map[string]*ZoneDTO, len(lc.Zones))
	for i := range lc.Zones {
		z := &lc.Zones[i]
		lc.byRef[z.Ref] = z
		rooms += len(z.Rooms)
		protos += len(z.Items) + len(z.Mobs)
		resets += len(z.Resets)
	}
	slog.Debug("content loaded", "packs", enabled, "zones", len(lc.Zones),
		"rooms", rooms, "prototypes", protos, "resets", resets)
	return lc, nil
}
