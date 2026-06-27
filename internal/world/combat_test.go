package world

import (
	"math/rand"
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// combat_test.go exercises Phase 6.3a: the round driver (PULSE_VIOLENCE + the Fighting state), the
// swing pipeline (gates -> to-hit -> avoidance ladder -> formula damage + crit -> soak -> apply ->
// OnHit), [G-B] soak-by-type, [G-H] $swing.index, [G8] cooldown completion, and the END-TO-END "kill a
// mob through the pipeline" milestone driven entirely by CONTENT profiles. Determinism: the d1/size-1
// trick (a "1d1" die always rolls 1, face 1) lets a band edge select an EXACT outcome; a seeded
// z.testCombatRng makes a multi-roll fight reproducible.

// combatZone builds a bare zone with the combat attributes a profile reads. It returns the zone and a
// player session (in z.players so resolve-by-id works). Profiles + mobs are added per-test so each
// controls its own to-hit/avoidance shape.
func combatZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	z := newZone("test")
	reg := func(ref string, base float64) {
		z.defs.attr.register(ref, &attributeDef{ref: ref, base: litNode{v: base}})
	}
	reg("strength", 14)
	reg("strength_bonus", 2)
	reg("accuracy", 0)
	reg("damroll", 0)
	reg("attacks", 1)
	reg("crit_mult", 2)
	reg("evasion", 0)
	reg("dodge", 0)
	reg("parry", 0)
	reg("block", 0)
	reg("soak_slash", 0)
	reg("combat_order", 0)
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	z.defs.dmg.register("slash", &damageTypeDef{ref: "slash", resist: map[string]float64{}})

	s := makeRoomPlayer(z, "Hero")
	return z, s
}

// autoHitProfile is a to-hit that ALWAYS lands (a single default "hit" band, no dice dependence) plus a
// content damage bonus. avoidance is supplied by the caller (nil => none).
func autoHitProfile(damageBonus formulaNode, avoidance ...*checkSpec) *combatProfile {
	return &combatProfile{
		toHit:       &checkSpec{label: "Attack", dice: mustDiceT("1d1"), bands: []checkBand{{label: "hit"}}},
		avoidance:   avoidance,
		damageBonus: damageBonus,
	}
}

// newBudget returns a fresh event budget pointer for a direct resolveSwing/resolveSwings test call
// (the round driver allocates one per round; a direct test call supplies its own).
func newBudget() *int { b := maxEventHandlers; return &b }

func mustDiceT(s string) diceSpec {
	d, err := parseDiceSpec(s)
	if err != nil {
		panic(err)
	}
	return d
}

// equipWeapon gives e a wielded Weapon (the swing's dice). The entity must have a Wearer.
func equipWeapon(e *Entity, w *Weapon) {
	item := e.zone.newEntity(ProtoRef("test:weapon"))
	Add(item, w)
	wr := actorWearer(e)
	wr.worn[WearLocWield] = item
}

// combatMob builds a non-player living target in actor's room with the given combat ref + hp.
func combatMob(z *Zone, actor *Entity, name, combatRef string, hp int) *Entity {
	e := z.newEntity(ProtoRef("test:mob"))
	e.short = name
	e.setKeywords([]string{name})
	Add(e, &Living{combatRef: combatRef})
	Move(e, actor.location)
	setResourceCurrent(e, "hp", hp)
	return e
}

func drainCombat(s *session) []string {
	var out []string
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil {
				out = append(out, o.GetMarkup())
			}
		default:
			return out
		}
	}
}

func contains(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

// --- The pipeline stages, each observable ------------------------------------------------------

// TestSwingHitFormulaDamageWithSoak proves: an auto-hit to-hit, [G-A] formula damage (weapon dice +
// str_bonus + damroll), and [G-B] soak-by-type reducing it. 1d6-fixed-to-6 weapon + str_bonus 2 +
// damroll 1 = 9 raw; soak_slash 3 -> 6 applied.
func TestSwingHitFormulaDamageWithSoak(t *testing.T) {
	z, s := combatZone(t)
	// A profile that auto-hits and adds str_bonus + damroll. damroll set to 1 via a base override.
	setAttrBase(s.entity, "damroll", 1)
	z.defs.combat.register("attacker", autoHitProfile(
		opFormula("+", an("$actor.strength_bonus"), an("$actor.damroll")),
	))
	s.entity.living.combatRef = "attacker"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"}) // 6d1 = always 6

	mob := combatMob(z, s.entity, "dummy", "", 100) // defender: no profile (no avoidance), soak 3
	z.defs.attr.register("soak_slash", &attributeDef{ref: "soak_slash", base: litNode{v: 3}})

	c := &effectCtx{z: z, actor: s.entity, source: s.entity, target: mob, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(1))}
	z.resolveSwing(s.entity, mob, 0, c.rng, newBudget())

	// 6 (weapon) + 2 (str_bonus) + 1 (damroll) = 9 raw; soak_slash 3 -> 6 applied. hp 100 -> 94.
	if got := resourceCurrent(mob, "hp"); got != 94 {
		t.Fatalf("hp after swing = %d, want 94 (9 raw - 3 soak)", got)
	}
	if out := drainCombat(s); !contains(out, "You hit") {
		t.Fatalf("expected a hit message, got %v", out)
	}
}

// TestSwingMiss proves a to-hit "miss" band ends the swing with no damage + a miss message.
func TestSwingMiss(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("misser", &combatProfile{
		toHit: &checkSpec{label: "Attack", dice: mustDiceT("1d1"), bands: []checkBand{{label: "miss"}}},
	})
	s.entity.living.combatRef = "misser"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "dummy", "", 100)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(mob, "hp"); got != 100 {
		t.Fatalf("hp after miss = %d, want 100 (no damage)", got)
	}
	if out := drainCombat(s); !contains(out, "You miss") {
		t.Fatalf("expected a miss message, got %v", out)
	}
}

// TestSwingAvoidanceLadder proves [G-F]: each avoidance rung negates the swing observably, AND a
// zeroed-skill rung auto-fails (gear-gating with no engine predicate, [G-C]). The to-hit auto-hits; the
// defender's ladder is dodge(0)->parry(100)->block(0): dodge auto-fails, parry succeeds -> "parry".
func TestSwingAvoidanceLadder(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("attacker", autoHitProfile(nil))
	s.entity.living.combatRef = "attacker"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})

	// Defender: dodge derives to 0 (auto-fail on 1d100 roll-under), parry to 100 (always succeeds).
	z.defs.attr.register("dodge", &attributeDef{ref: "dodge", base: litNode{v: 0}})
	z.defs.attr.register("parry", &attributeDef{ref: "parry", base: litNode{v: 100}})
	rollUnder := func(label, attr string) *checkSpec {
		return &checkSpec{label: label, dice: mustDiceT("1d100"), bands: []checkBand{
			{max: an("$actor." + attr), label: strings.ToLower(label)},
			{label: "fail"},
		}}
	}
	z.defs.combat.register("defender", &combatProfile{
		avoidance: []*checkSpec{rollUnder("Dodge", "dodge"), rollUnder("Parry", "parry"), rollUnder("Block", "block")},
	})
	mob := combatMob(z, s.entity, "dummy", "defender", 100)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(mob, "hp"); got != 100 {
		t.Fatalf("hp after parry = %d, want 100 (swing negated)", got)
	}
	if out := drainCombat(s); !contains(out, "parr") {
		t.Fatalf("expected a parry message, got %v", out)
	}
}

// TestSwingCritScalesDamage proves a "crit" to-hit band multiplies damage via the crit_mult attribute
// ([G-A] crit consequence). 6d1 weapon = 6 raw; crit_mult 2 -> 12 applied (no soak).
func TestSwingCritScalesDamage(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("critter", &combatProfile{
		toHit: &checkSpec{label: "Attack", dice: mustDiceT("1d1"), bands: []checkBand{{label: "crit"}}},
	})
	s.entity.living.combatRef = "critter"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "dummy", "", 100)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(mob, "hp"); got != 88 {
		t.Fatalf("hp after crit = %d, want 88 (6 dmg x2 crit_mult)", got)
	}
	if out := drainCombat(s); !contains(out, "CRITICALLY") {
		t.Fatalf("expected a crit message, got %v", out)
	}
}

// TestSwingIndexFeedsToHitFormula proves [G-H]: the per-swing index reaches a to-hit bonus formula as
// $swing.index. A bonus of -100*$swing.index makes swing 0 hit (margin>=0) and swing 1 miss
// (margin<0). We drive two swings and assert only the first damages.
func TestSwingIndexFeedsToHitFormula(t *testing.T) {
	z, s := combatZone(t)
	// to-hit: 1d1 (rolls 1) + (-100 * $swing.index) vs evasion 0. swing0: total 1, margin 1 -> hit;
	// swing1: total 1-100=-99, margin -99 -> miss.
	z.defs.combat.register("iterative", &combatProfile{
		toHit: &checkSpec{
			label: "Attack", dice: mustDiceT("1d1"),
			bonus: opFormula("*", litNode{v: -100}, an("$swing.index")),
			vs:    checkVs{dc: litNode{v: 0}},
			bands: []checkBand{{marginMin: litNode{v: 0}, label: "hit"}, {label: "miss"}},
		},
	})
	s.entity.living.combatRef = "iterative"
	setAttrBase(s.entity, "attacks", 2)
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "dummy", "", 100)
	s.entity.living.fighting = mob

	z.resolveSwings(s.entity, 0, newBudget())
	// Only swing 0 lands: 6 damage. swing 1 misses. hp 100 -> 94.
	if got := resourceCurrent(mob, "hp"); got != 94 {
		t.Fatalf("hp after 2 iterative swings = %d, want 94 (only swing 0 hits)", got)
	}
}

// TestOnHitFiresContentHandler proves the swing fires OnHit with the attacker as subject (a content
// handler on a resource builds rage). The handler modifies a "rage" resource the attacker has.
func TestOnHitFiresContentHandler(t *testing.T) {
	z, s := combatZone(t)
	// A rage resource the player HAS (max>0), with an OnHit handler that adds 5 rage.
	z.defs.attr.register("max_rage", &attributeDef{ref: "max_rage", base: litNode{v: 100}})
	z.defs.res.register("rage", &resourceDef{ref: "rage", maxAttr: "max_rage",
		onEvent: map[eventKind][]effectOp{
			evOnHit: {{kind: "modify_resource", resource: "rage", amount: 5, tgt: "self"}},
		}})
	z.defs.combat.register("attacker", autoHitProfile(nil))
	s.entity.living.combatRef = "attacker"
	setResourceCurrent(s.entity, "rage", 0)
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "dummy", "", 100)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(s.entity, "rage"); got != 5 {
		t.Fatalf("rage after OnHit = %d, want 5 (OnHit handler built rage)", got)
	}
}

// --- The PvP harm gate, exercised through the swing pipeline (security regression) -------------

// combatPlayerPair builds two players in the SAME room with the given combat ref, so a swing/kill can
// be driven attacker->victim. Returns (attacker session, victim entity).
func combatPlayerPair(z *Zone, combatRef string) (*session, *Entity) {
	atk := makeRoomPlayer(z, "Attacker")
	atk.entity.living.combatRef = combatRef
	vic := makePlayerTargetInRoom(z, atk.entity, "Victim")
	setResourceCurrent(vic.entity, "hp", 100)
	return atk, vic.entity
}

// TestSwingAgainstNonConsentingPlayerGated proves the swing's damage funnels guardHarmful: with no PvP
// consent, the victim takes NO damage even though the to-hit auto-hits, and OnHit/OnDamageTaken still
// fire with mag 0 (the swing landed mechanically; the harm was the part the gate refused).
func TestSwingAgainstNonConsentingPlayerGated(t *testing.T) {
	z, _ := combatZone(t)
	z.defs.combat.register("attacker", autoHitProfile(nil))
	atk, victim := combatPlayerPair(z, "attacker")
	equipWeapon(atk.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})

	// Sanity: the PvP gate forbids harm (no consent flags set).
	if pvpAllowed(atk.entity, victim) {
		t.Fatal("test precondition: expected no PvP consent")
	}

	z.resolveSwing(atk.entity, victim, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(victim, "hp"); got != 100 {
		t.Fatalf("non-consenting victim hp = %d, want 100 (the gate refused the swing's harm)", got)
	}
	// The swing still emitted a "hit" message (it landed mechanically) — the GATE is at the damage apply,
	// not the to-hit. The important security property is the hp is untouched.
	out := drainCombat(atk)
	if !contains(out, "You hit") {
		t.Fatalf("expected the swing to resolve mechanically, got %v", out)
	}
}

// TestSwingSafeRoomReGatesPerSwing proves the gate re-evaluates PER SWING: a fight that lands damage in
// a normal room stops landing the moment the victim's room is flagged "safe" (the absolute veto). Both
// players consent so the ONLY thing changing is the safe flag — proving per-swing re-gating, not a
// cached engage-time decision.
func TestSwingSafeRoomReGatesPerSwing(t *testing.T) {
	z, _ := combatZone(t)
	z.defs.combat.register("attacker", autoHitProfile(nil))
	atk, victim := combatPlayerPair(z, "attacker")
	equipWeapon(atk.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	// Both consent so harm is allowed in a normal room.
	setFlag(atk.entity, flagPvP, true)
	setFlag(victim, flagPvP, true)

	z.resolveSwing(atk.entity, victim, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(victim, "hp"); got != 94 {
		t.Fatalf("consenting victim hp = %d, want 94 (harm allowed in a normal room)", got)
	}

	// Flag the victim's room safe mid-fight: the next swing must no-op (per-swing re-gate).
	room := victim.location
	if room.room.namedFlags == nil {
		room.room.namedFlags = map[string]bool{}
	}
	room.room.namedFlags[flagSafe] = true

	z.resolveSwing(atk.entity, victim, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(victim, "hp"); got != 94 {
		t.Fatalf("victim hp = %d after a swing in a SAFE room, want 94 (gate re-evaluated, harm refused)", got)
	}
}

// TestKillNonConsentingPlayerRefused proves cmdKill refuses to ENGAGE a non-consenting player (the
// player-facing layer above the in-swing gate): "cannot harm", and no Fighting state is set.
func TestKillNonConsentingPlayerRefused(t *testing.T) {
	z, _ := combatZone(t)
	atk, victim := combatPlayerPair(z, "")
	ctx := &Context{z: z, s: atk, Actor: atk.entity, arg: "Victim"}
	if err := cmdKill(ctx); err != nil {
		t.Fatalf("cmdKill: %v", err)
	}
	if position(atk.entity) == posFighting || atk.entity.living.fighting != nil {
		t.Fatalf("kill engaged a non-consenting player (should be refused)")
	}
	if position(victim) == posFighting {
		t.Fatalf("kill put the non-consenting victim into combat")
	}
	if out := drainCombat(atk); !contains(out, "cannot harm") {
		t.Fatalf("expected a 'cannot harm' refusal, got %v", out)
	}
}

// --- The round driver + kill/flee --------------------------------------------------------------

// TestKillEntersCombatAndRoundDriverSwings is the END-TO-END milestone: `kill` engages a mob, the round
// driver swings on PULSE_VIOLENCE, and the mob takes formula damage across rounds — all from content
// profiles registered like the demo pack. Driven via the pulse so the real round timing is exercised.
func TestKillEntersCombatAndRoundDriverSwings(t *testing.T) {
	z, s := combatZone(t)
	z.testCombatRng = rand.New(rand.NewSource(1))
	z.defs.combat.register("melee", autoHitProfile(
		an("$actor.strength_bonus"), // +2 damage
	))
	s.entity.living.combatRef = "melee"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"}) // 6 + 2 = 8/swing
	mob := combatMob(z, s.entity, "goblin", "", 50)

	// kill the goblin (the command path).
	ctx := &Context{z: z, s: s, Actor: s.entity, arg: "goblin"}
	if err := cmdKill(ctx); err != nil {
		t.Fatalf("cmdKill: %v", err)
	}
	if position(s.entity) != posFighting || s.entity.living.fighting != mob {
		t.Fatalf("kill did not put the hero into combat")
	}
	if position(mob) != posFighting || mob.living.fighting != s.entity {
		t.Fatalf("kill did not make the goblin retaliate")
	}
	if z.combatPulse == nil {
		t.Fatalf("kill did not arm the round driver")
	}

	// Advance the pulse to the first violence round. The goblin has no weapon/profile -> auto-hits the
	// hero for 0 (no damage bonus, no weapon) — harmless; we only assert the hero's damage to the mob.
	startHP := resourceCurrent(mob, "hp")
	for i := uint64(0); i < PULSE_VIOLENCE; i++ {
		z.pulses.tick()
	}
	afterOne := resourceCurrent(mob, "hp")
	if afterOne != startHP-8 {
		t.Fatalf("after round 1 goblin hp = %d, want %d (8 damage)", afterOne, startHP-8)
	}
	// A second round deals another 8.
	for i := uint64(0); i < PULSE_VIOLENCE; i++ {
		z.pulses.tick()
	}
	if got := resourceCurrent(mob, "hp"); got != startHP-16 {
		t.Fatalf("after round 2 goblin hp = %d, want %d", got, startHP-16)
	}
}

// TestFleeLeavesCombat proves flee drops the actor out of the Fighting state.
func TestFleeLeavesCombat(t *testing.T) {
	z, s := combatZone(t)
	mob := combatMob(z, s.entity, "goblin", "", 50)
	z.startFight(s.entity, mob)
	if position(s.entity) != posFighting {
		t.Fatalf("startFight did not engage")
	}
	ctx := &Context{z: z, s: s, Actor: s.entity}
	if err := cmdFlee(ctx); err != nil {
		t.Fatalf("cmdFlee: %v", err)
	}
	if position(s.entity) == posFighting || s.entity.living.fighting != nil {
		t.Fatalf("flee did not leave combat")
	}
}

// --- The movement gate the round driver depends on (MUST-FIX regression) -----------------------

// TestCannotMoveWhileFighting proves move() REFUSES to walk while posFighting — the ENFORCED invariant
// the driver relies on (no fighting pointer crosses a zone). After flee, the same move is allowed.
func TestCannotMoveWhileFighting(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(s, "Hero")
	Move(s.entity, z.rooms[z.startRoom])
	z.players["Hero"] = s
	mob := combatMobIn(z, s.entity, "goblin")
	z.startFight(s.entity, mob)

	// A local move (temple -> market exists in the demo) must be refused while fighting.
	if z.move(s, "north") {
		t.Fatal("move while fighting released ownership (should have been refused)")
	}
	if s.entity.location != z.rooms[z.startRoom] {
		t.Fatal("move while fighting actually relocated the player")
	}
	out := drainCombat(s)
	if !contains(out, "can't leave while fighting") {
		t.Fatalf("expected the combat-exclusion message, got %v", out)
	}
	// flee, then the move is allowed.
	z.stopFight(s.entity)
	if z.move(s, "north") {
		// a local move returns false (no ownership release); it should SUCCEED (return false) and relocate.
	}
	if s.entity.location == z.rooms[z.startRoom] {
		t.Fatal("after flee the move should have succeeded")
	}
}

// TestNoFightingPointerCrossesZoneOnTransfer proves the belt-and-suspenders: even if combat state
// somehow reached the transfer path, transferOut+transferIn leave NO fighting pointer / posFighting on
// the moved entity, and the opponent left in the source zone is disengaged (not orphaned posFighting).
func TestNoFightingPointerCrossesZoneOnTransfer(t *testing.T) {
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	A, B := shard.zones["midgaard"], shard.zones["darkwood"]
	s := newTestPlayerEntity(A, "Mover")
	A.join(s, "")
	mob := combatMobIn(A, s.entity, "goblin")
	// Force a fighting state on BOTH (simulating the pre-fix exploit: a fighting entity at the transfer
	// boundary). transferOut must scrub it.
	A.startFight(s.entity, mob)
	if s.entity.living.fighting == nil || mob.living.fighting == nil {
		t.Fatal("startFight precondition failed")
	}

	// Transfer into B's START room (deterministic, and NOT the aggressive goblin-chief's lair): this
	// isolates what the test pins — that the transfer SCRUBS the crossed fighting pointer. (Picking a
	// random B room via map iteration is flaky now: an aggressive mob in the destination legitimately
	// re-engages the arrival via aggroOnEntry, which is correct behavior, not a crossed pointer.)
	bRoom := B.startRoom
	A.transferOut(s, B, bRoom, "north", s.entity.location)
	B.handle(<-B.inbox)

	// The moved entity carries NO fighting pointer and is NOT posFighting on the destination.
	if s.entity.living.fighting != nil {
		t.Fatal("a fighting pointer crossed the zone boundary (transfer did not scrub it)")
	}
	if position(s.entity) == posFighting {
		t.Fatal("posFighting crossed the zone boundary")
	}
	// The opponent left in A is disengaged (not orphaned posFighting at a departed target).
	if mob.living.fighting != nil || position(mob) == posFighting {
		t.Fatal("the source-zone opponent was left fighting a departed player (orphaned)")
	}
}

// combatMobIn builds a mob with combat attributes (so it can fight) in actor's room. Distinct from
// combatMob (which takes an explicit combatRef + hp); this one gives the demo-style defaults.
func combatMobIn(z *Zone, actor *Entity, name string) *Entity {
	e := z.newEntity(ProtoRef("test:mob"))
	e.short = name
	e.setKeywords([]string{name})
	Add(e, &Living{})
	Move(e, actor.location)
	return e
}

// TestGatherCombatantsDeterministicOrder proves the gather order is reproducible across runs (sorted by
// character id) despite Go's randomized map iteration — the tie-break the default combat_order=0 needs.
func TestGatherCombatantsDeterministicOrder(t *testing.T) {
	z, _ := combatZone(t)
	// Several players in one room, all fighting a shared mob, registered in a scrambled order.
	room := z.players["Hero"].entity.location
	mob := combatMobIn(z, z.players["Hero"].entity, "goblin")
	for _, name := range []string{"Zed", "Ann", "Mike", "Bob"} {
		ps := &session{character: name, out: make(chan *playv1.ServerFrame, 8), epoch: 1}
		z.newPlayerEntity(ps, name)
		Move(ps.entity, room)
		z.players[name] = ps
		z.startFight(ps.entity, mob)
	}
	z.startFight(z.players["Hero"].entity, mob)

	order := func() []string {
		var ids []string
		for _, e := range z.gatherCombatants() {
			if s, ok := sessionOf(e); ok {
				ids = append(ids, s.character)
			}
		}
		return ids
	}
	first := order()
	for i := 0; i < 20; i++ {
		got := order()
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("gather order nondeterministic: %v vs %v", first, got)
		}
	}
	// The player IDs must appear in sorted order (Ann, Bob, Hero, Mike, Zed).
	var players []string
	for _, id := range first {
		players = append(players, id)
	}
	want := []string{"Ann", "Bob", "Hero", "Mike", "Zed"}
	if strings.Join(players, ",") != strings.Join(want, ",") {
		t.Fatalf("player gather order = %v, want %v (sorted by id)", players, want)
	}
}

// TestRoundDriverSelfCancels proves the round driver retires when no Fighting entities remain (both
// sides fled), so an idle zone carries no combat pulse.
func TestRoundDriverSelfCancels(t *testing.T) {
	z, s := combatZone(t)
	mob := combatMob(z, s.entity, "goblin", "", 50)
	z.startFight(s.entity, mob)
	z.stopFight(s.entity)
	z.stopFight(mob)
	// A full round with no combatants returns false -> the driver nils combatPulse.
	for i := uint64(0); i < PULSE_VIOLENCE; i++ {
		z.pulses.tick()
	}
	if z.combatPulse != nil {
		t.Fatalf("round driver did not self-cancel when combat ended")
	}
}

// TestNoCombatProfileAutoHits proves the degenerate bare case ([G-F]): an attacker with no profile
// (combatRef "") auto-hits and flows straight to damage (no to-hit classifier, no avoidance) — the 5e/
// WoW "no avoidance defs" path the acceptance gate required.
func TestNoCombatProfileAutoHits(t *testing.T) {
	z, s := combatZone(t)
	// No combatRef on the player; just a weapon.
	s.entity.living.combatRef = ""
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "dummy", "", 100)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(mob, "hp"); got != 94 {
		t.Fatalf("no-profile swing hp = %d, want 94 (auto-hit, 6 weapon damage)", got)
	}
}

// --- [G8] cooldown completion -----------------------------------------------------------------

// TestCooldownGatesAndElapses proves [G8]: arming a cooldown records an elapses-at pulse; the step-3
// gate refuses the ability while it cools; and the pulse callback clears it when it elapses.
func TestCooldownGatesAndElapses(t *testing.T) {
	z, s := combatZone(t)
	def := &abilityDef{ref: "bash", cooldown: 5}
	z.armCooldown(s, def)
	if !z.onCooldown(s.entity, "bash") {
		t.Fatalf("bash should be on cooldown right after arming")
	}
	if z.checkRequires(s, def) {
		t.Fatalf("checkRequires should block an ability still on cooldown")
	}
	for i := 0; i < 5; i++ {
		z.pulses.tick()
	}
	if z.onCooldown(s.entity, "bash") {
		t.Fatalf("bash cooldown should have elapsed after 5 pulses")
	}
	if _, present := s.entity.living.cooldowns["bash"]; present {
		t.Fatalf("the cooldown entry should have been cleared by the pulse callback")
	}
}

// TestCooldownPersistenceRoundTrip proves the P6-D8 dump/re-arm: a mid-cooldown dump records the
// REMAINING pulses, and rearmCooldown re-installs it (conserved, not refreshed).
func TestCooldownPersistenceRoundTrip(t *testing.T) {
	z, s := combatZone(t)
	z.armCooldown(s, &abilityDef{ref: "bash", cooldown: 10})
	z.pulses.tick()
	z.pulses.tick() // 2 pulses elapsed -> remaining 8
	cds := dumpCooldowns(s.entity)
	if cds["bash"] != 8 {
		t.Fatalf("dumped remaining = %d, want 8", cds["bash"])
	}
	// A fresh session re-arms from the saved remaining.
	z2, s2 := combatZone(t)
	z2.rearmCooldown(s2, "bash", cds["bash"])
	if !z2.onCooldown(s2.entity, "bash") {
		t.Fatalf("re-armed cooldown should be active")
	}
	for i := 0; i < 8; i++ {
		z2.pulses.tick()
	}
	if z2.onCooldown(s2.entity, "bash") {
		t.Fatalf("re-armed cooldown should elapse after its remaining 8 pulses")
	}
}

// --- The demo content end-to-end (the real milestone, all from the demo pack) ------------------

// TestDemoGoblinKillThroughPipeline drives the ACTUAL demo pack: spawn the goblin, `kill` it, and run
// rounds until the to-hit/avoidance/formula-damage/soak pipeline whittles its hp down — proving the
// whole 6.3a surface works from CONTENT with zero engine flavor-hardcoding. Seeded for determinism.
func TestDemoGoblinKillThroughPipeline(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	z.testCombatRng = rand.New(rand.NewSource(7))

	// A player in the hollow where the reset placed the goblin.
	hollow := z.rooms["darkwood:room:hollow"]
	if hollow == nil {
		t.Fatal("darkwood:room:hollow missing")
	}
	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Hero")
	Move(s.entity, hollow)
	z.players["Hero"] = s
	// Give the hero a sword (the demo's steel longsword: 2d6 slash) + a strong arm. attacks 3 so the
	// hero out-damages the goblin's content hp regen (1/pulse = 10/round) and the fight resolves.
	setAttrBase(s.entity, "strength", 18) // str_bonus floor((18-10)/2) = 4
	setAttrBase(s.entity, "accuracy", 10) // reliably beats the goblin's evasion 8
	setAttrBase(s.entity, "attacks", 3)
	equipWeapon(s.entity, &Weapon{diceNum: 2, diceSize: 6, damageType: "slash"})
	if s.entity.living.combatRef != "melee" {
		t.Fatalf("player should default to the 'melee' combat profile, got %q", s.entity.living.combatRef)
	}

	// Find the goblin the reset spawned.
	var goblin *Entity
	for _, e := range hollow.contents {
		if e.proto == "darkwood:mob:goblin" {
			goblin = e
		}
	}
	if goblin == nil {
		t.Fatal("the reset did not spawn the goblin")
	}
	startHP := resourceCurrent(goblin, "hp")
	if startHP <= 0 {
		t.Fatalf("goblin start hp = %d, want > 0", startHP)
	}

	// kill it.
	ctx := &Context{z: z, s: s, Actor: s.entity, arg: "goblin"}
	if err := cmdKill(ctx); err != nil {
		t.Fatalf("cmdKill: %v", err)
	}
	if s.entity.living.fighting != goblin {
		t.Fatalf("kill did not engage the goblin")
	}

	// Run rounds; track the goblin's LOW-WATER hp. The full pipeline (to-hit -> avoidance -> formula
	// damage -> soak -> apply) must grind it down. NOTE: the goblin has content hp regen (1/pulse), and
	// 6.3a has NO death path (reserved 6.3b) — so a mob brought to 0 hp is NOT removed and regen pulls it
	// back up. The 6.3a milestone is "the pipeline lands damage across rounds", which the low-water mark
	// (well below its start hp) proves; "the corpse drops" is the 6.3b assertion.
	low := startHP
	progressed := false
	for round := 0; round < 20; round++ {
		for i := uint64(0); i < PULSE_VIOLENCE; i++ {
			z.pulses.tick()
		}
		hp := resourceCurrent(goblin, "hp")
		if hp < low {
			low = hp
			progressed = true
		}
	}
	if !progressed {
		t.Fatalf("goblin hp never decreased across rounds (the pipeline landed no damage)")
	}
	// The fight ground the goblin to near-death (the reserved depletion seam fires; regen + no death
	// path keeps it alive at 6.3a — 6.3b makes it die). Low-water must be deep (it reached 0/near-0).
	if low > 5 {
		t.Fatalf("goblin low-water hp = %d, want <= 5 (the pipeline barely dented it)", low)
	}
}

// an builds a scoped attr-ref leaf (["attr", name]) for a test formula — e.g. an("$actor.dodge"). The
// production parser builds these from content; tests build them directly. attrNode is the SAME node the
// parser produces, so the check resolver dispatches the $actor/$target/$swing scope identically.
func an(ref string) formulaNode { return attrNode{ref: ref} }

var _ = playv1.ServerFrame{}
