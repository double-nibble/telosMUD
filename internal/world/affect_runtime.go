package world

// affect_runtime.go holds the mutating half of the Affected runtime: attach (the entry 5.3's
// apply_affect op calls), the four stacking modes, expiry, the modifier/prevents recompute, and the
// per-ENTITY tick that drives every affect's countdown + tick hook + expiry AND resource regen.
//
// Single-writer: every function here runs on the zone goroutine (attach from a command/op; the tick
// from the pulse scheduler). No DB I/O. The cardinal concurrency piece is the tick's resolve-by-id/
// skip-frozen contract (pulse.go pulseFunc doc comment) — see affectTickFor.

import (
	"context"
	"log/slog"
	"math"
)

// durationIndefinite is the sentinel `remaining` an INDEFINITE affect (#545) carries: the tick never
// decrements or expires it (it ends only via dispel / remove_affect / death), and it round-trips through
// save/load unchanged so a persisted ward reloads still-indefinite. Negative so it is unmistakably not a
// live countdown (a countdown is always >= 0). The affect's def.indefinite flag is the AUTHORITY; this
// value is what the runtime stores/serializes for such an instance.
const durationIndefinite = -1

// attachOpts carries the optional knobs an apply_affect op supplies. Zero values mean "use the def's
// defaults" (duration from the def, magnitude 1, the def's stacking). reattach=true is the persistence
// path: it sets remaining from the SNAPSHOT (not the def's full duration) and suppresses the on_apply
// hook + the stacking rule (a load re-installs the saved instance verbatim, never re-fires side effects).
type attachOpts struct {
	source    *Entity // who applied it (part of the per-source stacking key); nil = self/ambient
	duration  int     // override remaining duration in pulses; <=0 => the def's duration
	magnitude float64 // applied magnitude; <=0 => 1
	stacks    int     // initial stacks (reattach path); <=0 => 1
	rung      int     // #541 initial ladder rung (reattach path); <1 => rung 1 for a ladder affect
	reattach  bool    // persistence re-attach: remaining is authoritative, no stacking, no on_apply
	fromEquip bool    // #515: this apply is derived from a worn item — mark the instance transient (not persisted)
	// suppressApply skips the on_apply hook + OnApplyAffect bus fire (#515): the load-time re-derivation of a
	// gear equip-affect must NOT re-fire on_apply (the item was already equipped — a relog is not a new
	// equip event; firing it would let a player farm an on-apply proc by re-logging). A LIVE wear passes
	// false so on_apply fires normally. Distinct from `reattach`, which also rewrites duration semantics.
	suppressApply bool
}

// applyAffect applies affect `ref` to entity e (docs/PHASE5-PLAN.md §1.4 — the runtime function
// 5.3's apply_affect op calls). It resolves the def, runs the stacking rule against any existing
// instance keyed by (ref[, source]) per stack_scope, records magnitude/stacks/source, updates the
// summed modifiers + prevents union, dirties the attr cache, fires the RESERVED OnApplyAffect hook,
// and ensures the per-entity tick is running. Returns the live instance (nil if the ref is unknown /
// no Living). Single-writer: zone goroutine. (Named applyAffect, not attach — a test helper owns the
// `attach` identifier in the package; this also reads as the apply_affect op name 5.3 will register.)
// `parent` is the IN-FLIGHT effect cascade ctx when this apply happens INSIDE one (an apply_affect op
// run by an event handler) — threaded into the OnApplyAffect bus fire so a NESTED apply inherits the
// cascade's depth + width budget and trips maxEventDepth/maxEventHandlers (event.go) instead of
// resetting to a fresh depth-0 root each pass. Without it, two mutually-applying affects recurse the
// Go stack unbounded (no Lua VM, so no sandbox defense) until the process fatal-panics. A TRUE root
// apply (a cast step, a tick, an equip, a persistence load) passes nil — a fresh root cascade. The
// zone-level backstop in fireEvent is the can't-forget second guard.
func applyAffect(e *Entity, ref string, opts attachOpts, parent *effectCtx) *affectInstance {
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
		if def.indefinite && opts.duration <= 0 {
			// #545: restore the sentinel for a genuinely-indefinite reattach (a saved -1 must not become
			// def.duration). A POSITIVE saved remaining is a FINITE lease copy of an indefinite room field
			// (landRoomAffectOn passes a finite lease duration) — it must reload finite so it still lapses,
			// not become a permanent CC. opts.duration (the saved remaining) is the discriminator.
			inst.remaining = durationIndefinite
		}
		inst.magnitude = mag
		inst.stacks = st
		if len(def.rungs) > 0 { // #541: restore the saved ladder rung (clamped; default rung 1)
			inst.rung = clampRung(def, opts.rung)
		}
		inst.sinceTick = 0
		// #539 NON-DURABILITY (review): the concentration SLOT is NOT restored on load. An affect's source
		// is not persisted (AffectJSON carries no source — the same fail-open property every source-keyed
		// affect behaviour has), so a reload cannot re-establish which source concentrates on what. A
		// self-cast concentration buff therefore reloads LIVE but UNSLOTTED — the single-slot cap is not
		// enforced against it until the caster re-casts a concentration spell (which claims a fresh slot).
		// This matches #546's "source-keyed affect state is transient across the persistence seam". Durable
		// single-slot would require persisting + re-resolving the source pointer, a broader change.
		a.recomputeMods()
		markAttrsDirty(e)
		a.ensureTick(e)
		e.zone.log.Debug("affect reattached", "ref", ref, "rid", e.rid,
			"remaining", inst.remaining, "stacks", inst.stacks)
		return inst
	}

	// IMMUNITY VETO (#538): a target trait can reject an incoming affect by its identity (ref/category/
	// tag) BEFORE it attaches and BEFORE its on_apply hook fires — the whole point of the primitive, and
	// why the check must sit here rather than at OnApplyAffect (which fires post-attach). Deliberately
	// AFTER the reattach branch: a persistence load re-installs saved state verbatim and must be
	// deterministic regardless of the order affects restore in (an immunity affect and a vetoed affect
	// restoring in either order must yield the same set), so the load path is never vetoed — a player who
	// gained immunity while logged out is handled by the same lifecycle that would strip it, not by a
	// load-order-sensitive veto here. A live apply (a cast, a proc, a tick) IS vetoed.
	//
	// SEMANTIC, stated explicitly (review): immunity BLOCKS NEW afflictions; it does NOT cleanse one
	// already attached. Gaining a charm-ward while charmed leaves the charm active — cleansing is a
	// separate remove_affect/dispel op (which a ward can run from its own on_apply). And immunity matches
	// on IDENTITY, not polarity, so a ward against a `mind` tag also blocks a benign affect that reuses
	// that tag; the shared token namespace is content's to keep disjoint. (The HARM GATE reads polarity
	// separately — a grant that can block a beneficial affect is classified harm at applyDebuff so it
	// can't be applied cross-player — but this identity veto itself is polarity-blind.)
	//
	// SELF-WARD EXEMPTION: an affect must not veto its OWN re-application. A ward that grants immunity to
	// a token it itself carries (spell_shield tagged `magic` granting immunity to `magic`) would else
	// refuse its own refresh and decay to a one-shot. When the matched token is one the INCOMING def
	// grants, the incoming affect IS a source of that immunity, so its (re)apply falls through to the
	// stacking rule below.
	if tok, immune := immuneToAffect(e, def); immune && !grantsToken(def, tok) {
		e.zone.log.Debug("affect apply vetoed: target is immune", "ref", ref, "matched", tok, "rid", e.rid)
		fireOnAffectBlocked(e, def, opts.source, parent)
		return nil // no attach, no stacking, no on_apply, no recompute — as if the apply never happened
	}

	inst := a.byKey[key]
	if inst == nil {
		// First instance of this (ref[,source]): install fresh. A LADDER affect (#541) starts at rung 1;
		// increment_rung raises it (rungIndex treats 0 as 1 too, so this is clarity, not strictly required).
		inst = &affectInstance{def: def, source: opts.source, remaining: dur, magnitude: mag, stacks: 1, fromEquip: opts.fromEquip}
		if len(def.rungs) > 0 {
			inst.rung = 1
		}
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
		case stackHighest:
			// HIGHEST-WINS (#541): a same-key re-apply REFRESHES like refresh (duration + magnitude). The
			// non-summing part is across DIFFERENT sources' instances (recomputeMods takes the strongest);
			// this same-(ref,source) branch must still refresh so a re-cast doesn't let the buff lapse.
			inst.remaining = dur
			inst.magnitude = mag
		case stackIgnore:
			// First wins: the new application is a no-op (timer + stacks unchanged).
		}
	}

	// #545: an INDEFINITE affect carries the sentinel remaining regardless of stacking mode — a refresh/
	// count/extend must not install a finite countdown on an "until dispelled" effect (extend would even
	// have summed two sentinels). The tick skips its countdown; it ends only via dispel/remove/death.
	//
	// EXCEPTION — an explicit positive opts.duration wins (review): the per-occupant LEASE of an indefinite
	// room field (landRoomAffectOn) passes a FINITE lease duration deliberately, so the copy lapses shortly
	// after the occupant leaves the room. Forcing the sentinel here made that lease permanent (the CC
	// followed the player out of the room and persisted across relog). Honoring an explicit finite duration
	// keeps the lease finite while a bare `apply_affect` of an indefinite def (opts.duration 0) stays
	// indefinite. So an author CAN pin an indefinite affect to a finite window by passing a duration.
	if def.indefinite && opts.duration <= 0 {
		inst.remaining = durationIndefinite
	}

	a.recomputeMods()
	markAttrsDirty(e)
	a.ensureTick(e)
	// CONCENTRATION single-slot (#539): a concentration affect bound to a source claims that source's one
	// slot, expiring its prior concentration (wherever attached). Done after recomputeMods so the new affect
	// is fully live before the prior tears down. A nil source is untracked.
	if def.concentration && opts.source != nil {
		e.zone.concentrationApply(opts.source, e, inst)
	}
	// CONCENTRATION break-on-incapacitation (#539): if THIS apply incapacitated a source that is currently
	// concentrating (a stun/paralyze/downed affect landing on the caster), break its concentration. Checked
	// for EVERY apply (not just concentration ones) because the incapacitating affect is a different affect
	// from the concentration one. Uses the post-recompute prevents/position so the new CC is visible.
	//
	// TRIGGER BOUNDARY (review): the break is APPLY-driven + DEATH-driven (die(), death.go). It catches the
	// in-scope incapacitators — a `prevents: [act]` stun (unioned at recompute) and the #535 downed/dying
	// state (a suspends_death affect applied via apply_affect, so canAct is false here). It does NOT catch
	// an incapacitation driven purely by a direct setPosition op (sleep with no affect) or a hold-at-0 with
	// no apply — those break only at the next apply/death re-check. In-scope content uses affects, so this
	// is a documented edge, not a live gap.
	if _, concentrating := e.zone.concentration[e]; concentrating && concentrationBroken(e) {
		e.zone.breakConcentration(e)
	}
	e.zone.republishCommsOnAccessChange(e) // hear-access may have crossed a channel floor (docs/REMAINING.md §1)
	if !opts.suppressApply {
		fireOnApplyAffect(e, inst, parent) // RESERVED hook + OnApplyAffect bus fire (threads the cascade)
	}
	e.zone.log.Debug("affect attached", "ref", ref, "rid", e.rid,
		"remaining", inst.remaining, "stacks", inst.stacks, "stacking", def.stacking)
	return inst
}

// expire removes an affect instance from the entity: drops it from list + byKey, recomputes the
// modifiers + prevents (so its contribution is gone), re-dirties the attr cache, and fires the
// RESERVED OnAffectExpire hook. Single-writer: zone goroutine (called from the tick).
//
// `parent` is the in-flight cascade ctx when expiry happens INSIDE an effect cascade (a remove_affect/
// dispel op run by a handler) — threaded into the OnAffectExpire bus fire so a NESTED expire (an
// OnAffectExpire handler that dispels another affect, …) inherits the cascade depth/width budget and
// trips the guards instead of resetting to a fresh root. A true root expire (the tick's countdown,
// a room-affect clear) passes nil. fireEvent's zone-level backstop is the can't-forget second guard.
func (a *Affected) expire(e *Entity, inst *affectInstance, parent *effectCtx) {
	for i, x := range a.list {
		if x == inst {
			a.list = append(a.list[:i], a.list[i+1:]...)
			break
		}
	}
	delete(a.byKey, keyFor(inst.def, inst.source))
	// #539: a concentration affect ending for ANY reason (countdown, dispel, a damage-save cancel, the
	// respawn strip) frees its source's slot so the source can concentrate again. Idempotent — a no-op if a
	// newer spell already replaced the slot, or breakConcentration already cleared it.
	if inst.def.concentration && inst.source != nil {
		e.zone.clearConcentrationSlot(inst.source, inst)
	}
	a.recomputeMods()
	markAttrsDirty(e)
	e.zone.republishCommsOnAccessChange(e) // hear-access may have crossed a channel floor (docs/REMAINING.md §1)
	fireOnAffectExpire(e, inst, parent)    // RESERVED hook + OnAffectExpire bus fire (threads the cascade)
	e.zone.log.Debug("affect expired", "ref", inst.def.ref, "rid", e.rid)
}

// stripHostileAffects HARD-removes every affect hostile to a respawning player (#318) — the chokepoint that
// makes "no hostile effect survives respawn" hold for EVERY death path that leaves an affect on the victim
// BEFORE die() -> respawnPlayer runs: a data op-list (already covered by the #69 runOps guard), a death-
// triggered OnKill / OnDamageTaken handler that debuffed the victim, a DoT/CC applied by an affect tick, or a
// Lua on_death hook. No caller has to remember to clean up — the strip lives at the one place every player
// death funnels through.
//
// It deliberately fires NO lifecycle hooks (on_expire / OnAffectExpire): death is a PURGE, not a natural
// expiry, and firing an on_expire handler here would re-open the very hole this closes — a death-triggered
// handler landing FRESH harm on the just-respawned player. So this drops the instances directly and only
// recomputes the derived modifier/prevents maps (a stripped debuff/CC must stop affecting the now-alive
// player) and refreshes comms hear-access (a silence/deafen that gated a channel is gone). Beneficial affects
// (buffs, heal-over-time) are LEFT: a respawn keeps your blessings. hasActiveAffects/needsRegen still drives
// the tick, so a now-affectless entity's tick self-cancels on its next fire. Single-writer: zone goroutine
// (respawnPlayer). nil/absent Affected component, or no hostile affect, is a no-op.
func stripHostileAffects(e *Entity) {
	a, ok := Get[*Affected](e)
	if !ok || len(a.list) == 0 {
		return
	}
	kept := make([]*affectInstance, 0, len(a.list))
	removed := false
	for _, inst := range a.list {
		// #515: an equip-DERIVED affect is tied to the STILL-WORN item, not to the death — respawn keeps the
		// player's gear, so its equip-affects must persist (a flame-tongue proc must keep proccing after a
		// death; a cursed item's debuff must not be sheddable by dying). Skipping the strip is SAFE against
		// the #318 hostile-purge purpose: a fromEquip affect is ALWAYS self-sourced from the victim's OWN
		// equipped item — an attacker has no path to apply one — so exempting it opens no cross-player hole.
		if inst.fromEquip {
			kept = append(kept, inst)
			continue
		}
		if affectStrippedOnRespawn(inst.def, e.zone.harmPolarity()) {
			delete(a.byKey, keyFor(inst.def, inst.source))
			removed = true
			continue
		}
		kept = append(kept, inst)
	}
	if !removed {
		return // no hostile affect: leave list/byKey and the derived maps untouched (no needless recompute)
	}
	a.list = kept
	a.recomputeMods()
	markAttrsDirty(e)
	if e.zone != nil {
		e.zone.republishCommsOnAccessChange(e) // a stripped silence/deafen may have restored a channel floor
	}
}

// recomputeMods rebuilds the entity's summed modifier maps + the prevents union from the CURRENT
// active affect set. Called on any apply/stack/expire. Magnitude scales an ADDITIVE modifier
// (poison's -2*stacks strength) and the stack count multiplies it; multiplicative modifiers compose
// by product. The caller dirties the attr cache after this so derivation picks up the new values.
func (a *Affected) recomputeMods() {
	a.flat = nil
	a.mul = nil
	a.prevents = nil
	a.preventsSrc = nil
	a.damageMult = nil
	a.immunity = nil
	// HIGHEST-WINS (#541): a stackHighest affect does NOT sum across its instances — only the STRONGEST
	// instance of each such ref contributes (5e "same-effect doesn't stack, take the strongest"). Compute
	// the winner (max magnitude*stacks) per ref up front; a weaker duplicate is skipped whole below.
	var highestWinner map[string]*affectInstance
	for _, inst := range a.list {
		if inst.def.stacking == stackHighest {
			if highestWinner == nil {
				highestWinner = map[string]*affectInstance{}
			}
			if w := highestWinner[inst.def.ref]; w == nil || instScale(inst) > instScale(w) {
				highestWinner[inst.def.ref] = inst
			}
		}
	}
	for _, inst := range a.list {
		// A weaker duplicate of a highest-wins ref contributes nothing (non-summing) — its modifiers,
		// prevents, immunity, damageMult AND preventsSource are all dropped. For the shared def-level sets
		// (prevents/immunity/damageMult) the winner re-supplies identical values, so no net loss. The one
		// tension (review F5, LOW): preventsSource is keyed PER SOURCE, so a highest-wins affect that ALSO
		// declares prevents_source would keep only the winner's source-relative CC. The two are semantically
		// at odds (highest-wins is for symmetric buffs/debuffs, not per-source charm); no content combines them.
		if inst.def.stacking == stackHighest && highestWinner[inst.def.ref] != inst {
			continue
		}
		// Resolve this instance's modifier + prevents set. A LADDER affect (#541) uses the CURRENT rung's
		// set (a discrete level, un-scaled) instead of the top-level modifiers/prevents; a plain affect uses
		// the def's, scaled by magnitude*stacks. The rung's non-linear per-level effects (exhaustion rung 4
		// halves max HP, rung 6 is death) are exactly what a scaled single debuff can't express.
		mods, prevents := inst.def.modifiers, inst.def.prevents
		scale := inst.magnitude * float64(maxInt(inst.stacks, 1))
		if len(inst.def.rungs) > 0 {
			r := inst.def.rungs[rungIndex(inst)]
			mods, prevents = r.modifiers, r.prevents
			scale = 1 // a ladder rung is a discrete level, not a dose to stack-scale
		}
		for _, m := range mods {
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
		for _, tag := range prevents {
			if a.prevents == nil {
				a.prevents = map[string]int{}
			}
			a.prevents[tag]++
		}
		// SOURCE-RELATIVE prevents (#546): key each tag by the affect's SOURCE (the charmer), so the gate can
		// block only actions targeting that specific entity. A nil source (a self/ambient affect) cannot be
		// source-relative — there is no "them" to scope against — so it is skipped (a nil-source charm is a
		// content error; it simply never blocks anything, rather than silently becoming a global block).
		if inst.source != nil {
			for _, tag := range inst.def.preventsSource {
				if a.preventsSrc == nil {
					a.preventsSrc = map[string]map[*Entity]int{}
				}
				if a.preventsSrc[tag] == nil {
					a.preventsSrc[tag] = map[*Entity]int{}
				}
				a.preventsSrc[tag][inst.source]++
			}
		}
		for _, tok := range inst.def.grantsImmunity {
			if a.immunity == nil {
				a.immunity = map[string]int{}
			}
			a.immunity[tok]++
		}
		// Per-target damage multipliers (#537), composed by PRODUCT across active affects. Two
		// resistances (0.5, 0.5) → 0.25, resist+vuln (0.5, 2) → 1 (cancel), and immunity (0) dominates
		// anything — the natural composition, and correct for immunity and cancellation. 5e's cap of
		// "one level of resistance" is a system rule content expresses through affect stacking, not the
		// engine's to impose here. Deliberately NOT scaled by magnitude/stacks: a multiplier is a
		// property of the CONDITION (you are resistant, or you are not), not a dose — a stackCount poison
		// does not make a resistance 4× stronger. An author wanting dose-scaled resistance composes
		// several affects.
		for typ, m := range inst.def.damageTakenMult {
			if a.damageMult == nil {
				a.damageMult = map[string]float64{}
			}
			cur, ok := a.damageMult[typ]
			if !ok {
				cur = 1
			}
			// Normalize EACH FACTOR before multiplying, not the composed product at read time. This is a
			// security requirement, not tidiness: the read-time clamp only catches an ODD count of
			// negatives — two `-3` factors compose to +9, a vulnerability that never trips a
			// post-composition `m < 0` guard, is classified a benign buff by hasVulnerability (which reads
			// the raw >1 test), and so lands cross-player ungated and amplifies incoming damage 9×. Clamping
			// per-factor to [0, ceiling] (and NaN to identity) makes that impossible: every factor is in
			// [0, ceiling], a product of factors <= 1 is <= 1, so a composed value > 1 REQUIRES a raw factor
			// > 1 — which hasVulnerability flags as harm and the PvP gate refuses.
			a.damageMult[typ] = cur * normDamageMultFactor(m)
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
			return needsRegen(e)
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
		// RE-ENTRANCY GUARD (#318 / security review). A DoT's own KILLING tick runs the whole death funnel
		// INLINE — fireOnTick -> runOps -> deal_damage -> die -> respawnPlayer -> stripHostileAffects — right
		// here inside this loop, and the strip removes hostile instances (this one, and any later snapshot
		// entry) from the live set. Skip any instance no longer present, so we neither re-fire a hostile tick
		// on the just-respawned player nor run the on_expire we deliberately suppressed during the strip. This
		// also correctly skips an instance a sibling affect's on_expire/dispel removed earlier in the loop.
		if a.byKey[keyFor(inst.def, inst.source)] != inst {
			continue
		}
		if inst.def.hasTick && inst.def.tickInterval > 0 {
			inst.sinceTick++
			if inst.sinceTick >= inst.def.tickInterval {
				inst.sinceTick = 0
				fireOnTick(e, inst, pulse) // RESERVED op-list (5.3 wires the gated deal_damage etc.)
				// EVENT BUS (7.8b): light the reserved OnAffectTick kind at each tick-interval
				// boundary, INDEPENDENT of the def's op-list (a subscriber reacts to the tick even
				// for an affect with no tickOps). Subject = the affected entity, counterpart = the
				// affect's source. A clean root fire (a tick, like the affect lifecycle hooks).
				e.zone.fireEvent(nil, evOnAffectTick, e, inst.source, float64(maxInt(inst.stacks, 1)))
			}
		}
		// #545 INDEFINITE: never count down, never expire from the tick — an "until dispelled" affect ends
		// only via dispel / remove_affect / death. Skipping the block below (where remaining == sentinel -1
		// would otherwise satisfy `<= 0` and expire it immediately) is what makes it durable. Its on_tick
		// (if any) still fired above, so an indefinite DoT/aura keeps pulsing.
		if !inst.def.indefinite {
			if inst.remaining > 0 {
				inst.remaining--
			}
			if inst.remaining <= 0 {
				a.expire(e, inst, nil) // a tick-countdown expiry is a genuine root (fresh cascade)
			}
		}
	}
	runRegen(e)
}

// fireOnApplyAffect / fireOnAffectExpire / fireOnTick are the affect-lifecycle hooks (docs/ABILITIES.md
// §8). They are LIVE, not stubs: fireOnApplyAffect and fireOnAffectExpire run the affect's Lua on_apply/
// on_expire hook AND fire the reserved OnApplyAffect / OnAffectExpire event-bus kinds (7.8b) so a content
// subscriber (a resource/affect on_event, a Lua bus handler) reacts; fireOnTick runs the on_tick op-list
// through the gated effect-op interpreter (the DoT path, 5.3). The only still-reserved surface is the
// OP-LIST form of on_apply/on_expire attached directly to the affect def (logged at DEBUG below, not yet
// executed) — the Lua and event-bus paths are the supported hooks. (OnRest stays dark until a rest
// mechanic exists to fire it.)
func fireOnApplyAffect(e *Entity, inst *affectInstance, parent *effectCtx) {
	// Lua on_apply hook (7.4d): runs when the affect attaches. `self` = e, actor = the affect's
	// source. nil-safe / no-op when no Lua hook. The op-list onApply remains reserved.
	if inst.def.onApplyLua != "" {
		e.zone.runAffectHookLua(e, inst, "on_apply", inst.def.onApplyLua)
	}
	if inst.def.onApply != nil {
		e.zone.log.Debug("affect on_apply hook (reserved op-list; 5.3)", "ref", inst.def.ref, "rid", e.rid)
	}
	// EVENT BUS (7.8b): light the reserved OnApplyAffect kind. The affect ATTACHED, so the bus fires
	// the event ABOUT the affected entity (subject = e) with the affect's source as the counterpart —
	// content/Lua subscribers (a resource/affect on_event, a Lua bus handler) react. `parent` THREADS
	// the in-flight cascade when this apply ran inside an effect cascade (an apply_affect op a handler
	// fired), so a NESTED apply trips maxEventDepth/maxEventHandlers; a true root (cast step/equip/load)
	// passes nil for a fresh cascade. So "a missing hook is an engine bug" holds AND the cascade stays
	// bounded (the fireEvent zone-backstop guards a forgotten thread besides).
	e.zone.fireEvent(parent, evOnApplyAffect, e, inst.source, 1)
}

func fireOnAffectExpire(e *Entity, inst *affectInstance, parent *effectCtx) {
	// Lua on_expire hook (7.4d): runs when the affect expires.
	if inst.def.onExpireLua != "" {
		e.zone.runAffectHookLua(e, inst, "on_expire", inst.def.onExpireLua)
	}
	if inst.def.onExpire != nil {
		e.zone.log.Debug("affect on_expire hook (reserved op-list; 5.3)", "ref", inst.def.ref, "rid", e.rid)
	}
	// EVENT BUS (7.8b): light the reserved OnAffectExpire kind (subject = the affected entity, the
	// affect's source as the counterpart). `parent` threads the in-flight cascade for a NESTED expire
	// (a remove_affect/dispel op a handler ran) so it trips the guards; a root expire passes nil.
	e.zone.fireEvent(parent, evOnAffectExpire, e, inst.source, 1)
}

// fireOnAffectBlocked lights the OnAffectBlocked event (#538) when an incoming affect was vetoed by the
// target's immunity — so content can narrate "your ward flares" or a proc can react. Subject = the
// immune entity, counterpart = the affect's would-be source (nil for a self/ambient apply). `parent`
// threads any in-flight cascade so a nested apply-that-was-blocked trips the same depth/width guards.
//
// KNOWN LIMIT, stated so it is a property and not an oversight: the event payload carries subject +
// source + mag, but NOT the blocked affect's ref — the fireEvent signature has no ref channel, and
// OnApplyAffect has the identical gap (an OnApplyAffect subscriber can't name which affect attached
// either). Content narrates at the granularity of "an incoming effect was warded", which is the common
// case (mind_blank flares regardless of which charm hit it). Threading a ref through the event payload
// is a broader change to fireEvent that would land OnApplyAffect and OnAffectBlocked together.
func fireOnAffectBlocked(e *Entity, def *affectDef, source *Entity, parent *effectCtx) {
	if e.zone == nil || def == nil {
		return
	}
	e.zone.fireEvent(parent, evOnAffectBlocked, e, source, 1)
}

// fireOnTick runs an affect's on_tick op-list through the GATED effect-op interpreter (Phase 5.3
// completes the 5.2-reserved hook). This is the DoT path: a poison's tick is just
// [deal_damage <type> <amt>], and its damage routes through the SAME shared mitigation pipeline +
// guardHarmful that a cast's deal_damage does — so a DoT on a protected player is gated exactly like a
// direct spell (the can't-bypass property covers the tick path). The effect's source/actor is the
// affect's SOURCE (who applied it — inst.source), NOT the victim, so the gate evaluates "may the
// applier still harm this target?" and per-source stacking inside any apply_affect keys correctly. The
// magnitude is the stack count (a poison's 4*stacks). A self/ambient affect (source nil) ticks with the
// victim as the source — a self-inflicted DoT is never gated against itself.
func fireOnTick(e *Entity, inst *affectInstance, pulse uint64) {
	if len(inst.def.tickOps) == 0 {
		return
	}
	src := inst.source
	sourceless := false
	if src == nil {
		src = e           // self/ambient: the victim is the source (self-harm is never gated)
		sourceless = true // ...but a sourceless ambient DoT's actor==target is an ARTIFACT (#397 item 1)
	} else if src.location == nil || src.living == nil {
		// FAIL-CLOSED: the affect's source has detached (reaped / handed off / mid-transfer). We must
		// NOT evaluate the PvP gate against a stale source pointer (it could read a wrong room flag or
		// race the owning goroutine). A harmful tick with no live, attributable source is a no-op this
		// pulse — the affect keeps counting down and will expire on its own. Explicit, not incidental.
		e.zone.log.Debug("affect on_tick: source detached, no-op harmful tick",
			"ref", inst.def.ref, "rid", e.rid)
		return
	}
	if e.zone.log.Enabled(context.Background(), slog.LevelDebug) {
		e.zone.log.Debug("affect on_tick", "ref", inst.def.ref,
			"rid", e.rid, "stacks", inst.stacks, "pulse", pulse)
	}
	c := &effectCtx{
		z: e.zone, actor: src, source: src, target: e,
		mag: float64(maxInt(inst.stacks, 1)), disp: dispHarmful,
		// Draw a DoT tick's damage dice from the zone combat rng (#58), not the process-global math/rand,
		// so a poison/bleed tick — and any loot from a DoT killing blow (die reads this ctx's rng) — join
		// the same reproducible stream as swings. Runs on the zone goroutine, so single-writer holds.
		rng: e.zone.combatRng(),
		// A sourceless ambient DoT (source nil -> actor==target=e) still honors a just-respawned victim's
		// spawn-protection window — the same one-flag fix the room-field twins use (#397 item 1). Latent
		// today (respawn strips hostile affects before opening the window), but closes the structural gap so
		// content that lands a nil-source ambient DoT can't bypass the shield.
		sourcelessAmbient: sourceless,
	}
	runOps(c, inst.def.tickOps)
}

// normDamageMultFactor clamps one content-declared damage_taken_mult factor (#537) into the safe
// domain [0, damageTakenMultCeiling] before it composes into the product. A negative factor becomes 0
// (immunity — a negative multiplier is nonsensical content and must never heal), a NaN becomes 1 (an
// unusable value is ignored, not treated as harm or immunity), and an over-ceiling / +Inf factor is
// capped. Doing this per-factor rather than on the composed product is what closes the two-negatives
// amplification hole (see recomputeMods).
func normDamageMultFactor(m float64) float64 {
	if math.IsNaN(m) {
		return 1
	}
	if m < 0 {
		return 0
	}
	if m > damageTakenMultCeiling {
		return damageTakenMultCeiling
	}
	return m
}

// instScale is an affect instance's effective strength, used by highest-wins (#541) to pick the strongest
// instance of a ref. For a LADDER affect the strength IS the current RUNG (magnitude/stacks are both 1 for
// a ladder, so without this two ladder instances would tie at 1 and a weaker rung installed first would
// suppress a severe one — security review F2); for a plain affect it is magnitude × stacks.
func instScale(inst *affectInstance) float64 {
	if len(inst.def.rungs) > 0 {
		return float64(rungIndex(inst) + 1) // 1-based rung
	}
	return inst.magnitude * float64(maxInt(inst.stacks, 1))
}

// rungIndex returns the 0-based index into def.rungs for a ladder affect's CURRENT rung (#541), clamping
// the 1-based inst.rung into [1, len(rungs)]. A 0/absent rung reads as rung 1. Caller guarantees rungs is
// non-empty.
func rungIndex(inst *affectInstance) int {
	r := inst.rung
	if r < 1 {
		r = 1
	}
	if r > len(inst.def.rungs) {
		r = len(inst.def.rungs)
	}
	return r - 1
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
