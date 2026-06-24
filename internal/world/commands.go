package world

import "strings"

// dispatch parses and runs one line of player input. It is called only from the
// zone goroutine (via handle -> inputMsg), so every verb handler below mutates zone
// state lock-free. Phase 1 hardcodes a handful of verbs; the real parser/registry
// (alias expansion, abbreviation, command tables) is Phase 3 (docs/MUDLIB.md §6).
//
// Every path ends by sending a fresh prompt, except "quit" — which sends a
// disconnect instead and lets the stream close.
func (z *Zone) dispatch(p *player, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		// Blank line: just re-prompt.
		p.send(promptFrame())
		return
	}

	verb, rest := split(line)
	z.log.Debug("dispatch", "player", p.id, "verb", strings.ToLower(verb), "line", line)
	switch strings.ToLower(verb) {
	case "look", "l":
		z.lookRoom(p)
	case "say", "'":
		z.say(p, rest)
	case "who":
		z.who(p)
	case "quit":
		z.log.Debug("player quit", "player", p.id, "room", p.room)
		// Mark a clean, intentional disconnect so when the stream drops the zone
		// removes the player immediately instead of waiting out the link-death grace.
		p.quitting = true
		p.send(textFrame("Farewell."))
		p.send(disconnectFrame("quit"))
		return // no prompt; the stream will close
	case "north", "south", "east", "west", "up", "down",
		"n", "s", "e", "w", "u", "d":
		z.move(p, canonDir(verb))
	default:
		z.log.Debug("unknown verb", "player", p.id, "verb", strings.ToLower(verb))
		p.send(textFrame("Huh?"))
	}
	p.send(promptFrame())
}

// lookRoom sends the actor the current room's name, description, exits, and the
// other players present. Used by "look" and automatically on join/move.
func (z *Zone) lookRoom(p *player) {
	r := z.rooms[p.room]
	var b strings.Builder
	b.WriteString(r.name)
	b.WriteByte('\n')
	b.WriteString(r.desc)
	b.WriteByte('\n')
	if ex := r.sortedExits(); len(ex) > 0 {
		b.WriteString("Exits: " + strings.Join(ex, ", "))
	} else {
		b.WriteString("Exits: none")
	}
	for id := range r.occupants {
		if id == p.id {
			continue
		}
		if o := z.players[id]; o != nil {
			b.WriteString("\n" + o.name + " is here.")
		}
	}
	p.send(textFrame(b.String()))
}

// say echoes a message to the actor and broadcasts it to everyone else in the room.
func (z *Zone) say(p *player, what string) {
	what = strings.TrimSpace(what)
	if what == "" {
		p.send(textFrame("Say what?"))
		return
	}
	p.send(textFrame("You say, '" + what + "'"))
	z.broadcast(z.rooms[p.room], p.id, p.name+" says, '"+what+"'")
}

// move walks the player through an exit: it validates the direction, detaches the
// player from the old room (announcing the departure there), reattaches to the
// destination (announcing the arrival there), and shows the new room. Phase 1 moves
// are always intra-zone, so this is a plain pair of slice/map ops with no handoff
// (cross-zone moves arrive in a later phase, docs/MUDLIB.md §4).
func (z *Zone) move(p *player, dir string) {
	if dir == "" {
		p.send(textFrame("Go where?"))
		return
	}
	from := z.rooms[p.room]
	dest, ok := from.exits[dir]
	if !ok {
		z.log.Debug("move blocked: no exit", "player", p.id, "room", p.room, "dir", dir)
		p.send(textFrame("You can't go that way."))
		return
	}
	delete(from.occupants, p.id)
	z.broadcast(from, p.id, p.name+" leaves "+dir+".")

	p.room = dest
	to := z.rooms[dest]
	to.occupants[p.id] = true
	z.broadcast(to, p.id, p.name+" arrives.")
	z.lookRoom(p)
	z.log.Debug("player moved", "player", p.id, "dir", dir, "from", from.id, "to", dest)
}

// who lists every player currently online in the zone.
func (z *Zone) who(p *player) {
	var b strings.Builder
	b.WriteString("Players online:")
	for _, o := range z.players {
		b.WriteString("\n  " + o.name)
	}
	p.send(textFrame(b.String()))
}

// split returns the first whitespace-delimited word and the trimmed remainder.
func split(line string) (verb, rest string) {
	i := strings.IndexFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimSpace(line[i+1:])
}

// canonDir maps a movement verb or its abbreviation to its canonical direction,
// returning "" for anything unrecognized.
func canonDir(s string) string {
	switch strings.ToLower(s) {
	case "n", "north":
		return "north"
	case "s", "south":
		return "south"
	case "e", "east":
		return "east"
	case "w", "west":
		return "west"
	case "u", "up":
		return "up"
	case "d", "down":
		return "down"
	}
	return ""
}
