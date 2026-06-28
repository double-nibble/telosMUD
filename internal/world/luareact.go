package world

import (
	"math"

	lua "github.com/yuin/gopher-lua"
)

// luareact.go — the result-altering REACTION context object (docs/PHASE7-PLAN.md slice 7.9,
// P7-D8, threat row T12). THE surface by which a Lua hook reaches INTO an in-flight action to
// alter or veto it (Counterspell cancels a cast, Shield raises AC for the triggering swing,
// concentration drops itself on a failed save), within the single-writer model and WITHOUT any
// pipeline surgery — Phase 6 deliberately published the named checkpoints; 7.9 only adds the
// alter-capable hook bodies.
//
// The shape is OBSERVE-THEN-RECHECK (the on_depleted death checkpoint's reference shape,
// PRINCIPLES.md): the engine fires the checkpoint with a typed `rx` bound, runs the Lua hook
// SYNCHRONOUSLY INLINE on the zone goroutine, then RE-READS the rx's RECORDED mutations and
// applies them at the seam. The hook never touches the pipeline directly — it only records its
// intent on `rx`, and the engine decides what (if anything) to honor. No raw pipeline pointer
// ever reaches Lua.
//
// THE THREE HARDENING INVARIANTS (T12 — the security crux the security-auditor probes):
//
//  1. FIELD IS A CLOSED PER-CHECKPOINT ENUM, resolved by a Go switch — NOT a free string indexed
//     into entity state. The to-hit checkpoint allows only {"ac"}; OnDamageTaken only {"amount"}.
//     A non-allowlisted field is a SILENT NO-OP (ignored — never an error that mutates or leaks,
//     never a Lua-supplied key reaching a component). reactCheckpoint.allows owns the allowlist;
//     rxModify consults it and drops a non-member modify on the floor.
//
//  2. rx:replace_target RE-RUNS guardHarmful against the NEW target. The original gate ran against
//     the ORIGINAL target; redirecting harm onto a non-consenting player would otherwise bypass
//     the PvP/consent gate. The re-gate (guardHarmful(harmActor, newTarget)) is the SECURITY
//     property — a redirect onto a non-consenting player is gate-BLOCKED. On a PASSED re-gate the
//     OnDamageTaken seam (applyDamageReaction → applyDamageRedirect) re-runs the WHOLE RAW blow
//     against the new target through the SHARED dealDamage pipeline — RE-MITIGATED against the new
//     target's OWN resistances/soak, firing ITS own reactions, threading the SAME eventBudget/depth
//     so a redirect loop terminates — and the original target takes 0. The re-applied blow re-routes
//     the normal dealDamage gate (the harm funnel holds; no direct state write).
//
//  3. THE REACTION PATH THREADS THE SAME eventBudget (effect_op.go:56). A reaction is fired through
//     the SAME fireEvent cascade (the checkpoint is a fired event), so a reaction that triggers a
//     reaction (a loop) is bounded by the SHARED maxEventDepth / maxEventHandlers / the zone-level
//     cascade backstop — no fresh budget, no privileged depth. The rx carries the firing effectCtx
//     so a harm op IN the reaction body inherits that budget (P7-D3 invariant 5), and the loop
//     terminates at the cap instead of spinning the zone goroutine.
//
// Single-writer: every rx method runs on the zone goroutine, inline within the checkpoint fire.

// luaReactTypeName is the metatable registry key for the reaction-context userdata type.
const luaReactTypeName = "telos.reaction"

// reactKind identifies which checkpoint a reaction is firing at — the discriminator the per-
// checkpoint field allowlist (and the cancel semantics) switch on. The engine OWNS this closed
// set; a checkpoint that wants a reaction surface names one here.
type reactKind int

const (
	reactBeforeCastCommit reactKind = iota // an ability is about to commit (Counterspell cancels)
	reactToHit                             // a swing's to-hit is about to roll (Shield raises AC)
	reactOnDamageTaken                     // the subject took damage (concentration drops on a failed save)
)

// reactCheckpoint is the static per-checkpoint policy: which numeric fields rx:modify may touch
// (the CLOSED enum, invariant 1) and whether rx:cancel is meaningful here. It is resolved by a Go
// switch (reactPolicy) — never indexed by a Lua-supplied string. Adding a checkpoint = adding a
// case; the allowlist is the audit surface.
type reactCheckpoint struct {
	allows     map[string]bool // the modify field allowlist for THIS checkpoint (invariant 1)
	canCancel  bool            // whether rx:cancel() does anything at this checkpoint
	canReplace bool            // whether rx:replace_target() does anything here (a harmful pending action)
}

// reactPolicy returns the static policy for a reaction kind — the ONE place the per-checkpoint
// field enum lives (invariant 1). The maps are built per-call (cheap, small) so no shared mutable
// allowlist exists for a script to ever reach; even if it could, it is engine-owned data here, not
// a script value. A kind with no policy (a future checkpoint not yet wired) allows nothing.
func reactPolicy(kind reactKind) reactCheckpoint {
	switch kind {
	case reactBeforeCastCommit:
		// A cast checkpoint: the only result-altering move is to VETO the whole cast. No numeric
		// field is exposed (there is no "amount"/"ac" to nudge on a pending cast), and there is no
		// pending harmful target to retarget at this stage.
		return reactCheckpoint{allows: map[string]bool{}, canCancel: true, canReplace: false}
	case reactToHit:
		// The to-hit checkpoint: a defender may raise its AC for THIS swing only (Shield). The ONLY
		// modifiable field is "ac"; cancel/replace_target are not meaningful (a missed swing is the
		// pipeline's job, not a reaction veto).
		return reactCheckpoint{allows: map[string]bool{"ac": true}, canCancel: false, canReplace: false}
	case reactOnDamageTaken:
		// The damage-taken checkpoint: a reaction may reduce the pending "amount" (a damage-shield
		// soak) and may CANCEL — concentration uses cancel to drop ITSELF on a failed save. A
		// harmful retarget (a redirect of the blow) re-gates against the new target (invariant 2).
		return reactCheckpoint{allows: map[string]bool{"amount": true}, canCancel: true, canReplace: true}
	default:
		return reactCheckpoint{allows: map[string]bool{}, canCancel: false, canReplace: false}
	}
}

// reaction is the engine-owned record of what a Lua reaction hook ASKED to do at a checkpoint —
// the observe side of observe-then-recheck. The Lua hook mutates ONLY this record (through the rx
// methods); the engine reads it back and applies only what the checkpoint permits. It is created
// per checkpoint fire, bound as `rx`, and read once the hook returns.
type reaction struct {
	kind reactKind   // which checkpoint (selects the field allowlist + cancel/replace semantics)
	c    *effectCtx  // the FIRING cascade ctx — the SAME depth/eventBudget the loop is bounded by (invariant 3)
	rt   *luaRuntime // the runtime (for re-gating a replace_target through guardHarmful, invariant 2)

	// harmActor is the ORIGINAL harm originator (the attacker whose blow this checkpoint is about) —
	// who a rx:replace_target re-gate asks "may THIS actor harm the NEW target?" against (invariant 2).
	// It is the original attacker, NOT the reacting subject: redirecting a blow re-gates the ORIGINAL
	// attacker against the new target, so a redirect onto a non-consenting player is blocked exactly as
	// the original harmful op would be. nil at a checkpoint with no harm originator (BeforeCastCommit /
	// to-hit), where replace_target is not a permitted move anyway.
	harmActor *Entity

	// canceled is set by a permitted rx:cancel(); the engine vetoes the in-flight action when true.
	canceled bool
	// newTarget is the entity a PASSED rx:replace_target() re-gate recorded — the entity the harmful
	// blow should be REDIRECTED onto (the OnDamageTaken checkpoint, invariant 2). It is set ONLY after
	// guardHarmful(harmActor, newTarget) PASSED, so the engine never honors a redirect onto a non-
	// consenting player. The seam (applyDamageReaction) reads it back and re-runs the WHOLE blow against
	// newTarget — RE-MITIGATED against ITS OWN resistances/soak, firing ITS OWN OnDamageTaken reactions,
	// threading the SAME eventBudget/depth (so a redirect loop is bounded) — and the ORIGINAL target then
	// takes 0. nil when no permitted redirect was recorded (the common path). A guardian/misdirection
	// mechanic. A second rx:replace_target in the same reaction overwrites (last-writer-wins), still
	// re-gated each time.
	newTarget *Entity
	// mods accumulates the permitted rx:modify(field, delta) deltas, keyed by the (allowlisted)
	// field name. A non-allowlisted field never lands here (rxModify drops it). The engine reads
	// the field it cares about at the seam (e.g. mods["ac"] at to-hit, mods["amount"] at damage).
	mods map[string]float64

	// firingAffect is the affect instance whose on_event_lua handler is CURRENTLY running, when the
	// reaction handler came from an affect (set per-handler in runReactionHandlers, cleared after).
	// At OnDamageTaken it is what rx:cancel() drops — the concentration affect cancels ITSELF on a
	// failed save (the affect drops when concentration breaks). nil when the firing handler is not an
	// affect's (a trigger / a resource handler), so rx:cancel() then falls back to vetoing the
	// triggering effect (the pending damage application). The engine reads cancelAffect at the seam.
	firingAffect *affectInstance
	// cancelAffect is the affect instance rx:cancel() asked to drop (firingAffect captured at the
	// cancel call). The engine expires it at the seam (the concentration break). nil if no affect-
	// scoped cancel happened.
	cancelAffect *affectInstance
}

// modDelta returns the accumulated delta for field (0 if none recorded). The seam reads it for the
// one field that checkpoint exposes.
func (r *reaction) modDelta(field string) float64 {
	if r == nil || r.mods == nil {
		return 0
	}
	return r.mods[field]
}

// installReactType registers the reaction-context userdata type on the runtime's LState, once at
// sandbox build. Like the entity handle (luahandle.go) it carries a pointer-safe __tostring (T15:
// the default tostring(ud) leaks the Go pointer) and a curated method table; the metatable is
// engine-owned and never exposed as a script global — scripts only ever receive an `rx` value the
// engine binds at a checkpoint.
func (rt *luaRuntime) installReactType() {
	L := rt.L
	mt := L.NewTypeMetatable(luaReactTypeName)

	// __tostring (T15): a safe, pointer-free representation — never the raw Go pointer.
	L.SetField(mt, "__tostring", L.NewFunction(func(l *lua.LState) int {
		l.Push(lua.LString("<reaction>"))
		return 1
	}))
	// __metatable: hide the real metatable behind a locked string (belt-and-suspenders alongside
	// the dropped get/setmetatable).
	L.SetField(mt, "__metatable", lua.LString("locked"))

	methods := map[string]lua.LGFunction{
		"cancel":           rt.rxCancel,
		"modify":           rt.rxModify,
		"replace_target":   rt.rxReplaceTarget,
		"consume_resource": rt.rxConsumeResource,
	}
	L.SetField(mt, "__index", L.SetFuncs(L.NewTable(), methods))
}

// newReactionUD mints the `rx` userdata wrapping reaction r. Carries only the engine-owned
// *reaction (never a pipeline pointer); the methods record intent onto it.
func (rt *luaRuntime) newReactionUD(r *reaction) lua.LValue {
	ud := rt.L.NewUserData()
	ud.Value = r
	rt.L.SetMetatable(ud, rt.L.GetTypeMetatable(luaReactTypeName))
	return ud
}

// checkReaction extracts the *reaction payload from the rx userdata at stack index n, or nil if
// the value there is not a reaction context (a script passing a non-rx value to an rx method is a
// clean no-op, never a panic).
func checkReaction(l *lua.LState, n int) *reaction {
	ud, ok := l.Get(n).(*lua.LUserData)
	if !ok {
		return nil
	}
	r, ok := ud.Value.(*reaction)
	if !ok {
		return nil
	}
	return r
}

// --- the rx methods (each records intent onto the reaction; the engine applies at the seam) ---

// rxCancel records a request to CANCEL the in-flight action (the cast at BeforeCastCommit; the
// triggering effect at OnDamageTaken — concentration drops itself). It is honored only at a
// checkpoint whose policy canCancel; at a checkpoint that cannot cancel it is a silent no-op (the
// reaction surface is per-checkpoint, T12). Returns true if the cancel was recorded.
func (rt *luaRuntime) rxCancel(l *lua.LState) int {
	r := checkReaction(l, 1)
	if r == nil {
		l.Push(lua.LFalse)
		return 1
	}
	pol := reactPolicy(r.kind)
	if !pol.canCancel {
		// Not a cancellable checkpoint: drop the request (no error, no mutation) — invariant 1's
		// per-checkpoint discipline applied to cancel.
		l.Push(lua.LFalse)
		return 1
	}
	r.canceled = true
	// If an AFFECT's handler is what's running (concentration), record THAT affect as the one to drop:
	// rx:cancel() at OnDamageTaken means "this affect cancels itself" (concentration breaks). The engine
	// expires r.cancelAffect at the seam. A non-affect handler (a trigger) leaves cancelAffect nil and
	// rx:cancel() falls back to its checkpoint's veto (cancel the cast at BeforeCastCommit; negate the
	// pending damage at OnDamageTaken).
	if r.firingAffect != nil {
		r.cancelAffect = r.firingAffect
	}
	l.Push(lua.LTrue)
	return 1
}

// rxModify records a numeric delta on a PENDING-RESULT field — but ONLY if `field` is in THIS
// checkpoint's closed allowlist (invariant 1, resolved by the Go switch in reactPolicy). A
// non-allowlisted field is a SILENT NO-OP: it is ignored, never an error, never a Lua-supplied key
// reaching entity state. To-hit allows {"ac"}; OnDamageTaken allows {"amount"}. Returns true if the
// modify was recorded (the field was allowlisted), false if it was dropped.
func (rt *luaRuntime) rxModify(l *lua.LState) int {
	r := checkReaction(l, 1)
	field := l.CheckString(2)
	delta := float64(l.CheckNumber(3))
	if r == nil {
		l.Push(lua.LFalse)
		return 1
	}
	// FINITENESS GUARD (security Finding B): a non-finite delta (rx:modify("ac", 1/0) /
	// rx:modify("amount", 0/0)) would flow into int(...) at the seam (implementation-defined) and
	// corrupt the pending result. Drop it SILENTLY — consistent with the non-allowlisted-field
	// no-op discipline (a bad input is ignored, never an error that mutates or leaks).
	if math.IsInf(delta, 0) || math.IsNaN(delta) {
		l.Push(lua.LFalse)
		return 1
	}
	pol := reactPolicy(r.kind)
	if !pol.allows[field] {
		// THE T12 CRUX: a non-allowlisted field is a silent no-op. `field` is NEVER used to index a
		// component or attribute map — it is only ever tested against this engine-owned allowlist,
		// then (if permitted) used as a key into the engine-owned mods map. A typo / a hostile field
		// name reaches nothing.
		l.Push(lua.LFalse)
		return 1
	}
	if r.mods == nil {
		r.mods = map[string]float64{}
	}
	r.mods[field] += delta
	l.Push(lua.LTrue)
	return 1
}

// rxReplaceTarget is the retarget reaction surface — RE-GATED, and (7.9 completion) WIRED to
// REDIRECT the harmful blow at the OnDamageTaken checkpoint:
//
//   - At a checkpoint that cannot retarget, or with no harm originator to re-gate against, it is a
//     clean no-op (false) — fail-closed.
//   - Otherwise it RE-RUNS guardHarmful against the new target (invariant 2): the SECURITY property —
//     a redirect onto a NON-CONSENTING player is gate-BLOCKED exactly as an original harmful op would
//     be. The re-gate runs the SAME funnel with the ORIGINAL harm originator (r.harmActor) as the
//     actor — "may the ATTACKER harm this new target?".
//   - On a PASSED re-gate it RECORDS newTarget on the reaction and returns true. The seam
//     (applyDamageReaction) reads it back and re-runs the WHOLE blow against newTarget — RE-MITIGATED
//     against ITS OWN resistances/soak, firing ITS OWN OnDamageTaken reactions, threading the SAME
//     eventBudget/depth (so a redirect loop terminates at the shared cascade bound) — and the ORIGINAL
//     target then takes 0. A guardian/misdirection mechanic.
//
// The surface stays HONEST: a FAILED re-gate returns false and records nothing (the original target
// keeps the blow); a redirect is only ever honored after the gate that an original harmful op routes.
func (rt *luaRuntime) rxReplaceTarget(l *lua.LState) int {
	r := checkReaction(l, 1)
	newTarget := resolveHandle(l, 2)
	if r == nil || newTarget == nil {
		l.Push(lua.LFalse)
		return 1
	}
	pol := reactPolicy(r.kind)
	if !pol.canReplace {
		l.Push(lua.LFalse) // not a retarget-able checkpoint: clean no-op
		return 1
	}
	// RE-GATE (invariant 2): build a harm ctx whose actor is the ORIGINAL HARM ORIGINATOR (the
	// attacker the checkpoint is about, r.harmActor) and re-run guardHarmful against the NEW target.
	// This is the SAME funnel an original harmful op routes; redirecting the attacker's blow onto a
	// non-consenting player is blocked here exactly as the original harmful op would be, never bypassing
	// the gate. The original attacker (not the reacting subject) is the correct gate actor — "may the
	// ATTACKER harm this new target?" is the question a redirect must re-ask.
	c := r.c
	gateActor := r.harmActor
	if c == nil || gateActor == nil {
		l.Push(lua.LFalse) // no harm originator to re-gate against: deny (fail-closed)
		return 1
	}
	gateCtx := &effectCtx{
		z: c.z, actor: gateActor, source: gateActor, target: newTarget,
		mag: 1, disp: dispHarmful, rng: c.rng, depth: c.depth, eventBudget: c.eventBudget,
	}
	if !guardHarmful(gateCtx, newTarget) {
		// The re-gate denied the redirect (a non-consenting player) — clean no-op, original target stands.
		l.Push(lua.LFalse)
		return 1
	}
	// PASSED the re-gate: record the redirect. The seam (applyDamageReaction) re-runs the blow against
	// newTarget — re-mitigated against ITS resistances, firing ITS reactions, threading the SAME budget
	// — and zeroes the original target. The redirect is authorized; the harm-funnel discipline holds
	// (the re-applied blow routes the normal dealDamage gate again).
	r.newTarget = newTarget
	rt.log.Debug("rx:replace_target redirect recorded (re-gate passed)", "new_target", newTarget.short)
	l.Push(lua.LTrue)
	return 1
}

// rxConsumeResource spends a reaction-economy resource on the reacting entity (the SUBJECT of the
// checkpoint — `self` in the reaction body) — the tie to the [G9] per-round reaction budget. It
// routes the EXISTING opModifyResource handler (the only resource-write path, P7-D3 invariant 3)
// with a negative delta, so a reaction that "costs a reaction" decrements the same `reactions`
// pool topUpReactions refills each round. Signature: rx:consume_resource(ref, n) — n defaults to
// 1. It returns true if at least `n` was available and spent, false otherwise (so a Lua reaction
// can gate "only if I have a reaction left": `if not rx:consume_resource("reactions", 1) then
// return end`). A self-write is never gated (the entity spends its OWN pool).
func (rt *luaRuntime) rxConsumeResource(l *lua.LState) int {
	r := checkReaction(l, 1)
	ref := l.CheckString(2)
	n := l.OptInt(3, 1)
	if r == nil || r.c == nil || r.c.actor == nil || n <= 0 {
		l.Push(lua.LFalse)
		return 1
	}
	subject := r.c.actor // the reacting entity (the checkpoint subject acts as the reaction actor)
	if resourceCurrent(subject, ref) < n {
		l.Push(lua.LFalse) // not enough to spend — the reaction can choose to bail
		return 1
	}
	// Self-write through the existing op handler (the only resource-write path). A self/own-pool
	// write is never gated (guardCrossPlayerWrite's self-exception), but we still route the funnel,
	// never a direct setResourceCurrent.
	c := &effectCtx{
		z: r.c.z, actor: subject, source: subject, target: subject,
		mag: 1, disp: dispNeutral, rng: r.c.rng, depth: r.c.depth, eventBudget: r.c.eventBudget,
	}
	_ = opModifyResource(c, &effectOp{kind: "modify_resource", resource: ref, amount: float64(-n)})
	l.Push(lua.LTrue)
	return 1
}

// --- the engine-side fire: observe-then-recheck at a checkpoint ----------------------------

// fireReaction runs the Lua reaction handlers subscribed (via on(name, fn) or the resource/affect
// on_event_lua columns) to event `kind` about `subject`, with a fresh `rx` bound, and returns the
// resulting *reaction (the recorded mutations) for the engine to apply at the seam. This is the
// OBSERVE phase of observe-then-recheck: it threads the firing cascade ctx `c` (the SAME depth +
// eventBudget pointer, invariant 3), so a reaction that fires a reaction is bounded by the shared
// cascade budget. The RECHECK phase is the caller's: it reads r.canceled / r.modDelta / r.cancelAffect
// and applies what its checkpoint permits.
//
// The reaction surface reuses the Phase-6 event bus (no parallel dispatcher): it gathers the
// subject's Lua handlers exactly like fireEvent's Lua-body path, but binds an additional `rx` so
// the body can alter the result. A non-Lua (op-list) handler at this checkpoint runs declaratively
// as before (it cannot alter — that is the Lua-only hatch); fireEvent already fires those.
func (z *Zone) fireReaction(c *effectCtx, kind eventKind, rk reactKind, subject, other, harmActor *Entity, mag float64) *reaction {
	if z == nil || z.lua == nil || subject == nil {
		return &reaction{kind: rk}
	}
	// ZONE-LEVEL RECURSION BACKSTOP (the can't-forget guard, maxEventCascadeDepth — event.go): a
	// reaction that fires an action that fires a reaction (an OnDamageTaken→damage→OnDamageTaken loop)
	// recurses the Go stack regardless of whether the per-ctx depth was threaded. The same backstop
	// fireEvent uses bounds the LIVE reaction-fire frame count on the goroutine stack, so the loop
	// terminates at the cap instead of spinning the zone (T12 invariant 3). Single-writer: the counter
	// is plain zone-owned int.
	if z.eventCascadeDepth >= maxEventCascadeDepth {
		z.log.Warn("reaction cascade depth backstop tripped; reactions truncated",
			"event", string(kind), "subject", subject.short, "cascade_depth", z.eventCascadeDepth)
		return &reaction{kind: rk, c: c}
	}
	z.eventCascadeDepth++
	defer func() { z.eventCascadeDepth-- }()

	// Depth bound (the per-ctx re-entrancy guard, maxEventDepth): refuse to run reactions once the
	// cascade is too deep, exactly like fireEvent. The handler ctx runs at depth+1 (built below).
	if c != nil && c.depth >= maxEventDepth {
		z.log.Warn("reaction depth budget exhausted; reactions dropped",
			"event", string(kind), "subject", subject.short, "depth", c.depth)
		return &reaction{kind: rk, c: c}
	}
	// Root a shared width budget if this is a cascade root (a reaction fired outside any event cascade —
	// a command-issued cast/swing/damage reaches here with c.eventBudget == nil), so a reaction loop is
	// width-bounded by maxEventHandlers even when it did not inherit a cascade's budget. A reaction
	// fired INSIDE a cascade keeps the inherited (shared) budget — never a fresh one (invariant 3).
	hc := c
	if hc == nil {
		hc = &effectCtx{z: z, depth: 0}
	}
	if hc.eventBudget == nil {
		b := maxEventHandlers
		hc = &effectCtx{
			z: hc.z, actor: hc.actor, source: hc.source, target: hc.target, other: hc.other,
			mag: hc.mag, disp: hc.disp, rng: hc.rng, depth: hc.depth, eventBudget: &b,
		}
	}
	// The handler ctx runs one level deeper (depth+1) so a reaction firing a reaction increments the
	// shared depth and trips maxEventDepth — the same re-entrancy discipline fireEvent applies.
	handlerCtx := &effectCtx{
		z: hc.z, actor: hc.actor, source: hc.source, target: hc.target, other: hc.other,
		mag: hc.mag, disp: hc.disp, rng: hc.rng, depth: hc.depth + 1, eventBudget: hc.eventBudget,
	}
	r := &reaction{kind: rk, c: handlerCtx, rt: z.lua, harmActor: harmActor}
	z.lua.runReactionHandlers(handlerCtx, kind, r, subject, other, mag)
	return r
}

// fireBeforeCastReaction runs the BeforeCastCommit REACTION pass (Counterspell, 7.9). It fires the
// reaction about each eligible OBSERVER in the caster's room (a living entity that is NOT the caster
// — the would-be counterspeller), with the CASTER as the event `other`, mirroring the OnLeaveRoom
// reactor convention. The subject's reaction hook may `rx:cancel()` the cast (and typically
// `rx:consume_resource("reactions", 1)` first). Returns true the moment ANY observer cancels — the
// caller then ABORTS the cast (observe-then-recheck). It threads ONE shared event budget across all
// observers (the same discipline fireLeaveRoom uses), so a roomful of reactors can't blow the
// heartbeat and the reaction loop is bounded (T12 invariant 3). Zone goroutine.
func (z *Zone) fireBeforeCastReaction(parent *effectCtx, caster, target *Entity) bool {
	if z == nil || z.lua == nil || caster == nil || caster.location == nil {
		return false
	}
	// One shared budget across every observer's reaction (matches fireLeaveRoom); a nil parent roots a
	// fresh cascade.
	if parent == nil || parent.eventBudget == nil {
		budget := maxEventHandlers
		parent = &effectCtx{z: z, actor: caster, source: caster, mag: 1, disp: dispNeutral, rng: z.lua.rng, eventBudget: &budget}
	}
	// Snapshot the observers first (a hook could mutate room contents).
	var observers []*Entity
	for _, e := range caster.location.contents {
		if e != caster && e.living != nil {
			observers = append(observers, e)
		}
	}
	for _, obs := range observers {
		// Re-validate per observer: a prior hook may have moved/killed it or the caster.
		if obs.living == nil || obs.location == nil || caster.living == nil || caster.location == nil ||
			obs.location != caster.location {
			continue
		}
		// The reaction fires about the OBSERVER (subject) with the caster as `other` — so the
		// observer's own on(BeforeCastCommit, ...) hook runs and can read ev.other (the caster) and
		// cancel. The firing ctx's actor is the observer (a consume_resource spends the observer's
		// reaction pool; a harm op would be attributed to + gated for the observer).
		c := &effectCtx{
			z: z, actor: obs, source: obs, target: obs, other: caster,
			mag: 1, disp: dispNeutral, rng: parent.rng, depth: parent.depth, eventBudget: parent.eventBudget,
		}
		r := z.fireReaction(c, evBeforeCastCommit, reactBeforeCastCommit, obs, caster, nil, 1)
		if r.canceled {
			return true // a counterspell vetoed the cast; abort (observe-then-recheck)
		}
	}
	return false
}

// fireToHitReaction runs the to-hit REACTION pass (Shield, 7.9) about the DEFENDER (subject =
// target, other = attacker) and returns the transient AC delta the defender's reaction recorded
// (the only allowlisted modify field here is "ac", luareact.go). The swing pipeline threads this
// delta into the to-hit DC for THIS swing only (combat.go / check.go). It threads the swing ctx
// `c` (the round-shared eventBudget — T12 invariant 3) so a reaction can't blow the heartbeat.
// Returns 0 when no reaction raised AC (the common path — a swing with no defender reaction). Zone
// goroutine.
func (z *Zone) fireToHitReaction(c *effectCtx, attacker, target *Entity) float64 {
	if z == nil || z.lua == nil || target == nil || c == nil {
		return 0
	}
	// The reaction fires about the DEFENDER (subject) with the attacker as `other`. The firing ctx's
	// actor is the defender (a consume_resource spends the DEFENDER's reaction pool; the reaction is
	// attributed to it). It threads the swing's eventBudget so the to-hit reaction shares the round's
	// width budget.
	rc := &effectCtx{
		z: z, actor: target, source: target, target: target, other: attacker,
		mag: 1, disp: dispNeutral, rng: c.rng, depth: c.depth, eventBudget: c.eventBudget,
	}
	r := z.fireReaction(rc, evToHit, reactToHit, target, attacker, nil, 1)
	return r.modDelta("ac")
}

// applyDamageReaction runs the OnDamageTaken REACTION pass about the damage TAKER `target` with the
// pending (post-mitigation) damage `dmg` as the event mag, and returns the damage to actually apply
// to `target` after the reaction's recorded mutations (observe-then-recheck). Four result-altering
// moves are honored at this checkpoint (luareact.go's reactPolicy(reactOnDamageTaken)):
//
//   - rx:modify("amount", delta) — the SOLE allowlisted modify field here. CONTRACT (combat Finding):
//     "amount" is a DAMAGE-SHIELD — REDUCE-ONLY. Only a NEGATIVE delta is honored (a positive delta
//     is dropped at the seam), so a reaction can soak the pending blow but CANNOT amplify it past the
//     original mitigated amount. A vulnerability/amplification reaction is deliberately NOT this
//     slice's surface (it would let a reaction make a blow harder than the original op — a separate,
//     auditable capability if ever wanted). The reduced amount is clamped to >= 0.
//   - rx:cancel() WITHOUT an affect handler firing — negate the blow entirely (return 0).
//   - rx:cancel() FROM a concentration affect's handler — drop THAT affect (the concentration break,
//     r.cancelAffect); the damage itself still lands (concentration breaking doesn't soak the hit).
//   - rx:replace_target(handle) (7.9 completion) — REDIRECT the whole blow. On a PASSED re-gate the
//     reaction recorded r.newTarget; this seam re-runs the SAME RAW blow against newTarget through
//     dealDamage (RE-MITIGATED against newTarget's OWN resistances/soak, firing newTarget's OWN
//     OnDamageTaken reactions), threading the SAME eventBudget/depth, and returns 0 so the ORIGINAL
//     target takes nothing. See applyDamageRedirect.
//
// It threads `c` (the SAME eventBudget — T12 invariant 3) so an OnDamageTaken→reaction→damage loop
// is bounded by the shared cascade budget. `raw`/`dmgType` are the PRE-mitigation blow (needed so a
// redirect re-mitigates against newTarget's resistances, not the original target's). Zone goroutine.
func (z *Zone) applyDamageReaction(c *effectCtx, target *Entity, dmg int, raw float64, dmgType string) int {
	if z == nil || z.lua == nil || target == nil {
		return dmg
	}
	// Fire about the target (subject), with the attacker (c.source) as `other`, pending damage as mag.
	// The firing ctx threads this damage cascade's budget so the reaction shares its width bound.
	rc := &effectCtx{
		z: z, actor: target, source: target, target: target, other: c.source,
		mag: float64(dmg), disp: dispNeutral, rng: c.rng, depth: c.depth, eventBudget: c.eventBudget,
	}
	r := z.fireReaction(rc, evOnDamageTaken, reactOnDamageTaken, target, c.source, c.source, float64(dmg))

	// REDIRECT (7.9 completion): a PASSED rx:replace_target re-gate recorded r.newTarget. Re-run the
	// WHOLE RAW blow against newTarget — re-mitigated against ITS resistances, firing ITS reactions,
	// threading the SAME cascade budget so a redirect loop terminates — and return 0 so the ORIGINAL
	// target takes nothing. The redirect SUPERSEDES the original target's outcome and is honored BEFORE
	// the cancel/amount/concentration handling below: those mutate what the ORIGINAL target takes, but
	// the original target takes nothing once the blow moves. A concentration break is deliberately NOT
	// applied here — the subject did NOT take the damage (it redirected away), so its concentration is
	// not jeopardized by a blow it never absorbed.
	if r.newTarget != nil {
		z.applyDamageRedirect(c, target, r, raw, dmgType)
		return 0
	}

	// Concentration break (affect-scoped cancel): drop the affect the reaction canceled, by ROUTING THE
	// EXISTING opRemoveAffect op handler (the only affect-removal path, P7-D3 invariant 3) — never a
	// direct a.expire. This is a SELF cleanse (the entity drops its own concentration), so it is ungated
	// (guardCrossPlayerWrite's self-exception); the funnel still owns the removal. The damage still
	// lands — concentration breaking is a consequence of the hit, not a soak of it.
	if r.cancelAffect != nil && r.cancelAffect.def != nil {
		rc := &effectCtx{
			z: z, actor: target, source: r.cancelAffect.source, target: target,
			mag: 1, disp: dispNeutral, rng: c.rng, depth: c.depth, eventBudget: c.eventBudget,
		}
		_ = opRemoveAffect(rc, &effectOp{kind: "remove_affect", affect: r.cancelAffect.def.ref})
		z.log.Debug("concentration broke (affect dropped via opRemoveAffect by OnDamageTaken reaction)",
			"ref", r.cancelAffect.def.ref, "rid", target.rid)
	}

	// Apply the "amount" modify delta — REDUCE-ONLY (the damage-shield contract, combat Finding). A
	// positive delta is dropped (a reaction may soak, never amplify past the original mitigated
	// amount); a negative delta reduces, clamped at >= 0.
	if d := r.modDelta("amount"); d < 0 {
		dmg += int(d)
		if dmg < 0 {
			dmg = 0
		}
	}
	// A non-affect-scoped rx:cancel() negates the pending damage entirely (a reaction that says "this
	// blow does not land"). An affect-scoped cancel (concentration) does NOT negate the damage — it
	// only dropped the affect above — so we negate only when no affect was the cancel's subject.
	if r.canceled && r.cancelAffect == nil {
		return 0
	}
	return dmg
}

// applyDamageRedirect lands a redirected harmful blow on `newTarget` (recorded by a PASSED
// rx:replace_target re-gate, r.newTarget) and is the seam that makes the redirect HONEST: it re-runs
// the WHOLE RAW blow through the SHARED dealDamage pipeline against newTarget, so the blow is
// RE-MITIGATED against newTarget's OWN resistances/soak (NOT the original target's), fires newTarget's
// OWN OnDamageTaken reactions/affects, builds threat + the lit combat events for newTarget, and can
// kill newTarget through the uniform death seam — exactly as if the original op had targeted newTarget.
//
// RE-ENTRANCY BOUND (the load-bearing safety req, T12 invariant 3): the redirect ctx threads the SAME
// eventBudget pointer and the SAME depth as the firing reaction's ctx (r.c) — it ROOTS NO fresh budget
// and grants NO privileged depth. So an A→B→A redirect loop (B's reaction redirects back to A, whose
// reaction redirects to B, …) re-enters dealDamage at ever-deeper cascade levels and TERMINATES at the
// shared maxEventDepth / maxEventHandlers width budget, and — as a can't-forget backstop independent of
// whether the budget was threaded — the zone-level eventCascadeDepth bound (fireReaction) truncates the
// reaction fire. A redirect loop returns (truncated with a warning), never crashes or spins the zone.
//
// HARM FUNNEL: the re-applied blow routes the NORMAL dealDamage gate (guardHarmful) again — the
// rx:replace_target re-gate authorized the redirect, and dealDamage re-gates the actual application, so
// no direct entity-state write happens here (the binding-funnel lint stays green). The ORIGINAL harm
// originator (c.source / c.actor) is preserved as the attribution so threat/OnHit/death attribute to the
// real attacker, not the redirecting subject. Zone goroutine.
func (z *Zone) applyDamageRedirect(c *effectCtx, origTarget *Entity, r *reaction, raw float64, dmgType string) {
	newTarget := r.newTarget
	if newTarget == nil || newTarget.living == nil {
		return
	}
	// Thread the SAME budget + depth as the firing reaction ctx (r.c) — never a fresh root, never a
	// privileged depth — so the redirect re-enters dealDamage under the shared cascade bound (T12
	// invariant 3). Preserve the ORIGINAL attacker (c.actor/c.source) as the actor/source so the
	// redirected blow is attributed + gated exactly as the original harmful op (threat/OnHit/death go to
	// the real attacker, and guardHarmful re-runs with the attacker as the gate actor).
	rc := &effectCtx{
		z: z, actor: c.actor, source: c.source, target: newTarget,
		// mag carried for ctx-shape parity only — it is NOT re-applied on this path: dealDamage takes
		// `raw` directly (crit already baked in) and never re-multiplies by ctx mag. Do NOT route the
		// redirect back through opDealDamage (which does `raw *= mag`) without dropping mag here, or the
		// blow would double-scale.
		mag: c.mag, disp: dispHarmful, rng: c.rng,
		depth: depthOf(r.c, c), eventBudget: budgetOf(r.c, c),
	}
	z.log.Debug("rx:replace_target redirect: re-running blow against new target",
		"orig", origTarget.short, "new", newTarget.short, "raw", raw, "type", dmgType)
	_ = dealDamage(rc, newTarget, raw, dmgType)
}

// depthOf returns the firing reaction ctx's depth when present (so the redirect re-enters one level
// into the SAME cascade), else the firing damage ctx's depth — never a privileged 0 inside a cascade.
func depthOf(reactionCtx, dmgCtx *effectCtx) int {
	if reactionCtx != nil {
		return reactionCtx.depth
	}
	return dmgCtx.depth
}

// budgetOf returns the firing reaction ctx's SHARED eventBudget pointer when present (the redirect
// must inherit it — invariant 3), else the firing damage ctx's. Never allocates a fresh budget.
func budgetOf(reactionCtx, dmgCtx *effectCtx) *int {
	if reactionCtx != nil && reactionCtx.eventBudget != nil {
		return reactionCtx.eventBudget
	}
	return dmgCtx.eventBudget
}

// runReactionHandlers gathers and runs the subject's Lua reaction handlers for `kind`, binding the
// shared `rx` (the reaction record) into each so the body can record cancel/modify/replace_target/
// consume_resource. It threads the firing cascade ctx `c` (depth + the SAME eventBudget pointer) so
// a reaction loop is bounded by the shared width/depth budget — there is no fresh budget here. It
// decrements the SAME shared budget per handler run, refusing to run once exhausted (the same
// truncation fireEvent applies). Zone goroutine only.
func (rt *luaRuntime) runReactionHandlers(c *effectCtx, kind eventKind, r *reaction, subject, other *Entity, mag float64) {
	if rt == nil || rt.L == nil || subject == nil {
		return
	}
	rx := rt.newReactionUD(r)
	// Gather the subject's AFFECT-sourced Lua reaction handlers for this kind, carrying the firing
	// affect INSTANCE so rx:cancel() can drop THAT affect (concentration cancels itself, luareact.go).
	// A snapshot is taken first because a handler can expire affects (mutating a.list mid-iteration).
	for _, fa := range gatherAffectReactions(subject, kind) {
		if rt.budgetExhausted(c, kind, subject) {
			return
		}
		ch := rt.chunkFor("react:affect:"+fa.def.ref, fa.luaSrc)
		if ch == nil {
			continue
		}
		// Set the firing affect so rx:cancel() drops it; cleared after the handler returns so a later
		// handler's rx:cancel() doesn't spuriously target this one.
		r.firingAffect = fa.inst
		ev := rt.reactEvTable(other, mag)
		_ = rt.invokeFromCtx(ch, c, subject, map[string]lua.LValue{"ev": ev, "rx": rx})
		r.firingAffect = nil
	}
	// Resource-sourced Lua reaction handlers (a `reactions`-style pool carrying an on_event_lua for
	// this kind). These are not affect-scoped, so rx:cancel() falls back to the checkpoint veto.
	for origin, src := range gatherResourceReactions(subject, kind) {
		if rt.budgetExhausted(c, kind, subject) {
			return
		}
		ch := rt.chunkFor("react:"+origin, src)
		if ch == nil {
			continue
		}
		ev := rt.reactEvTable(other, mag)
		_ = rt.invokeFromCtx(ch, c, subject, map[string]lua.LValue{"ev": ev, "rx": rx})
	}
	// The subject's per-instance on(kind, fn) trigger handler (a player/mob carrying a Shield/
	// Counterspell reaction registered as a trigger).
	rt.runReactionTrigger(c, kind, rx, subject, other, mag)
}

// budgetExhausted decrements the shared cascade eventBudget for one reaction-handler run and reports
// whether the budget is spent (T12 invariant 3). A nil budget (a root reaction outside a cascade) is
// never exhausted by this check — but the firing path always roots a fresh budget, so in practice a
// reaction loop is always bounded. Logs the truncation, matching fireEvent's discipline.
func (rt *luaRuntime) budgetExhausted(c *effectCtx, kind eventKind, subject *Entity) bool {
	if c.eventBudget == nil {
		return false
	}
	if *c.eventBudget <= 0 {
		rt.log.Warn("reaction handler budget exhausted; reactions truncated",
			"event", string(kind), "subject", subject.short)
		return true
	}
	*c.eventBudget--
	return false
}

// reactEvTable builds the read-only `ev` table a reaction body reads: the counterpart handle (the
// caster at BeforeCastCommit, the attacker at to-hit/OnDamageTaken) + the event magnitude (the
// pending damage at OnDamageTaken). A reaction reads `ev`, alters via `rx`.
func (rt *luaRuntime) reactEvTable(other *Entity, mag float64) *lua.LTable {
	t := rt.L.NewTable()
	if other != nil {
		t.RawSetString("other", rt.newHandle(other))
	}
	t.RawSetString("mag", lua.LNumber(mag))
	return t
}

// firingAffectReaction pairs an affect instance with its Lua reaction body for a kind — so the
// reaction loop can set reaction.firingAffect (what rx:cancel() drops).
type firingAffectReaction struct {
	inst   *affectInstance
	def    *affectDef
	luaSrc string
}

// gatherAffectReactions snapshots the subject's active affects that carry a Lua on_event_lua handler
// for `kind`, paired with their instances (so rx:cancel() can drop the firing affect). A snapshot
// (not a live walk) because a handler can expire affects mid-iteration. Zone goroutine read.
func gatherAffectReactions(e *Entity, kind eventKind) []firingAffectReaction {
	if e == nil {
		return nil
	}
	a, ok := Get[*Affected](e)
	if !ok {
		return nil
	}
	var out []firingAffectReaction
	for _, inst := range a.list {
		if inst == nil || inst.def == nil || inst.def.onReactionLua == nil {
			continue
		}
		if src := inst.def.onReactionLua[kind]; src != "" {
			out = append(out, firingAffectReaction{inst: inst, def: inst.def, luaSrc: src})
		}
	}
	return out
}

// gatherResourceReactions collects the subject's RESOURCE-sourced Lua reaction handlers for `kind`
// (a pool whose def carries an on_event_lua for the kind and which the entity has), keyed by origin
// for the compile cache. Resource reactions are not affect-scoped, so rx:cancel() from one falls
// back to the checkpoint veto. Zone goroutine read.
func gatherResourceReactions(e *Entity, kind eventKind) map[string]string {
	if e == nil || e.zone == nil {
		return nil
	}
	out := map[string]string{}
	for ref, def := range e.zone.resourceDefs().table() {
		if def.onReactionLua == nil {
			continue
		}
		if src := def.onReactionLua[kind]; src != "" && entityHasResource(e, ref) {
			out["resource:"+ref] = src
		}
	}
	return out
}

// runReactionTrigger runs the subject's on(kind, fn) trigger handler (if any) as a reaction, with
// `rx` bound. This is how a player-carried reaction (Shield/Counterspell registered as a trigger,
// or a concentration self-cancel) reaches the checkpoint. It threads the firing cascade ctx via the
// per-instance breaker key (one buggy instance is quarantined, not its prototype). Zone goroutine.
func (rt *luaRuntime) runReactionTrigger(c *effectCtx, kind eventKind, rx lua.LValue, subject, other *Entity, mag float64) {
	es := rt.ensureEntityScript(subject)
	if es == nil {
		return
	}
	h, ok := es.handlers.RawGetString(string(kind)).(*lua.LFunction)
	if !ok {
		return
	}
	if c.eventBudget != nil {
		if *c.eventBudget <= 0 {
			rt.log.Warn("reaction trigger budget exhausted; reaction skipped",
				"event", string(kind), "subject", subject.short)
			return
		}
		*c.eventBudget--
	}
	ev := rt.evTable(other, "")
	ev.RawSetString("mag", lua.LNumber(mag))
	binds := map[string]lua.LValue{
		"self":  rt.newHandle(subject),
		"state": es.state,
		"ev":    ev,
		"rx":    rx,
	}
	rt.fireReactionTriggerCall(es, h, c, binds)
}

// fireReactionTriggerCall invokes a stored reaction trigger handler under the per-instance breaker
// + the chokepoint, threading the firing cascade ctx `c` (depth + the SAME eventBudget pointer).
// Mirrors fireTriggerCall but the handler signature is on(kind, function(ev, rx) ... end) — `ev`
// and `rx` are both passed (the body reads `ev`, alters via `rx`). Pcall-isolated.
func (rt *luaRuntime) fireReactionTriggerCall(es *entityScript, h *lua.LFunction, c *effectCtx, binds map[string]lua.LValue) {
	key := breakerKeyInstance(es.rid)
	if rt.breakerDisabled(key) {
		return
	}
	L := rt.L
	env := rt.freshCallEnv(binds)
	h.Env = env

	inv := &luaInvocation{actor: c.actor, depth: c.depth, eventBudget: c.eventBudget, breakerKey: key}
	prev := rt.inv
	rt.inv = inv
	defer func() { rt.inv = prev }()

	// The handler is on(kind, function(ev, rx) ... end): push ev then rx (two args, no return).
	L.Push(h)
	L.Push(binds["ev"])
	L.Push(binds["rx"])
	if err := rt.pcallGuarded(key, "react:#"+ridStr(es.rid), 2, 0); err != nil {
		rt.log.Warn("lua reaction error (isolated; action unaltered, zone unaffected)",
			"event", "reaction", "rid", es.rid, "err", err.Error())
	}
}
