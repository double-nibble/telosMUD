package world

import (
	"math/rand"
	"sort"
)

// combat.go is the COMBAT ROUND DRIVER + the SWING PIPELINE (docs/COMBAT.md §1-3, docs/PHASE6-PLAN.md
// §1.3, slice 6.3a). It is the SECOND real registrant on the per-zone pulse (after the affect tick),
// and it runs ENTIRELY on the zone goroutine (single-writer) — every entity it touches is one this
// zone owns, re-resolved each round, never a stale cross-zone *Entity.
//
// # The no-fighting-pointer-crosses-a-zone invariant (ENFORCED, not assumed)
//
// The driver's safety rests on this: no `Living.fighting` pointer and no posFighting state ever crosses
// a zone boundary, so a swing's same-room gate never derefs an entity another goroutine owns. It is
// ENFORCED at THREE points: (1) move() REFUSES to walk while posFighting (commands.go — `flee` first);
// (2) transferOut + the cross-shard freeze call disengage(mover) before the entity leaves the room, so
// even a future bug that let a fighting player move drops every fighting link first; (3) transferIn
// clears fighting/position on the destination as belt-and-suspenders. Combat is TRANSIENT — it is never
// persisted and never handed off (P6-D8); a moved player re-engages with a fresh `kill`.
//
// # The shape (engine) vs the numbers (content) — the pillar
//
// The engine owns the FRAMEWORK: the round timer, the per-round iteration over Fighting entities, the
// fixed pipeline order (gates -> to-hit -> avoidance ladder -> damage -> soak -> apply -> OnHit). The
// NUMBERS — to-hit curve, the avoidance checks, attacks/round, crit, soak-by-type — are CONTENT: the
// to-hit is a content `check` spec (check.go), avoidance is zero-or-more content `check` specs, attacks
// is a content attribute, damage is a content deal_damage op with a [G-A] scoped formula. NO engine
// code here names a class/weapon/mob/condition (P6-D6). A pack with no combat content degenerates
// cleanly (no to-hit classifier -> the swing always lands and flows to soak/mitigate).
//
// # The round driver (one per zone)
//
// PULSE_VIOLENCE pulses apart, runCombatRound walks z.players-and-mobs in the Fighting state and
// resolves each one's swings. The driver is a SINGLE per-zone pulse callback (not one-per-combatant) so
// the iteration ORDER is centralized — the [G-G] simultaneous-default seam: the order is a stable sort
// by a content-overridable `combat_order` attribute (default 0 => stable insertion-ish order over a
// sorted key), NOT initiative. 5e initiative is an OPTIONAL content check at combat-start that writes
// combat_order; the driver just sorts by it.

// PULSE_VIOLENCE is the combat round length in base pulses (pulse.go pulseInterval). Diku's violence
// pulse is ~2.4s; at the 250ms base pulse that is 10 pulses. It is the only combat-timing knob — every
// fight in a zone resolves on this stride, hung off the zone's own heartbeat (no global lockstep). A
// content ruleset that wants faster/slower rounds tunes this multiple (a future per-pack override is a
// reserved seam; the constant is the engine default).
const PULSE_VIOLENCE uint64 = 10

// maxSwingsPerRound caps the swings one entity resolves per round, so a runaway `attacks` attribute (a
// content bug or a haste stack gone wrong) can never spin the single-writer zone goroutine. Far past any
// sane attacks/round; mirrors the maxDice spirit (a hard ceiling on per-tick work).
const maxSwingsPerRound = 50

// combatProfile is an entity's parsed combat data (built from content, content_map.go): the to-hit
// check the entity rolls when it ATTACKS, and the ordered avoidance ladder it rolls when it DEFENDS.
// Immutable after build (shared from the prototype by reference); the per-roll randomness is the ctx
// rng. A nil toHit means "no to-hit classifier" (the swing auto-lands); a nil/empty avoidance ladder
// means "no avoidance" (5e/WoW — straight to soak), exactly the [G-F] optional-ladder requirement.
type combatProfile struct {
	// toHit is the attacker's to-hit check ([G-F] the SOLE classifier when no avoidance is authored). Its
	// bands classify hit/miss/crit; the band ops are run by the pipeline (crit scales damage via [G-A]).
	// Resolved with the attacker as $actor and the defender as $target, so the spec reads
	// `$actor.accuracy` vs `$target.evasion`/`$target.ac`. nil => the swing auto-hits (degenerate case).
	toHit *checkSpec
	// avoidance is the DEFENDER's ordered avoidance ladder ([G-F]): zero-or-more content checks (dodge,
	// parry, block, ...) run IN ORDER after a hit; the FIRST success negates the swing. The pipeline does
	// NOT hardcode the set or order — it runs whatever the content authored (empty => none). Avoidance
	// GATING (parry needs a weapon) is content: the check's roll-under threshold derives to 0 from the
	// missing gear, so the check auto-fails (no engine predicate, [G-C]). Each carries its own label for
	// the combat message ("dodge"/"parry"/"block").
	avoidance []*checkSpec
	// damageBonus is the [G-A] scoped damage formula added to a swing's weapon dice (`$actor.str_bonus +
	// $actor.damroll`). Built once from content (content_map.go), evaluated by opDealDamage scoped to the
	// attacker ($actor). nil => raw weapon dice only. This is what lets a sword add STR as CONTENT (no Lua).
	damageBonus formulaNode
}

// startFight puts `attacker` into combat with `target` and, classically, makes the target RETALIATE
// (enter combat back against the attacker) if it is not already fighting — so a `kill` starts a real
// two-sided fight. It sets both positions to posFighting and arms the zone's round driver if it is not
// already running. Single-writer: zone goroutine. A no-op if either side is not a living, in-room
// entity, or they are not in the same room (gate-0: combat is same-room only).
func (z *Zone) startFight(attacker, target *Entity) bool {
	if attacker == nil || target == nil || attacker.living == nil || target.living == nil {
		return false
	}
	if attacker == target {
		return false
	}
	if attacker.location == nil || attacker.location != target.location {
		return false
	}
	if position(target) == posDead {
		return false
	}
	attacker.living.fighting = target
	setPosition(attacker, posFighting)
	// Retaliation: an unengaged target turns and fights its attacker (classic auto-retaliate). A target
	// already fighting someone else keeps its current target (threat/assist is 6.3b).
	if target.living.fighting == nil {
		target.living.fighting = attacker
		setPosition(target, posFighting)
	}
	z.ensureCombatRound()
	return true
}

// stopFight removes `e` from combat: clears its fighting target and drops it back to standing (unless
// dead — that position is owned by the 6.3b death path). It does NOT force the former opponent out of
// combat (it may still be fighting others / want to keep swinging at a corpse-to-be). The round driver
// self-cancels when no Fighting entities remain (ensureCombatRound). Single-writer: zone goroutine.
func (z *Zone) stopFight(e *Entity) {
	if e == nil || e.living == nil {
		return
	}
	e.living.fighting = nil
	if position(e) == posFighting {
		setPosition(e, posStanding)
	}
}

// disengage FULLY removes `e` from this zone's combat — both directions. It stopFights `e` (clears its
// own fighting/position) AND drops the fighting link of every OTHER fighter in e's room that was
// targeting `e`, so no `fighting` pointer to `e` survives. This is the boundary-crossing safety net: a
// player leaving the zone (transferOut / cross-shard freeze) MUST leave no fighting pointer or
// posFighting that could cross to another zone goroutine, and MUST not strand an opponent posFighting at
// a now-departed target (no player in that mob's room => the driver never re-gathers it). Called on the
// SOURCE goroutine before the entity is handed off, while e is still this zone's to write. Single-writer.
func (z *Zone) disengage(e *Entity) {
	if e == nil || e.living == nil {
		return
	}
	// Drop any opponent in the room whose fighting link points at the departing entity, BEFORE clearing
	// e's own — so an opponent left with no live target falls back to standing rather than swinging at a
	// pointer that's about to cross a zone boundary. (6.3b's threat list would re-target; here it stops.)
	if e.location != nil {
		for _, other := range e.location.contents {
			if other != e && other.living != nil && other.living.fighting == e {
				z.stopFight(other)
			}
		}
	}
	z.stopFight(e)
}

// fireLeaveRoom is the OnLeaveRoom CHECKPOINT ([G9], docs/PHASE6-PLAN.md §1.3.1 / §4 slice 6.4): it
// fires the in-zone OnLeaveRoom event when `leaver` is about to depart its room, so an ENGAGED foe can
// react with a declarative opportunity attack. It MUST be called BEFORE the leaver is detached from the
// room (Move(e, nil/to)) — while both the leaver and every reactor are still live and in-room — so the
// granted attack's guardHarmful sees a valid actor+target (the fail-closed-on-detached funnel, event.go).
//
// The SUBJECT of each fire is the REACTOR (a foe in the room whose fighting link points at the leaver),
// with `other` = the leaver. The bus gathers handlers from the subject, so the reactor's own
// subscription (a `reactions` resource's on_event[OnLeaveRoom]) runs and its ops bind the leaver via
// `target: other`. We fire about EACH engaged reactor independently, so a leaver mobbed by two foes can
// provoke two opportunity attacks (each spends its OWN reaction budget). The cascade shares the action's
// event budget (the depth+width guard, event.go) — a reaction firing here can't blow the heartbeat, and
// because the granted attack does NOT itself fire OnLeaveRoom there is no A-flees -> B's-OA -> ... loop.
// Single-writer: zone goroutine; `leaver` and the reactors are all this zone's (a same-room departure).
func (z *Zone) fireLeaveRoom(parent *effectCtx, leaver *Entity) {
	if z == nil || leaver == nil || leaver.living == nil || leaver.location == nil {
		return
	}
	// Share ONE event budget across every reactor's OnLeaveRoom handler (distsys/security SC1), matching
	// the round driver's discipline — not a fresh maxEventHandlers per reactor. A nil parent (the command
	// call sites) roots a fresh cascade here; a non-nil parent (a future nested fire) inherits its budget.
	if parent == nil {
		budget := maxEventHandlers
		parent = &effectCtx{z: z, eventBudget: &budget}
	}
	// Snapshot the reactors first (a handler runs ops that could mutate room contents). A reactor is a
	// living foe in the leaver's room whose fighting link points AT the leaver — the "engaged with the
	// leaver" requirement the engine enforces structurally (content additionally gates on a reaction).
	var reactors []*Entity
	for _, e := range leaver.location.contents {
		if e != leaver && e.living != nil && e.living.fighting == leaver {
			reactors = append(reactors, e)
		}
	}
	for _, reactor := range reactors {
		// Re-validate per reactor: a prior reactor's handler may have moved/killed it or the leaver.
		if reactor.living == nil || reactor.location == nil ||
			leaver.living == nil || leaver.location == nil || reactor.location != leaver.location {
			continue
		}
		z.fireEvent(parent, evOnLeaveRoom, reactor, leaver, 1)
	}
}

// topUpReactions refills a combatant's per-round reaction resources to their derived max at the START of
// a combat round — the "regens 1/round" tie to the round the reaction budget needs ([G9]). A resource
// declares `per_round: true` (content) to be topped up here; the engine names no "reactions" pool (the
// per-round flag is the convention, not the ref). This is why a reactor that spent its reaction on one
// flee can opportunity-attack again only on a LATER round, never twice in the same round. Single-writer.
func (z *Zone) topUpReactions(e *Entity) {
	if e == nil || e.living == nil || e.zone == nil {
		return
	}
	for ref, def := range e.zone.resourceDefs().table() {
		if !def.perRound {
			continue
		}
		if max := resourceMax(e, ref); max > 0 {
			setResourceCurrent(e, ref, max)
		}
	}
}

// ensureCombatRound arms the per-zone round driver on the pulse scheduler if it is not already running.
// It is idempotent (a stored handle gates re-registration) and called from startFight. ONE callback per
// zone drives ALL fights in the zone — the centralized iteration the [G-G] order seam needs. The
// callback re-resolves every combatant fresh each round (it scans live room contents), so it never
// closes over a stale entity. Zone goroutine only.
func (z *Zone) ensureCombatRound() {
	if z.combatPulse != nil {
		return
	}
	z.combatPulse = z.pulses.every(PULSE_VIOLENCE, func(pulse uint64) bool {
		alive := z.runCombatRound(pulse)
		if !alive {
			z.combatPulse = nil // no fights left: retire the driver (startFight re-arms it)
			return false
		}
		return true
	})
}

// runCombatRound resolves one combat round for the WHOLE zone (one PULSE_VIOLENCE). It gathers every
// Fighting entity, orders them ([G-G] simultaneous default — a stable sort by the content-overridable
// `combat_order` attribute, NOT initiative), and resolves each one's swings in turn. Returns whether ANY
// fight remains (so the driver self-cancels when combat ends). Single-writer: zone goroutine.
//
// Resolve-by-presence (the pulse contract): combatants are re-gathered from LIVE room contents each
// round — a player who transferred zones / a mob that was reaped is simply not in the scan, so the
// driver never touches a stale cross-zone entity. (A player can't change zones while fighting — move()
// ENFORCES this, COMBAT.md invariant — so the in-room scan is authoritative for this zone's fights.)
func (z *Zone) runCombatRound(pulse uint64) bool {
	combatants := z.gatherCombatants()
	if len(combatants) == 0 {
		return false
	}
	// [G-G] order: stable sort by combat_order (default 0 for every entity => the gather order is
	// preserved). This is the seam an OnEnterCombat initiative check writes into; the DEFAULT is
	// simultaneous (no initiative baked in). sort.SliceStable keeps ties in gather order — deterministic.
	sort.SliceStable(combatants, func(i, j int) bool {
		return attr(combatants[i], "combat_order") > attr(combatants[j], "combat_order")
	})
	// ONE shared event budget for the WHOLE round (security): every swing's OnHit + OnDamageTaken
	// fireEvent threads this same pointer, so the TOTAL handler-runs across all swings of all combatants
	// in the round is bounded by maxEventHandlers — NOT a fresh 256 per swing (which scaled
	// swings×combatants and was a heartbeat-starvation vector). Matches event.go's "TOTAL work bounded"
	// intent; the budget refreshes each round (a new round is a new action).
	budget := maxEventHandlers
	// Per-round reaction budget ([G9]): refresh every combatant's per-round reaction pool to its max at
	// the START of the round, so a reactor gets its bounded number of reactions (opportunity attacks) per
	// round. A reaction spent mid-round (an OA on a fleeing foe) stays spent until the next round tops it
	// up here — this is what makes a SECOND flee in the same round provoke no opportunity attack.
	for _, c := range combatants {
		z.topUpReactions(c)
	}
	anyFighting := false
	for _, atk := range combatants {
		// Threat retarget (6.3b): a MOB re-points at its highest-threat live foe before swinging, so a
		// mob whose current target died/fled this round turns to the next threat rather than idling or
		// swinging at a departed entity. A no-op for players and for a mob whose target is still valid.
		z.retargetMob(atk)
		// Re-validate the attacker each iteration: a prior swing this round may have ended its fight
		// (its target left / the death path fired), or retargetMob found no live foe and stopFought it.
		// Skip a no-longer-fighting attacker cleanly.
		if atk.living == nil || atk.living.fighting == nil {
			continue
		}
		z.resolveSwings(atk, pulse, &budget)
		// Track liveness INCREMENTALLY rather than a second full gatherCombatants scan: this attacker
		// may have ended ITS fight during the swing loop, so re-read after. The driver lives while ANY
		// gathered combatant still has a fighting link post-round.
		if atk.living != nil && atk.living.fighting != nil {
			anyFighting = true
		}
	}
	return anyFighting
}

// gatherCombatants collects every entity in THIS zone currently in the Fighting state — players AND
// mobs — in a DETERMINISTIC order. Players are gathered from z.players (the authoritative player set;
// their entity may be in a room not in z.rooms in tests, so a room scan alone would miss them) iterated
// by SORTED character id — Go map iteration is randomized, and with the default combat_order=0 every
// entry ties, so an unsorted gather would make who-swings-first nondeterministic (the same class of bug
// as the 6.2 event-handler ordering; the test-engineer's replay/chaos tests need reproducibility). Mobs
// are gathered (in room-contents order, which is stable insertion order) by scanning the rooms where a
// fighting player stands (a mob fights a player; an all-mob fight with no player present is out of scope
// this slice — see the TODO below). A frozen/absent player is skipped (resolve-by-presence). De-duped by
// entity so a mob in two players' rooms is counted once. Zone-goroutine-owned reads only.
//
// 6.3b status (the S4 distsys boundary): the death path now SCRUBS a departed/dead player's opponents
// (die() -> disengage, respawnPlayer -> disengage, flee -> stopFight) so a fight whose last player
// LEAVES no longer strands a mob posFighting at a stale target — the common case the TODO worried about
// is closed. The residual gap is a pure MOB-vs-MOB fight with NO player ever in the room: such a fight
// is never gathered (no player anchors the room scan) and idles. A true zone-level Fighting SET (not
// player-anchored) is the clean fix; deferred because no current content starts a playerless mob fight
// (aggro/assist/retaliate all originate from a player), so it cannot arise from 6.3b content. The
// threat list (death.go) drives mob TARGET selection within a gathered fight; it does not (yet) replace
// the player-anchored GATHER.
func (z *Zone) gatherCombatants() []*Entity {
	ids := make([]string, 0, len(z.players))
	for id := range z.players {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var out []*Entity
	seen := map[*Entity]bool{}
	add := func(e *Entity) {
		if e == nil || e.living == nil || seen[e] {
			return
		}
		if position(e) == posFighting && e.living.fighting != nil {
			seen[e] = true
			out = append(out, e)
		}
	}
	for _, id := range ids {
		s := z.players[id]
		if s == nil || s.frozen || s.entity == nil {
			continue
		}
		add(s.entity)
		// Scan the player's room for fighting mobs (the player's opponents), in stable contents order.
		if s.entity.location != nil {
			for _, e := range s.entity.location.contents {
				if !isPlayer(e) {
					add(e)
				}
			}
		}
	}
	return out
}

// resolveSwings runs one attacker's full round: it loops `attacks` swings (a content attribute, capped),
// each through the swing pipeline against the attacker's current fighting target. The target is
// re-read per swing so a swing that ends the fight (the reserved death path) stops the rest of the
// round. `budget` is the round-shared event budget (runCombatRound) every swing's OnHit/OnDamageTaken
// threads. Single-writer: zone goroutine.
func (z *Zone) resolveSwings(attacker *Entity, pulse uint64, budget *int) {
	n := int(attr(attacker, "attacks"))
	if n < 1 {
		n = 1 // every combatant gets at least one swing (a contentless attacker still swings once)
	}
	if n > maxSwingsPerRound {
		n = maxSwingsPerRound
	}
	rng := z.combatRng()
	for i := 0; i < n; i++ {
		target := attacker.living.fighting
		if target == nil {
			return // the fight ended (a prior swing dropped it)
		}
		z.resolveSwing(attacker, target, i, rng, budget)
	}
}

// resolveSwing is the SWING PIPELINE for ONE swing (docs/COMBAT.md §3, the ordered stages):
//
//	gates -> to-hit check -> avoidance ladder -> damage (deal_damage + crit) -> soak -> apply -> OnHit
//
// `swingIndex` is the 0-based index within the round ([G-H], exposed to the to-hit/damage formulas as
// $swing.index). The whole pipeline runs on the zone goroutine; every harmful application funnels
// dealDamage -> guardHarmful (the harm funnel is unchanged — no new harm path is introduced here).
func (z *Zone) resolveSwing(attacker, target *Entity, swingIndex int, rng *rand.Rand, budget *int) {
	// --- Gate-1: position / same-room / target alive (visibility + safe-room ride the gates below) ----
	if !z.swingGatesPass(attacker, target) {
		return
	}

	// The ctx the whole pipeline shares: the attacker is $actor, the defender is $target. swingIndex
	// feeds $swing.index. disp is harmful so any branch's apply_affect/deal_damage gates correctly. The
	// round-shared eventBudget bounds the OnHit/OnDamageTaken cascade's TOTAL work across the round.
	c := &effectCtx{
		z: z, actor: attacker, source: attacker, target: target,
		mag: 1, disp: dispHarmful, rng: rng, swingIndex: swingIndex, eventBudget: budget,
	}

	prof := combatProfileFor(attacker)

	// --- Stage to-hit: the content to-hit check classifies hit/miss/crit ([G-F] the sole classifier
	// when no avoidance is authored). A nil profile/spec auto-hits (degenerate bare-engine case). The
	// matched band's label drives the outcome: "miss"/"crit"/anything-else=hit; the band's OWN ops also
	// run (so content can narrate or rider-proc on a crit). ------------------------------------------
	hit, crit := true, false
	if prof != nil && prof.toHit != nil {
		res := resolveCheck(c, prof.toHit)
		hit, crit = classifyToHit(res)
		if res.band != nil {
			runOps(c, res.band.ops) // the band's content ops (crit narration, a nat-1 fumble rider, ...)
		}
	}
	if !hit {
		z.combatMsg(attacker, target, "miss", "")
		return
	}

	// --- Stage avoidance ladder ([G-F]): zero-or-more content avoidance checks run IN ORDER; the FIRST
	// success negates the swing. The pipeline does NOT hardcode dodge->parry->block — it runs whatever
	// the DEFENDER's profile authored (5e/WoW author none -> straight to damage). A defender with no
	// profile/ladder skips this entirely. Each avoidance check resolves with the DEFENDER as $actor and
	// the ATTACKER as $target (so it reads `$actor.dodge` etc.); we build a defender-scoped ctx. -------
	if dprof := defenderProfile(target); dprof != nil {
		dc := &effectCtx{z: z, actor: target, source: target, target: attacker,
			mag: 1, disp: dispNeutral, rng: rng, swingIndex: swingIndex, eventBudget: budget}
		for _, av := range dprof.avoidance {
			res := resolveCheck(dc, av)
			if res.band != nil {
				runOps(dc, res.band.ops)
			}
			if avoidanceSucceeded(res) {
				// The band LABEL (dodge/parry/block) is the message verb — lowercase content, not the
				// spec's display label (which may be capitalized "Parry").
				z.combatMsg(attacker, target, "avoid", res.bandLabel)
				return
			}
		}
	}

	// --- Stage damage: the content damage op (deal_damage with the [G-A] scoped bonus: weapon dice +
	// $actor.damroll + str). A crit scales it via a damage MULTIPLIER attribute the content sets
	// (`crit_mult`); the multiply rides the ctx mag so the SAME deal_damage path doubles cleanly. The
	// damage op + its soak/mitigate are run through buildSwingDamage. ------------------------------
	z.applySwingDamage(c, attacker, target, crit)
}

// swingGatesPass runs the swing pipeline's gates (docs/COMBAT.md §3 step-1). The attacker must be able
// to act (position), the target must be alive and present in the SAME room, and the attacker's own
// fighting link must still point at this target. visibility/safe-room ride guardHarmful at apply time
// (a safe-room blocks the harm funnel) — the gate here is the cheap pre-checks that avoid even rolling
// to-hit on an invalid pairing. Returns true to proceed.
func (z *Zone) swingGatesPass(attacker, target *Entity) bool {
	if attacker.living == nil || target.living == nil {
		return false
	}
	if !canAct(attacker) {
		return false // sleeping/dead attacker cannot swing
	}
	if !canDefend(target) {
		return false // a dead target is not a valid swing target (death path is 6.3b)
	}
	if attacker.location == nil || attacker.location != target.location {
		// The target left the room (fled / was moved). End the attacker's fight cleanly — it has nothing
		// to swing at this round; a re-`kill` re-engages.
		z.stopFight(attacker)
		return false
	}
	return true
}

// applySwingDamage runs the damage stage for a landed swing. The damage is a deal_damage op carrying the
// attacker's WEAPON profile ([G-A] scoped bonus); a crit multiplies it via the `crit_mult` attribute on
// the ctx mag (so the one deal_damage path crits cleanly). The whole consequence chain — threat,
// OnDamageTaken, OnHit, and the death/on_depleted checkpoint — now lives INSIDE the shared dealDamage
// funnel (effect_op.go, 6.5 uniform death), so a swing, a spell, an AoE, and a DoT all kill and react
// through ONE path. Here we only build the swing op, apply the crit multiplier, emit the swing message,
// then run it. Single-writer: zone goroutine.
func (z *Zone) applySwingDamage(c *effectCtx, attacker, target *Entity, crit bool) {
	dmgOp := buildSwingDamageOp(attacker)
	// Crit multiplier: a content `crit_mult` attribute (>1) scales the whole damage roll through the ctx
	// magnitude — the SAME deal_damage path, no special crit op (P6-D6: crit is content numbers). 1 (or
	// unset) => no scaling.
	prevMag := c.mag
	if crit {
		if m := attr(attacker, "crit_mult"); m > 1 {
			c.mag = prevMag * m
		}
	}
	// SC2 (distsys): emit the "You hit $N" message BEFORE opDealDamage. The funnel now runs death INSIDE
	// the op (death narration / respawn message), so emitting after would invert the ordering — the room
	// would read "$n is DEAD!" / the player the respawn line BEFORE the hit that caused it. combatMsg does
	// not depend on the applied amount (c.lastDamage), so it is safe to emit ahead of the damage.
	z.combatMsg(attacker, target, hitOrCrit(crit), "")

	// opDealDamage reads c.target; the swing's target IS c.target already. Run it through the registered
	// handler so the [G-A] formula bonus + dice_count and the FULL funnel (mitigation, threat, OnDamage-
	// Taken/OnHit, the death checkpoint) all apply identically to a spell. dealDamage may have killed the
	// target and (for a player) respawned it before this returns — do NOT re-read the victim's vital here
	// (c.lastDamage carries the applied amount for anything downstream that needs it).
	_ = opDealDamage(c, dmgOp)
	c.mag = prevMag
}

// buildSwingDamageOp builds the deal_damage op for an attacker's swing from its WEAPON profile + the
// content damage bonus formula. It reads the attacker's wielded weapon (Weapon component) for the dice
// + damage type, and attaches the [G-A] scoped bonus formula the attacker's combat profile carries
// (weapon damroll + str). With no wielded weapon it falls back to the entity's intrinsic unarmed
// attack (a content `unarmed_*` attribute path) — for a mob that IS its weapon, the content carries the
// dice on the prototype's Weapon component anyway (a mob is spawned wielding its natural weapon, or the
// prototype carries a Weapon directly). The bonus formula is read from the attacker's combat profile.
func buildSwingDamageOp(attacker *Entity) *effectOp {
	op := &effectOp{kind: "deal_damage"}
	if w := wieldedWeapon(attacker); w != nil {
		op.diceNum, op.diceSize, op.dmgType = w.diceNum, w.diceSize, w.damageType
	}
	// The damage BONUS is a content formula ([G-A]) the attacker's combat profile carries — `$actor.str +
	// $actor.damroll` etc. It is parsed once at content build and stored as op.bonus so opDealDamage adds
	// it scoped to $actor. A profile with no bonus formula leaves op.bonus nil (raw weapon dice only).
	if prof := combatProfileFor(attacker); prof != nil {
		op.bonus = prof.damageBonus
	}
	return op
}

// combatProfileFor resolves an entity's combat profile by its combatRef through the per-shard combat-
// profile registry. nil when the entity has no Living, no combatRef, or the ref is unregistered (the
// degenerate auto-hit case). Zone-goroutine read (the registry is a lock-free atomic-swap table).
func combatProfileFor(e *Entity) *combatProfile {
	if e == nil || e.living == nil || e.living.combatRef == "" || e.zone == nil {
		return nil
	}
	return e.zone.combatProfiles().get(e.living.combatRef)
}

// wieldedWeapon returns the Weapon component of the item in the attacker's WearLocWield slot, or — if
// nothing is wielded — the Weapon component on the attacker's OWN prototype (a mob whose natural attack
// is authored directly on its prototype). nil when the attacker has neither (an unarmed player: the
// content unarmed dice ride the bonus formula / the swing deals bonus-only damage). Zone-goroutine read.
func wieldedWeapon(attacker *Entity) *Weapon {
	if w, ok := Get[*Wearer](attacker); ok {
		if item := w.worn[WearLocWield]; item != nil {
			if wp, ok := Get[*Weapon](item); ok {
				return wp
			}
		}
	}
	// Fallback: a Weapon on the attacker itself (a mob's natural attack on its prototype).
	if wp, ok := Get[*Weapon](attacker); ok {
		return wp
	}
	return nil
}

// defenderProfile returns the defender's combat profile (for the avoidance ladder), or nil if it has
// none. A small accessor so the pipeline reads cleanly. Zone-goroutine read.
func defenderProfile(target *Entity) *combatProfile {
	return combatProfileFor(target)
}

// classifyToHit maps a to-hit check result to (hit, crit). The CONVENTION (engine-fixed so the pipeline
// can read an outcome without naming content): a band labelled "miss" => miss; a band labelled "crit"
// => a critical hit; ANY other matched band (or no bands) => a normal hit. This keeps the to-hit a plain
// ordered-band check (check.go) — content authors hit/miss/crit bands in whatever shape (nat-20 faceEq,
// a margin band, a %-chance max band) and the engine reads only the LABEL. A nil band (empty spec) is a
// hit (the swing lands; content supplied no classifier).
func classifyToHit(res checkResult) (hit, crit bool) {
	if res.band == nil {
		return true, false
	}
	switch res.band.label {
	case "miss", "fumble":
		return false, false
	case "crit", "critical":
		return true, true
	default:
		return true, false
	}
}

// avoidanceSucceeded reports whether an avoidance check NEGATED the swing. CONVENTION: a band labelled
// "success"/"dodge"/"parry"/"block"/"avoid" (the avoidance succeeded) negates; a "fail"/"miss" band (or
// no matched band) does not. The pipeline reads only the LABEL, so the avoidance check's shape is fully
// content. An auto-fail (the gear-zeroed roll-under-0 case, [G-C]) lands in the fail band and does NOT
// negate — exactly the intended "no shield => block can't fire" behavior with no engine predicate.
func avoidanceSucceeded(res checkResult) bool {
	if res.band == nil {
		return false
	}
	switch res.band.label {
	case "success", "dodge", "parry", "block", "avoid", "evade":
		return true
	default:
		return false
	}
}

// vitalCurrent returns the current value of an entity's first vital resource (hp), or a large sentinel
// when it has none (so a "depleted?" test on a vital-less entity is never true). Zone-goroutine read.
func vitalCurrent(e *Entity) int {
	pool := vitalResource(e)
	if pool == "" {
		return 1 << 30 // no vital pool: never "depleted"
	}
	return resourceCurrent(e, pool)
}

// hitOrCrit picks the combat-message key for a landed swing.
func hitOrCrit(crit bool) string {
	if crit {
		return "crit"
	}
	return "hit"
}

// combatRng returns the rng the round driver feeds swing checks/damage. Production uses the package
// default (nil ctx rng -> randIntn); a test installs a seeded source via z.testCombatRng for
// determinism (the d1/size-1 trick + a seeded source make a whole fight reproducible).
//
// TODO(reproducibility): production combat draws from the PROCESS-GLOBAL math/rand default, so a live
// fight isn't replayable from a zone seed. A zone-OWNED seeded *rand.Rand (seeded at zone build, mutated
// only on this goroutine) would make production fights reproducible for the test-engineer's replay/chaos
// harness without the test-only field — a small follow-up; deferred to keep 6.3a focused.
func (z *Zone) combatRng() *rand.Rand {
	return z.testCombatRng
}

// combatMsg emits the per-stage combat narration (docs/COMBAT.md §3 — each stage emits its own
// message). It routes through act() so the attacker, the victim, and bystanders each get the right
// perspective. The verb is engine-fixed flavor for the STAGE (miss/hit/crit/avoid); the GMCP structured
// per-stage event is a reserved Phase-9 hook (the emit point is here). `detail` carries the avoidance
// label ("parry") for the avoid case. Single-writer: zone goroutine.
func (z *Zone) combatMsg(attacker, target *Entity, stage, detail string) {
	switch stage {
	case "miss":
		z.act("You miss $N.", attacker, nil, target, "", "", ToActor)
		z.act("$n misses you.", attacker, nil, target, "", "", ToVictim)
		z.act("$n misses $N.", attacker, nil, target, "", "", ToRoom)
	case "avoid":
		// detail = "dodge"/"parry"/"block": the defender avoided.
		verb := detail
		if verb == "" {
			verb = "avoid"
		}
		z.act("$N "+avoidVerb(verb, "s")+" your attack.", attacker, nil, target, "", "", ToActor)
		z.act("You "+verb+" $n's attack.", attacker, nil, target, "", "", ToVictim)
		z.act("$N "+avoidVerb(verb, "s")+" $n's attack.", attacker, nil, target, "", "", ToRoom)
	case "crit":
		z.act("You CRITICALLY hit $N!", attacker, nil, target, "", "", ToActor)
		z.act("$n CRITICALLY hits you!", attacker, nil, target, "", "", ToVictim)
		z.act("$n CRITICALLY hits $N!", attacker, nil, target, "", "", ToRoom)
	default: // hit
		z.act("You hit $N.", attacker, nil, target, "", "", ToActor)
		z.act("$n hits you.", attacker, nil, target, "", "", ToVictim)
		z.act("$n hits $N.", attacker, nil, target, "", "", ToRoom)
	}
}

// avoidVerb returns the third-person form of an avoidance verb ("dodge"->"dodges"). A tiny helper so the
// bystander line reads grammatically; not content (engine flavor for the stage message).
func avoidVerb(verb, suffix string) string {
	if verb == "" {
		return "avoids"
	}
	return verb + suffix
}
