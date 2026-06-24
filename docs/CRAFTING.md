# Rarity, binding, crafting & deconstruction

The item-economy layer: rarity tiers and bind-on-pickup rules, plus professions that
**deconstruct** items into components and **craft/augment** new ones — a modern-MMO material
economy. It reuses two systems we already built: **crafting actions are abilities**
([ABILITIES.md](ABILITIES.md)) and **deconstruction yields are weighted rolls**
([LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md)).

Deep affixes/sockets/weapon-altering are **deferred** (see §10) — this doc scaffolds the hooks
without designing that depth.

Status: **proposal** — three choices flagged in §11.

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

- A character learns professions (count limited per §11 D2) and levels each skill by using it.
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
| `augment_item(item, mod)`   | apply a modification to an item's delta (depth deferred, §10) |

## 4. Deconstruction (salvage / disenchant)

The ability pattern:

```
ability "disenchant" {
  invocation = { kind = "command", words = {"disenchant"} }
  targeting  = { mode = "object", scope = "inventory", disposition = "neutral" }
  requires   = { profession = {enchanting = 1}, item_tag = "magical",
                 skill_vs_item_tier = true }          -- skill must meet the item's tier
  timing     = { cast_time = 0, lag = 8 }
  on_resolve = function(ctx)
    local yield = ctx.roll_salvage(ctx.target)         -- weighted roll (reuses loot resolver)
    ctx.consume_item(ctx.target, 1)
    for _, c in ipairs(yield) do ctx.produce_item(c.proto, c.qty, {bind = "unbound"}) end
    ctx.skillup("enchanting")
  end
}
```

- **Owner can deconstruct bound items** (§1). The matching profession + sufficient skill is
  required; under-skilled attempts fail or risk a poorer yield (content's call).
- **Yield = a weighted roll** — a `salvage_table` (same machinery as `loot_table`), keyed by the
  item's tier + type. Disenchanting an epic yields more/higher essence than a rare.
- **Component binding is tier-dependent** (§11 D1): low/mid-tier components are `unbound`
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
  station    = "tanning_rack"             -- optional; null = craft anywhere (§11 D3)
  inputs     = { {"mat:leather_scrap", 4}, {"mat:iron_buckle", 1} }
  output     = { item = "obj:reinforced_leather", quality = "skill_scaled" }
  on_crit    = "higher_quality"           -- skill-based crit → better roll (uses the deferred
                                          --   affix/quality layer when it lands)
}
```

A `craft` ability validates profession + skill (+ station presence), consumes inputs, and
produces the output. Output **quality** scales with skill / crit — but the *rich* quality roll
(affixes, item level) is the deferred affix system; for now an output is its prototype (plus a
coarse quality band).

## 7. Augment (scaffolded, depth deferred)

`augment_item(item, mod)` exists as the hook to modify an existing item using components — add
a socket, slot a gem, reinforce, apply an affix. The **catalog of modifications** is exactly
the deferred affix/socket/weapon-altering design (§10), so v1 ships the op and a minimal set
(e.g. a flat stat bump), and the rich modification vocabulary arrives with that subsystem.

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

## 10. Explicitly deferred

Parked by your call, to design as its own subsystem later:
- Deep **affix** system (stat budgets, affix pools per slot/tier, interaction with the
  attribute modifier stack).
- **Sockets** and gems.
- **Weapon-altering** buffs / enchants and the full `augment` modification catalog.
- Rich crafted-item **quality rolls** (depends on the affix system).

The hooks above (`augment_item`, `quality` bands, the `Affected`/affix delta) are placed so
that work slots in without reshaping items or crafting.

## 11. Decisions (settled)

| # | Decision | Resolution | Rationale |
|---|----------|------------|-----------|
| D1 | **Component binding** | **Tier-dependent:** low/mid-tier components `unbound` (tradeable); **top-tier (legendary) essence bound.** Threshold set on `rarity_tier_defs`. | Keeps a liquid mid-tier material market while sinking the top end so legendaries can't be bought — anti-inflation by design. |
| D2 | **Professions per character** | A **cap on crafting professions** (e.g. 2), with **gathering/utility skills unlimited**; content-configurable. | Forces specialization and an interdependent economy without gating basic resource collection. |
| D3 | **Crafting stations** | **Per-recipe:** a recipe may require a station (`forge`, `tanning_rack`); many craftable anywhere. | Creates social/economic hubs and makes places matter, without forcing every trivial craft to a station. |
