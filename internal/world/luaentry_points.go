package world

import (
	lua "github.com/yuin/gopher-lua"
)

// luaentry_points.go — the per-entry-point GLUE wiring reserved Lua columns to the invocation
// core (luaentry.go). Each function compiles-once-per-zone (chunkFor) and invokes through the
// core, threading the FIRING cascade's budget (invokeFromCtx from the engine's *effectCtx) and
// failing closed. This file is a Lua BINDING file (it constructs handle binds) and is covered by
// the type-aware funnel-reuse lint (luaharm_lint_test.go luaBindingFiles).
//
// Slice 7.4 lands the entry points incrementally; each unit adds its wiring here.

// --- 7.4b: ability on_resolve -------------------------------------------------------------

// runAbilityResolveLua runs an ability's Lua on_resolve body (if any) at step 8, BESIDE the
// declarative op-list. It threads the SAME step-8 effectCtx `c` via invokeFromCtx, so the body's
// harm ops inherit the gate disposition + cascade depth/budget (P7-D3 / invariant 1). `self` and
// `ctx.actor` are the caster; `ctx.target` the target; `ctx.room` the caster's room. Fail-closed:
// a broken body is inert (chunkFor caches nil), a runtime error fizzles just this resolve.
func (z *Zone) runAbilityResolveLua(c *effectCtx, def *abilityDef) {
	if def == nil || def.onResolveLua == "" || z.lua == nil {
		return
	}
	rt := z.lua
	ch := rt.chunkFor("ability:"+def.ref+":on_resolve", def.onResolveLua)
	if ch == nil {
		return // no body / compile failed (inert)
	}
	ctx := rt.abilityCtxTable(c)
	_ = rt.invokeFromCtx(ch, c, c.actor, map[string]lua.LValue{"ctx": ctx})
}

// --- 7.4d: affect hooks (on_apply / on_expire / on_dispel) --------------------------------

// runAffectHookLua runs an affect's Lua hook (on_apply/on_expire/on_dispel) on the affected
// entity e. The actor (harm source) is the affect's SOURCE — who applied it (inst.source) — so a
// harmful op in the hook gates "may the applier harm this target?", exactly like fireOnTick; a
// self/ambient affect (nil source) uses the victim as the source (self-harm is never gated). The
// hook is a clean ROOT invocation (an affect lifecycle event, not inside an event cascade), so a
// harm op in it starts its own cascade. `self` = the affected entity; the source is the
// invocation actor. Fail-closed: a broken/erroring hook is inert / fizzles. hookName is
// "on_apply"/"on_expire"/"on_dispel"; src is the Lua source.
func (z *Zone) runAffectHookLua(e *Entity, inst *affectInstance, hookName, src string) {
	if e == nil || inst == nil || src == "" || z.lua == nil {
		return
	}
	rt := z.lua
	ch := rt.chunkFor("affect:"+inst.def.ref+":"+hookName, src)
	if ch == nil {
		return
	}
	// The harm actor is the affect's source (mirror fireOnTick), fail-closed on a detached source.
	actor := inst.source
	if actor == nil {
		actor = e // self/ambient: the victim is the source (self-harm never gated)
	} else if actor.location == nil || actor.living == nil {
		// FAIL-CLOSED: the source has detached — do not evaluate the gate against a stale pointer.
		rt.log.Debug("affect Lua hook: source detached, hook skipped", "ref", inst.def.ref, "hook", hookName)
		return
	}
	// A clean ROOT ctx: depth 0, nil eventBudget (an affect lifecycle event, like a command-issued
	// action). The actor is the affect source; self is the affected entity.
	c := &effectCtx{z: z, actor: actor, source: actor, target: e, mag: float64(maxInt(inst.stacks, 1)), disp: dispHarmful, rng: rt.rng}
	_ = rt.invokeFromCtx(ch, c, e, nil)
}

// --- loot on_roll hatch (Phase 12.1, docs/REMAINING.md §4) --------------------------------

// runLootOnRollLua runs a loot table's on_roll(ctx) body for one looter and returns the list of ADDITIONAL
// item prototype refs it wants dropped (conditional drops the declarative rolls can't express). It is a
// READ-ONLY decision hatch: the body inspects ctx and returns refs; the caller (resolveLoot) delivers each
// through the normal loot pipeline (quality/binding/merge), so the hatch can neither bypass delivery nor
// attribute a drop to a spoofed source. `ctx.looter` is the receiving player, `ctx.source` the slain victim,
// both validated handles. A clean ROOT invocation (a loot event, not inside a cascade); the looter is the
// invocation actor. Fail-closed: no body / compile failure / runtime error / non-list return => no drops.
func (z *Zone) runLootOnRollLua(looter, victim *Entity, table *lootTableDef) []string {
	if table == nil || table.onRoll == "" || z.lua == nil || looter == nil {
		return nil
	}
	rt := z.lua
	ch := rt.chunkFor("loot:"+table.ref+":on_roll", table.onRoll)
	if ch == nil {
		return nil // no body / compile failed (inert)
	}
	ctx := rt.L.NewTable()
	ctx.RawSetString("looter", rt.newHandle(looter))
	ctx.RawSetString("source", rt.newHandle(victim))
	refs := rt.invokeForStringList(ch, &luaInvocation{actor: looter}, map[string]lua.LValue{"ctx": ctx})
	// Defensive bound (both reviewers): the Lua budget bounds EXECUTION, not the returned array size, so a
	// runaway/buggy body could flood the looter with spawns. Cap the delivered count and log the truncation
	// so a builder sees the runaway. A legitimate conditional-bonus hatch returns a handful.
	if len(refs) > maxLootOnRollDrops {
		rt.log.Warn("loot on_roll returned an over-long list; truncating",
			"table", table.ref, "returned", len(refs), "cap", maxLootOnRollDrops)
		refs = refs[:maxLootOnRollDrops]
	}
	return refs
}

// maxLootOnRollDrops caps how many items a single on_roll body may drop for one looter (a defensive bound
// on a runaway/buggy hatch; the Lua instruction budget bounds execution, not the returned list length).
const maxLootOnRollDrops = 64

// --- 7.4f: pvp_allowed policy + ruleset formulas ------------------------------------------

// consultedLuaFormulas is the set of ruleset-formula names the engine ACTUALLY consults from the
// pack's `formulas` map today (7.4f wired `regen` only; to_hit/soak/xp_for live in checkSpec/attr/
// OnKill and are not yet read here). defineGlobals warns at load when a content `formulas` entry
// names a ref NOT in this set, so a defined-but-dead formula seam is never silent. As later slices
// wire to_hit/soak/xp_for through luaFormula, add them here.
var consultedLuaFormulas = map[string]bool{
	"regen": true,
}

// consultPvpPolicy runs the pack's Lua pvp_allowed policy for (actor, target). It is a READ-ONLY
// query — the policy DECIDES, it does not mutate state — and FAIL-CLOSED via invokeForBool: a
// missing/erroring policy or any non-`true` return denies harm. `actor`/`target` are bound as
// validated handles; the policy returns true to permit. The invocation is a clean root (a gate
// query, not inside a cascade) and the actor is the harming actor.
func (rt *luaRuntime) consultPvpPolicy(body string, actor, target *Entity) bool {
	ch := rt.chunkFor("pvp_allowed", body)
	if ch == nil {
		return false // compile failed / empty: fail-closed (deny)
	}
	binds := map[string]lua.LValue{
		"actor":  rt.newHandle(actor),
		"target": rt.newHandle(target),
	}
	return rt.invokeForBool(ch, &luaInvocation{actor: actor}, binds)
}

// luaFormula runs a pack's Lua ruleset formula (to_hit/soak/regen/xp_for) for the given subject,
// returning (value, true) on a numeric return or (0, false) when the pack defines no Lua formula
// for `name`, the body fails to compile, errors, or returns a non-number — the caller then falls
// back to the engine's prefix-AST/default (fail-closed: a broken formula never silently corrupts
// a stat). `self` (and `target` if given) are bound as handles. A clean root invocation.
func (z *Zone) luaFormula(name string, self, target *Entity) (float64, bool) {
	if z == nil || z.lua == nil {
		return 0, false
	}
	b := z.defBundle()
	if b == nil || b.formulas == nil {
		return 0, false
	}
	body := b.formulas[name]
	if body == "" {
		return 0, false
	}
	rt := z.lua
	ch := rt.chunkFor("formula:"+name, body)
	if ch == nil {
		return 0, false
	}
	binds := map[string]lua.LValue{"self": rt.newHandle(self)}
	if target != nil {
		binds["target"] = rt.newHandle(target)
	}
	return rt.invokeForNumber(ch, &luaInvocation{actor: self}, binds)
}

// abilityCtxTable builds the `ctx` table an ability on_resolve body reads: actor/target/room
// handles (validated, re-resolved per method) + the magnitude. It is a plain data+handle table
// in the per-call fresh env — no engine pointer escapes. A nil actor/target yields a nil field.
func (rt *luaRuntime) abilityCtxTable(c *effectCtx) *lua.LTable {
	t := rt.L.NewTable()
	t.RawSetString("actor", rt.newHandle(c.actor))
	t.RawSetString("target", rt.newHandle(c.target))
	if c.actor != nil {
		t.RawSetString("room", rt.newHandle(c.actor.location))
	}
	t.RawSetString("mag", lua.LNumber(c.mag))
	return t
}
