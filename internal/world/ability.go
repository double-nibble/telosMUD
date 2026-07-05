package world

// ability.go is the ability LIFECYCLE (docs/ABILITIES.md §4, docs/PHASE5-PLAN.md §1.6) — the fixed
// 10-step execution pipeline every content-defined ability runs through, mapped onto the existing
// command/pulse spine. The engine owns steps 1-7 + 9-10; step 8 (on_resolve) is content (the effect-op
// interpreter, effect_op.go). The whole lifecycle runs ON THE ZONE GOROUTINE (single-writer); the one
// asynchronous seam is the cast-time / cooldown timers, which ride the pulse scheduler honoring the
// resolve-by-id/skip-frozen contract.
//
//	 1 invoke          a command-invocation ability registers a Command-like verb; dispatch enters here.
//	 2 resolve targets Zone.Resolve + the ability's target mode/scope.
//	 3 check requires  attr thresholds + tag-CC (does an active affect prevent a tag this ability carries?)
//	 4 hostility gate  harmful disposition vs a non-consenting player -> pvpAllowed (the OUTER PvP layer).
//	 5 reserve costs   enough of each resource? fail -> abort with a message.
//	 6 cast time       >0: a pulse.after lockout (interruptible; on interrupt refund). 0 -> straight to commit.
//	 7 commit          pay costs (modify_resource self), impose lag, arm cooldown via pulse.after.
//	 8 on_resolve      the effect-op interpreter; EVERY harmful op re-checks the gate + routes mitigation.
//	 9 emit            ctx.Act messages now; GMCP deltas (Phase 9) RESERVED.
//	10 events          OnAbilityResolved/OnHit (Phase 6/7) RESERVED — fire-no-op/log.
//
// proc/passive invocations (event-driven) RESERVE their hooks — events are Phase 6/7.

import (
	"context"
	"log/slog"
	"math/rand"
)

// castAbility runs the full lifecycle for actor invoking ability `def` with the raw target argument
// `arg` (the verb's tail). It is the entry point a command-invocation ability's dispatch path calls.
// Returns nothing; all feedback reaches the actor via Send/Act. Single-writer: zone goroutine.
//
// rng is the deterministic-injectable source for dice/chance (nil => the package default); tests pass
// a seeded rng. Production passes nil.
func (z *Zone) castAbility(s *session, def *abilityDef, arg string, rng *rand.Rand) {
	actor := s.entity
	z.log.Debug("ability lifecycle: invoke", "ability", def.ref, "actor", actor.short, "arg", arg)

	// --- Step 2: resolve targets -------------------------------------------------------------
	target, ok := z.resolveAbilityTarget(actor, def, arg)
	if !ok {
		s.send(textFrame("They aren't here."))
		return
	}
	z.log.Debug("ability lifecycle: targets", "ability", def.ref, "target", targetShort(target))

	// --- Step 3: check requires (attr thresholds + tag-CC) -----------------------------------
	if !z.checkRequires(s, def) {
		return // checkRequires sent the block message
	}

	// --- Step 4: hostility gate (the OUTER PvP layer; defense-in-depth) ----------------------
	// A harmful-disposition ability vs a non-consenting player is blocked BEFORE costs. This is the
	// whole-ability layer; the in-op guardHarmful (step 8) is the inner layer that can't be bypassed.
	if def.disposition == dispHarmful && target != nil && !pvpAllowed(actor, target) {
		z.log.Debug("ability lifecycle: blocked at step 4 (pvp gate)",
			"ability", def.ref, "actor", actor.short, "target", target.short)
		s.send(textFrame("You cannot harm " + target.Name() + " here."))
		return
	}

	// --- Step 5: reserve costs ---------------------------------------------------------------
	if !z.canAffordCosts(actor, def) {
		s.send(textFrame("You lack the energy to do that."))
		z.log.Debug("ability lifecycle: insufficient costs", "ability", def.ref, "actor", actor.short)
		return
	}

	// --- Step 6: cast time -------------------------------------------------------------------
	// cast_time 0 (the fireball milestone) goes straight to commit. cast_time > 0 schedules a pulse
	// lockout that re-resolves BOTH the caster AND the target by id before completing (resolve-by-id/
	// skip-frozen) — a target that transferred/froze/was reaped mid-cast aborts the cast cleanly, so
	// the deferred callback never writes an entity another zone goroutine now owns. Costs are
	// reserved-but-not-yet-paid; they are paid at commit. An interrupt before completion refunds
	// (nothing was paid, so a refund is a no-op this phase — the hook is here for the interrupt path).
	if def.castTime > 0 {
		z.scheduleCast(s, def, target, arg, rng)
		s.send(textFrame("You begin casting..."))
		z.log.Debug("ability lifecycle: cast time started", "ability", def.ref, "pulses", def.castTime)
		return
	}

	// Instant cast: commit + resolve now.
	z.commitAbility(s, def, target, arg, rng)
}

// castTarget is the STABLE IDENTITY of a deferred cast's target — captured by id, NEVER by a raw
// *Entity (the cross-zone single-writer violation the architect found: a *Entity captured at cast
// start may transfer to a sibling zone / be frozen / be reaped before the completion pulse, and the
// deferred callback running on the SOURCE goroutine would then write an entity another goroutine owns,
// AND ensureTick could append to the DESTINATION zone's pulses). We capture an id and re-resolve it
// scoped to THIS zone at completion, mirroring the caster's resolve-by-id and the affect-tick contract.
//
//   - self: kind tmSelf — collapses to the (already re-resolved) caster at completion.
//   - player: kind tmPlayer — the target's character id; re-resolved via z.players[playerID].
//   - mob: kind tmMob — the target's rid; re-resolved by rid in the CALLER's (caster's) current room.
//   - none: kind tmNone — no target (self/none-mode abilities, AoE seam).
type castTarget struct {
	kind     targetMode // tmSelf / tmEnemy(player or mob) / tmAlly / tmNone — only used to branch
	isSelf   bool       // the target IS the caster (collapse to the re-resolved caster)
	playerID string     // a player target's stable character id ("" if not a player)
	rid      RuntimeID  // a mob target's per-zone runtime id (valid only when playerID=="" and not self)
	hasMob   bool       // a mob target was captured by rid
}

// captureCastTarget records the deferred cast's target as a STABLE identity at cast start (zone
// goroutine; the target is live here). A nil target is tmNone; the caster itself is isSelf; a player is
// captured by character id; any other living is a mob captured by rid.
func captureCastTarget(actor, target *Entity) castTarget {
	if target == nil {
		return castTarget{kind: tmNone}
	}
	if target == actor {
		return castTarget{isSelf: true}
	}
	if s, ok := sessionOf(target); ok && s != nil {
		return castTarget{playerID: s.character}
	}
	return castTarget{rid: target.rid, hasMob: true}
}

// resolve re-resolves the deferred target scoped to THIS zone at completion (zone goroutine). It
// returns (target, true) when the target is still present-and-writable in THIS zone, or (nil, false)
// when it left mid-cast (transferred / frozen / reaped) — in which case the cast is a clean no-op. The
// caster has ALREADY been re-resolved by id; `caster` is that live entity. Every entity this returns is
// one THIS zone owns, so every write the deferred cast performs stays single-writer.
func (ct castTarget) resolve(z *Zone, caster *Entity) (*Entity, bool) {
	if ct.isSelf {
		return caster, true // self collapses to the re-resolved caster
	}
	if ct.kind == tmNone && ct.playerID == "" && !ct.hasMob {
		return nil, true // a none-mode ability resolves with no target (not an abort)
	}
	if ct.playerID != "" {
		// Player target: re-resolve by id. Absent (left/handed off) or frozen (mid-handoff) => the
		// target is no longer ours to write; abort cleanly.
		live, ok := z.players[ct.playerID]
		if !ok || live == nil || live.entity == nil || live.frozen {
			return nil, false
		}
		return live.entity, true
	}
	if ct.hasMob {
		// Mob target: re-resolve by rid in the caster's CURRENT room. A mob never migrates zones, so
		// finding it by rid in the room scopes the write to this zone. Gone (reaped/left the room) =>
		// abort cleanly.
		if caster.location == nil {
			return nil, false
		}
		for _, e := range caster.location.contents {
			if e.rid == ct.rid {
				return e, true
			}
		}
		return nil, false
	}
	return nil, false
}

// scheduleCast arms the cast-time lockout on the pulse scheduler (step 6). It captures BOTH the
// caster's stable id AND the TARGET's stable identity (resolve-by-id, not a raw *Entity) and
// re-resolves BOTH by id when the timer fires: if the caster departed or is frozen mid-handoff, the
// cast is ABORTED; if the TARGET departed (transferred to a sibling zone, frozen, or reaped) it is
// ABORTED too — the same contract the affect tick honors. This keeps every write the deferred cast
// performs (and any tick ensureTick arms) on entities THIS zone owns, never reaching across the zone
// boundary. Single-writer: the callback fires on the zone loop.
func (z *Zone) scheduleCast(s *session, def *abilityDef, target *Entity, arg string, rng *rand.Rand) {
	id := s.character
	ct := captureCastTarget(s.entity, target)
	z.pulses.after(pulseCount(def.castTime), func(_ uint64) bool {
		// Resolve-by-id: the caster may have transferred zones or frozen since the cast began.
		live, ok := z.players[id]
		if !ok || live == nil || live.entity == nil {
			z.log.Debug("cast aborted: caster absent at completion", "ability", def.ref, "id", id)
			return false // one-shot; nothing to reschedule
		}
		if live.frozen {
			z.log.Debug("cast aborted: caster frozen mid-handoff", "ability", def.ref, "id", id)
			return false
		}
		// Re-resolve the TARGET scoped to THIS zone. A target that left mid-cast is a clean no-op —
		// do NOT write a transferred/reaped entity another goroutine may now own.
		tgt, present := ct.resolve(z, live.entity)
		if !present {
			z.log.Debug("cast aborted: target left mid-cast (resolve-by-id)", "ability", def.ref, "id", id)
			return false
		}
		z.commitAbility(live, def, tgt, arg, rng)
		return false
	})
}

// commitAbility runs steps 7-10: pay costs, impose lag, arm cooldown, run the effect-op interpreter
// (step 8), then emit (step 9) and fire the reserved events (step 10). It is reached either instantly
// (cast_time 0) or from the cast-time callback (after the resolve-by-id check). Single-writer: zone
// goroutine.
func (z *Zone) commitAbility(s *session, def *abilityDef, target *Entity, arg string, rng *rand.Rand) {
	actor := s.entity

	// --- BeforeCastCommit checkpoint ([G9], event.go): an ability is about to commit. It fires as a
	// reserved DECLARATIVE checkpoint (a content on_event can build a resource / proc on a cast) AND,
	// since 7.9, as a RESULT-ALTERING REACTION checkpoint: a Lua Counterspell hook reaches in via the
	// typed `rx` and CANCELS the cast (rx:cancel()), observe-then-recheck — no pipeline surgery. The
	// declarative fire's subject is the caster; the cast's target rides as the event `other`. The
	// reaction fire (fireBeforeCastReaction) instead fires about each eligible OBSERVER in the room
	// (the would-be counterspeller), with the caster as `other` — the OnLeaveRoom reactor convention.
	// Depth+width guarded (event.go); the reaction threads the SAME budget (T12 invariant 3).
	prec := &effectCtx{z: z, actor: actor, source: actor, target: actor, mag: 1, disp: def.disposition, rng: rng}
	castOrigin := actor.location
	z.fireEvent(prec, evBeforeCastCommit, actor, target, 1)
	// The result-altering reaction pass (7.9): if any observer's Counterspell-style reaction cancels
	// the cast, ABORT before committing (no costs paid, no resolve). Observe-then-recheck: the engine
	// re-reads the recorded cancel after the inline hook ran.
	if z.fireBeforeCastReaction(prec, actor, target) {
		z.log.Debug("cast canceled by a BeforeCastCommit reaction (counterspell)", "ability", def.ref, "caster", actor.short)
		return
	}
	// SC2 (distsys review): if a BeforeCastCommit handler KILLED the caster, die()->respawnPlayer already
	// relocated + revived them — do NOT commit the cast (it would pay costs + resolve from the respawn
	// room). Same liveness-after-sub-call discipline as the flee/move M1 fix: a changed location (respawn
	// clears posDead) means the death path owns the caster now.
	if actor.location != castOrigin || position(actor) == posDead {
		return
	}

	// --- Step 7: commit (pay costs, lag, cooldown) -------------------------------------------
	z.payCosts(actor, def)
	if def.lag > 0 {
		// WAIT_STATE / ctx.Lag is a Phase 6 combat concern; reserve the hook (log) this phase.
		z.log.Debug("ability lifecycle: lag imposed (reserved)", "ability", def.ref, "lag", def.lag)
	}
	if def.cooldown > 0 {
		z.armCooldown(s, def)
	}
	z.log.Debug("ability lifecycle: committed", "ability", def.ref, "actor", actor.short)

	// --- Step 8: on_resolve (the effect-op interpreter) --------------------------------------
	// The disposition flows into the effectCtx so every harmful op re-checks the gate (in-op layer).
	// The source IS the actor for a cast (a DoT's source differs — see runAffectTickOps).
	c := &effectCtx{
		z: z, actor: actor, source: actor, target: target,
		mag: 1, disp: def.disposition, rng: rng, arg: arg,
	}
	runOps(c, def.ops)
	// Lua on_resolve (slice 7.4b): a content Lua body that composes effect ops via the 7.3
	// handles (ctx.target:damage{} …). It runs BESIDE the declarative op-list (no lifecycle
	// change), threading the SAME step-8 effectCtx `c` — so a harm op in the Lua body inherits the
	// gate disposition + the cascade depth/budget and gates per 7.3c, never a fresh invocation.
	// Compile-once-per-zone + fail-closed (a broken body is inert; a runtime error fizzles this
	// resolve and is logged — the breaker is 7.5). The body reads ctx.actor/ctx.target/ctx.room.
	z.runAbilityResolveLua(c, def)

	// --- Step 9: emit (act messages; GMCP RESERVED Phase 9) ----------------------------------
	if def.msgActor != "" {
		z.act(def.msgActor, actor, nil, target, "", "", ToActor)
	}
	if def.msgRoom != "" {
		z.act(def.msgRoom, actor, nil, target, "", "", ToRoom)
	}
	// GMCP vitals/affliction/cooldown deltas are RESERVED (Phase 9): the emit point is here.

	// --- Step 10: events (the in-zone event bus, [G3]) ---------------------------------------
	// Fire OnAbilityResolved so content (a resource that builds on ability use, a proc affect) reacts.
	// Synchronous, single-writer, depth-guarded (event.go). The target rides as the event `other`.
	// OnHit fires from the combat swing pipeline (6.3); OnApplyAffect from the affect runtime (later).
	z.fireEvent(c, evOnAbilityResolved, actor, target, 1)
	// Phase 11.3: a SKILL ability also fires OnSkillUse about the user, so a use-based track can advance on
	// use ("advance-through-use" — LP/Discworld/BRP) decoupled from the skill's primary effect. The skill
	// improvement (a chance to gain) is a content handler on OnSkillUse; the engine only fires the hook.
	// #38 slice B: an op that REFUSED (suppressSkillUse) cancels the fire — a gated action that did no work
	// must not advance the skill that gates it (else a player trains past the gate by spamming a refusal).
	if def.skill != "" && !c.suppressSkillUse {
		z.fireEvent(c, evOnSkillUse, actor, target, 1)
	}
}

// resolveAbilityTarget resolves the ability's target per its mode (step 2). self/none target the actor
// (or nothing); enemy/ally resolve a living in the room by the typed keyword via Zone.Resolve. Returns
// (target, true) on success, (nil, false) when a target was required but not found.
func (z *Zone) resolveAbilityTarget(actor *Entity, def *abilityDef, arg string) (*Entity, bool) {
	switch def.mode {
	case tmSelf:
		return actor, true
	case tmNone:
		return nil, true
	case tmEnemy, tmAlly:
		// Resolve a living target in the room by the typed keyword (the classic `cast fireball goblin`).
		hits := z.Resolve(actor, parseTargetSpec(arg), ScopeRoomLiving)
		if len(hits) == 0 {
			// [G12] AoE: an area ability (targeting.area room/room_and_adjacent) needs NO explicit
			// keyword target — its op-list loops over the area set (runOpArea binds each target), so a
			// bare `fireball` with no argument resolves with a nil primary target and still hits the
			// room. A SINGLE-target ability with no match is a clean failure (the unchanged path).
			if def.area != "" {
				return nil, true
			}
			// No explicit target: fall back to the actor's current fighting target (Phase 6) — none
			// this phase, so a missing target is a clean failure.
			return nil, false
		}
		return hits[0], true
	default:
		return nil, true
	}
}

// checkRequires runs lifecycle step 3: the declarative gates. It enforces attribute thresholds and
// tag-CC — does any active affect on the actor `prevents` a tag this ability carries (or a
// requires.not_prevented tag)? The 5.2 preventsAny query is now ENFORCED here. Returns true to proceed;
// on a block it sends the actor a message and returns false. Single-writer: zone goroutine.
func (z *Zone) checkRequires(s *session, def *abilityDef) bool {
	actor := s.entity
	// Cooldown ([G8], Phase 6.3a): refuse an ability still cooling down. This is the step-3 gate the
	// armed cooldown map (armCooldown) backs — today's fires-and-logs became a real gate.
	if def.cooldown > 0 && z.onCooldown(actor, def.ref) {
		z.log.Debug("ability lifecycle: blocked at step 3 (cooldown)", "ability", def.ref, "actor", actor.short)
		s.send(textFrame("That isn't ready yet."))
		return false
	}
	// Tag-CC (§6): the ability's own tags PLUS its requires.not_prevented tags must not be prevented.
	checkTags := def.tags
	if len(def.notPrevented) > 0 {
		checkTags = append(append([]string{}, def.tags...), def.notPrevented...)
	}
	if tag, blocked := preventsAny(actor, checkTags); blocked {
		z.log.Debug("ability lifecycle: blocked at step 3 (tag-CC)",
			"ability", def.ref, "actor", actor.short, "tag", tag)
		s.send(textFrame("You can't do that right now."))
		return false
	}
	// Attribute thresholds.
	for ref, min := range def.reqAttr {
		if attr(actor, ref) < min {
			z.log.Debug("ability lifecycle: blocked at step 3 (attr threshold)",
				"ability", def.ref, "attr", ref, "have", attr(actor, ref), "need", min)
			s.send(textFrame("You are not capable enough to do that."))
			return false
		}
	}
	// Ability OWNERSHIP (Phase 11.4a): an ownership-gated ability (a class feature, a trained skill) is
	// usable only by an actor it was GRANTED to. An un-gated ability (requires_grant false) stays
	// universally usable — the gate is opt-in, so existing content is unchanged.
	if def.requiresGrant && !hasGrantedAbility(actor, def.ref) {
		z.log.Debug("ability lifecycle: blocked at step 3 (not granted)", "ability", def.ref, "actor", actor.short)
		s.send(textFrame("You haven't learned how to do that."))
		return false
	}
	// Profession membership (Phase 13.3): a crafting verb (requires.profession) is usable only by an actor
	// that has LEARNED the trade. Like requiresGrant this is opt-in — "" leaves the ability ungated.
	if def.requiresProfession != "" && !hasProfession(actor, def.requiresProfession) {
		z.log.Debug("ability lifecycle: blocked at step 3 (profession)",
			"ability", def.ref, "profession", def.requiresProfession, "actor", actor.short)
		s.send(textFrame("You lack the training for that craft."))
		return false
	}
	return true
}

// canAffordCosts reports whether the actor has enough of every resource the ability costs (step 5,
// the reserve check). Pure read; the actual pay is payCosts at commit.
func (z *Zone) canAffordCosts(actor *Entity, def *abilityDef) bool {
	for _, cost := range def.costs {
		if resourceCurrent(actor, cost.resource) < cost.amount {
			return false
		}
	}
	return true
}

// payCosts subtracts each cost from the actor's pool (step 7, commit). Called only after
// canAffordCosts passed (step 5) — but it clamps at 0 defensively (setResourceCurrent). Single-writer.
func (z *Zone) payCosts(actor *Entity, def *abilityDef) {
	for _, cost := range def.costs {
		cur := resourceCurrent(actor, cost.resource)
		setResourceCurrent(actor, cost.resource, cur-cost.amount)
		z.log.Debug("ability lifecycle: cost paid", "ability", def.ref,
			"resource", cost.resource, "amount", cost.amount, "from", cur)
	}
}

// armCooldown registers the ability's cooldown lockout ([G8], Phase 6.3a). It records the ELAPSES-AT
// pulse in the actor's per-entity cooldown map (Living.cooldowns) so lifecycle step-3 (checkRequires)
// can refuse the ability while it cools, AND it schedules a pulse callback to CLEAR the entry when the
// cooldown elapses. The map is the source of truth (a logout mid-cooldown serializes the REMAINING from
// it, P6-D8); the callback is the cleanup so a stale entry never lingers. Resolve-by-id: the callback
// re-resolves the player by id and clears only the live entity's map (never a stale cross-zone entity).
func (z *Zone) armCooldown(s *session, def *abilityDef) {
	actor := s.entity
	l := mutableLiving(actor) // COW: fork a proto-aliased actor's Living before mutating its cooldown map (players are prototype==nil; a mob with abilities must not write the proto)
	if l == nil {
		return
	}
	if l.cooldowns == nil {
		l.cooldowns = map[string]uint64{}
	}
	elapsesAt := z.pulses.pulse + pulseCount(def.cooldown)
	l.cooldowns[def.ref] = elapsesAt
	id := s.character
	ref := def.ref
	z.pulses.after(pulseCount(def.cooldown), func(pulse uint64) bool {
		// Resolve-by-id: clear the entry only on the live player. Absent/frozen => the entity left;
		// do not touch it (the destination owns the map after a handoff). A re-armed cooldown (a
		// fresh use during this one) overwrote elapsesAt with a LATER pulse — only clear if this
		// callback's deadline is still the current one, so a re-arm isn't prematurely cleared.
		live, ok := z.players[id]
		if !ok || live == nil || live.entity == nil || live.entity.living == nil || live.frozen {
			return false
		}
		if at, present := live.entity.living.cooldowns[ref]; present && at <= pulse {
			delete(live.entity.living.cooldowns, ref)
			z.log.Debug("ability cooldown elapsed", "ability", ref, "id", id)
		}
		return false
	})
	if z.log.Enabled(context.Background(), slog.LevelDebug) {
		z.log.Debug("ability cooldown armed", "ability", def.ref, "pulses", def.cooldown,
			"id", id, "elapses_at", elapsesAt)
	}
}

// rearmCooldown re-installs a persisted cooldown ([G8] / P6-D8) from its REMAINING pulses on load. It
// is the armCooldown twin for the rehydrate path: it has only a ref + remaining (no def), so it sets
// the elapses-at from NOW + remaining and schedules the same clear callback. Runs on the DESTINATION
// zone goroutine (the entity is live here), so the timer write is single-writer. A remaining <= 0 is a
// no-op (already elapsed). Single-writer: zone goroutine.
func (z *Zone) rearmCooldown(s *session, ref string, remaining int) {
	actor := s.entity
	if actor == nil || actor.living == nil || remaining <= 0 {
		return
	}
	l := mutableLiving(actor) // COW: fork a proto-aliased actor's Living before mutating its cooldown map (rehydrate twin of armCooldown)
	if l.cooldowns == nil {
		l.cooldowns = map[string]uint64{}
	}
	elapsesAt := z.pulses.pulse + uint64(remaining)
	l.cooldowns[ref] = elapsesAt
	id := s.character
	z.pulses.after(uint64(remaining), func(pulse uint64) bool {
		live, ok := z.players[id]
		if !ok || live == nil || live.entity == nil || live.entity.living == nil || live.frozen {
			return false
		}
		if at, present := live.entity.living.cooldowns[ref]; present && at <= pulse {
			delete(live.entity.living.cooldowns, ref)
		}
		return false
	})
}

// onCooldown reports whether ability `ref` is still cooling down on entity e ([G8] step-3 gate). True
// when the actor's cooldown map holds an elapses-at pulse strictly AFTER the current zone pulse. A
// missing entry (never used, or already elapsed-and-cleared) is false. Zone-goroutine read.
func (z *Zone) onCooldown(e *Entity, ref string) bool {
	if e == nil || e.living == nil || e.living.cooldowns == nil {
		return false
	}
	at, ok := e.living.cooldowns[ref]
	return ok && at > z.pulses.pulse
}
