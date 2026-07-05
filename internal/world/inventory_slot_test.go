package world

import (
	"strings"
	"testing"
)

// inventory_slot_test.go — #85: `inventory` folds worn items in, grouped/flagged by slot, and a display
// template can read the per-slot equipment via self:equipment_slots(). Done-when: a worn helmet shows as
// "<worn on head> an iron helmet" in the inventory listing, worn items precede loose ones in content-slot
// order, and the handle accessor exposes each worn item's slot label + flag.

// TestInventoryFoldsWornBySlot: worn items appear in the inventory listing flagged by slot, in content order,
// ahead of the loose-carried items.
func TestInventoryFoldsWornBySlot(t *testing.T) {
	e := newCmdEnv(t)
	actor := e.actor.entity

	addTestItem(e.z, actor, "an iron helmet", []string{"helmet"}, wearableFor(WearLocHead))
	addTestItem(e.z, actor, "a steel sword", []string{"sword"},
		wearableFor(WearLocWield), &Weapon{diceNum: 1, diceSize: 8, damageType: "slash"})
	addTestItem(e.z, actor, "a pine torch", []string{"torch"}) // a loose, unworn item

	e.run("wear helmet")
	e.run("wield sword")

	inv, _ := e.run("inventory")
	joined := strings.Join(inv, "\n")

	if !strings.Contains(joined, "<worn on head> an iron helmet") {
		t.Errorf("inventory should flag the helmet by its worn slot: %q", joined)
	}
	if !strings.Contains(joined, "<wielded> a steel sword") {
		t.Errorf("inventory should flag the wielded sword: %q", joined)
	}
	if !strings.Contains(joined, "pine torch") { // loose lines are initial-capped (Track 1): "A pine torch"
		t.Errorf("inventory should still list the loose torch: %q", joined)
	}
	// Worn items precede the loose torch (content-slot order first, then carried).
	if iHead, iTorch := strings.Index(joined, "iron helmet"), strings.Index(joined, "pine torch"); iHead > iTorch {
		t.Errorf("worn items should be listed before loose items: %q", joined)
	}
	// The head slot (order 10) precedes the wield slot (order 50).
	if iHead, iWield := strings.Index(joined, "iron helmet"), strings.Index(joined, "steel sword"); iHead > iWield {
		t.Errorf("worn items should follow content slot order (head before wield): %q", joined)
	}
}

// TestInventoryEmptyStillReportsNothing: with nothing worn and nothing carried, the listing says "Nothing."
func TestInventoryEmptyStillReportsNothing(t *testing.T) {
	e := newCmdEnv(t)
	inv, _ := e.run("inventory")
	joined := strings.Join(inv, "\n")
	if !strings.Contains(joined, "Nothing.") {
		t.Errorf("an empty inventory should say Nothing: %q", joined)
	}
}

// TestEquipmentSlotsHandleAccessor: a display template can read each worn item's slot flag + label + handle
// via self:equipment_slots() (#85) — the bare self:equipment() list can't render "<worn on head>".
func TestEquipmentSlotsHandleAccessor(t *testing.T) {
	z := newZone("eqslots")
	z.defBundle().displayDefs["inventory"] = `
		local s = ui.sheet()
		for _, eq in ipairs(self:equipment_slots()) do
			s:row({eq.flag .. " " .. eq.item:name() .. " [" .. eq.slot .. "]"})
		end
		return s:render()`
	sess := scorePlayer(z, "Bilbo")
	helm := addTestItem(z, sess.entity, "an iron helmet", []string{"helmet"}, wearableFor(WearLocHead))
	// Equip directly so the test isolates the equipment_slots accessor (not the wear-command path).
	wr := actorWearer(sess.entity)
	wr.worn[WearLocHead] = helm

	z.dispatch(sess, "inventory")
	out := drainText(t, sess.out)
	if !strings.Contains(out, "<worn on head> an iron helmet") {
		t.Fatalf("equipment_slots template did not render the worn flag + item: %q", out)
	}
	if !strings.Contains(out, "[head]") {
		t.Fatalf("equipment_slots should expose the raw slot label: %q", out)
	}
}
