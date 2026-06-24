# Lua scripting — API & sandbox

The escape hatch and behavior layer behind the [extensibility pillar](PRINCIPLES.md). Content
composes the engine's [effect-op vocabulary](ABILITIES.md) through a **curated, stable Lua
API** — never raw Go internals. Lua runs *inside* the owning zone's goroutine, so scripts see
a consistent, single-threaded world.

Runtime: `gopher-lua`. Status: **proposal** — three choices flagged in §10.

---

## 1. Runtime model

- **One VM (`LState`) per zone.** Memory/perf amortized across that zone's scripts; isolation
  between zones is automatic (a script can only ever affect its own zone).
- **Single-threaded by construction.** The zone goroutine is the only caller into its
  `LState` (gopher-lua is not goroutine-safe — and we never need it to be). This *is* the
  actor model: scripts mutate world state with no locks because nothing else runs concurrently.
- **Per-script environment.** Each definition's chunk runs in its own sandboxed `_ENV` so
  scripts can't clobber each other's globals within the shared VM.
- **Never block.** No real sleeps, no I/O, no goroutines. Deferred work uses `mud.after`
  (§3), which schedules on the zone's timer wheel — not the OS scheduler. A script that needs
  data the zone doesn't hold returns control; the engine fetches async and re-invokes.

## 2. Where Lua plugs in (entry points)

All optional — a definition with no Lua is pure data.

| Entry point          | Fires when...                                                       |
|----------------------|------------------------------------------------------------------|
| **Triggers** (`on`)  | room/mob/item events: `enter`, `leave`, `speech`, `get`, `give`, `attack`, `death`, `tick`, `reset`, `greet` |
| **Ability `on_resolve`** | a skill/spell resolves (composes fx ops)                     |
| **Affect hooks**     | `on_apply` / `on_tick` / `on_expire` / `on_dispel`               |
| **Custom commands**  | content registers a brand-new verb implemented in Lua            |
| **Formulas**         | ruleset functions: `to_hit`, `soak`, `regen`, `xp_for`, derived stats |
| **PvP policy**       | the `pvp_allowed(actor, target)` decision (§7 of ABILITIES.md)   |

```lua
-- a mob definition's script
on("greet", function(ev)
  if not self.state.greeted[ev.actor:id()] then
    self.state.greeted[ev.actor:id()] = true
    self:say("Welcome, "..ev.actor:name()..". Seek the amulet in the catacombs.")
  end
end)

on("speech", function(ev)
  if ev.text:find("amulet") then self:emote("nods gravely") end
end)
```

## 3. The API surface

Curated and namespaced. Two shapes: **handle methods** (ergonomic, read like prose) and a
global **`mud`** table for world/utility ops.

**Entity handles** — `self`, `ev.actor`, `target`, `room`, results of queries. A handle is a
*validated* reference (§4), with methods:

```
h:id()  h:name()  h:short()                      -- identity
h:attr(name)  h:resource(name)  h:level()         -- queries
h:has_affect(id)  h:affect_magnitude(id)  h:has_flag(name)
h:is_enemy(other)  h:distance(other)  h:can_see(other)
h:send(markup)  h:act(tmpl, obj, vict, to)  h:say(text)  h:emote(text)
h:contents()  h:equipment()  h:room()  h:group()  -- traversal (returns handles)
-- effect ops (route through mitigation + the hostility gate, ABILITIES.md §3/§7):
h:damage{amount=, type=, can_avoid=}              -- to this handle as target
h:heal(resource, amount)   h:modify_resource(resource, delta)   h:drain(resource, amount, to)
h:apply_affect(id, {duration=, magnitude=, stacks=, source=})
h:remove_affect(id)   h:dispel{category=, count=}
h:move(dir)   h:teleport(room)   h:recall()
```

**`mud.*`** — world and utility:

```
mud.spawn(proto, room)   mud.transform(h, proto)   mud.summon(h)
mud.after(pulses, fn) -> handle    mud.cancel(handle)        -- zone-timer scheduling
mud.random()  mud.random(n)  mud.roll("2d6+1")               -- engine RNG (§9)
mud.now()     mud.pvp_allowed(a, b)                          -- queries
mud.scan(room)  mud.broadcast(room, markup)
mud.gmcp(h, package, data)                                   -- push a GMCP message
mud.log(level, msg)                                          -- structured log (print is redirected here)
```

**`on(event, fn)`** registers a trigger; **`self`** is the scripted entity in trigger/affect
context; **`ctx`** carries `actor`/`target(s)`/`room` in ability context.

## 4. Handles, not pointers

Lua never holds a `*Entity`. A handle wraps a `RuntimeID` (+ zone) as userdata; every method
call validates, in Go, that the entity **still exists and is in this zone** before acting.
This buys three things:

- **No dangling references** — an entity can die or be moved between script calls; a stale
  handle's methods become safe no-ops returning `nil`/`false`, not a crash.
- **No cross-zone reach** — a handle for an entity that left this zone is invalid here; any
  cross-zone interaction must go through engine-mediated events, preserving the single-writer
  invariant.
- **API/engine decoupling** — content depends on the handle API, not Go struct layout, so the
  engine can refactor internals without breaking content.

## 5. The sandbox

The VM is built with a **restricted global table**:

- **Removed:** `os`, `io`, `debug`, `package`/`require`, `dofile`, `loadfile`, `load`/
  `loadstring`, raw `collectgarbage`, and anything FFI-like. No filesystem, network, process,
  or code-loading reach.
- **Kept:** `string`, `table`, `math` (with `math.random` rebound to the engine RNG, §9),
  `pairs`/`ipairs`/`select`/`type`/`tostring`/`tonumber`/`pcall`/`error`/`assert`; `print`
  redirected to `mud.log`.
- **CPU quota:** every script invocation runs under an **instruction budget + wall-clock
  deadline** (via the LState context). Exceeding it aborts *that call* (caught by the engine's
  pcall), logs it, and counts against the script's error budget (§6). A runaway loop can't
  stall the zone.
- **Stack/recursion** capped (`SetMaxStackSize`); deep recursion errors out rather than
  blowing the goroutine stack.
- **Memory:** gopher-lua has no hard per-VM cap natively, so we bound it indirectly —
  instruction budget, table-size guards on script-writable state, and per-zone VM memory
  metrics with alerting. Noted as a known limitation, not a silent gap.

## 6. Error isolation & circuit breaker

Builder-authored scripts *will* have bugs; a live world must absorb them.

- Every entry point is invoked through `pcall`. An error (or budget abort) **fails just that
  action** — the ability fizzles with a generic message, the trigger is skipped — and is
  logged with `(zone, kind, ref, stack)`.
- Each script carries an **error budget**: repeated failures trip a **circuit breaker** that
  disables that script (not the zone), emits an ops alert, and leaves the rest of the world
  running. Re-enabled on the next successful hot-reload.

The invariant: **no script can crash a zone, stall a zone, or affect another zone.**

## 7. Script state & persistence

Scripts often need memory (a quest counter, whether a mob has greeted you).

- Each scripted entity exposes **`self.state`** — a plain Lua table that is mirrored to and
  from the entity's persisted `state` JSONB (PERSISTENCE.md §3/§4).
- **Data only** — numbers, strings, booleans, and nested tables of those. No functions,
  closures, userdata, or handles are persisted (store `h:id()`, re-resolve on load).
- The engine serializes `self.state` on the normal save cadence, so script state rides the
  same durability ladder as everything else.

## 8. Hot reload

- On a `(kind, ref)` content invalidation (PERSISTENCE.md §8), the engine recompiles that
  definition's chunk and swaps its registered handlers/functions for the new ones.
- `self.state` **data survives** (it's not code); in-flight `mud.after` callbacks bound to the
  old chunk are allowed to complete or are dropped by generation tag (configurable).
- This is what makes content iteration live — edit a mob's Lua, see it reload without a
  restart, consistent with the per-zone VM model in ARCHITECTURE.md §3.

## 9. Determinism

`mud.random`/`mud.roll` and the rebound `math.random` all draw from a **per-zone engine RNG**
(seedable). Combat, loot, and procs are therefore reproducible in tests and replays, and not
hostage to Lua's own RNG state. Scripts get no other entropy source.
