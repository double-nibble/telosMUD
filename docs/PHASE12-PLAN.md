# Phase 12 — Loot & scheduled spawns

The second slice of **Track D** (progression & economy). Two features that combine into the target "key
boss" experience — *kill the weekly boss for months chasing the legendary sword*: a **modern-MMO loot
system** (always-good drops with a rare legendary + bad-luck protection) and **long-timer scheduled
spawns** (a weekly world boss). Design: [LOOT-AND-SPAWNS.md](LOOT-AND-SPAWNS.md).

Status: **COMPLETE (12.1–12.4 + capstone landed, CI green).**

**Settled forks (confirmed):** (1) FULL scope (12.1–12.4 + capstone), 12.3 deliberately coarse (item level
+ a few affixes). (2) Eligibility v1 = "dealt any damage" via the existing damage/threat record + a content
tag hook. (3) Delivery = personal-direct to each looter (corpse holds only the body). (4) Scheduled-spawn
command path = a 10.4 remote-effect broadcast (zone reacts by spawning), no new transport. (5) Rare+ drops
persist, common ages out (ephemeral). (6) The `on_roll(ctx)` Lua escape hatch is DEFERRED — ship the
declarative resolver first.

## Why now (what it builds on — all in place)

- **The death path + `OnKill`** (Phase 6/11) and `makeCorpse` (death.go) — which already records the
  killer. The loot resolver hooks here; the killer/threat record is the loot-ownership seam.
- **Flyweight item instances + per-instance deltas** (Phase 4) — a rolled item is the shared prototype +
  a per-instance quality/affix delta; the prototype stays shared, only the delta varies.
- **The director tier + durable scope state** (Phase 10) — `world_state`/`region_state` (versioned,
  restart-safe) is exactly where `next_spawn_at` lives; the director heartbeat checks due schedules and
  commands the owning zone (the remote-effect path from 10.4).
- **The def-table precedent** (channel→region→track→bundle) — every new table (`loot_table_defs`,
  `rarity_tier_defs`, `affix_defs`, `spawn_schedule_defs`) is the same ref/pack/JSONB-body shape.
- **The per-zone deterministic RNG** (Phase 7) — the resolver rolls on the dying mob's zone goroutine.

On-pillar throughout: every table, tier, affix, and schedule is CONTENT; the engine runs the resolver +
the scheduler and names no boss, item, or tier.

## Testing mandate (binding — memory `testing-standard`)

Every slice ships tests across all tiers: unit + table-driven for the resolver math (weights, pity curve,
quality rolls — seeded RNG makes these deterministic), gated PG round-trip for each new def-table + the
pity/schedule persistence, and a milestone/e2e for the capstone. Per-slice reviews: owning engineer =
`progression-engineer`; cross-cutting = `persistence-engineer` (def-tables + pity/schedule state),
`distributed-systems-architect` (the director scheduler's restart-safety + idempotency),
`security-auditor` (loot can't be duped/forced; eligibility can't be gamed).

## Slices

### 12.1 — Rarity tiers + loot tables + the resolver core
The loot system's spine: content tables + the on-death resolver (no quality variance or pity yet).
- `rarity_tier_defs` (ordered named tiers: common→…→legendary, each with weight/color) + `loot_table_defs`
  (a list of independent `roll`s: `guaranteed` / `chance` / `weighted_one` / `weighted_n`, with an optional
  `quality_floor`). A mob prototype references a loot table by ref.
- The resolver runs on death (the dying mob's zone goroutine, seeded RNG): eligibility → per-looter
  (personal loot, each eligible player rolls independently) → resolve each roll → deliver to that player
  (the corpse holds only the body — no contested pickups, §5 step 6).
- **Done when:** a mob with a loot table dies and each eligible player independently receives their own
  rolled drop (a guaranteed rare+ item), delivered to them — content-defined, deterministic under a seed.

### 12.2 — Pity (bad-luck protection)
The bounded "grind for months": a `chance` roll may carry a `pity` spec `{key, step, cap}` — each kill
that misses nudges the chance up by `step` (to `cap`); a hit resets it. Counters are per-character
(`state.loot_pity` JSONB, riding the durability ladder), read+updated by the resolver.
- **Done when:** repeated kills without the drop raise the effective chance along the pity curve, a hit
  resets the counter, and the counter survives a save/reload — proven deterministically under a seed.

### 12.3 — Item quality variance (affixes)
The within-tier "always good, but it varies": on drop, the resolver rolls an instance quality — an item
level + a set of affixes from `affix_defs` — written into the item's per-instance delta (the prototype
stays shared). A legendary rolls from a richer pool. Optional per item (`quality_spec`). Coarse v1 (item
level + a small affix set); deep affix systems deferred.
- **Done when:** two drops of the same prototype carry DIFFERENT rolled affixes/level in their instance
  deltas, the delta round-trips through persistence, and a legendary rolls a richer set — under a seed.

### 12.4 — Director-owned scheduled spawns
The long-timer boss: `spawn_schedule_defs` content (`interval_after_death`/wall-clock, `on_missed`,
announce). The director persists `next_spawn_at` in world/region scope state, checks due schedules on its
heartbeat, commands the owning zone to spawn the boss (the 10.4 remote-effect path → a zone spawn), and on
the boss's death event computes the next time. Restart-safe: on startup the director loads schedules and
applies `on_missed` (`spawn_if_overdue` vs `skip_to_next`).
- **Done when:** a scheduled boss spawns when due, its death schedules the next spawn, and a director
  restart mid-interval resumes the schedule correctly (overdue → spawns; not-yet-due → waits) — no double
  spawn, no lost schedule.

### 12.x — Capstone (the done-when)
The combined scenario (§7): a weekly boss spawns on schedule, a raid kills it, and each eligible player
receives personal loot — a guaranteed rare+ item with rolled affixes — while independently rolling the
rare legendary with a working pity timer that survives a restart. Demo content + an e2e milestone + the
director-restart-mid-schedule + pity-survives-reload tests (Phase-10/11 capstone rigor).

## Decisions to settle before building (the open forks)

1. **Scope.** Full (12.1–12.4 + capstone), or core-loot-first (12.1 + 12.2 + 12.4 + capstone, deferring
   the affix/quality layer 12.3)? The roadmap calls quality variance "coarse v1"; the doc marks it optional
   (the old §8 D3). *Recommend:* full, but keep 12.3 deliberately coarse (item level + a few affixes).
2. **Eligibility model (v1).** Who may loot — *dealt damage to the mob* (simplest fair rule), tagged-the-mob
   (first/last hit), or group membership? *Recommend:* "dealt any damage" via the existing threat/damage
   record, with a content tag hook for later refinement.
3. **Delivery model.** Personal loot delivered DIRECTLY to each eligible player (no contested corpse
   pickup, per §5 step 6), vs. dropping into the corpse container. *Recommend:* personal-direct (matches the
   doc + the modern-MMO feel; the corpse holds only the body).
4. **Scheduled-spawn command path.** The director commands the owning zone to spawn via a 10.4 remote-effect
   broadcast (a zone `on_world`/`on_region` handler runs the spawn), vs. a new engine `spawn_in` op/gRPC.
   *Recommend:* the remote-effect broadcast — reuses Phase 10, no new transport.
5. **Persistent vs ephemeral drops.** A rolled legendary is a PERSISTENT item instance; a common drop left
   on the ground ages out (ephemeral). *Recommend:* persist by a rarity/`bind`-driven flag (rare+ persists),
   common stays ephemeral — confirm the threshold.
6. **The `on_roll(ctx)` Lua escape hatch** (conditional drops the declarative form can't express — "only
   while the realm is at war"). In scope for 12.1, or deferred? *Recommend:* defer to a follow-up; ship the
   declarative resolver first (declarative for the 80%, Lua hatch later).

## Builds on / relates to

Phase 6 (death + `OnKill`) · Phase 4 (item instances + the durability ladder) · Phase 10 (director +
durable scope state + remote effects) · Phase 11 (`OnKill` already drives XP; loot hangs off the same
hook). Crafting/economy (binding, salvage, the material economy) is the NEXT phase (**Phase 13**); auth +
the website are **Phase 14**.
