package world

import (
	"math/rand"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// death_test.go exercises Phase 6.3b: the death -> corpse -> OnKill path, the threat list + mob
// retargeting, assist/consider, aggressive-mob initiation, and the on_depleted hook. Determinism: an
// auto-hit profile + a fixed weapon (6d1 = always 6) make the killing-blow round exact; the seeded
// z.testCombatRng makes a driven fight reproducible.

// killShotProfile is an auto-hit profile with a big flat damage bonus so one swing fells a low-hp mob
// deterministically (no to-hit/avoidance randomness in the death assertions).
func killShotProfile(bonus float64) *combatProfile {
	return &combatProfile{
		toHit:       &checkSpec{label: "Attack", dice: mustDiceT("1d1"), bands: []checkBand{{label: "hit"}}},
		damageBonus: litNode{v: bonus},
	}
}

// --- Death -> corpse (the milestone core) ------------------------------------------------------

// TestMobDeathDropsCorpseWithLoot proves the [G-D] core: a swing that empties a mob's vital pool runs
// die() -> a CORPSE container appears in the room holding the mob's carried gear, and the mob entity is
// removed. The corpse is lootable (`get all corpse` / look) — verified by content moving its items out.
func TestMobDeathDropsCorpseWithLoot(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("killer", killShotProfile(100)) // 6 weapon + 100 = 106 >> mob hp
	s.entity.living.combatRef = "killer"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})

	mob := combatMob(z, s.entity, "goblin", "", 10)
	room := mob.location
	// Arm the goblin with a carried item (its "loot"): a knife in its inventory.
	knife := z.newEntity(ProtoRef("test:knife"))
	knife.short = "a rusty knife"
	knife.setKeywords([]string{"knife"})
	Move(knife, mob)

	z.startFight(s.entity, mob)
	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())

	// The mob entity is gone from the room.
	for _, e := range room.contents {
		if e == mob {
			t.Fatalf("dead mob still in the room (should be removed)")
		}
	}
	// A corpse container is in the room, holding the knife.
	var corpse *Entity
	for _, e := range room.contents {
		if _, ok := Get[*Container](e); ok {
			corpse = e
		}
	}
	if corpse == nil {
		t.Fatalf("no corpse appeared in the room after the mob died")
	}
	if len(corpse.contents) != 1 || corpse.contents[0] != knife {
		t.Fatalf("corpse contents = %v, want [the knife] (loot did not transfer)", corpse.contents)
	}
	// The corpse name derives from the victim and includes the "corpse" keyword (so `get all corpse`
	// resolves it).
	if !contains([]string{corpse.Name()}, "goblin") {
		t.Fatalf("corpse name = %q, want it to mention the victim", corpse.Name())
	}
}

// TestOnVitalDepletedIdempotent pins the posDead latch (distsys review S1): a SECOND depletion of an
// already-dead victim — the case a DoT/affect tick or any non-swing source will create — must NO-OP,
// not double-fire OnKill or build a duplicate corpse. die() sets posDead; onVitalDepleted checks it.
func TestOnVitalDepletedIdempotent(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("killer", killShotProfile(100))
	s.entity.living.combatRef = "killer"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})

	mob := combatMob(z, s.entity, "goblin", "", 10)
	room := mob.location
	z.startFight(s.entity, mob)
	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())

	countCorpses := func() int {
		n := 0
		for _, e := range room.contents {
			if _, ok := Get[*Container](e); ok {
				n++
			}
		}
		return n
	}
	if countCorpses() != 1 {
		t.Fatalf("after the kill: %d corpses, want 1", countCorpses())
	}
	if position(mob) != posDead {
		t.Fatalf("dead mob position = %v, want posDead (the idempotency latch)", position(mob))
	}

	// A second depletion (stand-in for a DoT tick landing on the already-dead mob) must be a clean no-op.
	z.onVitalDepleted(mob, s.entity, nil)
	if got := countCorpses(); got != 1 {
		t.Fatalf("second onVitalDepleted built a duplicate corpse: %d corpses, want 1 (latch failed)", got)
	}
}

// TestKillGoblinThroughDemoPipelineDropsLootableCorpse is the END-TO-END 6.3b milestone driven by the
// ACTUAL demo pack: spawn the goblin (armed with its rusty knife via the reset), `kill` it through the
// 6.3a pipeline across rounds, and assert it dies, drops a corpse holding the knife, and the player can
// `get all corpse` then carry the knife. All from content — the engine names no goblin/corpse/loot.
func TestKillGoblinThroughDemoPipelineDropsLootableCorpse(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	z.testCombatRng = rand.New(rand.NewSource(7))

	hollow := z.rooms["darkwood:room:hollow"]
	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Hero")
	Move(s.entity, hollow)
	z.players["Hero"] = s
	setAttrBase(s.entity, "strength", 18)
	setAttrBase(s.entity, "accuracy", 20) // reliably connect
	setAttrBase(s.entity, "attacks", 5)   // out-damage the goblin's regen so the fight resolves
	setAttrBase(s.entity, "damroll", 20)  // hit hard so it dies within the round budget
	equipWeapon(s.entity, &Weapon{diceNum: 2, diceSize: 6, damageType: "slash"})

	var goblin *Entity
	for _, e := range hollow.contents {
		if e.proto == "darkwood:mob:goblin" {
			goblin = e
		}
	}
	if goblin == nil {
		t.Fatal("the reset did not spawn the goblin")
	}
	// The reset armed the goblin with its rusty knife (into the mob's inventory).
	var hasKnife bool
	for _, e := range goblin.contents {
		if e.proto == "darkwood:obj:rusty-knife" {
			hasKnife = true
		}
	}
	if !hasKnife {
		t.Fatal("the reset did not arm the goblin with its rusty knife (corpse would be empty)")
	}

	ctx := &Context{z: z, s: s, Actor: s.entity, arg: "goblin"}
	if err := cmdKill(ctx); err != nil {
		t.Fatalf("cmdKill: %v", err)
	}

	// Run rounds until the goblin dies (its corpse appears). Bounded so a non-resolving fight fails.
	var corpse *Entity
	for round := 0; round < 40 && corpse == nil; round++ {
		for i := uint64(0); i < PULSE_VIOLENCE; i++ {
			z.pulses.tick()
		}
		for _, e := range hollow.contents {
			if _, ok := Get[*Container](e); ok {
				corpse = e
			}
		}
	}
	if corpse == nil {
		t.Fatal("the goblin never died / no corpse dropped across 40 rounds")
	}
	// The goblin entity is gone, and the hero is no longer fighting (its target died).
	for _, e := range hollow.contents {
		if e == goblin {
			t.Fatal("dead goblin still in the room")
		}
	}
	if s.entity.living.fighting != nil {
		t.Fatal("hero still fighting a dead/removed goblin (dangling fighting pointer)")
	}
	// The corpse holds the rusty knife — loot it via the player command path (`get all corpse`).
	getCtx := &Context{z: z, s: s, Actor: s.entity, arg: "all corpse"}
	if err := cmdGet(getCtx); err != nil {
		t.Fatalf("cmdGet: %v", err)
	}
	var looted bool
	for _, e := range s.entity.contents {
		if e.proto == "darkwood:obj:rusty-knife" {
			looted = true
		}
	}
	if !looted {
		t.Fatal("`get all corpse` did not transfer the knife to the hero (corpse not lootable)")
	}
}

// TestFreshUnarmedPlayerKillsHollowGoblin is the STARTER-COMBAT acceptance gate: a FRESH player (demo
// default stats, NO equipped weapon, NO stat boosts) must be able to punch the hollow goblin to death in
// a reasonable number of rounds without dying. This is the realistic melee the e2e death sequence drives
// (`kill goblin`). It guards the three starter-combat fixes together: (1) the unarmed swing now does real
// damage (unarmed_dice_* content + the engine fallback), (2) the goblin is retuned to a low-hp starter,
// (3) the engine pauses the goblin's hp regen mid-fight so it no longer claws back the player's damage.
// Before these, a bare-handed `kill goblin` ran for minutes and never resolved.
func TestFreshUnarmedPlayerKillsHollowGoblin(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	z.testCombatRng = rand.New(rand.NewSource(7))

	hollow := z.rooms["darkwood:room:hollow"]
	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Hero") // a FRESH player: demo defaults (str 10), no weapon, no boosts
	Move(s.entity, hollow)
	z.players["Hero"] = s

	// Sanity: the player is genuinely unarmed (no wielded weapon, no natural Weapon component) and at
	// default strength — so this proves the UNARMED fallback, not a buffed swing.
	if wieldedWeapon(s.entity) != nil {
		t.Fatal("fresh player is not unarmed (test would not exercise the unarmed swing)")
	}
	if got := attr(s.entity, "strength"); got != 10 {
		t.Fatalf("fresh player strength = %v, want the demo default 10", got)
	}

	var goblin *Entity
	for _, e := range hollow.contents {
		if e.proto == "darkwood:mob:goblin" {
			goblin = e
		}
	}
	if goblin == nil {
		t.Fatal("the reset did not spawn the hollow goblin")
	}
	startHP := resourceCurrent(goblin, "hp")
	t.Logf("hollow goblin starting hp = %d", startHP)

	ctx := &Context{z: z, s: s, Actor: s.entity, arg: "goblin"}
	if err := cmdKill(ctx); err != nil {
		t.Fatalf("cmdKill: %v", err)
	}

	// Run rounds until the goblin dies. The bound is the "reasonable number of rounds" contract: a fresh
	// unarmed player should fell the starter goblin well within it (measured median ~6 rounds, worst-case
	// ~13 over 60 seeds; 20 leaves headroom against an unlucky to-hit streak without going flaky). If the
	// fight does not resolve here the starter tuning regressed — regen clawback, over-tuned hp, or ~0
	// unarmed damage all manifest as a fight that never ends.
	const maxRounds = 20
	rounds := 0
	died := false
	for ; rounds < maxRounds && !died; rounds++ {
		for i := uint64(0); i < PULSE_VIOLENCE; i++ {
			z.pulses.tick()
		}
		for _, e := range hollow.contents {
			if _, ok := Get[*Container](e); ok {
				died = true
			}
		}
	}
	if !died {
		t.Fatalf("fresh unarmed player did NOT kill the hollow goblin within %d rounds "+
			"(goblin start hp %d) — starter combat is unplayable", maxRounds, startHP)
	}
	// The player survived (it is no longer fighting because its target died, and it is not dead itself).
	if !canAct(s.entity) {
		t.Fatal("the player died winning the fight — the starter goblin is too dangerous")
	}
	t.Logf("fresh unarmed player killed the hollow goblin in %d round(s)", rounds)
}

// TestLookCorpseShowsContents proves `look corpse` reveals the loot before looting.
func TestLookCorpseShowsContents(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("killer", killShotProfile(100))
	s.entity.living.combatRef = "killer"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "goblin", "", 5)
	knife := z.newEntity(ProtoRef("test:knife"))
	knife.short = "a rusty knife"
	knife.setKeywords([]string{"knife"})
	Move(knife, mob)
	z.startFight(s.entity, mob)
	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	drainCombat(s)

	ctx := &Context{z: z, s: s, Actor: s.entity, arg: "corpse"}
	if err := cmdLook(ctx); err != nil {
		t.Fatalf("cmdLook: %v", err)
	}
	if out := drainCombat(s); !contains(out, "rusty knife") {
		t.Fatalf("look corpse did not list the knife, got %v", out)
	}
}

// --- OnKill fires ------------------------------------------------------------------------------

// TestOnKillFiresWithKillerAsSubject proves the death path fires evOnKill with subject=KILLER (so a
// content XP/quest handler on the killer reacts). A resource on the killer with an OnKill handler that
// adds "honor" proves the subject binding + the fire point.
func TestOnKillFiresWithKillerAsSubject(t *testing.T) {
	z, s := combatZone(t)
	// An "honor" resource the killer HAS, with an OnKill handler that adds 7.
	z.defs.attr.register("max_honor", &attributeDef{ref: "max_honor", base: litNode{v: 100}})
	z.defs.res.register("honor", &resourceDef{
		ref: "honor", maxAttr: "max_honor",
		onEvent: map[eventKind][]effectOp{
			evOnKill: {{kind: "modify_resource", resource: "honor", amount: 7, tgt: "self"}},
		},
	})
	z.defs.combat.register("killer", killShotProfile(100))
	s.entity.living.combatRef = "killer"
	setResourceCurrent(s.entity, "honor", 0)
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "goblin", "", 5)
	z.startFight(s.entity, mob)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	if got := resourceCurrent(s.entity, "honor"); got != 7 {
		t.Fatalf("honor after OnKill = %d, want 7 (OnKill fired with the killer as subject)", got)
	}
}

// TestOnDepletedHookRunsBeforeDeath proves the content on_depleted op-list runs on the dying entity
// before die(). A "second wind" hook that heals the victim back above 0 ABORTS the death (corpse not
// dropped) — proving on_depleted runs first and can genuinely prevent death as pure content.
func TestOnDepletedHookRunsBeforeDeath(t *testing.T) {
	z, s := combatZone(t)
	// hp's on_depleted heals the victim to 50 — a content "second wind" that prevents death.
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onDepleted: []effectOp{{kind: "modify_resource", resource: "hp", amount: 50, tgt: "self"}},
	})
	z.defs.combat.register("killer", killShotProfile(100))
	s.entity.living.combatRef = "killer"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "goblin", "", 5)
	room := mob.location
	z.startFight(s.entity, mob)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	// The hook healed the mob: it lives, no corpse dropped.
	if resourceCurrent(mob, "hp") <= 0 {
		t.Fatalf("on_depleted heal did not take effect; mob hp = %d", resourceCurrent(mob, "hp"))
	}
	for _, e := range room.contents {
		if _, ok := Get[*Container](e); ok {
			t.Fatalf("a corpse dropped despite the on_depleted second-wind preventing death")
		}
	}
	if position(mob) == posDead {
		t.Fatalf("mob is posDead despite surviving via on_depleted")
	}
}

// --- Player death respawn ----------------------------------------------------------------------

// TestPlayerDeathRespawns proves a player killed in combat is moved to the start room, restored to a
// living position with full vitals, and never left wedged dead. Driven mob -> player.
func TestPlayerDeathRespawns(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	z.testCombatRng = rand.New(rand.NewSource(1))

	// A player in a non-start room (the market), so respawn must MOVE them.
	market := z.rooms["midgaard:room:market"]
	s := &session{character: "Victim", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Victim")
	s.entity.living.combatRef = "" // no avoidance ladder on the victim, so the killing swing lands
	Move(s.entity, market)
	z.players["Victim"] = s
	setResourceCurrent(s.entity, "hp", 5)

	// A mob that hits hard enough to kill the player in one swing.
	z.defs.combat.register("brute", killShotProfile(100))
	mob := z.newEntity(ProtoRef("test:brute"))
	mob.short = "a brute"
	mob.setKeywords([]string{"brute"})
	Add(mob, &Living{combatRef: "brute"})
	Move(mob, market)
	Add(mob, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	z.startFight(mob, s.entity)

	z.resolveSwing(mob, s.entity, 0, rand.New(rand.NewSource(1)), newBudget())

	// The player is at the start room (temple), alive, full hp, standing.
	if s.entity.location != z.rooms[z.startRoom] {
		t.Fatalf("dead player not moved to the start room (at %v)", targetShort(s.entity.location))
	}
	if position(s.entity) == posDead {
		t.Fatalf("respawned player is still posDead (wedged)")
	}
	if got := resourceCurrent(s.entity, "hp"); got != resourceMax(s.entity, "hp") {
		t.Fatalf("respawned player hp = %d, want full %d", got, resourceMax(s.entity, "hp"))
	}
	if s.entity.living.fighting != nil {
		t.Fatalf("respawned player still has a fighting pointer")
	}
	// The mob's fighting pointer at the (departed) player was dropped (no dangling pointer).
	if mob.living.fighting != nil {
		t.Fatalf("mob still fighting the dead/respawned player (dangling pointer)")
	}
}

// --- Threat list + retargeting -----------------------------------------------------------------

// TestThreatPicksHighestThreatTarget proves a mob fighting in a room re-targets the attacker with the
// most accumulated threat. Two attackers hit the mob; the one dealing more damage pulls aggro.
func TestThreatPicksHighestThreatTarget(t *testing.T) {
	z, s := combatZone(t)

	// Two attackers; "Heavy" deals more (higher threat) than the original hero.
	heavy := makePlayerTargetInRoom(z, s.entity, "Heavy")

	mob := combatMob(z, s.entity, "goblin", "", 1000)
	// Manually accrue threat (the swing path's addThreat) so the test is deterministic.
	addThreat(mob, s.entity, 10)
	addThreat(mob, heavy.entity, 50)
	mob.living.fighting = s.entity // currently on the hero
	setPosition(mob, posFighting)

	if top := topThreat(mob); top != heavy.entity {
		t.Fatalf("topThreat = %v, want Heavy (more threat)", targetShort(top))
	}
	// retargetMob keeps the current valid target (no thrashing), so force the current target invalid
	// (hero leaves the room) to prove the re-target picks Heavy.
	Move(s.entity, z.newEntity(ProtoRef("elsewhere"))) // hero leaves the room
	z.retargetMob(mob)
	if mob.living.fighting != heavy.entity {
		t.Fatalf("mob did not re-target the highest-threat foe; fighting = %v", targetShort(mob.living.fighting))
	}
}

// TestThreatScrubbedOnDeath proves a dead entity leaves no threat-key *Entity in any other combatant's
// table (the no-stale-pointer invariant).
func TestThreatScrubbedOnDeath(t *testing.T) {
	z, s := combatZone(t)
	z.defs.combat.register("killer2", killShotProfile(100))
	s.entity.living.combatRef = "killer2"
	equipWeapon(s.entity, &Weapon{diceNum: 6, diceSize: 1, damageType: "slash"})
	mob := combatMob(z, s.entity, "goblin", "", 5)
	// A bystander mob that has threat toward the soon-dead goblin.
	bystander := combatMob(z, s.entity, "wolf", "", 100)
	addThreat(bystander, mob, 30)
	z.startFight(s.entity, mob)

	z.resolveSwing(s.entity, mob, 0, rand.New(rand.NewSource(1)), newBudget())
	if bystander.living.threat[mob] != 0 {
		t.Fatalf("dead mob still referenced in the bystander's threat table (stale pointer)")
	}
}

// --- assist ------------------------------------------------------------------------------------

// TestAssistAdoptsAllyTarget proves `assist <ally>` joins the ally's fight (adopts their target).
func TestAssistAdoptsAllyTarget(t *testing.T) {
	z, s := combatZone(t)
	ally := makePlayerTargetInRoom(z, s.entity, "Ally")
	mob := combatMob(z, s.entity, "goblin", "", 100)
	z.startFight(ally.entity, mob)

	ctx := &Context{z: z, s: s, Actor: s.entity, arg: "Ally"}
	if err := cmdAssist(ctx); err != nil {
		t.Fatalf("cmdAssist: %v", err)
	}
	if s.entity.living.fighting != mob {
		t.Fatalf("assist did not adopt the ally's target; fighting = %v", targetShort(s.entity.living.fighting))
	}
	if position(s.entity) != posFighting {
		t.Fatalf("assist did not put the actor into combat")
	}
}

// TestAssistNonFightingAllyMessages proves assisting an idle ally is a clean message, not a crash.
func TestAssistNonFightingAllyMessages(t *testing.T) {
	z, s := combatZone(t)
	makePlayerTargetInRoom(z, s.entity, "Ally")
	ctx := &Context{z: z, s: s, Actor: s.entity, arg: "Ally"}
	if err := cmdAssist(ctx); err != nil {
		t.Fatalf("cmdAssist: %v", err)
	}
	if s.entity.living.fighting != nil {
		t.Fatalf("assist engaged combat against an idle ally")
	}
	if out := drainCombat(s); !contains(out, "isn't fighting") {
		t.Fatalf("expected an 'isn't fighting' message, got %v", out)
	}
}

// --- consider ----------------------------------------------------------------------------------

// TestConsiderReadsCombatPower proves consider yields a difficulty verdict from content combat
// attributes: a deadly mob (huge accuracy/damroll) reads as dangerous, a weak one as easy prey.
func TestConsiderReadsCombatPower(t *testing.T) {
	z, s := combatZone(t)
	// Give the hero a baseline power so the ratio is meaningful (max_hp 100 from combatZone).
	weak := combatMob(z, s.entity, "rat", "", 1)
	setAttrBase(weak, "accuracy", -500) // far below the hero's power -> easy prey
	deadly := combatMob(z, s.entity, "dragon", "", 1)
	setAttrBase(deadly, "accuracy", 500)
	setAttrBase(deadly, "damroll", 500) // far above -> deadly

	considerVerdictOf := func(name string) string {
		ctx := &Context{z: z, s: s, Actor: s.entity, arg: name}
		if err := cmdConsider(ctx); err != nil {
			t.Fatalf("cmdConsider %s: %v", name, err)
		}
		out := drainCombat(s)
		if len(out) == 0 {
			t.Fatalf("consider %s produced no output", name)
		}
		return out[len(out)-1]
	}
	if v := considerVerdictOf("rat"); !contains([]string{v}, "prey") && !contains([]string{v}, "one hand") {
		t.Fatalf("considering a weak rat read %q, want an easy verdict", v)
	}
	if v := considerVerdictOf("dragon"); !contains([]string{v}, "Death") && !contains([]string{v}, "dangerous") {
		t.Fatalf("considering a deadly dragon read %q, want a dangerous verdict", v)
	}
	_ = weak
	_ = deadly
}

// --- aggro -------------------------------------------------------------------------------------

// TestAggressiveMobInitiatesOnEntry proves an aggressive mob (a content `aggressive` attribute) starts
// a fight when a player enters its room (death.go aggroOnEntry).
func TestAggressiveMobInitiatesOnEntry(t *testing.T) {
	z, s := combatZone(t)
	z.defs.attr.register("aggressive", &attributeDef{ref: "aggressive", base: litNode{v: 0}})

	// An aggressive mob in a separate room; the player "enters" it.
	dest := z.newEntity(ProtoRef("test:lair"))
	Add(dest, &Room{})
	mob := z.newEntity(ProtoRef("test:aggromob"))
	mob.short = "an angry boar"
	mob.setKeywords([]string{"boar"})
	Add(mob, &Living{})
	setAttrBase(mob, "aggressive", 1)
	Move(mob, dest)

	Move(s.entity, dest)
	z.aggroOnEntry(s.entity, dest)

	if mob.living.fighting != s.entity {
		t.Fatalf("aggressive mob did not initiate combat on the entrant")
	}
	if position(s.entity) != posFighting {
		t.Fatalf("entrant was not put into combat by the aggressive mob")
	}
}

// TestDemoAggressiveChiefAttacksOnMove proves the END-TO-END aggro wiring through the real move()
// path + the demo pack: a player walking into the chief's lair is attacked by the aggressive goblin
// chief (a content `aggressive: 1` attribute), all from content.
func TestDemoAggressiveChiefAttacksOnMove(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	hollow := z.rooms["darkwood:room:hollow"]
	s := &session{character: "Walker", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Walker")
	Move(s.entity, hollow)
	z.players["Walker"] = s

	var chief *Entity
	for _, e := range z.rooms["darkwood:room:lair"].contents {
		if e.proto == "darkwood:mob:goblin-chief" {
			chief = e
		}
	}
	if chief == nil {
		t.Fatal("the reset did not spawn the aggressive goblin chief in the lair")
	}

	// Walk north into the lair — the chief should initiate combat on arrival.
	z.move(s, "north")
	if chief.living.fighting != s.entity {
		t.Fatalf("the aggressive chief did not attack the entering player")
	}
	if position(s.entity) != posFighting {
		t.Fatalf("the entering player was not put into combat by the aggressive chief")
	}
}

// TestPassiveMobDoesNotInitiate proves a non-aggressive mob ignores an entrant.
func TestPassiveMobDoesNotInitiate(t *testing.T) {
	z, s := combatZone(t)
	z.defs.attr.register("aggressive", &attributeDef{ref: "aggressive", base: litNode{v: 0}})
	dest := z.newEntity(ProtoRef("test:glade"))
	Add(dest, &Room{})
	mob := z.newEntity(ProtoRef("test:passivemob"))
	mob.short = "a deer"
	Add(mob, &Living{}) // no aggressive attribute -> 0
	Move(mob, dest)

	Move(s.entity, dest)
	z.aggroOnEntry(s.entity, dest)
	if mob.living.fighting != nil || position(s.entity) == posFighting {
		t.Fatalf("a passive mob initiated combat on entry")
	}
}
