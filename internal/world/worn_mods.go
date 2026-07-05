package world

// worn_mods.go — #35 WORN-AFFIX STAT EFFECT: a worn item's rolled affixes (its per-instance Quality,
// loot.go/crafting) become live attribute modifiers on the wearer. Equip was a stub — a worn item conferred
// no bonus. This wires the gear-modifier seam the attributes.go stub comment reserved: the Wearer is the
// entity's single gear modSource, registered ONCE (ensureWornModSource) and recomputed on every wear/remove
// (recomputeWornMods), exactly mirroring the Affected affect-source pattern. Single-writer: zone goroutine.

// ensureWornModSource registers the wearer's Wearer component as an attribute modSource the first time — so
// its summed affix bonus (flatMod) feeds derivation. Idempotent: the `registered` flag guards against a
// second equip re-adding the same source (which would double-count gear). The actor is a player entity
// (prototype==nil), so mutating the Wearer is plain instance state — the same discipline the equip verbs use.
func ensureWornModSource(e *Entity) {
	wr, ok := Get[*Wearer](e)
	if !ok || wr.registered {
		return
	}
	addModSource(e, wr)
	wr.registered = true
}

// recomputeWornMods re-sums every worn item's rolled affixes into the Wearer's mods map and dirties the
// wearer's attribute cache, so the next attr() reflects the current gear. Called after any wear/wield/hold/
// remove and after loading a character's equipment. A worn item with no Quality (an un-rolled prototype)
// contributes nothing. Repeated attrs across items ADD (two +2-str rings give +4 str).
func recomputeWornMods(e *Entity, wr *Wearer) {
	m := make(map[string]float64)
	for _, item := range wr.worn {
		if item == nil {
			continue
		}
		if q, ok := Get[*Quality](item); ok {
			for attr, v := range q.Affixes {
				m[attr] += v
			}
		}
	}
	wr.mods = m
	markAttrsDirty(e)
}

// applyWornMods is the shared post-equip hook the wear/wield/hold/remove verbs (and the load path) call:
// ensure the gear modSource is registered, then recompute the summed bonus. Keeping it one call means no
// equip path can forget either half (register-once + recompute).
func applyWornMods(e *Entity, wr *Wearer) {
	ensureWornModSource(e)
	recomputeWornMods(e, wr)
}
