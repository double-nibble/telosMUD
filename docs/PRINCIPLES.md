# Design principles

## Pillar: the engine is mechanism, content is flavor

**Nothing about the game's flavor is hardcoded.** No class, race, room, skill,
spell, affect, attribute, resource, damage type, or PvP rule exists in the
driver. The engine provides generic, named, composable primitives; **all flavor
is defined in content** (data + Lua).

The engine knows *kinds*; content defines *instances*:

| The engine knows the **kind**... | ...content defines the **instances** |
|--------------------------------|-------------------------------------|
| "an attribute exists"          | `strength`, `qi`, `sanity`          |
| "a resource exists"            | `hp`, `mana`, `stamina`, `blood`    |
| "a damage type exists"         | `slash`, `fire`, `holy`, `psychic`  |
| "an affect exists"             | `poison`, `haste`, `stun`, `curse`  |
| "an ability exists"            | `fireball`, `backstab`, `mend`      |
| "a character template exists"  | classes, races, factions            |

### Engine (Go, fixed) vs content (data + Lua, open)

- **Engine:** entity/component/containment, the attribute / resource / affect runtimes, the
  combat pipeline *shape*, the ability execution *lifecycle*, the event bus, targeting/scoping,
  the **effect-op vocabulary**, persistence, networking. The verbs and the stage machinery.
- **Content:** which attributes/resources/damage types/flags/affects exist; every ability,
  class, race, room, mob, item; every *formula* (to-hit, soak, regen, xp, derived stats); the
  PvP policy. The nouns and the rules.

### The litmus test
> A builder can add a new class with a brand-new resource ("Sanity") and a skill that drains
> an enemy's Sanity and applies a fear affect — **without recompiling the server.**

If a proposed engine change would make that *false*, it's the wrong change.

## Corollaries (how this shows up in the code)

1. **Named, not enumerated.** Identifiers are strings resolved against content registries at
   load time — never Go `enum`/`const`. There is no `const StrengthAttr` anywhere.
2. **Formulas are content.** Combat math and stat derivation live in a ruleset (data/Lua),
   not in Go. The engine runs the *pipeline*; content supplies the *numbers*.
3. **Engine reactions are tag-driven.** The engine never writes `if affect == "rooted"`. It
   asks generic questions content wires up — "does any active affect prevent the `move`
   tag?" — so new CC types need no engine change.
4. **Replaceable standard library.** The engine may ship a reference content pack (default
   attributes, a sample ruleset, common affects) — but it is *ordinary content*, fully
   overridable, never privileged or compiled in. Starting from "something" ≠ hardcoding.

This pillar is referenced by [ABILITIES.md](ABILITIES.md), [COMBAT.md](COMBAT.md), and
[MUDLIB.md](MUDLIB.md); when those docs say "content-defined," this is why.

## Pillar: every action and event is hookable

**If the engine does something, content can hook it.** Every meaningful action the
engine takes and every state transition it makes emits a *named event* content can
subscribe to — and the consequential ones offer a *before-checkpoint* content can
alter or veto. The engine performs the action; **content decides what it means and
what else happens.** Where Pillar 1 says content defines the nouns and the numbers,
this pillar says content defines the **consequences and reactions** — together they
let a builder compose *any* system out of generic primitives plus hooks into
everything those primitives do.

The engine fires the event; content supplies the reaction:

| The engine performs the action...        | ...content hooks the consequence                          |
|------------------------------------------|-----------------------------------------------------------|
| a swing lands                            | `OnHit` → lifesteal, rage build, on-hit poison            |
| an entity takes damage                   | `OnDamageTaken` → thorns, a "bloodied below 50%" trigger  |
| an entity's vital empties                | `on_depleted` → death, *or* a death-ward that cancels it  |
| an actor enters a room                   | `OnEnter` → aggro, traps, a greeter, zone ambiance        |
| an actor leaves a room                   | `OnLeaveRoom` → opportunity attacks, a farewell           |
| an ability is about to commit            | `BeforeCastCommit` → counterspell, an interrupt, a surcharge |
| an affect attaches / ticks / expires     | `OnApplyAffect` / `OnAffectTick` / `OnAffectExpire`       |
| a character logs in / rests / levels     | `OnLogin` / `OnRest` / `OnLevelUp`                        |

**Two kinds of hook.** *Observe-and-react* (after): the action already happened, the
hook adds consequences — most hooks, and they can't rewrite the past. *Checkpoint*
(before): the action is pending, the hook runs first and the engine reads the
resulting state to decide whether/how to proceed — the **death checkpoint is the
reference implementation** (the `on_depleted` hook revives the victim → the engine
re-reads the vital → death cancels; see [COMBAT.md](COMBAT.md)). Counterspell,
parry windows, and interrupts
all ride this same observe-then-recheck shape; the engine never hardcodes the
outcome, it exposes the seam and reads what content did.

### The litmus test
> A builder can add a **sailing** system — ships, docks, tides — entirely in content:
> subscribe to room-enter to board a vessel, fire their *own* `OnShipDock` event that
> their quests hook, and add a checkpoint that refuses disembarking mid-voyage —
> **without recompiling the server.**

If the engine can't express that — because some action emits no event, or because
builders can't define and fire their own events — the bus is incomplete.

## Corollaries (hookability)

5. **Comprehensive, not curated-to-taste.** The engine's job is to emit an event at
   *every* action and transition, not to guess which ones builders will want. A
   missing hook is an *engine bug*, not a content limitation. (A few `eventKind`s in
   [WORLD-EVENTS.md](WORLD-EVENTS.md) are named-and-reserved but not yet fired — the
   affect-lifecycle hooks; a subscription authored against them parses now.)
6. **Builders define their own events.** A system the engine never imagined needs
   builder-named events content *fires* and *subscribes to* — the bus is not limited to
   the engine's enumerated kinds. Content-namespaced custom events ride the `<pack>:Name`
   lane (e.g. `sailing:OnShipDock`), namespaced by pack so two packs' same-named events
   never collide, and validated distinctly from the engine's closed bare-name set.
7. **Hooks are bounded and gated.** Universal hookability is universal attack
   surface, so every hook runs under the shared depth/width budget (a recursive hook
   *terminates*, never overflows) and any harmful op inside re-funnels the PvP/hostility
   gate. Lua hooks additionally run in the sandbox (instruction budget, curated API).
   Power, without a foot-gun.
8. **Hooks are the glue between systems.** Loot, crafting, progression, and quests
   get no bespoke engine wiring — they are content subscribing to the same events
   combat, movement, and abilities already emit. Events are the integration layer.

Realized by the in-zone event bus ([internal/world/event.go](../internal/world/event.go),
[WORLD-EVENTS.md](WORLD-EVENTS.md)) and the ability/affect `on_event` hooks; the Lua
runtime makes the hook *bodies* arbitrary content.
