# Skill / ability framework

Built on the pillar in [PRINCIPLES.md](PRINCIPLES.md): the engine provides a fixed *execution
lifecycle* and a vocabulary of *effect ops*; every actual skill, spell, social-with-teeth, or
mob special is **content** (data + Lua) that composes those ops. The engine has never heard of
`fireball`.

Status: **proposal** — three choices flagged in §9.

---

## 1. The generic substrate

Before abilities, the things they manipulate — all content-defined, engine only knows the kind.

- **Attributes** — named values with a modifier stack: `base (race+class+level+alloc) -> flat
  mods (gear/affects) -> multipliers -> computed`. `attr(e, "strength")` returns the resolved
  value through the stack, cached and invalidated on change. Content declares which attributes
  exist and the derivation formulas. *Derived* attributes are just attributes whose base is a
  formula (`max_hp = con*10 + level*5`).
- **Resources** — named pools (`current`, `max`-as-derived-attribute, regen rule). A resource
  may be flagged `vital` with a depletion threshold + `on_depleted` hook (that's how "hp
  reaching 0 = death" is expressed *in content*, not Go).
- **Flags & tags** — open sets of named booleans/labels on entities and on abilities/affects.
  The engine stores and queries them; content gives them meaning (§6).
- **Damage types** — named, with a content-defined resist/vuln/immune matrix. `slash` and
  `fire` are not special to the engine.
- **Affects (status effects)** — content definitions (§5) the engine's `Affected` runtime
  ticks and expires.

## 2. Anatomy of an Ability

An ability definition is data with Lua hooks. Shape (concrete syntax TBD in §9 D1):

```
ability "fireball" {
  invocation  = { kind = "command", words = {"cast fireball", "fireball"} }
  targeting   = { mode = "enemy", scope = "room", range = 0, disposition = "harmful" }
  requires    = { known = "fireball", attr = {intellect = 12},
                  not_prevented = "cast", wielding_tag = "focus" }
  costs       = { resource = {mana = 30}, reagent = {"sulfur" = 1} }
  timing      = { cast_time = 0, lag = 12, cooldown = 0 }
  messages    = { actor = "You hurl a roaring fireball at $N!",
                  room  = "$n hurls a roaring fireball at $N!" }
  on_resolve  = <effect script>      -- declarative ops and/or Lua (§3, §9 D1)
}
```

Fields:
- **invocation** — `command` (becomes a verb in the actor's table once granted), `proc`
  (fires off an event, e.g. on-hit), or `passive` (always-on, usually just grants affects).
- **targeting** — `mode` (`self / ally / enemy / area / room / object / direction / none`),
  `scope`, `range`, and **`disposition`** (`helpful / harmful / neutral`). Disposition is what
  drives auto-validation and the PvP gate (§7).
- **requires** — declarative gates: known-skill, attribute thresholds, position,
  `not_prevented = <tag>` (CC check, §6), wielding/wearing tags, zone flags, etc.
- **costs** — resources, reagent items, hp, cooldown charges. Reserved on cast, paid on
  completion, refunded on interrupt.
- **timing** — `cast_time` (interruptible per a flag), `lag` (WAIT_STATE pulses — the
  round-based cooldown analog), `cooldown` (per-skill, rounds).
- **on_resolve** — the payload: a composition of effect ops (§3).

## 3. Effect ops — the engine's vocabulary

The fixed set of verbs `on_resolve` (and affect ticks, procs) compose. Each op that can harm
routes through the shared mitigation pipeline and the hostility gate (§7), so a spell and a
sword obey the same armor/resist/PvP rules.

| Category    | Ops                                                                            |
|-------------|--------------------------------------------------------------------------------|
| Damage      | `deal_damage(target, {amount, type, can_avoid, scaling})` -> runs combat soak/resist |
| Restore     | `heal(target, resource, amount)`, `restore(target, resource, amount)`           |
| Resource    | `modify_resource(target, resource, delta)`, `drain(target, resource, amount, to)` |
| Affects     | `apply_affect(target, id, {duration, magnitude, stacks, source})`, `remove_affect`, `dispel(target, {category, count})` |
| Attributes  | (via affects) `modify_attribute` — transient buffs/debuffs as affects            |
| Movement    | `move(target, dir)`, `teleport(target, room)`, `pull/push(target, n)`, `recall(target)` |
| Perception  | `scan(room) -> entities`, `reveal(target/room, what)`, `detect(target, category)`, `identify(item)` |
| World       | `spawn(proto, room)`, `summon(target)`, `transform(target, proto)`, `open/close/lock`           |
| Comms       | `act(template, actor, obj, vict, to)`, `send(target, markup)`, `gmcp(target, pkg, data)`        |
| Flow        | `if(cond, then, else)`, `for_each(list, fn)`, `chance(p, fn)`, `delay(pulses, fn)`              |

Queries available to Lua/conditions: `attr`, `resource`, `has_affect`, `affect_magnitude`,
`has_flag`, `los`, `distance`, `is_enemy`, `pvp_allowed`, `room_contents`, `group_members`,
`level`, `random`.

**This vocabulary is the entire contract.** New skills are new *compositions*; they never need
new engine ops unless a genuinely new *kind* of interaction appears (rare, and a deliberate
engine change).

## 4. Execution lifecycle (engine, fixed)

Every ability — player skill, mob special, item proc — runs the same pipeline, inside the
owning zone's goroutine:

```
1  invoke            (command / proc / passive trigger)
2  resolve targets   (targeting spec -> entities, visibility-filtered)
3  check requires    (skill known, attrs, position, not_prevented tag, wielding, zone flags)
4  hostility gate    (if disposition=harmful and target is a player -> PvP policy, §7)
5  reserve costs     (resources/reagents; fail -> abort with message)
6  cast time         (if >0: lockout, interruptible per flag; on interrupt -> refund)
7  commit            (pay costs, set lag/cooldown)
8  on_resolve        (run effect ops; each harmful op re-checks gate + routes mitigation)
9  emit              (act messages, GMCP: vitals/affects/cooldowns deltas)
10 events            (fire OnAbilityResolved, OnHit, OnApplyAffect... for procs/scripts to react)
```

Steps 1---7 are engine; step 8 is content; steps 9---10 are engine emitting + content reacting.

## 5. Affects as content

```
affect "poison" {
  category     = "poison"                 -- for dispel/cure targeting
  display      = { name = "Poisoned", gmcp_icon = "poison", color = "green" }
  stacking     = "stack"                   -- stack | refresh | extend | ignore
  max_stacks   = 5
  duration     = 30                        -- pulses, or a formula
  dispellable  = true
  prevents     = {}                        -- tags this affect blocks (§6); poison blocks none
  modifiers    = { {attr="strength", op="add", value=-2} }
  tick         = { interval = 6, on_tick = function(ctx)
                     ctx.deal_damage(ctx.target, {amount = 4 * ctx.stacks, type = "poison",
                                                  can_avoid = false, source = ctx.source})
                   end }
  on_apply     = function(ctx) ctx.act("$n turns a sickly green.", ctx.target, nil,nil, "room") end
  on_expire    = nil
  resist       = { vs = "constitution", formula = "..." }
}
```

The engine's `Affected` runtime owns duration/stacking/tick/expire and feeds `modifiers` into
attribute/resource derivation. DoTs are just affects whose `tick` calls `deal_damage`. CC is
just affects with `prevents` tags.

## 6. Crowd control without hardcoding — the tag model

The engine never knows "rooted" or "silenced." Instead:

- Every **ability/command declares tags**: `fireball` has `{"cast","verbal","somatic","fire"}`;
  `walk` has `{"move"}`.
- An **affect can declare `prevents = {tags}}`**: a `root` affect prevents `"move"`; `silence`
  prevents `"verbal"`; `disarm` prevents `"weapon"`.
- At lifecycle step 3, the engine checks: *does any active affect on the actor prevent any of
  this ability's tags?* If so, blocked, with the affect's block message.

New CC is pure content: define an affect, list the tags it prevents. The engine's gate is
unchanged. The same mechanism powers `requires.not_prevented`.

## 7. Hostility & the PvP gate

The thing you specifically called out — harmful effects on players gated by PvP. Made
first-class so content can't forget it:

- Ability `targeting.disposition` declares intent (`helpful/harmful/neutral`).
- **`pvp_allowed(actor, target)`** is an engine query backed by a **content-defined PvP
  policy** (consent flags, faction war, arena zones, level gaps, safe rooms — all data/Lua).
- The engine enforces it **automatically** at two points (defense in depth): lifecycle step 4
  (whole-ability gate) and inside every *harmful* effect op (step 8) — so even a creatively
  scripted ability can't damage/debuff a player where PvP is disallowed. Against mobs the gate
  is a no-op.
- Lua can also call `pvp_allowed` directly for custom branching.

This cleanly covers your enumerated cases:

| You asked for...                              | How it's expressed                                              |
|---------------------------------------------|----------------------------------------------------------------|
| cause damage                                | `deal_damage(enemy, ...)`                                         |
| investigate a room                          | `scan(room)`, `reveal(room, "hidden")`, `detect(...)` queries    |
| status ailment on another player            | `apply_affect(enemyPlayer, "blind", ...)` -> auto PvP-gated        |
| buff on self                                | `apply_affect(self, "haste", ...)` (disposition helpful)          |
| buff on another player (ally)               | `apply_affect(ally, "shield", ...)` (helpful -> no gate)           |
| debuff on an enemy (mob)                     | `apply_affect(mob, "weaken", ...)` (gate no-op vs mobs)           |
| debuff on enemy player **when PvP enabled** | `apply_affect(enemyPlayer, "weaken", ...)` -> permitted only if `pvp_allowed` |

## 8. Hooks & events (engine -> content reactions)

Content registers handlers; the engine fires them. This is how passives, procs, and reactive
abilities work without polling.

```
OnAbilityResolved(actor, ability, targets)
OnBeforeSwing / OnToHit / OnAvoid / OnHit(attacker, defender, ctx)   -- combat (COMBAT.md §9)
OnDamage(target, amount, type, source)        -- shields/absorbs intercept here
OnApplyAffect / OnAffectTick / OnAffectExpire / OnDispel
OnResourceDepleted(entity, resource)          -- e.g. content defines death here
OnDeath(victim, killer)
OnEnterRoom / OnLeaveRoom / OnGetItem / OnSay  -- world events (also drive triggers)
```

GMCP deltas (`Char.Vitals`, `Char.Afflictions`, `Mud.Cooldowns`, `Mud.Target`) are emitted by
the engine from these same events, so a content-defined affect automatically shows up as a
debuff timer on rich clients with no extra wiring.

## 9. Decisions (settled)

| # | Decision | Resolution | Rationale |
|---|----------|------------|-----------|
| D1 | **Effect authoring model** | Declarative op-list for the common case **+ Lua-function escape hatch** for complex logic; both usable within one ability's `on_resolve`. | Safe, toolable data for the 80%; full Lua power without a ceiling for the rest. |
| D2 | **PvP / hostility enforcement** | **Automatic** engine gate at the ability lifecycle *and* inside every harmful effect op, driven by a content-defined `pvp_allowed` policy. | Content cannot harm a player where PvP is disallowed — even via custom Lua. Can't-forget, defense in depth. |
| D3 | **Reference content pack** | Ship a replaceable "stdlib" pack of sample attributes/ruleset/affects/abilities **— cleanly strippable so the engine ships bare** (see §10). | Authors get a runnable starting point; deployments that want pure mechanism drop one module and the engine still boots. |

## 10. Stdlib packaging — bare-engine guarantee

The stdlib pack is **ordinary content with zero special privileges**, and the engine must run
to a live world with it entirely absent. Concretely:

- The pack lives in its own module/directory (e.g. `content/stdlib/`) loaded *only* through the
  normal content-loading path — no Go import from `internal/` into it, ever.
- **Boot-with-nothing:** the engine starts with zero attributes/resources/abilities defined.
  Loading content registers them; loading *no* content yields a bare but running server (you
  can connect, you just can't fight until content defines how). This is an engine invariant
  with a test that boots empty.
- **Strip = delete a directory** (or omit it from the content manifest). No code edits, no
  build tags required — though a build tag to exclude it from a binary is also offered.
- A `content-lint` step validates a pack references only the published effect-op vocabulary and
  registered names, so the stdlib stays honest about not reaching into engine internals.
