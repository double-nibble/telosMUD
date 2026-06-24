# Mudlib core model

The content-agnostic engine: how the world is represented (identity, entities, components,
containment) and how player input becomes action (command parser, targeting, messaging).
Everything else — skills, persistence, Lua API — references these shapes.

Status: **settled** — structural choices recorded in §8.

---

## 1. Identity

Three distinct kinds of id, on purpose:

| Id            | Type     | Scope / lifetime                        | Example                     |
|---------------|----------|-----------------------------------------|-----------------------------|
| `ProtoRef`    | string   | content key, authored, stable forever   | `midgaard:obj:longsword`    |
| `RuntimeID`   | uint64   | one running shard process; ephemeral    | `48213`                     |
| `PersistID`   | UUID     | durable identity for things we save     | players, unique/loaded items|

- **`ProtoRef`** identifies a *template* (prototype) in the content DB.
- **`RuntimeID`** is the cheap in-memory handle for live references (target pointers, aggro
  lists). Never persisted.
- **`PersistID`** is carried only by entities with durable state (every player; items flagged
  persistent — artifacts, player housing, mailed objects). Most spawned mobs/items have none.

## 2. Entity

Everything in the world — player, mob, item, room — is an `Entity`: an identity plus a bag of
optional **components**. This is "ECS-lite": composition over inheritance, but rich objects
(not data-oriented component arrays — MUD logic interacts with individual objects, not hot
loops over thousands of the same component).

```go
type Entity struct {
    rid      RuntimeID
    proto    ProtoRef          // template it was spawned from
    pid      *PersistID        // nil unless durable

    keywords []string          // targeting: {"long","sword","sharp"}
    short    string            // "a long sword"      (inline name)
    long     string            // "A long sword lies here."  (room line)

    location *Entity           // what holds this entity (room/container); nil for rooms
    contents []*Entity         // what this entity holds

    comps    componentSet      // optional capabilities (§3)
    zone     *Zone             // single-writer owner
}
```

Identity + containment are **core** (every entity has them). Capabilities are components.

## 3. Components

A component is a typed struct granting a capability. Access is generic and type-safe:

```go
type Component interface { componentKind() Kind }

func Get[T Component](e *Entity) (T, bool)   // lookup + assert
func Must[T Component](e *Entity) T           // panics if absent (use when invariant-guaranteed)
func Has[T Component](e *Entity) bool
func Add[T Component](e *Entity, c T)
```

Core capability set (content adds more via Lua-registered components):

| Component          | Grants                                                            |
|--------------------|------------------------------------------------------------------|
| `Physical`         | weight, size, material, condition/durability                     |
| `Living`           | hp/mp/mv (+max), `Position`, `CoreStats`, fighting target        |
| `PlayerControlled` | session link, account, aliases, prompt cfg, GMCP supports        |
| `Mob`              | AI state, aggro list, spawn ref, dialogue, shopkeeper flag       |
| `Room`             | exits, sector/environment, coords, room flags (safe/dark/indoor) |
| `Container`        | capacity, weight limit, open/closed/locked, key ref              |
| `Wearable`         | wear locations (worn/wield/hold)                                 |
| `Weapon`           | damage dice, damage type, weapon class, attack verb              |
| `Armor`            | armor values by damage type                                      |
| `Skilled`          | known skills/spells + proficiencies                              |
| `Affected`         | active affects (buffs/debuffs/DoTs) with durations               |
| `Scripted`         | attached Lua behaviors/triggers + opaque script state            |

`Scripted` is intentionally cross-cutting — a room, a mob, *and* a weapon can all carry
behavior. (See the composition matrix below.)

**Storage:** `componentSet` is a `map[reflect.Type]Component` behind the generic accessors.
The two near-universal hot components — `Living` and `Room` — are also held as direct typed
pointers on `Entity` so combat/movement never pay a map lookup. Promoting more components to
fields is a profiling-driven escape hatch, not a day-one concern.

## 4. World tree & containment

Containment is **uniform**: a room contains mobs/players/items; a container contains items; a
living being contains its inventory. One mechanism, `location`/`contents`, models all of it.

```go
func Move(e, dest *Entity)   // detach from e.location.contents, set location, append to dest
```

- A **Room** is just an `Entity` with a `Room` component and no `location` (its container is
  the zone). Its `contents` are the occupants and ground items.
- A **Zone** owns its rooms and runs the single goroutine that mutates everything inside it
  (the actor model). All `Move`s within a zone are plain slice ops — lock-free.
- **Intra-shard, cross-zone** move ⇒ channel handoff between zone goroutines.
- **Cross-shard** move ⇒ the `Handoff` protocol (see [PROTOCOL.md](PROTOCOL.md)).

**Concurrency rule:** entity mutation happens *only* inside the owning zone's goroutine.
Anything slow (DB load, cross-shard call) is done async and its result posted back to the zone
as an event — a command handler must never block the zone loop.

## 5. Prototypes & instancing

Content authors define **prototypes** (templates keyed by `ProtoRef`); the world spawns
**instances**. Proposed model (see §8 D1):

- **Immutable prototype** loaded once from the content DB and cached per shard: base
  descriptions, base stats, component template.
- **Instance = lightweight delta** over its prototype. Shared immutable fields (keywords,
  base descriptions, base stats) are *referenced* from the prototype; only fields that
  actually change on the instance (current hp, durability, enchants, contents) are stored
  locally. Copy-on-write when an instance first mutates a shared field.

This keeps a room full of 40 identical kobolds cheap, which matters at the stated scale. The
simpler alternative — deep-copy every field on spawn — is the §8 fork.

## 6. Command parser

Pipeline from a decoded `InputLine` (gate -> zone inbox) to action:

```
line
 └─ alias expansion (player-defined)
 └─ split -> (verb, rest)
 └─ resolve verb in actor's ACTIVE command table   (abbreviation-aware)
 └─ check Position / Level / flags  -> fail with the right message
 └─ cmd.Run(ctx)                     (targets resolved lazily inside)
 └─ if the command imposes lag, set WAIT_STATE (see COMBAT.md §5)
```

```go
type Command struct {
    Name    string        // canonical, e.g. "north"
    Aliases []string      // {"n"}
    MinPos  Position      // e.g. PosStanding to attack, PosResting to look
    Level   int           // min level / admin gate
    Flags   CmdFlag       // NoWhileFighting, Hidden, ...
    Run     func(*Context) error
}
```

- **Active command table** is a stack: base commands + skill-granted commands + mode tables
  (line editor, OLC builder, menus push their own table and pop on exit). The parser only
  resolves against the top-of-stack set plus the always-available base.
- **Abbreviation** resolves by: exact match wins; otherwise the highest-**priority** command
  whose name has the typed prefix. Movement and common verbs hold high priority so `n`->north,
  `k`->kill — never `nuke`. Implemented as a priority-ordered registry (optionally a trie).
- **Socials** (`smile`, `bow`) and **channels** (`gossip`) are checked after the command
  table; socials are data-driven act() templates, channels route over NATS.
- **Skills as commands** — active combat/utility skills register as commands that verify the
  actor has the skill, then resolve and apply lag (the round-based cooldown analog).

### Context
`Context` is what a command handler works through — it hides the zone/entity plumbing:

```go
ctx.Actor                      // *Entity
ctx.Arg(0), ctx.Rest()         // tokenized args
ctx.Target(scopes...) (*Entity, bool)   // resolve one target (§7)
ctx.Targets(scopes...) []*Entity        // resolve all / all.x
ctx.Send(markup)               // to the actor
ctx.Act(tmpl, actor, obj, vict, to)     // perspective messaging (§7)
ctx.Lag(pulses)                // impose WAIT_STATE
```

## 7. Targeting grammar & messaging

### Targeting (classic Diku grammar — chosen for genre familiarity)
```
sword            -> first visible match for keyword "sword"
2.sword          -> the 2nd matching "sword"
all.coin         -> every matching "coin"
all              -> everything in scope
long sword       -> match an entity whose keywords include both "long" and "sword"
```
- A multi-word target matches an entity when **every** typed word is a prefix of one of the
  entity's keywords (Diku `isname` semantics).
- **Scopes** are searched in a command-defined order. `get sword` searches the room floor;
  `drop sword` searches inventory; `look sword` searches room -> inventory -> equipment.

```go
type Scope int
const (
    ScopeInventory Scope = iota
    ScopeEquipment
    ScopeRoomLiving      // mobs/players in the room
    ScopeRoomItems       // items on the ground
    ScopeContainer       // an opened container's contents
)

func (z *Zone) Resolve(actor *Entity, spec TargetSpec, scopes ...Scope) []*Entity
```
Resolution always applies a **visibility filter** (dark rooms, invis, hidden) so you can't
target what you can't perceive.

### act() — perspective messaging
The Diku idiom every command leans on: one call, correct text for every observer.

```
ctx.Act("$n picks up $p.", actor, item, nil, ToRoom)
```
- `$n` actor, `$N` victim, `$p` object, `$P` second object, `$e/$s/$m` gender pronouns,
  `$t/$T` literal text args.
- The actor sees "You pick up a long sword."; everyone else sees "Kurt picks up a long
  sword."; an observer who can't see the actor sees "Someone picks up a long sword."
- Destinations: `ToActor`, `ToVictim`, `ToRoom`, `ToRoomExceptActor`, `ToGroup`.

Output flows through each entity's `Sink`: players -> the gate stream (as `Output` markup +
GMCP); mobs -> AI/Lua hooks; log channels -> admin tooling. Uniform `Send`.
