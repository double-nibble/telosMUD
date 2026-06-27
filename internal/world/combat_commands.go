package world

// combat_commands.go holds the player verbs that ENTER and LEAVE combat (docs/COMBAT.md §1, §7,
// Phase 6.3a/b): `kill <target>` engages a living target (and makes it retaliate), `flee` disengages,
// `assist <ally>` joins an ally's fight, `consider <target>` reads a target's relative difficulty.
// They are thin: kill resolves a room target and calls startFight (combat.go); flee calls stopFight.
// The round driver (combat.go) then resolves the swings every PULSE_VIOLENCE. All run on the zone
// goroutine via dispatch, single-writer.

// combatCommands returns the combat verbs for the base command table. Registered LAST (like the
// container verbs) so abbreviation precedence (movement/look/say first) is unchanged — `k` still
// resolves to a movement/earlier verb if one exists, never silently to `kill`.
func combatCommands() []*Command {
	return []*Command{
		{Name: "kill", Aliases: []string{"k"}, Run: cmdKill},
		{Name: "flee", Run: cmdFlee},
		{Name: "assist", Aliases: []string{"as"}, Run: cmdAssist},
		{Name: "consider", Aliases: []string{"con"}, Run: cmdConsider},
	}
}

// cmdKill engages a living target in melee (docs/COMBAT.md §1). It resolves a living in the actor's
// room by the typed keyword (`kill goblin`), then startFight sets both into the Fighting state and arms
// the zone round driver. A missing target / a self-target / a non-living is a clean message. It does NOT
// resolve swings itself — the round driver does, on the next PULSE_VIOLENCE — so `kill` returns
// immediately and the fight plays out over rounds. Single-writer: zone goroutine.
func cmdKill(c *Context) error {
	arg := c.Arg(0)
	if arg == "" {
		c.Send("Kill whom?")
		return nil
	}
	hits := c.z.Resolve(c.Actor, parseTargetSpec(arg), ScopeRoomLiving)
	if len(hits) == 0 {
		c.Send("They aren't here.")
		return nil
	}
	target := hits[0]
	if target == c.Actor {
		c.Send("You can't attack yourself.")
		return nil
	}
	// PvP: a `kill` against another non-consenting player is refused at the same gate harmful abilities
	// use (defense-in-depth — the swing's dealDamage funnels guardHarmful too, but refusing to ENGAGE is
	// the clean player-facing behavior, not a fight that lands zero damage every round).
	if isPlayer(target) && !pvpAllowed(c.Actor, target) {
		c.Send("You cannot harm " + target.Name() + " here.")
		return nil
	}
	if !c.z.startFight(c.Actor, target) {
		c.Send("You can't attack that.")
		return nil
	}
	c.z.act("You attack $N!", c.Actor, nil, target, "", "", ToActor)
	c.z.act("$n attacks you!", c.Actor, nil, target, "", "", ToVictim)
	c.z.act("$n attacks $N!", c.Actor, nil, target, "", "", ToRoom)
	return nil
}

// cmdFlee disengages the actor from combat (docs/COMBAT.md §7). Two forms:
//   - bare `flee`        — drops the actor out of the Fighting state IN PLACE (the 6.3a simple form).
//   - `flee <dir>`       — the classic ROM panic-flee: provoke an OPPORTUNITY ATTACK from every engaged
//     foe (the OnLeaveRoom checkpoint, [G9]) THEN bolt through the exit.
//
// The ORDERING is load-bearing: fireLeaveRoom runs WHILE the fleer is still posFighting and in-room, so
// each reactor's `fighting == fleer` link is live for the OA's harm gate (fail-closed-on-detached,
// effect_op.go). Only AFTER the reactions resolve does the fleer disengage + relocate — so a foe gets its
// granted swing at the back of the departing fleer, and the reaction-budget bound means a SECOND flee
// the same round (before the round driver tops the budget back up) provokes nothing. A directional flee
// that the leaver is ROOTED out of (`prevents: move`) is refused (you can't run while webbed/rooted).
func cmdFlee(c *Context) error {
	if c.Actor.living == nil || c.Actor.living.fighting == nil {
		c.Send("You aren't fighting anyone.")
		return nil
	}
	dir := c.Arg(0)
	// Bare flee: in-place disengage (the existing 6.3a contract — no room change, no provoke).
	if dir == "" {
		c.z.stopFight(c.Actor)
		c.z.act("You flee from combat!", c.Actor, nil, nil, "", "", ToActor)
		c.z.act("$n flees from combat!", c.Actor, nil, nil, "", "", ToRoom)
		return nil
	}
	// Directional flee: validate the exit BEFORE provoking, so a flee toward a wall does not waste the
	// reaction budget / leave the fleer half-departed. Same-zone only (combat is same-zone; a cross-zone
	// flee would cross the no-fighting-pointer boundary — refused as sealed, consistent with move()).
	from := c.Actor.location
	if from == nil || from.room == nil {
		c.Send("You can't flee that way.")
		return nil
	}
	ref, ok := from.room.exits[dir]
	if !ok {
		c.Send("You can't flee that way.")
		return nil
	}
	destZone, destRoom := parseRef(ref)
	if (destZone != "" && destZone != c.z.id) || c.z.rooms[destRoom] == nil {
		c.Send("You can't flee that way.")
		return nil
	}
	// Rooted? A `prevents: move` affect (a web/root) blocks the panic-flee just as it blocks a walk.
	if preventsTag(c.Actor, "move") {
		c.Send("You are held fast and cannot flee!")
		return nil
	}
	// THE OPPORTUNITY-ATTACK CHECKPOINT: fire OnLeaveRoom about every engaged foe (subject=reactor,
	// other=fleer) while the fleer is STILL fighting + in-room. A content reaction handler (a `reactions`
	// resource's on_event[OnLeaveRoom]) spends a reaction and lands a granted, GATED swing on the fleer.
	origin := c.Actor.location
	c.z.fireLeaveRoom(nil, c.Actor)
	// M1 (distsys review): if a lethal opportunity attack KILLED the fleer, die()->respawnPlayer already
	// relocated + messaged them (a player respawns at the start room; respawn clears posDead back to
	// standing, so the operative signal is a CHANGED location). Continuing the flee here would teleport
	// the just-respawned player to the flee destination — recording a "fled" location for someone who
	// died. Abort: the death path owns their fate. (posDead is the belt-and-suspenders for a non-player.)
	if c.Actor.location != origin || position(c.Actor) == posDead {
		return nil
	}
	// Now disengage (both directions) and relocate. disengage drops the fleer's fight AND every opponent's
	// link at it, so no fighting pointer survives the room change.
	c.z.disengage(c.Actor)
	c.z.act("You flee "+dir+"!", c.Actor, nil, nil, "", "", ToActor)
	c.z.act("$n flees "+dir+"!", c.Actor, nil, nil, "", "", ToRoom)
	Move(c.Actor, c.z.rooms[destRoom])
	c.z.act("$n arrives, panting.", c.Actor, nil, nil, "", "", ToRoom)
	// Arrival hooks: a webbed destination roots the entrant; an aggressive mob there re-engages.
	applyRoomAffectsTo(c.Actor)
	c.z.aggroOnEntry(c.Actor, c.z.rooms[destRoom])
	if c.s != nil {
		c.z.lookRoom(c.s)
	}
	return nil
}

// cmdAssist joins an ally's fight (docs/COMBAT.md §7, Phase 6.3b): `assist <ally>` adopts the ally's
// current target and starts swinging at it. It resolves a living ally in the room, reads who they are
// fighting, and startFights the actor against THAT target — so a second player can pile onto the same
// mob. The PvP gate is re-checked against the adopted target (you can't assist into harming a
// non-consenting player). A flailing ally (not fighting) is a clean message. Single-writer.
func cmdAssist(c *Context) error {
	arg := c.Arg(0)
	if arg == "" {
		c.Send("Assist whom?")
		return nil
	}
	hits := c.z.Resolve(c.Actor, parseTargetSpec(arg), ScopeRoomLiving)
	if len(hits) == 0 {
		c.Send("They aren't here.")
		return nil
	}
	ally := hits[0]
	if ally == c.Actor {
		c.Send("You can't assist yourself.")
		return nil
	}
	if ally.living == nil || ally.living.fighting == nil {
		c.z.act("$N isn't fighting anyone.", c.Actor, nil, ally, "", "", ToActor)
		return nil
	}
	target := ally.living.fighting
	if isPlayer(target) && !pvpAllowed(c.Actor, target) {
		c.Send("You cannot harm " + target.Name() + " here.")
		return nil
	}
	if !c.z.startFight(c.Actor, target) {
		c.Send("You can't attack that.")
		return nil
	}
	c.z.act("You assist $N!", c.Actor, nil, ally, "", "", ToActor)
	c.z.act("$n assists you!", c.Actor, nil, ally, "", "", ToVictim)
	c.z.act("$n assists $N!", c.Actor, nil, ally, "", "", ToRoom)
	return nil
}

// cmdConsider gives a rough difficulty readout for a target (docs/COMBAT.md §7, Phase 6.3b):
// `consider <target>`. It compares the actor's and target's combat attributes (their vital max + a
// damage/accuracy proxy) and reports a content-agnostic verdict. The READOUT is engine flavor; the
// NUMBERS it reads are content attributes (P6-D6) — a pack that defines no combat attributes yields a
// neutral read. Pure read; no state change. Single-writer: zone goroutine.
func cmdConsider(c *Context) error {
	arg := c.Arg(0)
	if arg == "" {
		c.Send("Consider whom?")
		return nil
	}
	hits := c.z.Resolve(c.Actor, parseTargetSpec(arg), ScopeRoomLiving)
	if len(hits) == 0 {
		c.Send("They aren't here.")
		return nil
	}
	target := hits[0]
	if target == c.Actor {
		c.Send("You consider yourself. A worthy opponent, surely.")
		return nil
	}
	c.z.act(considerVerdict(c.Actor, target), c.Actor, nil, target, "", "", ToActor)
	return nil
}

// considerVerdict builds the consider readout comparing the actor's "combat power" to the target's. It
// is a coarse ratio of an OFFENSE+DURABILITY proxy (vital max + accuracy + a damage-bonus stand-in)
// each side reads from CONTENT attributes — so a tougher/hittier mob reads as more dangerous without
// the engine naming any class/level. Equal or contentless powers read "a fair fight". The $N referent
// is the target. Returns the act() template.
func considerVerdict(actor, target *Entity) string {
	pa, pt := combatPower(actor), combatPower(target)
	var verdict string
	switch {
	case pt <= pa*0.5:
		verdict = "You could kill $N with one hand tied behind your back."
	case pt <= pa*0.8:
		verdict = "$N looks like easy prey."
	case pt < pa*1.25:
		verdict = "$N looks like a fair fight."
	case pt < pa*2.0:
		verdict = "$N looks dangerous. Watch yourself."
	default:
		verdict = "Death itself stalks beside $N. Flee while you can!"
	}
	return verdict
}

// combatPower is the coarse offense+durability proxy `consider` ranks by: the entity's vital max
// (durability) plus its accuracy and damroll (offense). Every term is a CONTENT attribute read through
// attr()/resourceMax — a contentless entity scores 0 on every term, so two contentless entities read
// as a fair fight (equal). Engine flavor reads these numbers; it never defines them. Zone-goroutine read.
func combatPower(e *Entity) float64 {
	if e == nil || e.living == nil {
		return 0
	}
	p := 0.0
	if pool := vitalResource(e); pool != "" {
		p += float64(resourceMax(e, pool))
	}
	p += attr(e, "accuracy") + attr(e, "damroll")
	return p
}
