package world

// equip_affects.go — #515 ITEM-SOURCED AFFECTS: an equipped item applies the affects it declares
// (WearableDTO.EquipAffects) to its wearer while worn, and removes them when unequipped. This is the
// generic mechanism behind on-equip magic effects AND OnHit weapon procs (a flame-tongue): an
// equip-applied affect participates in the event bus exactly like any active affect — gatherEventHandlers
// already collects handlers from the wearer's affects — so a weapon whose equip-affect subscribes OnHit
// deals its bonus damage with NO new item-subscription machinery in the bus.
//
// The affect is keyed by the ITEM as its source (attachOpts.source), so a wearer wearing two rings that
// both grant the same affect carries two distinct instances, and removing ONE ring strips only its own.
// CAVEAT: this per-item keying holds only for a SOURCE-scoped affect (the default). An equip-affect def
// authored `stack_scope: target` zeroes the source in keyFor, collapsing both rings to ONE instance — so
// removing one ring would strip the shared instance off the other. Author equip-affects source-scoped.
// The instance is marked fromEquip so it is transient — re-derived from worn gear on load, never
// independently persisted (character.go dumpAffects skips it; loadCharacter re-equips then re-derives).
//
// ON_APPLY fires on every LIVE wear/wield/hold (quiet=false), NOT once — putting a cursed helm on twice
// hurts twice, and a wear/remove macro re-fires a beneficial on_apply each cycle. Only the LOAD-time
// re-derivation is quiet (a relog must not re-fire it). Author guidance: put repeatable procs on OnHit /
// on_tick (which fire from gameplay), not on_apply (which fires per equip and is wear-cycle farmable).
//
// Self-application is not a cross-player harm vector: a wearer only ever equips their OWN items onto
// themselves, so applyAffect is called directly (no harm gate) — the gate exists for one entity affecting
// ANOTHER, which equipping never does. A debuff equip-affect (a cursed item) is self-inflicted by choice.
// The OnHit PROC's harm to a foe still funnels through the normal deal_damage harm gate at fire time.

// applyEquipAffects applies every affect an equipped item declares (#515), keyed by the item as source and
// marked transient. A no-op for an item without a Wearable or without equip affects, or a wearer without a
// zone. `quiet` suppresses the affect's on_apply hook — true on the equipment-LOAD re-derivation (the item
// was already equipped; a relog must not re-fire an on-apply proc), false on a LIVE wear/wield/hold.
func applyEquipAffects(wearer, item *Entity, quiet bool) {
	if wearer == nil || item == nil {
		return
	}
	wd, ok := Get[*Wearable](item)
	if !ok || len(wd.equipAffects) == 0 {
		return
	}
	for _, ref := range wd.equipAffects {
		applyAffect(wearer, ref, attachOpts{source: item, fromEquip: true, suppressApply: quiet}, nil)
	}
}

// removeEquipAffects removes every affect an equipped item applied (#515), matched by the (ref, item) key
// so it strips exactly this item's contribution and never a same-named affect from another source. Called
// from the remove verb and unequipFromWearer (the destroy/transfer chokepoint). A no-op if the wearer holds
// no matching instance (already expired, or never applied).
func removeEquipAffects(wearer, item *Entity) {
	if wearer == nil || item == nil || wearer.zone == nil {
		return
	}
	wd, ok := Get[*Wearable](item)
	if !ok || len(wd.equipAffects) == 0 {
		return
	}
	a, ok := Get[*Affected](wearer)
	if !ok {
		return
	}
	for _, ref := range wd.equipAffects {
		def := wearer.zone.affectDefs().get(ref)
		if def == nil {
			continue
		}
		if inst, present := a.byKey[keyFor(def, item)]; present {
			a.expire(wearer, inst, nil)
		}
	}
}
