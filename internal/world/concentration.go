package world

// concentration.go implements #539: source-bound single-slot concentration. A caster concentrates on at
// most ONE concentration-flagged affect at a time, WHEREVER that affect lives (a remote charmed enemy, a
// room field, the caster). The Zone holds a per-source slot (Zone.concentration); this file keeps it
// consistent across apply / expire / incapacitation. Single-writer: every function runs on the zone
// goroutine. The stored *Entity keys are only compared and used to expire a LIVE affect (the expire
// re-validates the instance is still attached), never dereferenced after the holder is gone.
//
// The break-on-damaged-save half of concentration is deliberately NOT here: it is clean content (an
// OnDamageTaken reaction that rx:cancel()s the affect, which drops it via expire and clears the slot).
// This file owns only the single-slot enforcement + the incapacitation break.

// concentrationSlot records the one concentration affect a source maintains: the entity it is attached to
// and the instance.
type concentrationSlot struct {
	holder *Entity
	inst   *affectInstance
}

// concentrationApply enforces the single-slot rule when a concentration affect `inst` (attached to
// `holder`, cast by `source`) is applied (#539). If the source already concentrates on a DIFFERENT affect,
// that prior one is expired first (firing its on_expire so an anchored effect tears down); then the slot
// points at the new affect. A nil source/inst is untracked. Single-writer: zone goroutine.
func (z *Zone) concentrationApply(source, holder *Entity, inst *affectInstance) {
	if source == nil || inst == nil || z.concentration == nil {
		return
	}
	if prev, ok := z.concentration[source]; ok && prev.inst != inst {
		z.expireConcentration(prev) // expire's clearConcentrationSlot removes the old entry
	}
	// The OUTERMOST apply wins the slot. Re-entrancy note (review): if prev's on_expire (fired inside
	// expireConcentration) itself applies ANOTHER concentration affect from this same source, that nested
	// apply sets the slot, and this line then clobbers it back to `inst` — leaving the nested affect
	// attached-but-untracked (it lapses on its own timer). That is the intended precedence (the spell the
	// caster is actually casting wins the slot over a side effect of a teardown), and it is not a crash or
	// a double-expire (expireConcentration's byKey re-validation prevents that). A concentration on_expire
	// that casts another concentration spell is pathological content; the single-slot cap is best-effort
	// under it.
	z.concentration[source] = concentrationSlot{holder: holder, inst: inst}
}

// breakConcentration ends whatever concentration `source` currently maintains (#539) — used when the source
// is INCAPACITATED (stunned / downed / dead). It clears the slot FIRST (so the expire's own slot-clear is a
// no-op and there is no re-entrancy through the slot), then expires the held affect firing on_expire. A
// no-op when the source concentrates on nothing. Single-writer: zone goroutine.
func (z *Zone) breakConcentration(source *Entity) {
	if source == nil || z.concentration == nil {
		return
	}
	slot, ok := z.concentration[source]
	if !ok {
		return
	}
	delete(z.concentration, source)
	z.expireConcentration(slot)
}

// expireConcentration expires the affect a slot holds, re-validating the holder still carries that exact
// instance (a prior cascade may have removed it) so a stale slot never double-expires or derefs a detached
// holder. It threads a nil parent: a concentration break is a genuine ROOT, a side effect of casting a new
// spell / being incapacitated, not part of the triggering op-list's cascade tree.
func (z *Zone) expireConcentration(slot concentrationSlot) {
	if slot.holder == nil || slot.inst == nil {
		return
	}
	// CROSS-ZONE GUARD (#539 review, CRITICAL): only touch a holder THIS zone still owns. If the holder
	// transferred to a sibling zone (intra-shard re-home) or was rebuilt cross-shard, its *Entity now
	// belongs to another goroutine — expiring it from here (delete/recompute/markDirty/fireOnExpire) would
	// be a double-writer data race on a live entity. A stale slot for a departed holder is cleaned at the
	// transfer/quit seam (breakConcentrationInvolving); this guard is the can't-forget backstop for any
	// seam that is missed — it degrades to a leaked slot, never a race. holder.zone == z is the discriminator.
	if slot.holder.zone != z {
		return
	}
	a, ok := Get[*Affected](slot.holder)
	if !ok {
		return
	}
	if a.byKey[keyFor(slot.inst.def, slot.inst.source)] == slot.inst {
		a.expire(slot.holder, slot.inst, nil)
	}
}

// clearConcentrationSlot drops the slot for `source` IF it points at `inst` (#539) — called from expire()
// when a concentration affect ends for ANY reason (natural countdown, dispel, a damage-save cancel, the
// respawn strip), so the source can concentrate again. A no-op if the slot points elsewhere (a newer spell
// already replaced it) or was already cleared. Single-writer: zone goroutine.
func (z *Zone) clearConcentrationSlot(source *Entity, inst *affectInstance) {
	if source == nil || z.concentration == nil {
		return
	}
	if slot, ok := z.concentration[source]; ok && slot.inst == inst {
		delete(z.concentration, source)
	}
}

// breakConcentrationInvolving ends any concentration where `e` is EITHER the SOURCE (a dead/reaped caster
// holds nothing) OR the HOLDER (the target of a concentration spell died, so the caster's spell ends and
// it is free to concentrate again). Called from the death funnel. The holder scan is O(active
// concentrations) — tiny and off the hot path (death is rare). Single-writer: zone goroutine.
func (z *Zone) breakConcentrationInvolving(e *Entity) {
	if e == nil || z.concentration == nil {
		return
	}
	// e as HOLDER: gather the sources whose slot points at e first (mutating the map during a range is
	// unsafe), then break each.
	var sources []*Entity
	for src, slot := range z.concentration {
		if slot.holder == e && src != e {
			sources = append(sources, src)
		}
	}
	for _, src := range sources {
		z.breakConcentration(src)
	}
	z.breakConcentration(e) // e as SOURCE
}

// concentrationBroken reports whether entity e is INCAPACITATED for concentration purposes (#539): it
// cannot act (sleeping/dead/downed via position) or is stun-locked (a `prevents: [act]` CC). Death
// suspension (downed) makes canAct false, so it is covered. A concentrating source that becomes any of
// these drops its concentration.
func concentrationBroken(e *Entity) bool {
	return !canAct(e) || preventsTag(e, "act")
}
