package world

import (
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
	return rand.Float64() < p
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
func runOps(c *effectCtx, ops []effectOp) {
	for i := range ops {
		op := &ops[i]
		h, ok := effectOpHandlers[op.kind]
		if !ok {
			c.z.log.Warn("effect op not understood", "op", op.kind, "actor", c.actor.short)
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
		if c.z.log.Enabled(nil, slog.LevelDebug) {
			c.z.log.Debug("effect op", "op", op.kind, "actor", c.actor.short,
				"target", targetShort(c.target), "disp", int(c.disp))
		}
		if err := h(c, op); err != nil {
			c.z.log.Debug("effect op error (skipped)", "op", op.kind, "err", err)
		}
		c.target = prev
	}
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

// dealDamage is the SHARED MITIGATION PIPELINE (docs/PHASE5-PLAN.md §1.6): the ONE function a spell's
// deal_damage op and (Phase 6) a sword swing both route through, so they obey the same armor/resist/
// PvP rules. The flow is:
//
//	guardHarmful (PvP)  ->  raw  ->  ×resist/vuln/immune (from the damage_type_def matrix)
//	  ->  −soak  ->  clamp at >=0  ->  subtract from the target's vital resource (clamp current at 0)
//
// Death / on_depleted is RESERVED (Phase 6): this never crosses 0 into a death path — it clamps the
// current at 0 and stops. Returns the damage actually applied (0 on a gated block / a non-living
// target). Single-writer: zone goroutine.
func dealDamage(c *effectCtx, target *Entity, raw float64, dmgType string) int {
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
	// clamps its current at 0 (resources.go); death/on_depleted is Phase 6.
	pool := vitalResource(target)
	if pool == "" {
		c.z.log.Debug("deal_damage: target has no vital resource; damage discarded", "target", target.short)
		return dmg
	}
	cur := resourceCurrent(target, pool)
	setResourceCurrent(target, pool, cur-dmg)
	c.z.log.Debug("deal_damage applied", "target", target.short, "type", dmgType,
		"raw", raw, "applied", dmg, "pool", pool, "from", cur, "to", resourceCurrent(target, pool))
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

// soak is the flat damage reduction a target's armor contributes (Phase 6 attaches the real armor
// component here). 0 this phase: the seam exists so deal_damage's pipeline is the single place both a
// spell and a weapon subtract armor, and Phase 6 fills it without touching any op.
func soak(target *Entity, dmgType string) float64 { return 0 }

// applyDebuff is the shared HARMFUL apply_affect path: a debuff (a harmful-disposition affect) on a
// target routes through guardHarmful before attaching, exactly like dealDamage. A helpful/neutral
// apply_affect (a buff on self/ally) does NOT go through here — it attaches directly. This is the
// second harmful op that funnels through the ONE guard, so the can't-bypass property covers debuffs
// too. Returns true if the affect was applied (false on a gated block). Single-writer: zone goroutine.
func applyDebuff(c *effectCtx, target *Entity, affectRef string, opts attachOpts) bool {
	if !guardHarmful(c, target) {
		return false
	}
	applyAffect(target, affectRef, opts)
	return true
}

// vitalResource returns the ref of the target's first content-defined VITAL resource (hp in the
// demo), or "" if none. The shared mitigation pipeline subtracts damage from it. Reads the registry.
func vitalResource(e *Entity) string {
	if e == nil || e.zone == nil {
		return ""
	}
	for ref, def := range e.zone.resourceDefs().table() {
		if def.vital {
			return ref
		}
	}
	return ""
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
