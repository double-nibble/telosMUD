package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// wearslot_test.go — #35 content-defined wear slots. Done-when: the slot vocabulary is content, not an engine
// enum; a pack can add a NEW slot (the demo's "waist") that an item wears in; the engine ships a default set
// when a pack declares none; and equip-verb routing follows a slot's content `kind`.

// TestDefaultWearVocabWhenPackDeclaresNone: buildWearVocab(nil) yields nil so z.wearSlots falls back to the
// engine default (the classic Diku core), and that default has the expected slots/kinds.
func TestDefaultWearVocabWhenPackDeclaresNone(t *testing.T) {
	if buildWearVocab(nil) != nil {
		t.Fatal("an empty slot list must build a nil vocab (so the engine default applies)")
	}
	v := defaultWearVocab
	for _, ref := range []WearLoc{WearLocHead, WearLocBody, WearLocHands, WearLocFeet, WearLocWield, WearLocHold} {
		if !v.has(ref) {
			t.Fatalf("default vocab missing slot %q", ref)
		}
	}
	if v.kindOf(WearLocWield) != content.WearKindWield || v.kindOf(WearLocHold) != content.WearKindHold {
		t.Fatal("default vocab hand-slot kinds are wrong")
	}
	if v.slotOfKind(content.WearKindWield) != WearLocWield {
		t.Fatalf("slotOfKind(wield) = %q, want %q", v.slotOfKind(content.WearKindWield), WearLocWield)
	}
}

// TestContentWearVocabOrderAndLabels: buildWearVocab sorts by Order (ref tiebreak) and carries labels/kinds,
// so the equipment list + the `N.` ordinal agree on a stable, content-driven order.
func TestContentWearVocabOrderAndLabels(t *testing.T) {
	v := buildWearVocab([]content.WearSlotDTO{
		{Ref: "wield", Label: "wielded", Order: 50, Kind: "wield"},
		{Ref: "head", Label: "head", Order: 10, Kind: "worn"},
		{Ref: "waist", Label: "waist", Order: 25}, // kind omitted => defaults to "worn"
	})
	refs := v.orderedRefs()
	want := []WearLoc{"head", "waist", "wield"}
	if len(refs) != len(want) {
		t.Fatalf("orderedRefs = %v, want %v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Fatalf("orderedRefs[%d] = %q, want %q (order: %v)", i, refs[i], want[i], refs)
		}
	}
	if v.label(WearLoc("wield")) != "wielded" {
		t.Fatalf("label(wield) = %q, want wielded", v.label("wield"))
	}
	if v.kindOf("waist") != content.WearKindWorn {
		t.Fatalf("an omitted kind must default to worn, got %q", v.kindOf("waist"))
	}
}

// TestWearContentDefinedSlot: the demo pack adds a "waist" slot the engine default lacks; a belt authored for
// it wears there and shows under the content label in the equipment list.
func TestWearContentDefinedSlot(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity

	// The demo's wear_slots section must have loaded the new slot.
	if !e.z.wearSlots().has(WearLoc("waist")) {
		t.Fatal("the demo pack's content-defined `waist` slot did not load")
	}

	belt := e.z.spawn(ProtoRef("midgaard:obj:leather-belt"))
	Move(belt, actor)

	e.run("wear belt")
	wr, _ := Get[*Wearer](actor)
	if wr.worn[WearLoc("waist")] != belt {
		t.Fatal("the belt should occupy the content-defined `waist` slot")
	}
	eq, _ := e.run("equipment")
	if !has(eq, "waist") || !has(eq, "leather belt") {
		t.Fatalf("equipment should list the belt under the waist slot: %v", eq)
	}
}

// TestWieldRoutesByKindNotRef: the wield verb resolves its slot by KIND, so it wields into whatever slot the
// content marks kind "wield" (here the demo's canonical "wield" ref).
func TestWieldRoutesByKindNotRef(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity
	if e.z.wieldSlot() != WearLocWield {
		t.Fatalf("demo wieldSlot() = %q, want %q", e.z.wieldSlot(), WearLocWield)
	}
	sword := addTestItem(e.z, actor, "a steel sword", []string{"sword"},
		wearableFor(WearLocWield), &Weapon{diceNum: 1, diceSize: 8, damageType: "slash"})
	e.run("wield sword")
	wr, _ := Get[*Wearer](actor)
	if wr.worn[e.z.wieldSlot()] != sword {
		t.Fatal("the sword should be in the kind-resolved wield slot")
	}
}

// TestResolveKeyToleratesLegacyLabels: a pre-#35 save keyed a worn item by the slot LABEL ("wielded"/"held");
// resolveKey maps those legacy keys back to the slot ref so old characters still load their gear.
func TestResolveKeyToleratesLegacyLabels(t *testing.T) {
	v := defaultWearVocab
	cases := map[string]WearLoc{
		"wield":   WearLocWield, // the ref
		"wielded": WearLocWield, // the label (a legacy save key)
		"held":    WearLocHold,  // legacy hand label
		"head":    WearLocHead,  // ref == label
	}
	for key, want := range cases {
		if got, ok := v.resolveKey(key); !ok || got != want {
			t.Fatalf("resolveKey(%q) = (%q,%v), want %q", key, got, ok, want)
		}
	}
	if _, ok := v.resolveKey("nonsense"); ok {
		t.Fatal("resolveKey must reject an unknown slot key")
	}
}
