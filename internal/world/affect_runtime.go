package world

// affect_runtime.go holds the mutating half of the Affected runtime: attach (the entry 5.3's
// apply_affect op calls), the four stacking modes, expiry, the modifier/prevents recompute, and the
// per-ENTITY tick that drives every affect's countdown + tick hook + expiry AND resource regen.
//
// Single-writer: every function here runs on the zone goroutine (attach from a command/op; the tick
// from the pulse scheduler). No DB I/O. The cardinal concurrency piece is the tick's resolve-by-id/
// skip-frozen contract (pulse.go pulseFunc doc comment) — see affectTickFor.

import "log/slog"

// attachOpts carries the optional knobs an apply_affect op supplies. Zero values mean "use the def's
// defaults" (duration from the def, magnitude 1, the def's stacking). reattach=true is the persistence
// path: it sets remaining from the SNAPSHOT (not the def's full duration) and suppresses the on_apply
// hook + the stacking rule (a load re-installs the saved instance verbatim, never re-fires side effects).
type attachOpts struct {
	source    *Entity // who applied it (part of the per-source stacking key); nil = self/ambient
	duration  int     // override remaining duration in pulses; <=0 => the def's duration
	magnitude float64 // applied magnitude; <=0 => 1
	stacks    int     // initial stacks (reattach path); <=0 => 1
	reattach  bool    // persistence re-attach: remaining is authoritative, no stacking, no on_apply
}

// applyAffect applies affect `ref` to entity e (docs/PHASE5-PLAN.md §1.4 — the runtime function
// 5.3's apply_affect op calls). It resolves the def, runs the stacking rule against any existing
// instance keyed by (ref[, source]) per stack_scope, records magnitude/stacks/source, updates the
// summed modifiers + prevents union, dirties the attr cache, fires the RESERVED OnApplyAffect hook,
// and ensures the per-entity tick is running. Returns the live instance (nil if the ref is unknown /
// no Living). Single-writer: zone goroutine. (Named applyAffect, not attach — a test helper owns the
// `attach` identifier in the package; this also reads as the apply_affect op name 5.3 will register.)
func applyAffect(e *Entity, ref string, opts attachOpts) *affectInstance {
	if e == nil || e.living == nil || e.zone == nil {
		return nil
	}
	def := e.zone.affectDefs().get(ref)
	if def == nil {
		e.zone.log.Debug("affect attach: unknown ref", "ref", ref, "rid", e.rid)
		return nil
	}
	a := affectedComponent(e)
	key := keyFor(def, opts.source)

	mag := opts.magnitude
	if mag <= 0 {
		mag = 1
	}
	dur := opts.duration
	if dur <= 0 {
		dur = def.duration
	}

	// Re-attach (persistence load): install the saved instance verbatim — remaining FROM THE
	// SNAPSHOT, stacks/magnitude from the snapshot — without running the stacking rule or the
	// on_apply hook. Must not double-tick or reset duration (docs/PHASE5-PLAN.md §3).
	if opts.reattach {
		st := opts.stacks
		if st < 1 {
			st = 1
		}
		inst := a.byKey[key]
		if inst == nil {
			inst = &affectInstance{def: def, source: opts.source}
			a.list = append(a.list, inst)
			a.byKey[key] = inst
		}
		inst.remaining = dur
		inst.magnitude = mag
		inst.stacks = st
		inst.sinceTick = 0
		a.recomputeMods()
		markAttrsDirty(e)
		a.ensureTick(e)
		e.zone.log.Debug("affect reattached", "ref", ref, "rid", e.rid,
			"remaining", inst.remaining, "stacks", inst.stacks)
		return inst
	}

	inst := a.byKey[key]
	if inst == nil {
		// First instance of this (ref[,source]): install fresh.
		inst = &affectInstance{def: def, source: opts.source, remaining: dur, magnitude: mag, stacks: 1}
		a.list = append(a.list, inst)
		a.byKey[key] = inst
	} else {
		// An instance already exists: run the stacking rule (P5-D3).
		switch def.stacking {
		case stackRefresh:
			inst.remaining = dur // reset duration to full; magnitude refreshed too
			inst.magnitude = mag
		case stackCount:
			if inst.stacks < def.maxStacks {
				inst.stacks++
			}
			inst.remaining = dur // a fresh application refreshes the timer as it stacks
			inst.magnitude = mag
		case stackExtend:
			inst.remaining += dur // sum durations
			inst.magnitude = mag
		case stackIgnore:
			// First wins: the new application is a no-op (timer + stacks unchanged).
		}
	}

	a.recomputeMods()
	markAttrsDirty(e)
	a.ensureTick(e)
	fireOnApplyAffect(e, inst) // RESERVED hook (5.3 wires the gated op-list)
	e.zone.log.Debug("affect attached", "ref", ref, "rid", e.rid,
		"remaining", inst.remaining, "stacks", inst.stacks, "stacking", def.stacking)
	return inst
}

// expire removes an affect instance from the entity: drops it from list + byKey, recomputes the
// modifiers + prevents (so its contribution is gone), re-dirties the attr cache, and fires the
// RESERVED OnAffectExpire hook. Single-writer: zone goroutine (called from the tick).
func (a *Affected) expire(e *Entity, inst *affectInstance) {
	for i, x := range a.list {
		if x == inst {
			a.list = append(a.list[:i], a.list[i+1:]...)
			break
		}
	}
	delete(a.byKey, keyFor(inst.def, inst.source))
	a.recomputeMods()
	markAttrsDirty(e)
	fireOnAffectExpire(e, inst) // RESERVED hook (5.3)
	e.zone.log.Debug("affect expired", "ref", inst.def.ref, "rid", e.rid)
}

// recomputeMods rebuilds the entity's summed modifier maps + the prevents union from the CURRENT
// active affect set. Called on any apply/stack/expire. Magnitude scales an ADDITIVE modifier
// (poison's -2*stacks strength) and the stack count multiplies it; multiplicative modifiers compose
// by product. The caller dirties the attr cache after this so derivation picks up the new values.
func (a *Affected) recomputeMods() {
	a.flat = nil
	a.mul = nil
	a.prevents = nil
	for _, inst := range a.list {
		scale := inst.magnitude * float64(maxInt(inst.stacks, 1))
		for _, m := range inst.def.modifiers {
			if m.add {
				if a.flat == nil {
					a.flat = map[string]float64{}
				}
				a.flat[m.attr] += m.value * scale
			} else {
				if a.mul == nil {
					a.mul = map[string]float64{}
				}
				cur, ok := a.mul[m.attr]
				if !ok {
					cur = 1
				}
				a.mul[m.attr] = cur * m.value
			}
		}
		for _, tag := range inst.def.prevents {
			if a.prevents == nil {
				a.prevents = map[string]int{}
			}
			a.prevents[tag]++
		}
	}
}

// ensureTick registers the per-ENTITY tick callback if it is not already running and the entity needs
// it (has affects, or has a resource with a regen rate). One callback per entity (not per affect),
// registered via z.pulses.every(1) so it fires every heartbeat. Idempotent. Zone goroutine only.
func (a *Affected) ensureTick(e *Entity) {
	if a.tick != nil {
		return
	}
	if !a.hasActiveAffects() && !needsRegen(e) {
		return
	}
	id := tickResolveID(e) // resolve-by-id key for the tick contract ("" => non-player)
	a.tick = e.zone.pulses.every(1, affectTickFor(e.zone, id, e))
}

// maybeStopTick cancels the per-entity tick when the entity no longer has affects OR a regen need.
// Called at the end of a tick. Zone goroutine only.
func (a *Affected) maybeStopTick(e *Entity) {
	if a.tick == nil {
		return
	}
	if a.hasActiveAffects() || needsRegen(e) {
		return
	}
	a.tick.cancel()
	a.tick = nil
}

// affectTickFor builds the per-entity tick callback. It HONOURS THE pulseFunc CONTRACT (pulse.go doc
// comment) VERBATIM: it captures the player's stable id (character) and re-resolves the live entity
// BY ID through z.players each tick — it NEVER closes over and mutates the *Entity captured at
// registration once that entity belongs to a player. If the player is absent (departed/handed off) or
// s.frozen (mid-handoff), it returns false to CANCEL — durations are conserved across the seam because
// only the owning zone's pulse decrements them.
//
// A NON-player entity (a future mob, id=="") has no z.players row to re-resolve through; for those the
// captured *Entity is the owner's own and is safe to use directly (the mob never migrates between
// zones the way a player does). This slice's tests + content drive players, so the resolve-by-id path
// is the one under -race scrutiny.
func affectTickFor(z *Zone, id string, fallback *Entity) pulseFunc {
	return func(pulse uint64) bool {
		e := fallback
		if id != "" {
			// Player: re-resolve by id. Absent or frozen => stop (do NOT touch a stale entity). Clear
			// the entity's tick handle (best-effort, by id) so a later attach re-arms a fresh tick
			// rather than seeing a stale (cancelled) handle and no-op'ing ensureTick.
			s, ok := z.players[id]
			if !ok || s == nil || s.entity == nil {
				// Absent here: the player either left (entity being reaped) or transferred to a
				// SIBLING zone (entity now owned by THAT zone's goroutine). Either way we must NOT
				// touch the entity — clearing a.tick from here could race the destination. We just
				// stop; the destination's transferIn re-arms a fresh tick, and a reap drops the
				// entity. (This is why transferIn clears+re-arms, not us.)
				return false
			}
			if s.frozen {
				if a, ok := Get[*Affected](s.entity); ok {
					a.tick = nil // a thaw + re-apply re-registers the tick
				}
				return false // mid-handoff: another zone may now own this entity; do not tick it
			}
			e = s.entity
		}
		if e == nil || e.living == nil {
			return false
		}
		a, ok := Get[*Affected](e)
		if !ok {
			// No affects component (e.g. tick kept alive purely for regen, then the component was
			// never created): just regen and decide whether to keep going.
			runRegen(e)
			if !needsRegen(e) {
				return false
			}
			return true
		}
		a.tickOnce(e, pulse)
		// Decide whether to keep the tick alive (affects remain or regen still needed).
		if !a.hasActiveAffects() && !needsRegen(e) {
			a.tick = nil
			return false
		}
		return true
	}
}

// tickOnce advances every active affect by one pulse: fire the on_tick hook at its interval, and
// EXPIRE any affect whose remaining hit 0. Then run resource regen. Single-writer: zone goroutine
// (the pulse). The iteration takes a snapshot of the instance slice so an expiry mid-loop (which
// mutates a.list) does not skip or double-visit. Expiry recomputes mods + dirties the cache.
func (a *Affected) tickOnce(e *Entity, pulse uint64) {
	snapshot := make([]*affectInstance, len(a.list))
	copy(snapshot, a.list)
	for _, inst := range snapshot {
		if inst.def.hasTick && inst.def.tickInterval > 0 {
			inst.sinceTick++
			if inst.sinceTick >= inst.def.tickInterval {
				inst.sinceTick = 0
				fireOnTick(e, inst, pulse) // RESERVED op-list (5.3 wires the gated deal_damage etc.)
			}
		}
		if inst.remaining > 0 {
			inst.remaining--
		}
		if inst.remaining <= 0 {
			a.expire(e, inst)
		}
	}
	runRegen(e)
}

// fireOnApplyAffect / fireOnAffectExpire / fireOnTick are the RESERVED affect hooks (docs/ABILITIES.md
// §8, docs/PHASE5-PLAN.md §1.4 / 5.2 scope boundary). This slice builds the hook POINTS and logs at
// DEBUG; the gated effect-op interpreter that runs the op-list lands in 5.3. They are intentionally
// no-op-with-a-log so the runtime is whole and 5.3 only fills the body — no lifecycle change.
func fireOnApplyAffect(e *Entity, inst *affectInstance) {
	if inst.def.onApply == nil {
		return
	}
	e.zone.log.Debug("affect on_apply hook (reserved; 5.3)", "ref", inst.def.ref, "rid", e.rid)
}

func fireOnAffectExpire(e *Entity, inst *affectInstance) {
	if inst.def.onExpire == nil {
		return
	}
	e.zone.log.Debug("affect on_expire hook (reserved; 5.3)", "ref", inst.def.ref, "rid", e.rid)
}

func fireOnTick(e *Entity, inst *affectInstance, pulse uint64) {
	if inst.def.onTick == nil {
		return
	}
	if e.zone.log.Enabled(nil, slog.LevelDebug) {
		e.zone.log.Debug("affect on_tick hook (reserved; 5.3)", "ref", inst.def.ref,
			"rid", e.rid, "stacks", inst.stacks, "pulse", pulse)
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// tickResolveID returns the stable player id (character) the tick re-resolves by, or "" for a
// non-player entity (a mob has no z.players row; its captured *Entity is the owner's own and safe to
// use directly). This is the key that makes the resolve-by-id/skip-frozen contract enforceable.
func tickResolveID(e *Entity) string {
	if s, ok := sessionOf(e); ok && s != nil {
		return s.character
	}
	return ""
}
