package world

// stack.go — Phase-13.2 STACKABLE MATERIALS (docs/CRAFTING.md §5): the item-model piece materials need.
// A material item is ONE entity carrying a Stack count; identical material stacks MERGE on pickup (bounded
// by the prototype's max), and a `split` command divides a stack. Which items stack is content (the
// ItemMeta material spec); stacking/merge/split is engine mechanic. The stack count rides the per-instance
// item delta (binding.go's itemDeltaJSON), so a partially-used stack survives a reload.

// defaultMaxStack caps a material that declares no explicit max (a large but bounded default).
const defaultMaxStack = 1000

// Stack is a material instance's current count. Per-instance state (added on spawn for a material item),
// COW-irrelevant (a freshly-spawned material gets its own Stack). Persisted in the item delta.
type Stack struct {
	count int
}

func (*Stack) componentKind() Kind { return KindStack }

// isMaterial reports whether the item is a stackable material (its ItemMeta declares a positive maxStack).
func isMaterial(item *Entity) bool {
	m, ok := Get[*ItemMeta](item)
	return ok && m.maxStack > 0
}

// stackMax returns the item's max stack size (0 for a non-material).
func stackMax(item *Entity) int {
	if m, ok := Get[*ItemMeta](item); ok {
		return m.maxStack
	}
	return 0
}

// itemStackCount returns the item's current stack count. A material with no Stack component yet counts as 1
// (a freshly-spawned single); a non-material returns 0 (the delta omits it).
func itemStackCount(item *Entity) int {
	if s, ok := Get[*Stack](item); ok {
		return s.count
	}
	if isMaterial(item) {
		return 1
	}
	return 0
}

// setItemStackCount sets the item's stack count (adding a Stack component if absent). Zone goroutine only.
func setItemStackCount(item *Entity, n int) {
	if item == nil {
		return
	}
	if s, ok := Get[*Stack](item); ok {
		s.count = n
		return
	}
	Add(item, &Stack{count: n})
}

// ensureStack attaches a Stack{1} to a freshly-spawned material item (called from spawn). A non-material
// gets nothing. Idempotent.
func ensureStack(item *Entity) {
	if isMaterial(item) && !Has[*Stack](item) {
		Add(item, &Stack{count: 1})
	}
}

// mergeStackInto tries to fold `src`'s count into an existing stack of the SAME material prototype already
// in `dest`'s contents, bounded by maxStack. Returns true if src was fully absorbed (and should be
// discarded by the caller); false if there is no compatible stack with room (src stands on its own).
// Single-writer: zone goroutine.
func mergeStackInto(dest, src *Entity) bool {
	if !isMaterial(src) {
		return false
	}
	limit := stackMax(src)
	for _, held := range dest.contents {
		if held == src || held.proto != src.proto || !isMaterial(held) {
			continue
		}
		room := limit - itemStackCount(held)
		if room <= 0 {
			continue
		}
		moved := itemStackCount(src)
		if moved > room {
			moved = room
		}
		setItemStackCount(held, itemStackCount(held)+moved)
		setItemStackCount(src, itemStackCount(src)-moved)
		if itemStackCount(src) <= 0 {
			return true // fully absorbed
		}
	}
	return false
}

// splitStack splits `n` off `item`'s stack into a NEW item of the same prototype placed in `owner`'s
// contents. Returns the new stack, or nil if the split is invalid (not a material, n<1, or n>=the whole
// stack — splitting the whole stack is a no-op). Zone goroutine only.
func (z *Zone) splitStack(owner, item *Entity, n int) *Entity {
	if !isMaterial(item) || n < 1 || n >= itemStackCount(item) {
		return nil
	}
	piece := z.spawn(item.proto)
	if piece == nil {
		return nil
	}
	setItemStackCount(piece, n)
	setItemStackCount(item, itemStackCount(item)-n)
	Move(piece, owner)
	return piece
}
