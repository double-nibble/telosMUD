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
	"math/rand"
)

// randIntn is the package-default rng draw (math/rand) used when a ctx carries no injected rng. Tests
// inject a seeded rng for determinism; production uses this.
func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	return rand.Intn(n) //nolint:gosec // seeded for gameplay determinism, not security
}

// opDealDamage: deal_damage(target, {amount|<N>d<S>, type}). Routes through the SHARED mitigation
// pipeline (dealDamage -> guardHarmful + resist/soak). The amount is either a flat `amount` or rolled
// dice, scaled by the ctx magnitude (a DoT's stacks). A blocked harmful op (PvP) is a clean no-op.
func opDealDamage(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("deal_damage: no target")
	}
	raw := op.amount
	// Dice: a content-formula dice COUNT ([G-A], a level-scaled rider) overrides the literal diceNum;
	// rollDice defensively caps the count at maxDice so a runaway formula can't spin the zone goroutine.
	num := op.diceNum
	if op.diceCount != nil {
		num = int(evalCheckFormula(c, op.diceCount, c.actor))
	}
	if num > 0 && op.diceSize > 0 {
		raw += float64(rollDice(c, num, op.diceSize))
	}
	// [G-A] scoped attribute bonus: `+ $actor.damroll + str_bonus` etc., over the actor/target/source
	// attributes (default scope = the actor dealing the damage). This is what lets a sword add STR, a
	// crit scale, and a combo-finisher read combo_points — all as CONTENT, not Lua.
	if op.bonus != nil {
		raw += evalCheckFormula(c, op.bonus, c.actor)
	}
	if c.mag > 0 {
		raw *= c.mag
	}
	dealDamage(c, c.target, raw, op.dmgType)
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
	// `bonus` formula — mirroring opDealDamage so a restorative op can roll `2d8 + $actor.wis_bonus` exactly
	// like a strike rolls `1d8 + $actor.str_bonus` (docs/REMAINING.md §4; the dice evaluator already exists,
	// the op-builder already parses these fields for every op — heal just wasn't reading them). Dice/bonus
	// are scoped to the ACTOR (the healer), so `+WIS` reads the caster's wisdom. restore delegates here, so
	// it inherits the same dice form.
	amt := op.amount
	num := op.diceNum
	if op.diceCount != nil {
		num = int(evalCheckFormula(c, op.diceCount, c.actor))
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
	detrimental := affectIsDetrimental(def)
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

// opDispel: dispel(target, {category, count}). Removes up to `count` (amount) dispellable affects of
// a matching category (the op.text carries the category; empty = any). On a SELF/ally/mob target this
// is a cleanse (helpful) — ungated. But dispelling another PLAYER's affects strips their protective
// buffs = HARM, so on a non-self player target it routes through the ONE shared guardHarmful and aborts
// cleanly on a deny (same funnel as deal_damage). count<=0 means "all matching".
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
	limit := int(op.amount)
	removed := 0
	// Snapshot: expire mutates a.list.
	snapshot := make([]*affectInstance, len(a.list))
	copy(snapshot, a.list)
	for _, inst := range snapshot {
		if limit > 0 && removed >= limit {
			break
		}
		if !inst.def.dispellable {
			continue
		}
		if op.text != "" && inst.def.category != op.text {
			continue
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

// opIf: if(cond, then, else). A minimal v1 condition with two predicate shapes:
//   - has_affect (op.affect): branch on whether the target has the named affect ("if poisoned, then ...");
//   - resource_min (op.ifResource >= op.ifResourceMin): branch on the SUBJECT's resource current — the
//     [G9] reaction-budget guard "if reactions >= 1, then [spend + opportunity attack]". The pool is read
//     off the ctx SUBJECT: the actor by default, or the counterpart when the if itself carries `target:
//     other` (runOps rebinds c.target before the handler, so the read tracks the selected entity).
//
// An empty affect ref AND an empty resource leave cond false. The query vocabulary expands in later
// slices; the flow op itself is the registered seam. Branches recurse back into runOps (same ctx).
func opIf(c *effectCtx, op *effectOp) error {
	cond := false
	if op.ifResource != "" {
		// Read the pool off the resolved ctx target (runOps already applied any `target: self|other`).
		subject := c.target
		if subject == nil {
			subject = c.actor
		}
		cond = subject != nil && float64(resourceCurrent(subject, op.ifResource)) >= op.ifResourceMin
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
		runOps(c, res.band.ops)
	}
	return nil
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
