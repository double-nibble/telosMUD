package world

import (
	"fmt"
	"math/rand"
)

// salvage.go — Phase-13.4 DECONSTRUCTION (docs/PHASE13-PLAN.md §13.4): salvage/disenchant — the economy's
// SOURCE of crafting components. A salvage ability's on_resolve runs salvage_item(item, table): it consumes
// the source item and rolls a salvage table (REUSING the Phase-12 loot resolver) into components delivered to
// the actor. Two binding rules the doc fixes apply:
//   - §1: the OWNER may deconstruct a BOUND item — destroying your own bound gear is not a transfer, so the
//     transfer gate never applies; salvage_item despawns the source regardless of its bound state.
//   - D1: component binding is TIER-DEPENDENT — a component whose rarity tier is flagged `binds` (the
//     legendary-essence sink) is bound on creation (can't be sold), while low/mid components stay tradeable.
// Single-writer: the zone goroutine, like every other effect op.

// itemNoSalvage reports whether the item is flagged UN-SALVAGEABLE (#38): the disenchant verb refuses it.
func itemNoSalvage(item *Entity) bool {
	if m, ok := Get[*ItemMeta](item); ok {
		return m.noSalvage
	}
	return false
}

// itemSalvageTable returns the item's per-item OVERRIDE salvage table ref (#38), or "" when it has none (the
// caller then falls back to the verb's default table).
func itemSalvageTable(item *Entity) string {
	if m, ok := Get[*ItemMeta](item); ok {
		return m.salvageTable
	}
	return ""
}

// itemTier returns the item's rarity tier ref (#38 slice B — the salvage-derivation key), or "" if untiered.
func itemTier(item *Entity) string {
	if m, ok := Get[*ItemMeta](item); ok {
		return m.tier
	}
	return ""
}

// itemLevel returns the item's rolled per-instance Quality LEVEL (#38 slice B — the salvage-skill scaler), or
// 0 for an un-rolled prototype item.
func itemLevel(item *Entity) int {
	if q, ok := Get[*Quality](item); ok {
		return q.Level
	}
	return 0
}

// hasItemTag reports whether the item carries content tag `tag` (#38 tag gate). An empty tag matches anything
// (no gate); an item with no ItemMeta has no tags.
func hasItemTag(item *Entity, tag string) bool {
	if tag == "" {
		return true
	}
	m, ok := Get[*ItemMeta](item)
	if !ok {
		return false
	}
	for _, t := range m.tags {
		if t == tag {
			return true
		}
	}
	return false
}

// salvageRefuse sends a refusal line to a player actor (a mob salvager gets nothing — no session).
func salvageRefuse(actor *Entity, msg string) {
	if s, ok := sessionOf(actor); ok {
		s.send(textFrame(msg))
	}
}

// tierBinds reports whether items of rarity tier ref bind on creation (D1). An unknown/empty tier never binds.
func (z *Zone) tierBinds(ref string) bool {
	if ref == "" {
		return false
	}
	t := z.rarityTierDefs().get(ref)
	return t != nil && t.binds
}

// opSalvageItem: salvage_item(item, table) — deconstruct one held `item` into the components rolled from
// `table`. It consumes the source (even if BOUND — owner deconstruction, §1) and delivers each rolled
// component to the actor with tier-dependent binding (D1). The roll is the Phase-12 loot resolver, so the
// yield is a deterministic weighted draw under the ctx rng (a test pins it with a seed). Errors when the
// actor holds no source or the table is unknown (the calling ability should gate with requires/check first).
func opSalvageItem(c *effectCtx, op *effectOp) error {
	if c.actor == nil {
		return fmt.Errorf("salvage_item: no actor")
	}
	// #38 slice B: default to SUPPRESSING the ability's OnSkillUse — a gated/failed disenchant must not
	// advance the salvaging skill that gates it. Cleared only on the success path (just before consume), so
	// every refuse/error return below leaves the skill un-advanced.
	c.suppressSkillUse = true
	// Two authoring shapes: FIXED proto (op.item set — the Phase-13.4 form) OR OBJECT-TARGETED (op.item
	// empty — `disenchant <item>`, #38), where the player's typed argument resolves a held item by keyword.
	var src *Entity
	if op.item != "" {
		if src = findHeldByProto(c.actor, op.item); src == nil {
			return fmt.Errorf("salvage_item: actor holds no %s", op.item)
		}
	} else {
		hits := c.z.Resolve(c.actor, parseTargetSpec(c.arg), ScopeInventory)
		if len(hits) == 0 {
			salvageRefuse(c.actor, "You aren't carrying that.")
			return nil
		}
		src = hits[0]
	}
	if !guardCrossPlayerWrite(c, c.actor) {
		return nil
	}
	// Gate: a WORN/wielded or keep-flagged source must be taken off / unkept first. Salvage is destructive —
	// more so than drop, which already refuses a worn (equippedBlocked) or kept (keptBlocked) item (#36) — so
	// it honors the same guards up front rather than vaporizing equipped or explicitly-kept gear from under
	// the player. (The consume below goes through Move, which also unequips a worn source as a #35 backstop,
	// but the refusal here is the player-facing guard.)
	if wr, ok := Get[*Wearer](c.actor); ok && wr.slotOf(src) != WearLocNone {
		salvageRefuse(c.actor, "You must remove it before you can salvage it.")
		return nil
	}
	if isKept(src) {
		salvageRefuse(c.actor, "It is marked keep; `unkeep` it before salvaging.")
		return nil
	}
	// Gate: a per-item BLOCK flag refuses the verb (a super-rare metal / quest item can't be broken down).
	if itemNoSalvage(src) {
		salvageRefuse(c.actor, "That cannot be salvaged.")
		return nil
	}
	// Gate: an item-TAG requirement (op.tag) — only items carrying the tag may be disenchanted (e.g. only
	// gear tagged `salvageable`). Empty op.tag => no tag gate.
	if !hasItemTag(src, op.tag) {
		salvageRefuse(c.actor, "You can't salvage that.")
		return nil
	}
	// Table DERIVATION (#38 slice B): a per-item OVERRIDE wins, else the item's rarity TIER default (derived
	// from tier+level), else the verb's fixed default table. An empty source is a clean refuse (the player
	// picked an item that yields nothing); a NON-empty ref that names no table is a content error.
	tier := c.z.rarityTierDefs().get(itemTier(src))
	tableRef := itemSalvageTable(src)
	if tableRef == "" && tier != nil {
		tableRef = tier.salvageTable
	}
	if tableRef == "" {
		tableRef = op.table
	}
	if tableRef == "" {
		salvageRefuse(c.actor, "You don't know how to salvage that.")
		return nil
	}
	table := c.z.lootTableDefs().get(tableRef)
	if table == nil {
		return fmt.Errorf("salvage_item: unknown table %q", tableRef)
	}
	// Skill gate + over-skill bonus (#38 slice B). The requirement scales with the item's tier (base) + its
	// rolled LEVEL; below it the actor can't break the item down. Far EXCEEDING it yields bonus table rolls
	// (extra mats). No op.skill => no skill gate and no bonus (the Phase-13.4 fixed form is unchanged).
	minSkill := itemLevel(src)
	bonusStep := 0
	if tier != nil {
		minSkill += tier.salvageSkill
		bonusStep = tier.salvageBonusStep
	}
	passes := 1
	if op.skill != "" {
		skillVal := int(attr(c.actor, op.skill))
		if skillVal < minSkill {
			salvageRefuse(c.actor, "Your skill is not yet equal to breaking that down.")
			return nil
		}
		if bonusStep > 0 {
			if extra := (skillVal - minSkill) / bonusStep; extra > 0 {
				if extra > maxSalvageBonus {
					extra = maxSalvageBonus
				}
				passes += extra
			}
		}
	}
	// All gates passed — this is a real salvage: allow the ability's OnSkillUse to fire (advance the skill).
	c.suppressSkillUse = false
	// Consume the source FIRST: destruction of an owned item ignores the bound state (a bound epic is
	// deconstructable by its owner, §1). A material source decrements one; a unique item despawns.
	if isMaterial(src) && itemStackCount(src) > 1 {
		setItemStackCount(src, itemStackCount(src)-1)
	} else {
		Move(src, nil)
	}
	// Roll the salvage table into components (the loot resolver), delivering each to the actor. Over-skill
	// runs the table `passes` times (base 1 + bonus rolls). The bonus passes roll ONLY the non-guaranteed
	// (chance) rolls: a GUARANTEED roll — e.g. a bound top-tier essence sink — is minted exactly once (the
	// base pass), so over-skill rewards extra FILLER without N-multiplying a scarce/bound component (#38 slice
	// B review). A table of only guaranteed rolls therefore yields no over-skill bonus, by design.
	rng := c.rng
	if rng == nil {
		rng = rand.New(rand.NewSource(rand.Int63())) //nolint:gosec // gameplay roll, not security
	}
	for p := 0; p < passes; p++ {
		for i := range table.rolls {
			// A bonus pass re-rolls ONLY probabilistic ("chance") rolls; guaranteed/weighted rolls (which
			// always yield — loot.go) are minted once on the base pass, so a bound sink is never multiplied.
			if p > 0 && table.rolls[i].kind != "chance" {
				continue
			}
			// #181: only the BASE pass advances the looter's pity counter. A bonus pass (p>0) re-rolls the
			// same chance with the pity-adjusted odds but does NOT mutate pity, so one over-skilled salvage of
			// a pitied table can't compound the counter up to 1+maxSalvageBonus times.
			for _, entry := range c.z.resolveRoll(c.actor, &table.rolls[i], rng, p == 0) {
				c.z.deliverComponent(c.actor, entry, rng)
			}
		}
	}
	return nil
}

// maxSalvageBonus caps the number of BONUS table rolls over-skill can grant (#38 slice B), so an absurdly
// high salvaging skill can't farm unbounded mats from one item.
const maxSalvageBonus = 3

// deliverComponent spawns one salvage component, rolls its quality (Phase 12.3), applies TIER-DEPENDENT
// binding (D1 — a binds-tier component is bound on creation), and delivers it to the actor, merging into an
// existing stack when it is a material (mergeStackInto, like a pickup). A nil/unknown prototype is a clean
// no-op (content-lint discipline). Zone goroutine.
func (z *Zone) deliverComponent(actor *Entity, entry lootEntry, rng *rand.Rand) {
	item := z.spawn(ProtoRef(entry.item))
	if item == nil {
		return
	}
	if entry.quality != nil {
		rollItemQuality(item, entry.quality, rng)
	}
	if z.tierBinds(entry.tier) {
		bindItem(item) // D1: a top-tier essence is bound the moment it exists (the no-trade sink)
	}
	Move(item, actor)
	if isMaterial(item) && mergeStackInto(actor, item) {
		actor.removeContent(item) // fully folded into a held stack of the same component
	}
	if s, ok := sessionOf(actor); ok {
		s.send(textFrame("You salvage " + itemName(item) + "."))
	}
}
