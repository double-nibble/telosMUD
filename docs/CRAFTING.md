# Rarity, binding, crafting & deconstruction

The item-economy layer: rarity tiers and bind-on-pickup rules, plus professions that
**deconstruct** items into components and **craft/augment** new ones — a modern-MMO material
economy. It reuses two systems already built: **crafting actions are abilities**
([ABILITIES.md](ABILITIES.md)) and **deconstruction yields are weighted rolls**
([LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md)).

---

## 1. Rarity & binding

- **Tiers (ordered):** `common < uncommon < rare < epic < legendary` — content-defined
  `rarity_tier_defs` (LOOT §2). The engine knows tiers are *ordered*; the names are content.
- **Binding states** (on an item, defaulted by tier, overridable per item):
  - `bind_on_pickup` (BoP) — binds when looted. **Default for epic+ and most rare+.**
  - `bind_on_equip` (BoE) — binds when worn.
  - `unbound` — freely tradeable (typical for common/uncommon and materials).
- **The binding rule:** a **bound** item cannot be given, traded, dropped-for-others, or
  sold to other players. It **can** still be equipped, sold to NPC vendors (if content
  allows), and — crucially — **deconstructed by its owner**.

So binding is a *trade* restriction, not a *use/destroy* restriction. This is the hinge the
whole economy turns on (§9).

## 2. Professions (content)

A **profession** is a content-defined trade skill (Leatherworking, Enchanting, Blacksmithing,
Alchemy, …). The engine knows the *kind* "profession"; the specific trades are content.

```
profession "enchanting" {
  display   = "Enchanting"
  grants    = { abilities = {"disenchant"}, recipes_tagged = "enchanting" }
  skill     = { max = 300, ... }            -- progression curve (content)
}
```

- A character learns professions (count capped, §10) and levels each skill by using it.
- A profession **grants abilities** (the deconstruct/craft verbs) and **unlocks recipes**
  tagged to it. Skill level gates which items/recipes are usable and affects yield/quality.
- Stored in character `state.professions` (PERSISTENCE §3).

## 3. Crafting actions are abilities

`disenchant`, `salvage`, `scrap`, `craft`, `augment` are all **abilities** resolved through the
existing lifecycle (ABILITIES §4): targeting an item, requirement gates (profession + skill +
maybe a station), cost/consumption, and an `on_resolve` that composes effect ops. No separate
crafting engine.

**New item effect ops** added to the vocabulary (ABILITIES §3):

| Op | Does |
|----|------|
| `consume_item(item, qty)`   | destroy / decrement an input |
| `produce_item(proto, qty, {quality, bind, owner})` | create an item (into inventory) |
| `augment_item(item, {attr, amount})` | apply a flat attribute bump to an item's delta |

## 4. Deconstruction (salvage / disenchant)

The ability pattern — a `salvage_item(item, table)` op in the ability's `on_resolve` op-list
(`internal/world/salvage.go`): it consumes the source item and rolls a salvage table (reusing
the loot resolver) into components delivered to the actor.

```
ability "disenchant" {
  invocation = { kind = "command", words = {"disenchant"} }
  targeting  = { mode = "object", scope = "inventory", disposition = "neutral" }
  requires   = { profession = {enchanting = 1}, item_tag = "magical" }
  timing     = { cast_time = 0, lag = 8 }
  on_resolve = { {"salvage_item", item = "<source proto>", table = "salvage:magical"} }
}
```

- **Owner can deconstruct bound items** (§1) — `salvage_item` despawns the source regardless of
  its bound state; the transfer gate never applies.
- Each salvage/disenchant ability is one verb bound to a **fixed source prototype + salvage
  table ref** (not an object-targeted `disenchant <item>` gated by an item tag).
- **Yield = a weighted roll** — a `salvage_table` (same machinery as `loot_table`), keyed by the
  item's tier + type. Disenchanting an epic yields more/higher essence than a rare.
- **Component binding is tier-dependent:** low/mid-tier components are `unbound`
  (tradeable) even when salvaged from a BoP item — this is what feeds the market — while
  **top-tier essence (legendary) is bound**, a deliberate sink at the tier where inflation
  hurts most. A content-defined threshold on `rarity_tier_defs` sets where binding kicks in.

## 5. Components & materials

Components are ordinary `item_prototypes` flagged `material` (leather scraps, arcane dust, epic
essence, …), with a tier/type. They are **stackable** — which adds a small item-model piece:

- Item instances gain an optional **`quantity`** (in the flyweight delta); a **`Stackable`**
  component defines max stack size and merge rules ([MUDLIB.md](MUDLIB.md) §3 component set).
- Stacking, splitting, and merging are engine mechanics; which items stack is content.

## 6. Crafting & recipes

```
recipe "craft:reinforced_leather" {
  profession = {leatherworking = 75}
  station    = "tanning_rack"             -- optional; null = craft anywhere
  inputs     = { {"mat:leather_scrap", 4}, {"mat:iron_buckle", 1} }
  output     = { item = "obj:reinforced_leather", quality = "skill_scaled" }
  on_crit    = "higher_quality"           -- skill-based crit → a better quality band
}
```

A `craft` ability validates profession + skill (+ station presence), consumes inputs, and
produces the output. Output **quality** is a coarse band that scales with skill / crit; the
output is its prototype plus that band. The recipe's skill gate reads the skill-level
*attribute* named by the recipe, not the profession track's `level_attr`.

Each craft, like salvage, is one verb bound to a **fixed recipe ref** rather than a
`craft <recipe>` chosen by argument.

## 7. Augment

`augment_item(item, {attr, amount})` modifies an existing item, applying a **flat attribute
bump** to the item's flyweight delta (`internal/world/crafting_op.go`). It is the single
modification the op supports.

## 8. Binding enforcement

A single engine gate — analogous to the PvP hostility gate (ABILITIES §7) — guards every item
*transfer* (give/trade/drop-to-other/player-sell): it checks the item's `bound` state and
refuses if bound. Deconstruction, equipping, and NPC-vendor sale go through different checks.
Content sets bind rules; the engine enforces them uniformly so no command path can leak a bound
item into trade.

## 9. The material economy loop

The payoff, and the answer to "does personal loot kill the economy?" — **no, because bound
gear re-enters the economy as components:**

```
bind-on-pickup loot (epic+)  ──disenchant/salvage──▶  components
        ▲                                          (low/mid: tradeable · top-tier: bound)
        │ becomes bound                                     │
        └──────────────── new gear ◀──── craft/augment ◀────┴────▶ trade (market)
```

You can't sell the bound epic, but you can disenchant it into essence, then trade or craft with
it. The broad economy flows through **mid-tier materials**, giving non-crafters a reason to feed
crafters — while **top-tier essence stays bound**, so legendary crafting runs on your own
salvage and the very top end can't simply be bought. Materials, not bound gear, are the
tradeable layer.

## 10. Economy design invariants

Three properties that shape the material economy, each with its *why*:

- **Component binding is tier-dependent.** Low/mid-tier components are `unbound` (tradeable);
  top-tier (legendary) essence is bound. The threshold is set on `rarity_tier_defs`. This keeps
  a liquid mid-tier material market while sinking the top end so legendaries can't be bought —
  anti-inflation by design.
- **Professions per character are capped.** A **uniform** ceiling (`craftProfessionCap`,
  currently 2) applies to *every* learned profession, forcing specialization and an
  interdependent economy (`internal/world/profession.go`).
- **Crafting stations are per-recipe.** A recipe may require a station (`forge`,
  `tanning_rack`); many are craftable anywhere. Stations create social/economic hubs and make
  places matter, without forcing every trivial craft to a station.
