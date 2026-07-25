package world

// temp_hp_test.go exercises the #536 pre-vital absorption buffer (temp HP / wards): a pool that soaks
// damage BEFORE the vital (all types, spill the remainder), an INSTANCE-SET capacity (no derived max),
// and the take-higher / set-absolute `set_resource` write. The dN-of-size-1 trick makes rolls exact.

import (
	"math/rand"
	"testing"
)

// absorbZone extends combatZone with a temp_hp absorb buffer fronting the primary vital (hp), and a mob
// victim (a mob is ungated, isolating the absorb math from the PvP gate).
func absorbZone(t *testing.T) (*Zone, *Entity, *Entity) {
	z, s := combatZone(t)
	z.defs.res.register("temp_hp", &resourceDef{ref: "temp_hp", absorb: true}) // no maxAttr: instance-set; fronts "" => hp
	mob := combatMob(z, s.entity, "dummy", "", 100)
	return z, s.entity, mob
}

func harmCtx(z *Zone, actor, target *Entity) *effectCtx {
	return &effectCtx{z: z, actor: actor, source: actor, target: target, mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1))}
}

// TestAbsorbSoaksThenSpills proves temp HP absorbs first and the remainder hits the vital.
func TestAbsorbSoaksThenSpills(t *testing.T) {
	z, hero, mob := absorbZone(t)
	setResourceCurrent(mob, "temp_hp", 10)
	c := harmCtx(z, hero, mob)
	dealDamage(c, mob, 30, "slash", "") // 30 raw, no soak; temp_hp soaks 10, 20 spills to hp
	if got := resourceCurrent(mob, "hp"); got != 80 {
		t.Fatalf("hp = %d, want 80 (30 - 10 absorbed)", got)
	}
	if got := resourceCurrent(mob, "temp_hp"); got != 0 {
		t.Fatalf("temp_hp = %d, want 0 (fully spent)", got)
	}
}

// TestAbsorbFullyBlocks proves a buffer that covers the whole blow leaves the vital untouched and reports
// zero applied damage (a fully-absorbed blow is a no-op at the vital, like full mitigation).
func TestAbsorbFullyBlocks(t *testing.T) {
	z, hero, mob := absorbZone(t)
	setResourceCurrent(mob, "temp_hp", 50)
	c := harmCtx(z, hero, mob)
	applied := dealDamage(c, mob, 30, "slash", "")
	if got := resourceCurrent(mob, "hp"); got != 100 {
		t.Fatalf("hp = %d, want 100 (fully absorbed)", got)
	}
	if got := resourceCurrent(mob, "temp_hp"); got != 20 {
		t.Fatalf("temp_hp = %d, want 20 (50 - 30)", got)
	}
	if applied != 0 || c.lastDamage != 0 {
		t.Fatalf("applied=%d lastDamage=%d, want 0 (nothing reached the vital)", applied, c.lastDamage)
	}
}

// TestNoBufferIsNotImmunity proves an absent/empty buffer never confers immunity — the blow flows to the
// vital in full (the sharp edge the issue warns about: routing damage INTO temp_hp would make no-temp-HP
// read as immune; consulting it as a pre-step does not).
func TestNoBufferIsNotImmunity(t *testing.T) {
	z, hero, mob := absorbZone(t)
	// temp_hp never set (absent) -> reads 0 -> skipped.
	c := harmCtx(z, hero, mob)
	dealDamage(c, mob, 30, "slash", "")
	if got := resourceCurrent(mob, "hp"); got != 70 {
		t.Fatalf("hp = %d, want 70 (no buffer => full damage, NOT immunity)", got)
	}
}

// TestAbsorbOnlyFrontsItsPool proves a buffer fronting `hp` does NOT soak a blow routed to a different
// pool — the absorb is tied to the vital it fronts (5e temp HP soaks HP damage, not a separate track).
func TestAbsorbOnlyFrontsItsPool(t *testing.T) {
	z, s := combatZone(t)
	z.defs.res.register("temp_hp", &resourceDef{ref: "temp_hp", absorb: true, fronts: "hp"})
	z.defs.attr.register("max_stun", &attributeDef{ref: "max_stun", base: litNode{v: 100}})
	z.defs.res.register("stun", &resourceDef{ref: "stun", maxAttr: "max_stun"})
	mob := combatMob(z, s.entity, "dummy", "", 100)
	setResourceCurrent(mob, "temp_hp", 50)
	setResourceCurrent(mob, "stun", 50)

	c := harmCtx(z, s.entity, mob)
	dealDamage(c, mob, 20, "", "stun") // routed to `stun`, which temp_hp does not front
	if got := resourceCurrent(mob, "stun"); got != 30 {
		t.Fatalf("stun = %d, want 30 (buffer must not soak a pool it does not front)", got)
	}
	if got := resourceCurrent(mob, "temp_hp"); got != 50 {
		t.Fatalf("temp_hp = %d, want 50 (untouched by a stun blow)", got)
	}
}

// TestAbsorbInstanceSetCapacity proves the buffer's capacity is INSTANCE-SET, not a derived max: with no
// max_attr it holds whatever value is written, beyond any stat-derived cap.
func TestAbsorbInstanceSetCapacity(t *testing.T) {
	_, _, mob := absorbZone(t)
	setResourceCurrent(mob, "temp_hp", 25)
	if got := resourceCurrent(mob, "temp_hp"); got != 25 {
		t.Fatalf("temp_hp = %d, want 25 (instance-set, uncapped)", got)
	}
}

// TestSetResourceTakeHigher proves the take-higher write: a higher roll replaces, a lower does not (temp
// HP's non-stacking re-cast).
func TestSetResourceTakeHigher(t *testing.T) {
	z, _, mob := absorbZone(t)
	setResourceCurrent(mob, "temp_hp", 10)
	c := harmCtx(z, mob, mob) // self write (ungated)
	if err := opSetResource(c, &effectOp{kind: "set_resource", resource: "temp_hp", amount: 25, mode: "take_higher"}); err != nil {
		t.Fatalf("set_resource: %v", err)
	}
	if got := resourceCurrent(mob, "temp_hp"); got != 25 {
		t.Fatalf("temp_hp = %d, want 25 (25 > 10 replaces)", got)
	}
	// A lower re-cast is ignored.
	if err := opSetResource(c, &effectOp{kind: "set_resource", resource: "temp_hp", amount: 15, mode: "take_higher"}); err != nil {
		t.Fatalf("set_resource: %v", err)
	}
	if got := resourceCurrent(mob, "temp_hp"); got != 25 {
		t.Fatalf("temp_hp = %d, want 25 (15 < 25 does NOT lower)", got)
	}
}

// TestSetResourceAbsoluteAndRolled proves the default "set" mode overwrites, and the amount is rolled via
// the shared rollOpAmount (dice + bonus).
func TestSetResourceAbsoluteAndRolled(t *testing.T) {
	z, _, mob := absorbZone(t)
	setResourceCurrent(mob, "temp_hp", 25)
	c := harmCtx(z, mob, mob)
	// Absolute overwrite to 5.
	if err := opSetResource(c, &effectOp{kind: "set_resource", resource: "temp_hp", amount: 5}); err != nil {
		t.Fatalf("set_resource: %v", err)
	}
	if got := resourceCurrent(mob, "temp_hp"); got != 5 {
		t.Fatalf("temp_hp = %d, want 5 (absolute set)", got)
	}
	// Rolled: 2d1 (=2) + bonus 3 = 5, set.
	if err := opSetResource(c, &effectOp{kind: "set_resource", resource: "temp_hp", diceNum: 2, diceSize: 1, bonus: litNode{v: 3}}); err != nil {
		t.Fatalf("set_resource rolled: %v", err)
	}
	if got := resourceCurrent(mob, "temp_hp"); got != 5 {
		t.Fatalf("temp_hp = %d, want 5 (2d1 + 3)", got)
	}
}

// TestAbsorbFullBlockSkipsDamageBus proves the documented full-absorb semantics (#536 review): a
// FULLY-absorbed blow does NOT fire the OnDamageTaken BUS (it reached the vital as 0, like full
// mitigation), while a PARTIAL absorb fires the bus (with the spillover). This is what makes a passive
// thorns/aggro bus hook key on damage that actually reached the vital.
func TestAbsorbFullBlockSkipsDamageBus(t *testing.T) {
	newZone := func() (*Zone, *Entity, *Entity) {
		z, s := combatZone(t)
		z.defs.res.register("temp_hp", &resourceDef{ref: "temp_hp", absorb: true})
		// A bus hook on the victim that counts OnDamageTaken fires into a `marker` pool.
		z.defs.res.register("marker", &resourceDef{ref: "marker"})
		z.defs.res.register("thorns", &resourceDef{
			ref: "thorns",
			onEvent: map[eventKind][]effectOp{
				evOnDamageTaken: {{kind: "modify_resource", resource: "marker", amount: 1, tgt: "self"}},
			},
		})
		mob := combatMob(z, s.entity, "dummy", "", 100)
		setResourceCurrent(mob, "thorns", 1) // mob HAS the bus hook
		return z, s.entity, mob
	}

	// Full absorb: temp_hp 50 covers a 30 blow -> the bus must NOT fire.
	z, hero, mob := newZone()
	setResourceCurrent(mob, "temp_hp", 50)
	dealDamage(harmCtx(z, hero, mob), mob, 30, "slash", "")
	if got := resourceCurrent(mob, "marker"); got != 0 {
		t.Fatalf("fully-absorbed blow fired the OnDamageTaken bus (marker=%d, want 0)", got)
	}

	// Partial absorb: temp_hp 10, a 30 blow spills 20 -> the bus fires once.
	z2, hero2, mob2 := newZone()
	setResourceCurrent(mob2, "temp_hp", 10)
	dealDamage(harmCtx(z2, hero2, mob2), mob2, 30, "slash", "")
	if got := resourceCurrent(mob2, "marker"); got != 1 {
		t.Fatalf("partially-absorbed blow did not fire the OnDamageTaken bus (marker=%d, want 1)", got)
	}
}

// TestSetResourceCrossPlayerGated proves a set_resource on a non-consenting player is gated (no write) —
// the same conservative any-cross-player-write rule as modify_resource.
func TestSetResourceCrossPlayerGated(t *testing.T) {
	z, _ := combatZone(t)
	z.defs.res.register("temp_hp", &resourceDef{ref: "temp_hp", absorb: true})
	atk := makeRoomPlayer(z, "Attacker")
	vic := makePlayerTargetInRoom(z, atk.entity, "Victim")
	if pvpAllowed(atk.entity, vic.entity) {
		t.Fatal("precondition: expected no PvP consent")
	}
	c := harmCtx(z, atk.entity, vic.entity)
	if err := opSetResource(c, &effectOp{kind: "set_resource", resource: "temp_hp", amount: 50, mode: "set"}); err != nil {
		t.Fatalf("set_resource: %v", err)
	}
	if got := resourceCurrent(vic.entity, "temp_hp"); got != 0 {
		t.Fatalf("victim temp_hp = %d, want 0 (cross-player write gated)", got)
	}
}
