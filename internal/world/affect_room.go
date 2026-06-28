package world

// affect_room.go is the ROOM-SCOPED affect runtime ([G13], docs/PHASE6-PLAN.md §1.3) — an affect that
// attaches to a ROOM entity (web, darkness, silence-field, consecrate) rather than to a creature. It
// REUSES the Phase-5 Affected runtime for the room affect's own lifecycle (duration / stacking / expiry
// over the per-entity pulse, the resolve-by-id/skip-frozen contract) and adds the one room-specific
// behavior: each interval it walks the room's living OCCUPANTS and lands the affect's effect on them,
// and a creature that WALKS IN gets it on arrival.
//
// # The occupant model (no def bloat)
//
// A room affect's modifiers/prevents (the web's `prevents: [move]`, a darkness's accuracy debuff) must
// apply to the CREATURES in the room, not to the room (a room is not Living). Rather than authoring a
// twin "the effect a web puts on you" def, the room affect applies the SAME def to each occupant as a
// short-lived ENTITY-scoped instance whose duration is just over one room-tick interval. Each room tick
// REFRESHES that instance on everyone present; the moment a creature leaves (no more refresh) or the
// room affect expires (clearRoomAffectFromOccupants), the per-occupant copy lapses on its own. This is
// why the occupant-copy uses a short refresh duration: it is a lease the room renews while you stand in
// it. The per-occupant instance flows through the normal Affected runtime, so its prevents feed the
// SAME tag-CC gate (preventsTag) a directly-cast root would — the engine never special-cases "in a web".
//
// # Single-writer + the harm funnel
//
// Everything runs on the zone goroutine (the room is zone-owned; the tick fires on the pulse). The room
// tick honours the resolve-by-id/skip-frozen contract by NEVER leaking a stale occupant pointer: it
// re-reads room.contents fresh each tick and only touches LIVE in-room occupants (a departed creature is
// simply absent from the snapshot). A room affect's on_tick op-list (a damage field, a consecrate heal)
// runs through the SAME gated effect-op interpreter every other tick uses — so a harmful room-tick on a
// protected player funnels guardHarmful per occupant, exactly like a DoT. CC/modifier application via
// applyAffect/applyDebuff is gated identically.
//
// # Persistence (transient — P6-D8 alignment)
//
// A room-scoped affect is TRANSIENT: it is NOT serialized into the room/character snapshot. Room affects
// are re-applied by content (a reset op, an ability) after a restart/repop — the same disposition combat
// and threat carry (transient, rebuilt) — so this slice does not bloat the StateJSON shape. A durable
// room condition (a permanent consecrated shrine) would be authored as a reset that re-applies it; the
// hook for that is the reset interpreter, not the snapshot. See the report's persistence note.

// roomAffectLeaseSlack is how many extra pulses past one tick interval a per-occupant copy lives. The
// room renews (refreshes) the copy every interval; the slack ensures the lease never lapses BETWEEN two
// consecutive room ticks for a creature that stays put (so the CC is continuous while you're in the
// room), yet still expires shortly after you leave or the room affect ends.
const roomAffectLeaseSlack = 1

// applyRoomAffect attaches room-scoped affect `ref` to room entity `room`, applied by `source` ([G13]).
// It validates the def IS room-scoped (a mis-call with an entity affect is a no-op + log), attaches it
// to the room's Affected component for duration/expiry tracking via the standard runtime, lands it on
// every current occupant immediately, and registers the per-room tick that re-lands it each interval and
// clears it on expiry. Returns the room affect instance (nil on a bad ref / non-room-scoped def / a room
// with no zone). Single-writer: zone goroutine.
func applyRoomAffect(room *Entity, ref string, source *Entity) *affectInstance {
	if room == nil || room.zone == nil || room.room == nil {
		return nil
	}
	def := room.zone.affectDefs().get(ref)
	if def == nil {
		room.zone.log.Debug("room affect: unknown ref", "ref", ref)
		return nil
	}
	if !def.roomScoped {
		// A non-room-scoped affect must not be attached to a room (it would never reach a creature).
		room.zone.log.Debug("room affect: def is not room-scoped (ignored)", "ref", ref)
		return nil
	}
	a := affectedComponent(room)
	// Attach to the room for lifecycle tracking. The room is non-Living, so its OWN modifiers/prevents
	// are inert; the per-occupant copies carry the actual effect. We bypass the Living guard in
	// applyAffect by installing the instance directly through the shared attach helper specialized for
	// rooms (attachRoomInstance) — duration/stacking come from the def, source keys per-applier.
	inst := attachRoomInstance(room, a, def, source)
	if inst == nil {
		return nil
	}
	// Land it on everyone already standing here, then ensure the per-room tick keeps renewing it.
	landRoomAffectOnOccupants(room, inst)
	ensureRoomTick(room, a)
	room.zone.log.Debug("room affect applied", "ref", ref, "room", room.proto,
		"remaining", inst.remaining)
	return inst
}

// attachRoomInstance installs (or refreshes, per the def's stacking) a room affect instance on the room
// entity's Affected component. It mirrors applyAffect's stacking logic but WITHOUT the Living guard (a
// room is not Living) and without firing the entity on_apply hook. Single-writer: zone goroutine.
func attachRoomInstance(_ *Entity, a *Affected, def *affectDef, source *Entity) *affectInstance {
	key := keyFor(def, source)
	dur := def.duration
	if existing := a.byKey[key]; existing != nil {
		switch def.stacking {
		case stackExtend:
			existing.remaining += dur
		case stackIgnore:
			// first wins
		default: // refresh / count both reset the timer for a room field
			existing.remaining = dur
		}
		return existing
	}
	inst := &affectInstance{def: def, source: source, remaining: dur, magnitude: 1, stacks: 1}
	a.list = append(a.list, inst)
	a.byKey[key] = inst
	return inst
}

// landRoomAffectOnOccupants applies a room affect's effect to every LIVING occupant of the room right
// now ([G13]). For a CC/modifier room affect it leases the same def to each occupant as a short-lived
// entity-scoped instance (renewed each tick); for an on_tick room affect it ALSO runs the tick op-list
// against each occupant through the gated interpreter. The harm funnel is per occupant: applyDebuff /
// the deal_damage in the op-list each call guardHarmful, so a non-consenting player in the field is a
// clean no-op while a foe is affected. Reads room.contents FRESH (no stale pointer). Zone goroutine.
func landRoomAffectOnOccupants(room *Entity, inst *affectInstance) {
	for _, occ := range room.contents {
		if occ.living == nil {
			continue
		}
		landRoomAffectOn(room, occ, inst)
	}
}

// landRoomAffectOn applies room affect `inst` to a single living occupant `occ` ([G13]). The CC/modifier
// is leased as a short-lived entity instance (the lease the room renews); an on_tick op-list runs once
// against the occupant through the gated interpreter. The room affect's SOURCE is the applier, so the
// per-occupant copy + any harmful op gate against "may the applier harm this creature?" (a self/ambient
// room field with no source never gates against the occupant). Single-writer: zone goroutine.
func landRoomAffectOn(room *Entity, occ *Entity, inst *affectInstance) {
	def := inst.def
	src := inst.source

	// The CC/modifier lease: only meaningful if the def actually carries a modifier or a prevents tag.
	// A pure on_tick room affect (a damage field) carries neither and skips the lease.
	if len(def.modifiers) > 0 || len(def.prevents) > 0 {
		lease := def.tickInterval
		if lease <= 0 {
			lease = def.duration // a tickless room affect leases for its whole remaining duration
		}
		lease += roomAffectLeaseSlack
		opts := attachOpts{source: src, duration: lease}
		// Route through the harm funnel exactly like opApplyAffect derives: a detrimental room affect
		// (web's prevents, a debuff modifier) on a player gates; a beneficial one (consecrate's buff)
		// lands ungated on allies.
		if affectIsDetrimental(def) {
			c := &effectCtx{z: room.zone, actor: nonNilSource(src, occ), source: nonNilSource(src, occ), target: occ, mag: 1, disp: dispHarmful}
			applyDebuff(c, occ, def.ref, opts)
		} else {
			applyAffect(occ, def.ref, opts, nil) // a room-affect lease is a root apply (fresh cascade)
		}
	}

	// The on_tick effect (a damage field / a consecrate heal): run the def's tick op-list against the
	// occupant through the SAME gated interpreter a per-entity DoT uses. Source is the applier (fail-
	// closed if it detached, like fireOnTick), so a harmful field gates per occupant.
	if len(def.tickOps) > 0 {
		effSrc := src
		if effSrc == nil {
			effSrc = occ // self/ambient field: the occupant is the source (never self-gated)
		} else if effSrc.location == nil || effSrc.living == nil {
			room.zone.log.Debug("room affect tick: source detached, no-op", "ref", def.ref)
			return
		}
		c := &effectCtx{
			z: room.zone, actor: effSrc, source: effSrc, target: occ,
			mag: 1, disp: dispHarmful,
		}
		runOps(c, def.tickOps)
	}
}

// nonNilSource returns src, or fallback when src is nil — so a self/ambient room field (no applier)
// gates against the occupant itself (self-harm is never blocked) rather than nil.
func nonNilSource(src, fallback *Entity) *Entity {
	if src != nil {
		return src
	}
	return fallback
}

// ensureRoomTick registers the per-ROOM affect tick if not already running. One callback per room (not
// per room affect) drives every room affect's countdown + occupant re-lease + expiry. It honours the
// pulse contract: a room never migrates zones (id "" path) so the captured *Entity is the owner's own
// and safe to use directly — the resolve-by-id concern is players, and a room is never a player.
func ensureRoomTick(room *Entity, a *Affected) {
	if a.tick != nil {
		return
	}
	if !a.hasActiveAffects() {
		return
	}
	// TODO(security review, hardening): the room tick fires EVERY pulse and re-leases the CC to every
	// occupant, so a pathological pack (a high-population room × many stacked fields) is heartbeat
	// pressure on the single-writer goroutine. Lease at the field's tickInterval instead of every pulse
	// (roomAffectLeaseSlack already covers a >1 renewal gap so coverage stays continuous), and/or cap
	// concurrent room affects per room. Linear + population-self-limited today (the milestone is one web
	// in a small room), so deferred — but close before any large-scale room-field content.
	a.tick = room.zone.pulses.every(1, roomTickFor(room))
}

// roomTickFor builds the per-room tick callback ([G13]). Each pulse it advances every room affect by one
// (re-leasing the CC/modifier + running any on_tick on the current occupants at the affect's interval)
// and expires any whose remaining hit 0, clearing it from occupants. It re-reads room.contents fresh
// each pulse (never a stale occupant pointer). The room is zone-owned and never migrates, so the
// captured *Entity is safe. Returns false to retire when no room affects remain. Zone goroutine.
func roomTickFor(room *Entity) pulseFunc {
	return func(pulse uint64) bool {
		a, ok := Get[*Affected](room)
		if !ok {
			return false
		}
		roomTickOnce(room, a, pulse)
		if !a.hasActiveAffects() {
			a.tick = nil
			return false
		}
		return true
	}
}

// roomTickOnce advances every room affect one pulse: at each affect's interval it RE-LEASES the CC/
// modifier and runs any on_tick over the CURRENT occupants; it decrements remaining and EXPIRES (clearing
// the field from occupants) at 0. Snapshots the instance slice so an expiry mid-loop is safe. Single-
// writer: zone goroutine.
func roomTickOnce(room *Entity, a *Affected, _ uint64) {
	snapshot := make([]*affectInstance, len(a.list))
	copy(snapshot, a.list)
	for _, inst := range snapshot {
		if !inst.def.roomScoped {
			continue // an entity affect mis-attached to a room (defensive) — never tick it as a room field
		}
		// Re-lease / re-tick over occupants at the affect's interval (every pulse for a tickless CC
		// field so the lease is continuously renewed; at the on_tick interval for a damage field).
		interval := inst.def.tickInterval
		if interval <= 0 {
			interval = 1 // a tickless CC room field renews its lease every pulse
		}
		inst.sinceTick++
		if inst.sinceTick >= interval {
			inst.sinceTick = 0
			landRoomAffectOnOccupants(room, inst)
		}
		if inst.remaining > 0 {
			inst.remaining--
		}
		if inst.remaining <= 0 {
			expireRoomAffect(room, a, inst)
		}
	}
}

// expireRoomAffect removes a room affect from the room and clears its leased copy from every current
// occupant ([G13] expiry: the field is gone, so no one standing here keeps the CC). It drops the instance
// from the room's Affected list/byKey and expires the per-occupant lease on each living occupant.
// Single-writer: zone goroutine.
func expireRoomAffect(room *Entity, a *Affected, inst *affectInstance) {
	for i, x := range a.list {
		if x == inst {
			a.list = append(a.list[:i], a.list[i+1:]...)
			break
		}
	}
	delete(a.byKey, keyFor(inst.def, inst.source))
	clearRoomAffectFromOccupants(room, inst)
	room.zone.log.Debug("room affect expired", "ref", inst.def.ref, "room", room.proto)
}

// clearRoomAffectFromOccupants expires the per-occupant lease of a room affect on every living occupant
// (used on room-affect expiry). The lease would lapse on its own within roomAffectLeaseSlack pulses, but
// clearing it immediately makes expiry crisp (the web vanishes and everyone is free at once). Keyed by
// the room affect's source so it removes exactly the copy this field leased. Zone goroutine.
func clearRoomAffectFromOccupants(room *Entity, inst *affectInstance) {
	for _, occ := range room.contents {
		oa, ok := Get[*Affected](occ)
		if !ok {
			continue
		}
		if copyInst, present := oa.byKey[keyFor(inst.def, inst.source)]; present {
			oa.expire(occ, copyInst, nil) // a room-affect clear is a root expire (fresh cascade)
		}
	}
}

// applyRoomAffectsTo lands every ACTIVE room affect in `room` onto a creature that just ENTERED ([G13] —
// "someone walking INTO a web-roomed room gets rooted"). Called from the movement/arrival path after the
// entrant is placed in the room. Each room affect leases its CC/modifier (and runs any on_tick) on the
// entrant through the same gated path as a tick, so a non-consenting player entering a player's web is a
// clean no-op while a foe is snared. A no-op when the room carries no room affects. Zone goroutine.
func applyRoomAffectsTo(entrant *Entity) {
	if entrant == nil || entrant.living == nil {
		return
	}
	room := entrant.location
	if room == nil {
		return
	}
	a, ok := Get[*Affected](room)
	if !ok {
		return
	}
	for _, inst := range a.list {
		if !inst.def.roomScoped {
			continue
		}
		landRoomAffectOn(room, entrant, inst)
	}
}
