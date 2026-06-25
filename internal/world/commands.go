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
		if z.move(p, canonDir(verb)) {
			// move released ownership of p: an intra-shard transfer handed the struct to
			// another zone goroutine, or a cross-shard handoff froze it. This goroutine
			// must not read or write p again (the new owner now does) — and the prompt is
			// the destination's job. Returning here is what keeps single-writer intact.
			return
		}
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
		b.WriteString("Exits: ")
		b.WriteString(strings.Join(ex, ", "))
	} else {
		b.WriteString("Exits: none")
	}
	for id := range r.occupants {
		if id == p.id {
			continue
		}
		if o := z.players[id]; o != nil {
			b.WriteByte('\n')
			b.WriteString(o.name)
			b.WriteString(" is here.")
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
// destination (announcing the arrival there), and shows the new room.
//
// It returns true when it RELEASED OWNERSHIP of p — an intra-shard transfer handed the
// struct to another zone goroutine (transferOut), or a cross-shard handoff froze it for
// redirect. In both cases the caller (dispatch) must not touch p again: the new owner
// will, and re-reading p here would be a data race / double-prompt. All in-zone outcomes
// (bad direction, sealed boundary, plain local move) return false so dispatch re-prompts
// normally.
func (z *Zone) move(p *player, dir string) bool {
	if dir == "" {
		p.send(textFrame("Go where?"))
		return false
	}
	from := z.rooms[p.room]
	ref, ok := from.exits[dir]
	if !ok {
		z.log.Debug("move blocked: no exit", "player", p.id, "room", p.room, "dir", dir)
		p.send(textFrame("You can't go that way."))
		return false
	}
	destZone, destRoom := parseRef(ref)

	// Intra-shard cross-zone move: the destination is a DIFFERENT zone that THIS shard
	// also hosts. Transfer the player in-process — no handoff, no snapshot, no epoch
	// bump, no directory change, no gate re-dial. The player keeps the same out channel
	// and appliedSeq. transferOut hands the struct to dest, so we release ownership.
	if destZone != "" && destZone != z.id && z.shard != nil {
		if dest := z.shard.zones[destZone]; dest != nil {
			z.transferOut(p, dest, destRoom, dir, from)
			return true
		}
	}

	// Cross-shard (cross-zone) move: hand the player off rather than moving locally.
	if destZone != "" && destZone != z.id {
		if z.handoff == nil {
			// Single-shard zone with no directory: the boundary is sealed.
			p.send(textFrame("The way is sealed."))
			return false
		}
		// Combat exclusion would be checked here (PROTOCOL.md §5); no combat in Phase 2.
		// Freeze first: from now on this shard stops acting for the player. Build the
		// snapshot on this (zone) goroutine, then kick off the async handoff.
		p.frozen = true
		// The player has departed this room: remove them from the occupant set so they
		// don't linger as a ghost others can see while the handoff is in flight. (The
		// frozen player struct itself is GC'd later, once a discard signal lands.)
		delete(from.occupants, p.id)
		z.broadcast(from, p.id, p.name+" departs "+dir+".")
		z.log.Debug("cross-shard move initiated", "player", p.id, "dest_zone", destZone, "dest_room", destRoom, "epoch", p.epoch)
		z.handoff(z, buildSnapshot(p), destZone, destRoom, p.epoch)
		// p is now frozen/redirecting; the source must stop acting for it (no prompt).
		return true
	}

	// Local move within this zone.
	to, ok := z.rooms[destRoom]
	if !ok {
		z.log.Debug("move blocked: unknown local room", "player", p.id, "ref", ref)
		p.send(textFrame("You can't go that way."))
		return false
	}
	delete(from.occupants, p.id)
	z.broadcast(from, p.id, p.name+" leaves "+dir+".")

	p.room = destRoom
	to.occupants[p.id] = true
	z.broadcast(to, p.id, p.name+" arrives.")
	z.lookRoom(p)
	z.log.Debug("player moved", "player", p.id, "dir", dir, "from", from.id, "to", destRoom)
	return false
}

// transferOut performs the SOURCE side of an intra-shard cross-zone walk. It runs on
// this (source) zone goroutine: it removes the player from this zone (player map +
// room occupants, announcing the departure), records a forwarding entry so any input
// the reader loop still posts here lands at the destination, and hands the player
// struct to the destination zone via transferInMsg. The destination then owns the
// player and repoints currentZone; the player keeps the same out channel and
// appliedSeq, so nothing is lost and forwarded replays dedup by appliedSeq.
//
// Single-writer note: once the player is removed here and posted to dest, only dest's
// goroutine touches the player struct. The brief overlap is bounded by handing the
// struct off through the inbox, never by sharing it across two live owners.
func (z *Zone) transferOut(p *player, dest *Zone, destRoom, dir string, from *Room) {
	delete(from.occupants, p.id)
	z.broadcast(from, p.id, p.name+" leaves "+dir+".")
	delete(z.players, p.id)
	// Forward in-flight input to dest until the reader loop observes the new
	// currentZone (which dest.transferIn Stores). dest dedups by appliedSeq.
	z.forwarding[p.id] = dest
	z.log.Debug("intra-shard transfer out", "player", p.id,
		"from_zone", z.id, "to_zone", dest.id, "room", destRoom)
	dest.post(transferInMsg{p: p, room: destRoom})
}

// who lists every player currently online in the zone.
func (z *Zone) who(p *player) {
	var b strings.Builder
	b.WriteString("Players online:")
	for _, o := range z.players {
		b.WriteByte('\n')
		b.WriteByte(' ')
		b.WriteString(o.name)
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
