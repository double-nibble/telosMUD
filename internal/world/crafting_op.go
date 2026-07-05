package world

import "fmt"

// crafting_op.go — Phase-13.3 CRAFTING OPS (docs/PHASE13-PLAN.md §13.3): the effect-op verbs a crafting
// ability composes — consume an input, produce an output, augment an item. They are ordinary registered ops
// (effect_op.go), so a craft ability's on_resolve is just an op-list — e.g. [consume_item ironbar ×2,
// produce_item dagger] — gated by requires.profession at the ability's step-3. All three operate on the
// ACTOR's inventory (the crafter is both actor and target of a craft). Single-writer: the zone goroutine.

// opQty returns an op's integer quantity from op.amount, defaulting to 1 (the common single-item craft).
func opQty(op *effectOp) int {
	if n := int(op.amount); n >= 1 {
		return n
	}
	return 1
}

// findHeldByProto returns e's first held item of prototype ref, or nil.
func findHeldByProto(e *Entity, ref string) *Entity {
	for _, it := range e.contents {
		if string(it.proto) == ref {
			return it
		}
	}
	return nil
}

// heldQuantity sums how many of prototype ref e holds, stack-aware: a material counts its Stack, every other
// item counts 1. The consume precheck reads this so a multi-stack input total is honored.
func heldQuantity(e *Entity, ref string) int {
	total := 0
	for _, it := range e.contents {
		if string(it.proto) != ref {
			continue
		}
		if isMaterial(it) {
			total += itemStackCount(it)
		} else {
			total++
		}
	}
	return total
}

// opConsumeItem: consume_item(item, amount) — destroy `amount` of prototype `item` from the actor's
// inventory (stack-aware: decrement a material stack, despawn a whole non-stack item, spanning multiple
// stacks if needed). It PRE-CHECKS the total and errors — aborting the op-list — when the actor is short, so
// a craft never produces from nothing (the ability SHOULD also gate with requires/check; this is the
// defensive backstop). Despawn is Move(it, nil), the death.go idiom.
func opConsumeItem(c *effectCtx, op *effectOp) error {
	if c.actor == nil {
		return fmt.Errorf("consume_item: no actor")
	}
	if op.item == "" {
		return fmt.Errorf("consume_item: no item")
	}
	if !guardCrossPlayerWrite(c, c.actor) {
		return nil
	}
	need := opQty(op)
	if heldQuantity(c.actor, op.item) < need {
		return fmt.Errorf("consume_item: actor holds < %d× %s", need, op.item)
	}
	for need > 0 {
		it := findHeldByProto(c.actor, op.item)
		if it == nil {
			return fmt.Errorf("consume_item: ran out of %s mid-consume", op.item) // unreachable after the precheck
		}
		if isMaterial(it) {
			if have := itemStackCount(it); have > need {
				setItemStackCount(it, have-need) // partial: this stack covers the rest
				return nil
			}
			need -= itemStackCount(it) // whole stack consumed
		} else {
			need--
		}
		Move(it, nil) // despawn the emptied stack / the discrete item
	}
	return nil
}

// opProduceItem: produce_item(item, amount, {bind}) — spawn the craft OUTPUT into the actor's inventory.
// `bind: bound` forces the produced item bound on creation (a soulbound craft); otherwise the item keeps its
// prototype's own bind rule (it may bind later on pickup/equip). A produced material is created as ONE stack
// of `amount` and then merged into a compatible held stack exactly like a pickup (mergeStackInto).
func opProduceItem(c *effectCtx, op *effectOp) error {
	if c.actor == nil {
		return fmt.Errorf("produce_item: no actor")
	}
	if op.item == "" {
		return fmt.Errorf("produce_item: no item")
	}
	if !guardCrossPlayerWrite(c, c.actor) {
		return nil
	}
	n := opQty(op)
	first := c.z.spawn(ProtoRef(op.item))
	if first == nil {
		return fmt.Errorf("produce_item: unknown prototype %s", op.item)
	}
	if isMaterial(first) {
		setItemStackCount(first, n) // one stack of the whole amount
		applyProduceBind(first, op.bind)
		Move(first, c.actor)
		if mergeStackInto(c.actor, first) {
			c.actor.removeContent(first) // fully folded into an existing held stack
		}
		return nil
	}
	// Non-material: n discrete items.
	applyProduceBind(first, op.bind)
	Move(first, c.actor)
	for i := 1; i < n; i++ {
		it := c.z.spawn(ProtoRef(op.item))
		if it == nil {
			break
		}
		applyProduceBind(it, op.bind)
		Move(it, c.actor)
	}
	return nil
}

// applyProduceBind applies a produce_item bind override: "bound" forces the item bound now; anything else
// leaves the prototype's own rule intact.
func applyProduceBind(item *Entity, bind string) {
	if bind == "bound" {
		bindItem(item)
	}
}

// opAugmentItem: augment_item(item, {attr, amount}) — the §10-deferred enchant/socket hook, kept to a FLAT
// STAT-BUMP STUB for v1 (the rich affix/socket catalog stays deferred). It bumps a named affix on the held
// item's per-instance Quality delta (Phase 12.3) by `amount` (default +1), creating the Quality component if
// absent. Because the bump rides the EXISTING item-instance delta it persists across a reload for free; the
// affix's worn stat EFFECT is applied by the Wearer gear modSource (#35) — and if the item is already worn,
// this op re-sums it live. `attr` names the stat
// (default "power"). The target is the actor's first held `item`.
func opAugmentItem(c *effectCtx, op *effectOp) error {
	if c.actor == nil {
		return fmt.Errorf("augment_item: no actor")
	}
	if op.item == "" {
		return fmt.Errorf("augment_item: no item")
	}
	it := findHeldByProto(c.actor, op.item)
	if it == nil {
		return fmt.Errorf("augment_item: actor holds no %s", op.item)
	}
	if !guardCrossPlayerWrite(c, c.actor) {
		return nil
	}
	q, ok := Get[*Quality](it)
	if !ok {
		q = &Quality{Affixes: map[string]float64{}}
		Add(it, q)
	} else if q.Affixes == nil {
		q.Affixes = map[string]float64{}
	}
	attr := op.attr
	if attr == "" {
		attr = "power"
	}
	bump := op.amount
	if bump == 0 {
		bump = 1
	}
	q.Affixes[attr] += bump
	// #35: if the augmented item is currently worn, re-sum the wearer's gear bonus so the bump takes effect
	// live (not only after a remove/re-wear). Harmless no-op for a carried-but-unworn item.
	if wr, ok := Get[*Wearer](c.actor); ok && wr.slotOf(it) != WearLocNone {
		recomputeWornMods(c.actor, wr)
	}
	return nil
}
