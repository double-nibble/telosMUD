package world

import "strings"

// dispatch parses and runs one line of player input. It is called only from the zone
// goroutine (via handle -> inputMsg), so every verb handler below mutates zone state
// lock-free. Slice 1 keeps the hardcoded switch from Phase 1; the real parser/registry
// (alias expansion, abbreviation, command tables) is slice 2 (docs/MUDLIB.md §6). The
// handlers now operate on the session's in-world Entity (s.entity) and its containment
// tree rather than the old player.room string.
//
// Every path ends by sending a fresh prompt, except "quit" — which sends a disconnect
// instead and lets the stream close.
func (z *Zone) dispatch(s *session, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		// Blank line: just re-prompt.
		s.send(promptFrame())
		return
	}

	verb, rest := split(line)
	z.log.Debug("dispatch", "player", s.character, "verb", strings.ToLower(verb), "line", line)
	switch strings.ToLower(verb) {
	case "look", "l":
		z.lookRoom(s)
	case "say", "'":
		z.say(s, rest)
	case "who":
		z.who(s)
	case "quit":
		z.log.Debug("player quit", "player", s.character)
		// Mark a clean, intentional disconnect so when the stream drops the zone
		// removes the player immediately instead of waiting out the link-death grace.
		s.quitting = true
		s.send(textFrame("Farewell."))
		s.send(disconnectFrame("quit"))
		return // no prompt; the stream will close
	case "north", "south", "east", "west", "up", "down",
		"n", "s", "e", "w", "u", "d":
		if z.move(s, canonDir(verb)) {
			// move released ownership of s/its entity: an intra-shard transfer handed them
			// to another zone goroutine, or a cross-shard handoff froze the session. This
			// goroutine must not read or write s/its entity again (the new owner now does)
			// — and the prompt is the destination's job. Returning here keeps single-writer.
			return
		}
	default:
		z.log.Debug("unknown verb", "player", s.character, "verb", strings.ToLower(verb))
		s.send(textFrame("Huh?"))
	}
	s.send(promptFrame())
}

// lookRoom sends the actor the current room's name, description, exits, and the other
// occupants present. Used by "look" and automatically on join/move. It reads the room
// entity (s.entity.location) and its Room component for exits/desc, and walks the room's
// contents for other players (MUDLIB §4).
func (z *Zone) lookRoom(s *session) {
	e := s.entity
	r := e.location // the room entity
	room := r.room  // its Room component (direct-pointer hot path, MUDLIB §3)
	var b strings.Builder
	b.WriteString(r.Name())
	b.WriteByte('\n')
	b.WriteString(r.long)
	b.WriteByte('\n')
	if ex := room.sortedExits(); len(ex) > 0 {
		b.WriteString("Exits: ")
		b.WriteString(strings.Join(ex, ", "))
	} else {
		b.WriteString("Exits: none")
	}
	for _, occ := range r.contents {
		if occ == e {
			continue
		}
		if Has[*PlayerControlled](occ) {
			b.WriteByte('\n')
			b.WriteString(occ.Name())
			b.WriteString(" is here.")
		}
	}
	s.send(textFrame(b.String()))
}

// say echoes a message to the actor and broadcasts it to everyone else in the room.
func (z *Zone) say(s *session, what string) {
	what = strings.TrimSpace(what)
	if what == "" {
		s.send(textFrame("Say what?"))
		return
	}
	s.send(textFrame("You say, '" + what + "'"))
	z.broadcast(s.entity.location, s.character, s.entity.Name()+" says, '"+what+"'")
}

// move walks the player through an exit: it validates the direction, detaches the
// entity from the old room (announcing the departure there), reattaches to the
// destination (announcing the arrival there), and shows the new room.
//
// It returns true when it RELEASED OWNERSHIP of the session/entity — an intra-shard
// transfer handed them to another zone goroutine (transferOut), or a cross-shard handoff
// froze the session for redirect. In both cases the caller (dispatch) must not touch
// s/its entity again: the new owner will, and re-reading here would be a data race /
// double-prompt. All in-zone outcomes (bad direction, sealed boundary, plain local move)
// return false so dispatch re-prompts normally.
func (z *Zone) move(s *session, dir string) bool {
	if dir == "" {
		s.send(textFrame("Go where?"))
		return false
	}
	from := s.entity.location // the current room entity
	ref, ok := from.room.exits[dir]
	if !ok {
		z.log.Debug("move blocked: no exit", "player", s.character, "room", from.proto, "dir", dir)
		s.send(textFrame("You can't go that way."))
		return false
	}
	destZone, destRoom := parseRef(ref)

	// Intra-shard cross-zone move: the destination is a DIFFERENT zone that THIS shard
	// also hosts. Transfer the player in-process — no handoff, no snapshot, no epoch
	// bump, no directory change, no gate re-dial. The session keeps the same out channel
	// and appliedSeq. transferOut hands the session+entity to dest, so we release ownership.
	if destZone != "" && destZone != z.id && z.shard != nil {
		if dest := z.shard.zones[destZone]; dest != nil {
			z.transferOut(s, dest, destRoom, dir, from)
			return true
		}
	}

	// Cross-shard (cross-zone) move: hand the player off rather than moving locally.
	if destZone != "" && destZone != z.id {
		if z.handoff == nil {
			// Single-shard zone with no directory: the boundary is sealed.
			s.send(textFrame("The way is sealed."))
			return false
		}
		// Combat exclusion would be checked here (PROTOCOL.md §5); no combat in Phase 2.
		// Freeze first: from now on this shard stops acting for the player. Build the
		// snapshot on this (zone) goroutine, then kick off the async handoff.
		s.frozen = true
		// The player has departed this room: detach the entity from the room so they
		// don't linger as a ghost others can see while the handoff is in flight. (The
		// frozen session/entity itself is GC'd later, once a discard signal lands.)
		z.broadcast(from, s.character, s.entity.Name()+" departs "+dir+".")
		Move(s.entity, nil)
		z.log.Debug("cross-shard move initiated", "player", s.character,
			"dest_zone", destZone, "dest_room", destRoom, "epoch", s.epoch)
		z.handoff(z, buildSnapshot(s), destZone, string(destRoom), s.epoch)
		// s is now frozen/redirecting; the source must stop acting for it (no prompt).
		return true
	}

	// Local move within this zone.
	to := z.rooms[destRoom]
	if to == nil {
		z.log.Debug("move blocked: unknown local room", "player", s.character, "ref", ref)
		s.send(textFrame("You can't go that way."))
		return false
	}
	z.broadcast(from, s.character, s.entity.Name()+" leaves "+dir+".")
	Move(s.entity, to)
	z.broadcast(to, s.character, s.entity.Name()+" arrives.")
	z.lookRoom(s)
	z.log.Debug("player moved", "player", s.character, "dir", dir, "from", from.proto, "to", destRoom)
	return false
}

// transferOut performs the SOURCE side of an intra-shard cross-zone walk. It runs on
// this (source) zone goroutine: it removes the session from this zone (player map; the
// entity is detached from the room, announcing the departure), records a forwarding
// entry so any input the reader loop still posts here lands at the destination, and
// hands the session+entity to the destination zone via transferInMsg. The destination
// then owns them and repoints currentZone; the session keeps the same out channel and
// appliedSeq, so nothing is lost and forwarded replays dedup by appliedSeq.
//
// Single-writer note: once the session is removed here and posted to dest, only dest's
// goroutine touches the session/entity. The brief overlap is bounded by handing them off
// through the inbox, never by sharing them across two live owners.
func (z *Zone) transferOut(s *session, dest *Zone, destRoom ProtoRef, dir string, from *Entity) {
	z.broadcast(from, s.character, s.entity.Name()+" leaves "+dir+".")
	Move(s.entity, nil) // detach from the source room before handing off
	delete(z.players, s.character)
	// Forward in-flight input to dest until the reader loop observes the new
	// currentZone (which dest.transferIn Stores). dest dedups by appliedSeq.
	z.forwarding[s.character] = dest
	z.log.Debug("intra-shard transfer out", "player", s.character,
		"from_zone", z.id, "to_zone", dest.id, "room", destRoom)
	dest.post(transferInMsg{s: s, room: destRoom})
}

// who lists every player currently online in the zone.
func (z *Zone) who(s *session) {
	var b strings.Builder
	b.WriteString("Players online:")
	for _, o := range z.players {
		b.WriteByte('\n')
		b.WriteByte(' ')
		b.WriteString(o.entity.Name())
	}
	s.send(textFrame(b.String()))
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
