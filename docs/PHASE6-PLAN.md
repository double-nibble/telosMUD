# Phase 6 — Combat (+ the check primitive, the event bus, AoE & room affects) — IMPLEMENTATION PLAN

Status: **proposal / planning** — the six gap-analysis forks are already settled (see
[GAME-SYSTEMS-GAP-ANALYSIS.md](GAME-SYSTEMS-GAP-ANALYSIS.md) §18); the §1 decisions below are
the *combat-specific consequences* + a few new ones, to confirm before slice 6.1.

Combat is the round-based, ROM-derived, layered-avoidance + soak model ([COMBAT.md](COMBAT.md)) —
but the gap analysis ([GAME-SYSTEMS-GAP-ANALYSIS.md](GAME-SYSTEMS-GAP-ANALYSIS.md) §2, §9, §17)
reframes Phase 6 as **the phase that builds the primitives combat is assembled *from*** before it
builds the fight loop. **Done when** (ROADMAP Phase 6): you fight a mob through the full pipeline
(to-hit check → miss/dodge/parry/block → soak), a fireball's save halves its damage across everyone
in the room, a rage bar builds on hit via an `OnHit` handler, and you kill the mob and loot its
corpse — **all from content, no engine changes**.

This phase builds the **check primitive [G2]**, the **in-zone event bus [G3]**, **AoE [G12]**,
**room-scoped affects [G13]**, **cooldown completion [G8]**, and **reaction checkpoints [G9]** on
top of the Phase 5 substrate (the shared mitigation pipeline, `dealDamage`, resources/affects, the
pulse scheduler, the PvP gate). It does **not** build: Lua handlers / result-altering reactions /
concentration (Phase 7), the cross-zone scoped + durable event bus (Phase 10), GMCP combat deltas
(Phase 9), or progression/chargen (Phase 11).

---

## 0. Where Phase 6 sits on the existing code

| Existing (Phase 3–5) | Phase 6 change |
|---|---|
| `effect_op.go` flow ops `if`/`chance` recurse into `runOps`; each op is a registered handler | The **`check` flow op [G2]** is the next flow op — same shape (resolve → pick a branch → recurse into `runOps`), but classifies a rolled total into an **ordered band list** instead of a bool. |
| `effect_op_handlers.go rollDice(c, num, size)` + `effectCtx.rng` (seeded per zone) | Extended to parse **content dice notation** beyond `NdS`: keep-high/low (`2d20kh1`), Fudge (`4dF`), pool-count-successes (`5d6>4`). The RNG seam is unchanged. |
| `formula.go` heads `+ - * / min max clamp lit attr` (prefix AST) | Add `floor`/`ceil`/`round`/`mod` + conditional heads [G1] (combat needs exact integer to-hit/soak); add `$actor`/`$target`/`$source` **scoped refs** for check `bonus`/`vs` [G2c]. |
| `ability.go` step 10 fires the **reserved** `OnAbilityResolved`/`OnHit` events as *log-only* (Phase 6/7 reserved) | Those reservations become a **real in-zone event dispatch [G3]** — synchronous, single-writer, content-subscribable. |
| `Affected` runtime (`affected.go`/`affect_runtime.go`) ticks per-entity on the pulse, resolve-by-id contract | Gains **room-scoped affects [G13]** (an affect attached to the *room* entity, ticking over occupants) and is the substrate AoE/DoT route through. |
| `pulse.go` per-zone scheduler (`every`/`after`, resolve-by-id-or-cancel) | Hosts **`PULSE_VIOLENCE`** (a fixed multiple of base pulse) — the round driver; plus cooldown timers [G8]. The `pulseFunc` contract is binding for the round loop. |
| `pvp.go guardHarmful` + `affectIsDetrimental` (the single harm funnel, Phase 5) | **Every new harm vector** — swing damage, AoE per-target, event-handler ops, reaction ops — routes through the *same* `guardHarmful`. No second gate. **security-auditor boundary.** |
| `Living`/resources (Phase 5): vitals are content resources, `vital` flag, `on_depleted` op-list | Combat reads the **`vital` resource by flag**, never a hardcoded `hp`; death = `on_depleted` firing (content) → the engine's corpse/threat machinery. |
| `commands.go` command table; abilities register commands (Phase 5) | Combat skills are abilities-as-commands with `lag` (WAIT_STATE, exists) + the new **cooldown gate [G8]**; `kill`/`flee`/`assist`/`consider` are engine commands driving the `Fighting` state. |
| Cross-shard handoff (Phase 2/4): fat snapshot, epoch CAS, freeze-during-transfer | Combat/threat is **transient** (dropped on quit/handoff); cooldowns + active affects **persist**. AoE "room + adjacent" must respect that adjacent rooms may live on **another shard** (message-pass, never reach across — see §5). |

The riskiest *structural* points: (a) the **event bus re-entrancy** (an `OnHit` handler that deals
damage that fires `OnDamageTaken` …) needs a depth/loop guard and a clear single-writer story; and
(b) the **Phase-6-vs-Phase-10 bus boundary** — Phase 6 is the *in-zone, synchronous, transient*
dispatch; Phase 10 ([WORLD-EVENTS.md](WORLD-EVENTS.md)) adds *cross-zone scopes + durable JetStream*.
See §1.2 and §5.

---

## 1. Tech / design decisions (confirm before slice 6.1)

Settled-by-gap-analysis decisions are marked **(settled)** — restated here as the binding input;
the rest are new combat-specific calls.

| # | Decision | Recommendation | Trade-off |
|---|----------|----------------|-----------|
| **P6-D1** | **Check primitive home & arity** (settled — gap §18.2, §18.4) | A `check` **flow op** in the effect-op interpreter (invokable from exits/objects/affect-ticks/abilities), classifying a content dice roll into an **ordered band list** (binary = the 2-band case). Engine stays ignorant of dice *shape* AND outcome *arity*. | Builds the most load-bearing abstraction first, standalone-testable (a climb check needn't wait for the fight loop). Cost: a dice-notation parser + band classifier + dual-scope formula context up front. Already the chosen ordering. |
| **P6-D2** | **Band classifier model** (new) | Bands are an **ordered list of `(test → label, ops)`** where `test` is a **threshold** (`≥N` against the rolled total, or `vs` a target's roll for contested) OR a **margin/degree** (`total − dc ≥ k`); first matching band (top-down) wins; the last band is the default. Supports binary `{success,failure}`, half-on-save `{success→half, failure→full}`, PbtA `{≥10, 7..9, ≤6}`, BRP degrees, Blades pools (count successes → band). | The union abstraction across the whole catalog (gap §2 design note). Cost: the test grammar must express both threshold and margin without becoming a mini-language — keep it to `{min, max, margin_min}` per band. Alt: hardcode bands per dice kind — rejected (breaks the pillar). |
| **P6-D3** | **In-zone event bus scope** (new — the keystone) | Phase 6 ships a **synchronous, single-writer, in-zone** dispatch: content subscribes op-lists to event *kinds* (`OnHit`/`OnDamageTaken`/`OnKill`/`OnAbilityResolved`/`OnCheck`/`OnLeaveRoom`/`OnRest`) via `on_<event>` fields on `resource_def`/`affect_def`/`ability_def`/items; handlers run **inline** on the zone goroutine at the fire point, **gated by a recursion-depth budget**. **No NATS, no durability, no cross-zone** — that is Phase 10 ([WORLD-EVENTS.md](WORLD-EVENTS.md)). | This is binding-constraint #3 (events-as-glue) and the highest-leverage unbuilt thing (gap §19). Synchronous-in-zone matches the actor model and gives content a consistent single-threaded view. Cost: re-entrancy/ordering discipline + the depth guard (§5). **distributed-systems-architect must review** the boundary with the durable bus. |
| **P6-D4** | **Reaction interrupt depth (v1)** (settled — gap §18.6) | The swing/cast pipeline exposes **named interruptible checkpoints** (`before_swing`, `on_to_hit`, `before_damage`, `on_damaged`, `on_leave_room`, `before_cast_commit`) that fire events. v1 reactions may **fire a granted op-list + consume a per-round reaction resource**; they may **NOT alter the in-flight result** (cancel a cast, add to AC after the roll). Result-altering = Lua (Phase 7). | Opportunity attack (a granted attack on `OnLeaveRoom`) and Hellish-Rebuke-style (an op-list on `OnDamaged`) work declaratively now; Counterspell/Shield wait for the Lua hatch. Cost: the checkpoint set must be designed into the pipeline *now* even though the alter-path lands later. |
| **P6-D5** | **Roll visibility** (settled — gap §18.1) | **Hidden by default**, opt-in `show` (and `summary`), overridable **per pack / per ability / per check**. Phase 6 emits via `act()`/`send` text (`"You scramble up the wall."` vs `"You rolled 14+6 vs 15 — success."`); the **GMCP** structured emit is a reserved hook (Phase 9). | A 5e pack shows the math; a WoW/IRE pack hides it. Cost: a visibility resolution order (check → ability → pack → engine default) + two render paths. |
| **P6-D6** | **Combat numbers are content attributes, not a ruleset table** (new) | The swing pipeline is **engine shape**; the *numbers* (accuracy, evasion, dodge/parry/block, soak-by-type, `attacks`, crit chance/mult, to-hit/soak curves) are **content-defined derived attributes** (Phase 5 substrate) the pipeline reads + feeds to `check`. **No `combat_ruleset` table** — the attribute_defs *are* the tunable ruleset; a pack swaps the curve by redefining the formula. | Keeps "ROM, refined" tunable without recompile (COMBAT §9) and avoids a parallel config system. Cost: a handful of *conventionally-named* attributes a combat pack must define (`accuracy`, `evasion`, `soak_<type>`, `attacks`) — conventional, not engine-hardcoded; a contentless world simply has no combat. |
| **P6-D7** | **Avoidance ladder = a sequence of checks** (new) | To-hit is `check` #1 (`accuracy` vs `evasion`/AC → bands `{crit, hit, miss}` by margin/nat-max). A *would-be hit* then runs **independent** dodge → parry → block checks (each gated by content requirements: parry needs a wielded weapon, block needs a shield); first success negates the swing. 5e's single-AC model is the degenerate case (no dodge/parry/block defs → straight to soak). | COMBAT §3's layered ladder falls straight out of [G2]; each stage emits its own message + (Phase 9) its own GMCP event. Cost: 1–4 checks per swing — bounded, on the zone goroutine; the RNG is cheap. |
| **P6-D8** | **What persists vs is transient** (new) | **Persist:** per-ability **cooldowns** [G8] (into `state` JSONB), active affects + resource currents (Phase 5, unchanged). **Transient:** the `Fighting` state, threat lists, the per-round reaction budget, room-scoped affects (re-applied by content/reset). A logout/handoff drops combat but not cooldowns or buffs. | Matches player expectation (you don't resume a fight after a crash) and keeps the snapshot small. **persistence-engineer must confirm** the cooldown shape + that combat-transient state is correctly excluded from the fat snapshot. |

### 1.1 The check primitive (P6-D1/D2) — the prefix

A **check spec** (inline JSONB on an op, or a named `check_def` for reuse) carries (gap §2b):

```
{
  dice:       "1d20" | "2d20kh1" | "1d100" | "2d6" | "4dF" | "5d6>4",   // content notation
  bonus:      <formula over $actor attrs>,        // e.g. ["+", ["attr","$actor.dex_mod"], ["attr","$actor.athletics"]]
  vs:         <formula DC>  |  {contested: <defender check spec>},      // literal/formula or opposed roll
  bands:      [ {min: 10,           label:"strong", ops:[...]},          // ordered, first match wins
                {min: 7, max: 9,    label:"weak",   ops:[...]},
                {            label:"miss",   ops:[...]} ],                // default (no test) = last
  visibility: "hide" | "show" | "summary"                               // optional; resolves up the chain
}
```

The engine: rolls `dice` (extended notation, `rollDice` + new parsers), evaluates `bonus`/`vs` in a
**dual-scope formula context** (`$actor`/`$target`/`$source` — P6-D1/§1.4), computes the total (or
the contested comparison / pool success count), classifies into the first matching `bands` entry,
fires **`OnCheck`** (so content can react — a reroll, a lucky-halfling, bardic inspiration), runs
that band's `ops` via `runOps`, and emits per the visibility config. The `check` op is **structurally
identical to `if`/`chance`** (a flow op that recurses into `runOps`), so it adds no lifecycle change.

This makes the canonical examples one shape:
- **Climb a wall** — a room exit / object carries `check = {dice:"1d20", bonus:"$actor.dex_mod + $actor.athletics", vs:15, bands:{success→[move ...], failure→[deal_damage fall ...]}}`.
- **Saving throw** — invoked from a spell's op-list: `{dice:"1d20", bonus:"$target.dex_save", vs:"$source.spell_dc", bands:{success→half, failure→full}}`.
- **Attack roll** — `{dice:"1d20", bonus:"$actor.accuracy", vs:"$target.ac", bands:{faceEq:20→crit, faceEq:1→miss, marginMin:0→hit, default→miss}}`.
- **Save-ends condition** — the *same* spec fired on an affect tick (Phase 5 hook).

**Band-test axes (built in 6.1, after the rpg-systems acceptance review).** A band tests any of: the
**total** (`min`/`max`), the **margin** over the DC (`marginMin`/`marginMax`), or the **natural faces**
(`faceEq` + `faceCount` — "≥ N dice showing exactly V"). Every numeric edge is a **formula** scoped
like `bonus`/`vs`, so an edge can be a *derived* value (WoW crit/miss boundaries; BRP roll-under crit
= `max: ["/", skill, 20]`). This is what makes the union abstraction actually cover the catalog: 5e
nat-20 auto-crit / nat-1 auto-miss (face tests, ordered before the margin band), d100/BRP roll-under
+ degrees (formula `max` edges), Fate shift windows + contested ties (`marginMin`+`marginMax`), Blades
6-6 (`faceEq:6, faceCount:2`).

**Deferred check refinements (from the 6.1 review — not schema-breaking, so safe to land later):**
- *Outcome magnitude bindable into band ops* — exposing `margin`/`total`/success-count as a
  `$check.*` ref a band's ops can read (Fate "boost = shifts", BRP "damage scales with degree"). Lands
  with the **event-bus value-binding** work in **slice 6.2**, not as a one-off.
- *Blades' highest-single-die reading* — a `dicePoolHigh` kind banding on the single highest face
  (1-3/4-5/6 position). Count-pools (`Nd S>T`) already serve Year Zero / generic; the Blades
  position/effect read is a later dice-kind addition (or Lua).

**Builder-guide authoring notes to carry into the eventual docs (from the 6.1 review):**
- Teach **roll-under** (d100/BRP, Cairn) as `max`-on-**total** (the bare die as `total`, no `vs.dc`,
  ceiling bands), *not* a DC + margin — BRP's degree sub-thresholds (`dc/5`, `dc/20`) are formula
  `max` edges, so the `max`-on-total idiom is both correct and the only one that composes.
- `faceEq` counts **all rolled faces**, not the kept die — so an advantage (`2d20kh1`) nat-20 crit is
  right (either die), but pairing a `faceEq:1` auto-miss band *with* advantage is a known wrong-ish
  edge (documented in `check.go`); scope that guidance.
- Scope the Blades coverage claim to **crit-only** (the 6-6 `faceEq` band) until `dicePoolHigh` lands;
  the highest-die *partial* (4-5) tier isn't expressible by count-pools.

### 1.2 The in-zone event bus (P6-D3) — the keystone

The Phase 5 reservation (`ability.go` step 10 fires `OnAbilityResolved`/`OnHit` as log-only)
becomes a real dispatch. The model:

- **Subscription is content, collected per-entity.** A `resource_def` carries
  `on_event: {OnHit: [modify_resource $actor rage +5]}`; an `affect_def`, `ability_def`, or item may
  too. When an entity is built/loaded, the engine **collects its active subscriptions** (from its
  resources + active affects + known abilities + equipped items) into a per-entity handler map keyed
  by event kind — so a fire is an O(handlers) lookup, **not** a global scan.
- **Fire points** are engine-named and synchronous: `OnAbilityResolved`/`OnCheck` (abilities/checks),
  `OnHit`/`OnDamageTaken`/`OnKill` (the swing pipeline), `OnLeaveRoom` (movement), `OnRest` (the rest
  command, [G5]), `OnAffectApply/Tick/Expire` (Phase 5, now dispatched not just logged).
- **Single-writer, inline, depth-guarded.** Handlers run on the zone goroutine at the fire point.
  An `effectCtx` carries a **remaining-depth budget**; a handler that fires another event decrements
  it; at 0 further fires are dropped + logged (kills the `OnHit`→damage→`OnDamageTaken`→damage… loop).
  Ordering within a kind is stable (subscription order).
- **Harm still gates.** A handler op-list that deals damage / applies a detrimental affect routes the
  *same* `guardHarmful` (P5-D4) — an event handler is **not** a PvP-gate bypass (§5, security).
- **Boundary with Phase 10.** This bus is *zone-local, transient, synchronous*. The **scoped +
  durable (JetStream, ordered, idempotent) cross-zone bus** ([WORLD-EVENTS.md](WORLD-EVENTS.md)) is
  Phase 10. Phase 6 fires only entity/room-scoped events handled *within the firing zone*; a handler
  that needs a cross-zone consequence enqueues it for the (Phase-10) director — reserved, no-op now.

This single mechanism delivers [G3] **and** unblocks the WoW resource zoo (rage/runic/combo builders
are `on_event → modify_resource`), conditional regen [G4] (`on_event OnEnterCombat/OnLeaveCombat`),
the rest event [G5], and the declarative reactions [G9].

**Built in 6.2 + carried from its reviews:** the dispatch is gather-at-fire-time (no cached map, no
invalidation surface); subscriptions come from the resources an entity HAS + its active affects
(ability/item subscriptions await the Skilled/equipment components). Two guards bound the cascade — a
**depth** cap (re-entrancy) AND a shared **width** budget (`maxEventHandlers`, total handler runs per
root action) so a wide non-recursive fan-out can't starve the heartbeat. The harm gate is structural:
`guardHarmful` fails **closed** on a detached actor/target (the `fireOnTick` lesson, now covering every
fire point), and the three non-damage cross-player writes funnel one `guardCrossPlayerWrite`.
*Deferred (non-blocking):* the per-entity subscription **index** (the plan's "built at load/affect-
apply time" optimization — recompute-at-fire is correct and cheap at current scale; revisit if 6.3's
per-swing fire points profile hot); and a **Phase-7 threat-model note** — a Lua handler receives a live
`other` pointer with attr/has-affect read access, so the sandbox must decide whether Lua may read a
counterpart's hidden state or must see a capability-narrowed view.

### 1.3 The combat round (COMBAT.md, on the substrate)

- **Round driver:** `PULSE_VIOLENCE` = a fixed multiple of the base zone pulse (≈2.4s/round,
  tunable), registered like any pulse callback (resolve-by-id contract). Every entity in the
  `Fighting` state resolves its swings for the round on that pulse — per-zone, no global lockstep.
- **Swings/round:** `attacks` (a content attribute — weapon speed, haste affect, ROM second/third-
  attack, dual-wield). A round loops `attacks` swings; each runs the pipeline.
- **Swing pipeline (P6-D7):** gates (position/visibility/safe-room/immunity) → **to-hit check** →
  **avoidance ladder** (dodge/parry/block checks) → **damage roll** (weapon dice + `damroll` + crit
  band scale) → **soak/mitigation** (the *built* Phase 5 `dealDamage`: resist/vuln/immune matrix +
  soak-by-type) → **apply** (subtract the `vital` resource; `on_depleted` = death) → **on-hit
  procs** (`OnHit` event → content). Each stage emits its own message (visibility-aware) + reserves a
  GMCP event (Phase 9).
- **Cooldowns [G8]:** an ability commit arms `lag` (WAIT_STATE, exists) + a **per-ability cooldown**
  (new `pulse.after` + a cooldown map). Lifecycle **step 3** gains a "still cooling down?" gate
  (today it fires-and-logs). The map serializes into `state` (P6-D8). The **GCD** is just a shared-
  tag lag affect (apply a `gcd` affect that `prevents` the `ability` tag for N pulses — pure content
  on the Phase 5 tag-CC model, no engine change). Charges = a small resource that regens one per cd.
- **Death / corpse / threat:** `on_depleted` on the `vital` resource triggers the engine death path
  — create a **corpse** container entity holding inventory+equipment+coins, fire `OnKill` (XP award
  is a content handler — the [G6] hook, not built here), drop `Fighting`, mob corpse takes a loot-
  roll reservation (Phase 11/12). A single primary `Fighting` target; `assist`/threat list (damage +
  heal weighted) chooses mob targets; aggressive mobs initiate on entry.

### 1.4 Formula context scoping & new heads (P6-D1/§G1)

`formula.go` gains: arithmetic heads `floor`/`ceil`/`round`/`mod` and a conditional head
(`["if", cond, then, else]`) for exact integer derivation (5e mods, PF BAB, WoW ratings); and a
**scoped `attr` ref** — `["attr", "$target.dex_save"]` resolves against whichever entity the check
binds to `$target`. The `effectCtx` already tracks actor/target/source; the check evaluator threads
that binding into `evalFormula`. Cycle detection (Phase 5 visited-set) is unchanged.

---

## 2. Schema + loader integration

Phase 6 is **light on new tables** — most of it is new *op handlers*, new *formula heads*, new
*pulse callbacks*, and new `on_<event>` JSONB fields on **existing** def-tables (additive, JSONB-tail
pattern — no migration strain; **persistence-engineer to confirm**).

### 2.1 Migration `00004_combat.sql` (small)

```sql
-- Named, reusable check specs (inline specs need no table; this is for shared checks).
CREATE TABLE check_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  body JSONB NOT NULL DEFAULT '{}'   -- {dice, bonus, vs, bands[], visibility}
);
```

Everything else rides existing tables as **new JSONB fields** (no `ALTER` needed — they live in the
existing `body`/tail JSONB and are parsed by the extended mapper):
- `resource_def.body.on_event`, `.regen` extended to a **conditional formula** [G4] (regen `when` a
  content predicate — e.g. `-N when not in_combat`).
- `affect_def.body.on_event`, `.scope` ∈ `entity|room` [G13] (a `room`-scoped affect attaches to the
  room entity and ticks over occupants).
- `ability_def.on_resolve` gains the `check` op + AoE targeting (`targeting.area` ∈
  `self|target|room|room_and_adjacent`); `ability_def` gains `cooldown` *completion* (the column
  exists from Phase 5; now enforced) + `reaction` metadata (`checkpoint`, `reaction_cost`).
- item/gear modifiers into the attr mod-stack [G14] (the `modSource` seam is built-and-waiting from
  Phase 5 — wire equipped gear as a modifier source; affixes are Phase 12).

### 2.2 Loader / mapper / registries

- `internal/content/dto.go`: add `CheckDTO` + the `on_event`/`scope`/`area`/`reaction` fields to the
  existing ability/affect/resource DTOs; add `Pack.Checks`.
- `internal/store/content.go LoadPacks`: `SELECT` `check_defs WHERE pack = ANY($1)`; single-row
  loader for hot-reload (mirror Phase 5).
- World side: a `checkRegistry` (atomic.Pointer-swapped, like the Phase 5 registries); the
  DTO→runtime builders parse the check spec (band list, dice notation, scoped formula). The **event
  dispatcher** is per-shard runtime state (the per-entity handler maps are built at entity
  load/equip/affect-apply time — not a registry, derived from active content).

### 2.3 Stdlib combat pack (the acceptance content)

Extend the demo/stdlib pack with the **conventionally-named combat attributes** (`accuracy`,
`evasion`, `dodge`, `parry`, `block`, `soak_slash`/`soak_fire`/…, `attacks`, `crit_chance`,
`crit_mult`, `ac`, the save attrs), a **weapon** item (dice + damage type + `attacks`), an **armor**
item (soak mods via the `modSource` seam), a **mob** with those attributes + a loot reservation, and
two abilities exercising the new surface: `bash` (a melee skill: to-hit check, lag+cooldown, a stun
affect on hit) and a `fireball` upgrade (now an **AoE** over the room with a per-target DEX save
[G12], and a rage resource that builds via an `OnHit` handler [G3]). The **bare-engine invariant**
holds: no combat pack ⇒ no combat attributes ⇒ `kill` simply reports nothing to fight (the empty-boot
test stays green; combat ops are unavailable, not erroring).

---

## 3. Persistence integration (P6-D8)

- **Cooldowns [G8]:** `dumpCharacter` adds `cooldowns: {abilityRef: remainingPulses}`; `loadCharacter`
  re-arms each via `pulse.after(remaining)`. A logout mid-cooldown does **not** refresh it. Affect
  durations + resource currents are the Phase 5 shape, unchanged.
- **Transient (NOT in the snapshot):** `Fighting`/target, threat lists, the per-round reaction budget,
  the per-entity event-handler maps (derived, rebuilt on load), room-scoped affects (re-applied by
  content/reset). On a crash or handoff, combat drops cleanly; the durability ladder carries only the
  cooldown map + the already-persisted affects/resources.
- **Handoff:** the cooldown map rides the fat snapshot (same subtree mechanics as Phase 5 affects);
  the destination re-arms on `transferIn` on the destination goroutine (the Phase 5.2 lesson — never
  a cross-goroutine timer write). **distributed-systems-architect + persistence-engineer** confirm.

---

## 4. Slicing (ordered, independently committable)

The spine is **primitive → glue → combat → area/reactions**. Each slice is a commit with the prior
phase's tests green and its owning + cross-cutting reviewers signing off
([subagent-review-after-every-step]).

| Slice | Scope | Done when | Tests added |
|-------|-------|-----------|-------------|
| **6.1 — The check primitive [G2]** (the prefix) | Dice-notation extension (kh/kl, `dF`, pool `>N`); the `check` flow op + ordered-band classifier (P6-D2); the `$actor`/`$target`/`$source` scoped formula context + the new heads `floor`/`ceil`/`round`/`mod`/`if` [G1]; visibility resolution + text emission (P6-D5); the `OnCheck` fire point reserved. **No combat, no event handlers yet.** | A room exit with a climb `check` resolves deterministically (seeded) and branches to `move`/`deal_damage`; a DEX save inside `fireball`'s op-list halves damage on success; a PbtA 3-band and a contested check both classify correctly; visibility `hide`/`show` render the two paths. | check-op + band-classifier unit tests (binary / half / 3-tier / degrees / contested / pool); dice-notation parser tests; scoped-formula + new-head tests; visibility render test. All Phase 1–5 green. |
| **6.2 — The in-zone event bus [G3]** (the keystone) | Synchronous single-writer dispatch (P6-D3): per-entity handler collection from `on_event` on resources/affects/abilities/items; the fire points (`OnAbilityResolved`/`OnCheck` live; `OnHit`/`OnKill`/`OnLeaveRoom`/`OnRest` reserved for 6.3/6.4); the **recursion-depth budget**; `guardHarmful` on handler ops. Conditional resource regen [G4] + the `rest` command/`OnRest` event [G5] ride here. | A `resource_def` with `on_event OnCheck → modify_resource $actor rage +N` builds rage when its owner makes a check; the depth guard halts a deliberately self-firing handler; a harmful handler op vs a protected player is **blocked** by the gate; `rest` refills a per-rest pool; rage decays out of combat via a conditional regen. | dispatch + subscription-collection tests; **depth-guard / re-entrancy test**; **gate-applies-to-handler-ops test (security)**; conditional-regen + rest-event tests. |
| **6.3 — Combat round resolution** (the milestone) | `PULSE_VIOLENCE` round driver + `Fighting` state; `attacks`/round; the swing pipeline (gates → to-hit check → dodge/parry/block ladder → damage+crit → `dealDamage` soak → apply → `OnHit`); **cooldown completion + step-3 gate + persistence [G8]**; the GCD-as-tag-affect; gear modifiers into the mod-stack [G14]; death → corpse; threat/`assist`/`consider`/`flee`. | The ROADMAP done-when: `kill mob` → fight through the full pipeline (miss/dodge/parry/block/soak visible in the log), it dies, a corpse holds its gear+coins, you loot it; a cooldown survives logout; the GCD blocks back-to-back skills — all content. | swing-pipeline stage tests (each avoidance layer); to-hit-as-check test; crit-band test; death→corpse test; **cooldown persistence round-trip**; threat-selection test; round-driver pulse test (resolve-by-id). |
| **6.4 — AoE, room-affects & reaction checkpoints** | AoE targeting [G12] (loop the *built* `dealDamage`/harm-gate per target over `room`/`room_and_adjacent`, **same-zone-guarded** — §5); room-scoped affects [G13] (attach to the room entity, tick over occupants); named interruptible **checkpoints** [G9] firing events, with declarative reactions (opportunity attack on `OnLeaveRoom`; `OnDamaged` rebuke) + the per-round reaction budget. Result-altering reactions remain Phase 7. | `fireball` over a room rolls a **per-target** save and gates **each** target independently; a `web` room-affect roots entrants and ticks; leaving a room with an engaged enemy provokes a declarative opportunity attack that consumes the reaction budget; an AoE never reaches a same-named room on another shard. | per-target AoE + **per-target gate** test; **cross-zone AoE-containment test**; room-affect attach/tick/occupant test; opportunity-attack + reaction-budget test. |

**Adjustment / justification.** 6.1 lands the check primitive *standalone* exactly as decided (gap
§18.2) — its biggest payoff is being usable by non-combat content (exits/objects) before combat
exists. 6.2 lands the bus next because 6.3's `OnHit`/`OnKill` and the resource builders depend on it.
If **6.3 proves large**, split **6.3a round driver + single-target swing pipeline + cooldowns** from
**6.3b death/corpse/threat/assist** (the death path touches loot reservations + groups). If **6.4
proves large**, split AoE+room-affects from reaction checkpoints. Recommend planning for the splits,
executing as one each if they stay small (the Phase 5.3 pattern).

---

## 5. Risks & out-of-scope

### Explicitly OUT of scope
- **Lua handlers / result-altering reactions [G9] / concentration [G11] / multiclass slot math [G7]
  = Phase 7.** Phase 6 ships the *declarative* event handlers + the *checkpoint events*; the alter-
  the-in-flight-result path and the complex-20% are the Lua hatch. Design the checkpoints **now** so
  Phase 7 only adds handlers, not pipeline surgery.
- **The cross-zone scoped + durable event bus = Phase 10** ([WORLD-EVENTS.md](WORLD-EVENTS.md)). Phase
  6's bus is in-zone, synchronous, transient. A handler needing a cross-zone consequence enqueues for
  the (Phase-10) director — reserved no-op.
- **Progression / chargen / XP-to-levels [G6] = Phase 11.** Phase 6 *fires* `OnKill` and awards no XP
  itself — the XP-on-kill handler is content that lands with the progression tracks. Classes/races as
  bundles, advancement modes: Phase 11.
- **GMCP combat deltas = Phase 9.** Emit `act()`/`send` text now (visibility-aware); reserve the
  `Char.Vitals`/`Char.Status`/`Mud.Target`/`Mud.Cooldowns`/`Mud.Afflictions` emit points (COMBAT §8).
- **Loot tables / affixes / scheduled boss spawns = Phase 11/12.** Death creates a corpse + reserves
  a loot roll; the resolver is later. Gear `modSource` wiring [G14] is here; affix *rolls* are Phase 12.
- **Tactical-grid spatial fidelity = never** (settled gap §18.3) — no intra-room coords; range =
  same-room vs adjacent-exit; positioning = abstract engaged/disengaged tags.

### Integration risks
1. **Event-bus re-entrancy & ordering (the keystone risk).** Synchronous in-zone dispatch means a
   handler runs *inside* the action that fired it. An `OnHit` handler that deals damage fires
   `OnDamageTaken`, whose handler might heal/reflect, etc. Mandatory: a **depth budget** in the
   `effectCtx`, stable within-kind ordering, and a rule that handlers see a **consistent single-
   threaded** zone (no I/O, no cross-entity timer writes). **distributed-systems-architect must
   review** — and confirm the clean seam to the Phase-10 durable/ordered/idempotent cross-zone bus so
   we don't build something Phase 10 must rip out.
2. **AoE must not reach across a shard.** "room + adjacent rooms" can name an exit whose destination
   is a zone on **another shard**. The AoE loop must enumerate **same-zone** targets only (or message-
   pass to the neighbor zone — reserved); never dereference a cross-zone `*Entity` (the Phase 5.2/5.3
   single-writer bug class). **distributed-systems-architect reviews** the containment.
3. **The PvP gate over the whole new harm surface (security).** Every new harm vector — swing damage,
   AoE per-target, event-handler ops, reaction op-lists, room-affect ticks — must funnel the **same**
   `guardHarmful` (P5-D4), gated **per target** (an AoE re-gates each occupant; an `OnHit` proc that
   damages re-gates). A check that *branches* into a harmful op does not bypass the gate (the gate is
   at the op, not the ability). **security-auditor must review 6.2, 6.3, 6.4** — this is the largest
   harm-injection surface added since the gate was built, and the in-op funnel is what makes it
   can't-forget. Also: a content `check` must not let a pack read/leak another player's hidden state
   via `$target` scoping beyond what targeting already permits.
4. **Don't block the zone goroutine.** Round driver, swings, checks, event handlers, cooldown timers
   all run single-writer on the pulse — any I/O (none expected) goes async + posts back (reset.go /
   saver pattern). The round loop must obey the `pulseFunc` resolve-by-id/skip-frozen contract.
5. **Combat numbers stay content (the pillar).** No hardcoded `hp` (read the `vital` resource by
   flag), `d20` (content dice notation), or `success/failure` (content bands). The conventionally-
   named combat attributes (`accuracy`/`evasion`/`soak_*`/`attacks`) are a *pack convention*, not
   engine constants — a non-d20 pack redefines them. The **rpg-systems-designer** validates that a
   plain Diku/ROM fight, a 5e attack-vs-AC, and a WoW hit/crit table all express in this pipeline
   (the §16 acceptance question) before 6.3 is cut.
6. **Cooldown/affect conservation across save & handoff.** Cooldowns (new) join affects/resources in
   the persisted subtree; re-arm on the destination goroutine on `transferIn`, never reset on load.

### Cross-cutting reviewers (per [subagent-review-after-every-step])
- **combat-engineer (owning):** 6.3/6.4 — the round driver, swing pipeline, avoidance ladder,
  cooldowns, death/corpse/threat; confirm the §20 validation (checks host to-hit/saves; checkpoints;
  AoE over the built `dealDamage`; `attacks`/avoidance over content formulas; `soak()`/`modSource`).
- **abilities-engineer (owning):** 6.1/6.2/6.4 — the `check` flow op fits beside `if`/`chance`
  additively; `on_event` subscriptions + AoE + room-affects fit the effect-op/Affected runtime with
  no lifecycle change; the resource→scaling read (combo finishers).
- **distributed-systems-architect:** 6.2/6.3/6.4 — the in-zone event-bus re-entrancy/ordering/depth
  guard + the Phase-10 boundary; combat on the pulse (single-writer); AoE same-zone containment;
  cooldown/affect conservation across the handoff + durability ladder.
- **security-auditor:** 6.2/6.3/6.4 — the PvP/hostility gate over the entire new harm surface
  (per-target AoE, event-handler ops, reaction ops, room-affect ticks); check `$target` scoping can't
  leak state; the in-op funnel is can't-bypass even ahead of the Phase 7 Lua hatch.
- **persistence-engineer:** 6.3 — cooldown serialization into `state`; combat-transient state excluded
  from the snapshot; `check_defs` follows the per-kind-table + JSONB-tail + `pack` pattern.
- **rpg-systems-designer (acceptance):** before 6.3 — confirm Diku/ROM, 5e attack-vs-AC, and a WoW
  hit/crit table all express in the pipeline + the check bands (the §16 full-spectrum acceptance).
- **scripting-engineer (forward-looking):** review the 6.4 checkpoint set so the Phase 7 result-
  altering reactions (Counterspell/Shield) + concentration have the hook points they need.
