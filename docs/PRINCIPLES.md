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
