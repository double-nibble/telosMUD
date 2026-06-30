package world

import "testing"

// chargen_test.go — Phase 14.8: the first-spawn application of a content chargen RESULT. Point-buy SETS the
// attribute bases; the chosen race/class bundles then ADD on top (so a racial +con stacks on the bought base).

func TestApplyPendingChargenOnFirstSpawn(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	dst := &session{character: "Aria"}
	z.newPlayerEntity(dst, "Aria")

	// A new character's recorded chargen: dwarf (+2 con, stout) + fighter (+3 str, martial, hero track),
	// with a 15/8/13 point-buy.
	snap := CharSnapshot{
		Name: "Aria", PID: "pid-aria",
		PendingChargen: &ChargenResult{
			Bundles: []string{"dwarf", "fighter"},
			Attrs:   map[string]float64{"strength": 15, "intellect": 8, "constitution": 13},
		},
	}
	loadCharacter(z, dst, snap)
	applyPendingChargen(z, dst, snap.PendingChargen)
	e := dst.entity

	// strength: 15 (point-buy) + 3 (fighter) = 18; constitution: 13 + 2 (dwarf) = 15; intellect: 8, no mod.
	if got := attr(e, "strength"); got != 18 {
		t.Fatalf("strength = %v, want 18 (15 point-buy + 3 fighter)", got)
	}
	if got := attr(e, "constitution"); got != 15 {
		t.Fatalf("constitution = %v, want 15 (13 point-buy + 2 dwarf)", got)
	}
	if got := attr(e, "intellect"); got != 8 {
		t.Fatalf("intellect = %v, want 8 (point-buy, no racial mod)", got)
	}
	if !hasFlag(e, "martial") || !hasFlag(e, "stout") {
		t.Fatal("chargen did not set the fighter (martial) + dwarf (stout) flags")
	}
	if !hasTrack(e, "hero_advancement") {
		t.Fatal("chargen did not grant the fighter's hero_advancement track")
	}
}

// TestApplyPendingChargenUnknownBundleSkipped proves an unknown bundle ref is skipped (logged), not fatal —
// the rest of the build still applies.
func TestApplyPendingChargenUnknownBundleSkipped(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	dst := &session{character: "Nix"}
	z.newPlayerEntity(dst, "Nix")
	applyPendingChargen(z, dst, &ChargenResult{
		Bundles: []string{"nonexistent", "elf"},
		Attrs:   map[string]float64{"intellect": 12},
	})
	e := dst.entity
	// elf still applied (+2 int on the 12 point-buy base) despite the bogus ref before it.
	if got := attr(e, "intellect"); got != 14 {
		t.Fatalf("intellect = %v, want 14 (12 point-buy + 2 elf); the unknown bundle must be skipped, not fatal", got)
	}
	if !hasFlag(e, "darkvision") {
		t.Fatal("elf bundle did not apply after the unknown bundle was skipped")
	}
}

// TestApplyPendingChargenNilNoop proves a nil result is a harmless no-op (every returning character).
func TestApplyPendingChargenNilNoop(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	dst := &session{character: "Plain"}
	z.newPlayerEntity(dst, "Plain")
	str0 := attr(dst.entity, "strength")
	applyPendingChargen(z, dst, nil)
	if got := attr(dst.entity, "strength"); got != str0 {
		t.Fatalf("nil chargen mutated the entity: strength %v -> %v", str0, got)
	}
}
