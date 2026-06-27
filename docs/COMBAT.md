# Combat model

Decided flavor: **round-based (Diku/ROM-derived), layered avoidance + soak resolution,
medium-depth affliction layer.** "ROM, refined." The engine provides the combat *framework*
(round driver, resolution pipeline, event emission, hook points); the *numbers and rules*
(to-hit curves, soak, skill and affliction tables) are data/Lua so designers tune without
recompiling.

---

## 1. The round

- Combat advances on `PULSE_VIOLENCE`, a fixed multiple of the base zone pulse
  (e.g. 24 × 100ms ≈ **2.4s/round**; tunable).
- Each **zone's heartbeat drives its own** violence pulse — no global lockstep, consistent
  with the actor-per-zone model. All fights in a zone resolve together on that pulse.
- On each violence pulse, every entity in the `Fighting` state resolves its attacks for the
  round.

## 2. Attacks per round

An entity gets **N swings/round** derived from `attacks` (weapon speed, haste affects, ROM-
style second/third/fourth-attack skills, dual-wield). A round is a sequence of swings; each
swing runs the resolution pipeline (§3). Off-hand and bonus attacks may carry penalties.

## 3. Swing resolution pipeline (layered avoidance + soak)

For each swing `attacker -> defender`:

1. **Gates** — position (must be ≥ resting to defend, fighting to attack), visibility,
   safe-room / sanctuary, immunities. Fail ⇒ swing aborts.
2. **To-hit** — attacker `accuracy/hitroll` vs defender `evasion`. Miss ⇒ swing ends (missed).
3. **Active avoidance ladder** — a would-be hit gets negated by the first that succeeds:
   - **Dodge** (DEX + skill)
   - **Parry** (requires a wielded weapon; skill)
   - **Block** (requires a shield; skill)
   Each is an independent gated chance; success ⇒ swing ends (avoided), emits its own message.
4. **Damage roll** — weapon dice + `damroll` + STR/skill/enchant bonuses; crit check.
5. **Soak / mitigation** — armor reduces by **damage type** (slash / pierce / bludgeon /
   magic / ...) as a blend of flat reduction and % resist; then resist / vuln / immune
   multipliers from affects and race.
6. **Apply** — subtract from hp; emit combat events (act() text + `OUT_COMBAT` output +
   GMCP); check death threshold.
7. **On-hit procs** — weapon effects, poison/affliction application (§6), riposte, lifesteal,
   etc. Lua-registerable.

The ordering matters: **to-hit first (did it land), then active avoidances (was it negated),
then soak (how much got through).** This produces a readable combat log and distinct GMCP
events per stage.

## 4. Stats that feed it

| Side    | Inputs                                                                        |
|---------|-------------------------------------------------------------------------------|
| Offense | accuracy/hitroll, damroll, attacks/round, crit chance, damage type, weapon dice |
| Defense | evasion, dodge / parry / block skills, armor-by-type, resist / vuln / immune    |

All derived from `CoreStats` (str/dex/con/int/wis/...) + skills + equipment + active affects.
Derivation is centralized so an affect or gear swap recomputes cleanly.

## 5. Skills & lag — the round-based "cooldowns"

- **Active combat skills** (bash, kick, disarm, backstab, trip, spells) are issued as
  commands. On use they resolve immediately but impose **skill lag** (`WAIT_STATE`): the
  actor can't act for X pulses afterward. This is the round-based analog of a GCD.
- **Per-skill cooldowns** — some skills are usable only every N rounds (independent of lag).
- **Command queue** — input received during lag is queued 1-deep (configurable) rather than
  dropped, so players can pre-enter their next action.

Both lag and cooldowns are first-class timers the **HUD renders** (§8).

## 6. Affliction / affect layer (medium)

Affects are timed modifiers with: duration (in pulses/rounds), stacking rule, dispellable
flag, application resist check, and an optional periodic tick.

- **DoTs** — poison, bleed, disease (tick on violence/affect pulse).
- **Crowd control** — stun (lose rounds), root (can't flee/move), silence (can't cast),
  slow (fewer attacks / longer lag), blind (accuracy penalty).
- **Buffs/debuffs** — stat and resist modifiers, sanctuary, haste, weaken, curse.
- **Curing** — dispels, cure spells, and natural decay. *Not* the full IRE herb/salve meta —
  that's a deliberate v1 boundary, leaving room to deepen later.

Hooks: `OnApplyAffect`, `OnAffectTick`, `OnAffectExpire`, `OnDispel`.

## 7. Death, targeting, groups

- **Death** — at hp ≤ threshold the entity dies: a **corpse** object is created holding
  inventory + equipment + gold; XP is awarded to the killer/group (threat-weighted split);
  player respawns at recall/temple and runs back for the corpse (classic). XP-loss / debt is
  a configurable ruleset knob.
- **Mob death** — loot-table roll into the corpse; the zone reset timer governs repop.
- **Targeting** — a single primary `Fighting` target; group members `assist` the tank.
- **Aggro** — mobs keep a threat list (damage + heal threat) to choose targets; aggressive
  mobs initiate on entry.
- **Area / multi-target** skills hit the room or the engaged group.

## 8. GMCP mapping (round-based still drives a rich HUD)

| GMCP message            | Feeds                                             |
|-------------------------|---------------------------------------------------|
| `Char.Vitals`           | self hp/mp/mv bars                                |
| `Char.Status`           | state=fighting, **round/lag remaining**, target  |
| `Mud.Target`            | target name, hp%, its afflictions -> target frame |
| `Mud.Cooldowns`         | skill lag + per-skill cooldowns -> action-bar dimming |
| `Char.Afflictions` (`Mud.*`) | active afflictions on self w/ durations -> debuff timers |
| `Mud.Group`             | party member vitals + afflictions -> party frames |
| `OUT_COMBAT` output     | combat log pane routing                          |

So the round/lag timers, target frame, debuff timers, and group frames all come straight out
of the round-based loop — the HUD ambition is fully intact.

## 9. Mudlib shape & hook points

The framework is engine code; rules are data/Lua. Hook points exposed to scripting:

```
OnBeforeSwing(attacker, defender, ctx)   -> may cancel/modify
OnToHit / OnAvoid / OnHit(attacker, defender, dmg, ctx)
OnDamage(target, amount, type, source)   -> mitigation/shields
OnDeath(victim, killer)
OnApplyAffect / OnAffectTick / OnAffectExpire
```

**Named interruptible checkpoints ([G9], Phase 6.4b).** The swing/cast/movement pipelines fire in-zone
events at named checkpoints content subscribes to via `on_event` (the event bus, event.go). The lit set:
`OnHit`/`OnDamageTaken` (a swing landed / a target took damage), `OnKill` (a target died), `OnLeaveRoom`
(an engaged foe is LEAVING a room — fired about each engaged reactor, the leaver bound as `other`, BEFORE
the leaver detaches so the harm gate sees live in-room entities), and `BeforeCastCommit` (an ability is
about to commit). v1 reactions are **declarative**: a handler may run a granted op-list + spend a
**per-round reaction resource** (`per_round: true`) — e.g. a mob's `OnLeaveRoom` opportunity attack on a
fleeing player, bounded to one/round so a second flee the same round provokes nothing. Reactions that
**alter the in-flight action** (Counterspell cancelling a cast, Shield adding AC after the roll) are the
Phase-7 Lua hatch: it only adds handlers at these same checkpoints — no pipeline surgery.

Formulas (to-hit, soak curve, attacks/round, crit) live in a tunable ruleset table so balance
changes don't require a recompile. Combat runs entirely inside the zone goroutine, so scripts
and procs see a consistent single-threaded fight.

## 10. Open / deferred

- Exact to-hit and soak **curves** (linear vs diminishing-returns) — tune during content work.
- PvP rules (consent, flagging, safe zones) — separate design pass.
- Whether casting uses the same lag model or a cast-time/interrupt model — leaning shared lag
  for v1; revisit if spellcasting needs interrupts.
