package world

import "testing"

// binding_test.go — Phase 13.1 item binding: the bind rules (BoP/BoE), the transfer gate (a bound item
// can't be dropped/stowed but can still be equipped), and the bound state surviving a reload.

func TestBindOnPickupAndEquipHelpers(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	// bind_on_pickup: binds when looted.
	bop := z.spawn(ProtoRef("midgaard:obj:torch"))
	addAny(bop, &ItemMeta{bindRule: bindRuleOnPickup})
	if isBound(bop) {
		t.Fatal("item bound before pickup")
	}
	bindOnPickup(bop)
	if !isBound(bop) {
		t.Fatal("bind_on_pickup did not bind on pickup")
	}

	// bind_on_equip: NOT bound on pickup, binds on equip.
	boe := z.spawn(ProtoRef("midgaard:obj:torch"))
	addAny(boe, &ItemMeta{bindRule: bindRuleOnEquip})
	bindOnPickup(boe)
	if isBound(boe) {
		t.Fatal("a bind_on_equip item bound on pickup")
	}
	bindOnEquip(boe)
	if !isBound(boe) {
		t.Fatal("bind_on_equip did not bind on equip")
	}

	// unbound: never binds.
	free := z.spawn(ProtoRef("midgaard:obj:torch"))
	bindOnPickup(free)
	bindOnEquip(free)
	if isBound(free) {
		t.Fatal("an untagged item bound")
	}
}

// TestBoundItemTransferGate proves a bound item is refused at the transfer commands (drop / put) while an
// unbound one passes — and a bound item can still be WORN (equip is not a transfer).
func TestBoundItemTransferGate(t *testing.T) {
	e := newCmdEnv(t)

	bound := addTestItem(e.z, e.actor.entity, "soulblade", []string{"soulblade"}, &Bound{})
	aout, _ := e.run("drop soulblade")
	if bound.location != e.actor.entity {
		t.Fatal("a bound item was dropped (left inventory)")
	}
	if !has(aout, "bound") {
		t.Fatalf("no bound-refusal message on drop: %v", aout)
	}

	// An unbound item drops normally.
	free := addTestItem(e.z, e.actor.entity, "stick", []string{"stick"})
	e.run("drop stick")
	if free.location == e.actor.entity {
		t.Fatal("an unbound item failed to drop")
	}

	// A bound WEARABLE can still be equipped (equip is not a transfer).
	worn := addTestItem(e.z, e.actor.entity, "cursed helm", []string{"helm"}, &Bound{}, wearableFromNames([]string{"head"}))
	e.run("wear helm")
	if wr, ok := Get[*Wearer](e.actor.entity); !ok || wr.slotOf(worn) == WearLocNone {
		t.Fatal("a bound item could not be equipped (binding must gate TRANSFER only, not equip)")
	}
}

// TestBindStateSurvivesReload proves a bound item's bound state round-trips through the item-instance delta.
func TestBindStateSurvivesReload(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Frodo"}
	e := z.newPlayerEntity(src, "Frodo")
	item := z.spawn(ProtoRef("midgaard:obj:torch"))
	bindItem(item)
	Move(item, e)

	snap := dumpCharacter(src)
	// The bound state rides the item's instance delta.
	foundBoundDelta := false
	for _, it := range snap.State.Inventory {
		if len(it.Delta) > 0 {
			foundBoundDelta = true
		}
	}
	if !foundBoundDelta {
		t.Fatal("a bound item dumped no instance delta")
	}

	dst := &session{character: "Frodo"}
	z.newPlayerEntity(dst, "Frodo")
	loadCharacter(z, dst, snap)
	var reloaded *Entity
	for _, it := range dst.entity.contents {
		if string(it.proto) == "midgaard:obj:torch" {
			reloaded = it
		}
	}
	if reloaded == nil {
		t.Fatal("the item did not reload")
	}
	if !isBound(reloaded) {
		t.Fatal("the bound state was lost on reload")
	}
}
