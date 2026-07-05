package world

import (
	"log/slog"
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
	binds  bool // Phase 13.4 (D1): items of this tier bind on creation (the top-tier no-trade sink)
}

type lootTableDef struct {
	ref    string
	rolls  []lootRoll
	onRoll string // Phase 12.1 conditional-drop Lua hatch (docs/REMAINING.md §4); "" = declarative only
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
	item    string
	tier    string
	weight  float64
	quality *qualitySpec // Phase 12.3: roll a per-instance level + affixes onto the dropped item
}

// qualitySpec is the runtime item-quality roll (Phase 12.3): a level range + N affixes drawn from a pool,
// each rolled to a value in its range.
type qualitySpec struct {
	affixes  []affixRoll
	count    int
	levelMin int
	levelMax int
}

type affixRoll struct {
	attr     string
	min, max float64
}

type lootPity struct {
	key  string
	step float64
	cap  float64
}

func buildRarityTierDef(d content.RarityTierDTO) *rarityTierDef {
	return &rarityTierDef{ref: d.Ref, order: d.Order, weight: d.Weight, color: d.Color, binds: d.Binds}
}

// affixDef is the runtime form of a content AffixDefDTO (#37): a NAMED affix (attr + roll range) a loot
// entry's quality pool references by ref, resolved into an inline affixRoll at build time.
type affixDef struct {
	ref      string
	attr     string
	min, max float64
}

func buildAffixDef(d content.AffixDefDTO) *affixDef {
	return &affixDef{ref: d.Ref, attr: d.Attr, min: d.Min, max: d.Max}
}

// buildLootTableDef maps a content loot table onto its runtime form (#37: affixes resolves each pool affix —
// a `ref` entry is looked up in the shared affix registry, an inline entry uses its own attr/min/max). The
// resolution happens once when the shard BUILDS its content (defineGlobals), so an authored affix_def is the
// single source of truth in the pack and an edit applies to every referencing pool the next time the shard
// rebuilds its content — NOT live: loot tables are not hot-reloaded (a pre-#37 Phase-12 limitation), so a
// running shard keeps the boot-time values until it restarts. affixes may be nil (no affix_defs loaded): a
// ref-entry then resolves to an empty (no-op) affix.
func buildLootTableDef(d content.LootTableDTO, affixes *defRegistry[*affixDef]) *lootTableDef {
	def := &lootTableDef{ref: d.Ref, onRoll: d.OnRoll}
	for _, r := range d.Rolls {
		roll := lootRoll{kind: r.Kind, chance: r.Chance, n: r.N, qualityFloor: r.QualityFloor}
		for _, e := range r.Pool {
			entry := lootEntry{item: e.Item, tier: e.Tier, weight: e.Weight}
			if e.Quality != nil {
				qs := &qualitySpec{count: e.Quality.Count, levelMin: e.Quality.LevelMin, levelMax: e.Quality.LevelMax}
				for _, a := range e.Quality.Affixes {
					qs.affixes = append(qs.affixes, resolveAffixRoll(a, affixes))
				}
				entry.quality = qs
			}
			roll.pool = append(roll.pool, entry)
		}
		if r.Pity != nil {
			roll.pity = &lootPity{key: r.Pity.Key, step: r.Pity.Step, cap: r.Pity.Cap}
		}
		def.rolls = append(def.rolls, roll)
	}
	return def
}

// lintAffixRefs warns (once per build) about any quality pool naming an affix `ref` that no loaded affix_def
// provides (#37 review): such a ref resolves to an inert empty affix, silently costing the drop an affix slot,
// so an operator gets a boot-time signal instead of an invisible dud. Content-lint discipline (like the
// unknown-proto/bundle warnings) — the malformed table still loads. Runs on the build path (defineGlobals).
func lintAffixRefs(lt content.LootTableDTO, affixes *defRegistry[*affixDef]) {
	for _, r := range lt.Rolls {
		for _, e := range r.Pool {
			if e.Quality == nil {
				continue
			}
			for _, a := range e.Quality.Affixes {
				if a.Ref != "" && !affixes.has(a.Ref) {
					slog.Warn("content: loot quality references an unknown affix_def; it will roll inert",
						"loot_table", lt.Ref, "item", e.Item, "affix_ref", a.Ref)
				}
			}
		}
	}
}

// resolveAffixRoll turns one content affix entry into a runtime affixRoll (#37). A `ref` entry is resolved
// from the shared affix registry (the normalized form: edit-once propagates); an inline entry (no ref) uses
// its own attr/min/max (the pre-#37 form). A ref that names no loaded affix_def resolves to an empty affix
// (attr ""), which rollItemQuality treats as a no-op — a misauthored ref degrades to nothing, never a panic.
func resolveAffixRoll(a content.AffixRollDTO, affixes *defRegistry[*affixDef]) affixRoll {
	if a.Ref != "" {
		if affixes != nil {
			if def := affixes.get(a.Ref); def != nil {
				return affixRoll{attr: def.attr, min: def.min, max: def.max}
			}
		}
		return affixRoll{} // unknown ref => inert affix (content-lint concern, not a crash)
	}
	return affixRoll{attr: a.Attr, min: a.Min, max: a.Max}
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
			for _, entry := range z.resolveRoll(looter, &table.rolls[i], rng) {
				z.deliverLoot(looter, entry, table.ref, rng)
			}
		}
		// on_roll(ctx) Lua hatch (docs/REMAINING.md §4): after the declarative rolls, a content body may
		// return additional CONDITIONAL drops (branching on looter/victim state the declarative form can't
		// express). Each returned item ref is delivered through the SAME pipeline (quality/binding/merge).
		for _, ref := range z.runLootOnRollLua(looter, victim, table) {
			z.deliverLoot(looter, lootEntry{item: ref}, table.ref, rng)
		}
	}
}

// --- pity (bad-luck protection, Phase 12.2) ----------------------------------------------------

// lootPityMisses returns the looter's consecutive-miss count for a pity key (0 if none). Zone read.
func lootPityMisses(e *Entity, key string) int {
	if e == nil || e.living == nil || e.living.lootPity == nil {
		return 0
	}
	return e.living.lootPity[key]
}

// setLootPityMisses records the looter's miss count for a pity key (COW-safe). 0 removes the entry (a
// hit resets, keeping the persisted subtree small). Zone goroutine only.
func setLootPityMisses(e *Entity, key string, n int) {
	l := mutableLiving(e)
	if l == nil {
		return
	}
	if n <= 0 {
		delete(l.lootPity, key)
		return
	}
	if l.lootPity == nil {
		l.lootPity = map[string]int{}
	}
	l.lootPity[key] = n
}

// pityAdjustedChance returns the effective drop chance for a chance roll given the looter's accumulated
// misses: base + misses*step, raised TO (clamped at) the cap. No pity spec => the bare base chance.
func pityAdjustedChance(roll *lootRoll, looter *Entity) float64 {
	if roll.pity == nil {
		return roll.chance
	}
	eff := roll.chance + float64(lootPityMisses(looter, roll.pity.key))*roll.pity.step
	if roll.pity.cap > 0 && eff > roll.pity.cap {
		eff = roll.pity.cap
	}
	return eff
}

// dumpLootPity renders the looter's per-key miss counts as a fresh map (a copy). nil when none.
func dumpLootPity(e *Entity) map[string]int {
	if e == nil || e.living == nil || len(e.living.lootPity) == 0 {
		return nil
	}
	out := make(map[string]int, len(e.living.lootPity))
	for k, v := range e.living.lootPity {
		out[k] = v
	}
	return out
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

// resolveRoll resolves one roll to a list of selected item prototype refs (0..N items) for `looter`.
// guaranteed and the weighted kinds always pick from the pool; chance gates on its (pity-adjusted)
// probability first. quality_floor filters the pool to entries at or above the floor tier's order.
func (z *Zone) resolveRoll(looter *Entity, roll *lootRoll, rng *rand.Rand) []lootEntry {
	pool := z.filterPoolByFloor(roll.pool, roll.qualityFloor)
	if len(pool) == 0 {
		return nil
	}
	switch roll.kind {
	case "guaranteed", "weighted_one":
		if e := z.weightedPick(pool, rng); e != nil {
			return []lootEntry{*e}
		}
	case "chance":
		hit := rng.Float64() < pityAdjustedChance(roll, looter)
		// Bad-luck protection (Phase 12.2): a miss raises this looter's counter (and so their next
		// chance); a hit resets it. Per-looter, per pity key, persisted.
		if roll.pity != nil {
			if hit {
				setLootPityMisses(looter, roll.pity.key, 0)
			} else {
				setLootPityMisses(looter, roll.pity.key, lootPityMisses(looter, roll.pity.key)+1)
			}
		}
		if hit {
			if e := z.weightedPick(pool, rng); e != nil {
				return []lootEntry{*e}
			}
		}
	case "weighted_n":
		n := roll.n
		if n < 1 {
			n = 1
		}
		var out []lootEntry
		for i := 0; i < n; i++ {
			if e := z.weightedPick(pool, rng); e != nil {
				out = append(out, *e)
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

// deliverLoot spawns the entry's item, rolls its quality (Phase 12.3) onto the instance, and delivers it
// directly into the looter's inventory with a message. Personal loot: the item is the looter's, never
// placed in the contested corpse. A nil/unknown prototype drops the entry (never aborts the kill); spawn
// already Warns generically, so the Warn here layers the LOOT context — which table produced the ref —
// onto it (the reset.go/character.go pattern). That context is what lets a builder chase a typo'd ref,
// especially one computed by an opaque on_roll(ctx) Lua body that no build-time lint can see.
func (z *Zone) deliverLoot(looter *Entity, entry lootEntry, tableRef string, rng *rand.Rand) {
	item := z.spawn(ProtoRef(entry.item))
	if item == nil {
		z.log.Warn("loot: unknown item prototype, entry dropped",
			"ref", entry.item, "table", tableRef, "looter", looter.short)
		return
	}
	if entry.quality != nil {
		rollItemQuality(item, entry.quality, rng)
	}
	bindOnPickup(item) // Phase 13.1: a bind_on_pickup item binds to its looter on personal-loot delivery
	Move(item, looter)
	if s, ok := sessionOf(looter); ok {
		s.send(textFrame("You receive " + itemName(item) + "."))
	}
}

// rollItemQuality rolls an item Level + Count affixes from the spec's pool onto the item's per-instance
// Quality component (Phase 12.3) — the within-tier variance, written into the instance delta (the
// prototype stays shared). Each affix value is rolled in its [min, max] range; a repeated attr takes the
// last roll (a coarse v1 — no de-dup of the pool). Uses the supplied seeded rng for reproducibility.
func rollItemQuality(item *Entity, spec *qualitySpec, rng *rand.Rand) {
	q := &Quality{Affixes: map[string]float64{}}
	if spec.levelMax > spec.levelMin {
		q.Level = spec.levelMin + rng.Intn(spec.levelMax-spec.levelMin+1)
	} else {
		q.Level = spec.levelMin
	}
	count := spec.count
	if count < 0 {
		count = 0
	}
	for i := 0; i < count && len(spec.affixes) > 0; i++ {
		a := spec.affixes[rng.Intn(len(spec.affixes))]
		val := a.min
		if a.max > a.min {
			val = a.min + rng.Float64()*(a.max-a.min)
		}
		q.Affixes[a.attr] = val
	}
	Add(item, q)
}

// Quality is a dropped item's per-instance loot quality (Phase 12.3): a rolled item Level + a set of
// rolled Affixes (attr -> value). It is the per-instance DELTA over the shared prototype — two drops of
// the same item differ only here. Persisted in ItemJSON.Delta. A worn affix's stat EFFECT is applied by the
// Wearer gear modSource (#35, worn_mods.go): wearing the item sums its Affixes into the wearer's attributes.
type Quality struct {
	Level   int
	Affixes map[string]float64
}

func (*Quality) componentKind() Kind { return KindQuality }

// itemQualityJSON is the on-disk shape of a Quality component (part of the item instance delta —
// binding.go's itemDeltaJSON wraps it alongside the bound state + stack count).
type itemQualityJSON struct {
	Level   int                `json:"level,omitempty"`
	Affixes map[string]float64 `json:"affixes,omitempty"`
}

// itemName renders an item entity's short name for a loot message (its short, else its proto ref).
func itemName(item *Entity) string {
	if item.short != "" {
		return item.short
	}
	return string(item.proto)
}
