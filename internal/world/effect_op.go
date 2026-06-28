package world

import (
	"context"
	"log/slog"
	"math/rand"
)

// effect_op.go is the effect-op INTERPRETER (docs/ABILITIES.md §3, docs/PHASE5-PLAN.md §1.3) — the
// engine's fixed vocabulary of verbs that an ability's on_resolve (and an affect's on_tick) compose.
// It is modeled DIRECTLY on reset.go's data-op interpreter: a switch over op.kind where each op is a
// REGISTERED handler `func(*effectCtx, *effectOp) error`. Adding an op = registering a handler — no
// lifecycle change. The interpreter:
//
//   - runs in lifecycle STEP 8 on the zone goroutine (single-writer, like a command handler);
//   - NEVER blocks and never does DB I/O (an op that wants slow work would post to an inbox);
//   - logs+skips an unknown op (content-lint is the real gate, exactly like applyReset).
//
// # The security boundary (P5-D4, §7) — the can't-bypass property
//
// EVERY op that can harm a target funnels through ONE shared function, guardHarmful, BEFORE it
// touches the target's state. There is no copy-pasted gate per op: a harmful op physically cannot
// reach a protected player's resources/affects because the single shared damage/debuff entry point
// (dealDamage / applyDebuff) calls guardHarmful first and aborts cleanly on a deny. This is what
// survives a step-4 bypass (a hostile op invoked directly), a multi-op on_resolve, and a future Lua
// on_resolve (Phase 7) — the in-op layer is the one the security-auditor must trust. The lifecycle
// step-4 outer check (ability.go) is defense-in-depth on top of this, not a substitute for it.
//
// The on_resolve_lua column is READ-NOT-RUN this phase (reserved Phase 7); the AST op-list is the
// whole milestone.

// effectCtx is the per-resolution context an op handler works through (the §1.3 effectCtx). It
// carries the actor (who is resolving the ability/tick), the resolved target(s), the source (who
// the EFFECT is attributed to — the actor for a cast, the affect's applier for a DoT tick), the
// magnitude scale, and the ability's disposition (which decides whether an op is harmful). It also
// carries the rng so a `chance` op and dice are deterministic in tests. Constructed per resolution,
// never escapes the zone goroutine.
type effectCtx struct {
	z      *Zone
	actor  *Entity // the entity resolving the ability (the caster); the gate's "attacker" side
	source *Entity // who the effect is attributed to (actor for a cast; the affect's source for a DoT)
	target *Entity // the primary target (single-target ops); may equal actor for self ops
	other  *Entity // event-handler counterpart (the victim for OnHit, the foe for a contested check);
	//                bound by an op's `target: other` selector (runOps). nil outside an event handler.
	mag  float64 // magnitude scale (an affect's stacks*magnitude; 1 for a plain cast; event mag)
	disp abilityDisposition
	rng  *rand.Rand // deterministic-injectable; nil => the package default
	// depth is how many EVENT-fires deep this resolution is (event.go re-entrancy guard). A command/
	// cast/tick ctx is depth 0; fireEvent runs handlers at depth+1 and refuses past maxEventDepth.
	depth int
	// eventBudget is the SHARED remaining count of handler executions for the whole event cascade
	// rooted at one action (event.go WIDTH guard). nil outside a cascade; the first fireEvent allocates
	// it and every nested fireEvent threads the SAME pointer, so TOTAL work (depth × fan-out) is
	// bounded — not just recursion depth. A wide subscription set that the depth cap alone wouldn't stop
	// (N handlers each firing M events) can't starve the single-writer zone goroutine.
	eventBudget *int
	// swingIndex is the 0-based index of the current melee swing within a combat round ([G-H], combat.go).
	// The swing pipeline sets it per swing so a to-hit/damage formula can read `$swing.index` (resolved
	// in resolveCheckScope) — PF iterative attacks (-5/-10/-15 by swing) are authorable without it. It is
	// 0 outside the swing loop (a spell's deal_damage / a check unrelated to a swing reads $swing.index 0).
	swingIndex int
	// lastDamage is the integer damage the most recent dealDamage call applied through THIS ctx (6.5).
	// The swing path reads it for the combat message / threat instead of re-reading the victim's vital
	// after opDealDamage returns — that read-back corrupts when death/respawn fires inside dealDamage (a
	// player respawn restores full hp, so a `before - after` delta goes negative). 0 until dealDamage runs.
	lastDamage int
}

// rollChance returns true with probability p (clamped to [0,1]). Uses the ctx rng when present so a
// test can force the branch; otherwise the package default.
func (c *effectCtx) rollChance(p float64) bool {
	if p <= 0 {
		return false
	}
	if p >= 1 {
		return true
	}
	if c.rng != nil {
		return c.rng.Float64() < p
	}
	return rand.Float64() < p //nolint:gosec // seeded for gameplay determinism, not security
}

// effectOp is one parsed op of an op-list: its kind plus the decoded argument bag, plus the nested
// op-lists the flow ops (if/chance) branch into. Parsed once at content load (parseOpList) into this
// typed form so the hot path is a kind switch, never a re-parse. Mirrors content.ResetDTO's role.
type effectOp struct {
	kind string // the registered op name: "deal_damage", "heal", "apply_affect", "if", "chance", ...

	// Common argument fields (a flat bag — different ops read different fields, exactly like the
	// reset op's Op/Proto/Room/Max). Decoded from the op map at parse time.
	resource string  // resource ref (heal/restore/modify_resource)
	affect   string  // affect ref (apply_affect/remove_affect)
	dmgType  string  // damage type ref (deal_damage)
	amount   float64 // a flat amount (heal/damage/modify_resource delta)
	diceNum  int     // dice count (deal_damage <N>d<S>)
	diceSize int     // dice size
	// bonus and diceCount are the [G-A] FORMULA-damage extension: a scoped formula over $actor/$target/
	// $source attributes added to deal_damage's raw amount (a weapon's `+ $actor.damroll + str_bonus`),
	// and an optional scoped formula for the DICE COUNT (a level-scaled rider like `ceil(level/2)d6`).
	// nil => not used (the flat amount + literal NdS path). Evaluated via the check scoping (check.go).
	bonus     formulaNode
	diceCount formulaNode
	duration  int     // affect duration override (apply_affect)
	magnitude float64 // affect magnitude override (apply_affect)
	prob      float64 // probability (chance)
	text      string  // a message template (act/send)
	to        string  // act recipient set: "actor" | "room" | "victim"
	harmful   bool    // explicit per-op harmful flag (debuff apply_affect, drain); else inherits ctx.disp
	tgt       string  // per-op target selector for event handlers: "" (ctx.target) | "self" | "other"
	// area is the per-op AoE target-set selector ([G12], docs/PHASE6-PLAN.md §1.3). "" => single-target
	// (the degenerate 1-target case — ctx.target, unchanged). "room" / "room_and_adjacent" make runOps
	// LOOP this one op over every valid living target in the area, binding c.target to each so the op
	// (deal_damage / a per-target check save / apply_affect) runs — and funnels guardHarmful — once PER
	// target. An ability sets it from its targeting.area onto each top-level op at build time (so an op
	// authored area-scoped runs area-scoped; a nested band/then op without it stays single-target within
	// the looped per-target ctx). Same-zone-contained (areaTargets never dereferences a cross-zone room).
	area string

	// ifResource + ifResourceMin are the `if` flow op's RESOURCE-threshold condition ([G9] reaction
	// budget): `if <ifResource> >= <ifResourceMin>` over the CURRENT of the ctx subject's pool (the actor
	// by default; `target: other` selects the counterpart). This is what lets a content reaction guard on
	// "do I have a reaction left?" — `if reactions >= 1, then [spend + opportunity attack]`. Empty
	// ifResource leaves `if` on its has_affect condition (op.affect), unchanged.
	ifResource    string
	ifResourceMin float64

	// then/els are the nested branches for the flow ops (if/chance). Parsed recursively.
	then []effectOp
	els  []effectOp

	// check is the parsed check spec for the `check` flow op (nil for every other op). Its bands
	// carry their own nested op-lists (check.go), so a check recurses into runOps like if/chance.
	check *checkSpec
}

// effectOpHandler is a registered op implementation. It runs on the zone goroutine with full single-
// writer access and MUST NOT block. A harmful op routes through guardHarmful (via dealDamage /
// applyDebuff) before touching the target. Returning an error logs+continues (one bad op does not
// abort the whole list — content-lint is the real gate), exactly like applyReset skipping a bad op.
type effectOpHandler func(c *effectCtx, op *effectOp) error

// effectOpHandlers is the registered op table (the §1.3 vocabulary). It is built once at package init
// and then only read, so no lock is needed. Adding an op = registering a handler in init — no
// lifecycle change. The flow ops (if/chance) recurse back into runOps. (Populated in init rather than a
// composite literal to break the static initialization cycle: a flow/tick op transitively reaches back
// into runOps -> the table.)
var effectOpHandlers map[string]effectOpHandler

func init() {
	effectOpHandlers = map[string]effectOpHandler{
		"deal_damage":     opDealDamage,
		"heal":            opHeal,
		"restore":         opRestore,
		"modify_resource": opModifyResource,
		"apply_affect":    opApplyAffect,
		"remove_affect":   opRemoveAffect,
		"dispel":          opDispel,
		"act":             opAct,
		"send":            opSend,
		"if":              opIf,
		"chance":          opChance,
		"check":           opCheck,
	}
}

// runOps interprets an op-list in order (the step-8 entry point + the if/chance recursion). It walks
// the ops, dispatching each to its registered handler; an unknown op is logged+skipped (content-lint
// is the real gate). Single-writer: zone goroutine. Never blocks.
//
// CROSS-RESPAWN HAZARD (security S1, documented-not-fixed this slice): runOps holds c.target across
// SIBLING ops. With 6.5 uniform death, an op-list like `[deal_damage <lethal>, apply_affect rooted]` on
// one bound target now KILLS+respawns on op1 (the shared funnel runs death inside deal_damage) and then
// lands op2 (the debuff) on the RESPAWNED player — the same *Entity, fresh hp, at the start room. This is
// SAFE today: op2 still funnels guardHarmful (it re-validates the now-respawned target every op, so no
// crash and the PvP gate still holds), and there is no respawn-invulnerability window to violate yet. It
// is a LATENT grief vector once respawn-sickness / spawn-protection exists. The real fix — skip the
// remaining same-op-list ops on a target that died/respawned mid-list (track the target's pre-op
// position/identity and bail) — lands WITH respawn-sickness, not here. Not marking it now by design.
func runOps(c *effectCtx, ops []effectOp) {
	for i := range ops {
		op := &ops[i]
		h, ok := effectOpHandlers[op.kind]
		if !ok {
			c.z.log.Warn("effect op not understood", "op", op.kind, "actor", c.actor.short)
			continue
		}
		// AoE ([G12]): an area-scoped op LOOPS over the area's valid living targets, running the SAME op
		// once per target with c.target bound to each. This is the whole [G12] mechanism — the per-op
		// handler (and the guardHarmful it funnels) is UNCHANGED, just invoked N times, gated N times. A
		// non-area op (op.area == "") is the degenerate 1-target case below.
		if op.area != "" {
			runOpArea(c, op, h)
			continue
		}
		// Per-op target selection (event handlers bind self/other): "self" -> the subject, "other" ->
		// the counterpart. Empty leaves ctx.target untouched, so a normal ability op is unaffected. The
		// swap is restored after the op so sibling ops in the list see the original target.
		prev := c.target
		switch op.tgt {
		case "self":
			c.target = c.actor
		case "other":
			if c.other != nil {
				c.target = c.other
			}
		}
		if c.z.log.Enabled(context.Background(), slog.LevelDebug) {
			c.z.log.Debug("effect op", "op", op.kind, "actor", c.actor.short,
				"target", targetShort(c.target), "disp", int(c.disp))
		}
		if err := h(c, op); err != nil {
			c.z.log.Debug("effect op error (skipped)", "op", op.kind, "err", err)
		}
		c.target = prev
	}
}

// runOpArea runs ONE area-scoped op once per valid living target in the op's area ([G12]). It resolves
// the target set (areaTargets — same-zone-contained), then binds c.target to each and invokes the op
// handler. EACH invocation funnels the op's own gate (a harmful op's dealDamage/applyDebuff ->
// guardHarmful) INDEPENDENTLY, so an AoE is N gate checks, never one: a consenting foe is harmed, a
// non-consenting player in the same room is a clean no-op — per target. A per-target `check` save rolls
// once per target (resolveCheck reads the bound c.target). c.target is restored after the loop so a
// sibling op in the list (area or not) sees the original target. Single-writer: zone goroutine.
func runOpArea(c *effectCtx, op *effectOp, h effectOpHandler) {
	targets := areaTargets(c, op.area)
	prev := c.target
	for _, t := range targets {
		c.target = t
		if c.z.log.Enabled(context.Background(), slog.LevelDebug) {
			c.z.log.Debug("effect op (area)", "op", op.kind, "area", op.area,
				"actor", c.actor.short, "target", targetShort(t), "disp", int(c.disp))
		}
		if err := h(c, op); err != nil {
			c.z.log.Debug("effect op error (skipped)", "op", op.kind, "err", err)
		}
	}
	c.target = prev
}

// --- The shared mitigation pipeline + the PvP guard (the security boundary) -------------------

// guardHarmful is THE single hostility chokepoint every harmful op funnels through (P5-D4, §7). It
// returns true when the actor MAY harm the target, false when the PvP policy forbids it (a blocked
// harmful op is then a CLEAN no-op with a message — never a partial effect). Against a non-player
// target the gate is a NO-OP (PvP only). This is the in-op layer of defense-in-depth: even an op
// invoked directly (a step-4 bypass, a DoT tick, a future Lua on_resolve) cannot harm a protected
// player because dealDamage / applyDebuff call this BEFORE touching the target. There is exactly one
// such function — a new harmful-op author routes through dealDamage/applyDebuff and physically cannot
// forget the guard.
func guardHarmful(c *effectCtx, target *Entity) bool {
	if target == nil {
		return false
	}
	// FAIL-CLOSED on a DETACHED actor or target (reaped / handed off / mid-transfer). The PvP policy
	// reads room flags off both entities' locations (pvp.go inSafeRoom/inArenaRoom), so evaluating it
	// against a stale pointer could read a wrong room or race the owning goroutine — the fireOnTick
	// lesson, here made structural so it covers EVERY harmful op (event handlers, future Lua, direct
	// invokes), not just the affect tick. A harm op with no live, in-room actor+target is a clean no-op.
	if c.actor == nil || c.actor.living == nil || c.actor.location == nil ||
		target.living == nil || target.location == nil {
		if c.z != nil {
			c.z.log.Debug("guardHarmful: actor or target detached, harm denied",
				"actor", targetShort(c.actor), "target", targetShort(target))
		}
		return false
	}
	// PvP only: against a non-player (a mob) the gate is a no-op (always allowed).
	if !isPlayer(target) {
		return true
	}
	ok := pvpAllowed(c.actor, target)
	if !ok {
		c.z.log.Debug("pvp gate: harmful op blocked (in-op guard)",
			"actor", c.actor.short, "target", target.short)
		// Tell the actor cleanly; never a partial effect.
		if s, has := sessionOf(c.actor); has {
			s.send(textFrame("You cannot harm " + target.Name() + " here."))
		}
	}
	return ok
}

// guardCrossPlayerWrite is the ONE shared predicate for ops that WRITE another entity's state where
// harm is NOT structurally derivable from a damage/affect def — modify_resource (any sign: a content
// pool's polarity is unknown), dispel, remove_affect (stripping/altering a player's buffs). A
// self-write, an ally/mob write, or a write the gate permits proceeds; writing ANOTHER player routes
// the single guardHarmful. Returns true to proceed, false to cleanly no-op. Funnelling all three (and
// any future such op, incl. a Lua-exposed one) through here means none can forget the !=actor self-
// exception or the isPlayer test (the security-auditor's can't-forget property for the non-damage
// cross-player writes — the damage/debuff paths already funnel dealDamage/applyDebuff).
func guardCrossPlayerWrite(c *effectCtx, target *Entity) bool {
	if target == nil {
		return false
	}
	if target == c.actor || !isPlayer(target) {
		return true // self-write or a mob/ally write — not a cross-player harm vector
	}
	return guardHarmful(c, target)
}

// dealDamage is the SHARED MITIGATION + DEATH PIPELINE (docs/PHASE5-PLAN.md §1.6, docs/COMBAT.md §3):
// the ONE function a spell's deal_damage op, a sword swing, an AoE, a DoT tick, and an opportunity
// attack all route through, so they obey the same armor/resist/PvP rules AND the same death seam. The
// flow is:
//
//	guardHarmful (PvP)  ->  raw  ->  ×resist/vuln/immune (from the damage_type_def matrix)
//	  ->  −soak  ->  clamp at >=0  ->  subtract from the target's vital resource (clamp current at 0)
//	  ->  threat  ->  OnDamageTaken (all damage)  ->  OnHit (attributable hit)  ->  depletion checkpoint
//
// 6.5 — UNIFORM DEATH: the depletion->death seam lives HERE, not in the swing path, so EVERY content
// damage source kills through one path (a pure-caster's fireball/DoT can now land a killing blow). When
// the applied damage empties the target's vital pool, onVitalDepleted runs the content on_depleted hook
// (which can CANCEL the death by reviving the victim — death.go) then die().
//
// BOUNDING (security M1): the death seam is NOT reached through fireEvent, so it does not inherit the
// bus's depth/width guard for free. onVitalDepleted->runDeathHook (death.go) does the bounding EXPLICITLY
// at the seam: it INCREMENTS c.depth per hook level and DECREMENTS the shared eventBudget when present,
// refusing the hook past maxEventDepth. So a recursive on_depleted (`deal_damage self`, or a `tgt: other`
// ping-pong) that loops back through this checkpoint terminates at maxEventDepth instead of overflowing
// the stack. depth alone bounds it (a command-issued cast reaches here with eventBudget==nil).
//
// ORDERING (LOAD-BEARING — do not reorder): apply -> threat -> OnDamageTaken -> OnHit -> depletion
// checkpoint.
//   - OnDamageTaken (about the victim) fires for ALL damage incl. a sourceless DoT, BEFORE the checkpoint,
//     so a thorns reflect / "below-20% hp" reaction lands on the SAME blow that then kills. A handler here
//     MAY kill or respawn an entity (a reflect that kills the attacker) — which is exactly why the
//     checkpoint below RE-READS current state instead of trusting a pre-fire snapshot, and why the OnHit
//     fire re-checks src liveness (SC1): reordering these would reopen a double-fire / dead-subject window.
//   - OnHit (about the attacker — lifesteal/rage) fires only when c.source is an attributable attacker
//     that is not the victim (a self/ambient DoT doesn't spuriously "hit"), AND only if that attacker is
//     still alive/in-room after OnDamageTaken (SC1 liveness re-check — a reflect may have killed it).
//   - The depletion checkpoint precedes nothing combat-event-wise (it is last), so OnDamageTaken/OnHit
//     always precede death.
//
// The swing path no longer re-reads the vital to compute the applied amount (that read corrupts if death/
// respawn fires mid-call); it reads the returned int / c.lastDamage instead. Returns the damage actually
// applied (0 on a gated block / a non-living target). Single-writer: zone goroutine.
func dealDamage(c *effectCtx, target *Entity, raw float64, dmgType string) int {
	c.lastDamage = 0
	if target == nil || target.living == nil {
		return 0
	}
	// THE GATE, first — before any state is touched. A blocked harmful op is a clean no-op.
	if !guardHarmful(c, target) {
		return 0
	}
	dmg := mitigate(target, raw, dmgType)
	if dmg <= 0 {
		c.z.log.Debug("deal_damage fully mitigated", "target", target.short, "type", dmgType, "raw", raw)
		return 0
	}
	// Apply to the target's vital resource pool (the first vital resource — hp in the demo). The pool
	// clamps its current at 0 (resources.go); the depletion checkpoint below turns a 0-crossing into death.
	pool := vitalResource(target)
	if pool == "" {
		c.z.log.Debug("deal_damage: target has no vital resource; damage discarded", "target", target.short)
		c.lastDamage = dmg
		return dmg
	}
	cur := resourceCurrent(target, pool)
	setResourceCurrent(target, pool, cur-dmg)
	c.lastDamage = dmg
	c.z.log.Debug("deal_damage applied", "target", target.short, "type", dmgType,
		"raw", raw, "applied", dmg, "pool", pool, "from", cur, "to", resourceCurrent(target, pool))

	// --- Threat + the lit combat events, now UNIFORM across all damage (moved out of the swing path so a
	// spell/AoE/DoT builds threat and triggers reactions identically to a melee swing). source is the
	// attacker the damage is attributed to (the swing attacker; a DoT's applier; nil for a sourceless
	// environmental hit). A self/ambient DoT (source == target) is NOT an attacker — no threat, no OnHit.
	src := c.source
	attributable := src != nil && src != target
	if attributable {
		// Threat the attacker built on the target — accrued BEFORE the death scrub (die() clears the
		// table) so a killing blow is still attributed. A non-living/zero amount is a no-op (addThreat).
		addThreat(target, src, float64(dmg))
	}
	// OnDamageTaken is ABOUT the defender and fires for ALL damage (a DoT must be able to trigger a
	// thorns reflect / a "below 20% hp" reaction). other = the attacker (nil for a sourceless hit). It
	// runs BEFORE the depletion checkpoint so a thorns reflect lands on the same blow that then kills.
	c.z.fireEvent(c, evOnDamageTaken, target, src, float64(dmg))
	// OnHit is ABOUT the attacker (lifesteal/rage). Only an attributable attacker "hits" — a self/ambient
	// DoT does not. This unifies OnHit across swings AND offensive spells (both carry source = caster).
	// SC1 liveness re-check: OnDamageTaken above may have KILLED/respawned src (a thorns reflect that fells
	// the attacker), so re-validate src is alive + in-room before firing OnHit — never proc on a dead mob
	// or a just-respawned full-hp player. (src is captured pre-OnDamageTaken; this re-reads its state.)
	if attributable && src.living != nil && src.location != nil && position(src) != posDead {
		c.z.fireEvent(c, evOnHit, src, target, float64(dmg))
	}

	// --- Depletion checkpoint (6.5, the uniform death seam): the applied damage emptied the vital pool ->
	// run the content on_depleted hook (which may CANCEL death by reviving the victim) then die(). source
	// is the killer attribution (nil for a sourceless death — no OnKill subject). Idempotent via the
	// posDead latch (onVitalDepleted/die). Shares this ctx's budget/depth so a death inside an event
	// cascade stays bounded. resourceCurrent already clamped at 0; <= 0 is the depleted test.
	if resourceCurrent(target, pool) <= 0 {
		c.z.onVitalDepleted(target, src, c)
	}
	return dmg
}

// mitigate runs the raw damage through the damage_type_def's resist/vuln/immune matrix and the
// target's soak. The matrix maps a damage-type/category ref to a multiplier the type's def declares
// (1 neutral, <1 resist, >1 vuln, 0 immune); we read the entry keyed by the damage type's OWN ref
// (the demo's slash:0.9 is "slash takes 0.9× slash"). Soak is a flat reduction (0 this phase — the
// armor component lands in Phase 6; the hook is here so a spell and a sword share the same math).
// Returns the final integer damage, floored at 0. Pure read of zone-owned + registry state.
func mitigate(target *Entity, raw float64, dmgType string) int {
	v := raw
	if c := target.zone; c != nil {
		if def := c.damageTypeDefs().get(dmgType); def != nil && def.resist != nil {
			if mult, ok := def.resist[dmgType]; ok {
				v *= mult
			}
		}
	}
	v -= soak(target, dmgType)
	if v < 0 {
		v = 0
	}
	return int(v)
}

// soak is the FLAT damage reduction a target's armor contributes, by damage TYPE ([G-B], docs/COMBAT.md
// §3 step-5). It reads a CONTENT-DERIVED attribute named `soak_<dmgType>` (e.g. `soak_slash`) — armor
// pieces feed it through the modSource mod-stack (a plate body adds +3 soak_slash), so a sword swing and
// a spell's deal_damage subtract the same by-type armor through this ONE seam. A ruleset that defines no
// such attribute (5e: armor is AC, not flat reduction) reads 0 here — the no-op case the gap analysis
// confirmed degenerates cleanly. Negative soak (a vulnerability armor) is clamped to 0: soak only ever
// REDUCES; an amplifier belongs in the resist/vuln matrix (mitigate), not here. Zone-goroutine read.
func soak(target *Entity, dmgType string) float64 {
	if target == nil || target.living == nil || dmgType == "" {
		return 0
	}
	v := attr(target, "soak_"+dmgType)
	if v < 0 {
		return 0
	}
	return v
}

// applyDebuff is the shared HARMFUL apply_affect path: a debuff (a harmful-disposition affect) on a
// target routes through guardHarmful before attaching, exactly like dealDamage. A helpful/neutral
// apply_affect (a buff on self/ally) does NOT go through here — it attaches directly. This is the
// second harmful op that funnels through the ONE guard, so the can't-bypass property covers debuffs
// too. Returns true if the affect was applied (false on a gated block). Single-writer: zone goroutine.
func applyDebuff(c *effectCtx, target *Entity, affectRef string, opts attachOpts) bool {
	if !guardHarmful(c, target) {
		return false
	}
	applyAffect(target, affectRef, opts, c) // thread the cascade ctx (bounds a nested OnApplyAffect)
	return true
}

// vitalResource returns the ref of the target's content-defined VITAL resource (hp in the demo), or ""
// if none. The shared mitigation pipeline subtracts damage from it. The pick is DETERMINISTIC — the
// lowest ref by sort order among the vital resources — so a pack that (against the convention) defines
// more than one vital still resolves the same pool every time, not whichever Go's randomized map
// iteration yields first. Content convention is ONE vital; lintVitalResources warns at load on >1.
// Reads the registry (lock-free atomic.Load).
//
// MULTI-VITAL UNSUPPORTED (latent flag, NOT this slice): the 6.5 death seam damages and tests depletion
// against THIS single lowest-ref pool (dealDamage's checkpoint) while the cancel re-check uses
// vitalCurrent — both collapse to the same lowest-ref vital here, so they cannot disagree TODAY. But if
// content ever marks two resources `vital`, only the lowest-ref pool damages/kills and the other is dead
// config (a target could sit at 0 in a higher-ref vital and never die). Multi-vital is unsupported; a
// real "death when ANY/ALL vitals deplete" policy is a future design decision, not a bug here.
func vitalResource(e *Entity) string {
	if e == nil || e.zone == nil {
		return ""
	}
	chosen := ""
	for ref, def := range e.zone.resourceDefs().table() {
		if def.vital && (chosen == "" || ref < chosen) {
			chosen = ref
		}
	}
	return chosen
}

// isPlayer reports whether e is a player-controlled entity (the gate's "is this a PvP target?"
// predicate). A mob has no PlayerControlled component, so the gate is a no-op against it.
func isPlayer(e *Entity) bool { return e != nil && Has[*PlayerControlled](e) }

// targetShort is a nil-safe name for logging.
func targetShort(e *Entity) string {
	if e == nil {
		return ""
	}
	return e.short
}
