package content

// wearslot.go — the engine's DEFAULT equipment vocabulary (#35). Wear slots are content (WearSlotDTO,
// authored in a pack's `wear_slots`), but the engine ships a built-in set so a pack that declares none — and
// the bare engine — keep the classic Diku slots with no authoring. Kept here beside the other content
// defaults (DefaultTrustTiers) so the world builds its runtime vocab from one shared source.

// Wear-slot kinds route the equip verb: a "worn" slot is filled by the generic `wear`, "wield" by the
// `wield` verb (and read as the combat weapon slot), "hold" by the off-hand `hold` verb. An unset kind is
// treated as "worn".
const (
	WearKindWorn  = "worn"
	WearKindWield = "wield"
	WearKindHold  = "hold"
)

// DefaultWearSlots is the engine's built-in slot vocabulary used when a pack declares no wear_slots: the
// classic Diku core (head/body/hands/feet + the two hands). Orders leave gaps (10,20,…) so a pack can slot a
// "waist"/"legs"/"finger" between them without renumbering. Refs are the canonical slot ids; labels are the
// words act() shows. This reproduces the pre-#35 fixed enum exactly, so existing content is unchanged.
func DefaultWearSlots() []WearSlotDTO {
	return []WearSlotDTO{
		{Ref: "head", Label: "head", Order: 10, Kind: WearKindWorn},
		{Ref: "body", Label: "body", Order: 20, Kind: WearKindWorn},
		{Ref: "hands", Label: "hands", Order: 30, Kind: WearKindWorn},
		{Ref: "feet", Label: "feet", Order: 40, Kind: WearKindWorn},
		{Ref: "wield", Label: "wielded", Order: 50, Kind: WearKindWield},
		{Ref: "hold", Label: "held", Order: 60, Kind: WearKindHold},
	}
}
