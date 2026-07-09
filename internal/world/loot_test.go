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

// TestPityAdjustedChance pins the pity curve: base + misses*step, clamped to the cap.
func TestPityAdjustedChance(t *testing.T) {
	z := lootZone(t)
	player := z.newPlayerEntity(&session{character: "Hero"}, "Hero")
	roll := &lootRoll{kind: "chance", chance: 0.1, pity: &lootPity{key: "sunsword", step: 0.05, cap: 0.3}}

	if got := pityAdjustedChance(roll, player); got != 0.1 {
		t.Fatalf("0 misses: chance = %v, want 0.1 (base)", got)
	}
	setLootPityMisses(player, "sunsword", 2)
	if got := pityAdjustedChance(roll, player); got != 0.2 {
		t.Fatalf("2 misses: chance = %v, want 0.2 (0.1 + 2*0.05)", got)
	}
	setLootPityMisses(player, "sunsword", 100)
	if got := pityAdjustedChance(roll, player); got != 0.3 {
		t.Fatalf("many misses: chance = %v, want 0.3 (capped)", got)
	}
}

// TestPityAccumulatesAndResets proves the resolver raises a looter's miss counter on a miss and resets it
// on a hit — deterministic with a chance of 0 (always miss until pity forces it to 1.0, then hit).
func TestPityAccumulatesAndResets(t *testing.T) {
	z := lootZone(t)
	z.defs.loot.register("pity_table", &lootTableDef{ref: "pity_table", rolls: []lootRoll{
		{
			kind: "chance", chance: 0.0, pity: &lootPity{key: "sunsword", step: 1.0, cap: 1.0},
			pool: []lootEntry{{item: "midgaard:obj:sword"}},
		},
	}})
	player := z.newPlayerEntity(&session{character: "Hero"}, "Hero")

	// Kill 1: base chance 0 -> miss -> counter 1, no drop.
	z.resolveLoot(lootVictim(z, player, "pity_table"), rand.New(rand.NewSource(1)))
	if lootPityMisses(player, "sunsword") != 1 {
		t.Fatalf("after a miss: pity = %d, want 1", lootPityMisses(player, "sunsword"))
	}
	if countItems(player, "midgaard:obj:sword") != 0 {
		t.Fatal("a zero-base-chance roll dropped on the first kill")
	}
	// Kill 2: chance = 0 + 1*1.0 = 1.0 -> guaranteed hit -> drop + reset.
	z.resolveLoot(lootVictim(z, player, "pity_table"), rand.New(rand.NewSource(1)))
	if countItems(player, "midgaard:obj:sword") != 1 {
		t.Fatal("pity did not force the drop once it reached the cap")
	}
	if lootPityMisses(player, "sunsword") != 0 {
		t.Fatalf("after a hit: pity = %d, want 0 (reset)", lootPityMisses(player, "sunsword"))
	}
}

// TestResolveRollMutatePityGate pins the #181 seam directly: a "chance" miss advances the looter's pity
// counter only when mutatePity is true (normal loot + the base salvage pass). A no-mutate resolve (a salvage
// over-skill BONUS pass) still evaluates the same roll but leaves the counter untouched.
func TestResolveRollMutatePityGate(t *testing.T) {
	z := lootZone(t)
	player := z.newPlayerEntity(&session{character: "Hero"}, "Hero")
	roll := &lootRoll{
		kind: "chance", chance: 0.0, pity: &lootPity{key: "sunsword", step: 0.05, cap: 0.5},
		pool: []lootEntry{{item: "midgaard:obj:torch"}},
	}
	rng := rand.New(rand.NewSource(1))

	// A mutating miss advances the counter.
	z.resolveRoll(player, roll, rng, true)
	if got := lootPityMisses(player, "sunsword"); got != 1 {
		t.Fatalf("mutating miss: pity = %d, want 1", got)
	}
	// Three no-mutate misses (the over-skill bonus passes) leave it at 1.
	for i := 0; i < 3; i++ {
		z.resolveRoll(player, roll, rng, false)
	}
	if got := lootPityMisses(player, "sunsword"); got != 1 {
		t.Fatalf("after no-mutate misses: pity = %d, want 1 (bonus passes must not advance it)", got)
	}
}

// TestPitySurvivesReload proves a looter's accumulated pity counter round-trips through dumpCharacter/
// loadCharacter — "I'm due a drop" progress is not lost on a relogin.
func TestPitySurvivesReload(t *testing.T) {
	z := lootZone(t)
	src := &session{character: "Hero"}
	e := z.newPlayerEntity(src, "Hero")
	setLootPityMisses(e, "sunsword", 7)

	snap := dumpCharacter(src)
	if snap.State.LootPity["sunsword"] != 7 {
		t.Fatalf("dumped loot_pity = %v, want sunsword:7", snap.State.LootPity)
	}
	dst := &session{character: "Hero"}
	z.newPlayerEntity(dst, "Hero")
	loadCharacter(z, dst, snap)
	if lootPityMisses(dst.entity, "sunsword") != 7 {
		t.Fatalf("reloaded pity = %d, want 7 (bad-luck protection must survive a relogin)", lootPityMisses(dst.entity, "sunsword"))
	}
}

// itemWithQuality returns the first item in e's contents that has a Quality component, or nil.
func itemWithQuality(e *Entity) *Quality {
	for _, it := range e.contents {
		if q, ok := Get[*Quality](it); ok {
			return q
		}
	}
	return nil
}

// TestLootRollsQuality proves a loot entry with a quality spec rolls a level + affixes onto the dropped
// instance, within the spec's ranges.
func TestLootRollsQuality(t *testing.T) {
	z := lootZone(t)
	z.defs.loot.register("epic", &lootTableDef{ref: "epic", rolls: []lootRoll{
		{kind: "guaranteed", pool: []lootEntry{{
			item:    "midgaard:obj:sword",
			quality: &qualitySpec{count: 2, levelMin: 5, levelMax: 10, affixes: []affixRoll{{attr: "strength", min: 1, max: 5}, {attr: "constitution", min: 1, max: 3}}},
		}}},
	}})
	player := z.newPlayerEntity(&session{character: "Hero"}, "Hero")
	z.resolveLoot(lootVictim(z, player, "epic"), rand.New(rand.NewSource(1)))

	q := itemWithQuality(player)
	if q == nil {
		t.Fatal("the dropped item carries no rolled Quality")
	}
	if q.Level < 5 || q.Level > 10 {
		t.Fatalf("rolled level = %d, want in [5,10]", q.Level)
	}
	if len(q.Affixes) == 0 {
		t.Fatal("no affixes rolled")
	}
	for attr, v := range q.Affixes {
		switch attr {
		case "strength":
			if v < 1 || v > 5 {
				t.Fatalf("strength affix = %v, want in [1,5]", v)
			}
		case "constitution":
			if v < 1 || v > 3 {
				t.Fatalf("constitution affix = %v, want in [1,3]", v)
			}
		default:
			t.Fatalf("unexpected affix attr %q", attr)
		}
	}
}

// TestLootQualityVaries proves two drops of the SAME prototype carry DIFFERENT rolled quality (the
// prototype stays shared; only the per-instance delta varies). Driven by advancing the same rng.
func TestLootQualityVaries(t *testing.T) {
	z := lootZone(t)
	z.defs.loot.register("epic", &lootTableDef{ref: "epic", rolls: []lootRoll{
		{kind: "guaranteed", pool: []lootEntry{{
			item:    "midgaard:obj:sword",
			quality: &qualitySpec{count: 1, levelMin: 1, levelMax: 100, affixes: []affixRoll{{attr: "strength", min: 1, max: 100}}},
		}}},
	}})
	rng := rand.New(rand.NewSource(42))
	a := z.newPlayerEntity(&session{character: "Alice"}, "Alice")
	b := z.newPlayerEntity(&session{character: "Bob"}, "Bob")

	z.resolveLoot(lootVictim(z, a, "epic"), rng)
	z.resolveLoot(lootVictim(z, b, "epic"), rng)
	qa, qb := itemWithQuality(a), itemWithQuality(b)
	if qa == nil || qb == nil {
		t.Fatal("both drops should carry quality")
	}
	if qa.Level == qb.Level && qa.Affixes["strength"] == qb.Affixes["strength"] {
		t.Fatalf("two drops rolled identical quality (lvl %d/str %v) — instances must vary", qa.Level, qa.Affixes["strength"])
	}
}

// TestQualitySurvivesReload proves a rolled item's quality round-trips through the per-instance delta
// (dumpItem -> ItemJSON.Delta -> loadItem).
func TestQualitySurvivesReload(t *testing.T) {
	z := lootZone(t)
	z.defs.loot.register("epic", &lootTableDef{ref: "epic", rolls: []lootRoll{
		{kind: "guaranteed", pool: []lootEntry{{
			item:    "midgaard:obj:sword",
			quality: &qualitySpec{count: 1, levelMin: 7, levelMax: 7, affixes: []affixRoll{{attr: "strength", min: 3, max: 3}}},
		}}},
	}})
	src := &session{character: "Hero"}
	player := z.newPlayerEntity(src, "Hero")
	z.resolveLoot(lootVictim(z, player, "epic"), rand.New(rand.NewSource(1)))
	want := itemWithQuality(player)
	if want == nil {
		t.Fatal("no quality rolled (precondition)")
	}

	snap := dumpCharacter(src)
	dst := &session{character: "Hero"}
	z.newPlayerEntity(dst, "Hero")
	loadCharacter(z, dst, snap)

	got := itemWithQuality(dst.entity)
	if got == nil {
		t.Fatal("rolled quality lost on reload")
	}
	if got.Level != want.Level || got.Affixes["strength"] != want.Affixes["strength"] {
		t.Fatalf("reloaded quality = lvl %d str %v, want lvl %d str %v",
			got.Level, got.Affixes["strength"], want.Level, want.Affixes["strength"])
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
