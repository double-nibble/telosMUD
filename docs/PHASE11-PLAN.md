# Phase 11 — Character progression & chargen

The opener of **Track D** (progression & economy) and the largest single content area the gap analysis
surfaced ([GAME-SYSTEMS-GAP-ANALYSIS.md](GAME-SYSTEMS-GAP-ANALYSIS.md) §5, gap **[G6]**). It depends only
on the Phase-6 event bus + Phase-7 Lua (both done) — nothing in Track C blocks it.

Status: **plan LOCKED (scope confirmed 2026-06-29).** Building 11.1 → 11.5 + capstone.

**Settled scope (the 5 forks, confirmed):**
1. **Core progression: 11.1–11.5 + the capstone. Chargen (11.6) is DEFERRED to pair with Phase 14**
   (account/login) — its interactive front end wants the auth flow; the grant/bundle machinery does not.
2. **Spell slots / per-class casting is DEFERRED** to a dedicated casting slice (and the hairy multiclass
   fractional-caster-level slot table regardless). Phase 11 stays on the track/grant/bundle core.
3. **One GENERIC `bundle_defs(kind, …)` table** (a kind discriminator), one loader path — not per-kind tables.
4. **`OnLevel`/`OnTrackStep`/`OnSkillUse` are new engine `eventKind`s** (the engine fires them).
5. Chargen depth — N/A this phase (deferred per #1).

## The binding shape (settled — do not re-litigate)

From the gap-analysis decisions (memory `gap-analysis-decisions`, settled 2026-06-26):

- **N independent advancement TRACKS.** Character level, guild/class levels, use-based skills — each a
  separate track. There is no single "level" the engine privileges.
- **`level` is an ORDINARY ATTRIBUTE.** Some tracks happen to raise a `level` attribute; a use-based MUD
  has tracks with no `level` at all. The engine must never grow a `level` concept. (The risk to watch.)
- **A level-up is an OP-LIST.** Grants reuse the whole effect-op interpreter + the event bus — they are
  not a new code path. "Which event feeds the track" is the only thing that differs between XP-auto,
  train-at-trainer, point-buy, and use-based — all four are content, not four engine paths.
- **Bundles are content.** `class_def`/`race_def`/`background_def`/`feat_def`/`talent_def` are content
  bundles of grants (+ track definitions); the engine knows only the KIND "bundle", never "fighter".
- **Acceptance target: the 5e SRD.** A 5e class+race expressed as pure content is the design proof
  (memory `srd-5e-as-design-target`), while staying expressible for Pathfinder / use-based / WoW-like
  (memory `extensibility-across-game-systems`).

## What already exists (the foundation Phase 11 builds on)

- **Effect-op interpreter** (`internal/world/effect_op*.go`): a registry of ops (`deal_damage`, `heal`,
  `modify_resource`, `apply_affect`, `if`, `chance`, `check`, …) run as op-lists. Phase 11 ADDS grant ops.
- **Per-entity attribute base override** (`setAttrBase`): chargen/point-buy/level-up write a stat's base
  here. `modify_attribute_base` is a thin op over it.
- **The in-zone event bus** (`event.go`): `OnHit`/`OnDamageTaken`/`OnKill` are LIVE; the closed `eventKind`
  set + the custom-event lane (`mud.fire`/`on(name,fn)`) exist. Phase 11 ADDS the progression events.
- **Abilities** granted as command verbs (per-shard ability registry + command table). `grant_ability`
  adds to an entity's granted set.
- **Character `state` persistence** already carries `templates`/`attributes`/`skills` subtrees — the
  persisted shape for tracks/grants largely exists; verify + extend in 11.2.

## Testing mandate (binding — memory `testing-standard`)

Every slice ships tests across all tiers (unit + table-driven + fuzz where a parser/threshold is involved
+ gated PG round-trip for any new def-table + the e2e/journey for the capstone) and per-slice reviews
(owning engineer = `progression-engineer`; cross-cutting = `persistence-engineer` for def-tables/state,
`rpg-systems-designer` for the system-fidelity check, `scripting-engineer` for any Lua surface). New
features mean new tests of every tier.

## Slices

### 11.1 — Grant ops [G6b] (the foundation)
The additive effect ops a level-up / bundle / chargen runs, reusing the effect-op interpreter + op-list
machinery. These two wrap EXISTING persisted seams (`setAttrBase`, `setFlag`) so they survive a reload by
construction (the state subtree is restored, not the grant re-run):
- `modify_attribute_base` (raise/lower a stat's per-entity base — the constraint explicitly names this),
  `set_flag`/`clear_flag` (the open-set named-flag grant).
- The other grant ops are coupled to mechanisms built in later slices and land there: **`grant_track`** with
  the track machinery (11.2); **`grant_ability`** with the per-entity ability-ownership model + the
  invocation gate (11.4 — content abilities currently dispatch globally, so the ownership gate is a real
  mechanism, not a thin op); **`grant_resource`** (= `modify_attribute_base` on the pool's max attr +
  set-current) folded into 11.2/11.4.
- **Done when:** an op-list raises an entity's `strength` base and sets a named flag, and both survive a
  save/reload.

### 11.2 — `track_defs` + the track machinery [G6a]
A track = `{ progress_attr, thresholds[], grants_per_step }` (`progress_attr` is just an attribute —
`xp`, `mining_skill`, `warrior_xp`). The engine watches the progress attribute, and on a threshold
CROSSING fires `OnTrackStep` (and `OnLevel` when the step grants a `level`-attr bump) → runs the step's
grant op-list (11.1). A per-entity track SET in `state`, persisted; the threshold check is the new
mechanism, not a new code path for grants.
- new def-table `track_defs` (migration + DTO + loader + store round-trip, mirroring `channel_defs`).
- **Done when:** an entity with an XP track crosses a threshold (progress attr raised by any op), the
  engine fires the step, applies the grants, advances to the next threshold, and the new level/grants
  survive a restart — exactly once across the reload (no double-grant).

### 11.3 — Progression events [G6d] (the "which event feeds the track" glue)
Wire the events that DRIVE tracks, so every advancement mode is just an event→op binding:
- `OnKill → modify_attribute(xp, +reward)` (XP-auto — Diku/5e). `OnKill` already exists; wire an XP grant.
- `OnSkillUse → chance(p, modify_attribute(skill, +1))` (use-based — LP/Discworld/BRP). `OnSkillUse` is a
  NEW engine event fired when a skill-tagged ability/check is used.
- `OnLevel`/`OnTrackStep` as the engine events 11.2 fires (content subscribes to run flavor/unlocks).
- **Done when:** killing a mob raises XP and auto-levels one track; using a skill has a chance to improve
  it (a second, level-less track) — both content-defined, no engine code per mode.

### 11.4 — Template/bundle def-tables [G6c]
`class_def`/`race_def`/`background_def`/`feat_def`/`talent_def`: content bundles of grants (attrs/
resources/abilities/flags/affects) + track definitions, applied when chosen or when a track step is
reached. `grant_track` (11.1) adds a class track at runtime; entry prerequisites are a `check` against
attributes (prestige class / multiclass requirement).
- one generic `bundle_defs` table with a `kind` discriminator (class/race/…) OR per-kind tables — a fork
  (see §Decisions). Bundles are pure content; the engine knows only "apply this bundle's grants".
- **Done when:** applying a `race_def` + a `class_def` bundle to a fresh entity grants the right
  attrs/abilities/track, a "join guild at 5" content action adds a second class track mid-life, and a
  prestige track's entry check gates it — all content, surviving a restart.

### 11.5 — The four advancement modes, demonstrated as content
Prove the union abstraction: the same track machinery expresses all four modes with no engine branch.
- XP-auto (11.3) ✓; **train-at-trainer** = an NPC ability that spends a currency resource and runs the
  step grant op-list directly (no auto-threshold); **point-buy** = a level grants a `points` resource +
  a `spend_points` ability that `modify_attribute_base(+1)` per point (WoW talents / 5e ASIs).
- **Done when:** the demo pack ships one track per mode and a journey test drives each (kill→auto-level,
  visit-trainer→train, spend-points→stat bump, use-skill→improve).

### 11.6 — Chargen flow [G6e] — **DEFERRED to Phase 14** (pairs with account/login)
The creation-time grant flow: choose race/class/background, point-buy stats, apply the bundles. Its
interactive front end wants the Phase-14 account/login flow, so it lands there. The grant/bundle machinery
it drives is built here (11.1/11.4), so Phase 14 chargen is "wire the existing bundles into a creation UI".

### 11.x — Capstone (the done-when)
A character is **created from a class+race bundle**, **gains XP on kills** (auto-leveling one track),
**trains a skill through use** on another track, and **the build survives a restart** — all content.
Demo content + an e2e milestone journey + a restart-survival test (mirrors the Phase-10 capstone rigor).

## Builds on / relates to

Phase 6 (check primitive + event bus) · Phase 7 (Lua + the effect-op interpreter) · Phase 4 (persistence —
the track/grant state must survive a restart, the capstone's done-when). Acceptance target: the 5e SRD
(memory `srd-5e-as-design-target`). The loot/spawns that hang off `OnKill` are **Phase 12**; auth/website
+ the real chargen front end are **Phase 14**.
