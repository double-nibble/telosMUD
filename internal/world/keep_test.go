package world

import "testing"

// keep_test.go covers #36 parts 2+3: `drop` refuses an equipped item (require `remove` first), and the
// `keep`/`unkeep` no-drop flag (incl. its durable round-trip).

// TestDropRefusesEquipped proves a worn item is no longer silently un-equipped on drop — it is refused
// with a "remove first" message — and drops fine once removed.
func TestDropRefusesEquipped(t *testing.T) {
	e := newCmdEnv(t)
	helm := addTestItem(e.z, e.actor.entity, "a helm", []string{"helm"}, wearableFor(WearLocHead))
	e.run("wear helm")
	wr, _ := Get[*Wearer](e.actor.entity)
	if wr == nil || wr.slotOf(helm) == WearLocNone {
		t.Fatal("precondition: the helm should be worn")
	}

	out, _ := e.run("drop helm")
	if helm.location != e.actor.entity {
		t.Fatal("a WORN item was dropped without an explicit remove")
	}
	if !has(out, "remove") {
		t.Fatalf("no remove-first refusal message on dropping a worn item: %v", out)
	}

	// Remove, then drop succeeds.
	e.run("remove helm")
	e.run("drop helm")
	if helm.location == e.actor.entity {
		t.Fatal("drop after remove failed")
	}
}

// TestKeepBlocksDropAndPut proves a kept item is refused by drop and put, and drops once unkept.
func TestKeepBlocksDropAndPut(t *testing.T) {
	e := newCmdEnv(t)
	stick := addTestItem(e.z, e.actor.entity, "a stick", []string{"stick"})
	box := addTestItem(e.z, e.actor.entity, "a box", []string{"box"}, &Container{capacity: 10})

	if out, _ := e.run("keep stick"); !has(out, "keep") {
		t.Fatalf("keep gave no confirmation: %v", out)
	}
	if !isKept(stick) {
		t.Fatal("`keep` did not flag the item")
	}

	// Drop is refused.
	if out, _ := e.run("drop stick"); stick.location != e.actor.entity || !has(out, "keep") {
		t.Fatalf("a kept item was dropped (or no message): loc-ok=%v out=%v", stick.location == e.actor.entity, out)
	}
	// Put is refused too.
	if out, _ := e.run("put stick in box"); stick.location != e.actor.entity || !has(out, "keep") {
		t.Fatalf("a kept item was put in a container (or no message): out=%v", out)
	}
	_ = box

	// Unkeep, then drop succeeds.
	if out, _ := e.run("unkeep stick"); !has(out, "keep") {
		t.Fatalf("unkeep gave no confirmation: %v", out)
	}
	if isKept(stick) {
		t.Fatal("`unkeep` did not clear the flag")
	}
	e.run("drop stick")
	if stick.location == e.actor.entity {
		t.Fatal("drop after unkeep failed")
	}
}

// TestKeepFlagSurvivesReload proves the keep flag round-trips through the item-instance delta (the
// dumpCharacter → loadCharacter persistence path).
func TestKeepFlagSurvivesReload(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Sam"}
	e := z.newPlayerEntity(src, "Sam")
	item := z.spawn(ProtoRef("midgaard:obj:torch"))
	keepItem(item)
	Move(item, e)

	snap := dumpCharacter(src)
	dst := &session{character: "Sam"}
	z.newPlayerEntity(dst, "Sam")
	loadCharacter(z, dst, snap)

	var reloaded *Entity
	for _, it := range dst.entity.contents {
		if it.proto == "midgaard:obj:torch" {
			reloaded = it
		}
	}
	if reloaded == nil {
		t.Fatal("the torch is not in the reloaded inventory")
	}
	if !isKept(reloaded) {
		t.Fatal("the keep flag did not survive a reload")
	}
}

// TestKeptItemDeltaRoundTrips is the hermetic field-drop guard for the Kept delta field (the store
// field-drop class): dump/load share itemDeltaJSON, so pinning the round-trip catches a mistyped tag.
func TestKeptItemDeltaRoundTrips(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	item := z.spawn(ProtoRef("midgaard:obj:torch"))
	keepItem(item)
	delta := dumpItemDelta(item)
	if len(delta) == 0 {
		t.Fatal("a kept item produced no delta")
	}
	item2 := z.spawn(ProtoRef("midgaard:obj:torch"))
	loadItemDelta(item2, delta)
	if !isKept(item2) {
		t.Fatal("the kept flag was dropped in the item-delta round-trip")
	}
}

// TestItemDeltaFamilyRoundTrips extends the Kept guard above to the WHOLE item-delta family (#87): an item
// carrying quality (level + affixes) AND bound AND a partial stack AND kept must dump to itemDeltaJSON and
// re-load with every field intact. dump/load share itemDeltaJSON, so this catches a mistyped tag or a
// dump/load asymmetry on ANY of the four fields — the hermetic counterpart to the real-PG round-trip in
// tests/integration (TestCharacterItemDeltaRoundTrip).
func TestItemDeltaFamilyRoundTrips(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	item := z.spawn(ProtoRef("midgaard:obj:torch"))
	Add(item, &Quality{Level: 5, Affixes: map[string]float64{"fire": 2, "keen": 1}})
	bindItem(item)
	setItemStackCount(item, 7)
	keepItem(item)

	delta := dumpItemDelta(item)
	if len(delta) == 0 {
		t.Fatal("a fully-populated item produced no delta")
	}

	item2 := z.spawn(ProtoRef("midgaard:obj:torch"))
	loadItemDelta(item2, delta)

	q, ok := Get[*Quality](item2)
	if !ok || q.Level != 5 {
		t.Fatalf("quality dropped/altered in the delta round-trip: ok=%v q=%+v", ok, q)
	}
	if q.Affixes["fire"] != 2 || q.Affixes["keen"] != 1 {
		t.Fatalf("quality affixes dropped in the delta round-trip: %+v", q.Affixes)
	}
	if !isBound(item2) {
		t.Fatal("the bound flag was dropped in the delta round-trip")
	}
	if s, ok := Get[*Stack](item2); !ok || s.count != 7 {
		t.Fatalf("the stack count was dropped/altered: ok=%v s=%+v", ok, s)
	}
	if !isKept(item2) {
		t.Fatal("the kept flag was dropped in the delta round-trip")
	}
}
