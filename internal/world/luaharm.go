package world

import (
	lua "github.com/yuin/gopher-lua"
)

// luaharm.go — the Lua EFFECT-OP handle methods (docs/PHASE7-PLAN.md slice 7.3c, P7-D3, T8).
// THE harm surface: Lua's first ability to harm. The cardinal rule — a Lua harm op is NOT a
// gate bypass: EVERY harm vector routes the EXISTING funnel (dealDamage / applyDebuff /
// guardCrossPlayerWrite, effect_op.go) or the existing op handler that funnels it. There is NO
// Lua-specific damage/affect/flag write path. The security-sensitive code is harmCtx — the
// effectCtx the funnels gate on — which MUST hold the five P7-D3 invariants:
//
//  1. actor/source are ENGINE-resolved from the invocation context (rt.inv), NEVER a Lua arg —
//     a script cannot name an arbitrary `source` to spoof attribution past the gate.
//  2. disp is ENGINE-set (dispHarmful for a harm method) — a script cannot set it helpful/self
//     to skip the gate.
//  3. the funnels are the ONLY write path — no method here writes a vital/affect/flag/position
//     directly (the build-failing lint, luaharm_lint_test.go, enforces this as the surface grows).
//  4. rng is ALWAYS the per-zone ctx rng — never script-injected (determinism).
//  5. depth/eventBudget are threaded from the invoking cascade (rt.inv), NEVER reset by the
//     binding — a Lua harm op inside an event/reaction cascade inherits the SAME shared budget.
//
// Single-writer: every method runs on the zone goroutine.

// harmCtx builds the effectCtx a Lua harm op gates on, for the given (already re-resolved)
// target. It is the ONE place a Lua harm op's context is constructed, and it holds the five
// invariants by construction: actor/source = the invocation actor (never a Lua arg), disp =
// dispHarmful (engine-set), rng = the zone rng, depth/eventBudget = threaded from rt.inv. It
// returns nil if there is no active invocation (no actor) — the caller then no-ops (fail-closed):
// a harm op outside any entry-point invocation has nobody to act as, so it does nothing.
func (rt *luaRuntime) harmCtx(target *Entity) *effectCtx {
	if rt.inv == nil || rt.inv.actor == nil {
		return nil
	}
	return &effectCtx{
		z:           rt.zone,
		actor:       rt.inv.actor, // invariant 1: engine-resolved, not script-supplied
		source:      rt.inv.actor, // invariant 1: source IS the invoker, never a Lua arg
		target:      target,
		mag:         1,
		disp:        dispHarmful,  // invariant 2: engine-set; a harm method is harmful
		rng:         rt.rng,       // invariant 4: always the zone rng
		depth:       rt.inv.depth, // invariant 5: threaded, not reset
		eventBudget: rt.inv.eventBudget,
	}
}

// helpfulCtx builds the effectCtx for a SELF/ALLY helpful op (heal / a buff apply_affect on
// self). It carries disp=dispHelpful and the same engine-resolved actor — a helpful op does NOT
// route guardHarmful, but it still must never take its actor/source from a Lua arg, and the
// helpful op handlers (opHeal) are the only write path. nil if no active invocation.
func (rt *luaRuntime) helpfulCtx(target *Entity) *effectCtx {
	if rt.inv == nil || rt.inv.actor == nil {
		return nil
	}
	return &effectCtx{
		z:           rt.zone,
		actor:       rt.inv.actor,
		source:      rt.inv.actor,
		target:      target,
		mag:         1,
		disp:        dispHelpful,
		rng:         rt.rng,
		depth:       rt.inv.depth,
		eventBudget: rt.inv.eventBudget,
	}
}

// --- h:damage{amount=, type=, can_avoid=} — routes dealDamage (the gate is first inside it) --

// hDamage applies damage to the handle's entity AS TARGET. It routes the existing dealDamage
// funnel, which runs guardHarmful FIRST (so a protected player in a safe room is a clean no-op,
// T8) and then the shared mitigation/death pipeline. The amount/type are op DATA (a Lua arg is
// data, never the actor/source/disp — those come from harmCtx). Returns the damage actually
// applied (0 on a gated block / no target / no invocation).
func (rt *luaRuntime) hDamage(l *lua.LState) int {
	target := resolveHandle(l, 1)
	opts := l.CheckTable(2)
	amount := optTableNumber(opts, "amount", 0)
	dmgType := optTableString(opts, "type", "")
	c := rt.harmCtx(target)
	if c == nil || target == nil {
		l.Push(lua.LNumber(0))
		return 1
	}
	applied := dealDamage(c, target, amount, dmgType)
	l.Push(lua.LNumber(applied))
	return 1
}

// --- h:heal(resource, amount) — helpful, never gated (routes opHeal) ----------------------

// hHeal raises the handle's entity's resource pool by amount. Heal only ever RAISES a pool (a
// negative amount is clamped to 0 inside opHeal — it cannot be weaponized into a cross-player
// drain; that path is modify_resource, which is gated). It routes the existing opHeal handler
// via a helpful ctx — the only resource-write path.
func (rt *luaRuntime) hHeal(l *lua.LState) int {
	target := resolveHandle(l, 1)
	resource := l.CheckString(2)
	amount := l.CheckNumber(3)
	c := rt.helpfulCtx(target)
	if c == nil || target == nil {
		return 0
	}
	_ = opHeal(c, &effectOp{kind: "heal", resource: resource, amount: float64(amount)})
	return 0
}

// --- h:modify_resource(resource, delta) — GATED cross-player (routes opModifyResource) -----

// hModifyResource applies a signed delta to the handle's entity's resource pool. ANY write to
// another PLAYER's pool (any sign — a content pool's polarity is unknown) routes the ONE shared
// guardCrossPlayerWrite inside opModifyResource; a self/mob write is ungated. It routes the
// existing op handler — the only resource-write path.
func (rt *luaRuntime) hModifyResource(l *lua.LState) int {
	target := resolveHandle(l, 1)
	resource := l.CheckString(2)
	delta := l.CheckNumber(3)
	c := rt.harmCtx(target) // harm ctx: a cross-player resource write is gated
	if c == nil || target == nil {
		return 0
	}
	_ = opModifyResource(c, &effectOp{kind: "modify_resource", resource: resource, amount: float64(delta)})
	return 0
}

// --- h:drain(resource, amount, to) — GATED (drain target, give to `to`) -------------------

// hDrain drains `amount` from the handle's entity's resource pool and (optionally) transfers it
// to the `to` entity. The drain FROM another player is harm (a cross-player resource write), so
// it routes guardCrossPlayerWrite via opModifyResource with a negative delta; the credit TO `to`
// is a helpful raise on the recipient (opHeal-style, never gated — you may always give). Both
// legs route the existing op handlers — no direct write.
func (rt *luaRuntime) hDrain(l *lua.LState) int {
	from := resolveHandle(l, 1)
	resource := l.CheckString(2)
	amount := l.CheckNumber(3)
	to := optResolve(l, 4)
	if from == nil || amount <= 0 {
		return 0
	}
	// Leg 1: drain from the target (gated cross-player write, negative delta).
	cFrom := rt.harmCtx(from)
	if cFrom == nil {
		return 0
	}
	before := resourceCurrent(from, resource)
	_ = opModifyResource(cFrom, &effectOp{kind: "modify_resource", resource: resource, amount: -float64(amount)})
	drained := before - resourceCurrent(from, resource) // what actually left (0 if gated)
	// Leg 2: credit the recipient (helpful raise) with what was actually drained.
	if to != nil && drained > 0 {
		cTo := rt.helpfulCtx(to)
		if cTo != nil {
			_ = opHeal(cTo, &effectOp{kind: "heal", resource: resource, amount: float64(drained)})
		}
	}
	return 0
}

// --- h:apply_affect(id, {duration=, magnitude=, stacks=}) — routes opApplyAffect -----------

// hApplyAffect applies an affect to the handle's entity. Whether it is GATED is DERIVED from
// the affect def + the engine-set disp inside opApplyAffect (a detrimental affect routes
// applyDebuff -> guardHarmful; a genuine buff on self/ally attaches ungated) — exactly the
// declarative path. SECURITY (invariant 1): the `source` is ALWAYS the invocation actor, set in
// the ctx — a `source=` key in the Lua opts table is IGNORED, so a script cannot spoof
// attribution. duration/magnitude/stacks are op DATA. Routes the existing op handler.
func (rt *luaRuntime) hApplyAffect(l *lua.LState) int {
	target := resolveHandle(l, 1)
	affect := l.CheckString(2)
	var duration int
	var magnitude float64
	if l.GetTop() >= 3 {
		if opts, ok := l.Get(3).(*lua.LTable); ok {
			duration = int(optTableNumber(opts, "duration", 0))
			magnitude = optTableNumber(opts, "magnitude", 0)
			// NOTE: a `source=` key is deliberately NOT read — source is engine-resolved.
		}
	}
	// Use the harm ctx (disp=dispHarmful): opApplyAffect derives gating from the def AND the
	// disp, so a harm ctx ensures a detrimental affect is gated; a genuinely-helpful affect on
	// self/ally still attaches ungated (opApplyAffect's own logic). The source in the ctx is the
	// invocation actor — never a Lua arg.
	c := rt.harmCtx(target)
	if c == nil || target == nil {
		l.Push(lua.LFalse)
		return 1
	}
	op := &effectOp{kind: "apply_affect", affect: affect, duration: duration, magnitude: magnitude}
	_ = opApplyAffect(c, op)
	l.Push(lua.LTrue)
	return 1
}

// --- h:remove_affect(id) — GATED cross-player (routes opRemoveAffect) ----------------------

// hRemoveAffect removes an affect instance from the handle's entity. Stripping an affect off
// ANOTHER player is harm (you can rip a protective buff), so it routes guardCrossPlayerWrite via
// opRemoveAffect; a self/ally/mob cleanse is ungated. Source-keyed by the invocation actor.
func (rt *luaRuntime) hRemoveAffect(l *lua.LState) int {
	target := resolveHandle(l, 1)
	affect := l.CheckString(2)
	c := rt.harmCtx(target)
	if c == nil || target == nil {
		return 0
	}
	_ = opRemoveAffect(c, &effectOp{kind: "remove_affect", affect: affect})
	return 0
}

// --- h:dispel{category=, count=} — GATED cross-player (routes opDispel) ---------------------

// hDispel removes up to count dispellable affects (optionally of a category) from the handle's
// entity. Dispelling another player's buffs is harm — routes guardCrossPlayerWrite via opDispel;
// a self/ally/mob cleanse is ungated.
func (rt *luaRuntime) hDispel(l *lua.LState) int {
	target := resolveHandle(l, 1)
	var category string
	var count float64
	if l.GetTop() >= 2 {
		if opts, ok := l.Get(2).(*lua.LTable); ok {
			category = optTableString(opts, "category", "")
			count = optTableNumber(opts, "count", 0)
		}
	}
	c := rt.harmCtx(target)
	if c == nil || target == nil {
		return 0
	}
	_ = opDispel(c, &effectOp{kind: "dispel", text: category, amount: count})
	return 0
}

// --- movement: same-zone only; cross-zone is a reserved no-op (distsys boundary) -----------

// hMove moves the handle's entity one step in the given direction, WITHIN this zone only. A
// direction whose exit leads to ANOTHER zone is a clean no-op — a Lua move must NOT smuggle an
// entity across the single-writer/handoff boundary (that is the engine's transfer/handoff path,
// reserved for the Phase-10 director). It uses the existing Move primitive + the arrival hooks
// (room affects, aggro), the same containment path the move command uses. Returns true if the
// entity moved.
func (rt *luaRuntime) hMove(l *lua.LState) int {
	e := resolveHandle(l, 1)
	dir := l.CheckString(2)
	if e == nil || e.location == nil || e.location.room == nil {
		l.Push(lua.LFalse)
		return 1
	}
	ref, ok := e.location.room.exits[dir]
	if !ok {
		l.Push(lua.LFalse)
		return 1
	}
	destZone, destRoom := parseRef(ref)
	// Cross-zone exit: reserved no-op. Never a direct cross-zone Move (single-writer boundary).
	if destZone != "" && destZone != rt.zone.id {
		rt.log.Debug("h:move cross-zone target reserved (no-op)", "rid", e.rid, "dir", dir, "dest", destZone)
		l.Push(lua.LFalse)
		return 1
	}
	to := rt.zone.rooms[destRoom]
	if to == nil {
		l.Push(lua.LFalse)
		return 1
	}
	rt.relocateWithinZone(e, to)
	l.Push(lua.LTrue)
	return 1
}

// hTeleport relocates the handle's entity to the given room handle, WITHIN this zone only. A
// room handle for ANOTHER zone, or a non-room destination, is a clean no-op (no cross-zone
// smuggle, no inject into a non-room). SECURITY (movement-grief): teleporting a NON-CONSENTING
// player is gated through the harm gate (a forced relocation of another player is harm) — only
// the invocation actor itself, a mob, or a consenting/gate-permitted player may be teleported.
// Returns true if the entity moved.
func (rt *luaRuntime) hTeleport(l *lua.LState) int {
	e := resolveHandle(l, 1)
	dest := resolveHandle(l, 2)
	if e == nil || dest == nil || !Has[*Room](dest) {
		l.Push(lua.LFalse)
		return 1
	}
	// Same-zone only: the dest room must be one of THIS zone's rooms.
	if dest.zone != rt.zone || !rt.destIsLocalRoom(dest) {
		rt.log.Debug("h:teleport cross-zone/foreign room reserved (no-op)", "rid", e.rid)
		l.Push(lua.LFalse)
		return 1
	}
	// Movement-grief gate: forcing ANOTHER player to teleport is harm. The harm gate decides.
	if !rt.mayRelocate(e) {
		l.Push(lua.LFalse)
		return 1
	}
	rt.relocateWithinZone(e, dest)
	l.Push(lua.LTrue)
	return 1
}

// hRecall relocates the handle's entity to this zone's start room (the recall point), WITHIN
// this zone only. Same movement-grief gate as teleport: recalling another non-consenting player
// is harm and is gated. Returns true if the entity moved.
func (rt *luaRuntime) hRecall(l *lua.LState) int {
	e := resolveHandle(l, 1)
	if e == nil {
		l.Push(lua.LFalse)
		return 1
	}
	to := rt.zone.rooms[rt.zone.startRoom]
	if to == nil {
		l.Push(lua.LFalse)
		return 1
	}
	if !rt.mayRelocate(e) {
		l.Push(lua.LFalse)
		return 1
	}
	rt.relocateWithinZone(e, to)
	l.Push(lua.LTrue)
	return 1
}

// mayRelocate decides whether the invocation may relocate entity e (teleport/recall). Moving the
// invocation actor itself, or a non-player (a mob/item), is always allowed. FORCING another
// PLAYER to move is harm (a grief vector), so it is gated through guardHarmful against e (the
// same gate dealDamage uses): a non-consenting player in a safe room is not relocated. nil
// invocation => not allowed (fail-closed).
func (rt *luaRuntime) mayRelocate(e *Entity) bool {
	if rt.inv == nil || rt.inv.actor == nil {
		return false
	}
	if e == rt.inv.actor || !isPlayer(e) {
		return true
	}
	return guardHarmful(rt.harmCtx(e), e)
}

// destIsLocalRoom reports whether dest is one of this zone's registered room entities (so a
// teleport target can only be a room the zone owns — never a foreign-zone room smuggled via a
// handle whose payload names this zone but whose rid is a room registered elsewhere).
func (rt *luaRuntime) destIsLocalRoom(dest *Entity) bool {
	for _, r := range rt.zone.rooms {
		if r == dest {
			return true
		}
	}
	return false
}

// relocateWithinZone performs a same-zone containment move + the standard arrival hooks (room
// affects, aggro-on-entry), mirroring the move command's local-move tail. It uses the existing
// Move primitive — NOT a direct contents/location write. Single-writer: zone goroutine. Both e
// and the destination are this zone's (the callers guarantee same-zone), so no cross-zone entity
// is ever dereferenced.
func (rt *luaRuntime) relocateWithinZone(e, dest *Entity) {
	Move(e, dest)
	applyRoomAffectsTo(e)
	rt.zone.aggroOnEntry(e, dest)
}

// --- small table-arg readers (Lua opts tables) --------------------------------------------

// optTableNumber reads a numeric field from a Lua opts table, or def if absent/non-number.
func optTableNumber(t *lua.LTable, key string, def float64) float64 {
	if v, ok := t.RawGetString(key).(lua.LNumber); ok {
		return float64(v)
	}
	return def
}

// optTableString reads a string field from a Lua opts table, or def if absent/non-string.
func optTableString(t *lua.LTable, key string, def string) string {
	if v, ok := t.RawGetString(key).(lua.LString); ok {
		return string(v)
	}
	return def
}
