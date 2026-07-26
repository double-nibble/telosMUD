package world

import (
	"reflect"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// equip_affects_test.go — #515 item-sourced OnHit procs / on-equip affects. Done-when: equipping an item
// applies its declared affects (keyed by the item), unequipping removes exactly them, an equipped weapon's
// affect participates in the event bus (an OnHit proc), and the derived affects are transient (not persisted;
// re-derived on load without re-firing on_apply).

// registerBlessed registers an indefinite "blessed" affect granting +3 strength.
func registerBlessed(z *Zone) {
	z.defs.affect.register("blessed", &affectDef{
		ref: "blessed", name: "Blessed", indefinite: true,
		modifiers: []affectModifier{{attr: "strength", add: true, value: 3}},
	})
}

// TestEquipAffectAppliedAndRemoved: wearing an item with an equip-affect applies it; removing strips it.
func TestEquipAffectAppliedAndRemoved(t *testing.T) {
	e := newCmdEnv(t)
	registerBlessed(e.z)
	actor := e.actor.entity
	base := attr(actor, "strength")

	addTestItem(e.z, actor, "a blessed amulet", []string{"amulet"},
		&Wearable{locs: []WearLoc{WearLocHead}, equipAffects: []string{"blessed"}})

	e.run("wear amulet")
	if got := attr(actor, "strength"); got != base+3 {
		t.Fatalf("strength wearing the blessed amulet = %v, want %v", got, base+3)
	}
	e.run("remove amulet")
	if got := attr(actor, "strength"); got != base {
		t.Fatalf("strength after removing the amulet = %v, want %v (equip affect stripped)", got, base)
	}
}

// TestEquipAffectOnHitProc: a wielded weapon whose equip-affect subscribes OnHit deals its bonus damage —
// the flame-tongue. It rides the EXISTING affect event-subscription path (gatherEventHandlers), so no new
// item-bus machinery is needed. Fired via fireEvent to isolate the subscription from the full combat loop.
func TestEquipAffectOnHitProc(t *testing.T) {
	e := newCmdEnv(t)
	registerFlametongue(e.z) // a FIXED (#515) +5 emberfire OnHit proc
	actor := e.actor.entity
	addTestItem(e.z, actor, "a flaming sword", []string{"sword"},
		&Wearable{locs: []WearLoc{WearLocWield}, equipAffects: []string{"flametongue"}},
		&Weapon{diceNum: 1, diceSize: 4, damageType: "slash"})

	mob := makeMobTarget(e.z, actor, "goblin")
	setResourceCurrent(mob, "hp", 100)

	// Fire OnHit with mag 8 (a realistic blow). The proc is `fixed`, so it deals exactly 5 regardless of
	// the blow — the flame-tongue's flat rider, not a proportional 8×5. Before wielding: no proc.
	e.z.fireEvent(nil, evOnHit, actor, mob, 8)
	if hp := resourceCurrent(mob, "hp"); hp != 100 {
		t.Fatalf("unwielded: mob hp = %d, want 100 (no proc)", hp)
	}

	e.run("wield sword")
	e.z.fireEvent(nil, evOnHit, actor, mob, 8)
	if hp := resourceCurrent(mob, "hp"); hp != 95 {
		t.Fatalf("flame-tongue OnHit proc: mob hp = %d, want 95 (fixed 5 emberfire, not 8×5, no self-loop)", hp)
	}

	// Removing the weapon removes the affect, so the proc stops.
	e.run("remove sword")
	e.z.fireEvent(nil, evOnHit, actor, mob, 8)
	if hp := resourceCurrent(mob, "hp"); hp != 95 {
		t.Fatalf("after removing the weapon: mob hp = %d, want 95 (proc gone)", hp)
	}
}

// registerFlametongue registers an indefinite affect whose OnHit handler deals a FIXED +5 emberfire to the
// counterpart — the flame-tongue. `fixed` (#515) keeps it from scaling with the blow's damage (the OnHit
// event mag). emberfire is a neutral (×1) damage type dedicated to these tests.
func registerFlametongue(z *Zone) {
	z.defs.dmg.register("emberfire", &damageTypeDef{ref: "emberfire"})
	z.defs.affect.register("flametongue", &affectDef{
		ref: "flametongue", name: "Flame Tongue", indefinite: true,
		onEvent: map[eventKind][]effectOp{
			evOnHit: {{kind: "deal_damage", dmgType: "emberfire", amount: 5, tgt: "other", fixed: true}},
		},
	})
}

// TestEquipProcFixedVsProportional (#515): a `fixed` OnHit deal_damage ignores the blow's damage (a
// flame-tongue's flat rider); a non-fixed one scales with it (a proportional proc). Pins the `fixed` flag.
func TestEquipProcFixedVsProportional(t *testing.T) {
	e := newCmdEnv(t)
	e.z.defs.dmg.register("emberfire", &damageTypeDef{ref: "emberfire"})
	e.z.defs.affect.register("fixedproc", &affectDef{
		ref: "fixedproc", name: "Fixed", indefinite: true,
		onEvent: map[eventKind][]effectOp{evOnHit: {{kind: "deal_damage", dmgType: "emberfire", amount: 5, tgt: "other", fixed: true}}},
	})
	e.z.defs.affect.register("propproc", &affectDef{
		ref: "propproc", name: "Proportional", indefinite: true,
		onEvent: map[eventKind][]effectOp{evOnHit: {{kind: "deal_damage", dmgType: "emberfire", amount: 5, tgt: "other"}}},
	})
	actor := e.actor.entity
	mob := makeMobTarget(e.z, actor, "goblin")

	// Fixed: 5 regardless of a mag-10 blow.
	applyAffect(actor, "fixedproc", attachOpts{source: actor, fromEquip: true}, nil)
	setResourceCurrent(mob, "hp", 100)
	e.z.fireEvent(nil, evOnHit, actor, mob, 10)
	if hp := resourceCurrent(mob, "hp"); hp != 95 {
		t.Fatalf("fixed proc under a mag-10 blow: mob hp = %d, want 95 (fixed 5)", hp)
	}
	// Proportional: 5×10 = 50.
	a, _ := Get[*Affected](actor)
	if inst := a.byKey[keyFor(e.z.defs.affect.get("fixedproc"), actor)]; inst != nil {
		a.expire(actor, inst, nil) // drop the fixed one so only the proportional fires
	}
	applyAffect(actor, "propproc", attachOpts{source: actor, fromEquip: true}, nil)
	setResourceCurrent(mob, "hp", 100)
	e.z.fireEvent(nil, evOnHit, actor, mob, 10)
	if hp := resourceCurrent(mob, "hp"); hp != 50 {
		t.Fatalf("proportional proc under a mag-10 blow: mob hp = %d, want 50 (5×10)", hp)
	}
}

// TestEquipAffectOnHitNoSelfLoop is the explicit regression for the OnHit self-loop (#515): an OnHit
// handler that deal_damages must fire EXACTLY ONCE per hit, not re-trigger itself up to the depth/budget
// cap. Without the withinOnHit guard the flame-tongue below drains the mob far past a single 5-damage proc.
func TestEquipAffectOnHitNoSelfLoop(t *testing.T) {
	e := newCmdEnv(t)
	registerFlametongue(e.z)
	actor := e.actor.entity
	addTestItem(e.z, actor, "a flaming sword", []string{"sword"},
		&Wearable{locs: []WearLoc{WearLocWield}, equipAffects: []string{"flametongue"}},
		&Weapon{diceNum: 1, diceSize: 4, damageType: "slash"})
	e.run("wield sword")

	mob := makeMobTarget(e.z, actor, "goblin")
	setResourceCurrent(mob, "hp", 100)
	e.z.fireEvent(nil, evOnHit, actor, mob, 1)
	// Exactly one 5-damage proc: 95. A re-entrant loop would land far below (the proc re-fires OnHit,
	// which re-runs the handler, up to the depth/budget cap).
	if hp := resourceCurrent(mob, "hp"); hp != 95 {
		t.Fatalf("OnHit proc self-loop: mob hp = %d, want exactly 95 (one 5-damage proc)", hp)
	}
}

// TestEquipAffectSourceKeyed: two items each granting the same affect are keyed per-ITEM, so both apply and
// removing one leaves the other's contribution intact.
func TestEquipAffectSourceKeyed(t *testing.T) {
	e := newCmdEnv(t)
	registerBlessed(e.z)
	actor := e.actor.entity
	base := attr(actor, "strength")

	addTestItem(e.z, actor, "a blessed amulet", []string{"amulet"},
		&Wearable{locs: []WearLoc{WearLocHead}, equipAffects: []string{"blessed"}})
	addTestItem(e.z, actor, "blessed plate", []string{"plate"},
		&Wearable{locs: []WearLoc{WearLocBody}, equipAffects: []string{"blessed"}})

	e.run("wear amulet")
	e.run("wear plate")
	if got := attr(actor, "strength"); got != base+6 {
		t.Fatalf("strength with two blessed items = %v, want %v (per-item instances stack)", got, base+6)
	}
	e.run("remove amulet")
	if got := attr(actor, "strength"); got != base+3 {
		t.Fatalf("strength after removing one = %v, want %v (the other's affect remains)", got, base+3)
	}
}

// TestEquipAffectDroppedOnDestroy: destroying a worn item (the salvage/consume path, Move to nil) drops its
// equip affect — the unequipFromWearer safety hook, not just the remove verb.
func TestEquipAffectDroppedOnDestroy(t *testing.T) {
	e := newCmdEnv(t)
	registerBlessed(e.z)
	actor := e.actor.entity
	base := attr(actor, "strength")

	item := addTestItem(e.z, actor, "a blessed amulet", []string{"amulet"},
		&Wearable{locs: []WearLoc{WearLocHead}, equipAffects: []string{"blessed"}})
	e.run("wear amulet")
	if attr(actor, "strength") != base+3 {
		t.Fatal("precondition: amulet not applying its affect")
	}
	Move(item, nil) // destroy the still-worn item
	if got := attr(actor, "strength"); got != base {
		t.Fatalf("strength after destroying the worn item = %v, want %v (no phantom affect)", got, base)
	}
}

// TestEquipAffectNotDumped: a gear-derived equip affect is NOT persisted (dumpAffects skips it), so a reload
// re-derives it from the worn item rather than double-applying a stale copy the source key can't match.
func TestEquipAffectNotDumped(t *testing.T) {
	e := newCmdEnv(t)
	registerBlessed(e.z)
	// A NON-equip affect applied normally IS dumped — the control.
	e.z.defs.affect.register("cursed", &affectDef{ref: "cursed", name: "Cursed", indefinite: true})
	actor := e.actor.entity

	addTestItem(e.z, actor, "a blessed amulet", []string{"amulet"},
		&Wearable{locs: []WearLoc{WearLocHead}, equipAffects: []string{"blessed"}})
	e.run("wear amulet")
	applyAffect(actor, "cursed", attachOpts{}, nil) // a normal, persisted affect

	dumped := dumpAffects(actor)
	var sawBlessed, sawCursed bool
	for _, a := range dumped {
		if a.ID == "blessed" {
			sawBlessed = true
		}
		if a.ID == "cursed" {
			sawCursed = true
		}
	}
	if sawBlessed {
		t.Fatal("an equip-derived affect must NOT be persisted (dumpAffects should skip it)")
	}
	if !sawCursed {
		t.Fatal("a normal affect must still be persisted (control)")
	}
}

// TestEquipAffectLoadReDerivesQuietly: the load re-derivation (quiet=true) installs the affect (so its
// modifiers are live) WITHOUT re-firing on_apply — a relog must not re-trigger an on-apply proc. A live wear
// (quiet=false) DOES fire on_apply.
func TestEquipAffectLoadReDerivesQuietly(t *testing.T) {
	e := newCmdEnv(t)
	// The affect boosts strength AND subscribes to its own OnApplyAffect to bump a counter resource, so we
	// can observe whether on_apply fired.
	e.z.defs.res.register("apply_count", &resourceDef{ref: "apply_count"})
	e.z.defs.affect.register("blessed", &affectDef{
		ref: "blessed", name: "Blessed", indefinite: true,
		modifiers: []affectModifier{{attr: "strength", add: true, value: 3}},
		onEvent: map[eventKind][]effectOp{
			evOnApplyAffect: {{kind: "modify_resource", resource: "apply_count", amount: 1, tgt: "self"}},
		},
	})
	actor := e.actor.entity
	base := attr(actor, "strength")
	item := addTestItem(e.z, actor, "a blessed amulet", []string{"amulet"},
		&Wearable{locs: []WearLoc{WearLocHead}, equipAffects: []string{"blessed"}})

	// LIVE wear: on_apply fires (counter 1) and the modifier is live.
	e.run("wear amulet")
	if resourceCurrent(actor, "apply_count") != 1 {
		t.Fatalf("live wear: apply_count = %d, want 1 (on_apply fired)", resourceCurrent(actor, "apply_count"))
	}
	if attr(actor, "strength") != base+3 {
		t.Fatal("live wear: modifier not live")
	}

	// Simulate the load re-derivation on a FRESH wearer: quiet, so on_apply is suppressed but the modifier is
	// still live.
	other := newTestPlayerEntity(e.z, "Carol")
	Move(other.entity, e.room)
	base2 := attr(other.entity, "strength")
	applyEquipAffects(other.entity, item, true /*quiet*/)
	if attr(other.entity, "strength") != base2+3 {
		t.Fatalf("quiet re-derive: strength = %v, want %v (modifier still live)", attr(other.entity, "strength"), base2+3)
	}
	if resourceCurrent(other.entity, "apply_count") != 0 {
		t.Fatalf("quiet re-derive: apply_count on the fresh wearer = %d, want 0 (on_apply suppressed)", resourceCurrent(other.entity, "apply_count"))
	}
}

// TestEquipAffectHoldVerb (#515): a HELD item's equip-affect applies (the third equip verb, distinct from
// wear/wield).
func TestEquipAffectHoldVerb(t *testing.T) {
	e := newCmdEnv(t)
	registerBlessed(e.z)
	actor := e.actor.entity
	base := attr(actor, "strength")
	addTestItem(e.z, actor, "a blessed orb", []string{"orb"},
		&Wearable{locs: []WearLoc{WearLocHold}, equipAffects: []string{"blessed"}})

	e.run("hold orb")
	if got := attr(actor, "strength"); got != base+3 {
		t.Fatalf("strength holding the blessed orb = %v, want %v", got, base+3)
	}
	e.run("remove orb")
	if got := attr(actor, "strength"); got != base {
		t.Fatalf("strength after removing the orb = %v, want %v", got, base)
	}
}

// TestEquipAffectSurvivesRespawn (#515, security F1/F2): an equip-affect is tied to the still-worn item,
// not the death — respawn must NOT strip it. A beneficial OnHit proc keeps proccing; a DETRIMENTAL curse
// keeps debuffing (not sheddable by dying).
func TestEquipAffectSurvivesRespawn(t *testing.T) {
	e := newCmdEnv(t)
	registerFlametongue(e.z)
	// A harmful equip-affect (a cursed item: -3 strength) — the kind stripHostileAffects targets.
	e.z.defs.affect.register("cursed_weakness", &affectDef{
		ref: "cursed_weakness", name: "Cursed", indefinite: true,
		modifiers: []affectModifier{{attr: "strength", add: true, value: -3}},
	})
	actor := e.actor.entity
	base := attr(actor, "strength")

	addTestItem(e.z, actor, "a cursed flaming sword", []string{"sword"},
		&Wearable{locs: []WearLoc{WearLocWield}, equipAffects: []string{"flametongue", "cursed_weakness"}},
		&Weapon{diceNum: 1, diceSize: 4, damageType: "slash"})
	e.run("wield sword")
	if attr(actor, "strength") != base-3 {
		t.Fatal("precondition: curse not applied on wield")
	}

	e.z.respawnPlayer(actor)

	// The curse survives (still worn), and the proc still fires.
	if got := attr(actor, "strength"); got != base-3 {
		t.Fatalf("strength after respawn = %v, want %v (curse survives — not sheddable by death)", got, base-3)
	}
	mob := makeMobTarget(e.z, actor, "goblin")
	setResourceCurrent(mob, "hp", 100)
	e.z.fireEvent(nil, evOnHit, actor, mob, 5)
	if hp := resourceCurrent(mob, "hp"); hp != 95 {
		t.Fatalf("flame-tongue after respawn: mob hp = %d, want 95 (proc survives respawn)", hp)
	}
}

// TestEquipAffectReloadRoundTrip (#515, test-engineer HIGH): a full dumpCharacter→loadCharacter cycle
// re-derives the equip-affect on the fresh entity (the item respawns from its proto, applyStateComponents
// re-applies) WITHOUT persisting it as a normal affect and WITHOUT re-firing on_apply. Uses a REAL proto
// in the cache so loadItem can respawn the worn item — pinning the load-path re-derivation call, which is
// otherwise a mutation survivor, and covering the shard-transfer seam (same applyStateComponents).
func TestEquipAffectReloadRoundTrip(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	registerBlessed(z) // +3 strength, indefinite
	// Register a real proto so loadItem can respawn the worn item on load.
	z.protos.define(ProtoRef("test:obj:blesshelm"), []string{"blesshelm"}, "a blessed helm", "A blessed helm.",
		componentSet{
			reflect.TypeFor[*Wearable](): &Wearable{locs: []WearLoc{WearLocHead}, equipAffects: []string{"blessed"}},
		})

	src := &session{character: "Wynne"}
	e := z.newPlayerEntity(src, "Wynne")
	Move(e, z.rooms["midgaard:room:temple"])
	base := attr(e, "strength")
	helm := z.spawn(ProtoRef("test:obj:blesshelm"))
	Move(helm, e)
	z.dispatch(src, "wear blesshelm")
	if attr(e, "strength") != base+3 {
		t.Fatalf("precondition: live wear did not apply the equip-affect (str=%v)", attr(e, "strength"))
	}

	snap := dumpCharacter(src)
	// The equip-affect is NOT persisted as an affect (it re-derives from the worn helm).
	for _, a := range snap.State.Affects {
		if a.ID == "blessed" {
			t.Fatal("equip-affect must not be persisted in the snapshot")
		}
	}

	// Load into a fresh entity: the helm respawns worn and the equip-affect re-derives. (The load call
	// passes quiet=true; the on_apply-suppression itself is pinned directly by TestEquipAffectLoadReDerives-
	// Quietly — here a resource observable would be masked anyway, since applyStateComponents restores
	// resources AFTER equipment, which is itself why relog on_apply-farming of a resource is impossible.)
	dst := &session{character: "Wynne"}
	z.newPlayerEntity(dst, "Wynne")
	loadCharacter(z, dst, snap)
	de := dst.entity
	if got := attr(de, "strength"); got != base+3 {
		t.Fatalf("reloaded strength = %v, want %v (equip-affect re-derived on load)", got, base+3)
	}
	// And unequipping on the reloaded entity still removes it (keyed by the NEW item pointer).
	z.dispatch(dst, "remove blesshelm")
	if got := attr(de, "strength"); got != base {
		t.Fatalf("reloaded strength after remove = %v, want %v (re-derived affect removable)", got, base)
	}
}

// TestEquipProcCrossAttackerReflectStillFires (#515, F5): the OnHit self-loop guard is precise — it
// suppresses only the ATTACKER's own OnHit re-fire, so a DIFFERENT entity's damage inside the cascade (a
// victim's thorns reflect) still procs that reflector's own OnHit. Attacker A's flame-tongue procs on B;
// B's thorns (OnDamageTaken -> deal_damage back at A) then procs B's OWN emberproc onto A.
func TestEquipProcCrossAttackerReflectStillFires(t *testing.T) {
	e := newCmdEnv(t)
	registerFlametongue(e.z) // A's weapon: OnHit -> 5 emberfire to other (fixed)
	// B's thorns: OnDamageTaken -> reflect 3 emberfire back at the attacker (other).
	e.z.defs.affect.register("thorns", &affectDef{
		ref: "thorns", name: "Thorns", indefinite: true,
		onEvent: map[eventKind][]effectOp{evOnDamageTaken: {{kind: "deal_damage", dmgType: "emberfire", amount: 3, tgt: "other", fixed: true}}},
	})
	// B's OWN OnHit proc: when B "hits" (its reflected damage counts), it burns the target for 2.
	e.z.defs.affect.register("emberproc", &affectDef{
		ref: "emberproc", name: "Ember", indefinite: true,
		onEvent: map[eventKind][]effectOp{evOnHit: {{kind: "deal_damage", dmgType: "emberfire", amount: 2, tgt: "other", fixed: true}}},
	})

	a := e.actor.entity                       // attacker A (player, carries the flame-tongue)
	b := makeMobTarget(e.z, a, "spikedbeast") // victim B (a mob — player↔mob harm is ungated, unlike PvP)
	setResourceCurrent(a, "hp", 100)
	setResourceCurrent(b, "hp", 100)
	applyAffect(a, "flametongue", attachOpts{source: a, fromEquip: true}, nil)
	applyAffect(b, "thorns", attachOpts{source: b, fromEquip: true}, nil)
	applyAffect(b, "emberproc", attachOpts{source: b, fromEquip: true}, nil)

	// A hits B (mag 1). A's flame-tongue -> 5 to B. That deal fires B's OnDamageTaken -> thorns 3 back at A.
	// The thorns deal (src=B) is NOT A's self-loop, so it fires B's OnHit -> emberproc 2 more onto A.
	e.z.fireEvent(nil, evOnHit, a, b, 1)

	if hp := resourceCurrent(b, "hp"); hp != 95 {
		t.Fatalf("B hp = %d, want 95 (A's fixed 5 flame-tongue)", hp)
	}
	// A takes thorns 3 + B's emberproc 2 = 5. If the guard were cascade-wide, B's OnHit would be suppressed
	// and A would take only 3.
	if hp := resourceCurrent(a, "hp"); hp != 95 {
		t.Fatalf("A hp = %d, want 95 (thorns 3 + B's cross-attacker OnHit proc 2)", hp)
	}
}

// TestWearableFromDTOEquipAffects: the content DTO's EquipAffects parse into the Wearable component.
func TestWearableFromDTOEquipAffects(t *testing.T) {
	d := &content.WearableDTO{Locations: []string{"wield"}, EquipAffects: []string{"flametongue", "sharpness"}}
	w := wearableFromDTO(d)
	if len(w.equipAffects) != 2 || w.equipAffects[0] != "flametongue" || w.equipAffects[1] != "sharpness" {
		t.Fatalf("equipAffects = %v, want [flametongue sharpness]", w.equipAffects)
	}
}
