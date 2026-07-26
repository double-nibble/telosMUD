package world

// effect_op_move.go — #516 declarative movement effect-ops: `teleport` (blink the target to a same-zone
// room) and `push` (force the target one step along an exit). Both surface the reviewed same-zone relocation
// primitive (z.relocateEntity, luaharm.go) into the ability op-list, so C10 spell-bucket-D spells (Misty
// Step, Dimension Door, Thunderwave, Thorn Whip) are authorable as CONTENT rather than Lua.
//
// SECURITY. Forcing a NON-CONSENTING PLAYER to move is a grief vector, so both ops route a player target
// through mayRelocateCtx -> guardHarmful (the same funnel dealDamage uses); the actor moving ITSELF, or
// moving a mob, is ungated. Same-zone-only is STRUCTURAL: a teleport destination resolves only through this
// zone's own rooms (z.rooms), and a push destination resolves only through an EXIT (Room.exits) — an
// INSTANCE-ENTRANCE lives in the separate Room.entrances map and is therefore refused by construction, so a
// forced move can never shove a player through a dungeon door onto someone else's initiative (the #435 mint
// invariant / the follow-mechanic warning). No death-generation bump is needed: a relocate keeps the entity
// alive and attached, and any death inside an arrival hook routes die() (which bumps the gen), which runOps'
// around-the-op snapshot already catches.

// mayRelocateCtx decides whether the effect ctx may relocate entity e (#516) — the declarative twin of the
// Lua mayRelocate. Moving the ctx ACTOR itself, or a non-player, is always allowed; forcing another PLAYER
// is harm, gated through guardHarmful against e (a non-consenting player in a safe room is not relocated).
// A nil ctx/actor fails closed.
//
// CONSCIOUS DESIGN CALL (security review #516, Finding 2): a MOB actor is ungated against a player (the
// guardHarmful mob->player short-circuit), so a mob CAN teleport a non-consenting player to an arbitrary
// SAME-ZONE room, not just push it one step. This is deliberate and matches the standing property that mobs
// shove players around inside OBSERVABLE space (the pre-existing Lua h:teleport already grants exactly this).
// It provably cannot reach a private instance (same-zone-only + instance-entrance refusal), so the blast
// radius is a same-zone relocation-grief a builder chose to author — accepted, not a silent hole.
func mayRelocateCtx(c *effectCtx, e *Entity) bool {
	if c == nil || c.actor == nil {
		return false
	}
	if e == c.actor || !isPlayer(e) {
		return true
	}
	return guardHarmful(c, e)
}

// opTeleport blinks the target (ctx.target; tgt:self => the actor) to a SAME-ZONE room — a literal `room`
// ref, `dest: actor` (the caster's room, i.e. pull/summon-to-me), or `dest: start` (the zone login room, a
// same-zone recall). No opportunity attack (a blink). A cross-zone / unknown / already-here destination is a
// clean no-op. A forced move of a non-consenting player is gated.
func opTeleport(c *effectCtx, op *effectOp) error {
	e := c.target
	if e == nil || e.location == nil {
		return nil
	}
	var dest *Entity
	switch {
	case op.moveRoom != "":
		dest = c.z.rooms[ProtoRef(op.moveRoom)] // this zone's rooms only (same-zone by construction)
	case op.moveDest == "actor":
		if c.actor != nil {
			dest = c.actor.location
		}
	case op.moveDest == "start":
		dest = c.z.rooms[c.z.startRoom]
	}
	if dest == nil || !Has[*Room](dest) || dest.zone != c.z || dest == e.location {
		return nil
	}
	if !mayRelocateCtx(c, e) {
		return nil // a non-consenting player in a safe room: clean no-op
	}
	c.z.relocateEntity(e, dest, false /* blink: no opportunity attack */, c)
	return nil
}

// opPush forces the target one step along the `dir` exit of its CURRENT room (Thunderwave-style eject). The
// destination resolves ONLY through Room.exits, so an instance-entrance direction (Room.entrances) is refused
// by construction, and a cross-zone exit target (not in this zone's rooms) is a clean no-op. No opportunity
// attack (a shove is not a walk). A forced push of a non-consenting player is gated.
func opPush(c *effectCtx, op *effectOp) error {
	e := c.target
	if e == nil || e.location == nil || op.moveDir == "" {
		return nil
	}
	room, ok := Get[*Room](e.location)
	if !ok {
		return nil
	}
	destRef, ok := room.exits[op.moveDir]
	if !ok {
		return nil // no such EXIT (an instance-entrance is in room.entrances, excluded here by construction)
	}
	dest := c.z.rooms[destRef]
	if dest == nil || dest == e.location {
		return nil // cross-zone / unknown destination => same-zone-only no-op
	}
	if !mayRelocateCtx(c, e) {
		return nil
	}
	c.z.relocateEntity(e, dest, false, c)
	return nil
}
