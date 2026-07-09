package world

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// reload_shareddef_test.go — #56: pack-global SHARED DEFS (abilities/affects/formulas/pvp policy/…) have no
// hot-reload loop (the contentbus emits only room/item/mob/zone/channel invalidations), so a shared-def edit
// takes effect only after a ROLLING REBOOT. These tests pin the operator-facing reminder that makes that
// explicit, so a live-edited pvp policy / formula / ability is never silently un-applied.

// TestSharedDefKinds pins the detection over EVERY pack-global shared-def kind defineGlobals registers with no
// hot-reload loop (the reviewer caught 8 originally missed). A pack that sets all of them reports the full
// sorted label set; a rooms/items-only pack reports none. (A zero-value slice element suffices — the detector
// only checks presence.)
func TestSharedDefKinds(t *testing.T) {
	pk := content.Pack{
		Attributes:     []content.AttributeDTO{{}},
		Resources:      []content.ResourceDTO{{}},
		DamageTypes:    []content.DamageTypeDTO{{}},
		Affects:        []content.AffectDTO{{}},
		Abilities:      []content.AbilityDTO{{}},
		CombatProfiles: []content.CombatProfileDTO{{}},
		Tracks:         []content.TrackDTO{{}},
		Bundles:        []content.BundleDTO{{}},
		RarityTiers:    []content.RarityTierDTO{{}},
		Affixes:        []content.AffixDefDTO{{}},
		LootTables:     []content.LootTableDTO{{}},
		Recipes:        []content.RecipeDTO{{}},
		WearSlots:      []content.WearSlotDTO{{}},
		TrustTiers:     []content.TrustTierDTO{{}},
		Commands:       []content.CommandDTO{{}},
		DisplayDefs:    []content.DisplayDefDTO{{}},
		Formulas:       map[string]string{"regen": "return 1"},
		PvpLua:         "return true",
	}
	got := sharedDefKinds([]content.Pack{pk})
	want := []string{
		"abilities", "affects", "affixes", "attributes", "bundles", "combat profiles", "custom commands",
		"damage types", "display templates", "loot tables", "progression tracks", "pvp policy", "rarity tiers",
		"recipes", "resources", "ruleset formulas", "trust tiers", "wear slots",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sharedDefKinds =\n  %v\nwant\n  %v", got, want)
	}
	// DefaultCombat alone (no CombatProfiles slice) still reports "combat profiles".
	if got := sharedDefKinds([]content.Pack{{DefaultCombat: "melee"}}); !reflect.DeepEqual(got, []string{"combat profiles"}) {
		t.Fatalf("DefaultCombat alone: got %v, want [combat profiles]", got)
	}
	// A rooms/items-only pack has no shared defs.
	if got := sharedDefKinds([]content.Pack{reloadTestPack()}); got != nil {
		t.Fatalf("a rooms/items-only pack has no shared defs, got %v", got)
	}
}

// TestReloadSummaryRollingRebootNote pins the readout wording: a successful/partial reload whose content has
// shared defs appends the rolling-reboot reminder (naming them); a rooms-only reload, a hard rejection, and
// an infra failure do NOT (the last two applied nothing, so a reboot note would mislead).
func TestReloadSummaryRollingRebootNote(t *testing.T) {
	s := reloadSummary("demo", reloadOutcome{published: 5, sharedDefs: []string{"abilities", "pvp policy"}})
	if !strings.Contains(s, "rolling reboot") || !strings.Contains(s, "abilities, pvp policy") {
		t.Fatalf("expected the rolling-reboot reminder naming the shared defs; got:\n%s", s)
	}
	if s := reloadSummary("demo", reloadOutcome{checkOnly: true, sharedDefs: []string{"pvp policy"}}); !strings.Contains(s, "rolling reboot") {
		t.Fatalf("a --check with shared defs should still remind (pre-flight); got:\n%s", s)
	}
	if s := reloadSummary("demo", reloadOutcome{published: 2}); strings.Contains(s, "rolling reboot") {
		t.Fatalf("no shared defs -> no reminder; got:\n%s", s)
	}
	if s := reloadSummary("demo", reloadOutcome{rejected: []string{"bad def"}, sharedDefs: []string{"abilities"}}); strings.Contains(s, "rolling reboot") {
		t.Fatalf("a REJECTED reload must not append the reboot note (nothing was applied); got:\n%s", s)
	}
	if s := reloadSummary("demo", reloadOutcome{failed: true, published: 1, sharedDefs: []string{"abilities"}}); strings.Contains(s, "rolling reboot") {
		t.Fatalf("an infra-failed reload must not append the reboot note (result unreliable); got:\n%s", s)
	}
}

// TestReloadRepublishReportsSharedDefs is the integration seam: republish over a pack that defines shared defs
// populates out.sharedDefs (so the readout reminds), while a rooms/items-only pack reports none.
func TestReloadRepublishReportsSharedDefs(t *testing.T) {
	pack := reloadTestPack()
	pack.PvpLua = "return true"                            // a pack-global pvp policy (no hot-reload loop)
	pack.Formulas = map[string]string{"regen": "return 1"} // a ruleset-formula override (no hot-reload loop)
	src := content.NewMemSource()
	src.SetPack(pack)
	bus := contentbus.NewMemBus()
	defer func() { _ = bus.Close() }()
	s := newReloadShard(t, src, bus)

	out := s.reloader.republish(context.Background(), []string{"reloadtest"}, false)
	if out.failed || len(out.rejected) > 0 {
		t.Fatalf("republish over a healthy source should succeed: %+v", out)
	}
	if want := []string{"pvp policy", "ruleset formulas"}; !reflect.DeepEqual(out.sharedDefs, want) {
		t.Fatalf("republish out.sharedDefs = %v, want %v", out.sharedDefs, want)
	}

	// A --check dry run also reports shared defs (pre-flight heads-up), and publishes nothing.
	if out := s.reloader.republish(context.Background(), []string{"reloadtest"}, true); !out.checkOnly || len(out.sharedDefs) == 0 {
		t.Fatalf("--check should validate + report shared defs without publishing; got %+v", out)
	}

	// The baseline rooms/items pack reports no shared defs.
	src2 := content.NewMemSource()
	src2.SetPack(reloadTestPack())
	bus2 := contentbus.NewMemBus()
	defer func() { _ = bus2.Close() }()
	s2 := newReloadShard(t, src2, bus2)
	if out := s2.reloader.republish(context.Background(), []string{"reloadtest"}, false); len(out.sharedDefs) != 0 {
		t.Fatalf("a rooms/items-only pack should report no shared defs, got %v", out.sharedDefs)
	}
}

// TestReloadRejectedPackReportsNoSharedDefs pins the ORDERING guarantee (test-engineer): a pack that FAILS
// validation early-returns before sharedDefKinds is computed, so out.sharedDefs is never set on a rejection —
// the reminder can't leak onto a reload that applied nothing (a refactor hoisting the computation would break
// this and no other test would catch it).
func TestReloadRejectedPackReportsNoSharedDefs(t *testing.T) {
	pack := reloadTestPack()
	pack.PvpLua = "return true" // a shared def IS present…
	// …but the pack is broken: a reset spawning an undefined prototype is rejected by the #197 gate.
	pack.Zones[0].Resets = []content.ResetDTO{{Op: "spawn_mob", Proto: "rt:mob:ghost", Room: "rt:room:hall"}}
	src := content.NewMemSource()
	src.SetPack(pack)
	bus := contentbus.NewMemBus()
	defer func() { _ = bus.Close() }()
	s := newReloadShard(t, src, bus)

	out := s.reloader.republish(context.Background(), []string{"reloadtest"}, false)
	if len(out.rejected) == 0 {
		t.Fatalf("the broken pack should be rejected; got %+v", out)
	}
	if len(out.sharedDefs) != 0 {
		t.Fatalf("a REJECTED reload must not carry sharedDefs (nothing was applied); got %v", out.sharedDefs)
	}
}
