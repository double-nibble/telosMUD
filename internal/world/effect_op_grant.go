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
	setAttrBase(c.target, op.attr, attrBaseValue(c.target, op.attr)+op.amount)
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
	if !guardCrossPlayerWrite(c, c.target) {
		return nil
	}
	setFlag(c.target, op.flag, true)
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
	if !guardCrossPlayerWrite(c, c.target) {
		return nil
	}
	setFlag(c.target, op.flag, false)
	return nil
}

// attrBaseValue returns entity e's current BASE for attribute `name` — the per-entity override if set,
// else the attribute def's evaluated default base (the same base step resolveAttr does), else 0. It is the
// "modify from the current base" seed for modify_attribute_base, so a delta on an un-overridden stat
// starts from the def default rather than zero. Read-only; zone goroutine.
func attrBaseValue(e *Entity, name string) float64 {
	if e == nil || e.living == nil {
		return 0
	}
	if ov, ok := e.living.attrBase[name]; ok {
		return ov
	}
	def := e.zone.attrDefs().get(name)
	if def == nil || def.base == nil {
		return 0
	}
	r := &formulaResolver{
		resolve: func(ref string, v map[string]bool) (float64, error) { return resolveAttr(e, ref, v) },
		visited: map[string]bool{},
	}
	v, err := evalFinite(def.base, r)
	if err != nil {
		return 0
	}
	return v
}
