package world

import (
	"math/rand"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// ability_test.go exercises slice 5.3: the effect-op interpreter + each registered op, the ability
// lifecycle (costs/cast-time/cooldown/tag-CC), the shared mitigation pipeline (resist/vuln/immune +
// soak), the PvP gate DEFENSE-IN-DEPTH (step-4 outer + the in-op guardHarmful, incl. a direct-invoke
// bypass attempt and the DoT path), and the end-to-end FIREBALL milestone. Effects are driven on the
// zone goroutine directly (no real timers) for determinism; the rng is seeded so dice are stable.

// abilityTestZone builds a bare zone wired like the demo pack's combat content: strength/max_hp/
// max_mana attributes, hp(vital)/mana resources, a fire + slash damage type with a resist matrix, a
// poison DoT affect, and the fireball ability (command-invocation, 30 mana, 8d6 fire + poison). It
// returns the zone plus a caster session in z.players (so resolve-by-id works).
func abilityTestZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	z := newZone("test")
	z.defs.attr.register("strength", &attributeDef{ref: "strength", base: litNode{v: 10}})
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.attr.register("max_mana", &attributeDef{ref: "max_mana", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	z.defs.res.register("mana", &resourceDef{ref: "mana", maxAttr: "max_mana"})
	z.defs.dmg.register("fire", &damageTypeDef{ref: "fire", resist: map[string]float64{"fire": 1.0}})
	z.defs.dmg.register("slash", &damageTypeDef{ref: "slash", resist: map[string]float64{"slash": 0.5}})
	z.defs.dmg.register("ice", &damageTypeDef{ref: "ice", resist: map[string]float64{"ice": 2.0}})
	z.defs.dmg.register("holy", &damageTypeDef{ref: "holy", resist: map[string]float64{"holy": 0.0}})

	z.defs.affect.register("poison", &affectDef{
		ref: "poison", name: "Poisoned", stacking: stackCount, maxStacks: 5, duration: 30,
		dispellable: true, category: "poison",
		modifiers: []affectModifier{{attr: "strength", add: true, value: -2}},
		hasTick:   true, tickInterval: 6,
		tickOps: []effectOp{{kind: "deal_damage", dmgType: "poison", amount: 4}},
	})
	z.defs.affect.register("haste", &affectDef{
		ref: "haste", name: "Hasted", stacking: stackRefresh, maxStacks: 1, duration: 20, dispellable: true,
		modifiers: []affectModifier{{attr: "strength", add: true, value: 5}},
	})

	// fireball ability: command, enemy, harmful, 30 mana, instant, 8d6 fire + poison.
	fireball := &abilityDef{
		ref: "fireball", name: "Fireball", invocation: "command", words: []string{"fireball"},
		mode: tmEnemy, disposition: dispHarmful,
		tags:     []string{"cast", "verbal", "fire"},
		costs:    []resourceCost{{resource: "mana", amount: 30}},
		msgActor: "You hurl a roaring fireball at $N!",
		ops: []effectOp{
			{kind: "deal_damage", dmgType: "fire", diceNum: 8, diceSize: 6},
			{kind: "apply_affect", affect: "poison", harmful: true},
		},
	}
	z.defs.ability.register("fireball", fireball)
	z.defs.abilityCmds["fireball"] = fireball

	caster := makeRoomPlayer(z, "Caster")
	return z, caster
}

// makeRoomPlayer builds a player session + entity placed in a fresh room in z, registered in
// z.players. Returns the session.
func makeRoomPlayer(z *Zone, name string) *session {
	room := z.newEntity(ProtoRef("test:room"))
	Add(room, &Room{})
	s := &session{character: name, out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(s, name)
	Move(s.entity, room)
	z.players[name] = s
	return s
}

// makeMobTarget builds a non-player living entity (no PlayerControlled) in the same room as actor.
func makeMobTarget(z *Zone, actor *Entity, name string) *Entity {
	e := z.newEntity(ProtoRef("test:mob"))
	e.short = name
	e.setKeywords([]string{name})
	Add(e, &Living{})
	Move(e, actor.location)
	return e
}

// makePlayerTargetInRoom builds a second player target in the same room as actor.
func makePlayerTargetInRoom(z *Zone, actor *Entity, name string) *session {
	s := &session{character: name, out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(s, name)
	s.entity.setKeywords([]string{name})
	Move(s.entity, actor.location)
	z.players[name] = s
	return s
}

// seededCtx builds a deterministic effectCtx for direct op-handler tests.
func seededCtx(z *Zone, actor, target *Entity, disp abilityDisposition) *effectCtx {
	return &effectCtx{
		z: z, actor: actor, source: actor, target: target, mag: 1, disp: disp,
		rng: rand.New(rand.NewSource(1)),
	}
}

// --- Effect-op handler tests -------------------------------------------------------------------

func TestOpDealDamageThroughMitigation(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	// fire is neutral (×1.0), no soak: a 20 raw deals 20.
	dealDamage(c, mob, 20, "fire")
	if got := resourceCurrent(mob, "hp"); got != 80 {
		t.Fatalf("fire 20 -> hp %d, want 80", got)
	}
}

func TestOpHealRaisesPoolClampedAtMax(t *testing.T) {
	z, caster := abilityTestZone(t)
	setResourceCurrent(caster.entity, "hp", 50)
	c := seededCtx(z, caster.entity, caster.entity, dispHelpful)
	opHeal(c, &effectOp{resource: "hp", amount: 30})
	if got := resourceCurrent(caster.entity, "hp"); got != 80 {
		t.Fatalf("heal 30 from 50 -> %d, want 80", got)
	}
	opHeal(c, &effectOp{resource: "hp", amount: 1000})
	if got := resourceCurrent(caster.entity, "hp"); got != 100 {
		t.Fatalf("heal past max -> %d, want clamp 100", got)
	}
}

func TestOpModifyResourceDrainGated(t *testing.T) {
	z, caster := abilityTestZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	setResourceCurrent(victim.entity, "mana", 50)
	// No consent => a drain (harmful) on a player is a clean no-op.
	c := seededCtx(z, caster.entity, victim.entity, dispHarmful)
	opModifyResource(c, &effectOp{resource: "mana", amount: -20})
	if got := resourceCurrent(victim.entity, "mana"); got != 50 {
		t.Fatalf("gated drain must be a no-op: mana %d, want 50", got)
	}
	// With consent on both, the drain lands.
	setFlag(caster.entity, flagPvP, true)
	setFlag(victim.entity, flagPvP, true)
	opModifyResource(c, &effectOp{resource: "mana", amount: -20})
	if got := resourceCurrent(victim.entity, "mana"); got != 30 {
		t.Fatalf("consented drain -> mana %d, want 30", got)
	}
}

func TestOpApplyAffectBuffNotGated(t *testing.T) {
	z, caster := abilityTestZone(t)
	ally := makePlayerTargetInRoom(z, caster.entity, "Ally")
	// A HELPFUL apply_affect on another player (no consent) must NOT be gated.
	c := seededCtx(z, caster.entity, ally.entity, dispHelpful)
	opApplyAffect(c, &effectOp{affect: "haste"})
	if attr(ally.entity, "strength") != 15 {
		t.Fatalf("helpful buff must apply ungated: strength %v, want 15", attr(ally.entity, "strength"))
	}
}

func TestOpChanceDeterministicWithRng(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	// prob 0 never runs; prob 1 always runs.
	opChance(c, &effectOp{prob: 0, then: []effectOp{{kind: "deal_damage", dmgType: "fire", amount: 10}}})
	if resourceCurrent(mob, "hp") != 100 {
		t.Fatal("chance 0 must not run the then-branch")
	}
	opChance(c, &effectOp{prob: 1, then: []effectOp{{kind: "deal_damage", dmgType: "fire", amount: 10}}})
	if resourceCurrent(mob, "hp") != 90 {
		t.Fatal("chance 1 must run the then-branch")
	}
}

func TestOpIfBranchesOnHasAffect(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	ifOp := &effectOp{
		kind: "if", affect: "poison",
		then: []effectOp{{kind: "deal_damage", dmgType: "fire", amount: 50}},
		els:  []effectOp{{kind: "deal_damage", dmgType: "fire", amount: 5}},
	}
	opIf(c, ifOp) // not poisoned -> else (5)
	if resourceCurrent(mob, "hp") != 95 {
		t.Fatalf("if-else (not poisoned) -> hp %d, want 95", resourceCurrent(mob, "hp"))
	}
	applyAffect(mob, "poison", attachOpts{source: caster.entity})
	opIf(c, ifOp) // poisoned -> then (50)
	if resourceCurrent(mob, "hp") != 45 {
		t.Fatalf("if-then (poisoned) -> hp %d, want 45", resourceCurrent(mob, "hp"))
	}
}

// --- Shared mitigation pipeline tests ----------------------------------------------------------

func TestMitigationResistVulnImmune(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "dummy")
	tests := []struct {
		dtype string
		raw   float64
		want  int
	}{
		{"fire", 100, 100},    // neutral ×1.0
		{"slash", 100, 50},    // resist ×0.5
		{"ice", 100, 200},     // vuln ×2.0
		{"holy", 100, 0},      // immune ×0.0
		{"unknown", 100, 100}, // no def -> raw passes through
	}
	for _, tc := range tests {
		if got := mitigate(mob, tc.raw, tc.dtype); got != tc.want {
			t.Errorf("mitigate(%s, %v) = %d, want %d", tc.dtype, tc.raw, got, tc.want)
		}
	}
}

// --- Lifecycle tests ---------------------------------------------------------------------------

func TestLifecycleCostsCheckedAndPaid(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	def := z.defs.ability.get("fireball")

	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))
	if got := resourceCurrent(caster.entity, "mana"); got != 70 {
		t.Fatalf("fireball must pay 30 mana: mana %d, want 70", got)
	}

	// Insufficient mana: the cast aborts, no mana spent, no damage.
	setResourceCurrent(caster.entity, "mana", 10)
	hpBefore := resourceCurrent(mob, "hp")
	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))
	if got := resourceCurrent(caster.entity, "mana"); got != 10 {
		t.Fatalf("insufficient-mana cast must not spend: mana %d, want 10", got)
	}
	if resourceCurrent(mob, "hp") != hpBefore {
		t.Fatal("insufficient-mana cast must deal no damage")
	}
}

func TestLifecycleCastTimeLockoutThenResolve(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)

	slow := &abilityDef{
		ref: "slowbolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		costs: []resourceCost{{resource: "mana", amount: 30}}, castTime: 3,
		ops: []effectOp{{kind: "deal_damage", dmgType: "fire", amount: 20}},
	}
	z.castAbility(caster, slow, "goblin", rand.New(rand.NewSource(1)))
	// Costs are NOT paid until commit; the cast is locked out for 3 pulses.
	if resourceCurrent(caster.entity, "mana") != 100 {
		t.Fatal("cast_time>0 must defer payment to commit")
	}
	if resourceCurrent(mob, "hp") != 100 {
		t.Fatal("cast must not resolve before its cast time elapses")
	}
	z.pulses.tick()
	z.pulses.tick()
	if resourceCurrent(mob, "hp") != 100 {
		t.Fatal("cast resolved early (before pulse 3)")
	}
	z.pulses.tick() // pulse 3: commit + resolve
	if resourceCurrent(caster.entity, "mana") != 70 {
		t.Fatalf("cast-time commit must pay: mana %d, want 70", resourceCurrent(caster.entity, "mana"))
	}
	if resourceCurrent(mob, "hp") != 80 {
		t.Fatalf("cast-time resolve must deal 20: hp %d, want 80", resourceCurrent(mob, "hp"))
	}
}

// TestLifecycleCastAbortsWhenCasterFrozen proves the resolve-by-id/skip-frozen contract for a deferred
// cast: a cast that completes after the caster froze (mid-handoff) must abort, not touch the entity.
func TestLifecycleCastAbortsWhenCasterFrozen(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)

	slow := &abilityDef{
		ref: "slowbolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		costs: []resourceCost{{resource: "mana", amount: 30}}, castTime: 2,
		ops: []effectOp{{kind: "deal_damage", dmgType: "fire", amount: 20}},
	}
	z.castAbility(caster, slow, "goblin", rand.New(rand.NewSource(1)))
	caster.frozen = true // the caster is handed off mid-cast
	z.pulses.tick()
	z.pulses.tick() // would commit here, but the caster is frozen -> abort
	if resourceCurrent(mob, "hp") != 100 {
		t.Fatal("a cast completing after the caster froze must abort (resolve-by-id contract)")
	}
	if resourceCurrent(caster.entity, "mana") != 100 {
		t.Fatal("an aborted cast must not pay costs")
	}
}

func TestLifecycleCooldownArmedOnCommit(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	def := &abilityDef{
		ref: "cdbolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		costs: []resourceCost{{resource: "mana", amount: 30}}, cooldown: 5,
		ops: []effectOp{{kind: "deal_damage", dmgType: "fire", amount: 10}},
	}
	before := len(z.pulses.due)
	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))
	if len(z.pulses.due) != before+1 {
		t.Fatalf("commit must arm a cooldown pulse: due %d, want %d", len(z.pulses.due), before+1)
	}
}

// TestLifecycleTagCCBlocksAtStep3: an active affect that prevents a tag the ability carries blocks the
// cast at step 3 — before costs, before damage.
func TestLifecycleTagCCBlocksAtStep3(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	// A silence affect prevents the "verbal" tag fireball carries.
	z.defs.affect.register("silence", &affectDef{
		ref: "silence", name: "Silenced", stacking: stackRefresh, maxStacks: 1, duration: 10,
		prevents: []string{"verbal"},
	})
	applyAffect(caster.entity, "silence", attachOpts{})
	def := z.defs.ability.get("fireball")
	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))
	if resourceCurrent(caster.entity, "mana") != 100 {
		t.Fatal("a tag-CC block at step 3 must precede cost payment")
	}
	if resourceCurrent(mob, "hp") != 100 {
		t.Fatal("a tag-CC block must deal no damage")
	}
}

// --- PvP gate defense-in-depth -----------------------------------------------------------------

// TestPvPGateBlocksHarmfulAbilityAtStep4: a harmful ability vs a non-consenting player target is
// blocked at the OUTER lifecycle layer — before costs, before any effect.
func TestPvPGateBlocksHarmfulAbilityAtStep4(t *testing.T) {
	z, caster := abilityTestZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	setResourceCurrent(victim.entity, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	def := z.defs.ability.get("fireball")

	z.castAbility(caster, def, "Victim", rand.New(rand.NewSource(1)))
	if resourceCurrent(caster.entity, "mana") != 100 {
		t.Fatal("step-4 gate must block BEFORE costs (no mana spent)")
	}
	if resourceCurrent(victim.entity, "hp") != 100 {
		t.Fatal("step-4 gate must deal no damage to a non-consenting player")
	}
	if a, ok := Get[*Affected](victim.entity); ok && len(a.list) > 0 {
		t.Fatal("step-4 gate must apply no affect to a non-consenting player")
	}
}

// TestPvPGuardBlocksHarmfulOpOnDirectInvoke is the CRITICAL can't-bypass test: a harmful op invoked
// DIRECTLY (simulating a step-4 bypass / a custom Lua on_resolve) cannot harm a protected player —
// the in-op guardHarmful funnel stops it. Covers deal_damage AND a debuff apply_affect.
func TestPvPGuardBlocksHarmfulOpOnDirectInvoke(t *testing.T) {
	z, caster := abilityTestZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	setResourceCurrent(victim.entity, "hp", 100)
	c := seededCtx(z, caster.entity, victim.entity, dispHarmful)

	// deal_damage directly — NO step-4 ran. The shared mitigation pipeline's guard must block it.
	if dealt := dealDamage(c, victim.entity, 50, "fire"); dealt != 0 {
		t.Fatalf("direct deal_damage on a protected player must deal 0, dealt %d", dealt)
	}
	if resourceCurrent(victim.entity, "hp") != 100 {
		t.Fatal("in-op guard must protect a non-consenting player from direct deal_damage")
	}

	// debuff apply_affect directly — applyDebuff's guard must block it.
	if applied := applyDebuff(c, victim.entity, "poison", attachOpts{source: caster.entity}); applied {
		t.Fatal("direct debuff apply_affect on a protected player must be blocked")
	}
	if a, ok := Get[*Affected](victim.entity); ok && len(a.list) > 0 {
		t.Fatal("in-op guard must protect a non-consenting player from a direct debuff")
	}
}

// TestPvPGuardAllowsConsentingAndArenaAndSafe exercises the data-driven policy: both-consent allows,
// a safe room vetoes even with consent, an arena forces PvP over no consent.
func TestPvPGatePolicy(t *testing.T) {
	z, caster := abilityTestZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")

	// Default: no consent => blocked.
	if pvpAllowed(caster.entity, victim.entity) {
		t.Fatal("default policy must forbid PvP without consent")
	}
	// Both consent => allowed.
	setFlag(caster.entity, flagPvP, true)
	setFlag(victim.entity, flagPvP, true)
	if !pvpAllowed(caster.entity, victim.entity) {
		t.Fatal("both-consent must allow PvP")
	}
	// Safe room is an absolute veto, even with consent.
	victim.entity.location.room.namedFlags = map[string]bool{flagSafe: true}
	if pvpAllowed(caster.entity, victim.entity) {
		t.Fatal("a safe room must veto PvP even with consent")
	}
	// Arena forces PvP even without consent (clear consent, set arena on both rooms — same room here).
	setFlag(caster.entity, flagPvP, false)
	setFlag(victim.entity, flagPvP, false)
	victim.entity.location.room.namedFlags = map[string]bool{flagArena: true}
	if !pvpAllowed(caster.entity, victim.entity) {
		t.Fatal("an arena room must force PvP without consent")
	}
}

// TestPvPGateNoOpVsMobAndBuff: anything vs a mob is NOT gated, and a buff on anyone is NOT gated.
func TestPvPGateNoOpVsMobAndBuff(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	c := seededCtx(z, caster.entity, mob, dispHarmful)
	// Harmful op vs a mob: always proceeds (PvP only).
	if dealt := dealDamage(c, mob, 30, "fire"); dealt != 30 {
		t.Fatalf("harmful op vs a mob must proceed: dealt %d, want 30", dealt)
	}
	// A helpful buff on a non-consenting player: never gated.
	ally := makePlayerTargetInRoom(z, caster.entity, "Ally")
	bc := seededCtx(z, caster.entity, ally.entity, dispHelpful)
	opApplyAffect(bc, &effectOp{affect: "haste"})
	if attr(ally.entity, "strength") != 15 {
		t.Fatal("a helpful buff must never be gated")
	}
}

// TestPvPGuardBlocksDoTTick: the DoT path is gated too — a poison applied by an attacker that loses
// PvP eligibility must not tick damage onto a protected player.
func TestPvPGuardBlocksDoTTick(t *testing.T) {
	z, caster := abilityTestZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	setResourceCurrent(victim.entity, "hp", 100)
	// Apply poison while consenting (both opted in), then revoke consent before the tick.
	setFlag(caster.entity, flagPvP, true)
	setFlag(victim.entity, flagPvP, true)
	applyAffect(victim.entity, "poison", attachOpts{source: caster.entity})
	setFlag(victim.entity, flagPvP, false) // victim opts out

	// Drive the affect tick to the poison interval (6). The DoT's deal_damage must funnel through the
	// guard with the affect's SOURCE as the actor — now blocked.
	a, _ := Get[*Affected](victim.entity)
	for i := 0; i < 6; i++ {
		a.tickOnce(victim.entity, uint64(i))
	}
	if resourceCurrent(victim.entity, "hp") != 100 {
		t.Fatalf("a DoT tick must be gated when the source can no longer harm the target: hp %d",
			resourceCurrent(victim.entity, "hp"))
	}
}

// --- Derived-harm gate (the disposition-hole regressions, security MUST-FIX #1) ----------------

// TestApplyAffectDerivedHarmBlocksMislabeledDebuff is the disposition-hole regression: a detrimental
// affect (stat-reducing, or carrying prevents tags) MUST be gated on a protected player even when the
// op/ability labels it helpful/neutral/unlabeled. The harm decision is DERIVED from the def, never
// trusted from the content label.
func TestApplyAffectDerivedHarmBlocksMislabeledDebuff(t *testing.T) {
	z, caster := abilityTestZone(t)
	// A stat-reducing debuff registered as a fresh ref so the test owns it.
	z.defs.affect.register("curse", &affectDef{
		ref: "curse", name: "Cursed", stacking: stackRefresh, maxStacks: 1, duration: 20,
		modifiers: []affectModifier{{attr: "strength", add: true, value: -5}},
	})
	// A prevents-only (CC) affect with no stat modifier and a neutral category.
	z.defs.affect.register("rooted", &affectDef{
		ref: "rooted", name: "Rooted", stacking: stackRefresh, maxStacks: 1, duration: 20,
		prevents: []string{"move"},
	})

	for _, ref := range []string{"curse", "rooted"} {
		victim := makePlayerTargetInRoom(z, caster.entity, "Victim_"+ref)
		// disp NEUTRAL and op.harmful FALSE — the only thing that can gate this is the derived harm.
		c := seededCtx(z, caster.entity, victim.entity, dispNeutral)
		opApplyAffect(c, &effectOp{affect: ref, harmful: false})
		if a, ok := Get[*Affected](victim.entity); ok && len(a.list) > 0 {
			t.Fatalf("mislabeled detrimental affect %q landed on a protected player (disposition hole)", ref)
		}
	}
}

// TestApplyAffectDerivedHarmAllowsBuffOnAlly proves the derived-harm gate does NOT over-block: a
// genuinely-beneficial affect (only stat-raising mods, no prevents) on another player stays ungated.
func TestApplyAffectDerivedHarmAllowsBuffOnAlly(t *testing.T) {
	z, caster := abilityTestZone(t)
	ally := makePlayerTargetInRoom(z, caster.entity, "Ally")
	// disp NEUTRAL, not harmful — haste is +5 strength, a pure buff: it must still land ungated.
	c := seededCtx(z, caster.entity, ally.entity, dispNeutral)
	opApplyAffect(c, &effectOp{affect: "haste"})
	if attr(ally.entity, "strength") != 15 {
		t.Fatalf("a genuinely-beneficial buff must land ungated: strength %v, want 15", attr(ally.entity, "strength"))
	}
}

// TestAffectIsDetrimentalDerivation unit-checks the derivation independent of the gate.
func TestAffectIsDetrimentalDerivation(t *testing.T) {
	cases := []struct {
		name string
		def  *affectDef
		want bool
	}{
		{"flat-reduction", &affectDef{modifiers: []affectModifier{{attr: "str", add: true, value: -1}}}, true},
		{"mult-reduction", &affectDef{modifiers: []affectModifier{{attr: "spd", add: false, value: 0.5}}}, true},
		{"prevents-tag", &affectDef{prevents: []string{"cast"}}, true},
		{"affliction-category", &affectDef{category: "poison"}, true},
		{"pure-buff-flat", &affectDef{modifiers: []affectModifier{{attr: "str", add: true, value: 5}}}, false},
		{"pure-buff-mult", &affectDef{modifiers: []affectModifier{{attr: "spd", add: false, value: 1.5}}}, false},
		{"empty", &affectDef{}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := affectIsDetrimental(tc.def); got != tc.want {
			t.Errorf("%s: affectIsDetrimental = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- dispel / remove_affect cross-player gating (security MUST-FIX #2) --------------------------

// TestDispelAndRemoveAffectGatedOnProtectedPlayer: stripping a protected player's buff via dispel or
// remove_affect is harm and must be blocked — their buff stays on.
func TestDispelAndRemoveAffectGatedOnProtectedPlayer(t *testing.T) {
	z, caster := abilityTestZone(t)

	// dispel: victim has a dispellable buff; an attacker with no consent must not strip it.
	{
		victim := makePlayerTargetInRoom(z, caster.entity, "DispelVic")
		applyAffect(victim.entity, "haste", attachOpts{}) // dispellable buff
		c := seededCtx(z, caster.entity, victim.entity, dispNeutral)
		opDispel(c, &effectOp{}) // any category, all
		if a, ok := Get[*Affected](victim.entity); !ok || len(a.list) != 1 {
			t.Fatal("dispel on a protected player must be blocked (buff must remain)")
		}
	}
	// remove_affect: same — keyed per-source by the caster, applied by the caster so the key matches.
	{
		victim := makePlayerTargetInRoom(z, caster.entity, "RemoveVic")
		applyAffect(victim.entity, "haste", attachOpts{source: caster.entity})
		c := seededCtx(z, caster.entity, victim.entity, dispNeutral)
		opRemoveAffect(c, &effectOp{affect: "haste"})
		if a, ok := Get[*Affected](victim.entity); !ok || len(a.list) != 1 {
			t.Fatal("remove_affect on a protected player must be blocked (buff must remain)")
		}
	}
	// Self-cleanse stays ungated: a player removing their OWN affect works.
	{
		applyAffect(caster.entity, "haste", attachOpts{})
		c := seededCtx(z, caster.entity, caster.entity, dispNeutral)
		opDispel(c, &effectOp{})
		if a, ok := Get[*Affected](caster.entity); ok && len(a.list) != 0 {
			t.Fatal("self-dispel must not be gated")
		}
	}
}

// --- modify_resource positive-delta cross-player gating (security MUST-FIX #3) ------------------

// TestModifyResourcePositiveDeltaGated: a POSITIVE modify_resource on a protected player (a
// "corruption"/"doom" pool the engine can't reason about) is gated like a drain.
func TestModifyResourcePositiveDeltaGated(t *testing.T) {
	z, caster := abilityTestZone(t)
	// A content-defined harmful-when-raised pool. We reuse mana as a stand-in pool: any cross-player
	// write must be gated regardless of sign.
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	setResourceCurrent(victim.entity, "mana", 50)
	c := seededCtx(z, caster.entity, victim.entity, dispNeutral)

	// No consent: a POSITIVE delta must be a clean no-op (the polarity-agnostic gate).
	opModifyResource(c, &effectOp{resource: "mana", amount: 20})
	if got := resourceCurrent(victim.entity, "mana"); got != 50 {
		t.Fatalf("positive cross-player modify_resource must be gated: mana %d, want 50", got)
	}
	// With both consenting, the write lands.
	setFlag(caster.entity, flagPvP, true)
	setFlag(victim.entity, flagPvP, true)
	opModifyResource(c, &effectOp{resource: "mana", amount: 20})
	if got := resourceCurrent(victim.entity, "mana"); got != 70 {
		t.Fatalf("consented cross-player modify_resource must land: mana %d, want 70", got)
	}
}

// TestHealNotWeaponizableCrossPlayer: heal stays ungated on an ally (the sanctioned exception) but a
// negative `amount` cannot turn it into a cross-player drain.
func TestHealNotWeaponizableCrossPlayer(t *testing.T) {
	z, caster := abilityTestZone(t)
	ally := makePlayerTargetInRoom(z, caster.entity, "Ally")
	setResourceCurrent(ally.entity, "hp", 50)
	c := seededCtx(z, caster.entity, ally.entity, dispHelpful)
	// A beneficial heal on an ally lands ungated.
	opHeal(c, &effectOp{resource: "hp", amount: 30})
	if got := resourceCurrent(ally.entity, "hp"); got != 80 {
		t.Fatalf("heal on an ally must land ungated: hp %d, want 80", got)
	}
	// A NEGATIVE heal amount must not drain (clamped to 0 magnitude).
	opHeal(c, &effectOp{resource: "hp", amount: -100})
	if got := resourceCurrent(ally.entity, "hp"); got != 80 {
		t.Fatalf("a negative heal must not drain a player: hp %d, want 80", got)
	}
}

// --- Deferred-cast TARGET resolve-by-id (concurrency MUST-FIX #4) -------------------------------

// TestDeferredCastTargetTransferAborts is THE target-transfer regression: a cast_time>0 ability aimed
// at a PLAYER target that transfers to a sibling zone BEFORE the completion pulse must abort cleanly —
// the source zone's deferred callback must (a) deal no damage/affect to the now-foreign entity and (b)
// NEVER touch the destination zone's pulse scheduler. Run under -race (the standing single-writer guard).
func TestDeferredCastTargetTransferAborts(t *testing.T) {
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	A, B := shard.zones["midgaard"], shard.zones["darkwood"]
	if A == nil || B == nil {
		t.Fatal("zones not built")
	}

	// A slow harmful ability with a cast time, registered into the shared ability registry.
	slow := &abilityDef{
		ref: "slowbolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		castTime: 3,
		ops: []effectOp{
			{kind: "deal_damage", dmgType: "fire", amount: 40},
			{kind: "apply_affect", affect: "poison", harmful: true},
		},
	}
	A.defs.ability.register("slowbolt", slow)

	// Caster and target player, both in A (consenting so the cast is not blocked at step 4).
	caster := newTestPlayerEntity(A, "Caster")
	A.join(caster, "")
	target := newTestPlayerEntity(A, "Target")
	A.join(target, "")
	Move(target.entity, caster.entity.location) // same room as the caster
	setFlag(caster.entity, flagPvP, true)
	setFlag(target.entity, flagPvP, true)
	setResourceCurrent(target.entity, "hp", 100)

	// Start the cast (cast_time 3 -> deferred). It captures the target by STABLE ID, not pointer.
	A.castAbility(caster, slow, "Target", rand.New(rand.NewSource(1)))

	// Transfer the target to sibling zone B BEFORE the completion pulse (the same path the 5.2 affect-
	// move test drives). After this, B's goroutine owns the target entity.
	var bRoom ProtoRef
	for ref := range B.rooms {
		bRoom = ref
		break
	}
	A.transferOut(target, B, bRoom, "north", target.entity.location)
	B.handle(<-B.inbox)
	if B.players["Target"] == nil || target.entity.zone != B {
		t.Fatal("target not re-homed to B")
	}

	bDueBefore := len(B.pulses.due)

	// Drive A's pulses to the completion pulse. The deferred cast must re-resolve the target by id,
	// find it absent in A, and abort — touching neither the foreign entity nor B's scheduler.
	A.pulses.tick()
	A.pulses.tick()
	A.pulses.tick() // pulse 3: completion

	if got := resourceCurrent(target.entity, "hp"); got != 100 {
		t.Fatalf("transferred target took damage from a source-zone deferred cast: hp %d, want 100", got)
	}
	if a, ok := Get[*Affected](target.entity); ok {
		for _, inst := range a.list {
			if inst.def.ref == "poison" {
				t.Fatal("transferred target received a poison affect from the source-zone deferred cast")
			}
		}
	}
	if len(B.pulses.due) != bDueBefore {
		t.Fatalf("source zone touched the destination scheduler: B.due %d, want %d (no cross-zone write)",
			len(B.pulses.due), bDueBefore)
	}
}

// TestDeferredCastAbortsWhenCasterAbsent: a deferred cast whose CASTER left (not just froze) before
// completion aborts cleanly (the caster-absent branch).
func TestDeferredCastAbortsWhenCasterAbsent(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	slow := &abilityDef{
		ref: "slowbolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		costs: []resourceCost{{resource: "mana", amount: 30}}, castTime: 2,
		ops: []effectOp{{kind: "deal_damage", dmgType: "fire", amount: 20}},
	}
	z.castAbility(caster, slow, "goblin", rand.New(rand.NewSource(1)))
	delete(z.players, caster.character) // the caster departs mid-cast
	z.pulses.tick()
	z.pulses.tick() // would commit, but the caster is absent -> abort
	if resourceCurrent(mob, "hp") != 100 {
		t.Fatal("a deferred cast whose caster left must abort (no damage)")
	}
}

// --- The end-to-end fireball milestone ---------------------------------------------------------

// TestFireballMilestone: cast fireball at a mob — 30 mana paid, 8d6 fire damage through the shared
// mitigation pipeline, and poison applied + ticking.
func TestFireballMilestone(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	def := z.defs.ability.get("fireball")

	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))

	// 30 mana paid.
	if got := resourceCurrent(caster.entity, "mana"); got != 70 {
		t.Fatalf("fireball mana %d, want 70 (30 paid)", got)
	}
	// Fire damage landed (8d6 is 8..48; hp dropped from 100).
	hp := resourceCurrent(mob, "hp")
	if hp >= 100 || hp < 52 {
		t.Fatalf("fireball fire damage hp %d, want in [52,99] (8d6)", hp)
	}
	// Poison applied.
	a, ok := Get[*Affected](mob)
	if !ok || a.byKey[keyFor(z.defs.affect.get("poison"), caster.entity)] == nil {
		t.Fatal("fireball must apply poison to the target")
	}
	// Poison ticks: drive to the interval and confirm hp drops further by the DoT (4 * 1 stack).
	hpBeforeTick := resourceCurrent(mob, "hp")
	for i := 0; i < 6; i++ {
		a.tickOnce(mob, uint64(i))
	}
	if resourceCurrent(mob, "hp") != hpBeforeTick-4 {
		t.Fatalf("poison DoT tick must deal 4: hp %d, want %d", resourceCurrent(mob, "hp"), hpBeforeTick-4)
	}
}

// TestFireballViaDispatch drives the WHOLE command spine: a typed "fireball goblin" line resolves the
// ability command and casts it — proving the command-invocation registration (defineGlobals -> the
// per-shard ability table -> dispatch fall-through).
func TestFireballViaDispatch(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)

	z.dispatch(caster, "fireball goblin")
	if resourceCurrent(caster.entity, "mana") != 70 {
		t.Fatalf("dispatched fireball must pay 30 mana: mana %d", resourceCurrent(caster.entity, "mana"))
	}
	if resourceCurrent(mob, "hp") >= 100 {
		t.Fatal("dispatched fireball must deal damage")
	}
}

// TestFireballFromDemoPack is THE milestone proof: fireball is loaded from the EMBEDDED demo YAML
// (zero engine changes to add it), registers as a command, and casts — paying 30 mana, dealing fire
// damage, applying poison. It drives the real content pipeline (newDemoZone -> defineGlobals).
func TestFireballFromDemoPack(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	if z.abilityForVerb("fireball") == nil {
		t.Fatal("demo pack must register the fireball command")
	}
	// A caster in the temple with a mob target.
	caster := &session{character: "Mage", out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(caster, "Mage")
	z.players["Mage"] = caster
	Move(caster.entity, z.rooms[z.startRoom])
	setAttrBase(caster.entity, "intellect", 12) // ensure mana headroom
	setResourceCurrent(caster.entity, "mana", resourceMax(caster.entity, "mana"))
	manaBefore := resourceCurrent(caster.entity, "mana")

	mob := makeMobTarget(z, caster.entity, "training dummy")
	mob.setKeywords([]string{"dummy"})
	setResourceCurrent(mob, "hp", 100)
	// fireball is now an AoE save-for-half ([G12]): a FAILED save deals full damage + the burn rider, a
	// made save halves it and skips the burn. Pin the mob to ALWAYS FAIL (a very negative dex_save) so
	// this milestone proof still exercises the full-damage + poison-rider path deterministically — the
	// AoE/save mechanics get their own per-target coverage in aoe_test.go.
	setAttrBase(mob, "dex_save", -100)

	z.dispatch(caster, "fireball dummy")

	if got := manaBefore - resourceCurrent(caster.entity, "mana"); got != 30 {
		t.Fatalf("demo fireball must cost 30 mana, spent %d", got)
	}
	if resourceCurrent(mob, "hp") >= 100 {
		t.Fatal("demo fireball must deal fire damage")
	}
	if a, ok := Get[*Affected](mob); !ok || len(a.list) == 0 {
		t.Fatal("demo fireball must apply poison on a failed save")
	}
}
