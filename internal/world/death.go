package world

import "sort"

// death.go is the DEATH -> CORPSE -> OnKill machinery + the THREAT list and AGGRO initiation
// (docs/COMBAT.md §7, docs/PHASE6-PLAN.md §1.3 / §1.3.1 [G-D]). It is the 6.3b half of combat: 6.3a
// reserved a seam in applySwingDamage (the "target vital depleted" log); this file wires that seam to
// die(). Everything here runs ON the zone goroutine (single-writer) — it is triggered BY a swing that
// already gated, drops combat pointers, removes the dead mob, and builds a corpse, all in-zone.
//
// # The seam (6.3b boundary)
//
// 6.3a's swing pipeline lands gated damage and, when a target's vital pool hits 0, LOGGED it and left
// the mob standing. 6.3b replaces that log with onVitalDepleted(victim, killer): run the content
// on_depleted op-list (if authored), then die(). 6.3a owns the swing; 6.3b owns the consequence of a
// swing that brings hp to 0. No swing-pipeline shape changed — only the depletion branch.
//
// # The no-stale-pointer invariant (the death twin of disengage)
//
// A dead entity must leave NO live *Entity referencing it: not a `fighting` pointer, not a threat-table
// key. die() drops every fighting link to the victim (reusing the 6.3a stopFight/disengage discipline)
// AND scrubs the victim from every other combatant's threat table in the room, BEFORE the mob entity is
// removed from the world tree. So the round driver re-gathers next pulse and never derefs a corpse.
//
// # Harm gating
//
// The death path itself introduces NO new harm vector: it is TRIGGERED by harm that already funneled
// guardHarmful (the swing's dealDamage). The on_depleted op-list and the OnKill handlers are ordinary
// ops/handlers — a harmful op in either still funnels the SAME guardHarmful (effect_op.go). Corpse
// creation is pure containment (Move), no harm. So the gate's can't-bypass property holds unchanged.

// onVitalDepleted is the 6.3b death entry the swing pipeline calls when `victim`'s vital pool hits 0
// (combat.go applySwingDamage). `killer` is the entity whose swing/effect emptied the pool (may be nil
// for a non-combat depletion — a DoT whose source already left). It runs the content on_depleted hook
// (a last-gasp narration / effect on the dying entity), then die(). Idempotent: a victim already dead
// (posDead) is a no-op, so a second swing in the same round (or a DoT tick racing the killing blow)
// can't double-kill. Single-writer: zone goroutine.
func (z *Zone) onVitalDepleted(victim, killer *Entity, parent *effectCtx) {
	if victim == nil || victim.living == nil || position(victim) == posDead {
		return
	}
	// Run the content on_depleted op-list on the DYING entity (victim as $actor/$target) BEFORE die()
	// drops combat / builds the corpse — so a death narration sees the entity still in the room. The
	// ops are ordinary (any harmful op funnels guardHarmful); they share the round's event budget so a
	// death hook can't spin the goroutine. nil/empty => nothing runs (engine default death).
	if pool := vitalResource(victim); pool != "" {
		if def := z.resourceDefs().get(pool); def != nil && len(def.onDepleted) > 0 {
			dc := z.deathCtx(victim, killer, parent)
			runOps(dc, def.onDepleted)
			// The hook may have healed the victim back above 0 (a content "second wind"): re-check and
			// abort the death if so, so on_depleted can genuinely prevent death as pure content.
			if vitalCurrent(victim) > 0 {
				return
			}
		}
	}
	z.die(victim, killer, parent)
}

// deathCtx builds the effectCtx the on_depleted / death-adjacent ops run under: the victim is the
// actor/source/target (a self-effect on the dying entity), the killer is `other` (so a hook op can
// reference $other = the slayer). It inherits the parent's depth/rng/event-budget so the death hook is
// bounded by the SAME round budget as the swing that triggered it (no fresh 256). disp neutral — a
// harmful op inside re-decides its own disposition and re-gates.
func (z *Zone) deathCtx(victim, killer *Entity, parent *effectCtx) *effectCtx {
	c := &effectCtx{z: z, actor: victim, source: victim, target: victim, other: killer,
		mag: 1, disp: dispNeutral}
	if parent != nil {
		c.rng, c.depth, c.eventBudget = parent.rng, parent.depth, parent.eventBudget
	}
	return c
}

// die runs the full death of `victim` slain by `killer`: fire OnKill, drop ALL combat pointers to/from
// the victim, scrub it from every threat table, then dispose of the body — a MOB becomes a CORPSE
// container holding its gear+inventory and the entity is removed; a PLAYER respawns at the start room
// in a living position (never left wedged dead). Single-writer: zone goroutine.
//
// Order matters: OnKill fires FIRST (subject=killer, other=victim) while the victim is still in the
// room and its threat/inventory are intact, so an XP/quest handler (content, Phase 11) sees the kill
// in context. Then combat is dropped and the body disposed.
func (z *Zone) die(victim, killer *Entity, parent *effectCtx) {
	if victim == nil || victim.living == nil {
		return
	}
	z.act("$n is DEAD!", victim, nil, nil, "", "", ToRoom)

	// --- OnKill (the reserved combat event, now LIT). subject=KILLER (the event is ABOUT the slayer —
	// it awards the slayer XP/quest credit), other=VICTIM. mag = the victim's max hp (a sensible "size of
	// the kill" the content XP formula can scale by; 1 when no vital max). The content handler is Phase 11
	// — here we just light the fire point, depth+width guarded like OnHit/OnDamageTaken. A nil killer (a
	// non-combat death) fires nothing (no subject).
	// TODO(phase11, security review): mag = raw victim max-hp is BUILDER-INFLUENCEABLE — a high-max-hp
	// mob becomes a farm target. The Phase-11 XP/honor formula must cap/normalize the kill magnitude
	// (or read a content-defined xp_value attribute), not trust raw max-hp. ------------------------
	if killer != nil {
		mag := 1.0
		if pool := vitalResource(victim); pool != "" {
			if m := resourceMax(victim, pool); m > 0 {
				mag = float64(m)
			}
		}
		z.fireEvent(parent, evOnKill, killer, victim, mag)
	}

	// --- Drop ALL combat pointers to/from the victim (the no-stale-pointer invariant). disengage drops
	// every opponent in the room whose `fighting` points at the victim AND the victim's own fighting/
	// position — reusing the exact 6.3a discipline so no dangling pointer survives. A surviving attacker
	// left with no target falls back to standing; 6.3b's threat re-target (retargetMob) then picks its
	// next foe next round. -------------------------------------------------------------------------
	z.disengage(victim)
	z.scrubThreat(victim)

	// Latch posDead — the REAL idempotency guard onVitalDepleted checks (disengage reset position to
	// standing just above, so set it here). Today death is only reached from the swing path, where
	// Move(victim,nil) + the swing's location-mismatch gate already prevent re-entry; but the moment a
	// non-swing depletion source (a DoT/affect tick, an environmental deal_damage, a future direct op)
	// calls onVitalDepleted on an already-dying entity, this latch is what stops a double OnKill / a
	// duplicate corpse. respawnPlayer below clears it back to standing on revive. (distsys review S1.)
	setPosition(victim, posDead)

	if isPlayer(victim) {
		z.respawnPlayer(victim)
		return
	}
	z.makeCorpse(victim, killer)
}

// makeCorpse turns a dead MOB into a CORPSE: an engine-built container entity placed in the victim's
// room holding everything the mob carried (inventory) and wore (equipment), then the mob entity is
// removed from the world tree. The corpse is an ENGINE container (not a content prototype) because it
// is a runtime artifact of a specific kill — its name is derived from the victim ("the corpse of a
// small goblin") and its contents are the victim's instance items, neither of which a static prototype
// can carry. The engine NAMES no mob/loot — the corpse merely inherits whatever CONTENT the mob held
// (P6-D6). Loot TABLES / affix rolls are Phase 11/12 (reserved): for now the corpse holds the carried
// items only.
// TODO(phase11, security review): the corpse is currently an UNOWNED free-for-all — any player in the
// room can `get all corpse` regardless of who landed the kill (ninja-loot / kill-steal). Low-impact
// today (mob loot only; player death drops NO gear), but BEFORE any drop-on-death loot ruleset ships,
// add a killer/threat-derived loot-ownership window. The hook exists: makeCorpse already records the
// killer. Single-writer: zone goroutine.
func (z *Zone) makeCorpse(victim, killer *Entity) {
	room := victim.location
	corpse := z.newCorpse(victim)

	// Move every item the victim carried into the corpse. Worn items live in the victim's contents too
	// (equipped is a STATE over a carried item, container.go), so a single contents walk captures BOTH
	// inventory and equipment — clearing the Wearer slots is unnecessary (the wearer is being removed).
	// Snapshot the slice first: Move mutates victim.contents underneath the loop.
	carried := make([]*Entity, len(victim.contents))
	copy(carried, victim.contents)
	for _, item := range carried {
		Move(item, corpse)
	}

	// Place the corpse where the mob fell, then remove the mob from the world tree. After Move(victim,
	// nil) no container references the victim; combat/threat were already scrubbed in die(), so the
	// *Entity is now unreferenced and GC-eligible. Repop (reset.go) re-spawns the mob on the zone's
	// reset cadence — the corpse is a separate, decaying artifact (decay is a later ruleset knob).
	if room != nil {
		Move(corpse, room)
	}
	z.log.Debug("mob died -> corpse", "mob", victim.short, "rid", victim.rid,
		"corpse_items", len(carried), "killer", targetShort(killer))
	Move(victim, nil)
}

// newCorpse builds the engine corpse container for a dead victim: a fresh non-prototype entity with a
// Container component (so get/put/look work on it exactly like the demo chest) and a name/long derived
// from the victim. Keywords include "corpse" so `get all corpse` / `look corpse` resolve it. The
// container is OPEN (closed=false) and unbounded (capacity 0) — you can loot it immediately. It is a
// plain item entity (no Living), so it is never a combat target and never ticks.
func (z *Zone) newCorpse(victim *Entity) *Entity {
	corpse := z.newEntity(ProtoRef("corpse"))
	name := "the corpse of " + victim.Name()
	corpse.setShort(name)
	corpse.setLong(name + " lies here.")
	corpse.setKeywords([]string{"corpse", "remains"})
	Add(corpse, &Container{capacity: 0, closed: false})
	return corpse
}

// respawnPlayer is the minimal player death recovery (docs/PHASE6-PLAN.md §1.3b): a player slain in
// combat is moved to the start/recall room and restored to a LIVING, full-vital standing position, so
// they are NEVER left wedged dead. This is intentionally minimal — the full corpse-retrieval / XP-loss
// ruleset is a later content/ruleset knob (a player corpse + drop-on-death is a Phase 11/ruleset
// decision, not wired here). The player keeps their gear (no drop) this slice. Single-writer.
func (z *Zone) respawnPlayer(victim *Entity) {
	// Restore vitals to full (re-set each vital resource current to its derived max) so the player is
	// alive again. resourceCurrent already clamps; setting to the max is the "fully healed on respawn"
	// minimal rule.
	for ref, def := range z.resourceDefs().table() {
		if def != nil && def.vital {
			setResourceCurrent(victim, ref, resourceMax(victim, ref))
		}
	}
	setPosition(victim, posStanding)
	if start := z.resolveRoom(z.startRoom); start != nil && start != victim.location {
		Move(victim, start)
	}
	if s, ok := sessionOf(victim); ok {
		s.send(textFrame("You have been slain! You awaken at the temple, alive but shaken."))
		z.lookRoom(s)
	}
	z.log.Debug("player died -> respawned", "player", victim.short, "room", targetShort(victim.location))
}

// --- Threat list (death.go) -------------------------------------------------------------------

// addThreat accumulates `amount` threat for `attacker` on `victim`'s threat table (death.go). A swing
// that lands damage adds the damage as threat; a heal on an ally adds weighted threat to the healer (a
// later content hook — this slice wires the damage source). Threat keys a LIVE *Entity; die()/disengage
// scrub a dead/departed key so no stale pointer survives. A non-positive amount is a no-op. Only a
// LIVING victim accrues threat (an item has none). Single-writer: zone goroutine.
func addThreat(victim, attacker *Entity, amount float64) {
	if victim == nil || victim.living == nil || attacker == nil || amount <= 0 {
		return
	}
	if victim.living.threat == nil {
		victim.living.threat = map[*Entity]float64{}
	}
	victim.living.threat[attacker] += amount
}

// scrubThreat removes `dead` from EVERY other combatant's threat table in its room, AND clears the
// dead entity's own table — so a killed entity leaves no threat-key *Entity anywhere. Called by die()
// before the body is removed (the threat twin of disengage). O(room population). Single-writer.
func (z *Zone) scrubThreat(dead *Entity) {
	if dead == nil {
		return
	}
	if dead.location != nil {
		for _, other := range dead.location.contents {
			if other != dead && other.living != nil && other.living.threat != nil {
				delete(other.living.threat, dead)
			}
		}
	}
	if dead.living != nil {
		dead.living.threat = nil
	}
}

// topThreat returns the live, in-room attacker with the highest accumulated threat on `mob`'s table —
// the foe a mob should swing at this round. Ties break DETERMINISTICALLY by the attacker's short name
// (Go map iteration is randomized; a mob's target must be reproducible for the test-engineer's replay
// harness). A threat entry whose entity left the room / died (scrubbed, but defensively re-checked) is
// skipped. Returns nil when the mob has no live threatening foe in its room. Zone-goroutine read.
func topThreat(mob *Entity) *Entity {
	if mob == nil || mob.living == nil || len(mob.living.threat) == 0 {
		return nil
	}
	type cand struct {
		e *Entity
		v float64
	}
	var cands []cand
	for e, v := range mob.living.threat {
		if e == nil || e.living == nil || e.location != mob.location {
			continue // departed/dead/cross-room: not a valid target
		}
		if position(e) == posDead {
			continue
		}
		cands = append(cands, cand{e, v})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].v != cands[j].v {
			return cands[i].v > cands[j].v // highest threat first
		}
		return cands[i].e.Name() < cands[j].e.Name() // stable tie-break
	})
	return cands[0].e
}

// retargetMob re-points a fighting MOB at its highest-threat live foe (death.go). Called each round
// before the mob swings so killing/fleeing its current target makes it turn to the next threat rather
// than idle or swing at a departed entity. A mob with no live threat in the room is stopFought (it has
// nothing to fight). A no-op for a player (player targets are chosen by the player's commands, not
// threat). Single-writer: zone goroutine.
func (z *Zone) retargetMob(mob *Entity) {
	if mob == nil || mob.living == nil || isPlayer(mob) {
		return
	}
	cur := mob.living.fighting
	// Keep the current target if it is still a live, in-room foe (don't thrash targets every round just
	// because threat shifted by a point — only re-target when the current target is gone).
	if cur != nil && cur.living != nil && cur.location == mob.location && position(cur) != posDead {
		return
	}
	next := topThreat(mob)
	if next == nil {
		z.stopFight(mob)
		return
	}
	mob.living.fighting = next
	setPosition(mob, posFighting)
}

// --- Aggro (death.go) -------------------------------------------------------------------------

// aggroOnEntry lets AGGRESSIVE mobs in `room` initiate combat on `mover` who just entered (docs/
// PHASE6-PLAN.md §1.3 — "aggressive mobs initiate on entry"). "Aggressive" is a CONTENT number, not an
// engine flag: a mob with an `aggressive` attribute > 0 (a base override on its prototype's Living)
// attacks an eligible entrant. Only a player entrant is auto-aggro'd (a mob wandering past another mob
// doesn't start a brawl this slice); the PvP gate is irrelevant (mob->player harm is always allowed).
// A mob already fighting keeps its target. Called from movement after a player arrives. Single-writer.
func (z *Zone) aggroOnEntry(mover *Entity, room *Entity) {
	if mover == nil || mover.living == nil || room == nil || !isPlayer(mover) {
		return
	}
	if position(mover) == posDead {
		return
	}
	for _, occ := range room.contents {
		if occ == mover || occ.living == nil || isPlayer(occ) {
			continue
		}
		if occ.living.fighting != nil {
			continue // already engaged
		}
		if attr(occ, "aggressive") <= 0 {
			continue
		}
		if z.startFight(occ, mover) {
			z.act("$n snarls and attacks you!", occ, nil, mover, "", "", ToVictim)
			z.act("$n snarls and attacks $N!", occ, nil, mover, "", "", ToRoom)
			z.log.Debug("aggressive mob initiated", "mob", occ.short, "target", mover.short)
		}
	}
}
