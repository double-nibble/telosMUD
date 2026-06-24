package world

import "strings"

// dispatch runs one line of player input. Called only from the zone goroutine.
// Phase 1 hardcodes a handful of verbs; the real parser/registry is Phase 3
// (docs/MUDLIB.md §6).
func (z *Zone) dispatch(p *player, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		p.send(promptFrame())
		return
	}

	verb, rest := split(line)
	switch strings.ToLower(verb) {
	case "look", "l":
		z.lookRoom(p)
	case "say", "'":
		z.say(p, rest)
	case "who":
		z.who(p)
	case "quit":
		p.send(textFrame("Farewell."))
		p.send(disconnectFrame("quit"))
		return // no prompt; the stream will close
	case "north", "south", "east", "west", "up", "down",
		"n", "s", "e", "w", "u", "d":
		z.move(p, canonDir(verb))
	default:
		p.send(textFrame("Huh?"))
	}
	p.send(promptFrame())
}

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

func (z *Zone) say(p *player, what string) {
	what = strings.TrimSpace(what)
	if what == "" {
		p.send(textFrame("Say what?"))
		return
	}
	p.send(textFrame("You say, '" + what + "'"))
	z.broadcast(z.rooms[p.room], p.id, p.name+" says, '"+what+"'")
}

func (z *Zone) move(p *player, dir string) {
	if dir == "" {
		p.send(textFrame("Go where?"))
		return
	}
	from := z.rooms[p.room]
	dest, ok := from.exits[dir]
	if !ok {
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
}

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
