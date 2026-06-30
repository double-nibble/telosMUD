# Phase 13 — Crafting & economy

The close of **Track D**: the item-economy layer — rarity binding + professions that **deconstruct** items
into components and **craft/augment** new ones, the modern-MMO material loop. Design:
[CRAFTING.md](CRAFTING.md). It answers "does personal loot (Phase 12) kill the economy?" — *no, because
bound gear re-enters the economy as components.*

Status: **plan LOCKED (scope confirmed 2026-06-29).** Building 13.1 → 13.5 + capstone.

**Settled forks (confirmed):** (1) FULL scope (13.1–13.5 + capstone), `augment_item` kept to a flat-stat-
bump stub (the rich affix/socket catalog stays §10-deferred). (2) Professions reuse the Phase-11.2 track
system for skill levels + a thin `state.professions` set for membership/the D2 cap. (3) The `bound` state +
stack count ride the item-instance delta (Phase 12.3 round-trip). (4) Stations = a ROOM flag (`forge`) for
v1, with a furniture-upgrade hook. (5) Stacking = coarse v1 (merge identical materials on pickup + `split`);
broader auto-stacking is a follow-up.

## The binding shape (settled — do not re-litigate)

CRAFTING.md §11 settled the three core decisions; they are binding inputs:

- **D1 — Component binding is TIER-DEPENDENT.** Low/mid-tier salvage components are `unbound` (tradeable —
  this feeds the market); top-tier (legendary) essence is `bound` (sinks the top end so legendaries can't
  be bought). The threshold sits on `rarity_tier_defs`.
- **D2 — A CAP on crafting professions** (e.g. 2), gathering/utility unlimited; content-configurable.
- **D3 — Stations are PER-RECIPE.** A recipe may require a `forge`/`tanning_rack`; many craft anywhere.

And the architecture the doc fixes:
- **Crafting actions are ABILITIES** (disenchant/salvage/craft/augment) — the existing lifecycle (requires
  gates + costs + an `on_resolve` op-list), no separate crafting engine. Adds new **item effect ops**.
- **Binding is a TRADE restriction, not a use/destroy one** — a bound item can still be equipped,
  destroyed, and (crucially) **deconstructed by its owner**. One engine gate guards every transfer.
- **Salvage yield = a weighted roll** reusing the Phase-12 loot resolver (a `salvage_table` keyed on the item).
- **Deep affixes/sockets/weapon-altering are DEFERRED** (§10) — Phase 13 scaffolds the `augment_item` hook
  + coarse quality bands; the rich modification catalog is its own later subsystem.

## What already exists (the foundation)

- **The effect-op interpreter** — the new ops (`consume_item`/`produce_item`/`augment_item`) register
  exactly like the Phase-11 grant ops (effect_op.go).
- **The ability lifecycle** — requires (reqAttr, `requires_grant`, cooldown, tags) + costs + on_resolve.
  Phase 13 adds a `requires.profession` + `requires.station` gate (checkRequires).
- **Item-instance deltas** (Phase 12.3) — `ItemJSON.Delta` + the round-trip; the `bound` state + an
  augment delta ride it, exactly like rolled quality.
- **The loot resolver** (Phase 12.1) — `salvage_table` reuses it (the same weighted-roll machinery).
- **`rarity_tier_defs`** (Phase 12.1) — the binding threshold (D1) is a field on a tier.
- **The track system** (Phase 11.2) — a profession's skill is a use-based track (`OnSkillUse`→advance);
  professions reuse it rather than inventing a new progression mechanism.
- **`grant_ability` + ownership** (Phase 11.4) — a profession grants its craft/deconstruct verbs.

On-pillar: every tier, binding rule, profession, recipe, and salvage table is CONTENT; the engine runs the
ops + the transfer gate and names no profession or item.

## Testing mandate (binding — memory `testing-standard`)

Every slice ships tests across all tiers: unit + table-driven for the ops + the binding gate + the salvage
roll (seeded RNG → deterministic), gated PG round-trip for each new def-table + the bound/stack/profession
state, and the capstone milestone. Per-slice reviews: owning engineer = `progression-engineer`;
cross-cutting = `persistence-engineer` (def-tables + item/profession state), `security-auditor` (the
binding gate can't be bypassed; no item dupe via craft/salvage/stack-split), `mudlib-engineer` (the
item-model stacking piece).

## Slices

### 13.1 — Binding + the transfer gate
The hinge the economy turns on. A per-item `bound` state in the instance delta; a bind rule on the item
prototype (`bind_on_pickup` / `bind_on_equip` / `unbound`), applied on loot (BoP) / equip (BoE). One engine
gate guards every TRANSFER (give / trade / drop-to-other / vendor-sell) and refuses a bound item — while
equip, destroy, and owner-deconstruct stay allowed.
- **Done when:** a bind-on-pickup item binds when looted, cannot be given/dropped-for-another, but can
  still be equipped + (later) deconstructed by its owner; the bound state survives a reload.

### 13.2 — Stackable materials
The item-model piece materials need. A `Stack` component (count + max) on a prototype flagged a material;
identical material instances merge on pickup; a `split` command divides a stack. Which items stack is
content (a `material` flag + tier/type on the prototype); stacking/merging/splitting is engine mechanic.
- **Done when:** picking up two stacks of the same material merges them (bounded by max), splitting yields
  two stacks, and stack counts round-trip through persistence.

### 13.3 — Crafting ops + professions
The op vocabulary + the trade-skill model. New ops: `consume_item(item, qty)` (destroy/decrement an input),
`produce_item(proto, qty, {quality, bind, owner})` (create into inventory), `augment_item(item, mod)` (the
deferred-depth hook + a minimal flat-stat-bump v1). Professions: `profession_defs` content (a trade kind +
the abilities/recipe-tags it grants); a profession is LEARNED (grant) and its skill is a use-based track
(Phase 11.2); the ability `requires` gains a `profession`+`skill` gate. `state.professions` (the cap, D2).
- **Done when:** learning a profession grants its verbs + a skill track; a craft-style ability gated on
  `requires.profession` is refused without it and runs the consume/produce ops with it; the profession +
  skill survive a reload.

### 13.4 — Deconstruction (salvage / disenchant)
The economy's source of components. `salvage` / `disenchant` abilities: gated on profession+skill+item-tag,
they roll a `salvage_table` (reusing the Phase-12 loot resolver), consume the item, and produce the
components — **owner can deconstruct a BOUND item** (§1), and **component binding is tier-dependent** (D1:
low/mid components unbound, top-tier essence bound).
- **Done when:** an owner disenchants a bound epic into tradeable mid-tier components (unbound) + a bound
  top-tier essence, the source item is consumed, and the yield is a deterministic weighted roll under a seed.

### 13.5 — Crafting & recipes
Closing the loop. `recipe_defs` (profession+skill, optional station, inputs, output + a coarse quality
band); a `craft` ability validates profession+skill (+ station presence, D3 — a station = a room flag),
consumes the inputs, and produces the output. Output quality scales with skill/crit (coarse band; the rich
roll is the deferred affix system).
- **Done when:** a character with the profession crafts an item at the required station from its component
  inputs (consumed), the output lands in inventory, and crafting without the station/skill is refused.

### 13.x — Capstone (the done-when)
The §9 material-economy loop: **disenchant a bound epic into tradeable mats, then craft a new item at a
station** — proving bound gear re-enters the economy as components, the transfer gate holds (the bound epic
can't be sold but its mid-tier components can be traded), and the whole flow survives a restart. Demo
content (a profession + a salvage table + a recipe + a station room) + the milestone + persistence tests.

## Decisions to settle before building (the open forks — §11 is already settled)

1. **Scope.** Full (13.1–13.5 + the minimal augment + capstone), or defer 13.5 crafting/recipes to keep it
   to the binding+salvage+economy-source half? *Recommend:* full — the capstone (disenchant → craft) needs
   both halves to demonstrate the loop; keep `augment_item` to a flat-stat-bump stub (the rich catalog is
   §10-deferred regardless).
2. **Professions: reuse the Phase-11.2 track system, or a dedicated `state.professions` subtree?**
   *Recommend:* a profession's SKILL is a use-based track (no new progression mechanism); a thin
   `state.professions` set records which professions are learned (for the D2 cap + recipe gating). Hybrid:
   tracks for levels, a small set for membership.
3. **The `bound` state + stack count: ride the item-instance delta (Phase 12.3), or a new per-item table?**
   *Recommend:* the instance delta — `bound` and stack count join rolled quality in `ItemJSON.Delta`, reusing
   the round-trip just built.
4. **Station model: a ROOM flag (a room tagged `forge`) vs a nearby furniture item?** *Recommend:* a room
   flag for v1 (simplest, "places matter"), with the content hook to upgrade to a furniture item later.
5. **Stacking depth: merge-on-pickup + a `split` command (coarse v1), or full auto-stacking everywhere
   (inventory, ground, containers, trade)?** *Recommend:* coarse v1 — merge identical materials on pickup +
   `split`; the broader auto-stack surface is a follow-up.

## Builds on / relates to

Phase 11 (the effect-op grant precedent, the track system, ability ownership) · Phase 12 (item-instance
deltas, the loot resolver reused for salvage, rarity tiers for the binding threshold). The deferred affix/
socket/weapon-altering subsystem (§10) is a FUTURE phase the `augment_item` hook + quality bands scaffold
for. Auth + the website (the real chargen front end the Phase-11 bundles feed) are **Phase 14**; hardening
+ scale are **Phase 15** — Phase 13 closes the engine/content arc before the services track.
