package world

import "strings"

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
//
// SHORT vs LONG rest (#512). `rest`/`sit` defaults to a SHORT rest; `rest long` (or `rest full`) is a
// LONG rest. Every rest fires the kind-agnostic OnRest (the back-compat hook), and ADDITIONALLY the
// specific OnShortRest / OnLongRest, so content can differentiate recovery (5e: a short rest spends Hit
// Dice; a long rest restores all HP + spell slots) purely as event handlers. The engine only names the
// distinction and lights the hooks — WHAT each kind restores stays content. The distinction rides
// distinct event KINDS (the engine's native subscription primitive), NOT the `mag` argument: mag doubles
// as an amount multiplier in rollOpAmount, so encoding the kind there would silently scale a handler's
// heal.

// restKind discriminates a short rest from a long rest (#512).
type restKind int

const (
	restShort restKind = iota
	restLong
)

// parseRestKind maps the `rest` verb's optional argument to a rest kind. "" / "short" → short (the
// default a bare `rest`/`sit` takes); "long" / "full" → long. Any other non-empty argument is a typo
// (e.g. "lng") and returns ok=false, so cmdRest refuses rather than silently defaulting to short.
func parseRestKind(arg string) (restKind, bool) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "short":
		return restShort, true
	case "long", "full":
		return restLong, true
	default:
		return restShort, false
	}
}

// restCommands returns the rest/stand verb set (registered low-priority so it never shadows a
// movement/look/say abbreviation).
func restCommands() []*Command {
	return []*Command{
		{Name: "rest", Aliases: []string{"sit"}, Run: cmdRest},
		{Name: "stand", Run: cmdStand},
	}
}

// cmdRest sits the actor down: it enters posResting (faster passive regen) and fires the rest events
// once. `rest`/`sit` is a SHORT rest; `rest long` / `rest full` is a LONG rest (#512). Refused mid-fight
// (rest is the opposite of fighting) or while dead; a no-op notice when already resting.
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
	kind, ok := parseRestKind(c.Arg(0))
	if !ok {
		c.Send("Rest how? Try 'rest', 'rest short', or 'rest long'.")
		return nil
	}
	// posStanding (and posSleeping, unreachable for players today — no sleep verb) fall through to sit.
	// When a `sleep` verb lands, add a posSleeping case here so resting a sleeper emits a wake transition.
	setPosition(e, posResting)
	if kind == restLong {
		c.Send("You settle in for a long rest.")
		c.z.act("$n settles in for a long rest.", e, nil, nil, "", "", ToRoom)
	} else {
		c.Send("You sit down for a short rest.")
		c.z.act("$n sits down for a short rest.", e, nil, nil, "", "", ToRoom)
	}
	// The rest events (event bus): a discrete root fire content reacts to with short/long-rest recovery.
	// Counterpart is NIL: rest is a solo reflexive action with no other party, so it patterns with
	// OnLevel/OnTrackStep (nil `other`), NOT a self-counterpart — a `target: other` op in a handler must
	// find no target, not silently resolve to the rester. Fired ONCE on entering rest; the ongoing benefit
	// is the passive regen bonus while posResting (runRegen). Every rest fires the kind-agnostic OnRest
	// (the back-compat hook a world subscribes to when it doesn't care which kind), THEN the specific
	// OnShortRest / OnLongRest so a handler can differentiate. A world with no subscriber is a clean no-op.
	c.z.fireEvent(nil, evOnRest, e, nil, 1)
	if kind == restLong {
		c.z.fireEvent(nil, evOnLongRest, e, nil, 1)
	} else {
		c.z.fireEvent(nil, evOnShortRest, e, nil, 1)
	}
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
