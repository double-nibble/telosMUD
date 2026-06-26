package world

import "strings"

// Containers, inventory, and equipment commands (docs/MUDLIB.md §3, §4, §6, §7) — the
// Phase-3 milestone: a player can get / drop / put / wear / wield / hold / remove items and
// others see the correct act() perspective messages. Plus open/close on a container, which
// is the first command that mutates a PROTOTYPE-SHARED component (Container.closed) on a
// spawned instance and so MUST go through the slice-3 copy-on-write entry point
// (mutableComponent) — see cmdOpen/cmdClose (Finding 6).
//
// The model (PHASE3-PLAN.md slice 4):
//
//   - INVENTORY is just the player entity's uniform containment: a carried item is an
//     entity in the player's contents. get/drop/put are pure containment Moves on the zone
//     goroutine — no COW (the player has prototype==nil; the items only change location).
//   - EQUIPMENT is worn-slot STATE on the wearer: a *Wearer component on the player maps a
//     slot to the item entity. A worn item STAYS in inventory (equipped is a state over a
//     carried item, classic Diku), so it is reachable as both inventory and equipment.
//   - Targeting scopes (MUDLIB §7) decide where each verb looks: get on the floor, drop in
//     inventory, remove in equipment, get/put-from/in an explicit container.
//
// Every handler runs on the zone goroutine via dispatch, so all the containment/slot
// mutation below is lock-free (MUDLIB §4).

// containerCommands returns the container/inventory/equipment verbs, appended to the base
// table by registerCommands. They are LOWER priority than movement/look/say (registered
// after them) so single-letter movement still wins abbreviation; "get"/"wear"/etc. are
// multi-letter and unambiguous.
func containerCommands() []*Command {
	return []*Command{
		{Name: "inventory", Aliases: []string{"i", "inv"}, Run: cmdInventory},
		{Name: "equipment", Aliases: []string{"eq"}, Run: cmdEquipment},
		{Name: "get", Aliases: []string{"take"}, Run: cmdGet},
		{Name: "drop", Run: cmdDrop},
		{Name: "put", Run: cmdPut},
		{Name: "wear", Run: cmdWear},
		{Name: "wield", Run: cmdWield},
		{Name: "hold", Run: cmdHold},
		{Name: "remove", Run: cmdRemove},
		{Name: "open", Run: cmdOpen},
		{Name: "close", Run: cmdClose},
	}
}

// cmdInventory lists what the actor is carrying (MUDLIB §6). A worn item is still in
// inventory but is shown by equipment, so inventory lists only the items NOT in a worn slot
// — the "in your hands/pack" set — matching player expectation. Pure read; no mutation.
func cmdInventory(c *Context) error {
	wr, _ := Get[*Wearer](c.Actor)
	var b strings.Builder
	b.WriteString("You are carrying:")
	n := 0
	for _, item := range c.Actor.contents {
		if wr != nil && wr.slotOf(item) != WearLocNone {
			continue // shown under equipment
		}
		b.WriteByte('\n')
		b.WriteString("  ")
		b.WriteString(item.Name())
		n++
	}
	if n == 0 {
		b.WriteString("\n  Nothing.")
	}
	c.Send(b.String())
	return nil
}

// cmdEquipment lists what the actor is wearing/wielding/holding by slot (MUDLIB §6). Reads
// the Wearer slot map in canonical slot order. Pure read.
func cmdEquipment(c *Context) error {
	wr, ok := Get[*Wearer](c.Actor)
	var b strings.Builder
	b.WriteString("You are using:")
	n := 0
	if ok {
		for _, loc := range wornOrder {
			item := wr.worn[loc]
			if item == nil {
				continue
			}
			b.WriteByte('\n')
			b.WriteString("  <")
			b.WriteString(wearLocName[loc])
			b.WriteString("> ")
			b.WriteString(item.Name())
			n++
		}
	}
	if n == 0 {
		b.WriteString("\n  Nothing.")
	}
	c.Send(b.String())
	return nil
}

// cmdGet picks an item up off the floor, or out of a container with `get <item> from
// <container>` (MUDLIB §6, §7). Scopes: a bare get searches the room floor (ScopeRoomItems);
// a get-from searches the named container's contents. Both are pure containment Moves into
// the actor's inventory — no COW (only location changes). The container itself is found on
// the floor or in inventory.
func cmdGet(c *Context) error {
	if c.Arg(0) == "" {
		c.Send("Get what?")
		return nil
	}
	// `get <item> from <container>`: split on the "from" keyword.
	if item, cont, ok := splitFrom(c.Rest()); ok {
		return c.z.getFrom(c, item, cont)
	}
	target, ok := c.Target(ScopeRoomItems)
	if !ok {
		c.Send("You don't see that here.")
		return nil
	}
	if target == c.Actor {
		c.Send("You can't get yourself.")
		return nil
	}
	Move(target, c.Actor)
	c.z.act("You get $p.", c.Actor, target, nil, "", "", ToActor)
	c.z.act("$n gets $p.", c.Actor, target, nil, "", "", ToRoom)
	c.z.log.Debug("cmd get", "player", c.s.character, "item", target.proto)
	return nil
}

// getFrom handles `get <item> from <container>`: resolve the container (floor or inventory),
// reject if closed, resolve the item within it, and Move it into inventory.
func (z *Zone) getFrom(c *Context, item, cont string) error {
	containers := c.z.Resolve(c.Actor, parseTargetSpec(cont), ScopeRoomItems, ScopeInventory)
	if len(containers) == 0 {
		c.Send("You don't see that container.")
		return nil
	}
	box := containers[0]
	cc, isContainer := Get[*Container](box)
	if !isContainer {
		c.z.act("$p is not a container.", c.Actor, box, nil, "", "", ToActor)
		return nil
	}
	if cc.closed {
		c.z.act("$p is closed.", c.Actor, box, nil, "", "", ToActor)
		return nil
	}
	matches := c.z.resolveInContainer(c.Actor, box, parseTargetSpec(item))
	if len(matches) == 0 {
		c.z.act("You don't see that in $p.", c.Actor, box, nil, "", "", ToActor)
		return nil
	}
	for _, m := range matches {
		Move(m, c.Actor)
		c.z.act2("You get $p from $P.", c.Actor, m, box, nil, "", "", ToActor)
		c.z.act2("$n gets $p from $P.", c.Actor, m, box, nil, "", "", ToRoom)
	}
	c.z.log.Debug("cmd get-from", "player", c.s.character, "container", box.proto, "n", len(matches))
	return nil
}

// cmdDrop drops a carried item to the room floor (MUDLIB §6). Scope: inventory only — you
// can't drop what you don't hold. A worn item is implicitly removed first (you can't drop
// something you're wearing without it leaving the slot). Pure containment Move.
func cmdDrop(c *Context) error {
	if c.Arg(0) == "" {
		c.Send("Drop what?")
		return nil
	}
	target, ok := c.Target(ScopeInventory)
	if !ok {
		c.Send("You aren't carrying that.")
		return nil
	}
	// Clear any worn slot the item occupies before it leaves inventory (so the slot map
	// never points at an item that is no longer carried).
	if wr, ok := Get[*Wearer](c.Actor); ok {
		if loc := wr.slotOf(target); loc != WearLocNone {
			delete(wr.worn, loc)
		}
	}
	Move(target, c.Actor.location)
	c.z.act("You drop $p.", c.Actor, target, nil, "", "", ToActor)
	c.z.act("$n drops $p.", c.Actor, target, nil, "", "", ToRoom)
	c.z.log.Debug("cmd drop", "player", c.s.character, "item", target.proto)
	return nil
}

// cmdPut places a carried item into a container with `put <item> in <container>` (MUDLIB
// §6, §7). Scopes: the item from inventory, the container from inventory or the floor.
// Rejects a closed container and a full one (capacity). Pure containment Move.
func cmdPut(c *Context) error {
	item, cont, ok := splitIn(c.Rest())
	if !ok || item == "" || cont == "" {
		c.Send("Put what in what?")
		return nil
	}
	items := c.z.Resolve(c.Actor, parseTargetSpec(item), ScopeInventory)
	if len(items) == 0 {
		c.Send("You aren't carrying that.")
		return nil
	}
	containers := c.z.Resolve(c.Actor, parseTargetSpec(cont), ScopeInventory, ScopeRoomItems)
	if len(containers) == 0 {
		c.Send("You don't see that container.")
		return nil
	}
	box := containers[0]
	cc, isContainer := Get[*Container](box)
	if !isContainer {
		c.z.act("$p is not a container.", c.Actor, box, nil, "", "", ToActor)
		return nil
	}
	if cc.closed {
		c.z.act("$p is closed.", c.Actor, box, nil, "", "", ToActor)
		return nil
	}
	for _, m := range items {
		if m == box {
			c.Send("You can't put something inside itself.")
			continue
		}
		if !cc.hasRoom(len(box.contents)) {
			c.z.act("$p can't hold any more.", c.Actor, box, nil, "", "", ToActor)
			break
		}
		// Clear a worn slot if the item being stowed was equipped.
		if wr, ok := Get[*Wearer](c.Actor); ok {
			if loc := wr.slotOf(m); loc != WearLocNone {
				delete(wr.worn, loc)
			}
		}
		Move(m, box)
		c.z.act2("You put $p in $P.", c.Actor, m, box, nil, "", "", ToActor)
		c.z.act2("$n puts $p in $P.", c.Actor, m, box, nil, "", "", ToRoom)
	}
	c.z.log.Debug("cmd put", "player", c.s.character, "container", box.proto)
	return nil
}

// cmdWear wears a carried wearable in its first legal free slot (MUDLIB §6). Scope:
// inventory. Equipping is worn-slot STATE on the actor's Wearer (the item stays in
// inventory). The actor entity has prototype==nil, so mutating its Wearer map is plain
// instance state — no COW.
func cmdWear(c *Context) error {
	if c.Arg(0) == "" {
		c.Send("Wear what?")
		return nil
	}
	target, ok := c.Target(ScopeInventory)
	if !ok {
		c.Send("You aren't carrying that.")
		return nil
	}
	w, ok := Get[*Wearable](target)
	if !ok {
		c.z.act("You can't wear $p.", c.Actor, target, nil, "", "", ToActor)
		return nil
	}
	// Pick the first legal slot that ISN'T wield/hold (those are the wield/hold verbs) and
	// is currently free.
	wr := actorWearer(c.Actor)
	for _, loc := range w.slots() {
		if loc == WearLocWield || loc == WearLocHold {
			continue
		}
		if wr.worn[loc] != nil {
			continue
		}
		wr.worn[loc] = target
		c.z.act("You wear $p on your $t.", c.Actor, target, nil, wearLocName[loc], "", ToActor)
		c.z.act("$n wears $p.", c.Actor, target, nil, "", "", ToRoom)
		c.z.log.Debug("cmd wear", "player", c.s.character, "item", target.proto, "slot", wearLocName[loc])
		return nil
	}
	c.z.act("You can't wear $p.", c.Actor, target, nil, "", "", ToActor)
	return nil
}

// cmdWield wields a carried weapon in the wield slot (MUDLIB §6). Scope: inventory. Requires
// the item be Wearable in WearLocWield (a weapon advertises that slot). The Weapon component
// carries the damage shape (data only this phase; combat is Phase 6).
func cmdWield(c *Context) error {
	if c.Arg(0) == "" {
		c.Send("Wield what?")
		return nil
	}
	target, ok := c.Target(ScopeInventory)
	if !ok {
		c.Send("You aren't carrying that.")
		return nil
	}
	w, ok := Get[*Wearable](target)
	if !ok || !w.canWear(WearLocWield) {
		c.z.act("You can't wield $p.", c.Actor, target, nil, "", "", ToActor)
		return nil
	}
	wr := actorWearer(c.Actor)
	if wr.worn[WearLocWield] != nil {
		c.Send("You are already wielding something.")
		return nil
	}
	wr.worn[WearLocWield] = target
	c.z.act("You wield $p.", c.Actor, target, nil, "", "", ToActor)
	c.z.act("$n wields $p.", c.Actor, target, nil, "", "", ToRoom)
	c.z.log.Debug("cmd wield", "player", c.s.character, "item", target.proto)
	return nil
}

// cmdHold holds a carried item in the off-hand hold slot (MUDLIB §6). Scope: inventory.
// Requires the item be Wearable in WearLocHold.
func cmdHold(c *Context) error {
	if c.Arg(0) == "" {
		c.Send("Hold what?")
		return nil
	}
	target, ok := c.Target(ScopeInventory)
	if !ok {
		c.Send("You aren't carrying that.")
		return nil
	}
	w, ok := Get[*Wearable](target)
	if !ok || !w.canWear(WearLocHold) {
		c.z.act("You can't hold $p.", c.Actor, target, nil, "", "", ToActor)
		return nil
	}
	wr := actorWearer(c.Actor)
	if wr.worn[WearLocHold] != nil {
		c.Send("You are already holding something.")
		return nil
	}
	wr.worn[WearLocHold] = target
	c.z.act("You hold $p.", c.Actor, target, nil, "", "", ToActor)
	c.z.act("$n holds $p.", c.Actor, target, nil, "", "", ToRoom)
	c.z.log.Debug("cmd hold", "player", c.s.character, "item", target.proto)
	return nil
}

// cmdRemove takes off a worn/wielded/held item (MUDLIB §6). Scope: equipment. The item
// returns to plain inventory (it was always there — removing just clears the slot state).
func cmdRemove(c *Context) error {
	if c.Arg(0) == "" {
		c.Send("Remove what?")
		return nil
	}
	target, ok := c.Target(ScopeEquipment)
	if !ok {
		c.Send("You aren't using that.")
		return nil
	}
	wr, _ := Get[*Wearer](c.Actor)
	if wr == nil {
		c.Send("You aren't using that.")
		return nil
	}
	loc := wr.slotOf(target)
	if loc == WearLocNone {
		c.Send("You aren't using that.")
		return nil
	}
	delete(wr.worn, loc)
	c.z.act("You stop using $p.", c.Actor, target, nil, "", "", ToActor)
	c.z.act("$n stops using $p.", c.Actor, target, nil, "", "", ToRoom)
	c.z.log.Debug("cmd remove", "player", c.s.character, "item", target.proto, "slot", wearLocName[loc])
	return nil
}

// cmdOpen opens a closed container (MUDLIB §6). Scope: room floor or inventory. This is the
// COW arming command (Finding 6): the container is a prototype-spawned INSTANCE whose
// Container component is SHARED with the prototype, so flipping `closed` MUST go through
// mutableComponent — writing through a Get[*Container] result would mutate the shared
// prototype and race every sibling instance across every zone goroutine. mutableComponent
// copies the Container onto this instance first, then the write lands locally.
func cmdOpen(c *Context) error {
	box, cc, ok := c.z.targetContainer(c, "Open what?")
	if !ok {
		return nil
	}
	if !cc.closed {
		c.z.act("$p is already open.", c.Actor, box, nil, "", "", ToActor)
		return nil
	}
	// COPY-ON-WRITE: take this instance's own Container before mutating. On a prototype-
	// spawned box this clones the shared Container onto the instance; on a non-prototype box
	// it returns the existing one. Never write cc.closed directly — cc may be the prototype's.
	mut := mutableComponent[*Container](box)
	mut.closed = false
	c.z.act("You open $p.", c.Actor, box, nil, "", "", ToActor)
	c.z.act("$n opens $p.", c.Actor, box, nil, "", "", ToRoom)
	c.z.log.Debug("cmd open", "player", c.s.character, "container", box.proto, "rid", box.rid)
	return nil
}

// cmdClose closes an open container (MUDLIB §6). The COW counterpart of cmdOpen: it mutates
// the prototype-shared Container.closed via mutableComponent so the write lands on this
// instance only.
func cmdClose(c *Context) error {
	box, cc, ok := c.z.targetContainer(c, "Close what?")
	if !ok {
		return nil
	}
	if cc.closed {
		c.z.act("$p is already closed.", c.Actor, box, nil, "", "", ToActor)
		return nil
	}
	mut := mutableComponent[*Container](box)
	mut.closed = true
	c.z.act("You close $p.", c.Actor, box, nil, "", "", ToActor)
	c.z.act("$n closes $p.", c.Actor, box, nil, "", "", ToRoom)
	c.z.log.Debug("cmd close", "player", c.s.character, "container", box.proto, "rid", box.rid)
	return nil
}

// targetContainer resolves the open/close target and returns it with its (possibly still
// prototype-shared) Container component for the closed/open check. The caller must take a
// mutableComponent before WRITING the returned Container — cc here is read-only. emptyMsg is
// the "X what?" prompt when no argument was given.
func (z *Zone) targetContainer(c *Context, emptyMsg string) (*Entity, *Container, bool) {
	if c.Arg(0) == "" {
		c.Send(emptyMsg)
		return nil, nil, false
	}
	box, ok := c.Target(ScopeRoomItems, ScopeInventory)
	if !ok {
		c.Send("You don't see that here.")
		return nil, nil, false
	}
	cc, isContainer := Get[*Container](box)
	if !isContainer {
		c.z.act("$p is not a container.", c.Actor, box, nil, "", "", ToActor)
		return nil, nil, false
	}
	return box, cc, true
}

// actorWearer returns the actor's *Wearer, creating an empty one on first use. The actor is
// a player entity (prototype==nil), so adding/mutating the Wearer is plain instance state —
// no COW path. Centralizes the lazy-init so every equip verb shares it.
func actorWearer(actor *Entity) *Wearer {
	if wr, ok := Get[*Wearer](actor); ok {
		if wr.worn == nil {
			wr.worn = map[WearLoc]*Entity{}
		}
		return wr
	}
	wr := &Wearer{worn: map[WearLoc]*Entity{}}
	Add(actor, wr)
	return wr
}

// splitFrom splits "<item> from <container>" on the first standalone " from " token,
// returning the item phrase, container phrase, and whether the token was present.
func splitFrom(rest string) (item, cont string, ok bool) {
	return splitKeyword(rest, "from")
}

// splitIn splits "<item> in <container>" on the first standalone " in " token.
func splitIn(rest string) (item, cont string, ok bool) {
	if i, c, ok := splitKeyword(rest, "in"); ok {
		return i, c, true
	}
	// "put X Y" with no "in" is also accepted as item=first-word, container=rest, but we
	// require explicit "in" to keep the grammar unambiguous; return not-ok otherwise.
	return "", "", false
}

// splitKeyword splits rest into the text before and after the first standalone occurrence
// of `kw` (a whitespace-delimited word, case-insensitive), e.g. "sword from chest" on "from"
// -> ("sword", "chest", true). Returns ok=false when kw is absent or at an edge with empty
// sides. Bounded: a single pass over the whitespace tokens the player typed.
func splitKeyword(rest, kw string) (before, after string, ok bool) {
	words := strings.Fields(rest)
	for i, w := range words {
		if strings.EqualFold(w, kw) && i > 0 && i < len(words)-1 {
			return strings.Join(words[:i], " "), strings.Join(words[i+1:], " "), true
		}
	}
	return "", "", false
}
