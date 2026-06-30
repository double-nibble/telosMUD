package world

import "encoding/json"

// binding.go — Phase-13.1 item BINDING + the transfer gate (docs/CRAFTING.md §1/§8). Binding is the hinge
// the crafting economy turns on: a BOUND item cannot be given/dropped-for-others/traded — but it CAN be
// equipped, destroyed, and deconstructed by its owner. The engine enforces it uniformly at every transfer
// command so no path leaks a bound item into trade; content sets the rules. Also the home of the
// generalized per-instance item DELTA (the bound state + rolled quality + a stack count) that rides
// ItemJSON.Delta, generalizing the Phase-12.3 quality round-trip.

// ItemMeta is an item prototype's crafting/economy metadata (Phase 13.1): its binding RULE, rarity tier,
// and tags. Proto data — immutable, proto-aliased on a flyweight item, never mutated at runtime.
type ItemMeta struct {
	bindRule string // "bind_on_pickup" | "bind_on_equip" | "unbound"/"" (freely tradeable)
	tier     string // a rarity_tier_def ref (Phase 12.1) — the component-binding threshold + recipe gating
	tags     []string
}

func (*ItemMeta) componentKind() Kind { return KindItemMeta }

// Bound is a per-INSTANCE marker: this item is bound to its owner (untradeable). Added when an item binds
// (on pickup / on equip); persisted in the item-instance delta.
type Bound struct{}

func (*Bound) componentKind() Kind { return KindBound }

// Bind-rule constants (content values).
const (
	bindRuleOnPickup = "bind_on_pickup"
	bindRuleOnEquip  = "bind_on_equip"
)

// itemBindRule returns the item's bind rule from its ItemMeta ("" if untagged / no meta). Read-only.
func itemBindRule(item *Entity) string {
	if m, ok := Get[*ItemMeta](item); ok {
		return m.bindRule
	}
	return ""
}

// isBound reports whether the item is currently bound.
func isBound(item *Entity) bool { return Has[*Bound](item) }

// bindItem binds the item to its owner (idempotent). The marker is per-instance (added to the live entity,
// not the proto), so it never leaks to sibling instances. Zone goroutine only.
func bindItem(item *Entity) {
	if item != nil && !isBound(item) {
		Add(item, &Bound{})
	}
}

// bindOnPickup binds an item if its rule is bind_on_pickup (called when loot is delivered to a looter).
func bindOnPickup(item *Entity) {
	if itemBindRule(item) == bindRuleOnPickup {
		bindItem(item)
	}
}

// bindOnEquip binds an item if its rule is bind_on_equip (called when an item is worn/wielded).
func bindOnEquip(item *Entity) {
	if itemBindRule(item) == bindRuleOnEquip {
		bindItem(item)
	}
}

// transferBlocked reports whether moving `item` to ANOTHER owner (give / drop-to-ground / put-in-a-shared-
// container) must be refused because it is BOUND — and tells the player so. A bound item can still be
// equipped, destroyed, or deconstructed by its owner; only TRANSFER is gated. This is the one engine gate
// every transfer command consults (the §8 uniform enforcement).
func transferBlocked(c *Context, item *Entity) bool {
	if isBound(item) {
		c.z.act("$p is bound to you and cannot be parted with.", c.Actor, item, nil, "", "", ToActor)
		return true
	}
	return false
}

// --- the generalized per-instance item delta (ItemJSON.Delta) ----------------------------------

// itemDeltaJSON is the on-disk per-instance delta over a shared item prototype: the rolled loot quality
// (Phase 12.3), the bound state (13.1), and a stack count (13.2). nil/empty when the item is a plain
// prototype instance. It generalizes the 12.3 quality-only delta.
type itemDeltaJSON struct {
	Quality *itemQualityJSON `json:"quality,omitempty"`
	Bound   bool             `json:"bound,omitempty"`
	Stack   int              `json:"stack,omitempty"`
}

// dumpItemDelta marshals an item instance's per-instance delta (quality + bound + stack) to its
// ItemJSON.Delta bytes (an OWNED copy, per the no-alias invariant). nil when the item carries no delta.
func dumpItemDelta(item *Entity) json.RawMessage {
	var d itemDeltaJSON
	if q, ok := Get[*Quality](item); ok {
		d.Quality = &itemQualityJSON{Level: q.Level, Affixes: q.Affixes}
	}
	d.Bound = isBound(item)
	// d.Stack is populated by Phase 13.2 (stackable materials).
	if d.Quality == nil && !d.Bound && d.Stack == 0 {
		return nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil
	}
	return b
}

// loadItemDelta re-attaches an item instance's per-instance delta (the persistence round-trip). A
// nil/empty/malformed delta is a clean no-op (a plain prototype instance).
func loadItemDelta(item *Entity, delta json.RawMessage) {
	if len(delta) == 0 {
		return
	}
	var d itemDeltaJSON
	if err := json.Unmarshal(delta, &d); err != nil {
		return
	}
	if d.Quality != nil && (d.Quality.Level != 0 || len(d.Quality.Affixes) > 0) {
		Add(item, &Quality{Level: d.Quality.Level, Affixes: d.Quality.Affixes})
	}
	if d.Bound {
		bindItem(item)
	}
	// d.Stack is applied by Phase 13.2 (stackable materials).
}
