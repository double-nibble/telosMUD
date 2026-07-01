package world

import (
	"sort"
	"time"
)

// corpseLootWindow is how long a fresh corpse is loot-OWNED by its killer: within it, only the killer may
// loot (no ninja-loot / kill-steal by a bystander); after it the corpse decays to a free-for-all. A mob
// killed by another mob (no player killer) has no owner and is free-for-all from the start.
const corpseLootWindow = 60 * time.Second

// CorpseOwner marks a corpse with a killer loot-ownership window (security: anti-ninja-loot). until is when
// the window lapses. ownerPID is the killer's durable PersistID when it was set at kill time — the STABLE
// key that survives a name being freed + reclaimed by a different character inside the window (docs/
// REMAINING.md §1); owner is the killer's character name, the fallback key used when either side lacks a
// PID (the async-create window, or a mob kill). A corpse with no CorpseOwner (or a lapsed one) is lootable
// by anyone. Checked at the get-from-container gate (container.go). Zone-goroutine-owned.
type CorpseOwner struct {
	owner    string
	ownerPID string
	until    time.Time
}

func (*CorpseOwner) componentKind() Kind { return KindCorpseOwner }

// owned reports whether the corpse actually has a loot owner (so an empty marker never gates everyone out).
func (co *CorpseOwner) owned() bool { return co.owner != "" || co.ownerPID != "" }

// looterIsOwner reports whether s is the killer who owns this corpse. It prefers the durable PersistID
// (immune to name reuse within the window) and falls back to the character name when either side has no PID.
func (co *CorpseOwner) looterIsOwner(s *session) bool {
	if co.ownerPID != "" && s.entity != nil && s.entity.pid != nil {
		return co.ownerPID == string(*s.entity.pid)
	}
	return co.owner != "" && co.owner == s.character
}

// death.go is the DEATH -> CORPSE -> OnKill machinery + the THREAT list and AGGRO initiation
// (docs/COMBAT.md §7, docs/PHASE6-PLAN.md §1.3 / §1.3.1 [G-D]). Everything here runs ON the zone
// goroutine (single-writer) — it is triggered BY harm that already gated, drops combat pointers, removes
// the dead mob, and builds a corpse, all in-zone.
//
// # The seam (6.5 uniform death)
//
// onVitalDepleted is invoked from the ONE shared dealDamage funnel (effect_op.go) whenever applied
// damage empties a target's vital pool — a melee swing, an offensive spell, an AoE, a DoT tick, and an
// opportunity attack ALL kill through this one seam (a pure-caster build can land a killing blow). It
// runs the content on_depleted op-list (if authored), RE-CHECKS the vital (a hook that revived the
// victim CANCELS the death), then die(). Death is the consequence of harm that brings hp to 0; the harm
// path owns the swing/spell, this file owns the consequence. No damage-pipeline shape changed — death is
// the depletion branch of the shared funnel, gated by the posDead idempotency latch.
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
// guardHarmful (the shared dealDamage). The on_depleted op-list and the OnKill handlers are ordinary
// ops/handlers — a harmful op in either still funnels the SAME guardHarmful (effect_op.go). Corpse
// creation is pure containment (Move), no harm. So the gate's can't-bypass property holds unchanged.

// onVitalDepleted is the UNIFORM death entry (6.5) the shared dealDamage funnel calls when `victim`'s
// vital pool hits 0 — a melee swing, a spell, an AoE, a DoT tick, and an opportunity attack ALL reach it
// through the one harm path (effect_op.go), not just the swing pipeline. `killer` is the entity the
// emptying damage is attributed to (the swing attacker / a DoT's applier; nil for a sourceless
// environmental hit / a self-DoT). It runs the content on_depleted hook (a last-gasp narration / effect
// on the dying entity), RE-CHECKS the vital, then die(). Idempotent: a victim already dead (posDead) is a
// no-op, so a second swing in the same round or a DoT tick racing the killing blow can't double-kill.
// Single-writer: zone goroutine.
//
// CANCELLABLE DEATH CHECKPOINT (6.5, user-mandated): the on_depleted hook IS the declarative cancel.
// After it runs we re-read the vital; if the hook revived the victim above 0 (a death-ward that did
// `modify_resource hp +1` and `apply_affect rooted`, or a "second wind" heal), we ABORT — no die(), no
// corpse, no respawn, posDead stays unset. No imperative "cancel" signal: the re-check is the mechanism,
// the engine supplies nothing but the seam, the content supplies whether death sticks. The hook runs
// BEFORE die() drops combat / builds the corpse, so a death narration (and a revive) sees the entity
// still in the room. The ops are ordinary (any harmful op funnels guardHarmful).
//
// RECURSION BOUND (security M1, the stack-overflow guard): the death hook is NOT reached through
// fireEvent, so it does NOT get the bus's depth/width bounding for free — yet it can recurse, because an
// on_depleted op-list may re-deal LETHAL damage (`deal_damage self`, or a `tgt: other` ping-pong between
// two entities), and dealDamage's depletion checkpoint calls back into onVitalDepleted. Left unbounded
// that is `dealDamage -> onVitalDepleted -> runOps -> opDealDamage -> dealDamage -> …` forever -> a
// process-fatal `fatal error: stack overflow` that takes down every zone, not just the fight. So we
// bound the hook EXACTLY as fireEvent bounds a handler (event.go is the template):
//   - The deathCtx inherits parent.depth. Refuse to run the hook once depth >= maxEventDepth (Warn, then
//     fall through to die() WITHOUT the hook) — this is the critical cap that terminates the recursion at
//     maxEventDepth=8. depth ALONE bounds it: a command-issued cast reaches here with a nil eventBudget,
//     so we must not rely on the budget pointer being non-nil.
//   - When a shared eventBudget exists, the hook is one unit of work: skip it (truncate) if the budget is
//     spent, else decrement it — so a wide cascade of death hooks can't starve the goroutine either.
//   - Run the ops at depth+1 (dc.depth++), so the nested dealDamage->onVitalDepleted sees an incremented
//     depth and the recursion provably terminates at the cap.
//
// nil/empty op-list => nothing runs (engine default death). A hook that exhausts the depth/width budget
// simply does not run — the victim then dies the engine-default way (no revive, since the cancel can only
// happen if the hook actually ran and raised the vital).
func (z *Zone) onVitalDepleted(victim, killer *Entity, parent *effectCtx) {
	if victim == nil || victim.living == nil || position(victim) == posDead {
		return
	}
	if pool := vitalResource(victim); pool != "" {
		if def := z.resourceDefs().get(pool); def != nil && len(def.onDepleted) > 0 {
			dc := z.deathCtx(victim, killer, parent)
			z.runDeathHook(dc, def.onDepleted, victim)
		}
	}
	// THE CANCEL RE-CHECK: after any on_depleted hook ran, re-read the vital. A hook that revived the
	// victim above 0 (death-ward / second wind) cancels the death declaratively — abort cleanly with no
	// die/corpse/respawn and the posDead latch left unset (the victim fights on at its revived hp). A
	// vital-less victim reads the 1<<30 sentinel (vitalCurrent) and so is never "depleted" here either.
	if vitalCurrent(victim) > 0 {
		return
	}
	z.die(victim, killer, parent)
}

// runDeathHook runs an on_depleted op-list under the SAME depth/width bound fireEvent applies to an event
// handler (event.go), so a hook that re-deals lethal damage recurses at most maxEventDepth deep instead
// of overflowing the stack (security M1). dc inherits parent.depth/eventBudget (deathCtx); this is the
// one place the death seam decrements the budget and increments the depth. A refusal (depth cap hit, or
// budget spent) runs NO ops — the caller then proceeds to the engine-default death. Single-writer.
func (z *Zone) runDeathHook(dc *effectCtx, ops []effectOp, victim *Entity) {
	if dc.depth >= maxEventDepth {
		// The stack-overflow guard: a runaway on_depleted (re-deals lethal damage) is capped here exactly
		// like a runaway event cascade. The victim falls through to die() with the hook UNrun.
		z.log.Warn("on_depleted depth budget exhausted; death hook dropped",
			"victim", targetShort(victim), "depth", dc.depth)
		return
	}
	if dc.eventBudget != nil {
		if *dc.eventBudget <= 0 {
			z.log.Warn("on_depleted handler budget exhausted; death hook dropped",
				"victim", targetShort(victim))
			return
		}
		*dc.eventBudget--
	}
	dc.depth++ // the nested dealDamage->onVitalDepleted->runDeathHook sees +1, so recursion terminates at the cap
	runOps(dc, ops)
}

// deathCtx builds the effectCtx the on_depleted / death-adjacent ops run under: the victim is the
// actor/source/target (a self-effect on the dying entity), the killer is `other` (so a hook op can
// reference $other = the slayer). It inherits the parent's depth/rng/event-budget so the death hook is
// bounded by the SAME round budget as the swing that triggered it (no fresh 256). disp neutral — a
// harmful op inside re-decides its own disposition and re-gates.
func (z *Zone) deathCtx(victim, killer *Entity, parent *effectCtx) *effectCtx {
	c := &effectCtx{
		z: z, actor: victim, source: victim, target: victim, other: killer,
		mag: 1, disp: dispNeutral,
	}
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

	// --- Loot (Phase 12.1): run the victim's loot table per eligible looter BEFORE the threat table is
	// scrubbed (it is the eligibility source — who dealt damage). Personal loot delivers directly to each
	// player; the corpse (below) holds only the body. A mob with no loot table is a clean no-op. The roll
	// source is the cascade's rng (deterministic in a test) else the zone's seeded Lua rng. -----------
	lootRNG := z.lua.rng
	if parent != nil && parent.rng != nil {
		lootRNG = parent.rng
	}
	z.resolveLoot(victim, lootRNG)

	// --- Drop ALL combat pointers to/from the victim (the no-stale-pointer invariant). disengage drops
	// every opponent in the room whose `fighting` points at the victim AND the victim's own fighting/
	// position — reusing the exact 6.3a discipline so no dangling pointer survives. A surviving attacker
	// left with no target falls back to standing; 6.3b's threat re-target (retargetMob) then picks its
	// next foe next round. -------------------------------------------------------------------------
	z.disengage(victim)
	z.scrubThreat(victim)

	// Latch posDead — the REAL idempotency guard onVitalDepleted checks (disengage reset position to
	// standing just above, so set it here). As of 6.5 death is reached from ANY deal_damage (swing, spell,
	// AoE, DoT tick, opportunity attack), so a second depletion on an already-dying entity is a live race —
	// a DoT tick landing the same round as the killing swing, or an OnDamageTaken handler's deal_damage
	// re-emptying the same victim. This latch is what stops a double OnKill / a duplicate corpse / a second
	// respawn. respawnPlayer below clears it back to standing on revive. (distsys review S1.)
	setPosition(victim, posDead)

	// Lua `death` trigger (7.4c): fired here, BEFORE the corpse/reap removes the victim from the
	// world tree, so the handler still sees the entity in-room (a death cry, a dropped quest flag).
	// nil-safe / no-op when the victim carries no script. A player respawns (its script, if any,
	// persists — it is not extracted), so this fires for a scripted mob's death.
	z.fireDeath(victim, killer)

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
// makeCorpse builds the victim's corpse, moves their carried items into it, and stamps a killer
// loot-ownership window (CorpseOwner) so a bystander can't ninja-loot / kill-steal a fresh kill (the gate is
// in container.go getFrom). Single-writer: zone goroutine.
func (z *Zone) makeCorpse(victim, killer *Entity) {
	room := victim.location
	corpse := z.newCorpse(victim)

	// Loot-ownership window (anti-ninja-loot): if a PLAYER landed the kill, only they may loot the corpse
	// until the window lapses. A mob-on-mob kill (no player killer) leaves it a free-for-all from the start.
	if killer != nil {
		if ks, ok := sessionOf(killer); ok && ks.character != "" {
			co := &CorpseOwner{owner: ks.character, until: time.Now().Add(corpseLootWindow)}
			if killer.pid != nil {
				co.ownerPID = string(*killer.pid) // stable key; survives the name being reclaimed in-window
			}
			Add(corpse, co)
		}
	}

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
	// The victim has left the world tree for good — drop its per-instance Lua trigger state so a
	// repop-on-timer zone doesn't leak an entityScript per dead scripted mob (7.4c review MUST-FIX),
	// and decrement the live Lua-spawn census so the per-zone spawn cap bounds the LIVE population
	// (7.5). 7.6 will flush self.state to JSONB immediately before the script drop. nil-safe.
	if z.lua != nil {
		z.lua.dropEntityScript(victim.rid)
		z.lua.dropLuaSpawn(victim.rid)
	}
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
	l := mutableLiving(victim) // COW: fork a proto-aliased mob's Living before mutating its threat table (else threat keys leak to the proto + siblings)
	if l.threat == nil {
		l.threat = map[*Entity]float64{}
	}
	l.threat[attacker] += amount
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
	// Clear the dead entity's own table only if it actually has one. A proto-aliased mob that never
	// accrued threat aliases the prototype's (nil) threat map; skipping the write avoids forking its
	// Living just to set nil->nil (and avoids a write-through to the shared proto). A mob that DID accrue
	// threat already COW'd its Living in addThreat, so this clears the instance-owned table.
	if dead.living != nil && dead.living.threat != nil {
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
	mutableLiving(mob).fighting = next // COW: fork a proto-aliased mob's Living before re-pointing its fighting target
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
