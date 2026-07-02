package world

// keep.go — the per-item KEEP flag (#36 parts 2+3, Track 4). A player marks a carried item `keep` to
// guard it against accidental loss: a kept item cannot be dropped or put into a container until it is
// `unkeep`ed. Alongside it, the drop/put commands now REFUSE a still-EQUIPPED item (require an explicit
// `remove` first) rather than silently un-equipping it. Both are engine mechanics enforced at the
// transfer-out commands (drop/put), mirroring the Phase-13.1 bound gate (binding.go).
//
// Kept is a per-INSTANCE marker (like Bound): it lives on the live item entity, never the shared
// prototype, and rides ItemJSON.Delta (itemDeltaJSON.Kept) so a kept flag survives logout/login.

// Kept is the per-instance "do not drop" marker. Toggled by keep/unkeep; persisted in the item delta.
type Kept struct{}

func (*Kept) componentKind() Kind { return KindKept }

// isKept reports whether the item is currently flagged keep.
func isKept(item *Entity) bool { return Has[*Kept](item) }

// keepItem / unkeepItem set/clear the flag (idempotent). Per-instance (added to the live entity, not the
// proto), so it never leaks to sibling instances. Zone goroutine only.
func keepItem(item *Entity) {
	if item != nil && !isKept(item) {
		Add(item, &Kept{})
	}
}

func unkeepItem(item *Entity) {
	if item != nil {
		Remove[*Kept](item)
	}
}

// keptBlocked reports whether a transfer-out of `item` (drop / put-in-a-container) must be refused
// because it is flagged keep — and tells the player how to clear it. Mirrors transferBlocked.
func keptBlocked(c *Context, item *Entity) bool {
	if isKept(item) {
		c.z.act("$p is marked keep; `unkeep` it first.", c.Actor, item, nil, "", "", ToActor)
		return true
	}
	return false
}

// equippedBlocked reports whether a transfer-out of `item` must be refused because it is still WORN —
// #36 part 2: dropping/putting an equipped item now requires an explicit `remove` first, rather than
// the old silent un-equip. A no-op for an unworn item or a wearer-less actor.
func equippedBlocked(c *Context, item *Entity) bool {
	if wr, ok := Get[*Wearer](c.Actor); ok && wr.slotOf(item) != WearLocNone {
		c.z.act("You must remove $p before you can part with it.", c.Actor, item, nil, "", "", ToActor)
		return true
	}
	return false
}

// cmdKeep flags a carried item so it can't be dropped/put until unkept.
func cmdKeep(c *Context) error {
	if c.Arg(0) == "" {
		c.Send("Keep what?")
		return nil
	}
	target, ok := c.Target(ScopeInventory)
	if !ok {
		c.Send("You aren't carrying that.")
		return nil
	}
	if isKept(target) {
		c.z.act("$p is already marked keep.", c.Actor, target, nil, "", "", ToActor)
		return nil
	}
	keepItem(target)
	c.z.act("You mark $p keep; it won't be dropped by accident.", c.Actor, target, nil, "", "", ToActor)
	return nil
}

// cmdUnkeep clears the keep flag so the item can be dropped/put again.
func cmdUnkeep(c *Context) error {
	if c.Arg(0) == "" {
		c.Send("Unkeep what?")
		return nil
	}
	target, ok := c.Target(ScopeInventory)
	if !ok {
		c.Send("You aren't carrying that.")
		return nil
	}
	if !isKept(target) {
		c.z.act("$p isn't marked keep.", c.Actor, target, nil, "", "", ToActor)
		return nil
	}
	unkeepItem(target)
	c.z.act("You no longer keep $p.", c.Actor, target, nil, "", "", ToActor)
	return nil
}
