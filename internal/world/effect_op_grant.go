package world

import "fmt"

// effect_op_grant.go — the Phase-11.1 GRANT ops (docs/PHASE11-PLAN.md §11.1, gap [G6b]): the additive
// effect ops a level-up / bundle / chargen op-list runs to permanently change an entity. They are thin
// wrappers over existing PERSISTED seams (setAttrBase, setFlag), so a grant survives a save/reload by
// construction — the state subtree is restored on load, the grant is never re-run (the double-apply guard
// the track machinery needs in 11.2 is about re-firing a STEP, not re-applying these ops).
//
// The single-writer + cross-player-write discipline matches modify_resource (§7/D2): a grant op writing
// ANOTHER player's state is gated through the one guardHarmful funnel regardless of sign — the engine
// cannot know whether raising a content stat or setting a content flag helps or harms the other player, so
// the safe default gates every cross-player write. A self-grant (the common level-up case, target == actor)
// is ungated.

// opModifyAttributeBase: modify_attribute_base(target, attr, amount) — add a signed delta to the target's
// per-entity attribute BASE (the override that holds race/class/level/point-buy bases). The first touch
// seeds from the attribute def's default base, so a +1 on an un-overridden stat raises it above its
// default rather than from zero. This is the op the progression constraint explicitly names (setAttrBase).
func opModifyAttributeBase(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("modify_attribute_base: no target")
	}
	if op.attr == "" {
		return fmt.Errorf("modify_attribute_base: no attr")
	}
	if !guardCrossPlayerWrite(c, c.target) {
		return nil // gated cross-player write: clean no-op
	}
	// Capture the pre-write base so the audit row carries old/new/delta (#350). Read BEFORE setAttrBase.
	old, degraded := attrBaseValue(c.target, op.attr)
	if degraded {
		// The seed derives from a screened (poisoned) attribute. Snapshotting it into a PERMANENT base
		// override would launder the degradation into a clean, formula-trusted number — fail closed
		// instead, matching the formula-side refusal. A content defect must not become durable state.
		c.z.log.Warn("modify_attribute_base refused: the base seed derives from a degraded attribute",
			"attr", op.attr, "target", targetShort(c.target))
		return nil
	}
	newVal := old + op.amount
	setAttrBase(c.target, op.attr, newVal)
	// Durable audit (#350): a permanent attribute-base grant is a tracked event. Enqueued off the zone
	// goroutine; a no-op for a mob target, a not-yet-saved player, or a storeless shard (the helper guards).
	c.z.auditAttributeBase(c.target, op.attr, old, newVal)
	// A grant op can cross a channel's access predicate (here a min_attr floor), so re-publish the
	// target's comms config — the same mid-session hear-set refresh the affect apply/expire sites do.
	// Without it a player who drops below (or rises to) a channel's floor keeps a stale subscription
	// until their next toggle/handoff/relog (security follow-up, round 5). Cheap: no-op unless the
	// target is a player and some channel actually gates hearing (republishCommsOnAccessChange guards).
	c.markCommsDirty(c.target)
	return nil
}

// opSetFlag: set_flag(target, flag) — set a named open-set flag on the target (a permanent passive marker
// a bundle/level grants, e.g. "darkvision", "guildmember"). Persisted in the entity's flags subtree.
func opSetFlag(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("set_flag: no target")
	}
	if op.flag == "" {
		return fmt.Errorf("set_flag: no flag")
	}
	if reservedFlag(op.flag) {
		// #27/#28: the trust/elevation flags (holylight/builder/admin) are set ONLY by the tier-application
		// path (applyTierFlags), never by content — else a builder pack could grant itself see-all/admin.
		c.z.log.Warn("content set_flag refused a reserved trust flag", "flag", op.flag)
		return nil
	}
	if !guardCrossPlayerWrite(c, c.target) {
		return nil
	}
	setFlag(c.target, op.flag, true)
	c.markCommsDirty(c.target) // a require_flag channel may now be hearable (see modify_attribute_base)
	if isConcealmentFlag(op.flag) {
		c.z.republishPresenceOnConcealChange(c.target) // #98: a now-invisible/hidden player drops from cross-shard who
	}
	return nil
}

// opClearFlag: clear_flag(target, flag) — the revoke inverse of set_flag (lose a passive / leave a guild).
func opClearFlag(c *effectCtx, op *effectOp) error {
	if c.target == nil {
		return fmt.Errorf("clear_flag: no target")
	}
	if op.flag == "" {
		return fmt.Errorf("clear_flag: no flag")
	}
	if reservedFlag(op.flag) {
		// #27/#28: content may not clear a reserved trust flag either — only the tier path manages them (a
		// content clear_flag "holylight" must not be able to strip an admin's see-all).
		c.z.log.Warn("content clear_flag refused a reserved trust flag", "flag", op.flag)
		return nil
	}
	if !guardCrossPlayerWrite(c, c.target) {
		return nil
	}
	setFlag(c.target, op.flag, false)
	c.markCommsDirty(c.target) // a require_flag channel may no longer be hearable — the guild-leave case
	if isConcealmentFlag(op.flag) {
		c.z.republishPresenceOnConcealChange(c.target) // #98: a now-revealed player reappears in cross-shard who
	}
	return nil
}

// attrBaseValue returns entity e's current BASE for attribute `name` — the per-entity override if set,
// else the attribute def's evaluated default base (the same base step resolveAttr does), else 0. It is the
// "modify from the current base" seed for modify_attribute_base, so a delta on an un-overridden stat
// starts from the def default rather than zero. Read-only; zone goroutine.
// It also reports whether the computed value is DEGRADED — whether the base formula read an attribute
// the fold screen had to bound (attributes.go). A grant that would SNAPSHOT a degraded value into a
// permanent base override is a laundering channel: the snapshot is a fresh, non-degraded number, so
// the propagation that keeps live derivation honest cannot reach it, and a formula later trusts the
// baked-in poison. The callers refuse a degraded seed for that reason.
func attrBaseValue(e *Entity, name string) (float64, bool) {
	if e == nil || e.living == nil {
		return 0, false
	}
	if ov, ok := e.living.attrBase[name]; ok {
		return ov, false // an explicit override is authoritative content, not a derived value
	}
	def := e.zone.attrDefs().get(name)
	if def == nil || def.base == nil {
		return 0, false
	}
	degraded := false
	r := &formulaResolver{
		resolve: func(ref string, v map[string]bool) (float64, error) {
			rv, rd, err := resolveAttr(e, ref, v)
			if rd {
				degraded = true
			}
			return rv, err
		},
		visited: map[string]bool{},
	}
	v, err := evalFinite(def.base, r)
	if err != nil {
		return 0, false
	}
	return v, degraded
}
