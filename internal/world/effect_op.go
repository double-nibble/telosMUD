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
	// arg is the raw target argument the command-invocation carried (the verb's tail), threaded from
	// castAbility so a NAME-RESOLVING op (craft_recipe with no fixed ref — `craft <name>`, #34) can
	// resolve its subject from what the player typed. "" for event/tick/AoE ctx (no invocation tail).
	arg string
	// suppressSkillUse lets an op that REFUSED (did no work) cancel the ability's step-10 OnSkillUse fire
	// (#38 slice B): a salvage/craft that was gated (skill too low, wrong tag, blocked…) must NOT advance the
	// very skill that gates it, else a player trains past the gate by spamming a refused action. The op sets
	// it true up front and clears it only on the success path; commitAbility skips OnSkillUse when it stays set.
	suppressSkillUse bool
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
	// credit is the attacker to ATTRIBUTE a self-directed blow to (#407). It is set only on a depletion ctx
	// (death.go), and dealDamage consults it only when a blow's source and target are the SAME entity —
	// i.e. the carry-over shape, a hook dealing the overflow at the entity whose pool just emptied. Without
	// it that blow is self-attributed and a kill through it credits nobody: no OnKill subject, no XP, no
	// corpse loot-ownership window. It is deliberately NOT `source`: rebinding source would break the
	// mirror case (a `tgt: other` retaliation) and would silently re-own affects and `$source.*` scope.
	credit *Entity
	// depletion is the arithmetic of the depletion this ctx was built for (#407) — how much of the blow the
	// pool absorbed and how much overflowed past 0. Set ONLY on a depletionCtx (death.go), so it is scoped
	// to exactly one hook execution and cannot bleed into the swing/cast ctx that outlives the checkpoint.
	// A content formula reads it as `$depletion.overflow` / `.applied` / `.amount` (check.go); the zero
	// value everywhere else makes a stray reference read 0 rather than an entity attribute.
	depletion depletion
	// sourcelessAmbient marks a ctx whose actor==target is an ARTIFACT of a sourceless ambient room field
	// (landRoomAffectOn sets effSrc = occ when the field has no applier), NOT a genuine self-directed op.
	// guardHarmful treats actor==target as self-harm (exempt from the spawn-protection window, #394) — the
	// right rule for a player's own self-effect, but wrong for an ambient lava/gas field that should still
	// honor a just-respawned occupant's window (#397 item 1). This flag lets the ENFORCEMENT side of the
	// window fire even when actor==target, while the CANCELLATION side stays actor!=target-gated (an
	// ambient field the victim did not summon must never drop the victim's own shield). false everywhere
	// except the sourceless branch of landRoomAffectOn, so a real self-directed op is unaffected.
	sourcelessAmbient bool
	// reactACBonus is a TRANSIENT, swing-scoped defender-AC bump recorded by a to-hit REACTION (7.9,
	// Shield: rx:modify("ac", +delta)). The swing pipeline (combat.go) sets it on this per-swing ctx
	// from the reaction's "ac" delta BEFORE the to-hit check; resolveCheck (check.go) adds it to the
	// to-hit DC (the DC IS the defender's AC) so the bump re-classifies hit/miss for THIS swing only and
	// never persists on the defender. 0 outside the swing's to-hit path (every other check ignores it).
	reactACBonus float64
	// commsCoalescing + commsDirty coalesce the mid-session comms republish across an op-list cascade (#77).
	// A grant op that crosses a channel's hear-access predicate (set_flag / clear_flag / modify_attribute_base)
	// marks its target dirty here instead of republishing inline; the OUTERMOST runOps flushes ONE republish
	// per DISTINCT target at op-list end. Without this a bundle of M same-target grants published the player's
	// config M times, and an AoE grant published once per (op × target) — membus Publish is O(subscriptions)
	// under a global lock on the zone goroutine, so a large grant bundle could stall it. commsCoalescing is
	// set only while inside a runOps cascade; a grant op run outside one republishes immediately (markCommsDirty).
	//
	// SCOPE: dedup is per ctx's op-list TREE (the outermost runOps + its if/chance/bundle/area nesting). A
	// FIRED EVENT handler runs under a FRESH ctx (event.go) and flushes independently, so a target granted in
	// both the outer list AND a fired handler is republished once per window — correct (republish is
	// idempotent), just not globally deduped. Affect apply/expire still republish inline (affect_runtime.go).
	commsCoalescing bool
	commsDirty      []*Entity

	// diedInCascade is the #69 cross-respawn dead-set: every entity that DIED while this ctx's op-list
	// tree was resolving. An op whose bound target is in here is skipped — it was aimed at someone who
	// has since been corpsed (a mob) or respawned across the world (a player), and landing it is the
	// "your debuff follows you to the temple" bug.
	//
	// It lives on the CTX, not on a runOps frame, for the same reason commsDirty does: the flow ops
	// (if/chance/check) and the area loop all recurse on THIS ctx and freely rebind c.target — so a
	// nested frame can kill `other` while the parent frame is watching only its own bound `target`.
	// A per-frame set cannot see that; a cascade-scoped set can. Populated by whichever frame observes
	// the death (runOps around each op, runOpArea around each victim) and read by every frame.
	// nil until something actually dies, so a death-free cascade — nearly all of them — allocates nothing.
	diedInCascade map[*Entity]bool
}

// markDied records that `e` died while this ctx's op-list tree was resolving, so every later op in the
// cascade that binds it as a target is skipped (#69). Allocates the set on first death.
func (c *effectCtx) markDied(e *Entity, opKind string) {
	if e == nil {
		return
	}
	if c.diedInCascade == nil {
		c.diedInCascade = map[*Entity]bool{}
	}
	c.diedInCascade[e] = true
	c.z.log.Debug("op killed its target; remaining ops on it are skipped (#69)",
		"op", opKind, "actor", c.actor.short, "target", targetShort(e))
}

// diedThisCascade reports whether an earlier op in this ctx's op-list tree already killed e (#69).
func (c *effectCtx) diedThisCascade(e *Entity) bool {
	return e != nil && c.diedInCascade[e]
}

// markCommsDirty records that e's comms config needs a mid-session republish (#77). Inside a runOps cascade
// (commsCoalescing) it dedups e into commsDirty for a single flush at the cascade's end; outside a cascade it
// republishes immediately, since there is no op-list boundary to coalesce to. nil-safe.
func (c *effectCtx) markCommsDirty(e *Entity) {
	if e == nil {
		return
	}
	if !c.commsCoalescing {
		c.z.republishCommsOnAccessChange(e)
		return
	}
	for _, x := range c.commsDirty {
		if x == e {
			return // already marked; the flush republishes it once
		}
	}
	c.commsDirty = append(c.commsDirty, e)
}

// flushCommsDirty ends the coalescing window and republishes each distinct target's comms config exactly once
// (#77). Called via defer by the outermost runOps. Order across distinct targets does not matter (each is an
// independent per-player config publish). republishCommsOnAccessChange re-guards (player + a hear-gating
// channel), so an entity that quit mid-cascade is a safe no-op.
func (c *effectCtx) flushCommsDirty() {
	c.commsCoalescing = false
	dirty := c.commsDirty
	c.commsDirty = nil
	for _, e := range dirty {
		c.z.republishCommsOnAccessChange(e)
	}
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
	resource   string  // resource ref (heal/restore/modify_resource)
	affect     string  // affect ref (apply_affect/remove_affect)
	dmgType    string  // damage type ref (deal_damage)
	attr       string  // attribute ref (modify_attribute_base — Phase 11.1 grant op)
	flag       string  // named flag (set_flag/clear_flag — Phase 11.1 grant op)
	track      string  // track ref (grant_track/advance_track — Phase 11.2 progression op)
	ability    string  // ability ref (grant_ability/revoke_ability — Phase 11.4a grant op)
	bundle     string  // bundle ref (apply_bundle — Phase 11.4b)
	item       string  // item prototype ref (consume_item/produce_item/augment_item — Phase 13.3 crafting ops)
	bind       string  // produce_item bind override ("bound" forces the produced item bound) — Phase 13.3
	profession string  // profession ref (learn_profession — Phase 13.3 trade membership)
	table      string  // loot/salvage table ref (salvage_item — Phase 13.4 deconstruction)
	tag        string  // required item tag gate (salvage_item object-target — #38; "" = no tag gate)
	skill      string  // salvaging skill attribute (salvage_item — #38 slice B; "" = no skill gate)
	recipe     string  // recipe ref (craft_recipe — Phase 13.5 recipe-driven crafting)
	amount     float64 // a flat amount (heal/damage/modify_resource delta; modify_attribute_base delta)
	diceNum    int     // dice count (deal_damage <N>d<S>)
	diceSize   int     // dice size
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
		// Phase 11.1 grant ops (the progression foundation). Additive, persisted-by-construction.
		"modify_attribute_base": opModifyAttributeBase,
		"set_flag":              opSetFlag,
		"clear_flag":            opClearFlag,
		// Phase 11.2 progression ops: grant a track to an entity / feed progress and apply step grants.
		"grant_track":   opGrantTrack,
		"advance_track": opAdvanceTrack,
		// Phase 11.4 ability ownership + bundles.
		"grant_ability":  opGrantAbility,
		"revoke_ability": opRevokeAbility,
		"apply_bundle":   opApplyBundle,
		// Phase 13.3 crafting ops: consume an input / produce an output / augment an item.
		"consume_item": opConsumeItem,
		"produce_item": opProduceItem,
		"augment_item": opAugmentItem,
		// Phase 13.3 professions: enroll an entity in a trade (membership for the cap + the requires gate).
		"learn_profession": opLearnProfession,
		// Phase 13.4 deconstruction: consume an item + roll a salvage table into tier-bound components.
		"salvage_item": opSalvageItem,
		// Phase 13.5 recipes: run a content recipe (profession/skill/station gates + consume inputs/produce output).
		"craft_recipe": opCraftRecipe,
		// #34 discovery: list the recipes the actor can craft, printing the exact names `craft <name>` accepts.
		"list_recipes": opListRecipes,
	}
}

// runOps interprets an op-list in order (the step-8 entry point + the if/chance recursion). It walks
// the ops, dispatching each to its registered handler; an unknown op is logged+skipped (content-lint
// is the real gate). Single-writer: zone goroutine. Never blocks.
//
// CROSS-RESPAWN GUARD (#69, security S1). runOps holds c.target across SIBLING ops, and since 6.5 made
// death uniform ANY deal_damage can run the whole death funnel inline. So an op-list like
// `[deal_damage <lethal>, apply_affect rooted]` used to kill+respawn on op1 and then land op2 on the
// RESPAWNED player — the same *Entity, fresh hp, standing at the start room. Nothing crashed (op2 still
// funneled guardHarmful, which re-validates its target and re-checks the PvP gate), but a debuff that
// follows you to the temple is wrong on its face, and it becomes a real grief vector the moment
// respawn-sickness / spawn-protection exists: an attacker's already-committed op-list would land inside
// the protection window by construction, bypassing a guard that had not yet been written.
//
// The guard: snapshot each op's bound target's DEATH GENERATION (death.go) around the op. If it changed,
// that op killed the target, and every LATER op in the CASCADE bound to that same entity is skipped. The
// dead-set (c.diedInCascade) is identity-keyed, so ops aimed at a DIFFERENT target — an AoE's other
// victims, a self-buff rider on the caster — still run; a kill does not silently swallow the rest of the
// list. If an op killed the ACTOR — a thorns proc, a reflected nuke, an on_depleted hook re-dealing lethal
// damage back at the killer — the whole list stops: nothing the actor was mid-way through doing should
// finish resolving from beyond the grave. That is the same liveness-after-sub-call discipline the flee/
// move (M1) and cast-commit (SC2) paths already apply.
//
// Position is NOT a usable signal here: die() -> respawnPlayer clears posDead inside the same call stack,
// so on return a respawned player reads as a healthy standing entity. Only the generation survives.
//
// The dead-set is CASCADE-scoped (on the ctx), not frame-scoped, and that is load-bearing. The flow ops
// (if/chance/check) and the area loop recurse on the SAME ctx and freely rebind c.target, so a nested
// frame can kill `other` while the parent frame is watching only its own bound `target` — the parent's
// around-the-op comparison would never see it, and a later `target: other` op would land on the respawned
// player. A per-frame set is structurally unable to compose across that rebinding. (mudlib review
// reproduced both the nested-branch and the area variants of this.)
//
// SCOPE, stated honestly. This guard enforces "no op lands on an entity this cascade already killed" for
// the DATA op-list path only. It is NOT the general invariant "no hostile effect survives respawn":
//   - Lua harm (luaharm.go) issues each `h:damage` / `h:apply_affect` as its own Go call outside any
//     op-list, so a script can kill and then debuff the respawned player. (security review, F1.)
//   - An op-list run by a fired event handler or an affect tick is a fresh ctx with a fresh dead-set.
//
// The durable form of that invariant lives at the respawnPlayer chokepoint: stripHostileAffects (#318) now
// purges every affect NOT provably benign (a debuff, CC, DoT, drain, or proc) that the victim carried into
// death there, so any hostile affect PRESENT at death (a death-triggered handler's debuff, a DoT/CC tick) is
// cleared no matter which death path applied it — no caller can forget. Harm a SEPARATE later call lands after
// respawn completes (a Lua script's own `h:apply_affect`, scenario 1) is normal gated harm on a living player;
// the complementary spawn-protection window that would refuse it is tracked as a follow-up. Nothing here is a
// substitute for the chokepoint.
//
// INVARIANT the guard rests on: die() is the only thing that bumps the generation, and the only op in the
// vocabulary that removes a LIVING target. An op that ever extracts or relocates a living entity without
// routing through die() would silently defeat this — a later non-harmful op (set_flag, grant_*, heal)
// would land on the detached entity, and guardHarmful's fail-closed-on-detached check only backstops
// HARMFUL ops. A new op that can unmake a living entity must bump the generation. (security review, F5.)
func runOps(c *effectCtx, ops []effectOp) {
	// #77: the OUTERMOST runOps owns the coalesced comms-republish flush. A grant op marks its target dirty
	// (markCommsDirty) instead of republishing inline; when this frame is the one that opened the cascade it
	// flushes ONE republish per distinct target at the end. Nested runOps (if/chance recursion, an area op's
	// per-target loop) inherit the same ctx and do NOT flush — so a whole grant bundle costs one publish per
	// distinct player, not one per mutation.
	flushOwner := !c.commsCoalescing
	if flushOwner {
		c.commsCoalescing = true
		defer c.flushCommsDirty()
	}
	// #69: the actor's generation is fixed for the whole frame (an actor that died BEFORE this frame was
	// entered is the caller's problem — and a nested frame's caller is the parent runOps, which aborts on
	// exactly that signal, so the check composes upward without a shared snapshot).
	actorGen := deathGen(c.actor)
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
		//
		// #69: runOpArea marks each victim it kills into the cascade's dead-set itself (it binds c.target
		// per victim, so only it can see which ones died); here we only have to honor the actor abort.
		if op.area != "" {
			runOpArea(c, op, h)
			if deathGen(c.actor) != actorGen {
				c.z.log.Debug("op-list stopped: the actor died mid-list (#69)", "op", op.kind, "actor", c.actor.short)
				return
			}
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
		// #69: an earlier op ANYWHERE in this cascade killed the entity this op is aimed at. It has already
		// been corpsed (a mob) or respawned across the world (a player); either way this op must not land.
		if c.diedThisCascade(c.target) {
			c.z.log.Debug("effect op skipped: its target died earlier in this cascade (#69)",
				"op", op.kind, "actor", c.actor.short, "target", targetShort(c.target))
			c.target = prev
			continue
		}
		if c.z.log.Enabled(context.Background(), slog.LevelDebug) {
			c.z.log.Debug("effect op", "op", op.kind, "actor", c.actor.short,
				"target", targetShort(c.target), "disp", int(c.disp))
		}
		tgt, tgtGen := c.target, deathGen(c.target)
		if err := h(c, op); err != nil {
			c.z.log.Debug("effect op error (skipped)", "op", op.kind, "err", err)
		}
		c.target = prev
		if deathGen(tgt) != tgtGen {
			c.markDied(tgt, op.kind)
		}
		if deathGen(c.actor) != actorGen {
			c.z.log.Debug("op-list stopped: the actor died mid-list (#69)", "op", op.kind, "actor", c.actor.short)
			return
		}
	}
}

// runOpArea runs ONE area-scoped op once per valid living target in the op's area ([G12]). It resolves
// the target set (areaTargets — same-zone-contained), then binds c.target to each and invokes the op
// handler. EACH invocation funnels the op's own gate (a harmful op's dealDamage/applyDebuff ->
// guardHarmful) INDEPENDENTLY, so an AoE is N gate checks, never one: a consenting foe is harmed, a
// non-consenting player in the same room is a clean no-op — per target. A per-target `check` save rolls
// once per target (resolveCheck reads the bound c.target). c.target is restored after the loop so a
// sibling op in the list (area or not) sees the original target. Single-writer: zone goroutine.
//
// #69: the loop is the ONLY place that knows which victims this op bound, so it is the only place that
// can record which ones it killed. It marks each into the cascade's dead-set (c.markDied) so a LATER op
// in the enclosing list — `[fireball area:room, apply_affect rooted]`, or a `target: other` rider — is
// skipped for anyone the blast already killed and respawned. Skipping a victim the cascade already killed
// also stops a second area op in the same list from hitting them again at the temple.
func runOpArea(c *effectCtx, op *effectOp, h effectOpHandler) {
	targets := areaTargets(c, op.area)
	prev := c.target
	for _, t := range targets {
		if c.diedThisCascade(t) {
			c.z.log.Debug("area op skipped for a target that died earlier in this cascade (#69)",
				"op", op.kind, "area", op.area, "actor", c.actor.short, "target", targetShort(t))
			continue
		}
		c.target = t
		if c.z.log.Enabled(context.Background(), slog.LevelDebug) {
			c.z.log.Debug("effect op (area)", "op", op.kind, "area", op.area,
				"actor", c.actor.short, "target", targetShort(t), "disp", int(c.disp))
		}
		gen := deathGen(t)
		if err := h(c, op); err != nil {
			c.z.log.Debug("effect op error (skipped)", "op", op.kind, "err", err)
		}
		if deathGen(t) != gen {
			c.markDied(t, op.kind)
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
	// SPAWN-PROTECTION CANCELLATION (#394): a just-respawned player who INITIATES a harmful op forfeits
	// their OWN protection window — the standard "acting hostilely drops the shield" rule. guardHarmful is
	// the one funnel every harmful op (melee swing, spell, Lua h:damage, cross-player write) passes through,
	// so no attack path can forget to drop it. Attempt-based: the window drops even if THIS op is then
	// refused for another reason. Skipped for self-harm (actor==target) so a protected player's own
	// self-effect never drops the shield. Placed before enforcement so a protected player attacking another
	// protected player correctly loses its own shield AND is still denied by the target's.
	if c.actor != target {
		c.z.clearSpawnProtection(c.actor)
	}
	// SPAWN-PROTECTION ENFORCEMENT (#394): harm aimed at a player still inside its post-respawn window is
	// refused cleanly (no partial effect). ACTOR-AGNOSTIC — a MOB attacker short-circuits pvpAllowed
	// (pvp.go: mob->player is always allowed) BEFORE the safe-room veto, so this cannot live in the PvP
	// policy; it sits here in the one in-op funnel, ahead of the mob-target no-op below. spawnProtected is
	// false for a mob target, so mob targets still fall through to the PvE no-op. Self-harm is exempt —
	// EXCEPT a sourceless ambient room field (#397 item 1), whose actor==target is only a gating artifact
	// (landRoomAffectOn's effSrc=occ fallback): it still honors the target's window so a lava/gas field
	// cannot damage a just-respawned occupant. The cancellation above stays actor!=target-gated, so an
	// ambient field never drops the victim's own shield.
	if (c.actor != target || c.sourcelessAmbient) && c.z.spawnProtected(target) {
		c.z.log.Debug("spawn-protection: harmful op refused (respawn window)",
			"actor", c.actor.short, "target", target.short, "until", target.living.protectedUntil)
		if s, has := sessionOf(c.actor); has {
			s.send(textFrame(target.Name() + " is protected and cannot be harmed yet."))
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
	// A sourceless ambient field (#397 item 1): actor==target is an ARTIFACT (effSrc=occ), not a genuine
	// self-write, so it must NOT take the self-write exemption below — route it through guardHarmful so a
	// modify_resource/dispel/remove_affect ambient tick honors a just-respawned occupant's spawn-protection
	// window UNIFORMLY with a deal_damage field. guardHarmful returns true for an unprotected occupant
	// (self/mob pass-through) and refuses only inside the window, without dropping the occupant's own shield.
	if c.sourcelessAmbient {
		return guardHarmful(c, target)
	}
	if target == c.actor || !isPlayer(target) {
		// A self-write or a mob/ally write is not a cross-player harm vector — it proceeds ungated.
		// But a NON-self harmful write (a mob target: dispel/remove_affect/modify_resource on a monster)
		// is still a hostile action by the actor, so it must forfeit the actor's own spawn-protection
		// window (#394) — uniformly with dealDamage/applyDebuff, which drop it via guardHarmful. Without
		// this a protected player could strip a boss's buffs or drain its pool with impunity through the
		// window while a plain sword swing would have dropped the shield.
		if target != c.actor {
			c.z.clearSpawnProtection(c.actor)
		}
		return true
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
// the applied damage empties the target's vital pool, onPoolDepleted runs the content on_depleted hook
// (which can CANCEL the death by reviving the victim — death.go) then die().
//
// #406 — ANY POOL HAS CONSEQUENCES: the same checkpoint also runs on_depleted for a NON-vital pool that
// this blow emptied, and stops there (no die()). `vital` therefore means only "the default consequence of
// emptying this pool is death" — a Sanity/Stress/Stun track can bottom out into an 'insane'/'shaken'
// affect instead. See the checkpoint below for why the two triggers are deliberately asymmetric.
//
// BOUNDING (security M1): the death seam is NOT reached through fireEvent, so it does not inherit the
// bus's depth/width guard for free. onPoolDepleted->runDepletionHook (death.go) does the bounding EXPLICITLY
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
// resource selects WHICH pool the blow lands on (#71 multi-vital): an explicit resource ref routes the
// damage to that pool; "" routes to the target's primary vital (a swing / untyped damage — the legacy
// behavior). A depletion drives death only when the hit pool is VITAL (vitalDepleted), so a blow to a
// non-vital pool (a stagger/mana bar) subtracts, may run that pool's on_depleted hook (#406), and never
// kills.
func dealDamage(c *effectCtx, target *Entity, raw float64, dmgType, resource string) int {
	c.lastDamage = 0
	if target == nil || target.living == nil {
		return 0
	}
	// THE GATE, first — before any state is touched. A blocked harmful op is a clean no-op. (The gate also
	// drops the ATTACKER's spawn-protection shield on the attempt, #394, so the immunity discard below
	// short-circuits the OUTCOME only, never the attempt.)
	if !guardHarmful(c, target) {
		return 0
	}
	// Resolve WHICH vital/resource pool this blow hits, in three tiers (#71 + #405):
	//
	//	op.resource  ??  damage_type.target_resource  ??  the primary vital
	//
	// The op-level `resource` is the per-blow override and always wins. Failing that, the damage TYPE may
	// name its own pool (#405): `psychic` routes to `sanity`, `bashing` to a stun track. That is how real
	// systems name the track, and it is what makes the routing hold for damage the pack did not author —
	// a third-party psychic spell, a mob's natural weapon, a Lua h:damage — none of which can be made to
	// repeat `resource: sanity` on every op. It also covers the SWING path, which sets a damage type from
	// the weapon but never a resource. Failing both, unrouted damage lands on the primary vital: the
	// legacy behavior, byte-for-byte.
	//
	// NATURAL IMMUNITY: a ROUTED pool the target has no capacity for (derived max <= 0 — a mindless
	// construct with no "sanity") is discarded here, before mitigation/reaction/threat, exactly like a
	// fully-resisted hit. `routed` is what the discard keys on, NOT `resource != ""` — a type-routed blow
	// is just as explicit a naming of a pool as an op-level one, and leaving it unguarded is the sharp
	// edge #405 introduces: without the discard the blow writes a phantom current on a pool the entity has
	// no capacity in, which (a) makes entityHasResource true forever, silently subscribing it to that
	// resource's event handlers, (b) persists through dumpResources, and (c) is a stored one-way door —
	// resourceCurrent's "absent reads as full" no longer applies, so if that pool EVER gains capacity
	// (gear, an affect, a class grant, a content patch) the entity reads permanently empty on it, and for
	// a vital pool the next point of damage is an instant kill.
	//
	// The UNROUTED path stays unguarded, deliberately: that is the legacy swing/DoT behavior and the
	// checkpoint's max > 0 clause still stands behind it.
	pool, routed, route := resource, resource != "", "op"
	if pool == "" {
		if p := damageTypePool(target, dmgType); p != "" {
			pool, routed, route = p, true, "type"
		}
	}
	if pool == "" {
		pool, route = vitalResource(target), "primary"
	}
	if routed && resourceMax(target, pool) <= 0 {
		c.z.log.Debug("deal_damage: target immune (no capacity in pool); discarded",
			"target", target.short, "pool", pool, "type", dmgType)
		return 0
	}
	dmg := mitigate(target, raw, dmgType)
	if dmg <= 0 {
		c.z.log.Debug("deal_damage fully mitigated", "target", target.short, "type", dmgType, "raw", raw)
		return 0
	}
	// --- OnDamageTaken REACTION checkpoint (7.9, P7-D8 / T12): BEFORE applying the (mitigated) damage,
	// fire a result-altering reaction about the TARGET (subject) carrying the pending `dmg` as the event
	// mag and the attacker as `other`. A Lua hook may reduce the pending amount (rx:modify("amount",
	// -soak)), CANCEL it (rx:cancel() negates the blow), or — the concentration case — rx:cancel() ITSELF
	// (drop the concentration affect on a failed save; the affect break is applied at the seam). The
	// reaction threads THIS ctx's eventBudget (T12 invariant 3) so an OnDamageTaken→reaction→damage loop
	// is bounded. Observe-then-recheck: we re-read the recorded mutations here and apply only what the
	// checkpoint permits ("amount" is the sole modify field; concentration's cancelAffect is dropped).
	if dmg = c.z.applyDamageReaction(c, target, dmg, raw, dmgType, pool); dmg <= 0 {
		c.z.log.Debug("deal_damage fully negated by an OnDamageTaken reaction", "target", target.short)
		return 0
	}
	// A vital-less target (no explicit resource routed the blow, and the target has no primary vital)
	// takes no pool write — discard but report the mitigated amount (legacy behavior for a bare entity).
	if pool == "" {
		c.z.log.Debug("deal_damage: target has no vital resource; damage discarded", "target", target.short)
		c.lastDamage = dmg
		return dmg
	}
	// RE-CHECK THE CAPACITY, because the discard above happened BEFORE applyDamageReaction and this write
	// happens after (a TOCTOU). A reaction can attach an affect that drops the routed pool's derived max to
	// 0 mid-blow — "shatter its mind, then the psychic hit has nothing to land on" is a perfectly natural
	// thing to author — and the earlier check is then stale. Without this the blow stores a current on a
	// pool with NO capacity, which is worse than it sounds: setResourceCurrent only clamps the TOP end when
	// max > 0, so the pool stores and reads back a POSITIVE value at max 0, and once anything is stored,
	// resourceCurrent's "absent reads as full" is gone for good — when capacity returns the entity reads
	// permanently empty, and on a vital pool the next point of damage kills.
	//
	// The TOCTOU predates #405 (an op-level `resource` route reaches it identically), but #405 is what takes
	// the exposed traffic from "ops an author deliberately wrote" to EVERY blow carrying a routed type, so
	// the guarantee this feature advertises has to actually hold.
	if routed && resourceMax(target, pool) <= 0 {
		c.z.log.Debug("deal_damage: target lost capacity in the routed pool mid-blow; discarded",
			"target", target.short, "pool", pool, "type", dmgType)
		return 0
	}
	// Apply to the resolved pool. The pool clamps its current at 0 (resources.go); the depletion
	// checkpoint below turns an emptied pool into that pool's on_depleted hook, and into death when (and
	// only when) the pool is a VITAL resource.
	cur := resourceCurrent(target, pool)
	setResourceCurrent(target, pool, cur-dmg)
	c.lastDamage = dmg
	// OVERFLOW (#407): how far PAST 0 this blow drove the pool — the excess the pool could not absorb.
	// setResourceCurrent clamps the store at 0 and the number is gone after the write, so it is captured
	// here, where both operands are in scope, and handed to the depletion hook as a referenceable amount
	// ($depletion.overflow). That is what a two-track system needs: a stun/stagger track carries its excess
	// into a lethal pool, FP-below-0 bites into HP, an HP-buffer spills into a stat.
	//
	// It is computed at the CALLER, not returned from setResourceCurrent: that setter is a general two-sided
	// clamp with seven callers (heal, modify_resource, an ability cost, regen, respawn, the per-round
	// top-up, character load), and for every one of them "overflow" is either meaningless or a TOP-end
	// discard (95 + 50 on a max-100 pool). A single returned int would be sign-ambiguous and would invite
	// future callers to plumb a value that only means something on the damage path.
	//
	// INVARIANT: 0 <= overflow <= dmg, and overflow > 0 implies the pool reads 0 after the write. It is
	// undefined only where no write happened at all — every early return above (the gate, the immunity
	// discard, full mitigation, a negating reaction, a vital-less target) precedes this line, which is
	// exactly why it is well-defined here. An exact-to-zero blow yields 0, so a hook must handle 0; a blow
	// onto an ALREADY-empty pool yields the full damage, which is the carry-over case working as intended.
	// The floor is DEFENSIVE, not live: the hook only runs when the checkpoint below sees the pool at <= 0,
	// which means dmg >= cur, so the subtraction cannot go negative on any path that consumes this. It is
	// kept so the value is well-defined AT ITS DEFINITION SITE instead of depending on a gate 60 lines
	// later — a negative overflow reaching a `deal_damage` amount would be a silent heal. Mutation-testing
	// confirms no test covers the branch, which is the point: it is an invariant, not a case.
	overflow := dmg - cur
	if overflow < 0 {
		overflow = 0
	}
	// The victim's death generation AS OF THIS WRITE (#69). The checkpoint below re-reads it: the events
	// fired in between (OnDamageTaken, OnHit) may KILL the target — an execute rider, a thorns reflect, a
	// DoT in the same list — and for a PLAYER, respawnPlayer then puts them at the start room, alive and
	// restored. Running this blow's depletion hook after that means firing a consequence at a victim who is
	// no longer in the fight, in a different room, with $other bound to a killer who is somewhere else: the
	// "your harm follows you to the temple" class (#318) that the runOps cross-respawn guard exists to stop.
	// Before #406 this was fail-safe only by accident (respawn refilled the vital, so vitalDepleted went
	// false); with a non-vital pool nothing refilled it, so the guard has to be explicit.
	tgtGen := deathGen(target)
	// `route` names WHICH precedence tier picked the pool (op / type / primary). Without it the three-tier
	// resolution is unobservable at the one place it matters: a registered-but-wrong route (someone adds
	// `target_resource: mana` to `fire`) has no lint and no other signal, and an operator staring at
	// "type=fire pool=mana" would have to already know the feature exists to explain it.
	c.z.log.Debug("deal_damage applied", "target", target.short, "type", dmgType,
		"raw", raw, "applied", dmg, "pool", pool, "route", route, "from", cur, "to", resourceCurrent(target, pool))

	// --- Threat + the lit combat events, now UNIFORM across all damage (moved out of the swing path so a
	// spell/AoE/DoT builds threat and triggers reactions identically to a melee swing). source is the
	// attacker the damage is attributed to (the swing attacker; a DoT's applier; nil for a sourceless
	// environmental hit). A self/ambient DoT (source == target) is NOT an attacker — no threat, no OnHit.
	src := c.source
	// ATTRIBUTION SUBSTITUTION (#407): a depletion hook runs with source == the victim, so a blow it deals
	// AT THE VICTIM (the carry-over: "spill my overflow into my own hp") is self-directed and would be
	// credited to nobody — no threat, no OnHit, and a kill through it resolves die(victim, killer=victim),
	// costing the real attacker the OnKill subject, XP and the corpse loot-ownership window. `credit` is the
	// attacker the depletion was attributed to; substituting it ONLY here, for a genuinely self-directed
	// blow, leaves a `tgt: other` retaliation hook (thorns / death-curse) attributed exactly as before.
	if src == target && c.credit != nil {
		src = c.credit
	}
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

	// --- Depletion checkpoint (6.5, the uniform death seam + #406): the applied damage emptied the pool ->
	// run THAT pool's on_depleted hook, then — only for a VITAL pool — die(). source is the killer/source
	// attribution (nil for a sourceless hit — no OnKill subject). Idempotent for death via the posDead latch
	// (onPoolDepleted/die). Shares this ctx's budget/depth so the hook stays bounded inside a cascade.
	//
	// #406 widened the trigger from `vitalDepleted` to `poolDepleted` — the SAME predicate minus the `vital`
	// clause — so a non-vital pool bottoming out runs its hook too. `vital` is now only what decides whether
	// death FOLLOWS the hook, and that decision lives inside onPoolDepleted (which re-reads vitalDepleted),
	// not here: keeping the one edge into die() behind one predicate is what makes "a Sanity break is not a
	// death" structural.
	//
	// LEVEL-TRIGGERED, not edge-triggered — the same rule the vital path has always had. The hook fires on
	// every blow that leaves the pool at 0, INCLUDING a blow onto an already-empty pool. Two reasons, and
	// they point the same way:
	//   - A two-track system (a stun/stagger track that carries its excess into a lethal pool) REQUIRES it.
	//     Under an edge rule an already-empty track would swallow every subsequent blow whole and the
	//     target would be immune to that damage kind forever — the carry-over is exactly what has to keep
	//     happening after the first crossing.
	//   - A vital pool can also reach 0 off the damage path (modify_resource, an ability cost, a max drop);
	//     the next blow onto that already-empty vital must still kill. An edge rule would leave an
	//     unkillable 0-hp victim.
	// The cost is that a non-vital hook can re-run while the pool stays empty (a vital one is latched by
	// posDead). That is a CONTENT concern, called out in ResourceDTO.OnDepleted: make the hook idempotent
	// (`if has_affect` / `stacking: ignore`) and never put a rewarding op in one.
	// The deathGen re-read is the cross-respawn guard described at the write above: if the victim died
	// during the events this blow fired, THIS blow's depletion is void — the entity that pool belonged to is
	// gone (a corpsed mob) or has already been restored and moved (a respawned player).
	//
	// DEFENSE IN DEPTH, not the primary guard: respawnPlayer refills every pool (death.go), so a respawned
	// player's pool reads full here anyway, and a dead mob is caught by onPoolDepleted's posDead entry check.
	// Reverting this line alone leaves the suite green — it is kept because both of those are properties of
	// OTHER functions, and this checkpoint should not depend on them staying true.
	if deathGen(target) == tgtGen && poolDepleted(target, pool) {
		c.z.onPoolDepleted(target, src, pool, depletion{overflow: overflow, applied: dmg - overflow, amount: dmg}, c)
	}
	return dmg
}

// damageTypePool returns the resource pool the damage TYPE `dmgType` routes to (#405), or "" when the type
// is unknown, names no pool, or the target carries no content at all. It is the middle tier of dealDamage's
// routing precedence — consulted only when the op named no `resource` of its own.
//
// Deliberately shaped like mitigate: it takes the target and reads the type's def off target.zone through
// the same accessor, so the two type-keyed lookups in the damage path read the same registry the same way
// and neither depends on c.z (which can in principle differ from target.zone). Zone-goroutine read
// (registry atomic.Load).
//
// WHY THE RESOLUTION LIVES AT THE FUNNEL and not in opDealDamage: dealDamage has entry points that never
// touch the op handler at all — Lua `h:damage{type=...}` (luaharm.go) and the reaction redirect's re-entry —
// and routing must not drift apart from mitigation, which reads the same def one step later. Note the
// swing path and the redirect are NOT discriminators here, though it is tempting to say so: the swing goes
// through opDealDamage, and the redirect carries the ALREADY-RESOLVED pool forward as an explicit resource
// rather than re-resolving against the new target (it coincides because routes are zone-global).
func damageTypePool(target *Entity, dmgType string) string {
	if target == nil || target.zone == nil || dmgType == "" {
		return ""
	}
	if def := target.zone.damageTypeDefs().get(dmgType); def != nil {
		return def.targetResource
	}
	return ""
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

// vitalResource returns the ref of the target's PRIMARY (default-damage) vital resource (hp in the
// demo), or "" if the target has no vital at all. Unrouted damage — a melee swing, a deal_damage with
// no explicit `resource` — subtracts from this pool. The pick is DETERMINISTIC: the vital flagged
// `primary` wins (the lowest-ref one if, against convention, several are); absent any primary flag it
// falls back to the lowest ref by sort order, so the choice never depends on Go's randomized map
// iteration. lintVitalResources nudges an author to designate a primary once >1 vital exists. Reads the
// registry (lock-free atomic.Load).
//
// MULTI-VITAL (#71): a pack may mark more than one resource `vital`; each is INDEPENDENTLY lethal —
// deal_damage can route to any of them via `resource`, and depleting any vital drives death at the
// dealDamage checkpoint (see vitalDepleted). This function only resolves the DEFAULT pool for unrouted
// damage; the checkpoint and the death seam operate on whichever pool a given blow actually hit.
func vitalResource(e *Entity) string {
	if e == nil || e.zone == nil {
		return ""
	}
	lowest, primary := "", ""
	for ref, def := range e.zone.resourceDefs().table() {
		if !def.vital {
			continue
		}
		if lowest == "" || ref < lowest {
			lowest = ref
		}
		if def.primary && (primary == "" || ref < primary) {
			primary = ref
		}
	}
	if primary != "" {
		return primary
	}
	return lowest
}

// depletion is the per-blow arithmetic of the depletion that just happened (#407), handed to the hook so a
// content amount can reference it (`$depletion.overflow` etc., check.go resolveDepletionRef). It is a value
// type: it describes ONE blow against ONE pool and is rebuilt per depletion, never shared or mutated.
//
//	amount  = the mitigated damage this blow applied to the pool
//	applied = what the pool actually ABSORBED (min(amount, current-before)) — "you lost N sanity"
//	overflow = what it could NOT absorb (amount - applied) — the excess a two-track system carries onward
//
// applied + overflow == amount always, so a hook can split a blow without arithmetic. The zero value (an
// op-list running outside a depletion) reads 0 for every field, which makes a stray reference inert.
type depletion struct {
	overflow int
	applied  int
	amount   int
}

// poolDepleted reports whether `pool` is a content-defined resource on `e` that has bottomed out — the
// VITAL-free half of the depletion predicate (#406). TRUE only when the resource is registered, its
// derived max is > 0, and its current is <= 0.
//
// The max > 0 clause is load-bearing twice over: it is the structural guard that a capacity-less pool
// can never read as "depleted" (a mindless construct with max_sanity 0 must not fire sanity's "you go
// insane" hook, exactly as it must not die of it), and it keeps this predicate consistent with the
// immunity discard the routing path applies (dealDamage above). Zone-goroutine read.
func poolDepleted(e *Entity, pool string) bool {
	if e == nil || e.zone == nil || pool == "" {
		return false
	}
	return e.zone.resourceDefs().get(pool) != nil &&
		resourceMax(e, pool) > 0 && resourceCurrent(e, pool) <= 0
}

// vitalDepleted reports whether `pool` is a VITAL resource on `e` that has bottomed out — the exact
// predicate the DEATH seam keys on (#71). It is poolDepleted PLUS `def.vital`, defined in terms of it so
// the two can never drift apart. Used at BOTH the dealDamage depletion checkpoint and onPoolDepleted's
// cancel re-check so those two can never disagree about "is this death real?" A non-vital pool (mana, a
// stagger bar, a Sanity track) can be damaged to 0 — and since #406 can run its own on_depleted hook —
// but is NEVER lethal: this is the one predicate standing between a depletion and die(), which is why the
// `vital` clause lives here and not at a call site. Zone-goroutine read.
func vitalDepleted(e *Entity, pool string) bool {
	if e == nil || e.zone == nil || pool == "" {
		return false
	}
	def := e.zone.resourceDefs().get(pool)
	return def != nil && def.vital && poolDepleted(e, pool)
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
