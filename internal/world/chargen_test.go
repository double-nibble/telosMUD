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

// TestChargenBuildSurvivesReloadNoReapply is the Phase-14 capstone seam: a chargen-built character, once
// dumped + reloaded (a relogin / restart), keeps its built stats EXACTLY — the dumped snapshot carries no
// marker, so the additive racial bundle mods are not applied a second time (no doubling).
func TestChargenBuildSurvivesReloadNoReapply(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	// First spawn: apply the recorded chargen (dwarf +2 con / stout, fighter +3 str / martial / hero track).
	src := &session{character: "Thrain"}
	z.newPlayerEntity(src, "Thrain")
	cg := &ChargenResult{Bundles: []string{"dwarf", "fighter"}, Attrs: map[string]float64{"strength": 15, "constitution": 13, "intellect": 8}}
	loadCharacter(z, src, CharSnapshot{Name: "Thrain", PID: "pid-thrain", PendingChargen: cg})
	applyPendingChargen(z, src, cg)
	wantStr := attr(src.entity, "strength")     // 18
	wantCon := attr(src.entity, "constitution") // 15

	// Dump the built character (what a save persists). It must carry NO chargen marker.
	snap := dumpCharacter(src)
	if snap.PendingChargen != nil {
		t.Fatal("a dumped (already-built) snapshot must carry no chargen marker")
	}

	// Relogin/restart: reload into a fresh entity. With no marker the build is NOT re-applied.
	dst := &session{character: "Thrain"}
	z.newPlayerEntity(dst, "Thrain")
	loadCharacter(z, dst, snap)
	if snap.PendingChargen != nil {
		applyPendingChargen(z, dst, snap.PendingChargen) // unreachable; the guard above already failed the test
	}
	if got := attr(dst.entity, "strength"); got != wantStr {
		t.Fatalf("after reload strength = %v, want %v (built stats persist, never re-applied/doubled)", got, wantStr)
	}
	if got := attr(dst.entity, "constitution"); got != wantCon {
		t.Fatalf("after reload constitution = %v, want %v", got, wantCon)
	}
	if !hasFlag(dst.entity, "martial") || !hasFlag(dst.entity, "stout") {
		t.Fatal("built flags did not survive the reload")
	}
	if !hasTrack(dst.entity, "hero_advancement") {
		t.Fatal("built track did not survive the reload")
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
