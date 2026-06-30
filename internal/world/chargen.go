package world

// chargen.go — Phase 14.8 (Model A): apply a not-yet-spawned character's content chargen RESULT on its FIRST
// spawn. telos-account recorded the chosen bundles + bought attribute values into the characters.chargen
// column (it has no world runtime, so it can't run the grant interpreter); the world reads them onto
// CharSnapshot.PendingChargen and applies them here, on the zone goroutine (single-writer), reusing the same
// grant path apply_bundle uses. The marker is cleared by the next save (SaveCharacter nulls the column),
// atomically with persisting the built state — so a crash before that save just re-applies from the still-
// empty DB state (no double-application of the additive racial mods).

// applyPendingChargen builds a freshly-created character on first spawn: it SETS the point-buy attribute bases
// (so a later racial bundle mod adds on top), then runs each chosen bundle's grants. Runs on the zone
// goroutine (called from loginRoom right after loadCharacter). A nil/empty result is a no-op.
func applyPendingChargen(z *Zone, s *session, cg *ChargenResult) {
	if cg == nil || s == nil || s.entity == nil {
		return
	}
	e := s.entity

	// 1. Point-buy: set each chosen attribute's BASE to the bought value. This must precede the bundles so a
	//    racial modify_attribute_base (+2 con) adds on top of the bought base, not the content default.
	for name, val := range cg.Attrs {
		setAttrBase(e, name, val)
	}

	// 2. Bundles (race/class/…): run each chosen bundle's grant op-list on the entity — the SAME ops
	//    apply_bundle runs (modify_attribute_base / set_flag / grant_track / grant_ability / …). Self-applied
	//    during chargen (actor == target == the new character), so no cross-player write concern.
	c := &effectCtx{z: z, actor: e, source: e, target: e, mag: 1, disp: dispNeutral}
	for _, ref := range cg.Bundles {
		def := z.bundleDefs().get(ref)
		if def == nil {
			z.log.Warn("chargen: unknown bundle, skipped", "bundle", ref, "player", s.character)
			continue
		}
		if len(def.grants) > 0 {
			runOps(c, def.grants)
		}
	}
	z.log.Info("chargen applied on first spawn", "player", s.character,
		"bundles", cg.Bundles, "attrs", len(cg.Attrs))
}
