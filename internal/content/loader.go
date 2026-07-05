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
	// Commands are the pack-global custom Lua verbs (Phase 7.4e), accumulated across packs (last-write
	// -wins by verb). The world side registers them into the per-shard custom-command table.
	Commands []CommandDTO
	// DisplayDefs are the pack-global display templates (Lua render body per surface), accumulated across
	// packs (last-write-wins by surface). The world side registers them into the per-shard display table.
	DisplayDefs []DisplayDefDTO
	// Channels are the pack-global comms channel definitions (Phase 8.3), same last-write-wins
	// override rule keyed by ref. The world side registers them into the per-shard channel registry
	// and binds each channel's verb(s) into the per-shard channel-command table. An empty list => no
	// channels => no channel verbs (the empty-boot invariant).
	Channels []ChannelDTO
	// Regions are the pack-global region definitions (Phase 10.3), same last-write-wins override rule
	// keyed by ref. A region groups member zones a director owns the supra-zone state of; an empty list
	// => no regions (only the world scope). The director/zone wiring consumes these in 10.3b/c.
	Regions []RegionDTO
	// Tracks are the pack-global advancement tracks (Phase 11.2), same last-write-wins override rule keyed
	// by ref. The world side parses each step's grant op-list and registers them into the per-shard track
	// registry; an empty list => no tracks (the empty-boot invariant).
	Tracks []TrackDTO
	// Bundles are the pack-global class/race/feat/… bundles (Phase 11.4b), same last-write-wins override
	// rule keyed by ref. The world side parses each bundle's grant op-list and registers them into the
	// per-shard bundle registry; apply_bundle runs one's grants on an entity.
	Bundles []BundleDTO
	// RarityTiers + LootTables are the pack-global loot definitions (Phase 12.1), same last-write-wins
	// override rule keyed by ref. The world side registers them into the per-shard loot registries; the
	// resolver runs a loot table per eligible looter on death.
	RarityTiers    []RarityTierDTO
	LootTables     []LootTableDTO
	SpawnSchedules []SpawnScheduleDTO
	// Recipes are the pack-global crafting recipes (Phase 13.5), same last-write-wins by ref. The world side
	// registers them into the per-shard recipe registry; the craft op reads one by ref.
	Recipes []RecipeDTO
	// WearSlots is the pack-global content-defined equipment vocabulary (#35), accumulated last-write-wins by
	// slot ref. The world builds its runtime wear-slot vocab from it; an empty list => the engine default set.
	WearSlots []WearSlotDTO
	// Chargens are the pack-global character-generation flows (Phase 14.8), same last-write-wins by ref.
	// telos-account reads them (not the world) to render + validate the signup form.
	Chargens []ChargenDTO
	// TrustTiers is the pack-global content-defined trust ladder (#27/#29, Round 9 Slice 0), accumulated
	// last-write-wins by tier NAME. BOTH the world (rank + flag derivation, command gating) AND telos-account
	// (tier validation + promote authz) read it. An empty list => the engine default ladder.
	TrustTiers []TrustTierDTO
	// PvpLua is the pack PvP-policy Lua hook (Phase 7.4f); the LAST non-empty pack value wins. Empty =>
	// the engine's built-in pvp_allowed. Formulas are the Lua ruleset-formula overrides (last-write-wins
	// by name).
	PvpLua   string
	Formulas map[string]string
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
	chanIdx := make(map[string]int)
	regIdx := make(map[string]int)
	trackIdx := make(map[string]int)
	bundleIdx := make(map[string]int)
	rarityIdx := make(map[string]int)
	lootIdx := make(map[string]int)
	schedIdx := make(map[string]int)
	recipeIdx := make(map[string]int)
	wearSlotIdx := make(map[string]int)
	chargenIdx := make(map[string]int)
	trustIdx := make(map[string]int)
	for _, p := range packs {
		if p.DefaultCombat != "" {
			lc.DefaultCombat = p.DefaultCombat // last non-empty pack wins
		}
		if p.PvpLua != "" {
			lc.PvpLua = p.PvpLua // last non-empty pack wins (7.4f)
		}
		for name, body := range p.Formulas { // 7.4f: last-write-wins by name
			if lc.Formulas == nil {
				lc.Formulas = map[string]string{}
			}
			lc.Formulas[name] = body
		}
		lc.Commands = append(lc.Commands, p.Commands...)          // 7.4e: accumulate custom verbs
		lc.DisplayDefs = append(lc.DisplayDefs, p.DisplayDefs...) // display templates (last-write-wins by surface)
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
		for _, ch := range p.Channels {
			if idx, ok := chanIdx[ch.Ref]; ok {
				lc.Channels[idx] = ch
			} else {
				chanIdx[ch.Ref] = len(lc.Channels)
				lc.Channels = append(lc.Channels, ch)
			}
		}
		for _, rg := range p.Regions {
			if idx, ok := regIdx[rg.Ref]; ok {
				lc.Regions[idx] = rg
			} else {
				regIdx[rg.Ref] = len(lc.Regions)
				lc.Regions = append(lc.Regions, rg)
			}
		}
		for _, tr := range p.Tracks {
			if idx, ok := trackIdx[tr.Ref]; ok {
				lc.Tracks[idx] = tr
			} else {
				trackIdx[tr.Ref] = len(lc.Tracks)
				lc.Tracks = append(lc.Tracks, tr)
			}
		}
		for _, bn := range p.Bundles {
			if idx, ok := bundleIdx[bn.Ref]; ok {
				lc.Bundles[idx] = bn
			} else {
				bundleIdx[bn.Ref] = len(lc.Bundles)
				lc.Bundles = append(lc.Bundles, bn)
			}
		}
		for _, rt := range p.RarityTiers {
			if idx, ok := rarityIdx[rt.Ref]; ok {
				lc.RarityTiers[idx] = rt
			} else {
				rarityIdx[rt.Ref] = len(lc.RarityTiers)
				lc.RarityTiers = append(lc.RarityTiers, rt)
			}
		}
		for _, lt := range p.LootTables {
			if idx, ok := lootIdx[lt.Ref]; ok {
				lc.LootTables[idx] = lt
			} else {
				lootIdx[lt.Ref] = len(lc.LootTables)
				lc.LootTables = append(lc.LootTables, lt)
			}
		}
		for _, sc := range p.SpawnSchedules {
			if idx, ok := schedIdx[sc.Ref]; ok {
				lc.SpawnSchedules[idx] = sc
			} else {
				schedIdx[sc.Ref] = len(lc.SpawnSchedules)
				lc.SpawnSchedules = append(lc.SpawnSchedules, sc)
			}
		}
		for _, rc := range p.Recipes {
			if idx, ok := recipeIdx[rc.Ref]; ok {
				lc.Recipes[idx] = rc
			} else {
				recipeIdx[rc.Ref] = len(lc.Recipes)
				lc.Recipes = append(lc.Recipes, rc)
			}
		}
		for _, ws := range p.WearSlots {
			if idx, ok := wearSlotIdx[ws.Ref]; ok {
				lc.WearSlots[idx] = ws
			} else {
				wearSlotIdx[ws.Ref] = len(lc.WearSlots)
				lc.WearSlots = append(lc.WearSlots, ws)
			}
		}
		for _, cg := range p.Chargens {
			if idx, ok := chargenIdx[cg.Ref]; ok {
				lc.Chargens[idx] = cg
			} else {
				chargenIdx[cg.Ref] = len(lc.Chargens)
				lc.Chargens = append(lc.Chargens, cg)
			}
		}
		// Trust tiers (#27/#29): accumulate last-write-wins by tier NAME across packs.
		for _, tt := range p.TrustTiers {
			if idx, ok := trustIdx[tt.Name]; ok {
				lc.TrustTiers[idx] = tt
			} else {
				trustIdx[tt.Name] = len(lc.TrustTiers)
				lc.TrustTiers = append(lc.TrustTiers, tt)
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
