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

	// Attributes/Resources/DamageTypes are the PACK-GLOBAL definition kinds (Phase 5.1): they
	// are zone-independent, accumulated across every loaded pack in load order. A later pack
	// shipping the same ref overrides the earlier one (last write wins), mirroring the zone
	// override rule. The world side registers them into per-shard registries (build.go).
	Attributes  []AttributeDTO
	Resources   []ResourceDTO
	DamageTypes []DamageTypeDTO
	// Affects are the pack-global status-effect definitions (Phase 5.2), same last-write-wins
	// override rule keyed by ref. The world side registers them into the per-shard affectRegistry.
	Affects []AffectDTO
	// Abilities are the pack-global ability definitions (Phase 5.3), same last-write-wins override
	// rule keyed by ref. The world side registers them into the per-shard abilityRegistry and
	// registers each command-invocation ability into the per-shard command table.
	Abilities []AbilityDTO
	// CombatProfiles are the pack-global combat profiles (Phase 6.3a), same last-write-wins override
	// rule keyed by ref. The world side parses each into a runtime combatProfile (to-hit/avoidance/
	// damage) and registers them into the per-shard combat-profile registry.
	CombatProfiles []CombatProfileDTO
	// DefaultCombat names the combat profile a player entity fights with when its own prototype
	// declares none (the pack's player default). The LAST non-empty pack value wins. Empty => players
	// have no combat profile (the degenerate auto-hit case).
	DefaultCombat string
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
	// Pack-global defs accumulate with the same last-write-wins override rule, keyed by ref.
	attrIdx := make(map[string]int)
	resIdx := make(map[string]int)
	dmgIdx := make(map[string]int)
	affIdx := make(map[string]int)
	abilIdx := make(map[string]int)
	cpIdx := make(map[string]int)
	for _, p := range packs {
		if p.DefaultCombat != "" {
			lc.DefaultCombat = p.DefaultCombat // last non-empty pack wins
		}
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
		for _, a := range p.Attributes {
			if idx, ok := attrIdx[a.Ref]; ok {
				lc.Attributes[idx] = a
			} else {
				attrIdx[a.Ref] = len(lc.Attributes)
				lc.Attributes = append(lc.Attributes, a)
			}
		}
		for _, r := range p.Resources {
			if idx, ok := resIdx[r.Ref]; ok {
				lc.Resources[idx] = r
			} else {
				resIdx[r.Ref] = len(lc.Resources)
				lc.Resources = append(lc.Resources, r)
			}
		}
		for _, d := range p.DamageTypes {
			if idx, ok := dmgIdx[d.Ref]; ok {
				lc.DamageTypes[idx] = d
			} else {
				dmgIdx[d.Ref] = len(lc.DamageTypes)
				lc.DamageTypes = append(lc.DamageTypes, d)
			}
		}
		for _, af := range p.Affects {
			if idx, ok := affIdx[af.Ref]; ok {
				lc.Affects[idx] = af
			} else {
				affIdx[af.Ref] = len(lc.Affects)
				lc.Affects = append(lc.Affects, af)
			}
		}
		for _, ab := range p.Abilities {
			if idx, ok := abilIdx[ab.Ref]; ok {
				lc.Abilities[idx] = ab
			} else {
				abilIdx[ab.Ref] = len(lc.Abilities)
				lc.Abilities = append(lc.Abilities, ab)
			}
		}
		for _, cp := range p.CombatProfiles {
			if idx, ok := cpIdx[cp.Ref]; ok {
				lc.CombatProfiles[idx] = cp
			} else {
				cpIdx[cp.Ref] = len(lc.CombatProfiles)
				lc.CombatProfiles = append(lc.CombatProfiles, cp)
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
		"rooms", rooms, "prototypes", protos, "resets", resets,
		"attributes", len(lc.Attributes), "resources", len(lc.Resources),
		"damage_types", len(lc.DamageTypes), "affects", len(lc.Affects),
		"abilities", len(lc.Abilities))
	return lc, nil
}
