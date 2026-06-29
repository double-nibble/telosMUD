package world

import (
	"math/rand"
	"testing"
)

// play_loop_test.go is the W10 capstone milestone journey: the full PLAYER-DRIVEN play loop — fight a
// mob to death, loot its corpse, and equip the looted gear — composed in ONE test through the real
// command dispatch. The constituents are unit-pinned elsewhere (death→corpse→loot in death_test.go,
// wear/equipment in container/handoff tests); this ties them together as the integrated arc a player
// actually performs, catching a break in the SEAMS between systems (a corpse whose loot can't be looted,
// a looted item that can't then be worn) that the per-system tests do not exercise together.
func TestPlayLoopKillLootEquip(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("killer", killShotProfile(100)) // one swing's damage >> the mob's hp
	s.entity.living.combatRef = "killer"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})

	mob := combatMob(z, s.entity, "goblin", "", 10)
	// The goblin's loot: a WEARABLE helm (head slot) carried in its inventory.
	helm := z.newEntity(ProtoRef("test:helm"))
	helm.short = "a goblin helm"
	helm.setKeywords([]string{"helm"})
	Add(helm, wearableFor(WearLocHead))
	Move(helm, mob)

	// FIGHT: one deterministic killing swing drops the goblin and leaves a lootable corpse holding the helm.
	z.startFight(s.entity, mob)
	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	drainCombat(s) // clear the combat narration before the loot/equip assertions

	// LOOT: `get all corpse` moves the helm from the corpse into the player's inventory.
	z.dispatch(s, "get all corpse")
	if !drainContains(t, s, "You get a goblin helm") {
		t.Fatal("looting the corpse did not transfer the helm to the player")
	}
	if helm.location != s.entity {
		t.Fatalf("helm location = %v, want the player entity (loot did not land in inventory)", helm.location)
	}

	// EQUIP: wear the looted helm; it occupies the head slot and shows under equipment — the seam from
	// loot to a worn item, end to end.
	z.dispatch(s, "wear helm")
	if !drainContains(t, s, "You wear a goblin helm on your head.") {
		t.Fatal("the looted helm could not be worn")
	}
	z.dispatch(s, "equipment")
	if !drainContains(t, s, "<head> a goblin helm") {
		t.Fatal("the worn helm is not shown under equipment after looting + wearing")
	}
}
