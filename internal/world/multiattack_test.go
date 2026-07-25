package world

// multiattack_test.go exercises heterogeneous multiattack (#543): a per-attacker attack ROUTINE (bite +
// 2 claws) the swing loop cycles, each attack with its own dice/type, replacing the attacks-count ×
// one-weapon loop. Size-1 dice make each attack's damage exact.

import "testing"

// TestMultiattackRoutineCyclesEntries proves a routine resolves sum(count) swings, each with ITS dice: a
// bite (2d1 = 2) plus two claws (3d1 = 3 each) = 8 damage in one round.
func TestMultiattackRoutineCyclesEntries(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("beast", &combatProfile{
		toHit: &checkSpec{label: "Attack", dice: mustDiceT("1d1"), bands: []checkBand{{label: "hit"}}}, // always hit
		routine: []attackEntry{
			{count: 1, diceNum: 2, diceSize: 1, dmgType: "slash"}, // bite: 2
			{count: 2, diceNum: 3, diceSize: 1, dmgType: "slash"}, // 2 claws: 3 each
		},
	})
	attacker := combatMob(z, s.entity, "beast", "beast", 100)
	dummy := combatMob(z, s.entity, "dummy", "", 100)
	z.startFight(attacker, dummy)

	z.resolveSwings(attacker, 0, newBudget())
	// bite 2 + claw 3 + claw 3 = 8. hp 100 -> 92.
	if got := resourceCurrent(dummy, "hp"); got != 92 {
		t.Fatalf("dummy hp = %d, want 92 (bite 2 + 2 claws 3 = 8)", got)
	}
}

// TestMultiattackCapsRunaway proves a runaway routine (count > maxSwingsPerRound) is capped so it can't
// spin the zone goroutine.
func TestMultiattackCapsRunaway(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("swarm", &combatProfile{
		toHit:   &checkSpec{label: "Attack", dice: mustDiceT("1d1"), bands: []checkBand{{label: "hit"}}},
		routine: []attackEntry{{count: 1000, diceNum: 1, diceSize: 1, dmgType: "slash"}}, // 1 dmg each, absurd count
	})
	attacker := combatMob(z, s.entity, "swarm", "swarm", 100)
	dummy := combatMob(z, s.entity, "dummy", "", 100)
	z.startFight(attacker, dummy)

	z.resolveSwings(attacker, 0, newBudget())
	// capped at maxSwingsPerRound (50) swings x 1 dmg = 50. hp 100 -> 50, NOT 0.
	if got := resourceCurrent(dummy, "hp"); got != 100-maxSwingsPerRound {
		t.Fatalf("dummy hp = %d, want %d (routine capped at maxSwingsPerRound)", got, 100-maxSwingsPerRound)
	}
}

// TestBuildSwingDamageOpEntry proves the damage op uses the ENTRY's dice/type when present, and falls back
// to the wielded weapon when nil.
func TestBuildSwingDamageOpEntry(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("beast", &combatProfile{
		routine: []attackEntry{{count: 1, diceNum: 4, diceSize: 6, dmgType: "fire"}},
	})
	attacker := combatMob(z, s.entity, "beast", "beast", 100)
	equipWeapon(attacker, &Weapon{diceNum: 1, diceSize: 8, damageType: "slash"})

	// With an entry: the entry's dice/type win over the wielded weapon.
	entry := &attackEntry{count: 1, diceNum: 2, diceSize: 10, dmgType: "cold"}
	op := buildSwingDamageOpEntry(attacker, entry)
	if op.diceNum != 2 || op.diceSize != 10 || op.dmgType != "cold" {
		t.Fatalf("entry op = %dd%d %s, want 2d10 cold (entry wins over weapon)", op.diceNum, op.diceSize, op.dmgType)
	}
	// Nil entry: the wielded weapon.
	op = buildSwingDamageOpEntry(attacker, nil)
	if op.diceNum != 1 || op.diceSize != 8 || op.dmgType != "slash" {
		t.Fatalf("nil-entry op = %dd%d %s, want 1d8 slash (wielded weapon)", op.diceNum, op.diceSize, op.dmgType)
	}
}

// TestMultiattackKillStopsRound proves a routine swing that KILLS the target stops the rest of the round
// (the per-swing target re-read) — the second attack does not fire at a dead/disengaged foe.
func TestMultiattackKillStopsRound(t *testing.T) {
	z, s := combatZone(t)
	// The attacker counts its OnHit fires into a marker pool.
	z.defs.res.register("marker", &resourceDef{ref: "marker"})
	z.defs.res.register("hits", &resourceDef{
		ref: "hits",
		onEvent: map[eventKind][]effectOp{
			evOnHit: {{kind: "modify_resource", resource: "marker", amount: 1, tgt: "self"}},
		},
	})
	z.defs.combat.register("beast", &combatProfile{
		toHit: &checkSpec{label: "Attack", dice: mustDiceT("1d1"), bands: []checkBand{{label: "hit"}}},
		routine: []attackEntry{
			{count: 1, diceNum: 50, diceSize: 1, dmgType: "slash"}, // 50 dmg: lethal to a 5-hp target
			{count: 1, diceNum: 50, diceSize: 1, dmgType: "slash"}, // should NOT fire
		},
	})
	attacker := combatMob(z, s.entity, "beast", "beast", 100)
	setResourceCurrent(attacker, "hits", 1) // attacker HAS the OnHit hook
	dummy := combatMob(z, s.entity, "dummy", "", 5)
	z.startFight(attacker, dummy)

	z.resolveSwings(attacker, 0, newBudget())
	if position(dummy) != posDead {
		t.Fatalf("dummy should be dead from the first routine attack, position=%v", position(dummy))
	}
	if got := resourceCurrent(attacker, "marker"); got != 1 {
		t.Fatalf("OnHit fired %d times, want 1 (the fatal first attack stops the round; the 2nd must not fire)", got)
	}
}

// TestMultiattackBrokenProfileRefuses proves a BROKEN profile with a routine resolves NO swings (both the
// routine gate and the per-swing refusal), so a content defect deals zero rather than auto-hitting.
func TestMultiattackBrokenProfileRefuses(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("beast", &combatProfile{
		broken:  true, // a sub-spec failed to parse at build
		routine: []attackEntry{{count: 3, diceNum: 9, diceSize: 1, dmgType: "slash"}},
	})
	attacker := combatMob(z, s.entity, "beast", "beast", 100)
	dummy := combatMob(z, s.entity, "dummy", "", 100)
	z.startFight(attacker, dummy)

	z.resolveSwings(attacker, 0, newBudget())
	if got := resourceCurrent(dummy, "hp"); got != 100 {
		t.Fatalf("dummy hp = %d, want 100 (a broken profile resolves no swings)", got)
	}
}

// TestMultiattackPerEntryBonus proves each entry's own bonus is used, falling back to the profile's
// damage_bonus when the entry has none.
func TestMultiattackPerEntryBonus(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("beast", &combatProfile{
		toHit:       &checkSpec{label: "Attack", dice: mustDiceT("1d1"), bands: []checkBand{{label: "hit"}}},
		damageBonus: litNode{v: 10}, // profile-level bonus
		routine: []attackEntry{
			{count: 1, diceNum: 2, diceSize: 1, dmgType: "slash", bonus: litNode{v: 5}}, // 2 + 5 = 7 (own bonus)
			{count: 1, diceNum: 2, diceSize: 1, dmgType: "slash"},                       // 2 + 10 = 12 (profile fallback)
		},
	})
	attacker := combatMob(z, s.entity, "beast", "beast", 100)
	dummy := combatMob(z, s.entity, "dummy", "", 100)
	z.startFight(attacker, dummy)

	z.resolveSwings(attacker, 0, newBudget())
	// 7 + 12 = 19. hp 100 -> 81.
	if got := resourceCurrent(dummy, "hp"); got != 81 {
		t.Fatalf("dummy hp = %d, want 81 (entry-bonus 7 + profile-fallback 12 = 19)", got)
	}
}

// TestNoRoutineUsesWeaponLoop proves the homogeneous fallback is unchanged: a profile with NO routine
// resolves `attacks` swings with the wielded weapon.
func TestNoRoutineUsesWeaponLoop(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("warrior", autoHitProfile(nil))
	attacker := combatMob(z, s.entity, "warrior", "warrior", 100)
	setAttrBase(attacker, "attacks", 2)
	equipWeapon(attacker, &Weapon{diceNum: 5, diceSize: 1, damageType: "slash"}) // 5 each
	dummy := combatMob(z, s.entity, "dummy", "", 100)
	z.startFight(attacker, dummy)

	z.resolveSwings(attacker, 0, newBudget())
	// 2 weapon swings x 5 = 10. hp 100 -> 90.
	if got := resourceCurrent(dummy, "hp"); got != 90 {
		t.Fatalf("dummy hp = %d, want 90 (2 weapon swings of 5; no routine)", got)
	}
}
