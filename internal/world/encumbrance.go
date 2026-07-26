package world

// encumbrance.go — #548 CARRYING CAPACITY: a hard cap on the aggregate weight an entity can carry, gating
// item acquisition. The cap itself is pure CONTENT — a `carry_capacity` DERIVED attribute (the 5e Str×15 is
// authored as a base formula, evaluated by the attribute machinery). The engine's only job is enforcing
// aggregate-carried-weight <= carry_capacity at each acquisition seam. A pack that models no encumbrance (no
// carry_capacity attr, or weightless items) leaves the whole thing a clean no-op — the bare-engine invariant.

// carriedWeight sums the mass an entity bears (#548). Worn gear stays in the entity's contents — equipped is
// a STATE over a carried item, not a separate store — so a single pass over contents covers both carried and
// worn, with no double count and no separate Wearer walk. Top-level contents only: a nested container's
// contents are deliberately NOT recursed this round (the "bag of holding reduces weight" refinement is a
// separate, depth-bounded fork).
func carriedWeight(e *Entity) int {
	if e == nil {
		return 0
	}
	w := 0
	for _, c := range e.contents {
		w += itemWeight(c)
	}
	return w
}

// itemWeight is the mass an item contributes — its Physical weight × its STACK COUNT (a 10-count stack of a
// unit-weight-100 material weighs 1000, not 100), or 0 for a weightless / no-Physical item (a corpse, an
// intangible). Multiplying by the stack count closes a bypass INSIDE the gated seams: without it a player
// could carry an arbitrarily large heavy stack for the price of one unit.
func itemWeight(item *Entity) int {
	p, ok := Get[*Physical](item)
	if !ok {
		return 0
	}
	n := itemStackCount(item)
	if n < 1 {
		n = 1 // a non-stackable / single item is one unit (itemStackCount returns 0 for a non-material)
	}
	return p.weight * n
}

// canCarry reports whether e may take on `add` more weight without exceeding its carry_capacity (#548). A
// carry_capacity of <= 0 — no such content attribute, or a pack that doesn't model encumbrance — is NO CAP,
// so the gate is a clean no-op (the contentless-pack invariant). Only a POSITIVE capacity enforces. This is
// the single shared predicate every acquisition site checks, so the gate is auditable in one place rather
// than re-derived per call site (the can't-forget discipline). Wired into cmdGet + getFrom this round; the
// OTHER acquisition paths (give/buy/autoloot, and the crafting family — produce_item / salvage / recipe
// output that spawns into inventory) do NOT yet route it and can exceed the cap — a documented deferral, not
// a silent hole: they adopt this same helper when encumbrance-hardened.
func canCarry(e *Entity, add int) bool {
	capacity := attr(e, "carry_capacity")
	if capacity <= 0 {
		return true // encumbrance not modeled (or no capacity yet) => ungated
	}
	return float64(carriedWeight(e)+add) <= capacity
}
