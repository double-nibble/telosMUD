package world

import (
	"math/rand"
	"testing"
)

// loot_onroll_test.go — the on_roll(ctx) Lua loot hatch (docs/REMAINING.md §4): a body run once per looter
// after the declarative rolls, returning additional CONDITIONAL item refs delivered through the normal
// pipeline. Covers list delivery, branching on ctx (the looter/victim handles), and fail-closed.

// TestLootOnRollDeliversReturnedList proves a static list return delivers each ref through deliverLoot (two
// torches from a two-element list).
func TestLootOnRollDeliversReturnedList(t *testing.T) {
	z := lootZone(t)
	z.defs.loot.register("bonus", &lootTableDef{
		ref:    "bonus",
		onRoll: `return {"midgaard:obj:torch", "midgaard:obj:torch"}`,
	})
	src := &session{character: "Hero"}
	player := z.newPlayerEntity(src, "Hero")
	registerRoom(z, player.location)
	victim := lootVictim(z, player, "bonus")

	z.resolveLoot(victim, rand.New(rand.NewSource(1)))

	if got := countItems(player, "midgaard:obj:torch"); got != 2 {
		t.Fatalf("on_roll list: player has %d torches, want 2 (both refs delivered)", got)
	}
}

// TestLootOnRollBranchesOnCtx proves the hatch reads ctx: it drops only when the victim's short matches, so
// the SAME body drops for a "warden" victim and drops nothing for a "goblin" victim.
func TestLootOnRollBranchesOnCtx(t *testing.T) {
	const body = `if ctx.source:short() == "warden" then return {"midgaard:obj:torch"} else return {} end`
	z := lootZone(t)
	z.defs.loot.register("cond", &lootTableDef{ref: "cond", onRoll: body})

	// A warden victim: the condition is true → one torch. makeRoomPlayer + registerRoom so the on_roll
	// body's ctx.source handle re-resolves (entityByRID walks z.rooms).
	hero := makeRoomPlayer(z, "Hero").entity
	registerRoom(z, hero.location)
	z.resolveLoot(lootVictim(z, hero, "cond"), rand.New(rand.NewSource(1)))
	if got := countItems(hero, "midgaard:obj:torch"); got != 1 {
		t.Fatalf("on_roll(warden): hero has %d torches, want 1 (condition true)", got)
	}

	// A goblin victim: the condition is false → no drop.
	rogue := makeRoomPlayer(z, "Rogue").entity
	registerRoom(z, rogue.location)
	gob := makeMobTarget(z, rogue, "goblin")
	addThreat(gob, rogue, 50)
	mutableLiving(gob).lootTable = "cond"
	z.resolveLoot(gob, rand.New(rand.NewSource(1)))
	if got := countItems(rogue, "midgaard:obj:torch"); got != 0 {
		t.Fatalf("on_roll(goblin): rogue has %d torches, want 0 (condition false)", got)
	}
}

// TestLootOnRollCapsDeliveredCount proves a runaway body returning an over-long list is truncated to
// maxLootOnRollDrops (the Lua budget bounds execution, not the returned array size).
func TestLootOnRollCapsDeliveredCount(t *testing.T) {
	z := lootZone(t)
	z.defs.loot.register("flood", &lootTableDef{
		ref:    "flood",
		onRoll: `local t = {} for i = 1, 500 do t[i] = "midgaard:obj:torch" end return t`,
	})
	p := z.newPlayerEntity(&session{character: "Hero"}, "Hero")
	registerRoom(z, p.location)
	z.resolveLoot(lootVictim(z, p, "flood"), rand.New(rand.NewSource(1)))
	if got := countItems(p, "midgaard:obj:torch"); got != maxLootOnRollDrops {
		t.Fatalf("on_roll flood: player has %d torches, want the cap %d", got, maxLootOnRollDrops)
	}
}

// TestLootOnRollFailsClosed proves a non-list return and a runtime error each add zero drops (a broken
// hatch never fabricates loot, and never crashes the resolve).
func TestLootOnRollFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"non-table return", `return 7`},
		{"nil return", `return nil`},
		{"runtime error", `error("boom")`},
		{"non-string elements", `return {1, 2, 3}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := lootZone(t)
			z.defs.loot.register("brk", &lootTableDef{ref: "brk", onRoll: tc.body})
			p := z.newPlayerEntity(&session{character: "Hero"}, "Hero")
			registerRoom(z, p.location)
			z.resolveLoot(lootVictim(z, p, "brk"), rand.New(rand.NewSource(1)))
			if got := countItems(p, "midgaard:obj:torch"); got != 0 {
				t.Fatalf("%s: player has %d torches, want 0 (fail-closed)", tc.name, got)
			}
		})
	}
}
