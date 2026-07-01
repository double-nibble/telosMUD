# Loot & scheduled spawns

Two features for "key boss" content: **long-timer scheduled spawns** (a weekly world boss) and
a **modern-MMO loot system** (always-good drops with a rare legendary). They combine into the
target experience: *kill the weekly boss for months chasing the legendary sword.*

Both stay on-pillar — every table, tier, affix, and schedule is content — and inside the actor
model — scheduling lives in the director, resolution in the zone goroutine.

---

## 1. Scheduled spawns vs. zone resets

These are **two different mechanisms**; only "certain key bosses" use the new one.

| | Zone reset (existing) | Scheduled spawn (new) |
|---|---|---|
| Cadence | minutes (repop) | days/weeks, or wall-clock cron |
| Anchor | zone uptime | **persisted, wall-clock** (survives restarts/deploys) |
| Scope | per zone, many mobs | a few key bosses; often **unique** (world/region) |
| Owner | the zone | the **`telos-director`** ([WORLD-EVENTS.md](WORLD-EVENTS.md)) |
| Lose on restart? | fine (rats repop) | **never** (the timer is durable) |

### Schedule definition (content)
```
spawn_schedule "boss:duskwall_warden" {
  proto    = "mob:duskwall_warden"
  where    = "duskwall:throne"
  cadence  = { kind = "interval_after_death", every = "7d" }  -- rolling: 7d after each kill
           -- or { kind = "cron", expr = "0 0 * * 0" }        -- fixed: every Sunday 00:00
  unique   = "world"               -- one instance MUD-wide ("region" or "none" also valid)
  on_missed = "spawn_if_overdue"   -- if the window passed during downtime; vs "skip_to_next"
  announce = { spawn = "A great roar echoes across the land — the Warden has returned!",
               death = "The Warden falls; silence returns to Duskwall." }
}
```

- **`interval_after_death`** gives the "kill it, then wait a week" loop; **`cron`** gives the
  "available every Sunday" model. Content picks.
- The director persists `next_spawn_at` (in world/region state, §6), checks due schedules on
  its heartbeat, issues `spawn_in(where, proto)` as a command to the owning zone, and on the
  boss's death event computes the next time. **Restart-safe:** on startup the director loads
  schedules and applies `on_missed`.
- `unique` prevents duplicates across zones/shards (the director is the single arbiter).

## 2. Loot system — structure

A **loot table** is content, referenced by a mob prototype. It is a list of independent
**rolls**; the modern-MMO feel comes from mixing roll kinds:

```
loot_table "boss:duskwall_warden" {
  -- always something good (the quality floor):
  roll { kind = "weighted_one", quality_floor = "rare",
         pool = { {item="obj:warden_blade",  weight=60},
                  {item="obj:warden_shield", weight=40} } }

  -- independent rare chance, NOT mutually exclusive, with optional pity (§4):
  roll { kind = "chance", chance = 0.005, item = "obj:legendary_sunsword",
         pity = { key = "sunsword", step = 0.0005, cap = 0.05 } }

  -- another independent chance:
  roll { kind = "chance", chance = 0.05, item = "obj:warden_signet" }

  -- guaranteed currency:
  roll { kind = "guaranteed", item = "obj:gold", amount = "200d10" }
}
```

Roll kinds:
- **`guaranteed`** — always yields (optionally from a pool filtered by `quality_floor`).
- **`chance`** — independent probability `p`; multiple chance rolls can all fire.
- **`weighted_one` / `weighted_n`** — pick 1 / N from a weighted pool; weights *are* the rarity
  distribution. `quality_floor` filters the pool to a minimum tier.

### Rarity tiers (content)
`rarity_tier_defs`: ordered, named tiers (`common → uncommon → rare → epic → legendary`), each
with a color and default weight. **Engine knows the *concept* of ordered tiers; the tiers
themselves are content** — add a `mythic` tier with a content write.

## 3. Item quality variance — "always good, but it varies"

The within-tier variation that makes even non-legendary drops feel rolled. On drop, the
resolver rolls an **instance quality**: item level + a set of **affixes** from `affix_defs`
weighted by the item's tier, written into the item's flyweight **delta** ([MUDLIB.md](MUDLIB.md)
§5). So two `warden_blade`s differ in stats/affixes; the prototype stays shared, only the delta
varies. A legendary additionally rolls from a richer affix pool. This layer is optional per
item (`quality_spec`). The affix pool is authored **inline** in each loot entry's `quality`
spec (`internal/content/packs/demo.yaml`).

## 4. Bad-luck protection (pity)

The bounded version of "grind for months." A `chance` roll may carry a **pity** spec: each kill
that *doesn't* drop the item nudges the chance up by `step` (to a `cap`); a successful drop
resets it. Counters are **per character**, stored in `state.loot_pity` (PERSISTENCE §3), read
and updated by the resolver. Pure RNG (no pity) is just a roll with no pity spec — content's
choice per entry.

## 5. The resolver (engine, on death)

Runs in the dying mob's zone goroutine, using the deterministic per-zone RNG (LUA §9):

```
1  eligibility   — who may loot (tagged the mob / damage threshold / group membership)
2  per looter    — each eligible player rolls independently (personal loot)
3  for each roll — apply pity + luck modifiers → resolve (guaranteed / chance / weighted)
4  select item(s)
5  roll quality + affixes → write instance delta
6  deliver       — each player's drops bind/deliver to them; the mob's corpse holds
                   only its body, never the rolled loot (no contested pickups)
7  update pity   — increment misses / reset on the legendary; emit GMCP + messages
```

The resolver is **fully declarative**: a loot table is composed entirely of the roll kinds of
§2 plus pity and quality specs, with no per-table Lua hook.

**Modifiers:** drop chances can be scaled by content-defined factors — a `luck` attribute,
difficulty, group size, a first-kill bonus — all just multipliers the resolver applies before
the roll.

## 6. Persistence

- **Schedules / next-spawn times** → director-owned `world_state` / `region_state`
  (WORLD-EVENTS §7), versioned, restart-safe.
- **Pity counters** → character `state.loot_pity` JSONB, riding the normal durability ladder.
- **Definitions** → `loot_table_defs`, `rarity_tier_defs`, `spawn_schedule_defs` — the same
  per-type-table + JSONB-tail + `pack` pattern as everything else (PERSISTENCE §1). Affix pools
  ride inline in each loot entry's `quality` spec rather than a separate table.
- A dropped legendary is a **persistent** item instance (PERSISTENCE §4); a common drop left on
  the ground is ephemeral.

## 7. The combined scenario

Weekly Warden + personal loot + pity = exactly your target:

1. The director spawns the Warden each week (`interval_after_death 7d`), announcing it.
2. A raid kills it. The resolver runs per eligible player (personal loot): everyone gets a
   guaranteed rare+ Warden item (rolled affixes — "always good, but varies").
3. Each player independently rolls the 0.5% Sunsword; pity nudges it up a hair each weekly miss.
4. Months in, a player's roll finally hits (or pity caps them into it) — *their* Sunsword, with
   its own rolled affixes. Pity resets.
