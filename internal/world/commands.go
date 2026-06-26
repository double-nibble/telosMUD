package world

import "strings"

// Verb handlers and the base command table (docs/MUDLIB.md §6). The parser/registry and
// the dispatch loop live in parser.go; this file holds registerCommands (the priority-
// ordered base table) and the handlers themselves. Each handler runs on the zone
// goroutine via dispatch, so it mutates zone state lock-free (MUDLIB §4).
//
// The handlers preserve every external behavior the slice-1 hardcoded switch produced:
// the text players see for look/say/who/move/quit is byte-for-byte unchanged. Broadcast
// lines (say/arrive/leave/depart) now flow through act() perspective messaging (act.go)
// rather than ad-hoc broadcast strings, but render the SAME text.

// registerCommands returns the base command set in PRIORITY order (MUDLIB §6): index 0 is
// highest priority, so a typed prefix that collides resolves to the earlier entry. This
// ordering is the single source of the abbreviation rule — movement and the common verbs
// come first so "n"->north, "l"->look, never a rarer verb. Built once into baseTable
// (parser.go); the active-table stack would layer on top in a later phase.
//
// Movement verbs are registered with their canonical names and single-letter aliases; the
// handler routes to z.move (unchanged handoff/transfer logic) and flags ctx.moved when
// move released ownership, so dispatch honors the early-return invariant.
func registerCommands() []*Command {
	mv := func(dir string) func(*Context) error {
		return func(c *Context) error {
			if c.z.move(c.s, dir) {
				c.moved = true // move released ownership: dispatch must not re-prompt
			}
			return nil
		}
	}
	base := []*Command{
		// Movement first: highest priority so single letters resolve to directions.
		{Name: "north", Aliases: []string{"n"}, Run: mv("north")},
		{Name: "south", Aliases: []string{"s"}, Run: mv("south")},
		{Name: "east", Aliases: []string{"e"}, Run: mv("east")},
		{Name: "west", Aliases: []string{"w"}, Run: mv("west")},
		{Name: "up", Aliases: []string{"u"}, Run: mv("up")},
		{Name: "down", Aliases: []string{"d"}, Run: mv("down")},
		// Common verbs next.
		{Name: "look", Aliases: []string{"l"}, Run: cmdLook},
		{Name: "say", Aliases: []string{"'"}, Run: cmdSay},
		{Name: "who", Run: cmdWho},
		{Name: "quit", Run: cmdQuit},
	}
	// Container/inventory/equipment verbs last: lower priority than movement/look/say so
	// abbreviation precedence (the "n->north not nuke" rule) is unchanged. They live in
	// container.go (the Phase-3 milestone).
	return append(base, containerCommands()...)
}

// cmdLook shows the actor their current room (MUDLIB §6). Slice 2 keeps look's behavior
// identical to slice 1: it always describes the room (targeted `look <thing>` is a slice-4
// concern once items exist). Routes to the existing lookRoom so the output is unchanged.
func cmdLook(c *Context) error {
	c.z.lookRoom(c.s)
	return nil
}

// cmdSay echoes a message to the actor and broadcasts it to everyone else in the room.
// The literal say text is passed as the $t arg, so a '%' or '$' inside it is rendered
// verbatim (no format-string interpretation, act.go).
func cmdSay(c *Context) error {
	what := strings.TrimSpace(c.Rest())
	if what == "" {
		c.Send("Say what?")
		return nil
	}
	// Two perspectives: the speaker sees "You say", bystanders see "<Name> says". The
	// say string is data ($t), never a template.
	c.z.act("You say, '$t'", c.Actor, nil, nil, what, "", ToActor)
	c.z.act("$n says, '$t'", c.Actor, nil, nil, what, "", ToRoom)
	return nil
}

// cmdWho lists every player currently online in the zone (MUDLIB §6). Unchanged output.
func cmdWho(c *Context) error {
	c.z.who(c.s)
	return nil
}

// cmdQuit marks a clean, intentional disconnect and closes the stream (MUDLIB §6).
// Behavior preserved from slice 1: it sets quitting, sends "Farewell." + a disconnect,
// and sends NO prompt. It signals dispatch to skip the prompt by flagging ctx.moved —
// the same early-return path movement uses — because after a quit the stream closes and
// the actor must not be re-prompted.
func cmdQuit(c *Context) error {
	c.z.log.Debug("player quit", "player", c.s.character)
	// Mark a clean, intentional disconnect so when the stream drops the zone removes the
	// player immediately instead of waiting out the link-death grace.
	c.s.quitting = true
	c.s.send(textFrame("Farewell."))
	c.s.send(disconnectFrame("quit"))
	c.moved = true // suppress the prompt; the stream will close
	return nil
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
	b.WriteString(r.Long())
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
		// TODO(phase5-visibility): route this presence/name disclosure through canSee/
		// nameFor once dark/invis flags exist — this is a second path past the canSee
		// chokepoint (see who()), not just act()/targeting.
		if Has[*PlayerControlled](occ) {
			b.WriteByte('\n')
			b.WriteString(occ.Name())
			b.WriteString(" is here.")
		}
	}
	s.send(textFrame(b.String()))
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
//
// Slice 2 leaves move's handoff/transfer logic and the released-ownership contract
// untouched; only the departure/arrival broadcast strings now flow through act() (same
// text). dir is already canonical here (the registry binds each movement command to its
// canonical direction).
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
		z.act("$n departs "+dir+".", s.entity, nil, nil, "", "", ToRoom)
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
	z.act("$n leaves "+dir+".", s.entity, nil, nil, "", "", ToRoom) // announced from `from`
	Move(s.entity, to)
	z.act("$n arrives.", s.entity, nil, nil, "", "", ToRoom) // announced from `to`
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
	z.act("$n leaves "+dir+".", s.entity, nil, nil, "", "", ToRoom)
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
	// TODO(phase5-visibility): this online list discloses presence/name bypassing the
	// canSee chokepoint; honor anonymity/invis flags here when they land.
	for _, o := range z.players {
		b.WriteByte('\n')
		b.WriteByte(' ')
		b.WriteString(o.entity.Name())
	}
	s.send(textFrame(b.String()))
}
