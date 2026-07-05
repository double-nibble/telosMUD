package world

import (
	"sort"

	"github.com/double-nibble/telosmud/internal/content"
)

// wearslot.go — the runtime CONTENT-DEFINED equipment vocabulary (#35). The wear-slot set was an engine-fixed
// enum; it is now content (WearSlotDTO, a pack's `wear_slots`) resolved here into an ordered vocab the equip
// verbs, targeting, and display read. Built once per shard (build.go) from the loaded slots, falling back to
// the engine default (content.DefaultWearSlots) when a pack declares none — so the bare engine and existing
// packs keep the classic Diku slots. Mirrors the trustLadder pattern (tier.go): a small immutable ordered
// vocab with an engine default, read lock-free on the zone goroutine.

// wearSlot is one resolved equipment slot: its label (the act() word) and kind (the equip-verb router).
type wearSlot struct {
	ref   WearLoc
	label string
	order int
	kind  string
}

// wearSlotVocab is the ordered, ref-keyed equipment vocabulary. `order` is the slice in display/selection
// order (sorted by the content Order, then ref for determinism); `byRef` is the O(1) lookup. Immutable after
// construction, so every zone sharing the default vocab shares one instance.
type wearSlotVocab struct {
	order []wearSlot
	byRef map[WearLoc]wearSlot
}

// defaultWearVocab is the engine's built-in vocab, built ONCE from the shared content default so the world
// never drifts from content.DefaultWearSlots. Shared by every zone with no content wear_slots.
var defaultWearVocab = buildWearVocab(content.DefaultWearSlots())

// buildWearVocab resolves the content slot DTOs into the runtime vocab (#35). A nil/empty list returns nil so
// the caller (z.wearSlots) falls back to the engine default. Slots are sorted by Order (ties broken by ref)
// so the `equipment` list and the `N.` ordinal selector agree on a stable order regardless of authoring/DB
// order. An unset kind defaults to "worn"; an unset label defaults to the ref.
func buildWearVocab(dtos []content.WearSlotDTO) *wearSlotVocab {
	if len(dtos) == 0 {
		return nil
	}
	v := &wearSlotVocab{byRef: make(map[WearLoc]wearSlot, len(dtos))}
	for _, d := range dtos {
		s := wearSlot{ref: WearLoc(d.Ref), label: d.Label, order: d.Order, kind: d.Kind}
		if s.label == "" {
			s.label = d.Ref
		}
		if s.kind == "" {
			s.kind = content.WearKindWorn
		}
		v.order = append(v.order, s)
		v.byRef[s.ref] = s
	}
	sort.SliceStable(v.order, func(i, j int) bool {
		if v.order[i].order == v.order[j].order {
			return v.order[i].ref < v.order[j].ref
		}
		return v.order[i].order < v.order[j].order
	})
	return v
}

// wearSlots is the zone-goroutine accessor for the shard's equipment vocab (#35), falling back to the engine
// default when a pack declares none. Lock-free (the built vocab is immutable).
func (z *Zone) wearSlots() *wearSlotVocab {
	if v := z.defBundle().wearSlots; v != nil {
		return v
	}
	return defaultWearVocab
}

// has reports whether ref is a defined slot in this vocab.
func (v *wearSlotVocab) has(ref WearLoc) bool { _, ok := v.byRef[ref]; return ok }

// label returns the human word for a slot (act() `$t`, the equipment list), or the bare ref for an unknown
// slot so a stale/dropped slot still renders something rather than blank.
func (v *wearSlotVocab) label(ref WearLoc) string {
	if s, ok := v.byRef[ref]; ok {
		return s.label
	}
	return string(ref)
}

// kindOf returns a slot's equip-verb kind ("worn"/"wield"/"hold"), or "" for an unknown slot.
func (v *wearSlotVocab) kindOf(ref WearLoc) string {
	if s, ok := v.byRef[ref]; ok {
		return s.kind
	}
	return ""
}

// isWorn reports whether a slot is filled by the generic `wear` verb (kind "worn") rather than wield/hold —
// so container.go picks a `wear` target without importing the content kind constants.
func (v *wearSlotVocab) isWorn(ref WearLoc) bool { return v.kindOf(ref) == content.WearKindWorn }

// orderedRefs returns every slot ref in display/selection order — the successor to the old package `wornOrder`
// var, now content-driven. Used by the equipment list, the inventory-by-slot render (#85), and ScopeEquipment.
func (v *wearSlotVocab) orderedRefs() []WearLoc {
	out := make([]WearLoc, 0, len(v.order))
	for _, s := range v.order {
		out = append(out, s.ref)
	}
	return out
}

// resolveKey maps a persisted/authored slot key to a defined slot ref (#35), tolerating three forms so old
// saves and label-authored content still load: (1) the ref itself; (2) a slot's LABEL (the pre-#35 on-disk
// form persisted the label, e.g. "wielded"/"held"); (3) the legacy hand aliases. Returns (ref, false) when
// the key names no defined slot — the caller drops the item with a warning (the prior unknown-slot behavior).
func (v *wearSlotVocab) resolveKey(key string) (WearLoc, bool) {
	if v.has(WearLoc(key)) {
		return WearLoc(key), true
	}
	for _, s := range v.order {
		if s.label == key {
			return s.ref, true
		}
	}
	switch key { // legacy hand-slot labels from pre-#35 saves
	case "wielded":
		if v.has(WearLocWield) {
			return WearLocWield, true
		}
	case "held":
		if v.has(WearLocHold) {
			return WearLocHold, true
		}
	}
	return WearLocNone, false
}

// slotOfKind returns the first slot ref (in order) whose kind matches — how the `wield`/`hold` verbs and the
// combat weapon slot find their slot without hardcoding a ref, so a pack may rename or reorder the hands.
// Returns WearLocNone when no slot has that kind.
func (v *wearSlotVocab) slotOfKind(kind string) WearLoc {
	for _, s := range v.order {
		if s.kind == kind {
			return s.ref
		}
	}
	return WearLocNone
}

// wieldSlot / holdSlot are the shard's weapon-hand / off-hand slot refs (by kind), used by the wield/hold
// verbs and combat. They fall back to the canonical refs so a vocab that omits a kind still resolves sanely.
func (z *Zone) wieldSlot() WearLoc {
	if s := z.wearSlots().slotOfKind(content.WearKindWield); s != WearLocNone {
		return s
	}
	return WearLocWield
}

func (z *Zone) holdSlot() WearLoc {
	if s := z.wearSlots().slotOfKind(content.WearKindHold); s != WearLocNone {
		return s
	}
	return WearLocHold
}
