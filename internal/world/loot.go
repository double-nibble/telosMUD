package world

import (
	"math/rand"

	"github.com/double-nibble/telosmud/internal/content"
)

// loot.go — the Phase-12.1 LOOT RESOLVER (docs/LOOT-AND-SPAWNS.md §2/§5): content loot tables + the
// on-death resolver. A mob references a loot_table; on death the resolver runs the table PER ELIGIBLE
// LOOTER (personal loot — each eligible player rolls independently), resolving each independent roll and
// delivering the result DIRECTLY to that player (the corpse holds only the body — no contested pickups).
// Runs on the dying mob's zone goroutine with the per-zone seeded RNG, so a seed makes a run deterministic.
// On-pillar: every tier, table, roll, and weight is content; the engine names no item or tier.
//
// Pity (12.2) and item quality/affixes (12.3) layer onto this slice's structure; the lootPity field +
// quality hook are carried but inert here.

// --- runtime defs ------------------------------------------------------------------------------

type rarityTierDef struct {
	ref    string
	order  int
	weight float64
	color  string
}

type lootTableDef struct {
	ref   string
	rolls []lootRoll
}

type lootRoll struct {
	kind         string // "guaranteed" | "chance" | "weighted_one" | "weighted_n"
	chance       float64
	n            int
	qualityFloor string
	pool         []lootEntry
	pity         *lootPity // 12.2
}

type lootEntry struct {
	item   string
	tier   string
	weight float64
}

type lootPity struct {
	key  string
	step float64
	cap  float64
}

func buildRarityTierDef(d content.RarityTierDTO) *rarityTierDef {
	return &rarityTierDef{ref: d.Ref, order: d.Order, weight: d.Weight, color: d.Color}
}

func buildLootTableDef(d content.LootTableDTO) *lootTableDef {
	def := &lootTableDef{ref: d.Ref}
	for _, r := range d.Rolls {
		roll := lootRoll{kind: r.Kind, chance: r.Chance, n: r.N, qualityFloor: r.QualityFloor}
		for _, e := range r.Pool {
			roll.pool = append(roll.pool, lootEntry{item: e.Item, tier: e.Tier, weight: e.Weight})
		}
		if r.Pity != nil {
			roll.pity = &lootPity{key: r.Pity.Key, step: r.Pity.Step, cap: r.Pity.Cap}
		}
		def.rolls = append(def.rolls, roll)
	}
	return def
}

// --- the resolver ------------------------------------------------------------------------------

// resolveLoot runs the victim mob's loot table for every eligible looter, delivering each looter's rolled
// drops directly to them. Called from die() BEFORE the threat table is scrubbed (the eligibility source).
// A victim with no loot table, no table registered, or no eligible looters is a clean no-op. rng is the
// roll source (deterministic per zone). Single-writer: zone goroutine.
func (z *Zone) resolveLoot(victim *Entity, rng *rand.Rand) {
	if victim == nil || victim.living == nil || victim.living.lootTable == "" {
		return
	}
	table := z.lootTableDefs().get(victim.living.lootTable)
	if table == nil {
		return
	}
	looters := z.eligibleLooters(victim)
	for _, looter := range looters {
		for i := range table.rolls {
			for _, itemRef := range z.resolveRoll(&table.rolls[i], rng) {
				z.deliverLoot(looter, itemRef)
			}
		}
	}
}

// eligibleLooters returns the PLAYERS who dealt damage to the victim (the v1 "dealt any damage" rule):
// every player key in the victim's threat table. The threat table is the existing damage record, so no
// new accounting is needed. A future tag/group rule refines this.
func (z *Zone) eligibleLooters(victim *Entity) []*Entity {
	if victim.living.threat == nil {
		return nil
	}
	var out []*Entity
	for attacker := range victim.living.threat {
		if isPlayer(attacker) {
			out = append(out, attacker)
		}
	}
	return out
}

// resolveRoll resolves one roll to a list of selected item prototype refs (0..N items). guaranteed and
// the weighted kinds always pick from the pool; chance gates on its probability first. quality_floor
// filters the pool to entries at or above the floor tier's order.
func (z *Zone) resolveRoll(roll *lootRoll, rng *rand.Rand) []string {
	pool := z.filterPoolByFloor(roll.pool, roll.qualityFloor)
	if len(pool) == 0 {
		return nil
	}
	switch roll.kind {
	case "guaranteed", "weighted_one":
		if e := z.weightedPick(pool, rng); e != nil {
			return []string{e.item}
		}
	case "chance":
		if rng.Float64() < roll.chance {
			if e := z.weightedPick(pool, rng); e != nil {
				return []string{e.item}
			}
		}
	case "weighted_n":
		n := roll.n
		if n < 1 {
			n = 1
		}
		var out []string
		for i := 0; i < n; i++ {
			if e := z.weightedPick(pool, rng); e != nil {
				out = append(out, e.item)
			}
		}
		return out
	}
	return nil
}

// filterPoolByFloor keeps only entries whose rarity tier is at or above the floor tier's order. An empty
// floor (or an unknown floor/entry tier) keeps the entry — the floor is an opt-in filter, never a silent
// drop of an un-tiered entry.
func (z *Zone) filterPoolByFloor(pool []lootEntry, floor string) []lootEntry {
	if floor == "" {
		return pool
	}
	ft := z.rarityTierDefs().get(floor)
	if ft == nil {
		return pool
	}
	var out []lootEntry
	for _, e := range pool {
		et := z.rarityTierDefs().get(e.tier)
		if et == nil || et.order >= ft.order {
			out = append(out, e)
		}
	}
	return out
}

// weightedPick selects one entry from the pool weighted by each entry's weight (its own, else its rarity
// tier's default weight, else 1). Uses the supplied seeded rng so a run is reproducible. Returns nil for
// an empty pool.
func (z *Zone) weightedPick(pool []lootEntry, rng *rand.Rand) *lootEntry {
	if len(pool) == 0 {
		return nil
	}
	total := 0.0
	for i := range pool {
		total += z.entryWeight(pool[i])
	}
	if total <= 0 {
		return &pool[0] // all-zero weights: deterministic first entry rather than a divide-by-zero
	}
	r := rng.Float64() * total
	for i := range pool {
		r -= z.entryWeight(pool[i])
		if r < 0 {
			return &pool[i]
		}
	}
	return &pool[len(pool)-1]
}

// entryWeight is an entry's pool weight: its own weight if set, else its rarity tier's default weight,
// else 1 (an un-tiered, un-weighted entry is equally likely).
func (z *Zone) entryWeight(e lootEntry) float64 {
	if e.weight > 0 {
		return e.weight
	}
	if e.tier != "" {
		if t := z.rarityTierDefs().get(e.tier); t != nil && t.weight > 0 {
			return t.weight
		}
	}
	return 1
}

// deliverLoot spawns the item prototype and delivers it directly into the looter's inventory, with a
// message. Personal loot: the item is the looter's, never placed in the contested corpse. A nil/unknown
// prototype is a clean no-op (content-lint discipline).
func (z *Zone) deliverLoot(looter *Entity, itemRef string) {
	item := z.spawn(ProtoRef(itemRef))
	if item == nil {
		return
	}
	Move(item, looter)
	if s, ok := sessionOf(looter); ok {
		s.send(textFrame("You receive " + itemName(item) + "."))
	}
}

// itemName renders an item entity's short name for a loot message (its short, else its proto ref).
func itemName(item *Entity) string {
	if item.short != "" {
		return item.short
	}
	return string(item.proto)
}
