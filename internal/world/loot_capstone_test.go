package world

import (
	"math/rand"
	"testing"
)

// loot_capstone_test.go is the Phase-12 loot capstone (docs/LOOT-AND-SPAWNS.md §7): the combined boss
// scenario, hermetic + -race — a raid kills the boss, EVERY eligible player gets personal loot (a
// guaranteed rare item with rolled affixes), each INDEPENDENTLY chases a rare legendary whose pity timer
// nudges it up every miss until it drops + resets, and the whole grind SURVIVES A RESTART. (The schedule
// half — the director spawning the boss weekly + resuming across a restart — is proven in the director
// scheduler tests; this capstone is the player-facing loot experience the schedule feeds.)

// raidKill makes a fresh boss the whole raid has damaged (eligibility) with the given loot table, then
// runs the on-death resolver — one weekly kill.
func raidKill(z *Zone, table string, rng *rand.Rand, raid ...*Entity) {
	mob := makeMobTarget(z, raid[0], "warden")
	for _, p := range raid {
		addThreat(mob, p, 50)
	}
	mutableLiving(mob).lootTable = table
	z.resolveLoot(mob, rng)
}

func TestLootCapstoneWeeklyBossPersonalLootPityAndRestart(t *testing.T) {
	z := lootZone(t) // ships common/rare tiers + spawnable demo items
	// The Warden's table: a GUARANTEED rare warden-blade (with rolled quality) for every looter, plus an
	// INDEPENDENT legendary "sunsword" chance with a pity timer (0% base, +100%/miss, capped at 100% — so
	// after one weekly miss the next kill forces it: the bounded "grind for months", compressed for a test).
	z.defs.loot.register("warden", &lootTableDef{ref: "warden", rolls: []lootRoll{
		{kind: "guaranteed", pool: []lootEntry{{
			item:    "midgaard:obj:sword",
			tier:    "rare",
			quality: &qualitySpec{count: 1, levelMin: 1, levelMax: 50, affixes: []affixRoll{{attr: "strength", min: 1, max: 10}}},
		}}},
		{
			kind: "chance", chance: 0.0, pity: &lootPity{key: "sunsword", step: 1.0, cap: 1.0},
			pool: []lootEntry{{item: "midgaard:obj:torch", tier: "rare"}},
		}, // "sunsword" stand-in
	}})

	raid := []*Entity{
		z.newPlayerEntity(&session{character: "Aragorn"}, "Aragorn"),
		z.newPlayerEntity(&session{character: "Gimli"}, "Gimli"),
		z.newPlayerEntity(&session{character: "Legolas"}, "Legolas"),
	}
	rng := rand.New(rand.NewSource(1))

	// --- Week 1: the raid kills the Warden. Every player gets their OWN rare blade with rolled quality
	// (personal loot, no contested pickup). Nobody gets the legendary yet (0% base) — pity ticks to 1. ---
	raidKill(z, "warden", rng, raid...)
	for _, p := range raid {
		if countItems(p, "midgaard:obj:sword") != 1 {
			t.Fatalf("%s did not get their personal guaranteed blade", p.short)
		}
		if itemWithQuality(p) == nil {
			t.Fatalf("%s's blade carries no rolled quality", p.short)
		}
		if lootPityMisses(p, "sunsword") != 1 {
			t.Fatalf("%s pity = %d after one miss, want 1", p.short, lootPityMisses(p, "sunsword"))
		}
		if countItems(p, "midgaard:obj:torch") != 0 {
			t.Fatalf("%s got the legendary at 0%% base", p.short)
		}
	}

	// --- A returning raider RELOADS mid-grind: their pity progress (and their blade) must survive. ---
	gimli := raid[1]
	snap := dumpCharacter(&session{character: "Gimli", entity: gimli})
	if snap.State.LootPity["sunsword"] != 1 {
		t.Fatalf("dumped pity = %v, want sunsword:1", snap.State.LootPity)
	}
	dst := &session{character: "Gimli"}
	z.newPlayerEntity(dst, "Gimli")
	loadCharacter(z, dst, snap)
	if lootPityMisses(dst.entity, "sunsword") != 1 {
		t.Fatal("a returning raider lost their pity progress across a relogin")
	}
	if itemWithQuality(dst.entity) == nil {
		t.Fatal("a returning raider lost their rolled blade across a relogin")
	}
	raid[1] = dst.entity // the reloaded Gimli rejoins the raid

	// --- Week 2: pity has climbed to the 100% cap (0% base + 1 miss * 100%/miss), so the second weekly
	// kill FORCES the legendary for every raider — the bounded "grind" pays out — and the counter resets. ---
	raidKill(z, "warden", rng, raid...)
	for _, p := range raid {
		if countItems(p, "midgaard:obj:torch") != 1 {
			t.Fatalf("%s did not get the legendary once pity reached the cap", p.short)
		}
		if lootPityMisses(p, "sunsword") != 0 {
			t.Fatalf("%s pity = %d after the legendary dropped, want 0 (reset)", p.short, lootPityMisses(p, "sunsword"))
		}
	}
}
