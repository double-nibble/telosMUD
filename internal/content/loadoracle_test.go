package content

import (
	"context"
	"fmt"
	"log/slog"
)

// loadoracle_test.go — the INDEPENDENT ORACLE for the #423 read/merge split.
//
// #423 cut content.Load into LoadPacks + LintPacks + Merge so the snapshot refresh could validate packs
// between the read and the merge. The risk in that refactor is this repo's recurring failure mode: a
// definition FIELD or a whole KIND silently dropping out of a merge/store path with a green suite.
//
// The first attempt at guarding it compared Load against Merge. That test was TAUTOLOGICAL — after the
// refactor Load *is* LintPacks+Merge, so both sides of the comparison ran the same code, and inserting
// `packs = nil` at the top of Merge still passed. It is recorded here because the mistake is a subtle one
// and the shape of the fix is the lesson: an equivalence test needs an oracle the change cannot move.
//
// loadOLD below is the pre-refactor content.Load, copied VERBATIM from the commit before the split
// (`git show 409a1b9:internal/content/loader.go`). It is frozen: it must never be "kept in sync" with the
// production loader, because being independent of it is the entire point. If a legitimate change to the
// merge rules makes these tests fail, update loadOLD in the SAME commit as the behavior change and say so
// in the message — that turns a silent field-drop into a deliberate, reviewed edit.
func loadOLD(ctx context.Context, src Source, enabled []string) (*LoadedContent, error) {
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
	// Content-lint (#212): warn on any NON-core pack shipping a world-ref under the reserved core:
	// namespace, which would clobber the embedded bootstrap room via the last-write-wins merge below.
	// Non-fatal (the merge still runs), mirroring the world content-lints.
	for _, v := range LintReservedCoreRefs(packs) {
		slog.Warn("content: pack ships a ref under the reserved core: namespace; it will clobber the "+
			"embedded bootstrap pack — rename it out of the core: prefix",
			"pack", v.Pack, "kind", v.Kind, "ref", v.Ref)
	}
	// Content-lint (#66): warn on any identity token (ref/verb/surface) with a character outside the safe
	// charset — the GMCP strip, comms-subject routing, and targeting tokenizer all assume refs can't carry
	// braces/dots/whitespace/controls. Non-fatal at boot (the reload gate hard-rejects it).
	for _, v := range LintRefCharset(packs) {
		slog.Warn("content: identity token has characters outside its safe charset; "+
			"it can break GMCP keys / comms subjects / the tokenizer — rename it",
			"pack", v.Pack, "field", v.Field, "value", v.Value, "allowed", v.Charset)
	}
	// Content-lint (#111): trust-ladder footguns — a baseline tier granting a capability (elevates the whole
	// playerbase), duplicate/nameless rungs, un-grantable flags. Non-fatal at boot (the reload gate hard-rejects
	// the Reject-severity ones), but logged at Error so a REJECT can never scroll past as routine noise.
	for _, v := range LintTrustLadder(packs) {
		msg := "content: trust-ladder lint"
		attrs := []any{"pack", v.Pack, "tier", v.Tier, "severity", v.Severity.String(), "detail", v.Detail}
		if v.Severity == TrustLadderReject {
			slog.Error(msg+" REJECT — this ladder will be refused by a fleet reload; fix it before it elevates the wrong accounts", attrs...)
			continue
		}
		slog.Warn(msg, attrs...)
	}
	// Content-lint (#60): a comms channel's access/hear_access predicate that LOOKS restrictive but resolves
	// to open because a condition was left blank (`require_flag:` present-but-null, a half-specified min_attr).
	// Non-fatal (the channel is trusted content and the engine boots to the open shape) — warned so a builder's
	// typo doesn't silently leave a restricted channel world-readable.
	for _, v := range LintChannelAccess(packs) {
		slog.Warn("content: channel access-condition lint — a present-but-empty/partial condition (likely a typo); "+
			"the channel may be unintentionally open or unreachable",
			"pack", v.Pack, "channel", v.Channel, "field", v.Field, "detail", v.Detail)
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
	toggleIdx := make(map[string]int)
	regIdx := make(map[string]int)
	trackIdx := make(map[string]int)
	bundleIdx := make(map[string]int)
	rarityIdx := make(map[string]int)
	affixIdx := make(map[string]int)
	lootIdx := make(map[string]int)
	schedIdx := make(map[string]int)
	recipeIdx := make(map[string]int)
	helpIdx := make(map[string]int)
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
		if p.WorldScript != "" {
			lc.WorldScript = p.WorldScript // last non-empty pack wins (#47: one world orchestrator)
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
		for _, tg := range p.ToggleDefs {
			if idx, ok := toggleIdx[tg.Ref]; ok {
				lc.ToggleDefs[idx] = tg
			} else {
				toggleIdx[tg.Ref] = len(lc.ToggleDefs)
				lc.ToggleDefs = append(lc.ToggleDefs, tg)
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
		for _, af := range p.Affixes {
			if idx, ok := affixIdx[af.Ref]; ok {
				lc.Affixes[idx] = af
			} else {
				affixIdx[af.Ref] = len(lc.Affixes)
				lc.Affixes = append(lc.Affixes, af)
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
		for _, hd := range p.HelpDefs {
			if idx, ok := helpIdx[hd.Ref]; ok {
				lc.HelpDefs[idx] = hd
			} else {
				helpIdx[hd.Ref] = len(lc.HelpDefs)
				lc.HelpDefs = append(lc.HelpDefs, hd)
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
