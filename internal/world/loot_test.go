package world

import (
	"math/rand"
	"testing"
)

// loot_test.go — the Phase-12.1 loot resolver: personal-loot delivery to eligible looters, deterministic
// rolls under a seed, weighted picks, the quality_floor filter, and the no-table no-op.

// lootZone is a demo zone (it ships spawnable item prototypes) with registered rarity tiers + a helper to
// register a loot table and attach it to a fresh mob victim a player has damaged.
func lootZone(t *testing.T) *Zone {
	t.Helper()
	z := newDemoZone("midgaard", newProtoCache())
	z.defs.rarity.register("common", &rarityTierDef{ref: "common", order: 0, weight: 100})
	z.defs.rarity.register("rare", &rarityTierDef{ref: "rare", order: 2, weight: 5})
	return z
}

// lootVictim makes a mob in the player's room, records that the player damaged it (eligibility), and
// stamps the loot table ref onto it. Returns the victim.
func lootVictim(z *Zone, player *Entity, table string) *Entity {
	mob := makeMobTarget(z, player, "warden")
	addThreat(mob, player, 50)
	mutableLiving(mob).lootTable = table
	return mob
}

func countItems(e *Entity, ref string) int {
	n := 0
	for _, it := range e.contents {
		if string(it.proto) == ref {
			n++
		}
	}
	return n
}

func TestLootResolverDeliversGuaranteedDrop(t *testing.T) {
	z := lootZone(t)
	z.defs.loot.register("warden_loot", &lootTableDef{ref: "warden_loot", rolls: []lootRoll{
		{kind: "guaranteed", pool: []lootEntry{{item: "midgaard:obj:torch", tier: "common"}}},
	}})
	src := &session{character: "Hero"}
	player := z.newPlayerEntity(src, "Hero")
	victim := lootVictim(z, player, "warden_loot")

	z.resolveLoot(victim, rand.New(rand.NewSource(1)))

	if countItems(player, "midgaard:obj:torch") != 1 {
		t.Fatalf("guaranteed drop: player has %d torches, want 1 (personal loot delivered)", countItems(player, "midgaard:obj:torch"))
	}
}

// TestLootChanceRespectsSeed proves a chance roll is deterministic under a seed: a chance of 1.0 always
// drops, 0.0 never does.
func TestLootChanceRespectsSeed(t *testing.T) {
	z := lootZone(t)
	z.defs.loot.register("always", &lootTableDef{ref: "always", rolls: []lootRoll{
		{kind: "chance", chance: 1.0, pool: []lootEntry{{item: "midgaard:obj:torch"}}},
	}})
	z.defs.loot.register("never", &lootTableDef{ref: "never", rolls: []lootRoll{
		{kind: "chance", chance: 0.0, pool: []lootEntry{{item: "midgaard:obj:torch"}}},
	}})
	src := &session{character: "Hero"}
	player := z.newPlayerEntity(src, "Hero")

	z.resolveLoot(lootVictim(z, player, "always"), rand.New(rand.NewSource(1)))
	if countItems(player, "midgaard:obj:torch") != 1 {
		t.Fatal("chance 1.0 must always drop")
	}
	z.resolveLoot(lootVictim(z, player, "never"), rand.New(rand.NewSource(1)))
	if countItems(player, "midgaard:obj:torch") != 1 {
		t.Fatal("chance 0.0 must never drop (count should stay 1)")
	}
}

// TestLootQualityFloorFilters proves quality_floor keeps only entries at or above the floor tier — a
// weighted_one with floor "rare" can only pick the rare item even though a common is in the pool.
func TestLootQualityFloorFilters(t *testing.T) {
	z := lootZone(t)
	z.defs.loot.register("floored", &lootTableDef{ref: "floored", rolls: []lootRoll{
		{kind: "weighted_one", qualityFloor: "rare", pool: []lootEntry{
			{item: "midgaard:obj:torch", tier: "common"}, // below the floor — filtered out
			{item: "midgaard:obj:sword", tier: "rare"},   // at the floor — eligible
		}},
	}})
	src := &session{character: "Hero"}
	player := z.newPlayerEntity(src, "Hero")
	z.resolveLoot(lootVictim(z, player, "floored"), rand.New(rand.NewSource(7)))

	if countItems(player, "midgaard:obj:torch") != 0 {
		t.Fatal("a below-floor common item was dropped despite quality_floor=rare")
	}
	if countItems(player, "midgaard:obj:sword") != 1 {
		t.Fatal("the at-floor rare item was not dropped")
	}
}

// TestLootIsPersonalPerLooter proves each eligible player rolls + receives INDEPENDENTLY (personal loot):
// two damagers each get their own guaranteed drop.
func TestLootIsPersonalPerLooter(t *testing.T) {
	z := lootZone(t)
	z.defs.loot.register("warden_loot", &lootTableDef{ref: "warden_loot", rolls: []lootRoll{
		{kind: "guaranteed", pool: []lootEntry{{item: "midgaard:obj:torch"}}},
	}})
	a := z.newPlayerEntity(&session{character: "Alice"}, "Alice")
	b := z.newPlayerEntity(&session{character: "Bob"}, "Bob")
	mob := makeMobTarget(z, a, "warden")
	addThreat(mob, a, 30)
	addThreat(mob, b, 20)
	mutableLiving(mob).lootTable = "warden_loot"

	z.resolveLoot(mob, rand.New(rand.NewSource(3)))
	if countItems(a, "midgaard:obj:torch") != 1 || countItems(b, "midgaard:obj:torch") != 1 {
		t.Fatalf("personal loot: Alice=%d Bob=%d torches, want 1 each",
			countItems(a, "midgaard:obj:torch"), countItems(b, "midgaard:obj:torch"))
	}
}

func TestLootNoTableNoOp(t *testing.T) {
	z := lootZone(t)
	src := &session{character: "Hero"}
	player := z.newPlayerEntity(src, "Hero")
	mob := makeMobTarget(z, player, "rat")
	addThreat(mob, player, 5)
	// No loot table set on the mob.
	z.resolveLoot(mob, rand.New(rand.NewSource(1))) // must not panic
	if len(player.contents) != 0 {
		t.Fatalf("a tableless mob dropped %d items, want 0", len(player.contents))
	}
}
