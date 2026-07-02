package world

// rest.go — the REST mechanic (Track 5, #39): the rest/sit/stand verbs that move a player between
// posStanding and posResting (position.go), the discrete OnRest event that fires ONCE when a player
// rests (the content hook for short/long-rest recovery — 5eSRD Track 8), and, paired with it, the
// resting REGEN BONUS (runRegen in resources.go multiplies passive regen while posResting).
//
// The split of concerns matches the engine=mechanism / content=flavor pillar: the engine only sets the
// bodily STATE, speeds passive regen while in it, and lights the OnRest hook; what a rest actually
// RESTORES (hit points, spell slots, a short/long-rest budget) is CONTENT — an OnRest handler or a
// rest-applied affect. OnRest fires on ENTER (the discrete "you rested" action, like a tabletop short
// rest), not per tick; the continuous benefit is the passive regen multiplier.

// restCommands returns the rest/stand verb set (registered low-priority so it never shadows a
// movement/look/say abbreviation).
func restCommands() []*Command {
	return []*Command{
		{Name: "rest", Aliases: []string{"sit"}, Run: cmdRest},
		{Name: "stand", Run: cmdStand},
	}
}

// cmdRest sits the actor down: it enters posResting (faster passive regen) and fires OnRest once. Refused
// mid-fight (rest is the opposite of fighting) or while dead; a no-op notice when already resting.
func cmdRest(c *Context) error {
	e := c.Actor
	switch position(e) {
	case posFighting:
		c.Send("You can't rest while fighting!")
		return nil
	case posDead:
		c.Send("You can't do that right now.")
		return nil
	case posResting:
		c.Send("You are already resting.")
		return nil
	}
	// posStanding (and posSleeping, unreachable for players today — no sleep verb) fall through to sit.
	// When a `sleep` verb lands, add a posSleeping case here so resting a sleeper emits a wake transition.
	setPosition(e, posResting)
	c.Send("You sit down and rest.")
	c.z.act("$n sits down to rest.", e, nil, nil, "", "", ToRoom)
	// OnRest (event bus, evOnRest): the subject rested — a discrete root fire content reacts to with
	// short/long-rest recovery. Counterpart is NIL: rest is a solo reflexive action with no other party,
	// so it patterns with OnLevel/OnTrackStep (nil `other`), NOT a self-counterpart — a `target: other`
	// op in a handler must find no target, not silently resolve to the rester. Fired ONCE on entering
	// rest; the ongoing benefit is the passive regen bonus while posResting (runRegen). A world with no
	// OnRest subscriber is a clean no-op.
	c.z.fireEvent(nil, evOnRest, e, nil, 1)
	return nil
}

// cmdStand brings the actor back to posStanding. A no-op notice when already up; refused while dead.
func cmdStand(c *Context) error {
	e := c.Actor
	switch position(e) {
	case posDead:
		c.Send("You can't do that right now.")
		return nil
	case posStanding:
		c.Send("You are already standing.")
		return nil
	case posFighting:
		c.Send("You are already on your feet.") // fighting is an upright, active state
		return nil
	}
	// posResting (and posSleeping, unreachable for players today) fall through to stand — see cmdRest.
	setPosition(e, posStanding)
	c.Send("You stand up.")
	c.z.act("$n stands up.", e, nil, nil, "", "", ToRoom)
	return nil
}
