package world

// effect_op_handlers.go holds the registered effect-op handlers (the P5-D2 v1 op set, docs/PHASE5-
// PLAN.md §1.3). Each is a `func(*effectCtx, *effectOp) error` registered in effectOpHandlers
// (effect_op.go). They run on the zone goroutine in lifecycle step 8 (or an affect tick), single-
// writer, never blocking.
//
// EVERY op that writes another (non-self) PLAYER's state routes through the ONE shared chokepoint
// guardHarmful — the harm decision is DERIVED, never trusted from a content label (§7/D2):
//   - deal_damage      -> dealDamage()  -> guardHarmful() + the mitigation pipeline
//   - apply_affect     -> applyDebuff() -> guardHarmful() when op.harmful || disp==harmful ||
//                         affectIsDetrimental(def) (derived from the def: stat reductions / prevents)
//   - dispel           -> guardHarmful() when the target is another player (stripping their buffs)
//   - remove_affect    -> guardHarmful() when the target is another player (stripping their buffs)
//   - modify_resource  -> guardHarmful() on ANY cross-player write (any sign — a "corruption" pool)
// A new such op author calls dealDamage/applyDebuff or the cross-player guard and physically cannot
// forget the gate (the can't-bypass property — see effect_op.go's header).
//
// HELPFUL/NEUTRAL ops never touch the gate — the gate is for HARM only (§7). heal/restore is the
// deliberate exception to "gate every cross-player resource write": it is structurally beneficial
// (clamped non-negative, only raises toward max), so healing an ally stays ungated (see opHeal).

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
)

// randIntn is the package-default rng draw (math/rand) used when a ctx carries no injected rng. Tests
// inject a seeded rng for determinism; production uses this.
func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	return rand.Intn(n) //nolint:gosec // seeded for gameplay determinism, not security
}

// rollOpAmount evaluates the shared `amount + dice + bonus`, magnitude-scaled amount used by both the
// harmful (deal_damage) and helpful (heal/restore) ops, so the two can't drift. The amount is a flat
// `amount` plus rolled dice (a literal diceNum, or a content-formula dice COUNT — [G-A], a level-scaled
// rider — that overrides it; rollDice caps the count at maxDice so a runaway formula can't spin the zone
// goroutine) plus a scoped `bonus` formula (`+ $actor.damroll + str_bonus`, defaulting to the ACTOR
// scope — what lets a sword add STR, a heal read WIS, a combo-finisher read combo_points, all as CONTENT
// not Lua), all multiplied by the ctx magnitude (a DoT's stacks). Callers apply their own sign/clamp
// policy (heal clamps non-negative) after this.
func rollOpAmount(c *effectCtx, op *effectOp) float64 {
	amt := op.amount
	num := op.diceNum
	if op.diceCount != nil {
		num = int(evalCheckFormula(c, op.diceCount, c.actor))
	}
	// CRIT DICE-DOUBLING (#544): on a crit the DICE COUNT is multiplied (2d8 instead of 1d8) so the roll
	// gains extra NdS with the correct variance — NOT a constant scale of one roll. Applied to the dice
	// COUNT only, so the flat `amount` and the scoped `bonus` below are added ONCE (the 5e rule: a crit
	// doubles the dice, not the modifier). Rolling `num*mult` dice of the same size is distributionally
	// identical to rolling the term `mult` times. Inert (mult <= 1) outside a crit; the whole-roll
	// `crit_mult` path composes separately through c.mag below. NOTE: the doubled count is still subject to
	// rollDice's maxDice ceiling (the anti-spin bound), so a [G-A] dice_count formula already near maxDice
	// loses its crit doubling to the clamp — a crit adds no extra dice at the very top of the scaling
	// curve. Acceptable: that ceiling exists to stop a runaway count from spinning the zone goroutine, and
	// degrading to fewer dice is the safe direction. Realistic content is far below the cap.
	if c.critDiceMult > 1 && num > 0 {
		num *= c.critDiceMult
	}
	if num > 0 && op.diceSize > 0 {
		amt += float64(rollDice(c, num, op.diceSize))
	}
	if op.bonus != nil {
		amt += evalCheckFormula(c, op.bonus, c.actor)
	}
	if c.mag > 0 {
		amt *= c.mag
	}
	return amt
}

// opDealDamage: deal_damage(target, {amount|<N>d<S>, type, resource}). Routes through the SHARED
// mitigation pipeline (dealDamage -> guardHarmful + resist/soak). The amount is either a flat `amount`
// or rolled dice, scaled by the ctx magnitude (a DoT's stacks). The optional `resource` names WHICH
// vital/resource pool the blow hits (#71 multi-vital); empty = the primary vital (a swing). A blocked
// harmful op (PvP) is a clean no-op.
func opDealDamage(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("deal_damage: no target")
	}
	dealDamage(c, c.target, rollOpAmount(c, op), op.dmgType, op.resource)
	return nil
}

// opHeal: heal(target, resource, amount). A HELPFUL op — never gated, including on another player
// (healing an ally is a real, sanctioned use case). DECISION (§7/D2): heal/restore is the deliberate
// exception to "gate every cross-player resource write" because it is structurally beneficial — its
// amount is clamped to a non-negative magnitude here (a negative `amount` cannot weaponize heal into a
// drain) and setResourceCurrent only ever RAISES toward the derived max, never crossing toward 0. A
// content author who wants to subtract from another player's pool must use modify_resource, which IS
// gated (any sign). So heal cannot be turned into a cross-player harm. Scaled by the ctx magnitude.
func opHeal(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("heal: no target")
	}
	if op.resource == "" {
		return fmt.Errorf("heal: no resource")
	}
	// Amount: flat `amount` + rolled dice (literal diceNum/diceSize, or a diceCount formula) + a scoped
	// `bonus` formula via the SHARED rollOpAmount — mirroring opDealDamage so a restorative op can roll
	// `2d8 + $actor.wis_bonus` exactly like a strike rolls `1d8 + $actor.str_bonus` (docs/REMAINING.md §4).
	// Dice/bonus are scoped to the ACTOR (the healer), so `+WIS` reads the caster's wisdom. restore
	// delegates here, so it inherits the same dice form.
	amt := rollOpAmount(c, op)
	// heal only ever RAISES a pool: a negative amount cannot weaponize it into a cross-player drain
	// (that path is modify_resource, which is gated). Clamp the magnitude non-negative.
	if amt < 0 {
		amt = 0
	}
	cur := resourceCurrent(c.target, op.resource)
	setResourceCurrent(c.target, op.resource, cur+int(amt))
	return nil
}

// opRestore: restore(target, resource, amount). Identical mechanics to heal this phase (raise a pool);
// kept as a distinct op so content can express "restore mana" vs "heal hp" with the right verb and a
// later slice can differentiate (e.g. restore ignores healing-reduction debuffs). Helpful — never gated.
func opRestore(c *effectCtx, op *effectOp) error { return opHeal(c, op) }

// opModifyResource: modify_resource(target, resource, delta). A signed delta on a pool. ANY write to
// ANOTHER player's resource pool is gated through the ONE shared guardHarmful, regardless of sign — a
// negative delta is an obvious drain, but a POSITIVE delta to a content-defined "corruption"/"heat"/
// "doom" pool is just as harmful (§7/D2: the engine can't know a content pool's polarity, so every
// cross-player resource WRITE is gated; the safe default the auditor recommended). A self-target or a
// mob target is ungated. This is the harmful-resource path through the same funnel as deal_damage.
func opModifyResource(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("modify_resource: no target")
	}
	if op.resource == "" {
		return fmt.Errorf("modify_resource: no resource")
	}
	// Any resource write on another PLAYER (any sign) is gated: the engine can't know a content pool's
	// polarity, so a positive delta to a "corruption" pool is treated as potential harm.
	if !guardCrossPlayerWrite(c, c.target) {
		return nil // clean no-op on a gated block
	}
	delta := int(op.amount)
	cur := resourceCurrent(c.target, op.resource)
	setResourceCurrent(c.target, op.resource, cur+delta)
	return nil
}

// opSetResource: set_resource(target, resource, amount, mode). Writes a pool to an ABSOLUTE value (mode
// "set"/"absolute"/"", the default) or the HIGHER of current-and-amount (mode "take_higher"/"higher" —
// temp HP's non-stacking re-cast, where a fresh 2d4+CON replaces the old only if it rolls higher). The
// amount is rolled via the SHARED rollOpAmount (flat `amount` + dice + a scoped `bonus` formula), so a
// ward is `2d4 + $actor.con_bonus`, clamped non-negative. This is the take-higher / set-absolute write the
// absorb-buffer primitive (#536) needs — modify_resource is strictly additive (cur+delta), which cannot
// express "roll a NEW temp HP amount, keep the higher."
//
// GATED like modify_resource: the engine cannot know a content pool's polarity (setting a "corruption"
// pool high is harm; a "temp_hp" buffer is a boon), so ANY cross-player write routes through the ONE
// shared guardCrossPlayerWrite and no-ops on a deny. A SELF write (a self-shield) is ungated; an ally
// shield needs the same PvP consent every cross-player write does.
func opSetResource(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("set_resource: no target")
	}
	if op.resource == "" {
		return fmt.Errorf("set_resource: no resource")
	}
	if !guardCrossPlayerWrite(c, c.target) {
		return nil // clean no-op on a gated block
	}
	amt := int(rollOpAmount(c, op))
	if amt < 0 {
		amt = 0 // a pool current is non-negative; a negative roll floors to 0 (setResourceCurrent clamps too)
	}
	switch op.mode {
	case "take_higher", "higher":
		if cur := resourceCurrent(c.target, op.resource); amt > cur {
			setResourceCurrent(c.target, op.resource, amt)
		}
	default: // "set" / "absolute" / ""
		setResourceCurrent(c.target, op.resource, amt)
	}
	return nil
}

// opApplyAffect: apply_affect(target, id, {duration, magnitude}). Whether the apply is GATED is
// DERIVED from the affect def (affectIsDetrimental) — never trusted from the content label alone
// (§7/D2: a detrimental affect mislabeled helpful/neutral/unlabeled must NOT land on a protected player
// ungated). The op routes through the gated applyDebuff -> guardHarmful when the op is explicitly
// harmful OR the ability's disposition is harmful OR the affect is derived-detrimental (a stat-reducing
// or prevents/affliction affect). The label stays an OR so an author can still FORCE-gate, but can
// never un-gate a genuine debuff. A genuinely-beneficial affect (no stat reductions, no prevents) on
// another player stays ungated (a buff on an ally). The source is the EFFECT source (the caster, or a
// DoT's applier), so per-source stacking keys correctly.
func opApplyAffect(c *effectCtx, op *effectOp) error {
	if op.affect == "" {
		return fmt.Errorf("apply_affect: no affect ref")
	}
	// [G13] room-scoped affect: a room affect (web/darkness/...) attaches to the actor's ROOM entity,
	// not to a creature, and lands on the room's occupants + entrants. The interpreter detects the
	// room-scoped def and routes to applyRoomAffect (the per-occupant harm funnel lives inside it), so
	// a single `apply_affect: web` op authors a room field — no separate op kind. The applier is the
	// effect source (the caster), keying the field per-applier.
	if def := c.z.affectDefs().get(op.affect); def != nil && def.roomScoped {
		room := c.actor.location
		if room == nil {
			return fmt.Errorf("apply_affect (room): actor has no room")
		}
		applyRoomAffect(room, op.affect, c.source)
		return nil
	}
	if c.target == nil {
		return fmt.Errorf("apply_affect: no target")
	}
	opts := attachOpts{source: c.source, duration: op.duration, magnitude: op.magnitude}
	def := c.target.zone.affectDefs().get(op.affect)
	detrimental := affectIsDetrimental(def, c.target.zone.harmPolarity())
	if op.harmful || c.disp == dispHarmful || detrimental {
		c.z.log.Debug("apply_affect routed through gate (derived harm)", "affect", op.affect,
			"op_harmful", op.harmful, "disp", int(c.disp), "derived", detrimental)
		applyDebuff(c, c.target, op.affect, opts)
		return nil
	}
	applyAffect(c.target, op.affect, opts, c) // thread the cascade ctx (bounds a nested OnApplyAffect)
	return nil
}

// opRemoveAffect: remove_affect(target, id). Removes a single affect instance (the self/ally cleanse
// case). Stripping an affect off ANOTHER player is HARM (you can rip their protective buff), so on a
// non-self player target it routes through the ONE shared guardHarmful and aborts cleanly on a deny —
// the same funnel deal_damage uses. Self/ally/mob cleanse stays ungated. Keyed per-source by the source.
func opRemoveAffect(c *effectCtx, op *effectOp) error {
	if c.target == nil || c.target.living == nil {
		return fmt.Errorf("remove_affect: no living target")
	}
	if op.affect == "" {
		return fmt.Errorf("remove_affect: no affect ref")
	}
	// Stripping an affect off another PLAYER is harm: gate it through the single funnel.
	if !guardCrossPlayerWrite(c, c.target) {
		return nil // clean no-op on a gated block
	}
	a, ok := Get[*Affected](c.target)
	if !ok {
		return nil
	}
	def := c.target.zone.affectDefs().get(op.affect)
	if def == nil {
		return nil
	}
	if inst, present := a.byKey[keyFor(def, c.source)]; present {
		a.expire(c.target, inst, c) // thread the cascade ctx (bounds a nested OnAffectExpire)
	}
	return nil
}

// opIncrementRung: increment_rung(target, affect, [amount]). Raises a LADDER affect's rung by `amount`
// (default 1), APPLYING the affect at that rung if the target doesn't have it yet (the first exhaustion
// level). Raising exhaustion is HARM, so a cross-player increment routes through the ONE shared
// guardCrossPlayerWrite and no-ops on a deny. The rung is clamped to the top; it never overflows.
func opIncrementRung(c *effectCtx, op *effectOp) error { return adjustRung(c, op, +1) }

// opDecrementRung: decrement_rung(target, affect, [amount]). Lowers a LADDER affect's rung by `amount`
// (default 1) — the exhaustion "recover 1 on a long rest" step. Dropping BELOW rung 1 removes the affect
// (fully recovered). Recovery is HELPFUL, so it is ungated (like heal). A no-op if the target lacks the affect.
func opDecrementRung(c *effectCtx, op *effectOp) error { return adjustRung(c, op, -1) }

// adjustRung moves a ladder affect's rung by sign*amount (#541). Shared by increment/decrement_rung.
func adjustRung(c *effectCtx, op *effectOp, sign int) error {
	if c.target == nil || c.target.living == nil {
		return fmt.Errorf("adjust_rung: no living target")
	}
	if op.affect == "" {
		return fmt.Errorf("adjust_rung: no affect ref")
	}
	by := int(op.amount)
	if by <= 0 {
		by = 1 // default step of 1 rung
	}
	def := c.target.zone.affectDefs().get(op.affect)
	if def == nil || len(def.rungs) == 0 {
		return nil // not a ladder affect: nothing to adjust
	}
	// GATE BOTH DIRECTIONS (security review): any rung change on ANOTHER player's ladder affect is a
	// cross-player affect WRITE — increment worsens (obvious harm), and DECREMENT strips/weakens it, which
	// is harm too when the ladder is BENEFICIAL (a graded blessing/momentum) or when scope_target keying
	// lets an attacker reach an instance they never sourced. The engine can't know a content ladder's
	// polarity, so — exactly like dispel/remove_affect/modify_resource — every cross-player rung write
	// funnels the ONE shared guardCrossPlayerWrite (which self-short-circuits for a self/mob target, so
	// exhaustion recovery on yourself and a mob's own ladder stay ungated). A gated cross-player call is a
	// clean no-op; an ally rung change needs the same PvP consent every cross-player affect write does.
	if !guardCrossPlayerWrite(c, c.target) {
		return nil // gated block: clean no-op
	}
	a, ok := Get[*Affected](c.target)
	inst := (*affectInstance)(nil)
	if ok {
		inst = a.byKey[keyFor(def, c.source)]
	}
	if inst == nil {
		if sign < 0 {
			return nil // decrementing an absent ladder affect: nothing to recover
		}
		// Increment with no existing instance: install the affect (at rung 1), then raise to `by`.
		inst = applyAffect(c.target, op.affect, attachOpts{source: c.source}, c)
		if inst == nil {
			return nil // unknown ref / immunity veto / no living
		}
		inst.rung = clampRung(def, by)
		a, _ = Get[*Affected](c.target)
		a.recomputeMods()
		markAttrsDirty(c.target)
		return nil
	}
	newRung := inst.rung
	if newRung < 1 {
		newRung = 1
	}
	newRung += sign * by
	if newRung < 1 {
		// Fell off the bottom of the ladder: fully recovered — remove the affect.
		a.expire(c.target, inst, c)
		return nil
	}
	inst.rung = clampRung(def, newRung)
	a.recomputeMods()
	markAttrsDirty(c.target)
	return nil
}

// clampRung clamps a 1-based rung into [1, len(def.rungs)].
func clampRung(def *affectDef, r int) int {
	if r < 1 {
		return 1
	}
	if r > len(def.rungs) {
		return len(def.rungs)
	}
	return r
}

// opDispel: dispel(target, {category, count, check}). Removes up to `count` (amount) dispellable affects
// of a matching category (the op.text carries the category; empty = any). On a SELF/ally/mob target this
// is a cleanse (helpful) — ungated. But dispelling another PLAYER's affects strips their protective
// buffs = HARM, so on a non-self player target it routes through the ONE shared guardHarmful and aborts
// cleanly on a deny (same funnel as deal_damage). count<=0 means "all matching".
//
// ORDERING BY LEVEL (#545): candidates are removed HIGHEST-LEVEL first (affectDef.level DESC), so a
// count-limited dispel strips the strongest effects — 5e Dispel Magic's "removes the highest-level
// effect". Ties keep active-list order (stable).
//
// PER-AFFECT CHECK GATE (#545, optional): when the dispel op carries a `check` spec, it is resolved ONCE
// PER CANDIDATE with the affect's potency exposed as `$affect.level` (so the DC can read `10 +
// $affect.level`, 5e's contested dispel). A candidate whose gate check RESISTS (its matched band is a
// fail/miss/resist band) is left attached and does NOT count against the limit; the dispel moves on to
// the next. With no `check`, every matching candidate is removed unconditionally (the prior behaviour).
func opDispel(c *effectCtx, op *effectOp) error {
	if c.target == nil || c.target.living == nil {
		return fmt.Errorf("dispel: no living target")
	}
	// Dispelling another PLAYER's affects is harm (you can strip their buffs): gate it.
	if !guardCrossPlayerWrite(c, c.target) {
		return nil // clean no-op on a gated block
	}
	a, ok := Get[*Affected](c.target)
	if !ok {
		return nil
	}
	// Gather the dispellable, category-matching candidates, then order HIGHEST-LEVEL first (#545). Built
	// as a snapshot up front (expire mutates a.list); the per-candidate presence re-check below covers a
	// cascade that removed one mid-loop.
	var cands []*affectInstance
	for _, inst := range a.list {
		if !inst.def.dispellable {
			continue
		}
		if op.text != "" && inst.def.category != op.text {
			continue
		}
		cands = append(cands, inst)
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].def.level > cands[j].def.level })

	limit := int(op.amount)
	removed := 0
	for _, inst := range cands {
		if limit > 0 && removed >= limit {
			break
		}
		// Re-check presence: a prior candidate's on_dispel/on_expire cascade may have already removed this
		// one (or it expired). keyFor(def, source) is how expire keys removal.
		if a.byKey[keyFor(inst.def, inst.source)] != inst {
			continue
		}
		// PER-AFFECT CHECK GATE: resolve the dispel's check with this affect's level bound to $affect.level.
		// A resist leaves the affect and does not consume the limit. Restored after so a stray $affect.level
		// elsewhere reads 0.
		if op.check != nil {
			prev := c.affectLevel
			c.affectLevel = inst.def.level
			res := resolveCheck(c, op.check)
			c.affectLevel = prev
			if dispelResisted(res) {
				continue
			}
			// Re-check presence AFTER the gate: resolveCheck fires OnCheck, whose handler could have
			// removed/re-applied this very instance (a pathological but reachable re-entrancy). Expiring a
			// stale inst would delete a same-key affect that OnCheck freshly re-applied. Skip if it is gone.
			if a.byKey[keyFor(inst.def, inst.source)] != inst {
				continue
			}
		}
		// Lua on_dispel hook (7.4d): fires BEFORE removal (the affect is still attached) so the
		// hook can read its own magnitude/state. on_expire then also fires from expire() — a
		// dispel is an expire too. nil-safe / no-op when no Lua hook.
		if inst.def.onDispelLua != "" {
			c.z.runAffectHookLua(c.target, inst, "on_dispel", inst.def.onDispelLua)
		}
		a.expire(c.target, inst, c) // thread the cascade ctx (bounds a nested OnAffectExpire)
		removed++
	}
	return nil
}

// dispelResisted reports whether a dispel's per-affect check gate (#545) FAILED to remove the affect —
// i.e. the affect withstood the dispel. Convention (shared with classifyToHit's fail labels): a matched
// band labelled "resist", "fail", or "miss" is a resist; any OTHER matched band is a success (the affect
// is removed). An UNMATCHED gate roll (res.band == nil — the author's bands didn't cover the roll) is
// treated as a RESIST, the FAIL-SAFE default (review): an indeterminate gate must NOT strip a protective
// buff (a cross-player dispel is harm), matching the "indeterminate predicate fails toward inaction"
// discipline. A bare dispel with NO check never reaches here (it removes unconditionally) — this fail-safe
// applies only when a gate check was authored but classified nothing.
func dispelResisted(res checkResult) bool {
	if res.band == nil {
		return true // unmatched gate roll: do not remove (fail-safe)
	}
	switch res.band.label {
	case "resist", "fail", "miss":
		return true
	default:
		return false
	}
}

// opAct: act(template, to). Emits a perspective message (step-9 style) via the zone's act(). to is
// "actor"|"room"|"victim". The actor is the effect actor; the victim is the target. A comms op —
// never gated (saying something is not harm).
func opAct(c *effectCtx, op *effectOp) error {
	to := ToRoom
	switch op.to {
	case "actor":
		to = ToActor
	case "victim":
		to = ToVictim
	}
	c.z.act(op.text, c.actor, nil, c.target, "", "", to)
	return nil
}

// opSend: send(target, markup). Sends raw markup to the target's own stream (markup is data, never a
// format string — act.go discipline). A comms op — never gated.
func opSend(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return nil
	}
	if s, ok := sessionOf(c.target); ok {
		s.send(textFrame(op.text))
	}
	return nil
}

// opIf: if(cond, then, else). Its condition has these predicate shapes, ORed together:
//   - has_affect (op.affect): branch on whether the target has the named affect ("if poisoned, then ...");
//   - a NUMERIC COMPARISON (#542): `<lhs> <cmp> <rhs>` where lhs is a pool current (ifResource) OR a
//     formula (ifValue — an attribute / a ctx scalar like $depletion.overflow), cmp is >=/<=/>/</==/!=
//     (ifCmp, default >=), and rhs is a formula (ifThreshold) or the legacy literal (ifResourceMin). This
//     is what lets a derived threshold be authored declaratively — "hp <= half of max_hp" (bloodied via an
//     OnDamageTaken handler), "$depletion.overflow >= max_hp" (instant death via an on_depleted hook) —
//     rather than forcing Lua. The legacy `if reactions >= 1` reaction-budget guard is the default-cmp
//     case, byte-for-byte unchanged. The pool/formula is read off the ctx SUBJECT: the actor by default,
//     or the counterpart when the if itself carries `target: other` (runOps rebinds c.target first).
//
// An empty affect ref AND an empty resource/ifValue leave cond false. Branches recurse into runOps.
func opIf(c *effectCtx, op *effectOp) error {
	cond := false
	// NUMERIC PREDICATE (#542): a comparison of a numeric LHS against a numeric RHS. The LHS is a POOL
	// CURRENT (ifResource) or, when the compared quantity is not a pool, a FORMULA (ifValue — an attribute
	// or a ctx scalar like $depletion.overflow). The RHS is a FORMULA (ifThreshold — a derived threshold
	// like max_hp/2), or the legacy ifResourceMin literal when no formula is authored. The comparator
	// (ifCmp) defaults to ">=" so pre-#542 `if resource >= min` content is unchanged. Both formulas scope
	// to the ctx SUBJECT (the actor by default; `target: other` selected the counterpart before us).
	if op.ifResource != "" || op.ifValue != nil {
		// Read off the resolved ctx target (runOps already applied any `target: self|other`).
		subject := c.target
		if subject == nil {
			subject = c.actor
		}
		if subject != nil {
			// A degraded/broken formula operand makes the comparison INDETERMINATE. evalCheckFormula would
			// collapse it to 0 — which is fine for an additive bonus (0 = no contribution) but NOT for a
			// threshold, where 0 is an extreme of the range: a degraded `max_hp` on the headline instant-
			// death predicate `$depletion.overflow >= max_hp` collapses the RHS to 0, and overflow >= 0 is
			// always true, firing the HARMFUL branch on a debuffed victim. So evaluate with the error
			// SURFACED and, if either operand fails (errored formula, degraded attribute, non-finite), leave
			// cond FALSE — the predicate fails toward INACTION (skip `then`), the same "a broken channel is
			// no channel" discipline as the boon/bane selection and the check `when` axis. (#542 review.)
			lhsVal, lhsOK := float64(resourceCurrent(subject, op.ifResource)), true
			if op.ifValue != nil { // formula LHS takes precedence when set
				v, err := evalCheckFormulaErr(c, op.ifValue, subject)
				lhsVal, lhsOK = v, err == nil
			}
			rhsVal, rhsOK := op.ifResourceMin, true // legacy literal is always finite
			if op.ifThreshold != nil {
				v, err := evalCheckFormulaErr(c, op.ifThreshold, subject)
				rhsVal, rhsOK = v, err == nil
			}
			cond = lhsOK && rhsOK && compareIf(lhsVal, op.ifCmp, rhsVal)
		}
	}
	if op.affect != "" && c.target != nil {
		if def := c.target.zone.affectDefs().get(op.affect); def != nil {
			if a, ok := Get[*Affected](c.target); ok {
				_, has := a.byKey[keyFor(def, c.source)]
				if !has {
					_, has = a.byKey[keyFor(def, nil)]
				}
				cond = cond || has
			}
		}
	}
	if cond {
		runOps(c, op.then)
	} else {
		runOps(c, op.els)
	}
	return nil
}

// compareIf evaluates `lhs <cmp> rhs` for the #542 numeric predicate. cmp is one of >=,<=,>,<,==,!=;
// an empty or unrecognized cmp defaults to ">=" (the legacy reaction-budget comparison, so pre-#542
// content and a content typo both fall back to the historical behaviour rather than silently inverting).
// Equality uses a small epsilon because both operands can be DERIVED floats (max_hp/2 on an odd max),
// where exact float equality would be a footgun; the ordered comparisons are exact.
func compareIf(lhs float64, cmp string, rhs float64) bool {
	// Equality uses a RELATIVE epsilon so it holds at any magnitude: an absolute 1e-9 is below a float64
	// ULP once the operands reach ~1e9 (the codebase anticipates derived values that large), where `==`
	// would then miss values that are equal after rounding. Scaling by the larger operand keeps == and !=
	// exact complements (|d| <= eps vs |d| > eps) at hp-scale and at overflow/damage scale alike. The
	// ordered comparisons stay exact — they have no boundary-match hazard.
	eps := 1e-9 * math.Max(1, math.Max(math.Abs(lhs), math.Abs(rhs)))
	switch cmp {
	case "<=":
		return lhs <= rhs
	case ">":
		return lhs > rhs
	case "<":
		return lhs < rhs
	case "==":
		return math.Abs(lhs-rhs) <= eps
	case "!=":
		return math.Abs(lhs-rhs) > eps
	default: // ">=" and any unrecognized comparator (defense in depth; parse rejects unknown comparators)
		return lhs >= rhs
	}
}

// isValidIfCmp reports whether cmp is one of the recognized #542 comparison operators. Parse rejects any
// other non-empty value (an empty cmp is the legacy ">=" default, handled at runtime by compareIf).
func isValidIfCmp(cmp string) bool {
	switch cmp {
	case ">=", "<=", ">", "<", "==", "!=":
		return true
	default:
		return false
	}
}

// opChance: chance(p, then). Runs the `then` op-list with probability p (deterministic via the ctx
// rng in tests). A flow op — the branch's harmful ops still funnel through the guard.
func opChance(c *effectCtx, op *effectOp) error {
	if c.rollChance(op.prob) {
		runOps(c, op.then)
	}
	return nil
}

// opCheck: check(spec). The check/save/contested flow op ([G2], check.go) — resolves a content dice
// roll against a DC (or a contested defender), classifies into the first matching ORDERED band, and
// runs that band's nested op-list via runOps (the same recursion if/chance use). A check that BRANCHES
// into a harmful op does NOT bypass the PvP gate: the harm decision still lives at the op (dealDamage/
// applyDebuff -> guardHarmful), not at the check. A spec with no bands is a no-op (the roll still emits
// per visibility). Single-writer: zone goroutine; deterministic under the ctx rng.
func opCheck(c *effectCtx, op *effectOp) error {
	if op.check == nil {
		return fmt.Errorf("check: no spec")
	}
	res := resolveCheck(c, op.check)
	if res.band != nil {
		// CRIT DICE-DOUBLING for a CHECK-BAND crit (#544): when the matched band is a crit (its label is
		// "crit"/"critical"), scale the DICE term of any deal_damage/heal in the band's ops by the ROLLER's
		// `crit_dice` attribute — so a SPELL attack roll's crit doubles dice exactly like a melee swing crit
		// (which applySwingDamage handles outside opCheck). The roller is res.roller (the ctx actor by
		// default, or the ctx TARGET under subject: target — the saving-throw idiom), so the attribute is
		// read from the entity that actually rolled, matching the check's own scoping. This is the ONLY crit
		// knob wired here: the whole-roll `crit_mult` stays a swing-only mechanism, so a pack's melee
		// crit_mult can't silently double spell damage. Inert unless content sets crit_dice > 1 (default 1).
		//
		// SEMANTICS (documented decisions, not accidents):
		//   - SET, not multiply (c.critDiceMult = cd): a check crit band inside an already-critting swing/
		//     band overwrites rather than compounds, so two nested crit bands do NOT stack to 4x. crit_dice
		//     therefore composes differently from crit_mult (which multiplies through c.mag); a pack wanting
		//     multiplicative crit uses crit_mult.
		//   - The context covers the WHOLE band op-list, including a NESTED non-crit check's ops: everything
		//     that happens on the crit is crit damage. Saved/restored around the band ops so the outer crit
		//     context (a swing crit, an enclosing band) is exactly restored afterwards.
		//   - "crit"/"critical" is a RESERVED mechanical band label here (as in classifyToHit): a band so
		//     labelled with a deal_damage in it doubles when the roller has crit_dice > 1, even if the author
		//     meant the label as flavor. Documented in docs/ABILITIES.md; inert for packs that never set
		//     crit_dice.
		prevCritDice := c.critDiceMult
		if isCritBandLabel(res.band.label) {
			if cd := int(attr(res.roller, "crit_dice")); cd > 1 {
				c.critDiceMult = cd
			}
		}
		runOps(c, res.band.ops)
		c.critDiceMult = prevCritDice
	}
	return nil
}

// isCritBandLabel reports whether a check-band label denotes a critical (the engine-fixed convention,
// shared with classifyToHit in combat.go): "crit" or "critical". Used to wire crit dice-doubling to a
// spell/ability check-band crit (#544) without the engine naming any content.
func isCritBandLabel(label string) bool {
	return label == "crit" || label == "critical"
}

// rollDice rolls diceNum d diceSize (each die 1..size), using the ctx rng when present for
// determinism. Returns the sum. Used by deal_damage's <N>d<S> form.
func rollDice(c *effectCtx, num, size int) int {
	if num <= 0 || size <= 0 {
		return 0
	}
	// Defensive cap (mirrors parseDice's maxDice) for ops built directly, not via the parser, so a
	// runaway count never spins the zone goroutine's heartbeat.
	if num > maxDice {
		num = maxDice
	}
	if size > maxDice {
		size = maxDice
	}
	sum := 0
	for i := 0; i < num; i++ {
		if c.rng != nil {
			sum += c.rng.Intn(size) + 1
		} else {
			sum += randIntn(size) + 1
		}
	}
	return sum
}
