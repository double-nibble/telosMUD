package world

import (
	"bytes"
	"context"
	"strconv"
	"strings"
)

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
		{Name: "tell", Run: cmdTell},
		{Name: "reply", Run: cmdReply},
		{Name: "quit", Run: cmdQuit},
	}
	// Container/inventory/equipment verbs, then combat verbs (kill/flee), last: lower priority than
	// movement/look/say so abbreviation precedence (the "n->north not nuke" rule) is unchanged. They
	// live in container.go (Phase 3) and combat_commands.go (Phase 6.3a).
	base = append(base, containerCommands()...)
	base = append(base, combatCommands()...)
	// Comms toggles (Phase 8.6): channels/ignore/afk. Registered last (lowest priority) so they never
	// shadow or abbreviate a movement/look/say verb.
	base = append(base, commsCommands()...)
	// Mail (Phase 8.7): the durable inbox. Registered last with the other comms commands so it never
	// shadows a movement/look/say verb.
	return append(base, mailCommands()...)
}

// cmdLook shows the actor their current room (MUDLIB §6). Slice 2 keeps look's behavior
// identical to slice 1: it always describes the room (targeted `look <thing>` is a slice-4
// concern once items exist). Routes to the existing lookRoom so the output is unchanged.
func cmdLook(c *Context) error {
	// `look <target>` examines a specific entity (an item, a corpse, an occupant); a bare `look` shows
	// the room. Examining a CONTAINER (a corpse, a chest) lists its contents, so `look corpse` reveals
	// the loot before `get all corpse` (Phase 6.3b). Scopes: room items, the floor occupants, and
	// inventory so you can look at what you carry.
	if c.Arg(0) != "" {
		target, ok := c.Target(ScopeRoomItems, ScopeRoomLiving, ScopeInventory)
		if !ok {
			c.Send("You don't see that here.")
			return nil
		}
		c.z.lookAt(c.s, target)
		return nil
	}
	c.z.lookRoom(c.s)
	return nil
}

// lookAt examines a single entity: its long description, and — if it is a CONTAINER (a corpse, a
// chest) — its contents, so a player can see a corpse's loot before looting it. A closed container
// shows as closed (no contents revealed). Pure read; the contents walk is over the entity's own
// contents (zone-goroutine-owned). Single-writer: zone goroutine.
func (z *Zone) lookAt(s *session, target *Entity) {
	var b strings.Builder
	b.WriteString(target.Long())
	if cc, ok := Get[*Container](target); ok {
		if cc.closed {
			b.WriteString("\nIt is closed.")
		} else if len(target.contents) == 0 {
			b.WriteString("\nIt is empty.")
		} else {
			b.WriteString("\nIt holds:")
			// Identical items coalesce to "<Name> (N)" (Track 1); materials/containers list individually.
			for _, line := range coalesceItemLines(target.contents, (*Entity).Name) {
				b.WriteString("\n  ")
				b.WriteString(line)
			}
		}
	}
	s.send(textFrame(b.String()))
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
	// Lua `speech` trigger (7.4c): each scripted mob in the room reacts to what was said. The
	// raw speech is textsan-cleaned inside the ev table. nil-safe / no-op when no scripted mob.
	c.z.fireSpeech(c.Actor, what)
	return nil
}

// cmdWho lists every player online ACROSS ALL SHARDS (docs/PHASE8-PLAN.md slice 8.4). It reads the shared
// presence roster (every shard's residents), not just this zone's players. When presence is disabled (no
// Redis / single-shard run) it falls back to the zone-local list, so the pre-8.4 behavior — and the
// existing who tests — are preserved.
//
// The roster read is blocking Redis I/O, so it must NEVER run on the zone goroutine. We capture the
// session's out channel on-goroutine and spawn a short-lived goroutine for the List + render + write — the
// same off-goroutine discipline the login epoch-resume and async character create use. The async frame is
// written straight to the out channel (ack 0, like a comms frame), so it does not touch the zone-owned
// appliedSeq from another goroutine.
func cmdWho(c *Context) error {
	z := c.z
	out := c.s.out
	if !z.presenceEnabled() {
		z.who(c.s) // zone-local fallback (no roster): unchanged output
		return nil
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), presenceIOTimeout)
		defer cancel()
		entries, ok := z.rosterList(ctx)
		if !ok {
			// A roster read error degrades to the zone-local list — never an error to the player. We post
			// a message back to the zone goroutine so the fallback render stays single-writer.
			z.post(whoFallbackMsg{out: out})
			return
		}
		writeFrameTo(out, textFrame(renderWho(entries)))
	}()
	return nil
}

// presenceEnabled reports whether this zone's shard has a live presence roster (so `who` should read it
// cross-shard rather than the zone-local list). False on a bare zone or a no-Redis run.
func (z *Zone) presenceEnabled() bool {
	return z.shard != nil && z.shard.presence != nil && z.shard.presence.enabled()
}

// cmdTell is `tell <name> <msg>` (Phase 8.5, durable-always): a directed player->player message. It
// delegates to the zone's source tell path (tell.go), which resolves the target via the directory,
// sanitizes, stamps the engine-set author, and PublishDurable's to the durable stream. Tells are
// ENGINE mechanism (no content needed). It never releases ownership, so dispatch prompts on return.
func cmdTell(c *Context) error {
	c.z.cmdTell(c.s, c.Rest())
	return nil
}

// cmdReply is `reply <msg>` (Phase 8.5): a tell to the last player who told YOU this session.
func cmdReply(c *Context) error {
	c.z.cmdReply(c.s, c.Rest())
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

// coalesceItemLines renders a list of items as listing lines, GROUPING identical discrete items into ONE line
// with a " (N)" count (Track 1). `render` maps an item to its base line — `(*Entity).Name` for inventory/
// container listings, the ground long for lookRoom — and each line is presentation-capped (capitalizeFirst).
// The group key is the prototype PLUS the per-instance DELTA (bound state + rolled quality, via dumpItemDelta),
// so a bound or quality-varied item never merges with a plain one (docs/REMAINING.md). Materials (Stack items,
// which already carry their own count) and containers (chests/corpses, whose hidden contents differ) are NEVER
// grouped — each lists on its own line. First-appearance order; a single item is uncounted.
func coalesceItemLines(items []*Entity, render func(*Entity) string) []string {
	type grp struct {
		line string
		n    int
	}
	order := make([]string, 0, len(items))
	groups := map[string]*grp{}
	uniq := 0
	for _, it := range items {
		// The delta key is STABLE because dumpItemDelta serializes via encoding/json, which sorts map keys —
		// so two items with the same rolled-affix map (map[string]float64) produce byte-identical JSON and
		// group. A future refactor to hand-rolled delta serialization must preserve that sorted-key output.
		key := string(it.proto) + "\x00" + string(dumpItemDelta(it))
		if isMaterial(it) || Has[*Container](it) {
			uniq++
			key = "\x00u" + strconv.Itoa(uniq) // a material/container never groups (its own line)
		}
		g := groups[key]
		if g == nil {
			g = &grp{line: capitalizeFirst(render(it))}
			groups[key] = g
			order = append(order, key)
		}
		g.n++
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		if g := groups[key]; g.n > 1 {
			out = append(out, g.line+" ("+strconv.Itoa(g.n)+")")
		} else {
			out = append(out, groups[key].line)
		}
	}
	return out
}

// colorize wraps s in an engine color token + reset (the internal/telnet/color.go `{{TOKEN}}` vocabulary),
// for the engine's default auto-coloring (e.g. exits cyan). It is plain markup TEXT — the gate renders the
// tokens to ANSI SGR downstream of the control-strip, or strips them for a `color off` player — so the world
// never ships raw ESC. token is a bare name like "FG_CYAN"; content authors color room longs the same way.
func colorize(s, token string) string { return "{{" + token + "}}" + s + "{{RESET}}" }

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
		// Engine auto-color (Track 1): exits render cyan. This emits the `{{TOKEN}}` markup the color layer
		// (internal/telnet/color.go) renders to SGR at the edge — or strips for a `color off` player — so it
		// is safe plain-looking text, never raw ESC. Content can layer its own colors in room longs.
		b.WriteString("Exits: ")
		b.WriteString(colorize(strings.Join(ex, ", "), "FG_CYAN"))
	} else {
		b.WriteString("Exits: none")
	}
	// Room contents: render EVERY visible occupant — other players ("X is here"), mobs, ground items,
	// and corpses (a mob/item/corpse's `long` IS its room/ground presence line). Previously only
	// PlayerControlled entities rendered, so mobs and dropped items/corpses were invisible to `look`
	// even though they were really in the room (targeting/`kill` still resolved them) — a render gap.
	// CREATURES (players + mobs) render individually, in room order; GROUND ITEMS are collected and coalesced
	// below so identical items show as one "<long> (N)" line (Track 1). Each line is presentation-capped.
	var groundItems []*Entity
	for _, occ := range r.contents {
		if occ == e {
			continue
		}
		// TODO(phase5-visibility): route this presence/name disclosure through canSee/nameFor once
		// dark/invis flags exist — rendering all contents here is a second path past the canSee
		// chokepoint (see who()), consistent with the existing player-presence disclosure.
		if occ.living == nil && !Has[*PlayerControlled](occ) {
			groundItems = append(groundItems, occ) // a dropped item / corpse — coalesced after the creatures
			continue
		}
		var line string
		if Has[*PlayerControlled](occ) {
			line = occ.Name() + " is here."
		} else if occ.Long() != "" { // a mob: its long line is its room presence
			line = occ.Long()
		} else {
			line = occ.Name() + " is here."
		}
		b.WriteByte('\n')
		b.WriteString(capitalizeFirst(line))
	}
	// Ground items: identical ones coalesce to "<long> (N)" (capitalizeFirst is applied inside the helper).
	for _, line := range coalesceItemLines(groundItems, func(it *Entity) string {
		if it.Long() != "" {
			return it.Long() // a ground item / corpse's long IS its ground-presence line
		}
		return it.Name() + " is here."
	}) {
		b.WriteByte('\n')
		b.WriteString(line)
	}
	s.send(textFrame(b.String()))

	// GMCP Room.Info (Phase 9.3): emit the structured room data alongside the look text so a rich
	// client can update its minimap. Change-detected — re-looking the SAME room doesn't re-emit; only a
	// room CHANGE (movement) does. lookRoom is the single entry/look chokepoint, so this covers arrival,
	// join/attach, and an explicit look.
	if rm := z.roomInfoJSON(r); !bytes.Equal(rm, s.lastRoom) {
		s.lastRoom = rm
		s.send(gmcpFrame("Room.Info", rm))
	}
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
	// Combat exclusion (docs/COMBAT.md invariant; PROTOCOL.md §5): you cannot walk while fighting. This
	// is the ENFORCED gate the combat round driver depends on — it is why no `fighting` pointer or
	// posFighting can ever cross a zone boundary (a cross-zone walk is impossible mid-fight), so the
	// driver never gathers a stale cross-zone entity. `flee` is the sanctioned way out (it stopFights,
	// then a later move is allowed). Applies to EVERY branch below (local / intra-shard / cross-shard).
	if position(s.entity) == posFighting {
		s.send(textFrame("You can't leave while fighting! Flee first."))
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
		if dest := z.shard.zoneByID(destZone); dest != nil {
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
		z.initiateHandoff(s, from, destZone, destRoom, "$n departs "+dir+".")
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
	// OnLeaveRoom checkpoint ([G9], combat.go): fire BEFORE detach so any foe engaged with the leaver
	// can react (an opportunity attack) while both are still live and in-room (the harm gate's fail-
	// closed-on-detached funnel). move() refuses while posFighting, so the leaver here is unengaged; the
	// fire is the general movement checkpoint (a foe with a one-sided fighting link still provokes). A
	// directional `flee` (combat_commands.go) is the engaged-leaver path that exercises the OA milestone.
	moveOrigin := s.entity.location
	z.fireLeaveRoom(nil, s.entity)
	// M1 (distsys review): if a reaction killed the mover, die()->respawnPlayer already relocated them —
	// don't continue the move (it would teleport the respawned player to the move destination). A changed
	// location is the signal (respawn clears posDead). Unengaged movers rarely take a lethal reaction, but
	// a one-sided fighting link can still provoke, so guard the same way as the flee path.
	if s.entity.location != moveOrigin || position(s.entity) == posDead {
		return false
	}
	// Lua `leave` trigger (7.4c): fire on the FROM room BEFORE the move detaches the leaver, so
	// the room can still see them. nil-safe / no-op when the room carries no script.
	z.fireRoomLeave(s.entity, from)
	z.act("$n leaves "+dir+".", s.entity, nil, nil, "", "", ToRoom) // announced from `from`
	Move(s.entity, to)
	z.act("$n arrives.", s.entity, nil, nil, "", "", ToRoom) // announced from `to`
	z.lookRoom(s)
	z.log.Debug("player moved", "player", s.character, "dir", dir, "from", from.proto, "to", destRoom)
	// [G13] room-scoped affects: a creature entering a web/darkness/silence-field room gets it on
	// arrival (the field lands on entrants, not just on those present at cast). Single-writer: the
	// entrant + room are both this zone's (a local move), so this never reaches a cross-zone entity.
	applyRoomAffectsTo(s.entity)
	// Aggro (6.3b): an aggressive mob in the destination room initiates combat on the entrant — a content
	// `aggressive` attribute, not an engine flag (death.go). Done after the arrival look so the player
	// sees the room, then the attack. A local move only; cross-zone arrivals (transferIn) are a later hook.
	z.aggroOnEntry(s.entity, to)
	// EVENT BUS (7.8b): light the new OnEnter movement hook — the event is ABOUT the entrant
	// (subject) entering, with the destination room as the counterpart. Distinct from the per-room/
	// per-mob `enter`/`greet` TRIGGERS below (those are entity scripts; this is the bus a resource/
	// affect on_event or a Lua bus handler subscribes to). Fired independent of Lua presence (an
	// op-list subscriber works in a script-less zone). A clean ROOT fire (a movement, not inside a
	// cascade). "A missing hook is an engine bug" — OnEnter now actually fires.
	z.fireEvent(nil, evOnEnter, s.entity, to, 1)
	// Lua `enter`/`greet` triggers (7.4c): the room reacts to the arrival, and each scripted mob
	// in the room greets the entrant. After aggro so a hostile greeting reads naturally. nil-safe.
	z.fireRoomEntry(s.entity, to)
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
func (z *Zone) transferOut(s *session, dest *Zone, destRoom ProtoRef, dir string, _ *Entity) {
	// Combat exclusion is ENFORCED in move() (refuses while posFighting), so a fighting player never
	// reaches here. disengage anyway, BEFORE detaching from the room: no `fighting` pointer or
	// posFighting may cross to dest's goroutine (the transferred entity would otherwise carry a SOURCE-
	// owned *Entity that dest's round driver would deref), and an opponent left behind must not stay
	// posFighting at a now-departed target. The room scan still finds opponents here (pre-detach).
	z.disengage(s.entity)
	z.act("$n leaves "+dir+".", s.entity, nil, nil, "", "", ToRoom)
	Move(s.entity, nil) // detach from the source room before handing off
	z.delPlayer(s.character)
	// Forward in-flight input to dest until the reader loop observes the new
	// currentZone (which dest.transferIn Stores). dest dedups by appliedSeq.
	z.forwarding[s.character] = dest
	z.log.Debug("intra-shard transfer out", "player", s.character,
		"from_zone", z.id, "to_zone", dest.id, "room", destRoom)
	dest.post(transferInMsg{s: s, room: destRoom})
}

// who lists every player currently online in the zone (the zone-local fallback when presence is
// disabled / a roster read failed). Sends the whoLocal() render to s.
func (z *Zone) who(s *session) {
	s.send(textFrame(z.whoLocal()))
}

// whoLocal builds the zone-local "Players online:" list (this zone's players only). It is the pre-8.4
// who output, kept as the no-roster fallback. Runs on the zone goroutine (reads z.players single-writer).
func (z *Zone) whoLocal() string {
	var b strings.Builder
	b.WriteString("Players online:")
	// TODO(phase5-visibility): this online list discloses presence/name bypassing the
	// canSee chokepoint; honor anonymity/invis flags here when they land.
	for _, o := range z.players {
		b.WriteByte('\n')
		b.WriteByte(' ')
		b.WriteString(o.entity.Name())
	}
	return b.String()
}
