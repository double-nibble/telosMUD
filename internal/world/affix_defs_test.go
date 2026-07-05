package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// demoMidgaard boots a shard from the full demo pack and returns its midgaard zone, so a test can read the
// registries build.go populated (rarity + affix + loot) — the end-to-end content-build path.
func demoMidgaard(t *testing.T) *Zone {
	t.Helper()
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	sh := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "", nil, nil)
	z := sh.ZoneByID("midgaard")
	if z == nil {
		t.Fatal("midgaard zone not built from the demo pack")
	}
	return z
}

// affix_defs_test.go — #37: a loot entry's quality pool can reference a shared, named affix_def by ref
// instead of inlining attr/min/max. buildLootTableDef resolves the ref against the per-shard affix registry
// at build time, so an edit to the def propagates to every referencing pool on reload.

// affixReg builds an affix registry seeded with the given defs (test helper).
func affixReg(defs ...*affixDef) *defRegistry[*affixDef] {
	r := newDefRegistry[*affixDef]()
	for _, d := range defs {
		r.register(d.ref, d)
	}
	return r
}

// lootDTOWithAffix builds a one-entry loot table whose sole quality affix is `a`.
func lootDTOWithAffix(a content.AffixRollDTO) content.LootTableDTO {
	return content.LootTableDTO{
		Ref: "t",
		Rolls: []content.LootRollDTO{{
			Kind: "guaranteed",
			Pool: []content.LootEntryDTO{{
				Item: "midgaard:obj:sword", Tier: "rare",
				Quality: &content.QualitySpecDTO{Count: 1, LevelMin: 1, LevelMax: 10, Affixes: []content.AffixRollDTO{a}},
			}},
		}},
	}
}

// firstAffix returns the resolved affixRoll of the built table's single quality entry.
func firstAffix(t *testing.T, def *lootTableDef) affixRoll {
	t.Helper()
	if len(def.rolls) == 0 || len(def.rolls[0].pool) == 0 || def.rolls[0].pool[0].quality == nil ||
		len(def.rolls[0].pool[0].quality.affixes) == 0 {
		t.Fatal("built loot table has no resolved affix")
	}
	return def.rolls[0].pool[0].quality.affixes[0]
}

func TestBuildLootResolvesAffixRef(t *testing.T) {
	reg := affixReg(&affixDef{ref: "of_the_bear", attr: "strength", min: 1, max: 5})
	def := buildLootTableDef(lootDTOWithAffix(content.AffixRollDTO{Ref: "of_the_bear"}), reg)

	got := firstAffix(t, def)
	if got.attr != "strength" || got.min != 1 || got.max != 5 {
		t.Fatalf("a ref affix resolved to %+v, want strength 1-5 from the affix_def", got)
	}
}

// TestBuildLootInlineAffixStillWorks: an inline affix (no ref) keeps using its own attr/min/max (the pre-#37
// form must coexist with the ref form).
func TestBuildLootInlineAffixStillWorks(t *testing.T) {
	def := buildLootTableDef(lootDTOWithAffix(content.AffixRollDTO{Attr: "intellect", Min: 2, Max: 4}), affixReg())
	got := firstAffix(t, def)
	if got.attr != "intellect" || got.min != 2 || got.max != 4 {
		t.Fatalf("an inline affix resolved to %+v, want intellect 2-4", got)
	}
}

// TestBuildLootUnknownAffixRefInert: a ref that names no loaded affix_def resolves to an inert (empty-attr)
// affix — a misauthored ref degrades to a no-op, never a crash or a wrong attribute.
func TestBuildLootUnknownAffixRefInert(t *testing.T) {
	def := buildLootTableDef(lootDTOWithAffix(content.AffixRollDTO{Ref: "of_the_nonexistent"}), affixReg())
	got := firstAffix(t, def)
	if got.attr != "" {
		t.Fatalf("an unknown affix ref resolved to %+v, want an inert empty affix", got)
	}
}

// TestDemoGoblinLootUsesNamedAffix: the demo goblin_loot's rare sword references the `of_the_bear` affix_def,
// which resolves through the shard's registry to a strength roll — the end-to-end content wiring.
func TestDemoGoblinLootUsesNamedAffix(t *testing.T) {
	z := demoMidgaard(t)
	table := z.lootTableDefs().get("goblin_loot")
	if table == nil {
		t.Fatal("demo goblin_loot table not loaded")
	}
	// Find the sword entry's resolved affix.
	var found bool
	for _, r := range table.rolls {
		for _, e := range r.pool {
			if e.quality == nil {
				continue
			}
			for _, a := range e.quality.affixes {
				if a.attr == "strength" && a.min == 1 && a.max == 5 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("goblin_loot's of_the_bear ref did not resolve to the strength 1-5 affix")
	}
}
