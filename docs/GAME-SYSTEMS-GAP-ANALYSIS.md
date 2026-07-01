# Game-systems content gap analysis

**Owner:** `rpg-systems-designer`. **Status:** design input for Phases 6 / 7 / a chargen-progression
phase / 11 / 12. **Companion docs:** [PRINCIPLES.md](PRINCIPLES.md), [ABILITIES.md](ABILITIES.md),
[COMBAT.md](COMBAT.md), [PERSISTENCE.md](PERSISTENCE.md), [OPEN-GAME-SYSTEMS.md](OPEN-GAME-SYSTEMS.md),
[the roadmap in COMPLETED.md](COMPLETED.md#roadmap-overview).

This document does **not** modify code or schema. It answers one question exhaustively: *can the
TelosMUD engine express our target game systems as pure content (def-table rows + JSONB + Lua), with
zero engine changes for flavor — and where it can't, exactly what new mechanism is needed and which
roadmap phase should own it?*

---

## 0. Thesis, targets, methodology

### 0.1 The pillar restated
The engine is **mechanism**; content is **flavor** ([PRINCIPLES.md](PRINCIPLES.md)). The engine knows
*kinds* ("an attribute exists", "a resource exists", "an affect exists", "an effect op exists"); every
*instance* (`strength`, `rage`, `stunned`, `fireball`) is a content row. A proposed engine change that
can only be expressed by naming a class / spell / resource / condition in Go is a design smell. The
test the whole analysis applies: **design to the union abstraction; test against the spread.** A
mechanism that fits only 5e is wrong.

### 0.2 The targets
- **Three capstones** (chosen because they diverge sharply — they triangulate the abstraction):
  - **D&D 5e SRD 5.2** (CC-BY): Vancian spell *slots*, six ability scores → modifiers + proficiency
    bonus, advantage/disadvantage, class+subclass+background+feat at fixed levels, short/long rest.
  - **Pathfinder SRD (1e)** (OGL): the d20 3.x lineage 5e simplified — *much* more granular: BAB,
    separate Fort/Ref/Will saves, iterative attacks, skill *ranks*, feat *chains*, prepared *and*
    spontaneous casting, prestige classes, CMB/CMD for combat maneuvers, size modifiers.
  - **Text World of Warcraft** (the WoW d20 RPG, OGL, used as the rules skeleton, plus the live-MMO
    feel as the experience target): class **resource diversity** (rage that *builds* from combat,
    energy that *regens fast*, focus, mana, runic power, combo points, soul shards), **talent trees**,
    **cooldowns** as the primary pacing mechanism (not slots), **threat/aggro**, **dual-spec**, and a
    raid/loot economy (the [LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md) / [CRAFTING.md](CRAFTING.md) target).
- **MUD heritage as a first-class baseline** (NOT an afterthought). The full spectrum the engine spans:
  - **TinyMUD / MUSH** — contentless social/building; little-to-no stats; bare engine + in-game OLC.
  - **DikuMUD / Merc / ROM** — fixed HP/SP/move pools, level growth, recovery **ticks** (the pulse
    scheduler *is* the tick), an optional class pool. The *simplest* subset of the generic model.
  - **LPMud / Discworld mudlib** — **skill-based**, advance-through-**use** (no levels;
    `OnSkillUse → chance-to-improve`), heavy soft-coded objects → Lua.
  - **Rich tabletop** — the three capstones.
  An explicit acceptance question, asked in §16: *can it still be a plain Diku / LP / Tiny MUD?*
- **Catalog breadth** ([OPEN-GAME-SYSTEMS.md](OPEN-GAME-SYSTEMS.md), ~36 systems) used to pressure-test
  generality — d100/BRP (Delta Green, OpenQuest, Legend), dice-pool/narrative (FATE, PbtA/Dungeon
  World, Blades, Year Zero, Cypher, Gumshoe), rules-light (Cairn, Tunnel Goons, Lumen), supers
  (FASERIP/4C). These surface gaps the three capstones hide (a dice *pool*, a *clock*, a *stress* track,
  a *push/devil's-bargain*).

### 0.3 The two translation theses (applied to every concept — this is the analysis, not a footnote)

**Thesis 1 — DM-adjudication → deterministic builder-content + engine roll.** Tabletop assumes a human
referee making non-deterministic calls. We have none. Every check / save / attack / contested roll
becomes one shape: a **check-gated branch**. A *builder* authors the parameters (the climbable wall =
"DEX check vs DC 15", or "unclimbable: too slick"); the *engine* resolves it deterministically (seeded
per-zone RNG: `roll + modifier vs DC`); the outcome runs a branch (`on success X / on fail Y | half |
none`). Saving throws, attack rolls, ability checks, contested rolls, opposed skill rolls — all one
shape. The check's RNG is engine; the DC / stat / consequence is content. **Roll visibility** ("you
rolled 14+6 vs 15 — success" vs "you scramble up the wall") is a **system-level default, overridable
per action** — never baked into the engine.

**Thesis 2 — the room-graph reframes space.** Tabletop assumes continuous space (feet, grids, templates,
line-of-sight, facing). Ours is a room/exit graph. Every spatial mechanic must be reframed and called
out: teleport → move-to-a-room-ref (named dest / marked anchor / random adjacent); AoE → "the room" or
"room + adjacent rooms" (loop the harm-gate per target); range → exit distance / line-of-sight along
exits; movement-in-combat → between rooms or an abstract intra-room position. Where a spell genuinely
can't fit (precise teleport coordinates, true flight, grappling positioning), say so and propose the
content-expressible substitute.

### 0.4 The binding constraints (non-negotiable design inputs)
1. **Generic resources** — content pools + content-defined dynamics (passive regen, event-driven gain,
   decay, cost). No hardcoded hp/mana; `vital` is a flag.
2. **N independent advancement tracks** — char level / guild level / class level, content grants, with
   *all* modes expressible: XP-threshold auto, train-at-trainer, point-buy, and **use-based**.
3. **Events as universal glue** — `OnHit / OnKill / OnLevel / OnSkillUse / OnCheck → content ops`.
4. **The full spectrum stays first-class** — contentless → fixed-pool-leveled-ticks → skill-use → rich
   tabletop.
5. **Presentation is driver-emits-data** — GMCP + optional server-side UTF-8/color map; rich client out
   of scope; honor the room graph.

### 0.5 What is actually built (Phases 0–5), grounded in the code
This analysis grounds every "maps onto our model" verdict in the read source, not assumption:
- **Attributes** (`internal/world/attributes.go`, `defs.go`): content `attribute_defs` with a
  modifier stack `base → +flat → ×mul → clamp`, derived attributes are formulas over other attributes
  (recursive, cycle-linted), per-entity base overrides (`setAttrBase`), cache+dirty. No `level`/`str`
  is hardcoded — all are rows.
- **Resources** (`resources.go`): named pools, `max` is a derived attribute, per-entity `current`,
  `vital` flag, flat `regen` per tick driven by the pulse. `vitalResource()` finds the first `vital`
  pool — combat subtracts from *that*, never a hardcoded "hp".
- **Damage types** (`defs.go`): named, with a resist/vuln/immune multiplier matrix.
- **Affects** (`affected.go`, `affect_runtime.go`, `defs.go`): content `affect_defs` with modifiers,
  `prevents` tags (the tag-CC model), four stacking modes, durations in pulses, per-entity tick,
  `on_tick` op-list (DoTs), reserved `on_apply`/`on_expire` (Phase 7 Lua), derived-harm gate
  (`affectIsDetrimental`).
- **Abilities** (`ability.go`, `defs.go`): the fixed 10-step lifecycle; `ability_defs` with targeting
  (self/none/enemy/ally), disposition, requires (attr thresholds + `not_prevented` tag-CC), resource
  costs, cast-time/lag/cooldown timers on the pulse, an `on_resolve` op-list, reserved `on_resolve_lua`.
- **Effect ops** (`effect_op.go`, `effect_op_handlers.go`): `deal_damage` (flat or `NdS` dice, scaled),
  `heal`, `restore`, `modify_resource`, `apply_affect`, `remove_affect`, `dispel`, `act`, `send`, `if`
  (only "has affect?" today), `chance`. A **seeded RNG** (`effectCtx.rng`, `rollDice`, `rollChance`) is
  already in the resolution context — the deterministic-roll substrate exists.
- **The PvP / hostility gate** (`pvp.go`, `guardHarmful`): every harmful op funnels through *one*
  chokepoint before touching a player's state; harm is **derived**, not trusted from a label.
- **Persistence** (`PERSISTENCE.md`, `character.go`): one def-table per kind + JSONB tail; character
  `state` JSONB holds `templates / attributes / resources / skills / affects / flags / inventory /
  equipment`. New flavor = a row, never a migration.
- **Pulse scheduler** (`pulse.go`): `every(n)` / `after(n)` timers with the resolve-by-id / skip-frozen
  contract. The recovery **tick** and combat **round** both ride this.
- **Not yet built (the gap surface):** combat round/swing pipeline (Phase 6), Lua (Phase 7), the event
  bus that fires `OnHit/OnKill/OnLevel` to content (Phase 6/7), progression / chargen, GMCP (Phase 9),
  loot (Phase 11), crafting/economy (Phase 12).

### 0.6 How to read a section
Each concept area gives: **(a)** the cross-system comparison (where 5e / PF / WoW / MUD-heritage
*diverge* — divergence is where the abstraction earns its keep); **(b)** the deterministic/content
translation (+ roll-visibility); **(c)** the room-graph translation where spatial; **(d)** *maps onto
our model as* — concrete def-table / component / op / affect / event / Lua hook / track; **(e)** a
**verdict**: `expressible-today` / `needs-new-mechanism` / `needs-Lua`, and for a new mechanism, *what*
and *which phase owns it*. New mechanisms are tagged **[G#]** and collected in §17.

---

## 1. Attributes, ability scores & modifiers

**(a) Cross-system.** 5e: six scores (STR/DEX/CON/INT/WIS/CHA, 1–20+), the modifier `(score−10)/2`
floored, plus a single **proficiency bonus** scaling with character level. PF: same six, but modifiers
feed a denser web (BAB, three saves, skill ranks, CMB/CMD) and there is no single proficiency bonus —
each thing scales separately. WoW d20: the same six scores; the live-MMO layer adds derived combat
ratings (attack power, spell power, crit %, haste, armor) that are *themselves* attributes derived from
gear. Diku: STR/INT/WIS/DEX/CON (+ apply tables). LP/Discworld: often *no* fixed attributes — bonuses
come from skills. TinyMUD: none.

**(b) Deterministic/content translation.** Pure derivation, no DM call. The 5e modifier is a derived
attribute `str_mod = floor((strength − 10) / 2)`; proficiency bonus is a derived attribute keyed off a
`level` attribute. PF's BAB / saves / CMB / CMD are each derived attributes (`bab`, `fort_save`,
`will_save`, `cmd = 10 + bab + str_mod + dex_mod + size_mod`). WoW's attack power = a formula over
gear-granted attributes. The *contentless* case is the default: an entity with no attribute defs has
none, and `attr()` returns 0 sanely (the bare-engine invariant).

**(d) Maps onto our model as.** `attribute_defs` rows + the formula stack (`attributes.go`). Score = a
literal-base attribute with a per-entity base override (chargen/point-buy writes it via `setAttrBase`);
modifier / proficiency / BAB / save / AP = `value_kind:"derived"` attributes whose base is a formula
over other attributes. The formula AST today supports `+ - * / min max clamp attr lit`.

**(e) Verdict: expressible-today**, with one formula-vocabulary gap. The 5e modifier needs `floor`
(integer division) and PF/WoW use conditional and step formulas (`floor`, `ceil`, a "every Nth level"
step). **[G1] Formula vocabulary extension** — add `floor`/`ceil`/`round`/`mod`/conditional (`if`/
`select`) heads to the formula AST. Small, additive, **Phase 6** (combat needs derived to-hit/AC
formulas anyway) or pulled into a chargen phase. Until then, content can approximate with the existing
`/` and `clamp`, but integer flooring is not exact — so this is a genuine (small) gap.

---

## 2. Checks, saves & contested rolls — the deterministic-roll primitive

This is the single biggest new mechanism the analysis surfaces. It is the engine home of **Thesis 1**.

**(a) Cross-system.** 5e: `d20 + ability mod + proficiency (if proficient) vs DC`; *advantage/
disadvantage* = roll 2d20 keep high/low; saving throws are checks vs a spell-save DC; attack rolls are
checks vs AC; ability checks vs a DM-set DC; contested = both roll, higher wins. PF: `d20 + modifier vs
DC`, no advantage, but many stacking typed bonuses; opposed rolls; combat maneuvers = `d20 + CMB vs
CMD`. WoW d20 RPG: same d20 core; the *MMO* layer replaces "does it hit?" with hit/crit % chances and
"does the CC land?" with a resist roll. d100/BRP (Delta Green, Legend): `roll d100 ≤ skill%`; degrees of
success (crit/special/fumble). PbtA (Dungeon World): `2d6 + stat`, 10+ full / 7–9 partial / 6− miss — a
*three-outcome* check. Blades: a *dice pool* (Nd6, highest die: 6 full / 4-5 partial / 1-3 bad, 6-6
crit). FATE: `4dF (−,0,+) + skill vs difficulty`, shifts matter. Cairn/Tunnel Goons: roll-under /
2d6+stat. Diku skills: a stored proficiency % rolled against. LP/Discworld: a skill *bonus* vs a task
difficulty, and crucially **the use itself is the advancement trigger** (§5).

The divergence is total in *dice shape* (d20 / d100 / 2d6 / NdF / dice-pool) and in *outcome arity*
(binary / half-on-success / three-tier / degrees-of-success / shifts). The **shape that is invariant**:
*resolve a randomized magnitude against a threshold (or another roll), classify into one of N
outcome bands, run the band's branch.*

**(b) Deterministic/content translation.** The builder authors a **check spec** as content; the engine
resolves it deterministically with the seeded per-zone RNG (already present in `effectCtx.rng`). A check
spec carries:
- `dice` — the randomized term, content-defined: `1d20`, `2d20kh1` (advantage), `1d100`, `2d6`, `4dF`,
  `3d6` (a pool count). The engine knows how to roll dice (`rollDice` exists); the *notation* is content.
- `bonus` — a formula over the actor's attributes (`+str_mod + prof`).
- `vs` — either a literal/formula **DC**, or `contested` (roll the defender's own check spec; compare).
- `bands` — an ordered list of `(threshold|margin → outcome label)`: binary `{success, failure}`;
  half-on-save `{success→half, failure→full}`; PbtA `{≥10→strong, 7..9→weak, ≤6→miss}`; BRP degrees
  `{crit, special, success, failure, fumble}`. The engine classifies the rolled total into the matching
  band and runs that band's op-list branch.
- `visibility` — `show` / `hide` / `summary` (the roll-visibility config; system default + per-check
  override).

This makes the climbable-wall concrete: a room exit / object carries `check = {dice:"1d20",
bonus:"dex_mod + athletics", vs:15, bands:{success→[move ...], failure→[deal_damage fall ...]}}`; the
builder authored the wall, the engine rolls. A saving throw is the *same* spec invoked from a spell's
op-list (`save = {dice:"1d20", bonus:"$target.dex_save", vs:"$caster.spell_dc", bands:{success→half,
failure→full}}`). An attack roll is the same spec vs the defender's AC attribute (§9). A 5e *condition*
that ends "on a successful save at end of turn" is the same spec fired on the affect tick.

**(c) Room-graph.** N/A for the roll itself; the *consequence* branch may be spatial (the failed climb
moves you down a room — §7's teleport-to-room-ref).

**(d) Maps onto our model as.** A new **`check` effect op** (the flow op that resolves a check spec and
runs the matching band's nested op-list — structurally identical to the existing `if`/`chance` flow ops,
which already recurse into `runOps`). Plus a **`check_def` / inline check spec** carried in JSONB on
abilities, exits, objects, and affect ticks. The dice roller and seeded RNG already exist; what's
missing is (i) dice *notation* parsing beyond `NdS` (keep-highest, dF, pools), (ii) the band classifier,
(iii) the `bonus`/`vs` formula evaluation against *both* actor and target, (iv) the visibility config and
its GMCP/text emission. The `OnCheck` event (constraint 3) fires here so content can react (a bardic-
inspiration reroll, a lucky halfling).

**(e) Verdict: needs-new-mechanism. [G2] The check/save/contested primitive** — the central gap.
Sub-parts: **[G2a]** extend dice notation (keep-high/low for advantage, `dF`, pool-count-successes);
**[G2b]** the `check` flow op + outcome-band classifier; **[G2c]** check-spec formula context with
`$actor`/`$target`/`$source` scoping; **[G2d]** roll-visibility config + emission; **[G2e]** the
`OnCheck` event. **Phase owner: Phase 6 (combat).** Rationale: attack rolls and saving throws are
checks, and the combat pipeline (§9) is *built from* this primitive — so the check primitive should land
*with* or *just before* combat, not after. It is the load-bearing abstraction for everything from a
climb to a fireball save to a Blades dice-pool action; getting its generality right (outcome **bands**,
not a hardcoded binary) is the most important single design decision in this document.

> **Design note — keep the engine ignorant of dice *shape*.** The engine must roll a *content-named*
> dice expression and classify into *content-named* bands. If the engine ever hardcodes "d20" or
> "success/failure", PbtA's three-tier and BRP's degrees and Blades' pools become inexpressible. The
> band list is the union abstraction; binary 5e is just the 2-band case.

---

## 3. Resources & their dynamics (HP / mana / SP / rage / energy / focus / ki / slots / per-rest)

This is binding-constraint #1, and the WoW capstone is what stresses it hardest.

**(a) Cross-system.** 5e: HP (vital, restored on rest), spell **slots** (per-level discrete pools,
refilled on long rest; some on short rest), a few per-rest dice (hit dice, channel divinity, ki,
sorcery points, superiority dice, wild shape). PF: HP, slots (prepared *or* spontaneous), per-day class
pools (ki, bardic performance rounds, channel energy), grit/panache, mythic power. WoW: the resource
*zoo* — **mana** (regens, large pool), **rage** (starts at 0, *builds* from dealing/taking damage,
decays out of combat), **energy** (small, *regens fast*, rogue), **focus** (hunter, like energy),
**runic power** (builds from rune use), **combo points** (a 0–5 builder consumed by finishers),
**soul shards / holy power / chi** (charge-style builders), and **cooldowns** as the dominant pacing
resource. Diku/ROM: HP / mana / move, all `vital`-ish, all passive-regen on a tick. LP/Discworld: often
just GP (guild points) for spells; HP. TinyMUD: none.

The divergence: **dynamics**. 5e slots refill on a *rest event*; rage *builds on a combat event* and
*decays on a timer when out of combat*; energy regens *fast and passively*; combo points are a *bounded
builder* spent by a *finisher*; mana regens *passively*. No single "regen rate" covers these.

**(b) Deterministic/content translation.** Already mostly content. A `resource_def` has a `max` (derived
attribute) and a `regen` (per-tick). The missing dynamics are **event-driven** and **conditional**:
- *Builders* (rage, runic power, combo points) — content hooks `OnHit`/`OnDamageTaken`/`OnAbilityResolved`
  → `modify_resource(self, rage, +N)`. This is *exactly* the events-as-glue constraint. The op already
  exists; the **event** to hook does not (Phase 6).
- *Decay out of combat* (rage) — a conditional regen: `regen: −N when not in combat`. Needs the regen
  rule to be *conditional* on a content predicate (a flag / a state), not a bare constant.
- *Per-rest pools* (slots, ki, channel divinity) — refill on a **rest event** (§4): `OnRest →
  restore(self, slot_1, max)`. A "rest" is content (a `rest` command/ability that fires the event +
  applies a recovery affect); the engine just needs the event and the restore op (both exist / are
  small).
- *Spell slots specifically* — N discrete pools (`slot_1 … slot_9`), each a resource whose `max` is a
  derived attribute from the class track (§6). Casting a level-3 spell `costs: {slot_3: 1}`; upcasting =
  content choosing which slot to spend (a small Lua/op branch, or distinct ability variants). Pact magic
  / sorcery-point conversion = content ops on a rest or on demand. This is the per-rest case plus a
  cost; both expressible once the rest event exists.
- *Bounded builders + finishers* (combo points) — a resource with `max:5`; finishers read the current
  value (`scaling` by `resource(self, combo)`), then zero it. Needs ops to *read a resource into a
  damage/scaling term* and `modify_resource` to 0 — the read-into-scaling is a small op gap.

**(d) Maps onto our model as.** `resource_defs` (built) for the pools; `attribute_defs` for their
derived maxes; `modify_resource`/`restore`/`heal` ops (built) for the changes; the **event bus** (Phase
6/7) to drive event-based gain/decay; a **conditional regen predicate** on `resource_def`; a **rest
event** (§4). Cooldowns already exist as a timing field on abilities (`armCooldown`) but are transient
(not persisted) and lack a step-3 "still cooling down" check — see §7.

**(e) Verdict: mostly expressible, three small new mechanisms.**
- **[G3] Event-driven resource dynamics** — requires the **engine event bus** firing `OnHit /
  OnDamageTaken / OnKill / OnAbilityResolved` to content op-lists (the universal glue). **Phase 6** (the
  events originate in combat) with the subscription surface finished in **Phase 7** (Lua handlers).
- **[G4] Conditional / formula regen** — let `resource_def.regen` be a formula/predicate (`−5 when
  flag:out_of_combat`) rather than a constant. Small, **Phase 6**.
- **[G5] A `rest` event + resource-refill semantics** — a content `rest`/`recover` action that fires an
  event pools subscribe to; short-rest vs long-rest are just two content events with different op-lists.
  **Phase 6** (pairs with regen) or a chargen phase. Also: a **`resource → scaling` read** so a finisher
  scales by combo points (extend `deal_damage`'s `scaling` to read a resource; tiny). And **cooldown
  persistence** (§7).

> **Binding-constraint check (resources):** the constraint holds, *provided the event bus lands*. Rage,
> energy, combo points, and slots are all generic pools + content dynamics — but "content dynamics" for
> the interesting cases *means* event subscriptions. The single most important enabling mechanism for
> the resource constraint is therefore **[G3] the event bus**, not anything resource-specific. `vital`
> already decouples "death pool" from "hp" (`vitalResource()` reads the flag), so a system with no HP at
> all (a pure social MUD) or a system where the death pool is "Wounds" (WoW d20's wound points, §3 of the
> WoW RPG) both work without engine change.

---

## 4. Rest / recovery / the tick

**(a) Cross-system.** 5e: short rest (1 hr — hit dice, some pools) / long rest (8 hr — HP full, slots,
most pools). PF: 8-hr rest for slots/HP. WoW: out-of-combat regen (fast for energy, eating/drinking for
mana/HP), cooldown decay in real time — *no* slot-rest concept. Diku/ROM: the **tick** (a fixed
real-time interval) regenerates HP/mana/move and is the heartbeat of recovery; the pulse scheduler *is*
this tick. LP: regen on a heartbeat too.

**(b) Translation.** The Diku tick is already the pulse (`runRegen` on the affect/regen tick). 5e/PF
rests are a content **event**: a `rest` ability with a cast-time (or a safe-room requirement) that fires
`OnRest`, which content op-lists turn into `restore` of slots/pools + HP. Short vs long = two events.
WoW's fast out-of-combat regen = conditional regen (§3 [G4]) keyed on a combat-state flag.

**(d) Maps onto our model as.** `runRegen` (built) for the tick; a `rest` `ability_def` + the `OnRest`
event ([G5]) for tabletop rests; conditional regen ([G4]) for combat-state-dependent rates.

**(e) Verdict: Diku tick expressible-today; tabletop rests need [G5] + [G4] (Phase 6).**

---

## 5. Progression, leveling, advancement modes & character-creation timing

Binding-constraint #2, deferred today (reserved `class_defs`/`race_defs`). The richest gap.

**(a) Cross-system.**
- *Character level vs class level.* 5e: a single **character level**; class features are grants at that
  level; multiclassing splits levels across classes but caps total at 20 and shares proficiency bonus.
  PF: **per-class levels** that sum to a character level; BAB/saves are per-class and *added*; prestige
  classes are extra tracks with entry prerequisites. WoW d20: class levels; the MMO feel adds **talent
  points** per level spent in **trees**, and **dual-spec** (two saved talent allocations).
- *Timing.* 5e/PF/WoW: class chosen at creation. Many MUDs: **newbie, then join a guild at level 5** —
  the class track *starts later*, joined by an in-game action. LP/Discworld: you join a guild and then
  **advance skills by using them** — no levels at all.
- *Advancement mode.* 5e/PF/WoW: **XP-threshold auto-level** (cross a threshold → gain a level → apply
  grants), or milestone (a content trigger grants a level). Diku: XP-auto-level, sometimes **train at a
  guildmaster** (spend gold/XP to gain practices, then practice skills). Older D&D / some MUDs:
  **train-at-a-trainer** (visit an NPC, spend currency). Point-buy: spend a pool per level (stat or
  talent points). **Use-based** (LP/Discworld, also BRP's "check the box, roll to improve on rest"):
  `OnSkillUse → chance-to-improve` — an event-driven track with *no levels*.

The divergence is total: number of tracks, when they start, what advances them, and whether "level" even
exists. The union abstraction the constraint already names: **N independent advancement tracks, each
with content-defined XP/progress sources, thresholds, and per-step grants, joined by content actions.**

**(b) Deterministic/content translation.** No DM here — leveling is mechanical. The model:
- A **track** is content: `{ progress_attr, thresholds[], grants_per_step }`. `progress_attr` is just an
  attribute (`xp`, `mining_skill`, `warrior_xp`). A threshold list maps progress → step. A step's
  **grants** are an op-list run once when the step is reached: `setAttrBase` raises level/stats,
  `grant_ability`, `grant_resource` (unlock a pool), `apply_affect` (a permanent passive), set a flag.
- **Advancement modes** are *which event feeds the track*:
  - XP-auto: `OnKill → modify_attribute(xp, +reward)`; the engine checks the track's thresholds and, on a
    crossing, fires `OnLevel` → runs the grants. (Diku.)
  - Train-at-trainer: an NPC `ability` that spends a currency and runs the grant op-list directly (no
    auto-threshold). (Old-school MUD.)
  - Point-buy: a level grants a "points" resource; a `spend_points` ability `setAttrBase(+1)` per point.
    (WoW talents, 5e ASIs.)
  - Use-based: `OnSkillUse → chance(p, modify_attribute(skill, +1))`; the skill attribute *is* the track,
    no level. (LP/Discworld, BRP.)
- **Timing** is content: a class track with an empty grant at step 0 and entry gated by a "join guild"
  ability that *adds the track to the entity* at level 5. Multiclass = a second class track added by a
  content action. Prestige class = a track with entry-prerequisite checks (a `check` against attributes).
- **Grants need a place to live.** Today, attributes have per-entity base overrides and abilities are
  granted as command verbs; a `class_def`/`race_def`/`background_def`/`feat_def`/`talent_def` is a
  content **bundle**: a set of grants (attrs/resources/abilities/flags/affects) applied when chosen or
  when a track step is reached. The engine knows the *kind* "template/bundle"; the bundles are content.

**(c) Room-graph.** N/A.

**(d) Maps onto our model as.** New def-tables (the reserved `class_defs`/`race_defs` + new
`background_defs`/`feat_defs`/`talent_defs`/`track_defs`), the **events** `OnKill`/`OnLevel`/
`OnSkillUse` (the glue), and new **grant ops** (`grant_ability`, `grant_track`, `modify_attribute_base`,
`grant_resource`). Character `state` already carries `templates`/`attributes`/`skills` — the persisted
shape exists. The lifecycle and effect-op interpreter already exist to *run* a grant op-list; what's
missing is the *track machinery* (threshold checking, step grants) and the *grant ops*.

**(e) Verdict: needs-new-mechanism — the largest single area. [G6] The progression-track subsystem:**
- **[G6a]** `track_defs` + an entity's set of tracks (progress attr, thresholds, per-step grant op-list),
  with the engine checking thresholds and firing `OnLevel`/`OnTrackStep`.
- **[G6b]** **Grant ops**: `modify_attribute_base` (the constraint explicitly calls this out — raise a
  stat's *base*, the per-entity override already exists), `grant_ability`, `grant_track`,
  `grant_resource`, `revoke_*`. Additive ops.
- **[G6c]** Template/bundle def-tables (`class_defs`, `race_defs`, `background_defs`, `feat_defs`,
  `talent_defs`) — content bundles of grants + track definitions.
- **[G6d]** `OnKill` / `OnSkillUse` events (XP-on-kill, use-based advancement) — part of [G3]'s bus.
- **[G6e]** Chargen flow (point-buy, choose race/class/background, allocate ASIs/talents) — the
  interactive front end.

**Phase owner: a dedicated chargen+progression phase** (the analysis recommends slotting it after Phase
7, paired with Phase 13 account/chargen; [G6d] events come free with [G3] in Phase 6). This is deferred
today by design; the value of *this* document is to fix the **shape** (N tracks + grant ops + bundles +
events) so the phase is designed right.

> **Binding-constraint check (progression):** the constraint holds and is *well-served* by the N-track
> model — but only if **grants are ops** (so a level-up is just an op-list, reusing the whole effect-op
> machinery and the event bus) and **tracks are content** (so use-based, XP-auto, train, and point-buy
> are all "which event feeds the track" rather than four engine code paths). The risk to watch: do *not*
> let "level" become an engine concept. `level` must stay an ordinary attribute that *some* tracks
> happen to raise; a use-based MUD has tracks with no `level` attribute at all.

---

## 6. Spell slots & per-class casting resources (the progression × resource intersection)

Called out separately because it sits across §3 and §5 and is the sharpest 5e-vs-WoW divergence.

**(a) Cross-system.** 5e: discrete slots per spell level, max derived from class level (the
multiclass-caster table sums fractional caster levels — the genuinely hairy bit). PF: prepared (memorize
specific spells into slots) vs spontaneous (sorcerer: known list, flexible slots); domain/school slots.
WoW: *no slots* — cooldowns + a mana pool; some abilities have charges. Warlock pact magic: few slots,
all max level, refill on short rest.

**(b/d) Translation + mapping.** Slots = N resources whose maxes are derived attributes off the class
track (§5 grants set them); casting `costs:{slot_n:1}`; rest refills them ([G5]). Prepared casting = a
"prepared spells" list in `state` (content; a prepare-spell action moves from known → prepared). The
**multiclass spell-slot table** (fractional caster levels → a shared slot table) is the one piece the
declarative formula stack can't cleanly express — it is a lookup table keyed on a sum of weighted class
levels. WoW cooldowns = the ability cooldown timer (built, modulo persistence §7).

**(e) Verdict:** slots, prepared-casting, pact magic, and WoW cooldowns = **expressible** once [G5]
(rest) + cooldown persistence land. The **5e multiclass slot table = needs-Lua** ([G7]): a Lua
`on_resolve`/derived-attribute hook computing the slot maxes from the multiclass formula — the canonical
"complex 20%" the Lua escape hatch exists for. **Phase 7.**

---

## 7. Cooldowns, lag, the global cooldown & casting time

**(a) Cross-system.** 5e/PF: no real-time cooldowns; "once per rest" is a per-rest pool (§3). WoW: the
core pacing mechanism — per-ability cooldowns (seconds to minutes), a **global cooldown** (GCD) after any
ability, charges. MUD/ROM: **skill lag** (`WAIT_STATE`) after a skill — the round-based GCD analog —
plus per-skill cooldowns. 5e casting time (1 action / bonus action / reaction / minutes) maps to lag /
cast-time.

**(b/d) Translation + mapping.** Ability `lag` (WAIT_STATE) = the GCD; `cooldown` = per-ability cooldown;
`cast_time` = a ritual/long cast. All three are *built fields* on `ability_def` and ride the pulse
scheduler (`scheduleCast`, `armCooldown`). What's incomplete: cooldowns are **transient** (not persisted
across save/load) and there is **no step-3 "still cooling down" gate** (the timer fires-and-logs today).
Reactions (5e reaction, opportunity attacks, counterspell) are a *different* shape — §9/§11.

**(e) Verdict: mostly expressible-today. [G8] Cooldown completion + persistence** — a per-ability
cooldown map, a step-3 "is this on cooldown?" requires-gate, and serialization into `state` (so a logout
doesn't refresh cooldowns). **Phase 6** (combat pacing). The GCD is just a shared-tag lag affect (`apply
a 'gcd' affect that prevents the 'ability' tag for N pulses` — pure content on the existing tag-CC model;
no engine change). Charges = a small resource pool that regens one per cooldown.

---

## 8. Races / origins / lineages & backgrounds

**(a) Cross-system.** 5e: race grants ability bonuses, speed, traits (darkvision, resistances,
proficiencies), sometimes innate spells; 5.2 SRD shifted ability bonuses to **background** + species
traits. PF: race grants ability mods, size, speed, type, racial traits, favored class. WoW: race grants
small stat/skill bonuses + racial abilities (e.g. an escape, a stat buff), faction. Diku: race = stat
modifiers + a few flags (infravision). LP: often raceless. TinyMUD: none.

**(b/d) Translation + mapping.** A race/origin/background is a **content bundle** (§5 [G6c]): a
`race_def`/`background_def` whose grants set attribute *bases* (`modify_attribute_base`), grant abilities
(darkvision = a passive `detect`; an innate spell = a granted ability with a per-rest cost), grant
resistances (a permanent `apply_affect` feeding the damage-type matrix, or a resist attribute), and set
flags (size, type, faction). Applied at chargen. Innate-spell-once-per-day = a granted ability with a
per-rest resource.

**(e) Verdict: needs the bundle + grant ops [G6b]/[G6c] (chargen phase).** Once those exist, races are
*pure content*. Resistances-as-affect and traits-as-passive-ability are already expressible; only the
*grant-at-creation* plumbing is missing.

---

## 9. Combat — initiative, rounds, attack resolution, AC, damage types, crits, multiattack, reactions

The Phase 6 heart. Combat is **Thesis 1 applied repeatedly** over content numbers.

**(a) Cross-system.**
- *Initiative / turn order.* 5e/PF: roll initiative (`d20 + dex_mod`), act in order each round. WoW/MUD:
  no initiative — **simultaneous rounds** on a pulse; everyone in the fight resolves on `PULSE_VIOLENCE`.
  This is the decided model ([COMBAT.md](COMBAT.md) §1): round-based, ROM-derived.
- *Attack resolution.* 5e: `d20 + atk_bonus vs AC` → hit → roll damage. PF: same + iterative attacks at
  −5 each (BAB ≥ 6). WoW MMO-feel: a hit/crit/miss table by attack rating vs defense. ROM (the decided
  model, COMBAT.md §3): a *layered* pipeline — to-hit, then a **dodge/parry/block** avoidance ladder, then
  **soak** by damage type, then apply. 5e's single AC roll is a *degenerate* case of this ladder (fold
  dodge/parry/block into one AC number).
- *AC / avoidance.* 5e: one AC (armor + dex + shield). PF: touch/flat-footed/normal AC, CMD. WoW d20:
  Defense bonus by class/level + armor. ROM: evasion + dodge/parry/block skills + armor soak.
- *Damage types, resist, crit.* All systems: typed damage with resist/vuln/immune; crits (5e: double
  dice on a nat 20; ROM: a crit chance multiplier). The damage-type matrix is **built**.
- *Multiattack.* 5e: extra attacks at higher level; PF: iterative; ROM: second/third/fourth-attack
  skills, dual-wield — `attacks/round` (COMBAT.md §2).
- *Reactions / opportunity / counterspell.* 5e: a reaction per round (opportunity attack on leave,
  Shield, Counterspell, Hellish Rebuke). This is an **interrupt** triggered by an *event* on another
  actor's turn — the hardest combat shape to express.

**(b) Deterministic/content translation.** The whole pipeline is content numbers over an engine pipeline
(COMBAT.md is explicit: the engine runs the *shape*, content supplies to-hit/soak/crit *formulas*). The
attack roll is a **check** ([G2]) `vs` the defender's AC attribute, with bands `{hit, miss}` (or `{crit,
hit, miss}` by margin / nat-20 special). Damage routes through the *built* `dealDamage` mitigation
pipeline (resist matrix + soak + the PvP gate). Initiative is a per-combatant `check` writing an order
(only needed if a system wants strict turn order; the default round model doesn't). The **roll-visibility
default** matters most here — a 5e pack shows "you hit AC 15 with a 22"; a WoW pack hides it behind "Your
Mortal Strike crits for 4,210."

**(c) Room-graph.** Combat is per-room (COMBAT.md §7): a fight is among entities in one room. Movement-in-
combat = fleeing to an adjacent room (a `move` that may provoke an opportunity reaction — §11). Reach /
melee-vs-ranged = "same room" (melee) vs "an adjacent room along an exit with line-of-sight" (ranged) —
**range as exit distance**. Positioning (flanking, cover) has *no* grid; model as abstract intra-room
position tags or simply drop it (most MUDs do). Call out: **5e's 5-foot-step / disengage / flanking
do not survive the room graph** — substitute an abstract "engaged/disengaged" affect and a flanking
*chance* tied to outnumbering, not facing.

**(d) Maps onto our model as.** Phase 6's round driver on `PULSE_VIOLENCE`; the swing pipeline calling
`dealDamage` (built) per the avoidance ladder; the **check primitive** [G2] for to-hit/initiative; the
damage-type matrix (built); `attacks/round` from an attribute; crits as a check band + a damage scale;
multiattack as a loop; conditions as affects (§10). Reactions need a new event-driven interrupt
mechanism.

**(e) Verdict:** the round/swing/avoidance/soak pipeline is **Phase 6 as already designed** (COMBAT.md) —
it builds *on* `dealDamage` and needs [G2] (to-hit is a check). The genuinely new combat mechanisms:
- **[G2]** to-hit/save/initiative as checks (already counted).
- **[G9] Event-driven reactions / interrupts** — an `OnX` event (OnLeaveRoom, OnCast, OnDamaged, OnHit)
  that lets a *third party's* content op-list fire *during* another actor's action and *modify or cancel*
  it (opportunity attack, Counterspell cancels a cast, Shield raises AC after seeing the roll, Hellish
  Rebuke on taking damage). This is the hardest combat shape: it needs (i) the event to fire mid-
  resolution at an interruptible point, (ii) a way for a reaction to *consume a per-round reaction
  resource*, (iii) a way to *alter the in-flight result* (cancel the cast, add to AC before the hit
  resolves). **Phase 6** for the event points + the simple cases (opportunity attack = OnLeaveRoom →
  a granted attack); the *result-altering* cases (Counterspell, Shield) likely **need-Lua** (Phase 7)
  because they reach into an in-flight ability's state. Flag for the combat-engineer: design the swing/
  cast pipeline with **named interruptible checkpoints** that fire events, so reactions are content.
- **[G1]** floor/ceil for to-hit/AC formulas (already counted).

---

## 10. Conditions / status effects

**(a) Cross-system.** 5e conditions: blinded, charmed, deafened, frightened, grappled, incapacitated,
invisible, paralyzed, petrified, poisoned, prone, restrained, stunned, unconscious, exhaustion (6
stacking levels). PF: the same plus dazed, dazzled, entangled, fatigued, nauseated, shaken, sickened,
staggered, etc. (a longer, more granular list). WoW: stun, root, snare, silence, disarm, fear, polymorph,
disorient, bleed/poison/disease DoTs, diminishing returns on repeated CC. MUD/ROM: stun, root, silence,
slow, blind, poison, bleed (COMBAT.md §6). The 5.2 SRD condition redesign maps *cleanly* onto the
already-built tag-CC model.

**(b/d) Translation + mapping.** This is the *best-fit* area in the whole document — it's exactly what
`affect_defs` + the `prevents` tag model were built for (PRINCIPLES.md corollary 3; the srd memory notes
this maps cleanly). Each condition = an `affect_def`: `restrained` prevents `move` + grants
disadvantage-on-attacks (a modifier, or a check-band tweak); `stunned` prevents `move`/`ability`/
`weapon`; `blinded` applies an attack penalty + prevents sight-targeting; `frightened` prevents
approach + a check penalty; `poisoned` = a modifier + a DoT tick; `exhaustion` = a **stacking** affect
(the built `stackCount` mode, max 6) whose magnitude scales penalties; `prone` = a position flag + melee
advantage to attackers. The derived-harm gate already auto-PvP-gates any `prevents`-bearing affect.
WoW **diminishing returns** (each successive same-category CC is shorter, then immune) = a content
pattern: an affect that, on apply, applies a hidden "DR" affect that shortens the next application —
expressible via stacking + duration scaling, or a small Lua `on_apply`.

**(e) Verdict: expressible-today** for the vast majority. Two refinements: **(i)** "save ends at end of
turn" conditions (paralyzed-until-save) need the **check primitive** [G2] fired on the affect tick
(`on_tick` runs a save check, success → expire) — counted under [G2]. **(ii)** Diminishing returns is a
Lua `on_apply` pattern (Phase 7) for exactness, or an approximation today. Otherwise: the single
cleanest mapping in the analysis — a confirmation the tag-CC model was the right call.

---

## 11. The magic / spell system

The deepest content area; broken into casting model, components, concentration, ritual, and the spatial
spells (their own sub-analysis).

### 11.1 Casting model (slots vs points vs cooldowns)
Covered in §3/§6/§7: 5e/PF slots = per-rest resources; WoW = mana + cooldowns; sorcery points / ki =
point pools; spell schools = a content tag on the ability (for dispel-by-school, counterspell, anti-magic
fields). **Verdict: expressible** once rest [G5] + cooldown persistence [G8] land.

### 11.2 Components (verbal / somatic / material) & spell save DC
5e spells need V/S/M; silence prevents verbal, restraint/grapple prevents somatic, material components may
be consumed. **Maps onto:** the **built** tag-CC model — a spell carries tags `{cast, verbal, somatic}`;
a `silence` affect prevents `verbal`; a material cost = a `reagent` cost (the ABILITIES.md cost model has
reagents; the built `costs` is resource-only today — **[G10] reagent/item costs** on abilities, a small
addition, Phase 6/loot). Spell save DC = a derived attribute (`8 + prof + casting_mod`) used as the
`vs` of a save check [G2]. **Verdict: expressible-today** except reagent costs [G10] (small).

### 11.3 Concentration
**(a)** 5e: a caster concentrates on *one* spell at a time; taking damage forces a CON save (DC 10 or
half the damage) or the spell ends; casting another concentration spell ends the first. **(b/d)
Translation:** a `concentration` affect on the caster that (i) is single-instance (`stackIgnore` /
`stack_scope:target`, ends the prior on a new cast), (ii) holds a reference to the concentrated effect so
ending it ends that effect, (iii) on the caster taking damage (`OnDamaged` event) runs a CON save check
[G2]; on failure, expire → which dispels the linked effect. The bookkeeping (linking the affect to a
spawned effect/summon and tearing it down) is the canonical "complex 20%."
**(e) Verdict: needs-Lua [G11].** Concentration is the textbook Lua `on_apply`/`on_tick`/`on_expire`
case (the memory and ABILITIES.md both flag it): an affect whose Lua state holds the concentrated
effect's handle, whose `OnDamaged` hook fires the save, and whose expiry tears down the linked effect.
**Phase 7**, riding the [G3] event bus + [G2] check + [G9] event points.

### 11.4 Ritual casting & long casts
5e ritual = cast a spell over 10 min without a slot. **Maps onto:** an ability variant with a long
`cast_time` and no slot cost (built timing fields). **Verdict: expressible-today.**

### 11.5 Spatial spells (Thesis 2 sub-analysis)
This is the room-graph payoff. Each spell is reframed onto the exit graph and the misfits are called out.

| Spell archetype | Tabletop (continuous space) | Room-graph translation | Maps onto |
|---|---|---|---|
| **Fireball / AoE burst** | 20-ft radius sphere, save for half | "the room" (all valid targets) or "room + adjacent rooms"; **loop the harm-gate + save per target** | **[G12]** AoE/area targeting: a `for_each(targets_in(scope), …)` over `ScopeRoomLiving`, each running `dealDamage` + a per-target save check. The harm-gate already runs *per op*, so looping is safe. The memory flags this as deferred in 5.3. |
| **Cone / line** (burning hands, lightning bolt) | a shaped template | collapses to "the room" (no geometry) or "an exit direction" (line = the room the exit leads to) | same AoE op; cone≈room, line≈`move`-direction target room |
| **Dimension Door / Misty Step / teleport (short)** | move up to N feet to a seen point | move to a **room ref**: a named destination, a marked anchor, or "a random adjacent room" | the **built** `teleport(target, room)` op (reserved in ABILITIES.md §3); needs the op wired (Phase 6) + a room-ref resolver |
| **Teleport (long, named-location)** | arrive at a known location, mishap table | move to a named room ref (a recall point, a beacon); the mishap table = a `check` with bands (on-target / off-target room / mishap damage) | `teleport` + `recall` ops + [G2] for the mishap table |
| **Teleport (precise coordinates)** | arrive at exact coordinates you can see | **genuine misfit** — there are no coordinates, only rooms. Substitute: teleport to the *room* containing a marked anchor, or the nearest room. State the loss explicitly. | content substitute; no precise-coord support |
| **Wall of Fire / Stone / Force, Web, grease** | a persistent shaped zone | a **room affect** (an affect attached to the *room* entity) that ticks damage on occupants or prevents an exit/`move` tag, with a duration | **[G13]** room-scoped affects: attach an affect to a Room entity (rooms are entities; the Affected runtime is generic), ticking over `room.contents`. Also models *anti-magic field*, *silence (the spell)*, *darkness*, *hallowed ground*, *consecrate/desecrate*, *spike growth*. |
| **Summon / conjure / animate** | spawn a creature at a point | `summon`/`spawn(proto, room)` (built op, reserved) into the caster's room; the summon is an ephemeral mob, optionally concentration-linked | the **built** `spawn`/`summon` ops (wire in Phase 6); concentration link via [G11] |
| **Flight / levitate / spider climb** | vertical/3D movement | the room graph is 2.5D via `up`/`down` exits; flight = the ability to use `up` exits or ignore a `fall`/`climb` check on an exit | a flag/affect that bypasses an exit's movement check; **partial** — true free flight over terrain has no analog |
| **Movement (push/pull/grapple positioning)** | shove N feet, grapple to hold in place | push/pull = a `move` to an adjacent room (the **built** `pull`/`push` ops, reserved); grapple = a `restrained`/`grappled` affect preventing `move` (tag-CC) — *not* positional | `push`/`pull` ops + a grapple affect; positioning lost, hold-in-place preserved |
| **Detection / scrying / sight** (detect magic, see invis, scry) | sense within a radius | `scan(room)` / `detect(category)` / `reveal` queries (built, reserved) over the room (or room+adjacent for "60-ft" effects); scry = a read of a remote room's contents | the **built** perception ops |
| **Light / darkness / fog** | a radius of light/obscurement | a room affect (visibility flag) — [G13]; affects targeting/`can_see` | room affect + the visibility filter (built in targeting) |

**(e) Verdict (spatial spells):** mostly **expressible** with two new mechanisms — **[G12] AoE/area
targeting** (loop the per-target harm-gate; Phase 6) and **[G13] room-scoped affects** (attach affects
to room entities; Phase 6/7) — plus wiring the *already-reserved* `teleport`/`spawn`/`summon`/`push`/
`pull`/`scan`/`recall` ops. **Genuine misfits called out:** precise-coordinate teleport (no coords),
free-flight-over-terrain (no continuous space), and grid positioning (flanking/cover/5-ft-step) — each
gets a content substitute (room-ref / exit-flag / abstract-engagement affect), with the fidelity loss
stated rather than papered over.

---

## 12. Equipment — weapons, armor, shields, magic items, attunement, wield

**(a) Cross-system.** 5e: weapons (dice + type + properties: finesse/versatile/reach/thrown/two-handed/
ammunition/light), armor (AC by category + dex cap + str req + stealth disadvantage), shields (+2 AC),
magic items (+N, attunement — max 3 attuned, charges, sentient), proficiency gating. PF: more granular —
enhancement bonuses, specific material (cold iron, silver, adamantine for DR bypass), masterwork, weapon
groups. WoW: item level, stat budgets, gem sockets, set bonuses, durability, BoP/BoE binding, gear score.
ROM/Diku: weapon dice + damroll, armor class by slot, wear locations, `wield`/`wear`/`hold`.

**(b/d) Translation + mapping.** The mudlib already has `Physical`/`Wearable`/`Weapon`/`Armor`
components ([MUDLIB.md](MUDLIB.md) §3) and item prototypes + COW deltas. A weapon = a `Weapon` component
(dice/type/verb); armor = `Armor` (values by damage type, feeding `soak` in the built mitigation
pipeline); shield = a `Wearable` in a shield slot granting a block chance / AC. Weapon *properties*
(finesse = use dex; versatile = bigger die two-handed; reach = "adjacent room" range; thrown = a ranged
attack consuming the item) = flags/tags on the component the combat formulas read. Magic-item `+N` =
instance-delta modifiers (the loot affix system, [LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md) §3). **Charges**
= a per-item resource pool. **Attunement** = a flag + an attuned-count limit (a content rule: a
`requires` gate `attuned_count < 3`) and an `attune` action. Proficiency gating = a `requires` check
against a granted proficiency flag/skill. Material-vs-DR (PF cold iron) = a damage-type/tag the resist
matrix reads. **Set bonuses** = an `OnEquip` event counting set pieces → apply an affect (events-as-glue).

**(e) Verdict:** weapons/armor/shields/wield = **Phase 6** (combat reads the components; the soak hook is
*built*, awaiting the armor component). Magic-item `+N`/charges/affixes = **Phase 11** loot (the delta +
quality system). **[G14] Item-borne modifiers & procs** — gear contributing to the attribute mod stack
(`attributes.go` has the `modSource` seam *built and waiting* — the doc says gear is a stub there) and
weapon/armor on-hit procs (events). Wiring gear into `modSources()` is **Phase 6**; procs ride [G3]/[G9].
Attunement limits + set bonuses + proficiency gating = content over `requires`/events + [G14]. The
attribute mod-stack already anticipating gear is a strong sign this fits.

---

## 13. Economy — money, coins, encumbrance, value

**(a) Cross-system.** 5e/PF: cp/sp/ep/gp/pp (a coin hierarchy), item values, encumbrance by STR. WoW:
copper/silver/gold (100:1), vendor prices, an auction house, BoP/BoE binding, repair costs. ROM/Diku:
gold (sometimes silver), weight-based carry limit, shop buy/sell with markup. TinyMUD: none.

**(b/d) Translation + mapping.** Coins = stackable item prototypes (a `gold` item with a `count` delta)
*or* a `currency` resource pool — content's choice (the persistence shape supports stacks; a resource is
cleaner for a single currency). The coin *hierarchy* = content (a vendor's price formula converts).
Encumbrance = a derived attribute (`carry_weight = str * 15`) vs the summed `Physical.weight` of inventory,
gating with a `check` or a `prevents` affect when over. Shops = a content `Mob` with a shopkeeper flag +
buy/sell abilities (markup is a formula). Binding/BoP = the [CRAFTING.md](CRAFTING.md) binding gate
(Phase 12). Auction house = a cross-shard service (Phase 8/12).

**(e) Verdict: expressible-today** for coins/weight/shops as content (stacks + a derived carry attribute
+ a shopkeeper mob). Encumbrance enforcement wants the **check** [G2] (over-weight → a movement penalty
affect) — small. The deeper economy (binding, auction, repair) is **Phase 12** as designed. No new core
mechanism beyond what loot/crafting already plan.

---

## 14. Monsters — statblocks, multiattack, legendary & lair actions, recharge, regeneration

**(a) Cross-system.** 5e: a statblock = attributes + AC/HP + attacks (multiattack) + traits + actions +
**legendary actions** (act between turns, a per-round budget) + **lair actions** (on initiative count 20,
environment effects) + **recharge** abilities (a breath weapon usable again on a d6 roll of 5–6 each
round) + regeneration + damage resistances/immunities + condition immunities. PF: similar, plus CR, SR
(spell resistance), DR/type. WoW: bosses with phases, enrage timers, adds, mechanics, threat tables, soft
enrage. ROM/Diku: HP/damage/AC + special procs (a `spec_fun`), aggression, wander.

**(b/d) Translation + mapping.** A monster = a `Mob` prototype + the same attribute/resource/affect/
ability content a player uses (the engine doesn't distinguish — a mob casts the same `fireball`). HP/AC =
attributes; attacks = abilities; multiattack = `attacks/round`; traits = passive affects; resistances =
the damage-type matrix + condition immunities = a `prevents`-immunity affect (an affect that grants
immunity to a tag/category). **Recharge** = a per-ability cooldown whose "ready" is a `check` (d6 ≥ 5) on
each round — a check [G2] on the round event. **Regeneration** = a `heal` on a tick affect (built).
**Legendary actions** = an event-driven budget: a per-round "legendary point" resource the boss spends on
extra abilities outside its turn — needs the round event + the action-budget pattern (events + a resource;
[G3]). **Lair actions** = a room-scoped scheduled effect on `PULSE_VIOLENCE` (a room affect [G13] or a
zone-script trigger firing abilities). **Boss phases / enrage** = content scripting (Lua triggers on
HP thresholds → apply an enrage affect / spawn adds) — the [WORLD-EVENTS.md](WORLD-EVENTS.md) / Lua layer.
**Threat/aggro** = COMBAT.md §7's threat list (Phase 6).

**(e) Verdict:** ordinary statblocks, multiattack, resistances, regeneration, recharge = **expressible**
(Phase 6 + [G2] for recharge). Legendary actions = the action-budget pattern over [G3] events + a resource
(**Phase 6**, no new primitive beyond the bus). Lair actions / phases / enrage = **Lua triggers** (Phase
7) + room affects [G13] — the [WORLD-EVENTS.md] orchestration (Phase 10) for region-wide boss ripples.
Threat = Phase 6. No monster-specific engine primitive needed beyond the event bus and [G13].

---

## 15. Presentation — map / overworld / GMCP (driver-emits-data)

**(a/b) The constraint.** Presentation is driver-emits-data; the rich *client* is out of scope; honor the
room graph. Some MUDs want a colorful UTF-8/extended-ASCII overworld map (Dwarf-Fortress style) when
leaving town, client themes, a HUD.

**(d) Maps onto our model as.** Two delivery paths, both already planned:
1. **GMCP structured data** (Phase 9): `Room.Info` (+ room **coords** [deferred, see the room-coordinates
   memory] + sector/terrain), `Char.Vitals/Stats/Status`, `Mud.*` (cooldowns, target, afflictions). The
   combat/affect events already emit the *data* (COMBAT.md §8, ABILITIES.md §8 reserve the GMCP emit
   points); Phase 9 wires the encoder. "GMCP-first, ANSI/text-fallback" so clients theme it.
2. **Server-side UTF-8 + color map** rendered as a view/mode and pushed as output — needs **room coords**,
   a **region/zone map model**, and a **color/ANSI output renderer** (the edge does UTF-8-safe input strip
   today; rich color *output* is flagged-future edge work).

**Room-graph constraint:** any map is a projection of the room/exit graph onto coords; it is *not*
continuous terrain. A "Dwarf-Fortress overworld" is a coords-per-room minimap, not a tile world.

**(e) Verdict:** **Phase 9** (GMCP) + room coords (deferred) deliver the structured-data path with **no
new core mechanism**. The server-side color map is **edge/future work** (a renderer + a zone map model) —
**[G15] ANSI/UTF-8 color output renderer + zone map model**, low priority, post-Phase-9 edge. The graphical
client app itself stays out of scope.

---

## 16. Basic-MUD support (the acceptance question)

The constraint is explicit: *can it still be a plain Diku / LP / Tiny MUD?* Confirming this is as
important as chasing 5e.

- **TinyMUD / MUSH (contentless social/building).** Bare engine + OLC. **Confirmed natural today.** The
  bare-engine invariant (`empty_world_test.go`) boots with zero attributes/resources/abilities; an entity
  with no content has no pools, no stats, `attr()`/`resourceCurrent()` return 0/absent sanely. Building
  (rooms/exits/objects) is the mudlib core (Phase 3) + content pipeline (Phase 4). Socials are act()
  templates (built). Channels are Phase 8. **The only gap is in-game OLC** (a building command set writing
  content rows) — a tooling feature, not an engine primitive; **[G16] in-game OLC** (a content phase /
  Phase 4 follow-on). Nothing in the rich-tabletop work threatens this — it's the *floor*, and the floor
  exists.
- **DikuMUD / Merc / ROM (fixed pools, levels, ticks).** **Confirmed natural / nearly fully built.** HP/
  mana/move = three `vital`-ish resources with `regen` on the pulse tick (built — `runRegen` *is* the
  Diku tick). Level = an attribute; XP-auto-level = a track [G6] fed by `OnKill` [G3]. Stat apply tables =
  derived attributes. Combat = Phase 6 (ROM is the *decided* model — COMBAT.md is literally "ROM,
  refined"). Skills-as-commands with lag = built (`ability_def.lag`). This is the **easy subset** the
  engine was shaped around; the only deferred pieces are the track [G6] and the combat pipeline (Phase 6),
  both already planned. A plain ROM is the most *directly* supported target.
- **LPMud / Discworld (skill-use, soft-coded objects).** **Mostly natural; one defining mechanism is the
  use-based track.** Skills = attributes; advance-through-use = `OnSkillUse → chance(p,
  modify_attribute(skill,+1))` ([G6] track in use-based mode + [G3] event). No levels = a track with no
  `level` attribute (the N-track model explicitly admits this). Soft-coded objects = **Lua** (Phase 7) —
  the engine's `Scripted` component + the curated Lua API is exactly the LPMud "every object has code"
  model. GP-for-spells = a resource. Guild-join = a content action adding a track. **Confirmed natural
  once [G6] (use-based mode) + [G3] (OnSkillUse) + Phase 7 land** — all already planned. The risk to
  watch (§5 note): if "level" leaks into the engine, the no-levels LP case breaks; the N-track model
  prevents this *by design*.

**Verdict:** all three heritage baselines remain first-class. TinyMUD works *today* (modulo OLC tooling
[G16]); Diku is the most-directly-supported target (Phase 6 + [G6] finish it); LP needs the use-based
track mode + OnSkillUse + Lua, all planned. **No part of chasing 5e/PF/WoW has cost the simple
baselines** — the same primitives (generic resources, N tracks, events, affects) serve all four tiers,
which is the strongest evidence the abstraction is right.

---

## 17. Consolidated gaps → roadmap

Every new mechanism the analysis surfaced, the systems that need it, and the phase that should own it.
"Expressible-today" areas are omitted (attributes, conditions, basic equipment-as-components, coins,
ordinary statblocks, ritual casts, the Diku tick, TinyMUD).

| # | New mechanism | Why / who needs it | Verdict | Phase owner |
|---|---|---|---|---|
| **G1** | Formula vocabulary: `floor`/`ceil`/`round`/`mod`/conditional heads | 5e modifiers, PF BAB/saves, WoW ratings — exact integer derivation | new (small) | **6** (or chargen) |
| **G2** | **The check/save/contested primitive** + outcome **bands** + dice notation (kh/kl/dF/pool) + visibility config + `OnCheck` | *everything*: climb checks, saves, attacks, initiative, contested, PbtA/BRP/Blades breadth, save-ends conditions | **new — the central gap** | **6** |
| **G3** | **The event bus** firing `OnHit/OnDamageTaken/OnKill/OnAbilityResolved/...` to content op-lists | rage/runic/combo builders, XP-on-kill, use-based skills, procs, set bonuses, legendary/recharge, concentration | new — the universal glue | **6** (origin) + **7** (Lua handlers) |
| **G4** | Conditional / formula resource regen | rage decay out of combat, WoW fast OOC regen | new (small) | **6** |
| **G5** | `rest`/`recover` event + resource-refill; resource→scaling read | 5e/PF slots & per-rest pools, short/long rest, combo finishers | new (small) | **6** / chargen |
| **G6** | **Progression-track subsystem**: track_defs + thresholds + grant ops (`modify_attribute_base`, `grant_ability/track/resource`) + bundle defs (class/race/background/feat/talent) + chargen | all leveling/multiclass/use-based/point-buy/train; races/feats/talents | **new — the largest area** | **dedicated chargen+progression phase** (+ [G3] events from 6) |
| **G7** | Multiclass spell-slot table (fractional caster levels) | 5e multiclassing | needs-Lua | **7** |
| **G8** | Cooldown completion: per-ability map + step-3 gate + persistence; charges | WoW cooldowns/charges, MUD per-skill cooldowns | new (small) | **6** |
| **G9** | **Event-driven reactions/interrupts** + interruptible-checkpoint events; result-altering | opportunity attacks, Counterspell, Shield, Hellish Rebuke | new; simple cases Phase 6, result-altering needs-Lua | **6** (points) / **7** (alter) |
| **G10** | Reagent / item costs on abilities | 5e material components, consumables | new (small) | **6** / **11** |
| **G11** | **Concentration** (linked-effect affect + OnDamaged save + teardown) | 5e/PF concentration spells | needs-Lua | **7** (on G2/G3/G9) |
| **G12** | **AoE / area targeting** (loop the per-target harm-gate + per-target save) | fireball, cone, line, any multi-target | new | **6** |
| **G13** | **Room-scoped affects** (attach affects to room entities; tick over occupants) | wall spells, web, darkness, silence-spell, anti-magic, lair actions, consecrate | new | **6**/**7** |
| **G14** | Item-borne modifiers into the attr mod-stack + weapon/armor procs (the `modSource` seam is built-and-waiting) | magic items `+N`, set bonuses, on-hit procs | new (seam exists) | **6** (mods) + **11** (affixes) |
| **G15** | Server-side ANSI/UTF-8 color output renderer + zone map model | overworld map, themed output | new (low priority) | **edge / post-9** |
| **G16** | In-game OLC (building command set writing content rows) | TinyMUD/MUSH building, all builder workflows | tooling | **content phase / Phase 4 follow-on** |

**The wire-up-the-reserved-ops list (no new primitive, just connect what's documented/reserved):**
`teleport`, `spawn`, `summon`, `push`/`pull`, `recall`, `scan`/`detect`/`reveal`/`identify`, the GMCP emit
points, the gear `modSource`, and the `attacks/round`/`soak`/threat hooks (COMBAT.md) — all reserved in
ABILITIES.md §3 / the code and slot into Phase 6/9.

**Top 8 new mechanisms (the headline):** [G2] the check primitive (Phase 6) · [G3] the event bus (Phase
6/7) · [G6] the progression-track subsystem (a chargen+progression phase) · [G12] AoE targeting (Phase 6)
· [G13] room-scoped affects (Phase 6/7) · [G9] event-driven reactions (Phase 6/7) · [G11] concentration
(Phase 7) · [G8] cooldown completion+persistence (Phase 6).

---

## 18. Open design questions for the user

**ALL RESOLVED 2026-06-26** — the user settled every fork before Phase 6; these are now
binding design inputs for Phases 6/7 and the chargen+progression phase:

1. **Roll visibility:** HIDDEN by default; opt-in `show`; overridable per pack/ability/check.
2. **Check primitive home:** lives in the EFFECT-OP INTERPRETER (invokable from exits, objects,
   affect-ticks, AND abilities), built as a near-term Phase-6 PREFIX — so non-combat checks
   (climb a wall) don't wait for the full combat pipeline.
3. **Spatial fidelity:** ACCEPT the room-graph's losses (no tactical grid / precise-coord
   teleport / flight positioning); use room-ref + abstract-engagement substitutes; build NO
   intra-room coordinate system.
4. **Outcome-band arity:** FULL ordered-band generality from day one (binary 5e = the 2-band
   case); the engine stays ignorant of BOTH dice shape and outcome arity.
5. **Progression:** N independent advancement TRACKS; `level` is an ordinary attribute (no
   engine specialness); all four advancement modes (XP / train / point-buy / use-based) are
   content; the dedicated chargen+progression phase lands AFTER Phase 7 (so it has the event
   bus + Lua).
6. **Reactions (v1):** the Phase-6 swing/cast pipeline exposes named interruptible CHECKPOINTS
   firing events; easy reactions (opportunity attacks) are declarative content; result-altering
   ones (Counterspell/Shield) are Lua.

The original analysis of each fork follows.

1. **How deterministic do we surface the dice? ([G2] visibility default.)** Per-system default + per-
   action override is the recommendation — but what's the *engine default*? A 5e pack wants the math shown
   ("22 vs AC 15 — hit"); a WoW/IRE-style pack wants it hidden behind flavor. Recommendation: hidden
   default, opt-in show, both overridable. **User call:** confirm the default and the granularity (per
   pack? per ability? per check?).

2. **Where does the check primitive live — combat phase or ability lifecycle? ([G2] home.)** Attack rolls
   and saves are checks *and* combat is built on them. Recommendation: the `check` flow op lives in the
   **effect-op interpreter** (so an exit/object/affect-tick/ability can all invoke it), and Phase 6
   *consumes* it for to-hit — i.e. build the primitive *slightly ahead of* the combat pipeline, not inside
   it. **User call:** accept that ordering (check primitive is a near-term Phase-6-prefix), or defer checks
   into combat and accept that non-combat checks (climb a wall) wait for Phase 6 too.

3. **How much spatial fidelity to chase? (Thesis 2 boundary.)** The room graph cleanly handles AoE-as-room,
   teleport-as-room-ref, range-as-exit-distance, and wall-spells-as-room-affects. It *loses* precise-coord
   teleport, free flight, and grid positioning (flanking/cover/5-ft-step). Recommendation: accept the loss,
   use the abstract-engagement / room-ref substitutes, *don't* build an intra-room coordinate system.
   **User call:** confirm we're not chasing tactical-grid fidelity (a large, system-specific effort that
   would strain the room-graph constraint).

4. **The outcome-band arity of [G2].** Binary (5e save) vs three-tier (PbtA) vs degrees (BRP) vs shifts
   (FATE) vs pools (Blades). Recommendation: an ordered **band list** (binary is the 2-band case) so the
   union abstraction holds across the whole catalog. **User call:** confirm we design for the full band
   generality now (cheap at design time, expensive to retrofit) vs. binary-now-extend-later.

5. **When does the progression/chargen phase land, and is "level" allowed to be special?** Recommendation:
   a dedicated phase after Phase 7 (so it has the event bus + Lua), N tracks, `level` strictly an ordinary
   attribute. **User call:** confirm the N-track-no-special-level shape (the LP/Discworld case depends on
   it) and the phase placement.

6. **Reactions: how far into result-altering do we go? ([G9].)** Opportunity attacks (a granted attack on
   OnLeaveRoom) are easy. Counterspell/Shield (cancel/alter an *in-flight* ability after seeing a roll) are
   hard and intrude on the combat pipeline's structure. Recommendation: design Phase 6's swing/cast pipeline
   with **named interruptible checkpoints** that fire events, support the easy cases declaratively, and put
   result-altering in Lua. **User call:** confirm we accept Lua-only result-altering reactions for v1, or
   want declarative Counterspell (more pipeline complexity).

---

## 19. Where the binding constraints are strained (and by which target)

Honest accounting of where a target system pushes a constraint to its edge:

- **Generic resources — strained by WoW's resource zoo, *held* by the event bus.** Rage (build + decay),
  combo points (bounded builder + finisher), runic power, energy — none is expressible as "a pool with a
  regen rate." They are expressible as "a pool + event-driven dynamics," which is on-constraint *only
  because* [G3] the event bus exists. The constraint holds, but its viability is **contingent on [G3]**;
  without the bus, the resource constraint is not actually satisfiable for the WoW capstone. This is the
  single most important downstream dependency in the document.
- **N-track progression — strained by 5e multiclass slot math and PF prestige prerequisites; *held* by
  letting grants be ops and one Lua escape ([G7]).** The track model is generous, but the 5e multiclass
  *spell-slot table* is a genuine lookup that doesn't fit declarative formulas — it is the one place the
  progression constraint needs the Lua hatch. That's acceptable (it's the documented 20%) but worth naming.
  The deeper risk is *cultural*: keeping `level` a mere attribute under pressure from three systems that
  all center "level" — the LP/Discworld no-levels case is the canary that keeps us honest.
- **Events-as-glue — not strained, but *load-bearing far beyond its current built state*.** The bus is
  referenced as Phase 6/7 and currently fires nothing to content. This analysis shows it underpins
  resources ([G3]), progression ([G6d]), reactions ([G9]), concentration ([G11]), procs ([G14]), and
  monster legendary/recharge — i.e. it is the highest-leverage unbuilt mechanism. Its generality
  (content-subscribable, scoped, ordered for durable cases per WORLD-EVENTS.md) must be designed deliberately,
  not bolted on.
- **Full-spectrum — not strained; *confirmed* (§16).** The same primitives serve TinyMUD through WoW. The
  only spectrum risk is the engine accreting a rich-tabletop assumption (a hardcoded "level", "hp", "d20",
  or "success/failure") that breaks the simple tiers — which the band-generality ([G2]) and N-track
  ([G6]) and `vital`-flag (built) decisions specifically guard against.

---

## 20. Validation requests (domain-expert agents)

Things this analysis asserts about engine reality or proposes as mechanism that the owning engineers
should confirm before the phases are designed:

- **combat-engineer:** confirm the Phase 6 swing/cast pipeline can host (i) [G2] checks as the to-hit/save
  resolution, (ii) [G9] named interruptible checkpoints firing events mid-resolution, (iii) [G12] AoE as a
  per-target loop over the *built* `dealDamage` gate, and (iv) `attacks/round` + the avoidance ladder over
  content formulas. Validate that the built `soak()`/`modSource` seams are where gear ([G14]) and armor land.
- **abilities-engineer:** confirm [G2] (a `check` flow op alongside `if`/`chance`), [G10] (reagent costs),
  [G13] (attaching affects to room entities via the generic Affected runtime), and the resource→scaling read
  all fit additively in the effect-op interpreter without a lifecycle change.
- **progression-engineer:** validate the [G6] N-track shape (track_defs + grant ops + bundle defs + the
  four advancement modes as "which event feeds the track") and that `modify_attribute_base` over the
  existing per-entity base override is the right grant primitive; confirm `level`-as-attribute holds.
- **scripting-engineer:** confirm [G7] (multiclass slot math), [G11] (concentration teardown), [G9]
  result-altering reactions, and monster lair/phase scripting are the right Lua-hatch boundary, and that the
  curated handle API + `self.state` + the event subscription surface support them.
- **persistence-engineer:** confirm [G8] (cooldown serialization into `state`), the track/grant state shape
  in `state` JSONB, and that the new def-tables ([G6c] class/race/background/feat/talent, check_defs,
  loot/affix) follow the per-kind-table + JSONB-tail + `pack` pattern with no schema strain.
- **mudlib-engineer:** confirm [G16] in-game OLC scope and that the TinyMUD/Diku/LP baselines (§16) hold
  against the mudlib core as built (containment, command table, act(), the `Scripted` component for LP).
