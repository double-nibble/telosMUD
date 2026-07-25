package world

// concentration_test.go exercises source-bound single-slot concentration (#539): a caster concentrates on
// at most ONE concentration-flagged affect, wherever it lives; a new one expires the prior; incapacitation
// or death (of the caster OR the spell's target) breaks it.

import "testing"

// concZone registers concentration affects A/B (each a distinct ref so they'd normally coexist) plus a
// stun, and returns the zone + a mob caster + two mob targets, all in one room.
func concZone(t *testing.T) (*Zone, *Entity, *Entity, *Entity) {
	z, player := affectTestZone(t)
	reg := func(ref string, conc bool, prevents ...string) {
		z.defs.affect.register(ref, &affectDef{
			ref: ref, name: ref, stacking: stackRefresh, maxStacks: 1, duration: 100,
			concentration: conc, prevents: prevents,
			modifiers: []affectModifier{{attr: "strength", add: true, value: -1}},
		})
	}
	reg("charm_a", true)
	reg("charm_b", true)
	reg("stun", false, "act")
	caster := makeMobTarget(z, player, "caster")
	t1 := makeMobTarget(z, player, "t1")
	t2 := makeMobTarget(z, player, "t2")
	return z, caster, t1, t2
}

// TestConcentrationSingleSlot proves a new concentration spell expires the caster's prior one, wherever it
// lives (charm_a on t1, then charm_b on t2 -> charm_a gone).
func TestConcentrationSingleSlot(t *testing.T) {
	z, caster, t1, t2 := concZone(t)
	applyAffect(t1, "charm_a", attachOpts{source: caster}, nil)
	if !hasAffect(t1, "charm_a") {
		t.Fatal("charm_a should be active on t1")
	}
	applyAffect(t2, "charm_b", attachOpts{source: caster}, nil) // a second concentration spell
	if hasAffect(t1, "charm_a") {
		t.Fatal("charm_a should have expired when the caster concentrated on charm_b")
	}
	if !hasAffect(t2, "charm_b") {
		t.Fatal("charm_b should be active on t2")
	}
	// The slot now points at charm_b.
	if slot, ok := z.concentration[caster]; !ok || slot.holder != t2 {
		t.Fatalf("concentration slot = %+v, want holder t2", slot)
	}
}

// TestConcentrationRecastSameDoesNotBreak proves refreshing the SAME concentration affect (same slot) does
// not expire it.
func TestConcentrationRecastSameDoesNotBreak(t *testing.T) {
	z, caster, t1, _ := concZone(t)
	applyAffect(t1, "charm_a", attachOpts{source: caster}, nil)
	applyAffect(t1, "charm_a", attachOpts{source: caster}, nil) // refresh, same (ref, source) -> same inst
	if !hasAffect(t1, "charm_a") {
		t.Fatal("re-casting the same concentration spell must not break it")
	}
	if slot := z.concentration[caster]; slot.holder != t1 {
		t.Fatalf("slot holder = %v, want t1", targetShort(slot.holder))
	}
}

// TestConcentrationBreaksOnIncapacitation proves a stun (prevents: [act]) landing on the caster breaks its
// concentration.
func TestConcentrationBreaksOnIncapacitation(t *testing.T) {
	z, caster, t1, _ := concZone(t)
	applyAffect(t1, "charm_a", attachOpts{source: caster}, nil)
	// A stun lands on the CASTER (from some other source) -> incapacitated -> concentration breaks.
	applyAffect(caster, "stun", attachOpts{source: t1}, nil)
	if hasAffect(t1, "charm_a") {
		t.Fatal("concentration should break when the caster is incapacitated (stunned)")
	}
	if _, ok := z.concentration[caster]; ok {
		t.Fatal("the concentration slot should be cleared on incapacitation")
	}
}

// TestConcentrationBreaksOnCasterDeath proves a caster's death breaks its concentration (which may live on
// a remote target).
func TestConcentrationBreaksOnCasterDeath(t *testing.T) {
	z, caster, t1, _ := concZone(t)
	applyAffect(t1, "charm_a", attachOpts{source: caster}, nil)
	z.die(caster, nil, nil)
	if hasAffect(t1, "charm_a") {
		t.Fatal("concentration should break when the caster dies")
	}
	if _, ok := z.concentration[caster]; ok {
		t.Fatal("the slot should be cleared on caster death")
	}
}

// TestConcentrationBreaksOnTargetDeath proves that when the TARGET of a concentration spell dies, the
// caster is freed (its slot cleared), matching 5e's "the spell ends if its only target dies".
func TestConcentrationBreaksOnTargetDeath(t *testing.T) {
	z, caster, t1, _ := concZone(t)
	applyAffect(t1, "charm_a", attachOpts{source: caster}, nil)
	z.die(t1, nil, nil) // the charmed target dies
	if _, ok := z.concentration[caster]; ok {
		t.Fatal("the caster should be freed to concentrate again when its spell's target dies")
	}
}

// TestConcentrationExpiryClearsSlot proves a natural expiry / removal of the concentration affect frees
// the slot so the caster can concentrate again.
func TestConcentrationExpiryClearsSlot(t *testing.T) {
	z, caster, t1, _ := concZone(t)
	inst := applyAffect(t1, "charm_a", attachOpts{source: caster}, nil)
	a, _ := Get[*Affected](t1)
	a.expire(t1, inst, nil) // simulate a dispel / countdown / save-break
	if _, ok := z.concentration[caster]; ok {
		t.Fatal("expiring the concentration affect must clear the caster's slot")
	}
}

// TestConcentrationCrossZoneGuard proves the CRITICAL fix (#539 review): an origin-side break must NOT
// touch a holder that transferred to another zone — expireConcentration's holder.zone==z guard skips it,
// so the origin goroutine never mutates an entity another zone now owns (the single-writer violation).
func TestConcentrationCrossZoneGuard(t *testing.T) {
	z, caster, t1, _ := concZone(t)
	applyAffect(t1, "charm_a", attachOpts{source: caster}, nil)
	// Simulate t1 transferring to a sibling zone: it now belongs to another zone's goroutine.
	other := newZone("other")
	t1.zone = other
	// An origin-side break (caster re-casts / is incapacitated / dies). The guard must skip the foreign
	// holder — the affect on t1 stays, and the origin does not write t1.
	z.breakConcentration(caster)
	if !hasAffect(t1, "charm_a") {
		t.Fatal("origin must NOT expire a transferred holder's affect (cross-zone single-writer guard)")
	}
	if _, ok := z.concentration[caster]; ok {
		t.Fatal("the origin slot should still be cleared even though the guard skipped the foreign expire")
	}
}

// TestConcentrationSelfCast proves a self-cast concentration (holder == source) is single-slotted and
// breaks on the caster's death (breakConcentrationInvolving handles source==holder once).
func TestConcentrationSelfCast(t *testing.T) {
	z, caster, _, _ := concZone(t)
	applyAffect(caster, "charm_a", attachOpts{source: caster}, nil) // self-buff
	if slot := z.concentration[caster]; slot.holder != caster {
		t.Fatalf("self-cast slot holder = %v, want caster", targetShort(slot.holder))
	}
	applyAffect(caster, "charm_b", attachOpts{source: caster}, nil) // a second self-concentration
	if hasAffect(caster, "charm_a") {
		t.Fatal("a second self-cast concentration must expire the first")
	}
	z.die(caster, nil, nil)
	if _, ok := z.concentration[caster]; ok {
		t.Fatal("a self-cast concentration slot must clear on the caster's death")
	}
}

// TestConcentrationStaleSlotNoOp proves the byKey re-validation guard: a slot pointing at an
// already-detached instance is a clean no-op (no double-expire, no panic).
func TestConcentrationStaleSlotNoOp(t *testing.T) {
	z, caster, t1, _ := concZone(t)
	inst := applyAffect(t1, "charm_a", attachOpts{source: caster}, nil)
	a, _ := Get[*Affected](t1)
	a.expire(t1, inst, nil) // detach the affect (clears the slot via clearConcentrationSlot)
	// Re-inject a STALE slot pointing at the now-detached inst.
	z.concentration[caster] = concentrationSlot{holder: t1, inst: inst}
	z.breakConcentration(caster) // expireConcentration: byKey[key] != inst -> no-op
	if hasAffect(t1, "charm_a") {
		t.Fatal("a stale slot must not resurrect/re-expire a detached affect")
	}
}

// TestNonConcentrationAffectsCoexist proves the single-slot rule applies ONLY to concentration affects:
// two ordinary affects from one source both persist.
func TestNonConcentrationAffectsCoexist(t *testing.T) {
	z, caster, t1, t2 := concZone(t)
	z.defs.affect.register("buff_a", &affectDef{ref: "buff_a", stacking: stackRefresh, maxStacks: 1, duration: 100})
	z.defs.affect.register("buff_b", &affectDef{ref: "buff_b", stacking: stackRefresh, maxStacks: 1, duration: 100})
	applyAffect(t1, "buff_a", attachOpts{source: caster}, nil)
	applyAffect(t2, "buff_b", attachOpts{source: caster}, nil)
	if !hasAffect(t1, "buff_a") || !hasAffect(t2, "buff_b") {
		t.Fatal("non-concentration affects from one source must both persist (no single-slot)")
	}
	if _, ok := z.concentration[caster]; ok {
		t.Fatal("a non-concentration affect must not occupy the concentration slot")
	}
}
