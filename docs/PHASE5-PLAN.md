# Phase 5 — Attributes, resources, affects & the ability framework — IMPLEMENTATION PLAN

Status: **proposal / planning** — needs the four decisions in §1 confirmed before slice 1.

The generic substrate ([ABILITIES.md](ABILITIES.md) §1–2) + the effect-op vocabulary (§3) + the
ability lifecycle (§4) + the `Affected` runtime (§5) + tag CC (§6) + the automatic PvP gate
(§7, D2). **Done when** (ROADMAP Phase 5): a data-defined `fireball` casts, costs mana, deals
typed damage, and applies a content-defined affect — *all without engine code changes*.

This phase builds the **substrate** combat (Phase 6) consumes; it does **not** build combat
round resolution. Lua `on_resolve_lua` is Phase 7 — Phase 5 ships the **declarative** op-list and
**reserves** the Lua hook. GMCP is Phase 9.

---

## 0. Where Phase 5 sits on the existing code

| Existing (Phase 1–4) | Phase 5 change |
|---|---|
| `Living` (components.go) carries `hp/maxHP/mp/...` as **stub int fields** | Becomes **real**: vitals are content-defined *resources*; `CoreStats` become content-defined *attributes* with a modifier stack. |
| `Affected` / `Skilled` component **kinds reserved** (component.go, never instantiated) | `Affected` becomes a real component the runtime ticks; `Skilled` carries known abilities + prof. |
| `pulse.go` per-zone scheduler (single-writer, `every`/`after`, resolve-by-id contract) | Affect **ticks** + cast-time/cooldown timers hang off this — the contract in the `pulseFunc` doc comment is load-bearing. |
| `content` pkg: Pack → **Zones** → rooms/protos/resets; `Source.LoadPacks` | New **pack-scoped, zone-independent** definition kinds (attributes/resources/damage-types/affects/abilities). This is a *structural* extension — today every DTO hangs under a `ZoneDTO`. |
| `content_map.go` DTO→component mapper; `build.go defineContent` | Extends to register the new global defs into per-shard registries (not the proto cache). |
| `reset.go` data-op interpreter (switch on `r.Op`, single-writer, no-I/O) | The **template** for the new effect-op interpreter — same switch-on-kind, same single-writer/no-blocking discipline. |
| `character.go dump/loadCharacter`; `StateJSON` reserves `attributes/resources/affects/skills/flags` (round-tripped **opaquely** in Phase 4) | Those subtrees become **interpreted**: current resources + active affects (remaining dur/stacks) round-trip and re-attach. |
| migrations `00001_definition_tables.sql` | New migration adds the 5 (±2) global definition tables. |

The riskiest *structural* point is that today **all content is zone-scoped**; attributes/resources/
damage-types/affects/abilities are **global to a pack** (a `fireball` is not "owned" by Midgaard).
See §2.

---

## 1. Tech / design decisions (confirm before slice 1)

| # | Decision | Recommendation | Trade-off |
|---|----------|----------------|-----------|
| **P5-D1** | Derived-attribute **formula language** | A small **declarative postfix/AST expression** evaluated by an engine interpreter (no Lua). Operators `+ - * / min max`, refs to other attrs (`con`, `level`), literals. Stored as JSON in `default_base`. | Keeps formulas content-defined *now* (Lua is Phase 7). Cost: we build a tiny evaluator + must avoid cycles. Alt: defer all derived attrs to Phase 7 Lua — rejected (`max_hp` is needed for the milestone resource caps). |
| **P5-D2** | v1 **effect-op set** | Implement: `deal_damage`, `heal`, `restore`, `modify_resource`, `apply_affect`, `remove_affect`, `dispel`, `act`/`send`, and flow `if`/`chance`. Defer: `drain`, `move`/`teleport`/`pull`/`push`/`recall`, all `scan`/`reveal`/`detect`/`identify`, `spawn`/`summon`/`transform`, `gmcp`, `for_each`/`delay`. | The deferred set is exactly what combat (6), perception, world-manip, and GMCP (9) own; the v1 set is the minimum for "costs mana, typed damage, applies affect". Each op is a *registered handler*, so deferred ops are later additions, not rework. |
| **P5-D3** | Affect **stacking model** | Support all four §5 modes — `refresh` (default), `stack` (count up to `max_stacks`, magnitude scales), `extend` (sum durations), `ignore` (first wins) — as a `stacking` enum on the affect def. Keyed by `(affectRef, source)` or `(affectRef)` per a `stack_scope` field (default: per-source). | Poison needs `stack`; haste needs `refresh`. Implementing all four now is cheap (one switch in `apply`) and avoids a v2 migration of saved affects. Alt: ship only `refresh`+`stack` — rejected, the data field is the same effort. |
| **P5-D4** | PvP-gate **shape** (security boundary) | `pvp_allowed(actor, target) bool` is an **engine function** backed by a **content-defined policy** (a small declarative ruleset: consent flags, safe-room flag, level gap, arena zone flag). Enforced at **two** choke points: lifecycle step 4 (whole-ability) **and** inside *every harmful effect op* (a shared `guardHarmful` the op handlers funnel through). Mob targets ⇒ no-op. | Defense-in-depth, can't-forget. The in-op check is what makes even a future Lua `on_resolve` unable to bypass. Cost: every harmful op must route through the guard — enforced by making the damage/debuff path a single shared function, not copy-paste. **Security-auditor must review.** |

### 1.1 Derivation model (P5-D1)

`attr(e, "strength")` resolves through the stack (ABILITIES §1):

```
base            (literal, or a derived FORMULA evaluated against other attrs)
  → + flat mods   (Σ of additive modifiers from gear + active affects)
  → × multipliers  (Π of multiplicative modifiers)
  → computed       (clamped to the attr's min/max if declared)
```

- **base** comes from the attribute_def `default_base`: either a literal (`{"lit": 10}`) or a
  formula (`{"expr": ["*", ["attr","con"], 10]}` → `con*10`). Per-entity overrides
  (race/class/level/point-buy) live in the entity's `attributes` state and replace the default base.
- **mods** come from two sources: equipped gear (an `Armor`/affix component — coarse this phase)
  and **active affects** (`modifiers = [{attr, op:add|mul, value}]`, §5). The `Affected` runtime
  is the writer of the affect-sourced modifier list.
- **Formula form** (recommended): a nested-array prefix AST, JSON-serializable, no Lua:
  `["+", ["*", ["attr","con"], 10], ["*", ["attr","level"], 5]]` = `con*10 + level*5`.
  Allowed heads: `+ - * / min max clamp`, `["attr", name]`, `["lit", n]`. A reference to an attr
  pulls *its* resolved value (recursive `attr()`), so derived-of-derived works.
- **Cycle / cost guard:** derivation is **memoized per entity** with a dirty-bit invalidated when
  any base/mod/affect changes (`attr()` is hot). Cycle detection = a per-resolution visited set
  (a self/mutual reference errors at load via content-lint, and defensively at eval). Caching
  detail is in §5 (Risks) — it is the main perf concern.

### 1.2 Resources

A `resource_def` is `{name, max_attr (derived attr ref, e.g. "max_hp"), regen (formula or
per-tick literal), vital (bool), depleted_threshold, on_depleted (op-list)}`. The engine holds
`current`; `max` is a derived attribute (so gear/affects that raise `max_hp` flow through §1.1).
`vital` + `on_depleted` is how "hp at 0 = death" is **content**, not Go (§5 ABILITIES). Regen ticks
on the pulse scheduler alongside affect ticks.

### 1.3 Effect-op interpreter (mirrors reset.go)

`on_resolve` is a JSON op-list. The interpreter is a `switch` over `op.Kind` exactly like
`reset.go applyReset` — single-writer (runs in step 8 on the zone goroutine), never blocks, unknown
op logged+skipped (content-lint is the real gate). Each op is a registered handler
`func(*effectCtx, opArgs) error`. The `effectCtx` carries actor, target(s), source, magnitude, and
the **shared mitigation + PvP guard** helpers. Adding an op = registering a handler — no lifecycle
change. The `on_resolve_lua` column is **read but not executed** this phase (reserved for Phase 7).

### 1.4 Affected runtime (P5-D3)

- **Attach:** `apply_affect` resolves the `affect_def`, runs the stacking rule against any existing
  instance keyed by `(ref[, source])`, sets `remaining = duration` (literal or formula), records
  `magnitude`/`stacks`/`source`, fires `OnApplyAffect`, and **dirties the target's attribute cache**
  (so its `modifiers` take effect on the next `attr()`).
- **Tick:** the runtime registers ONE pulse callback per affected entity (not per affect) via
  `pulse.every`, following the `pulseFunc` resolve-by-id-or-cancel contract verbatim — re-resolve
  the entity by id each tick, stop if absent/frozen. The callback decrements `remaining`, runs each
  affect's `tick.on_tick` op-list at its interval (a DoT is just `tick → deal_damage`), and expires
  any at 0 (`OnAffectExpire`, clear modifiers, re-dirty cache). Resource regen rides the same
  callback.
- **Modifiers feed derivation:** the `Affected` component exposes `flatMods(attr)`/`mulMods(attr)`
  that §1.1 sums/multiplies. Affect changes dirty the attr cache; that is the whole coupling.
- **Stacking:** see P5-D3.

### 1.5 Tag-based CC (§6)

Tags are **strings** (open set, no enum — pillar). An ability def carries `tags: ["cast","verbal",
"fire"]`; an affect def carries `prevents: ["move","verbal"]`. At lifecycle **step 3** the engine
asks: *does any active affect on the actor `prevents` any tag this ability carries?* If so, blocked
with the affect's block message. `requires.not_prevented = "cast"` uses the same query. The engine
never names a CC type. Represent active prevents as a small per-entity set the `Affected` runtime
maintains (union of active affects' `prevents`), so the step-3 check is O(tags).

### 1.6 Ability lifecycle (§4) on the existing machinery

The 10 steps (§4) map onto the existing command/pulse spine:
- **Step 1 invoke**: a `command`-invocation ability **registers a `Command`** (MUDLIB §6 command
  table) whose `Run` enters the lifecycle. `proc`/`passive` invoke from events (events are mostly
  Phase 6/7; reserve the hooks).
- **Steps 2–5** (targets / requires / **gate** / costs): synchronous, in the command handler, on the
  zone goroutine. Targeting reuses `Zone.Resolve` + scopes (MUDLIB §7).
- **Step 6 cast_time**: a `pulse.after` lockout (interruptible per a flag); on interrupt → refund.
  `cast_time = 0` (the fireball milestone) skips straight to commit.
- **Step 7 commit**: pay costs, impose `lag` via `ctx.Lag` (existing WAIT_STATE), arm `cooldown` via
  `pulse.after`.
- **Step 8 on_resolve**: the effect-op interpreter (§1.3); every harmful op re-checks the gate +
  routes the shared mitigation pipeline.
- **Steps 9–10 emit/events**: `ctx.Act` messages now; GMCP deltas (Phase 9) and the event bus
  (Phase 6/7) are **reserved hooks** — fire-no-op or log this phase.

The **shared mitigation pipeline** (`deal_damage` → resist/vuln/immune matrix from
`damage_type_defs` → soak) is a single function so "a spell and a sword obey the same rules"; Phase 6
attaches weapon swings to the same entry point.

---

## 2. Schema + loader integration

### 2.1 New definition tables (migration `00003_ability_tables.sql`)

Pack-scoped, **zone-independent** (no `zone_ref`), same ref/pack + columns + JSONB-tail pattern:

```sql
CREATE TABLE attribute_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  display_name TEXT NOT NULL,
  value_kind   TEXT NOT NULL,        -- 'int' | 'float' | 'derived'
  default_base JSONB,                -- literal or formula AST (§1.1)
  body JSONB NOT NULL DEFAULT '{}'   -- min/max, clamp, display
);
CREATE TABLE resource_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  display_name TEXT NOT NULL,
  max_attr TEXT,                     -- derived-attr ref that caps it (e.g. "max_hp")
  vital BOOLEAN NOT NULL DEFAULT false,
  body JSONB NOT NULL DEFAULT '{}'   -- regen, depleted_threshold, on_depleted op-list
);
CREATE TABLE damage_type_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  display_name TEXT NOT NULL,
  body JSONB NOT NULL DEFAULT '{}'   -- resist/vuln/immune matrix, color
);
CREATE TABLE affect_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  name TEXT NOT NULL,
  category TEXT,                     -- dispel/cure targeting
  stacking TEXT NOT NULL DEFAULT 'refresh',
  max_stacks INT NOT NULL DEFAULT 1,
  dispellable BOOLEAN NOT NULL DEFAULT true,
  body JSONB NOT NULL DEFAULT '{}'   -- duration, modifiers, prevents[], tick{}, on_apply/expire, resist
);
CREATE TABLE ability_defs (
  ref TEXT PRIMARY KEY, pack TEXT NOT NULL,
  name TEXT NOT NULL,
  invocation TEXT NOT NULL,          -- 'command' | 'proc' | 'passive'
  targeting JSONB NOT NULL,          -- mode/scope/range/disposition
  tags TEXT[] NOT NULL DEFAULT '{}', -- §6 CC tags
  requires JSONB NOT NULL DEFAULT '{}',
  costs    JSONB NOT NULL DEFAULT '{}',
  cast_time INT NOT NULL DEFAULT 0, lag INT NOT NULL DEFAULT 0, cooldown INT NOT NULL DEFAULT 0,
  on_resolve     JSONB,             -- declarative op-list (this phase)
  on_resolve_lua TEXT,              -- RESERVED, read-not-run (Phase 7)
  messages JSONB
);
```

**`class_defs` / `race_defs`: DEFER.** They are PERSISTENCE §1's named tables but the milestone
needs neither (a character can carry attributes/skills directly). Real class/race progression
(point-buy, level-up grants) is a meaningful slice of its own; reserve the tables, build them when
chargen/progression lands. *Confirm this deferral.*

### 2.2 Content pipeline + mapper extension

- **DTO:** add `AttributeDTO`, `ResourceDTO`, `DamageTypeDTO`, `AffectDTO`, `AbilityDTO` to
  `internal/content/dto.go`. **Structural choice:** these are **pack-global**, not under a `ZoneDTO`.
  Recommendation: add them to the top-level `Pack` (`Pack.Attributes`, `Pack.Affects`, …) and extend
  `LoadedContent` with global slices/maps (`lc.Attributes`, `lc.Abilities`, …) alongside `Zones`.
  This is the cleanest fit for "global to a pack" and keeps `Source.LoadPacks` returning whole packs.
- **Store:** extend `internal/store/content.go LoadPacks` to also `SELECT` the five tables
  `WHERE pack = ANY($1)` and fill the new `Pack` fields; add single-row loaders for hot-reload
  (mirroring `loadRoomDefinition`).
- **Mapper / registries:** the world side gains **per-shard registries** (not the proto cache —
  these aren't prototypes): `attrRegistry`, `resourceRegistry`, `damageTypeRegistry`,
  `affectRegistry`, `abilityRegistry`, each an `atomic.Pointer`-swapped table like `protoCache`
  (PERSISTENCE §8 reload semantics). `build.go defineContent` registers them at boot; ability
  command-invocations register into the command table. `content_map.go` gains the DTO→runtime
  builders (formula AST parse, op-list parse).

### 2.3 Strippable stdlib pack (D3 / §10)

Extend the embedded demo pack (`internal/content/packs/demo.yaml`) — or a new `stdlib` pack — with
sample: attributes (`strength`, `intellect`, `constitution`, `level`, derived `max_hp`, `max_mana`),
resources (`hp`, `mana` — `hp` vital with `on_depleted`), damage types (`fire`, `slash` with a small
resist matrix), affects (`poison` — the §5 example, a DoT + `-2 strength`; `haste` — a `refresh`
buff), and abilities (`fireball` — costs 30 mana, `deal_damage fire`, `apply_affect poison`). The
**bare-engine invariant** holds: boot with no pack ⇒ zero attrs/resources/abilities, server still
runs (the empty-boot test from Phase 4 must stay green and gain an assertion that combat ops are
simply unavailable).

---

## 3. Persistence integration

`StateJSON` (character.go) already reserves `attributes/resources/affects/skills/flags`
(Phase 4 round-trips them opaquely). Phase 5 makes them **interpreted**:

- **dumpCharacter** adds: `attributes` (per-entity base overrides only — derived values are
  recomputed, never stored), `resources` (`{hp:{cur:N}, mana:{cur:N}}` — current only; max is
  derived), `affects` (`[{id, dur(remaining), mag, stacks, source}]` — the §3 PERSISTENCE shape),
  `skills` (known abilities + prof), `flags` (incl. `pvp_opt_in`).
- **loadCharacter** re-applies: install attribute bases, set resource currents (clamped to derived
  max), and **re-attach each affect** via the runtime's attach path with `remaining` from the
  snapshot — which **re-registers the per-entity tick callback** and re-seeds the prevents set and
  modifier contributions. Affects must not double-tick or reset duration on load.
- **Affect durations across save/restore AND handoff:** durations are stored as **remaining pulses**
  (already pulse-denominated). On a handoff (cross-shard, fat snapshot — not via the store), the
  same `affects` subtree rides the snapshot; the destination zone re-attaches them exactly like a
  load. Decrementing only happens on the owning zone's pulse, so a frozen/in-flight entity does not
  tick (the resolve-by-id/skip-frozen contract) — durations are conserved across the seam. Surface
  to persistence-owner: confirm the snapshot/Redis-checkpoint/handoff all share this one subtree.

---

## 4. Slicing (ordered, independently committable)

The natural spine is **substrate → affected runtime → ability lifecycle/gate**. Each slice is a
commit with the prior phase's tests green.

| Slice | Scope | Done when | Tests that stay green / added |
|-------|-------|-----------|-------------------------------|
| **5.1 — Generic substrate** | Content-defined attribute/resource/damage-type/flag defs + tables + DTO/loader/registry wiring; the **modifier stack + derivation** (formula evaluator, memoized cache, invalidation); `Living` made real (vitals = resources, stats = attributes) without breaking call sites. No abilities yet. | A demo character loads with content-defined `strength`/`max_hp`/`hp`; `attr(e,"max_hp")` evaluates a formula through the stack; bare-engine boot (no pack) still runs. | All Phase 1–4 green (esp. handoff/character/zone tests as `Living` fields change behind accessors). New: derivation unit tests, formula-eval tests, empty-boot assertion. |
| **5.2 — Affected runtime** | Affects as content (`affect_defs` + DTO/loader); the real `Affected` component; attach / stacking (4 modes) / duration / **tick on the pulse scheduler** / expire; modifiers feed §1.1 derivation; **tag-based CC** (prevents set + step-3-style query, even before full lifecycle); persistence round-trip of active affects. | `apply` a `poison` affect → it ticks damage on the pulse, decrements, expires; a `-2 strength` affect changes `attr(e,"strength")`; a `prevents:["move"]` affect blocks a tagged action; an affect survives save/load with correct remaining duration. | 5.1 green. New: affect attach/stack/tick/expire tests, derivation-with-modifiers tests, CC query test, affect persistence round-trip test, **pulse resolve-by-id-or-cancel test for affect ticks**. |
| **5.3 — Ability lifecycle + effect ops + PvP gate (the milestone)** | The 10-step lifecycle on the command/pulse spine; the effect-op interpreter (§1.3, P5-D2 op set) modeled on reset.go; the **automatic PvP/hostility gate** at step 4 + inside every harmful op (P5-D4); the shared mitigation pipeline; `ability_defs` + DTO/loader; `fireball` in the stdlib pack. | **`fireball` casts**: a granted command, costs 30 mana (reserved/paid), resolves a typed `fire` damage op through resist/soak, and `apply_affect poison` — all from data, **zero engine changes** to add it. PvP gate blocks a harmful op vs a non-consenting player at *both* choke points. | 5.1+5.2 green. New: lifecycle-step tests, each op-handler test, **PvP-gate defense-in-depth tests (lifecycle AND in-op, incl. a hostile op that tries to skip step 4)**, mitigation-pipeline test, end-to-end fireball test. |

**Adjustment / justification.** I keep the three-slice spine from the brief but flag two things:
(a) tag-CC's *query* lands in 5.2 (it is an `Affected` capability) while its *enforcement at
lifecycle step 3* lands in 5.3 (no lifecycle exists until then) — the query is testable standalone in
5.2; (b) if 5.3 proves large, split off a **5.3a costs/timing/targeting lifecycle** (no harmful ops)
from **5.3b effect ops + PvP gate + fireball** so the security-sensitive gate lands as its own
reviewable commit. Recommend planning for the split, executing as one if it stays small.

---

## 5. Risks & out-of-scope

### Explicitly OUT of scope
- **Combat round resolution = Phase 6.** Phase 5 builds the substrate it uses (the shared mitigation
  pipeline, `deal_damage`, lag/cooldown, vitals/resources, `on_depleted`/death-as-content). No
  `PULSE_VIOLENCE`, attacks-per-round, avoidance ladder, threat, or corpses here.
- **Lua `on_resolve_lua` / affect-hook Lua = Phase 7.** Ship the declarative op-list; **reserve** the
  `on_resolve_lua` column and the `on_apply`/`on_tick`/`on_expire` Lua hooks (read-not-run, the AST
  form covers the milestone).
- **GMCP vitals/affliction deltas = Phase 9.** Emit step-9 messages via `act`; reserve the GMCP
  emit points.
- **class_defs / race_defs progression = deferred** (reserve tables; §2.1) — confirm.
- **Event bus (`OnHit`/`OnAffectTick`/proc dispatch) = mostly Phase 6/7.** Reserve the hook points so
  passives/procs wire in later without lifecycle change.

### Integration risks
1. **Don't block the zone goroutine.** Every lifecycle step, op, affect tick, and regen runs
   single-writer in the zone loop; any I/O (none expected this phase) must go async + post back,
   exactly as reset.go / the saver already do.
2. **Affect ticks must obey the pulse contract.** The `pulseFunc` doc comment (pulse.go) is binding:
   re-resolve the player by id each tick, **stop if absent or `s.frozen`**, never close over a stale
   `*Entity`. This is the first real registrant of that contract (the comment anticipated Phase 5);
   **distributed-systems-architect must review** the affect-tick registration + the
   duration-across-handoff conservation.
3. **Derivation performance — `attr()` is hot.** Memoize per entity with a dirty-bit invalidated on
   base/mod/affect change; a naive recompute-every-call through a formula tree will dominate combat.
   Cycle detection on derived-of-derived formulas (visited set + content-lint).
4. **The PvP gate must be truly can't-bypass.** Funnel *every* harmful op through one shared
   `guardHarmful` so a new op author can't forget it, and keep the lifecycle step-4 check as the
   outer layer (defense-in-depth). **Security-auditor must review** — this is the §7/D2 security
   boundary, and the in-op layer is what survives a future Lua escape hatch.
5. **Keeping Phase 1–4 green as `Living`/`Affected` become real.** `Living.hp/maxHP/...` change from
   raw fields to resource/derived-attr lookups; route reads through accessors so handoff/character/
   zone/COW tests don't churn. `cloneComponent` must learn the new `Affected` shape (its
   reference-typed slices/maps) or the COW `default` panic fires — handle it explicitly.

### Cross-cutting reviewers
- **security-auditor:** slice **5.3** (the PvP/hostility gate — both choke points, the in-op guard,
  the "Lua can't bypass" property even though Lua is Phase 7).
- **distributed-systems-architect:** slices **5.2 & 5.3** (affect ticks + cast/cooldown timers on the
  pulse scheduler; the resolve-by-id/skip-frozen contract; affect-duration conservation across a
  handoff and across the durability ladder).
- **persistence-owner (cross-component, surface-don't-edit):** the `StateJSON` interpreted subtrees +
  the shared affects/resources subtree across snapshot/checkpoint/handoff (§3).
