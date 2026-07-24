package world

// position.go is the Position enum (docs/MUDLIB.md §6, docs/COMBAT.md §3 gate-1) — the
// standing/resting/sleeping/fighting state that gates which commands run and whether an entity can
// attack or defend. It REPLACES the semantics of the Living.position int stub (components.go) with a
// named set; the field stays an int so the component layout, the COW shallow-copy, and the persisted
// shape are all unchanged — this file only gives the values names + accessors.
//
// The combat round driver (combat.go) reads posFighting to find combatants; the swing pipeline's
// gate-1 reads it to decide if an entity can act/defend. Nothing here is content-named: the positions
// are engine mechanics (a mob can't swing while sleeping in EVERY ruleset), distinct from the combat
// NUMBERS (to-hit/soak/attacks) which are content attributes (P6-D6).

// Position is a living entity's bodily state. The zero value is posStanding so a freshly-spawned
// entity (player or mob) defaults to able-to-act — the bare-engine sane default.
type Position int

const (
	posStanding Position = iota // upright, able to act and defend (the default)
	posResting                  // sitting/resting: can defend, limited action
	posSleeping                 // asleep: cannot act, defends poorly (gate-1 reads this)
	posFighting                 // engaged in melee: the round driver swings these every PULSE_VIOLENCE
	posDead                     // hp depleted; reserved for the 6.3b death path (no swings, no defense)
)

// position returns the entity's Position (posStanding for a Living-less entity — it cannot fight, but
// the read is sane). Zone-goroutine-owned read.
func position(e *Entity) Position {
	if e == nil || e.living == nil {
		return posStanding
	}
	return Position(e.living.position)
}

// setPosition writes the entity's Position. Single-writer: zone goroutine. A no-op on a Living-less
// entity (an item has no position).
func setPosition(e *Entity, p Position) {
	l := mutableLiving(e) // COW: fork a proto-aliased mob's Living before writing (else death corrupts the proto)
	if l == nil {
		return
	}
	l.position = int(p)
}

// canAct reports whether the entity's position allows it to INITIATE an action (attack, cast). Dead/
// sleeping entities cannot. The round driver gates each attacker's swings on this (combat.go gate-1).
func canAct(e *Entity) bool {
	p := position(e)
	if p == posSleeping || p == posDead {
		return false
	}
	// DOWNED = STRUCTURALLY CANNOT ACT (#535, security F3). A death-suspended entity (held alive at 0 in
	// a depleted vital) is unconscious/dying — it cannot swing (swingGatesPass gates on canAct) or take
	// a reaction (canReact gates on canAct). This is keyed on the SUSPENSION itself, not on content
	// remembering to add `prevents: [act]` to its dying affect: without it, a suspends_death affect with
	// no prevents tags produces a bearer that cannot die (every lethal blow re-holds) yet can still
	// attack — an unkillable-and-acting PvP exploit. Being downed implies cannot-act structurally.
	if deathSuspended(e) {
		return false
	}
	return true
}

// reactPreventTag is the conventional CC tag an incapacitating affect carries to suppress reactions
// (#540). It sits alongside the other open-string prevents tags (move/cast/verbal): an affect that
// stuns/paralyzes/knocks-unconscious lists `prevents: [react, ...]`. The engine names no condition —
// this is just the tag the reaction gate queries, exactly as the cast gate queries the ability's tags.
const reactPreventTag = "react"

// canReact reports whether an entity may take a REACTION — an action out of turn (an opportunity
// attack, a Shield, a Counterspell, an OnDamageTaken reaction). It is stricter than canAct in one
// direction only: a reactor must be able to act by POSITION (not sleeping/dead) AND must not carry an
// affect that prevents the `react` tag. That second half is what lets a content-defined incapacitating
// condition (stunned/paralyzed/unconscious) suppress reactions without the engine learning any
// condition name — the same tag mechanism the ability cast gate uses. Zone-goroutine read.
func canReact(e *Entity) bool {
	return canAct(e) && !preventsTag(e, reactPreventTag)
}

// canDefend reports whether the entity's position allows it to DEFEND (the to-hit/avoidance the swing
// pipeline runs against it still applies, but a dead target cannot be attacked at all). A sleeping
// target defends — content's evasion/avoidance attributes carry the penalty (a content concern), not
// the engine; the engine only refuses to let a DEAD entity be a combat target.
func canDefend(e *Entity) bool {
	return position(e) != posDead
}
