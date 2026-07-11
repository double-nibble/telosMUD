package content

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// packtree.go implements the DIRECTORY-TREE pack layout (#53). A pack may be authored as EITHER:
//
//   - a single file  packs/<name>.yaml         (the classic layout — unchanged), OR
//   - a directory    packs/<name>/**/*.yaml     (a TREE of small files split by concern:
//     a pack.yaml manifest + attributes.yaml + abilities/*.yaml + zones/<z>/rooms/*.yaml …).
//
// The tree is MERGED into one Pack before it reaches the boot loader (Load) or the store importer,
// so everything downstream stays source-shape-agnostic — it only ever sees a Pack. The merge is
// the across-FILES extension of Load's across-PACKS rule: files are read in sorted path order and
// combined last-write-wins by ref. The single-file form is a tree of exactly one file, so both
// layouts flow through the identical code.
//
// Feeding the merged Pack through Load yields the SAME LoadedContent as if each file had been a
// separate pack — with ONE deliberate exception: a ZONE that appears in several files has its
// children UNIONED (rooms/mobs/items merged by ref, resets concatenated) rather than the whole
// zone replaced. That union is the entire point of the tree: it lets `zones/town/rooms/*.yaml`
// split one zone's rooms across many files.

// loadPackFS reads pack `name` from fsys, resolving the single-file OR directory-tree layout and
// returning the fully merged Pack. found=false (with no error) means neither packs/<name>.yaml nor
// the directory packs/<name>/ exists — the caller skips it, preserving the invariant that enabling
// a pack the source does not carry contributes nothing (the bare-engine boot).
//
// If BOTH a packs/<name>.yaml file and a packs/<name>/ directory exist, the single file wins and the
// directory is ignored — a pack is authored in one shape or the other, not both.
func loadPackFS(fsys fs.FS, name string) (pack Pack, found bool, err error) {
	// Single-file form first (the classic layout): packs/<name>.yaml.
	if data, rerr := fs.ReadFile(fsys, path.Join("packs", name+".yaml")); rerr == nil {
		p, perr := ParsePack(data)
		if perr != nil {
			return Pack{}, false, fmt.Errorf("content: parse pack %q: %w", name, perr)
		}
		if p.Pack == "" {
			p.Pack = name
		}
		return p, true, nil
	}

	// Directory-tree form: packs/<name>/**/*.yaml.
	dir := path.Join("packs", name)
	info, serr := fs.Stat(fsys, dir)
	if serr != nil || !info.IsDir() {
		return Pack{}, false, nil // neither a file nor a directory: nothing to contribute
	}

	files, err := collectPackFiles(fsys, dir)
	if err != nil {
		return Pack{}, false, err
	}
	parts := make([]Pack, 0, len(files))
	for _, f := range files {
		data, rerr := fs.ReadFile(fsys, f)
		if rerr != nil {
			return Pack{}, false, fmt.Errorf("content: read pack file %q: %w", f, rerr)
		}
		part, perr := ParsePack(data)
		if perr != nil {
			return Pack{}, false, fmt.Errorf("content: parse pack file %q: %w", f, perr)
		}
		parts = append(parts, part)
	}
	merged := mergePacks(parts)
	if merged.Pack == "" {
		merged.Pack = name
	}
	return merged, true, nil
}

// collectPackFiles walks dir and returns every .yaml/.yml file path in sorted (deterministic)
// order, so the merge is stable regardless of the filesystem's directory-entry order.
func collectPackFiles(fsys fs.FS, dir string) ([]string, error) {
	var files []string
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("content: walk pack dir %q: %w", dir, err)
	}
	sort.Strings(files)
	return files, nil
}

// mergePacks folds the per-file Packs (in the caller's order) into one Pack. Pack globals accumulate
// last-write-wins by ref (name for trust tiers) — the same rule Load applies across packs, so the
// merged Pack is interchangeable with the separate-packs form. Command/DisplayDef lists are appended
// (not ref-deduped) because Load appends them too and the world-side registry resolves the last-write
// -wins there. Zones are UNIONED by ref (mergeZone) rather than replaced. Scalars take the last
// non-empty value; Formulas is a plain map merge.
func mergePacks(parts []Pack) Pack {
	var out Pack
	for i := range parts {
		p := parts[i]
		if out.Pack == "" {
			out.Pack = p.Pack
		}
		if p.DefaultCombat != "" {
			out.DefaultCombat = p.DefaultCombat
		}
		if p.PvpLua != "" {
			out.PvpLua = p.PvpLua
		}
		if p.WorldScript != "" {
			out.WorldScript = p.WorldScript // #47: last non-empty file/pack wins
		}
		for name, body := range p.Formulas {
			if out.Formulas == nil {
				out.Formulas = map[string]string{}
			}
			out.Formulas[name] = body
		}

		// Append-only lists (world registry dedups) — mirrors Load.
		out.Commands = append(out.Commands, p.Commands...)
		out.DisplayDefs = append(out.DisplayDefs, p.DisplayDefs...)

		// Zones: union children by ref rather than replace the whole zone.
		out.Zones = mergeZones(out.Zones, p.Zones)

		// Pack globals: last-write-wins by ref (name for trust tiers).
		out.Attributes = mergeByKey(out.Attributes, p.Attributes, func(a AttributeDTO) string { return a.Ref })
		out.Resources = mergeByKey(out.Resources, p.Resources, func(r ResourceDTO) string { return r.Ref })
		out.DamageTypes = mergeByKey(out.DamageTypes, p.DamageTypes, func(d DamageTypeDTO) string { return d.Ref })
		out.Affects = mergeByKey(out.Affects, p.Affects, func(a AffectDTO) string { return a.Ref })
		out.Abilities = mergeByKey(out.Abilities, p.Abilities, func(a AbilityDTO) string { return a.Ref })
		out.CombatProfiles = mergeByKey(out.CombatProfiles, p.CombatProfiles, func(c CombatProfileDTO) string { return c.Ref })
		out.Channels = mergeByKey(out.Channels, p.Channels, func(c ChannelDTO) string { return c.Ref })
		out.ToggleDefs = mergeByKey(out.ToggleDefs, p.ToggleDefs, func(t ToggleDTO) string { return t.Ref })
		out.Regions = mergeByKey(out.Regions, p.Regions, func(r RegionDTO) string { return r.Ref })
		out.Tracks = mergeByKey(out.Tracks, p.Tracks, func(t TrackDTO) string { return t.Ref })
		out.Bundles = mergeByKey(out.Bundles, p.Bundles, func(b BundleDTO) string { return b.Ref })
		out.RarityTiers = mergeByKey(out.RarityTiers, p.RarityTiers, func(r RarityTierDTO) string { return r.Ref })
		out.Affixes = mergeByKey(out.Affixes, p.Affixes, func(a AffixDefDTO) string { return a.Ref })
		out.LootTables = mergeByKey(out.LootTables, p.LootTables, func(l LootTableDTO) string { return l.Ref })
		out.SpawnSchedules = mergeByKey(out.SpawnSchedules, p.SpawnSchedules, func(s SpawnScheduleDTO) string { return s.Ref })
		out.Recipes = mergeByKey(out.Recipes, p.Recipes, func(r RecipeDTO) string { return r.Ref })
		out.HelpDefs = mergeByKey(out.HelpDefs, p.HelpDefs, func(h HelpDTO) string { return h.Ref })
		out.WearSlots = mergeByKey(out.WearSlots, p.WearSlots, func(w WearSlotDTO) string { return w.Ref })
		out.Chargens = mergeByKey(out.Chargens, p.Chargens, func(c ChargenDTO) string { return c.Ref })
		out.TrustTiers = mergeByKey(out.TrustTiers, p.TrustTiers, func(t TrustTierDTO) string { return t.Name })
	}
	return out
}

// mergeZones folds src zones into dst, unioning by zone ref: a zone already present has its children
// merged (mergeZone); a new zone is appended. Positional index tracking mirrors Load — the ref->index
// map avoids taking a &dst[i] that a later append would dangle.
func mergeZones(dst, src []ZoneDTO) []ZoneDTO {
	idx := make(map[string]int, len(dst))
	for i := range dst {
		idx[dst[i].Ref] = i
	}
	for i := range src {
		z := src[i]
		if j, ok := idx[z.Ref]; ok {
			dst[j] = mergeZone(dst[j], z)
		} else {
			idx[z.Ref] = len(dst)
			dst = append(dst, z)
		}
	}
	return dst
}

// mergeZone unions one zone's children across files: rooms/items/mobs are merged last-write-wins by
// ref (so `zones/town/rooms/plaza.yaml` and `.../market.yaml` combine into one zone), resets are
// concatenated (a reset op has no ref — order is its identity), and the scalar fields take the last
// non-empty/non-zero value. dst.Ref is preserved (it is the merge key).
func mergeZone(dst, src ZoneDTO) ZoneDTO {
	if src.Name != "" {
		dst.Name = src.Name
	}
	if src.StartRoom != "" {
		dst.StartRoom = src.StartRoom
	}
	if src.ResetSecs != 0 {
		dst.ResetSecs = src.ResetSecs
	}
	dst.Rooms = mergeByKey(dst.Rooms, src.Rooms, func(r RoomDTO) string { return r.Ref })
	dst.Items = mergeByKey(dst.Items, src.Items, func(p ProtoDTO) string { return p.Ref })
	dst.Mobs = mergeByKey(dst.Mobs, src.Mobs, func(p ProtoDTO) string { return p.Ref })
	dst.Resets = append(dst.Resets, src.Resets...)
	return dst
}

// mergeByKey appends src onto dst with last-write-wins by key(v): a value whose key already appears
// in dst replaces it in place; a new key is appended. Order is dst's existing order, then first
// appearance of each new key — deterministic given the caller's sorted file order.
func mergeByKey[T any](dst, src []T, key func(T) string) []T {
	if len(src) == 0 {
		return dst
	}
	idx := make(map[string]int, len(dst)+len(src))
	for i := range dst {
		idx[key(dst[i])] = i
	}
	for i := range src {
		k := key(src[i])
		if j, ok := idx[k]; ok {
			dst[j] = src[i]
		} else {
			idx[k] = len(dst)
			dst = append(dst, src[i])
		}
	}
	return dst
}
