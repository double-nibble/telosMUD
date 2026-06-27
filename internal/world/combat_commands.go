package world

// combat_commands.go holds the player verbs that ENTER and LEAVE combat (docs/COMBAT.md §1, §7,
// Phase 6.3a): `kill <target>` engages a living target (and makes it retaliate), `flee` disengages.
// They are thin: kill resolves a room target and calls startFight (combat.go); flee calls stopFight.
// The round driver (combat.go) then resolves the swings every PULSE_VIOLENCE. Both run on the zone
// goroutine via dispatch, single-writer. (assist/consider/threat-list are 6.3b — reserved.)

// combatCommands returns the combat verbs for the base command table. Registered LAST (like the
// container verbs) so abbreviation precedence (movement/look/say first) is unchanged — `k` still
// resolves to a movement/earlier verb if one exists, never silently to `kill`.
func combatCommands() []*Command {
	return []*Command{
		{Name: "kill", Aliases: []string{"k"}, Run: cmdKill},
		{Name: "flee", Run: cmdFlee},
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

// cmdFlee disengages the actor from combat (docs/COMBAT.md §7). This slice's flee is the simple form:
// it drops the actor out of the Fighting state (stopFight). A directional flee (move to a random exit,
// the classic ROM panic-flee) and a `prevents: move`-aware root check are a follow-up; the disengage
// itself is the 6.3a piece (assist/threat is 6.3b). A flee while not fighting is a clean message.
func cmdFlee(c *Context) error {
	if c.Actor.living == nil || c.Actor.living.fighting == nil {
		c.Send("You aren't fighting anyone.")
		return nil
	}
	c.z.stopFight(c.Actor)
	c.z.act("You flee from combat!", c.Actor, nil, nil, "", "", ToActor)
	c.z.act("$n flees from combat!", c.Actor, nil, nil, "", "", ToRoom)
	return nil
}
