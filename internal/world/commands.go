package world

import (
	"bytes"
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
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
		{Name: "score", Aliases: []string{"sc"}, Run: cmdScore},
		{Name: "who", Run: cmdWho},
		{Name: "tell", Run: cmdTell},
		{Name: "reply", Run: cmdReply},
		{Name: "quit", Run: cmdQuit},
	}
	// Container/inventory/equipment verbs, then combat verbs (kill/flee), last: lower priority than
	// movement/look/say so abbreviation precedence (the "n->north not nuke" rule) is unchanged. They
	// live in container.go (Phase 3) and combat_commands.go (Phase 6.3a).
	base = append(base, containerCommands()...)
	// Non-cardinal movement words (#360): classic MUD "go through a named exit" verbs so a pack can author
	// an exit keyed `enter`/`exit`/`out` (a city gate, a portal). Registered LOW-priority (after the cardinals
	// AND after container verbs) and with NO single-letter alias, so they never win an abbreviation — in
	// particular `o` still resolves to `open`, not `out` (the earlier-registered command wins the prefix; an
	// exact `out`/`enter`/`exit` still matches via byExact). `move()` already dispatches any exit key; these
	// just make the words typeable. A room with no such exit gets the normal "You can't go that way."
	base = append(base,
		&Command{Name: "enter", Run: mv("enter")},
		&Command{Name: "exit", Run: mv("exit")},
		&Command{Name: "out", Run: mv("out")},
	)
	base = append(base, combatCommands()...)
	// Rest/stand (Track 5, #39): registered low-priority (after combat) so rest/sit/stand never shadow
	// a movement/look/say abbreviation.
	base = append(base, restCommands()...)
	// Live-vitals toggle (Track 5, #40): also low-priority.
	base = append(base, vitalsCommands()...)
	// Comms toggles (Phase 8.6): channels/ignore/afk. Registered last (lowest priority) so they never
	// shadow or abbreviate a movement/look/say verb.
	base = append(base, commsCommands()...)
	// Mail (Phase 8.7): the durable inbox. Registered last with the other comms commands so it never
	// shadows a movement/look/say verb.
	base = append(base, mailCommands()...)
	// Audit (#350): the durable permanent-change trail. Bare `audit` is a mortal self-view; `audit <name>`
	// is staff-gated inside the handler. Registered last with the other low-priority commands so it never
	// shadows a movement/look/say verb.
	base = append(base, auditCommands()...)
	// Screen utilities (#31): `clear` — engine-owned raw-ANSI output, universal. Low-priority so `cl` still
	// abbreviates to `close`.
	base = append(base, screenCommands()...)
	// NOTE: `help` (#64) is NOT registered here. Its handler (cmdHelp) transitively reads baseTable to
	// auto-include the command set, so registering it inside this function — which baseTable's initializer
	// calls — would form a package-init cycle. It is appended post-construction via an init() in help.go.
	// Staff verbs (#29 stat, #30 view toggles): registered LAST (lowest priority) and each carries a
	// positive MinRank, so a staff verb is both invisible below that rank (dispatch gate) and never wins an
	// abbreviation against a mortal verb.
	base = append(base, staffCommands()...)
	return append(base, staffToggleCommands()...)
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
// cmdScore shows the actor their character sheet. The layout is CONTENT: if the pack defines a `score` display
// template (a Lua render body built with the `ui` toolkit), it renders that with `self` bound to the actor;
// otherwise it falls back to a minimal built-in sheet (name/level/resources) so the verb works for any pack.
func cmdScore(c *Context) error {
	if sheet, ok := c.z.renderDisplaySheet("score", c.Actor); ok {
		c.Send(sheet)
		return nil
	}
	c.Send(c.z.defaultScoreSheet(c.Actor))
	return nil
}

// cmdWho lists every player online ACROSS ALL SHARDS (docs/PHASE8-PLAN.md slice 8.4). It reads the shared
// presence roster (every shard's residents), not just this zone's players. When presence is disabled (no
// Redis / single-shard run) it falls back to the zone-local list, so the pre-8.4 behavior — and the
// existing who tests — are preserved.
//
// SPLIT ACROSS THE GOROUTINE BOUNDARY (the shape #24 depends on):
//
//   - The roster read is blocking Redis I/O, so it must NEVER run on the zone goroutine. We capture the
//     session's out channel + viewer + see-all bit on-goroutine and spawn a short-lived goroutine for the
//     List — the same off-goroutine discipline the login epoch-resume and async character create use.
//   - The RENDER, by contrast, must run ON the zone goroutine: a content `who` display template enters the
//     zone-owned, one-per-zone Lua VM. So the fetcher does I/O ONLY, then posts its result back to the zone
//     inbox (whoRenderMsg on success, whoFallbackMsg on a read error) and the handler renders + writes there.
//     No frame is ever rendered in the fetch goroutine.
//
// The frame is written straight to the out channel (ack 0, like a comms frame), so it does not touch the
// zone-owned appliedSeq from another goroutine.
func cmdWho(c *Context) error {
	z := c.z
	// Per-session cooldown (docs/REMAINING.md, scale): the shared roster cache already collapses
	// CONCURRENT reads to one SCAN per window; this blunts one session hammering the verb. Runs on
	// the zone goroutine (dispatch), which owns both z.whoCooldown and s.lastWho.
	if z.whoCooldown > 0 {
		if time.Since(c.s.lastWho) < z.whoCooldown {
			c.Send("You just checked; give it a moment.")
			return nil
		}
		c.s.lastWho = time.Now()
	}
	out := c.s.out
	if !z.presenceEnabled() {
		z.who(c.s) // zone-local fallback (no roster): unchanged output
		return nil
	}
	// Capture every zone-owned read HERE, on the zone goroutine — the fetch goroutine must touch no live
	// entity state. #98: the viewer's see-all (holylight) gates whether concealed roster rows are disclosed.
	// The viewer POINTER travels only as the identity the render re-binds on the zone goroutine; the fetch
	// goroutine never dereferences it.
	viewer := c.s.entity
	seeAll := hasFlag(viewer, flagHolylight)
	s := c.s
	// This command is ASYNC: its output is produced by the whoRenderMsg/whoFallbackMsg inbox handler below,
	// not here. Tell dispatch to skip the trailing prompt so it doesn't land BEFORE the async output (#371);
	// the handler emits the prompt itself, after writing the sheet (promptAfterAsync). Set only on this
	// async branch — the cooldown and presence-disabled early returns above are synchronous and prompt normally.
	c.deferPrompt = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), presenceIOTimeout)
		defer cancel()
		entries, ok := z.rosterList(ctx)
		if !ok {
			// A roster read error degrades to the zone-local list — never an error to the player. We post
			// a message back to the zone goroutine so the fallback render stays single-writer.
			z.post(whoFallbackMsg{s: s, out: out, viewer: viewer})
			return
		}
		// Success: hand the (remote, plain-data) roster snapshot back to the zone goroutine, which renders it
		// — through the content `who` template if the pack defines one, else the built-in renderWho.
		z.post(whoRenderMsg{s: s, out: out, viewer: viewer, entries: entries, seeAll: seeAll})
	}()
	return nil
}

// defaultWhoCooldown is the per-session `who` rate limit (zone.whoCooldown, checked in cmdWho). Long
// enough to blunt a spammer, short enough that a human retyping it never notices; the roster cache's
// whoCacheTTL (1s) already amortizes CONCURRENT sessions, so this only guards the per-session path.
const defaultWhoCooldown = 2 * time.Second

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

// itemGroup is one coalesced run of identical items: a REPRESENTATIVE entity (the first of the run, in
// appearance order) and how many items merged into it. It is the structured form of a coalesced listing line,
// so both the built-in renderers (coalesceItemLines) and the room display template (self:room_items(), which
// wants the count as a number rather than baked into a string) read the SAME grouping rule.
type itemGroup struct {
	rep *Entity // the first item of the run — what a listing line names
	n   int     // how many identical items merged (>= 1)
}

// coalesceItems GROUPS identical discrete items (Track 1). The group key is the prototype PLUS the
// per-instance DELTA (bound state + rolled quality, via dumpItemDelta), so a bound or quality-varied item
// never merges with a plain one (docs/REMAINING.md). Materials (Stack items, which already carry their own
// count) and containers (chests/corpses, whose hidden contents differ) are NEVER grouped — each gets its own
// group of one. Groups come back in first-appearance order.
//
// This is THE coalescing rule; every ground/inventory/container listing and the room display template's
// coalesced accessor go through it, so they can never drift apart.
func coalesceItems(items []*Entity) []itemGroup {
	order := make([]string, 0, len(items))
	groups := map[string]*itemGroup{}
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
			g = &itemGroup{rep: it}
			groups[key] = g
			order = append(order, key)
		}
		g.n++
	}
	out := make([]itemGroup, 0, len(order))
	for _, key := range order {
		out = append(out, *groups[key])
	}
	return out
}

// coalesceItemLines renders a list of items as listing lines, GROUPING identical discrete items into ONE line
// with a " (N)" count (Track 1) via coalesceItems. `render` maps an item to its base line — `(*Entity).Name`
// for inventory/container listings, the ground long for lookRoom — and each line is presentation-capped
// (capitalizeFirst). First-appearance order; a single item is uncounted.
func coalesceItemLines(items []*Entity, render func(*Entity) string) []string {
	groups := coalesceItems(items)
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		line := capitalizeFirst(render(g.rep))
		if g.n > 1 {
			line += " (" + strconv.Itoa(g.n) + ")"
		}
		out = append(out, line)
	}
	return out
}

// isCreature reports whether e is a LIVING thing — a mob (a Living) or a player (PlayerControlled) — as
// opposed to a ground item / corpse / container. It is the one predicate that splits a room's contents into
// the two listings `look` renders (occupants vs coalesced ground items), shared with the room display
// template's self:room():occupants() / :room_items() accessors so the split can never drift.
func isCreature(e *Entity) bool {
	return e != nil && (e.living != nil || Has[*PlayerControlled](e))
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

	// Room darkness (#99): a viewer in an unlit dark room sees nothing — not the name, desc, exits, or any
	// occupant. Short-circuit to the pitch-black notice before rendering anything. The occupant loop below
	// already drops individually-concealed occupants via canSee; this handles the room-wide case in one place
	// (and is what carrying/dropping a light source toggles). GMCP Room.Info is still emitted so a rich
	// client's minimap tracks position even when the player can't read the room.
	//
	// This gate sits ABOVE the content template on purpose: darkness is an ENGINE visibility rule, so no pack
	// can template its way around the pitch-black room. (Per-occupant concealment is enforced a second time,
	// independently, inside self:room():occupants() — a template never receives a hidden occupant to render.)
	if !canSeeRoomContents(e) {
		s.send(textFrame("It is pitch black — you can see nothing."))
		z.emitRoomInfo(s, r)
		return
	}

	// Content may template the room sheet (a `render(self)` body reading self:room():exits()/:occupants()/
	// :room_items()); absent one, fall through to the built-in description below. Same render+fallback shape
	// as score/inventory/equipment — a broken template fails closed to the built-in, so `look` always works.
	if sheet, ok := z.renderDisplaySheet("room", e); ok {
		s.send(textFrame(sheet))
		z.emitRoomInfo(s, r)
		return
	}

	// Diagnostic (#361): an overworld room with the `overworld` toggle ON should have rendered the map
	// template above, not fallen through to the built-in render. If it did, surface WHY (a stale/transient
	// state, a deadline, a nil coord) — this fires ONLY for that anomaly, so it is quiet in normal play.
	if r.room != nil && r.room.namedFlags["overworld"] {
		if def := z.toggleDefs().get("overworld"); def != nil && s.comms.toggleEnabled(def) {
			z.log.Warn("overworld map fell back to built-in render (template returned no sheet)",
				"player", s.character, "room", r.proto)
		}
	}

	room := r.room // its Room component (direct-pointer hot path, MUDLIB §3)
	var b strings.Builder
	b.WriteString(r.Name())
	b.WriteByte('\n')
	b.WriteString(r.Long())
	b.WriteByte('\n')
	if ex := room.displayExits(); len(ex) > 0 {
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
		// Route presence/name disclosure through the canSee chokepoint (#28): an occupant the viewer
		// can't perceive (invisible, no detect) is OMITTED from the room listing entirely — a builder
		// with holylight still sees it. Ground items always show (they carry no viewer concealment).
		if isCreature(occ) && !z.canSee(e, occ) {
			continue
		}
		if !isCreature(occ) {
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
	z.emitRoomInfo(s, r)
}

// emitRoomInfo emits GMCP Room.Info (Phase 9.3) for room r: the structured room data alongside the look text
// so a rich client can update its minimap. Change-detected — re-looking the SAME room doesn't re-emit; only a
// room CHANGE (movement) does. lookRoom is the single entry/look chokepoint, so this covers arrival,
// join/attach, and an explicit look, on ALL THREE of its render paths (pitch-black, content template, and the
// built-in description) — the minimap must track position even when the player can't read the room.
// Single-writer: zone goroutine (it writes s.lastRoom).
func (z *Zone) emitRoomInfo(s *session, r *Entity) {
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
// isRoomExit reports whether the player's current room has an exit keyed `dir` (#370). It backs the
// named-exit dispatch fall-through (parser.go): a content exit keyword becomes typeable as a movement verb.
// It checks the ordinary `exits` map ONLY — never `entrances` (dungeon doors, whose crossing is the
// instance-entry security path, reached exclusively through registered direction verbs, never an arbitrary
// typed keyword). Pure read of zone-owned state; zone goroutine. False for a session with no live entity/room.
func (z *Zone) isRoomExit(s *session, dir string) bool {
	if s == nil || s.entity == nil {
		return false
	}
	from := s.entity.location
	if from == nil || from.room == nil {
		return false
	}
	_, ok := from.room.exits[dir]
	return ok
}

// maxTraverseRedirects bounds how many times a `traverse` hook may redirect a single move (#370) before the
// engine refuses further redirection — a cheap loop/cycle backstop (a hook that redirects grove->gate->grove,
// or one that redirects unconditionally), analogous to the alias-expansion depth cap. At the floor a further
// redirect is refused as a block ("hopelessly turned around").
const maxTraverseRedirects = 3

// move traverses `dir` from the player's current room — the player-command movement entry (the mv() verbs and
// the named-exit dispatch fall-through). It is the depth-0 entry to attemptMove; a `traverse` hook's redirect
// re-enters attemptMove with a decremented redirect budget. Returns true only when it RELEASED OWNERSHIP of
// the session (an intra-shard transfer / cross-shard handoff).
func (z *Zone) move(s *session, dir string) bool {
	return z.attemptMove(s, dir, maxTraverseRedirects)
}

func (z *Zone) attemptMove(s *session, dir string, redirectsLeft int) bool {
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
	// Auto-stand: a resting player gets to their feet before walking (rest.go, #39). Standing here (not
	// refusing the move) keeps movement frictionless — you don't have to `stand` then walk.
	if position(s.entity) == posResting {
		setPosition(s.entity, posStanding)
		s.send(textFrame("You stop resting and stand up."))
		z.act("$n stops resting and stands up.", s.entity, nil, nil, "", "", ToRoom)
	}
	from := s.entity.location // the current room entity
	ref, ok := from.room.exits[dir]
	if !ok {
		// An INSTANCE ENTRANCE — a dungeon door (#435). Checked only after the ordinary exits miss, so that
		// content which somehow declared a direction as both (the loader rejects it) fails toward the exit
		// rather than toward minting.
		//
		// THIS IS THE WHOLE SECURITY DESIGN OF THE FEATURE, so it is worth being explicit about why it is
		// here and not in a trigger. The crossing is a MOVEMENT the player typed, so the player is the actor:
		// requestInstanceEntry is reached with the mover as both the invoker and the target, which satisfies
		// mud.send_to_instance's self-only rule STRUCTURALLY — there is no third party in the call frame to
		// exclude, and no new gate that could be got wrong. The mint is billed to the mover's own account
		// rather than to a victim's, which the trigger-based alternative could not have guaranteed.
		//
		// It also means no path that moves a player on ANOTHER party's initiative can reach a dungeon door:
		// hMove, directional flee, and every other mover resolve a direction through `exits`, which does not
		// contain entrances. IF A FOLLOW MECHANIC IS EVER ADDED it becomes the first path that moves a player
		// through a direction on someone else's initiative, and it MUST refuse an entrance direction or this
		// property collapses silently.
		if tmpl, isDoor := from.room.entrances[dir]; isDoor {
			// Every refusal — not instanceable, caps, rate limit, no nesting, no verified account, a mint
			// already pending, a draining shard — is requestInstanceEntry's or the async mint's, and each
			// already speaks to the player. `true` is the gate decision: the mover IS the invoking actor.
			z.requestInstanceEntry(s, tmpl, true)
			// FALSE, deliberately: `move` returns true only when it RELEASED OWNERSHIP of the session. This
			// released nothing. The player is still standing in this room and stays fully live — the actual
			// transferOut happens hops later, from instanceReady, on this same goroutine. Returning true
			// would mark the command as having moved and suppress their prompt until then.
			return false
		}
		z.log.Debug("move blocked: no exit", "player", s.character, "room", from.proto, "dir", dir)
		s.send(textFrame("You can't go that way."))
		return false
	}
	destZone, destRoom := parseRef(ref)

	// Cancellable traverse hook (#370): fire the FROM room's `traverse` trigger BEFORE any transfer branch
	// (local / intra-shard / cross-shard), so content can BLOCK, message, or redirect a move — a guard
	// stepping in, a locked gate, a quest gate — including a cross-zone move. Placed here (after the exit
	// resolves, before the branch split) precisely so it gates every destination uniformly. It runs on the
	// zone goroutine before the move commits; nil-safe / allow when the room carries no `traverse` handler.
	gateGen := deathGen(s.entity)
	gateOrigin := s.entity.location
	blocked, msg, redirectDir := z.fireCanExit(s.entity, from, dir, string(ref))
	// REDIRECT takes precedence: the hook asked to send the mover through a DIFFERENT exit of this same room
	// (a portal, a confusion effect). Re-attempt the move via that exit, bounded by the redirect budget so a
	// hook that redirects unconditionally — or a redirect cycle — cannot recurse without end. At the floor a
	// further redirect is refused as a block. A redirect to the SAME exit is ignored (treated as no redirect)
	// so a hook can `redirect(ev.dir)` harmlessly. The mover is unmoved here, so the re-attempt re-resolves
	// from the same room with the new direction.
	if redirectDir != "" && redirectDir != dir {
		if redirectsLeft <= 0 {
			s.send(textFrame("You are hopelessly turned around and can't make any progress."))
			z.log.Debug("move redirect budget exhausted", "player", s.character, "room", from.proto, "dir", dir)
			return false
		}
		z.log.Debug("move redirected by traverse hook", "player", s.character, "from_dir", dir, "to_dir", redirectDir)
		return z.attemptMove(s, redirectDir, redirectsLeft-1)
	}
	if blocked {
		if msg == "" {
			msg = "You can't go that way."
		}
		s.send(textFrame(msg))
		z.log.Debug("move blocked by traverse hook", "player", s.character, "room", from.proto, "dir", dir)
		return false
	}
	// The hook may have relocated or killed the mover — a co-located guard mob it revealed that then acted, a
	// future living-actor context. If so, ABANDON this move so we don't teleport the relocated/respawned player
	// onward: the hook's outcome stands. This mirrors the fireLeaveRoom re-check below; the deathGen check
	// catches a lethal reaction that respawned the mover in place (location unchanged). A bare room hook (actor
	// has no Living) cannot itself harm/relocate the player — the harm gate fails closed — so this is
	// forward-looking defense-in-depth on the single move funnel, not a hot path today.
	if deathGen(s.entity) != gateGen || s.entity.location != gateOrigin || position(s.entity) == posDead {
		return false
	}

	// Intra-shard cross-zone move: the destination is a DIFFERENT zone that THIS shard
	// also hosts. Transfer the player in-process — no handoff, no snapshot, no epoch
	// bump, no directory change, no gate re-dial. The session keeps the same out channel
	// and appliedSeq. transferOut hands the session+entity to dest, so we release ownership.
	// Resolve AND claim the destination in one hold of the shard mutex (#409): a bare zoneByID here leaves a
	// window in which a concurrent UnhostZone tears the destination down before transferOut's handover is
	// claimed, dropping the handover on a dead inbox. A nil claim means the zone is no longer hosted here, so
	// this falls through to the cross-shard branch with nothing yet mutated.
	//
	// ownsZoneRef, not a raw `!= z.id`: an instance (#72) hosts its template's AUTHORED room refs, so every
	// exit naming a template room stays inside the instance and an exit naming any other zone leaves normally.
	// That single predicate is what makes an instance a closed copy of its template.
	if !z.ownsZoneRef(destZone) && z.shard != nil {
		if dest := z.shard.claimTransferTarget(destZone); dest != nil {
			z.transferOut(s, dest, destRoom, "$n leaves "+dir+".")
			return true
		}
	}

	// Cross-shard (cross-zone) move: hand the player off rather than moving locally.
	if !z.ownsZoneRef(destZone) {
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
	moveGen := deathGen(s.entity)
	z.fireLeaveRoom(nil, s.entity)
	// M1 (distsys review): if a reaction killed the mover, die()->respawnPlayer already revived them — don't
	// continue the move (it would teleport the respawned player to the move destination). Unengaged movers
	// rarely take a lethal reaction, but a one-sided fighting link can still provoke.
	//
	// deathGen is the load-bearing check (#69, combat review): a mover slain while standing in the START
	// ROOM respawns IN PLACE, so location is unchanged and posDead is already cleared — both weaker signals
	// read "fine" and the move proceeded, walking a just-respawned player back out of the temple. The
	// location check still earns its place: it also catches a NON-lethal forced relocation (a knockback
	// reaction), which is likewise a reason to abandon the move.
	if deathGen(s.entity) != moveGen || s.entity.location != moveOrigin || position(s.entity) == posDead {
		return false
	}
	// Lua `leave` trigger (7.4c): fire on the FROM room BEFORE the move detaches the leaver, so
	// the room can still see them. nil-safe / no-op when the room carries no script.
	z.fireRoomLeave(s.entity, from)
	z.actConceal("$n leaves "+dir+".", s.entity, ToRoom) // announced from `from`; #100: silent to those who can't see the mover
	Move(s.entity, to)
	z.actConceal("$n arrives.", s.entity, ToRoom) // announced from `to`; #100: silent to those who can't see the mover
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
	// Lua `witness_leave` trigger (#202): each scripted mob left behind in `from` learns the mover departed
	// and WHICH WAY (ev.dir), so a chaser can follow (self:move(ev.dir)). Fired last, after the move fully
	// completes, so a follower relocates toward the mover's destination. Distinct from the room's `leave`.
	z.fireWitnessLeave(s.entity, from, dir)
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
//
// departMsg is the act() line the room left behind sees ("$n leaves east.", "$n vanishes."). It is a full
// message rather than a direction because #72 added two callers that are not walks: stepping into an instance
// and being evicted out of one, neither of which has a direction to name.
func (z *Zone) transferOut(s *session, dest *Zone, destRoom ProtoRef, departMsg string) {
	// Own the claim move() took on dest, so it is released on EVERY exit from this function and not only the
	// one that reaches the send (#409). Only transferIn releases a claim otherwise, and it never runs if we
	// leave without posting — a panic below is recovered by dispatchSafe/handle, which run no compensator. A
	// leaked claim is permanent: dest can never be unhosted or rebalanced again, and every later BeginDrain on
	// this process burns its whole deadline waiting for a quiescence that will never come.
	//
	// The SAME compensator releases the mid-transfer residency mark taken below (#379), because the mark's
	// lifetime is deliberately this claim's: one lifetime, one audited pair of release sites (here and
	// transferIn's defer), so a future path cannot leak the mark without also leaking `incoming` — which is
	// loud (releaseInboundArrival's underflow report) and wedges the zone. See markTransferInFlight.
	posted := false
	defer func() {
		if !posted {
			dest.incoming.Add(-1)
			if z.shard != nil {
				z.shard.clearTransferInFlight(s.character)
			}
		}
	}()
	// Combat exclusion is ENFORCED in move() (refuses while posFighting), so a fighting player never
	// reaches here. disengage anyway, BEFORE detaching from the room: no `fighting` pointer or
	// posFighting may cross to dest's goroutine (the transferred entity would otherwise carry a SOURCE-
	// owned *Entity that dest's round driver would deref), and an opponent left behind must not stay
	// posFighting at a now-departed target. The room scan still finds opponents here (pre-detach).
	z.disengage(s.entity)
	if departMsg != "" {
		z.actConceal(departMsg, s.entity, ToRoom) // #100: silent to those who can't see the mover
	}
	Move(s.entity, nil) // detach from the source room before handing off
	// Mark the character mid-transfer BEFORE removing them from this zone (#379), so there is no instant in
	// which the shard's residency index answers "nowhere" for a session that is very much alive. From here
	// until transferIn (or the compensator above) a token=="" reconnect is REFUSED with Unavailable rather
	// than falling through to the stale durable zone_ref and fresh-logging a second copy — see
	// markTransferInFlight for the full lifetime argument.
	//
	// The mark is only-if-mine, so it can decline. That is unreachable today (we are on z's goroutine, for a
	// session in z.players, which setPlayer indexed to z) and it is not fatal when it happens — Zone.attach's
	// delivery-time residency check is the actual double-own net, and this refusal only buys the earlier,
	// epoch-preserving refusal — but it means the index disagrees with z.players about who we hold, which
	// nothing else would ever report.
	if z.shard != nil && !z.shard.markTransferInFlight(s.character, z) {
		z.log.Error("BUG: transferring a character this zone's residency index does not attribute to us; "+
			"a reconnect racing this transfer loses its early refusal (#379)",
			"player", s.character, "from_zone", z.id, "to_zone", dest.id)
	}
	z.delPlayer(s.character)
	// Forward in-flight input to dest until the reader loop observes the new
	// currentZone (which dest.transferIn Stores). dest dedups by appliedSeq.
	z.forwarding[s.character] = dest
	z.log.Debug("intra-shard transfer out", "player", s.character,
		"from_zone", z.id, "to_zone", dest.id, "room", destRoom)
	// The claim on dest was taken atomically with resolving it (claimTransferTarget, #409); from here it is
	// transferIn's to release on every path, so disarm our compensator.
	posted = true
	dest.postTransferIn(s, destRoom)
}

// who lists every player currently online in the zone (the zone-local fallback when presence is
// disabled / a roster read failed). Sends the whoLocalSheet() render to s.
func (z *Zone) who(s *session) {
	s.send(textFrame(z.whoLocalSheet(s.entity)))
}

// whoLocalSheet renders the ZONE-LOCAL who list — through the pack's `who` display template if it defines
// one, else the built-in whoLocal listing. The template sees the same (self, list) binding as the cross-shard
// path (#24), so a player's `who` looks the SAME whether the shard reads the presence roster or has degraded
// to the local list; only the row SET differs. Runs on the zone goroutine (it walks z.players and enters the
// zone-owned Lua VM), which is where both callers live: cmdWho's presence-disabled branch and the
// whoFallbackMsg inbox handler.
//
// SECURITY: rows are pre-filtered by the canSee chokepoint (whoLocalRecords), exactly like whoLocal — a
// concealed player never becomes a record, so no template can disclose one.
func (z *Zone) whoLocalSheet(viewer *Entity) string {
	if sheet, ok := z.renderDisplayList("who", viewer, z.whoLocalRecords(viewer)); ok {
		return sheet
	}
	return z.whoLocal(viewer)
}

// whoLocalRecords snapshots this zone's visible players as plain-data `who` rows, in the same shape the
// cross-shard roster produces (renderWhoSheet): {name, shard, afk, concealed}. The visibility rule is
// whoLocal's — the viewer always sees themselves, everyone else must pass canSee. Sorted by name so the
// local and cross-shard lists order identically.
func (z *Zone) whoLocalRecords(viewer *Entity) []*displayRecord {
	// shardID is write-once at WithPresence (before any zone goroutine runs), so this is a safe lock-free read.
	shardID := ""
	if z.shard != nil && z.shard.presence != nil {
		shardID = z.shard.presence.shardID
	}
	// A slice, not a name-keyed map: two sessions must never collapse into one row just because they render
	// the same display name (whoLocal lists one line per SESSION).
	type row struct {
		name string
		afk  bool
	}
	rows := make([]row, 0, len(z.players))
	for _, o := range z.players {
		if o.entity != viewer && !z.canSee(viewer, o.entity) {
			continue // THE chokepoint (#28): concealed players never become a record
		}
		rows = append(rows, row{name: o.entity.Name(), afk: playerAFK(o)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	out := make([]*displayRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, newDisplayRecord().
			str("name", r.name).
			str("shard", shardID).
			boolean("afk", r.afk).
			boolean("concealed", false)) // a concealed player was filtered out above
	}
	return out
}

// whoLocal builds the zone-local "Players online:" list (this zone's players only), as seen BY viewer. It
// is the pre-8.4 who output, kept as the no-roster fallback. Runs on the zone goroutine (reads z.players
// single-writer). A player the viewer can't perceive (invisible, no detect) is OMITTED (#28) — a holylight
// viewer still sees everyone. (The CROSS-SHARD roster path renderWho is a separate follow-up: it needs the
// presence Entry to carry a concealment bit.)
func (z *Zone) whoLocal(viewer *Entity) string {
	var b strings.Builder
	b.WriteString("Players online:")
	for _, o := range z.players {
		if o.entity != viewer && !z.canSee(viewer, o.entity) {
			continue
		}
		b.WriteByte('\n')
		b.WriteByte(' ')
		b.WriteString(o.entity.Name())
	}
	return b.String()
}
