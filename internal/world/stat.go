package world

// stat.go — the staff inspection command `stat [<target>]` (#29, Track 2). A trusted staff member examining
// a thing sees the INSTANCE + PROTOTYPE identity and the internal state an ordinary player never does: the
// runtime id, the prototype/content key ("vnum"), whether it is flyweight-backed, the component set, the
// LIVE flag set (including the reserved trust flags dumpFlags hides from the persistence boundary), and the
// serialized entity state (dumpStateComponents, the same shape the durable save carries). It is the first
// TRUST-GATED WORLD command — registered with MinRank (parser.go), so dispatch makes the verb invisible to
// anyone below staff rank. Read-only: it resolves and renders, mutating nothing.
//
// Two trust checks, both content-defined via the ladder (#29 Slice 0):
//   - USE gate (dispatch, MinRank=rankStaff): the actor's account tier must resolve to a POSITIVE rank —
//     any tier above the baseline. A player (rank 0) can't see or run the verb at all.
//   - TARGET gate (here): you may only inspect a target whose trust rank is <= your own — so a builder
//     can stat players/mobs/items but not an admin. A mob/item/room has no tier (baseline rank 0), so it
//     is always inspectable by any staff member.
//
// Targeting: a bare `stat` inspects the actor's current room; `stat me`/`stat self` the actor; otherwise the
// argument resolves across the actor's room (living + ground items), inventory, and equipment. Resolution
// runs through the same canSee chokepoint as any command; staff tiers carry holylight (#28), so an invisible
// mob is still inspectable — exactly the see-all the ladder grants.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// rankStaff is the minimum trust rank for a staff verb: any tier above the baseline (rank 0). Content-
// defined ladders always place the baseline at rank 0 (tier.go), so "> 0" is "any staff" independent of a
// pack's specific tier names.
const rankStaff = 1

// staffCommands returns the trust-gated staff verbs (#29+). Registered LAST in registerCommands (lowest
// priority) so a staff verb never shadows or abbreviates a mortal verb; each carries MinRank (dispatch
// hides it below that rank) + CmdHidden (kept out of help). `stat` is the inspection verb; more land later.
func staffCommands() []*Command {
	return []*Command{
		{Name: "stat", MinRank: rankStaff, Flags: CmdHidden, Run: cmdStat},
		// `reload [<pack>]` (#53): propagate a content hot-reload across the fleet (reloadcmd.go). Staff-
		// gated + hidden like `stat`; registered here so it never shadows/abbreviates a mortal verb.
		{Name: "reload", MinRank: rankStaff, Flags: CmdHidden, Run: cmdReload},
		// `pull <version>` (#212 slice 4 PR E): request a director-coordinated install of a PUBLISHED
		// content version from the external store (pullcmd.go). Staff-gated + hidden like the others.
		{Name: "pull", MinRank: rankStaff, Flags: CmdHidden, Run: cmdPull},
	}
}

// cmdStat renders the inspection sheet for a target entity (#29). Bare -> the current room; `me`/`self` ->
// the actor; otherwise a keyword resolve over room/inventory/equipment. The USE gate (positive rank) is
// enforced in dispatch, so reaching this handler already implies staff trust; here we enforce the TARGET
// gate — you may not inspect a target that outranks you.
func cmdStat(c *Context) error {
	arg := strings.ToLower(c.Arg(0))
	var target *Entity
	switch arg {
	case "":
		target = c.Actor.location // the room the staff member stands in (nil only for a room-less actor)
	case "me", "self":
		target = c.Actor
	default:
		if t, ok := c.Target(ScopeRoomLiving, ScopeRoomItems, ScopeInventory, ScopeEquipment); ok {
			target = t
		}
	}
	if target == nil {
		c.Send("stat: nothing here by that name.")
		return nil
	}
	// TARGET gate: refuse to inspect a target whose trust rank exceeds the actor's own (a builder can't
	// stat an admin). Self is always allowed (equal rank). A mob/item/room is baseline rank 0.
	ladder := c.z.trustLadder()
	if target != c.Actor && ladder.rank(entityTier(target)) > ladder.rank(c.s.tier) {
		c.Send("stat: you cannot inspect someone of a higher trust tier.")
		return nil
	}
	c.Send(statSheet(target))
	return nil
}

// entityTier returns the account trust tier carried by an entity's session (a player), or "" (the baseline)
// for a mob/item/room or a session-less entity. The reverse entity->session link is the PlayerControlled
// component (session.go). Used for the stat TARGET-rank comparison.
func entityTier(e *Entity) string {
	if pc, ok := Get[*PlayerControlled](e); ok && pc.session != nil {
		return pc.session.tier
	}
	return ""
}

// statSheet builds the multi-line inspection dump for e. Kept pure (a string builder over accessors) so it
// is unit-testable without a session. Order: identity header, prototype/flyweight, component set, live flags,
// then the entity-kind-specific body (room exits/flags, or Living vitals + the serialized state subtree).
func statSheet(e *Entity) string {
	var b strings.Builder
	name := e.Name()
	if name == "" {
		name = "(unnamed)"
	}
	fmt.Fprintf(&b, "{{CYAN}}%s{{RESET}}  [rid #%d]\r\n", name, e.RuntimeID())

	proto := string(e.proto)
	if proto == "" {
		proto = "(none — instance-authored)"
	}
	flyweight := "no"
	if e.prototype != nil {
		flyweight = "yes"
	}
	fmt.Fprintf(&b, "proto: %s   flyweight: %s\r\n", proto, flyweight)

	if kw := e.keywordList(); len(kw) > 0 {
		fmt.Fprintf(&b, "keywords: %s\r\n", strings.Join(kw, " "))
	}
	fmt.Fprintf(&b, "components: %s\r\n", strings.Join(componentNames(e), ", "))

	if flags := statFlags(e); flags != "" {
		fmt.Fprintf(&b, "flags: %s\r\n", flags)
	}

	switch {
	case e.room != nil:
		statRoomBody(&b, e)
	case e.living != nil:
		statLivingBody(&b, e)
	}
	return strings.TrimRight(b.String(), "\r\n")
}

// componentNames lists the entity's component kinds as sorted short type names ("Living", "Physical", …),
// stripping the "*world." prefix reflect.Type.String reports. Sorted so the line is stable across runs
// (a componentSet is a map, unordered). "(none)" for a bare entity with no components.
func componentNames(e *Entity) []string {
	if len(e.comps) == 0 {
		return []string{"(none)"}
	}
	names := make([]string, 0, len(e.comps))
	for t := range e.comps {
		names = append(names, strings.TrimPrefix(t.String(), "*world."))
	}
	sort.Strings(names)
	return names
}

// statFlags renders the entity's LIVE flag set — unlike dumpFlags it does NOT hide the reserved trust
// flags (holylight/builder/admin): showing a builder the real elevation state is the whole point of stat.
// Reserved flags are marked with a trailing "*" so their special (tier-derived, non-persisted) nature is
// visible. Sorted for stability; "" when the entity carries no flags.
func statFlags(e *Entity) string {
	if e.living == nil || len(e.living.flags) == 0 {
		return ""
	}
	names := make([]string, 0, len(e.living.flags))
	for name := range e.living.flags {
		if reservedFlag(name) {
			name += "*"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// statRoomBody appends room-specific inspection lines: the sorted exit table (direction -> destination
// ProtoRef) and the room's authored named flags. A room's display text is already shown by `look`, so stat
// focuses on the routing/flag internals a player never sees.
func statRoomBody(b *strings.Builder, e *Entity) {
	r := e.room
	if len(r.exits) > 0 {
		dirs := make([]string, 0, len(r.exits))
		for d := range r.exits {
			dirs = append(dirs, d)
		}
		sort.Strings(dirs)
		var parts []string
		for _, d := range dirs {
			parts = append(parts, d+"->"+string(r.exits[d]))
		}
		fmt.Fprintf(b, "exits: %s\r\n", strings.Join(parts, "  "))
	}
	if len(r.namedFlags) > 0 {
		names := make([]string, 0, len(r.namedFlags))
		for name := range r.namedFlags {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(b, "room-flags: %s\r\n", strings.Join(names, ", "))
	}
}

// statLivingBody appends the Living inspection body: position, the conventional vitals, and the serialized
// state subtree (dumpStateComponents marshaled indented — the SAME shape the durable save carries, so a
// builder sees exactly what persists). A marshal error is reported inline rather than dropped.
func statLivingBody(b *strings.Builder, e *Entity) {
	fmt.Fprintf(b, "position: %s\r\n", positionName(position(e)))
	fmt.Fprintf(b, "vitals: hp %d/%d  mana %d/%d\r\n", e.HP(), e.MaxHP(), e.Mana(), e.MaxMana())
	st := dumpStateComponents(e)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		fmt.Fprintf(b, "state: (marshal error: %s)\r\n", err.Error())
		return
	}
	// Normalize to CRLF so the JSON block renders cleanly over telnet like the rest of the sheet.
	b.WriteString("state:\r\n")
	b.WriteString(strings.ReplaceAll(string(raw), "\n", "\r\n"))
	b.WriteString("\r\n")
}

// positionName maps a Position to its display word for the stat sheet. Local to stat.go — the enum
// (position.go) is an engine mechanic with no content-facing name elsewhere.
func positionName(p Position) string {
	switch p {
	case posStanding:
		return "standing"
	case posResting:
		return "resting"
	case posSleeping:
		return "sleeping"
	case posFighting:
		return "fighting"
	case posDead:
		return "dead"
	default:
		return "unknown"
	}
}
