package world

import (
	"math/rand"
	"reflect"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// deathgen_test.go pins the #69 CROSS-RESPAWN OP-LIST GUARD and the death generation it is built on.
//
// The hazard: 6.5 made death uniform, so any `deal_damage` op can run the entire death funnel — including
// respawnPlayer — inline, and return to runOps with the victim standing at the temple on full hp. An
// op-list that killed its target on op 1 then landed op 2 on the RESPAWNED player. runOps now snapshots
// each op's target's DEATH GENERATION around the op and skips the rest of the list for anything that died.
//
// Position cannot express this: respawnPlayer clears posDead inside the same call stack, so on return a
// respawned player is indistinguishable from an untouched one by position/hp. Every test below therefore
// asserts on the generation, or on an effect that must NOT have landed post-respawn.

// deathGenZone builds a demo zone (real start room, real hp/max_hp defs, a real respawn destination) and a
// consenting attacker. The demo pack's start room is where respawnPlayer lands the victim.
func deathGenZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	z := newDemoZone("midgaard", newProtoCache())
	s := &session{character: "Attacker", out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(s, "Attacker")
	Move(s.entity, z.rooms[z.startRoom])
	z.players["Attacker"] = s
	setFlag(s.entity, flagPvP, true) // consent, so the harm gate is never what stops an op
	return z, s
}

// deathGenVictim adds a consenting player victim in the attacker's room, at `hp`.
func deathGenVictim(z *Zone, attacker *Entity, name string, hp int) *session {
	v := makePlayerTargetInRoom(z, attacker, name)
	setFlag(v.entity, flagPvP, true)
	setResourceCurrent(v.entity, "hp", hp)
	return v
}

func deathGenCtx(z *Zone, actor, target, other *Entity) *effectCtx {
	return &effectCtx{
		z: z, actor: actor, source: actor, target: target, other: other,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
}

// TestRunOpsSkipsRemainingOpsOnRespawnedTarget is THE #69 regression. `[deal_damage lethal, set_flag,
// deal_damage 5]` on a player: op 1 kills and respawns them; ops 2 and 3 must NOT touch the entity that
// walked out of the temple. Pre-fix the flag landed and the 5 damage was dealt to the fresh body.
func TestRunOpsSkipsRemainingOpsOnRespawnedTarget(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 10)
	maxHP := resourceMax(victim.entity, "hp")
	genBefore := deathGen(victim.entity)

	c := deathGenCtx(z, attacker.entity, victim.entity, nil)
	runOps(c, []effectOp{
		{kind: "deal_damage", amount: 999},
		{kind: "set_flag", flag: "rooted"},
		{kind: "deal_damage", amount: 5},
	})

	if got := deathGen(victim.entity); got != genBefore+1 {
		t.Fatalf("victim should have died exactly once: death generation %d, want %d", got, genBefore+1)
	}
	if position(victim.entity) != posStanding {
		t.Fatalf("respawnPlayer should leave the victim standing, got position %v", position(victim.entity))
	}
	// The whole point: the debuff and the follow-up damage were aimed at a corpse and must be dropped,
	// not re-aimed at the respawned player.
	if hasFlag(victim.entity, "rooted") {
		t.Fatal("op 2 (set_flag) landed on the RESPAWNED player — the cross-respawn guard did not skip it (#69)")
	}
	if got := resourceCurrent(victim.entity, "hp"); got != maxHP {
		t.Fatalf("op 3 (deal_damage 5) landed on the RESPAWNED player: hp %d, want the full %d respawnPlayer restored (#69)", got, maxHP)
	}
}

// TestRunOpsGuardIsPerTargetNotABlanketAbort proves the guard SKIPS ops on the dead entity without
// swallowing the rest of the list. A kill must not silently cancel a self-buff rider or the next victim's
// share of a multi-target list — the dead-set is identity-keyed.
func TestRunOpsGuardIsPerTargetNotABlanketAbort(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 10)

	before := deathGen(victim.entity)
	c := deathGenCtx(z, attacker.entity, nil, victim.entity)
	runOps(c, []effectOp{
		{kind: "deal_damage", tgt: "other", amount: 999},  // kills the victim
		{kind: "set_flag", tgt: "other", flag: "rooted"},  // SKIPPED: aimed at the entity we just killed
		{kind: "set_flag", tgt: "self", flag: "bloodied"}, // RUNS: a different, living target
	})

	// Precondition (test-engineer review): without this, a future refactor that made op 1 silently no-op
	// would leave "rooted was skipped" passing for entirely the wrong reason.
	if got := deathGen(victim.entity); got != before+1 {
		t.Fatalf("precondition: op 1 did not kill the victim (death generation %d, want %d)", got, before+1)
	}
	if hasFlag(victim.entity, "rooted") {
		t.Fatal("an op aimed at the killed target must be skipped (#69)")
	}
	if !hasFlag(attacker.entity, "bloodied") {
		t.Fatal("an op aimed at a DIFFERENT, living target must still run — the guard is per-target, not a blanket abort")
	}
}

// TestRunOpsStopsWhenTheActorDies is the other half of the guard. A thorns proc / reflected nuke can kill
// the ACTOR mid-list; whatever they were still doing must not finish resolving from beyond the grave. Here
// op 1 kills the actor outright and op 2 is aimed at a perfectly live third party — it must not run.
func TestRunOpsStopsWhenTheActorDies(t *testing.T) {
	z, attacker := deathGenZone(t)
	bystander := deathGenVictim(z, attacker.entity, "Bystander", 100)
	setResourceCurrent(attacker.entity, "hp", 10)

	c := deathGenCtx(z, attacker.entity, nil, bystander.entity)
	runOps(c, []effectOp{
		{kind: "deal_damage", tgt: "self", amount: 999},  // the actor dies (and respawns)
		{kind: "set_flag", tgt: "other", flag: "cursed"}, // must NOT run: the actor is dead
	})

	if deathGen(attacker.entity) == 0 {
		t.Fatal("test is not exercising the guard: the actor never died")
	}
	if hasFlag(bystander.entity, "cursed") {
		t.Fatal("the op-list kept running after the ACTOR died — it must stop outright (#69)")
	}
}

// TestRunOpsGuardCatchesAKillInsideANestedBranch closes the recursion hole. A nested runOps frame (if /
// chance / check) has its own dead-set, so the guard must ALSO live in the parent's around-the-op
// comparison — otherwise `[if{then:[deal_damage lethal]}, apply_affect]` sails straight past it.
func TestRunOpsGuardCatchesAKillInsideANestedBranch(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 10)

	c := deathGenCtx(z, attacker.entity, victim.entity, nil)
	runOps(c, []effectOp{
		// `if hp >= 1` over the ctx target: deterministically true (the victim is alive), so the branch runs.
		{kind: "if", ifResource: "hp", ifResourceMin: 1, then: []effectOp{
			{kind: "deal_damage", amount: 999},
		}},
		{kind: "set_flag", flag: "rooted"},
	})

	if deathGen(victim.entity) == 0 {
		t.Fatal("test is not exercising the guard: the nested branch never killed the victim")
	}
	if hasFlag(victim.entity, "rooted") {
		t.Fatal("a kill inside a NESTED op-list must still stop the parent list's later ops on that target (#69)")
	}
}

// TestRunOpsGuardCatchesANestedKillOfOther is the mudlib review's BUG 1 regression, and the reason the
// dead-set lives on the CTX rather than on a runOps frame.
//
// opIf/opChance/opCheck all recurse into a fresh runOps frame on the SAME ctx, and a nested op can rebind
// to `other`. With a per-frame dead-set, the nested frame recorded the kill in a map that died with it,
// while the parent frame was comparing generations only on ITS bound target (the actor here) — so a later
// `target: other` op sailed through onto the respawned player. This is an ordinary content shape: a
// reflect/thorns handler is `self`/`other`-bound, and "reflect, then root the attacker" is a normal rider.
func TestRunOpsGuardCatchesANestedKillOfOther(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 10)

	// Bound target is the ACTOR (a self-buff shape); the nested branch kills `other`.
	c := deathGenCtx(z, attacker.entity, attacker.entity, victim.entity)
	runOps(c, []effectOp{
		{kind: "if", ifResource: "hp", ifResourceMin: 1, then: []effectOp{
			{kind: "deal_damage", tgt: "other", amount: 999},
		}},
		{kind: "set_flag", tgt: "other", flag: "rooted"},
	})

	if deathGen(victim.entity) == 0 {
		t.Fatal("precondition: the nested branch did not kill `other`")
	}
	if hasFlag(victim.entity, "rooted") {
		t.Fatal("a NESTED frame's kill of `other` escaped the guard — the dead-set must be cascade-scoped, not per-frame (#69)")
	}
}

// TestRunOpsGuardCatchesAnAreaKillOfOther is the mudlib review's BUG 2 — the same composition hole via the
// area loop. runOpArea binds c.target per victim, so only IT can see which victims died; if it does not
// record them into the cascade's dead-set, a later `target: other` op lands on a player the blast killed.
func TestRunOpsGuardCatchesAnAreaKillOfOther(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 10)

	c := deathGenCtx(z, attacker.entity, attacker.entity, victim.entity)
	runOps(c, []effectOp{
		{kind: "deal_damage", area: "room", amount: 999}, // the blast kills `other` among its victims
		{kind: "set_flag", tgt: "other", flag: "rooted"},
	})

	if deathGen(victim.entity) == 0 {
		t.Fatal("precondition: the area op did not kill `other`")
	}
	if hasFlag(victim.entity, "rooted") {
		t.Fatal("an AREA kill of `other` escaped the guard — runOpArea must record each victim it kills (#69)")
	}
}

// TestRunOpsAreaKillSkipsFollowupOnBoundTarget drives the area branch's own guard code with the bound
// target as the casualty, and proves the abort is still per-target: a self-op after the blast must run.
func TestRunOpsAreaKillSkipsFollowupOnBoundTarget(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 10)
	before := deathGen(victim.entity)

	c := deathGenCtx(z, attacker.entity, victim.entity, nil)
	runOps(c, []effectOp{
		{kind: "deal_damage", area: "room", amount: 999},  // AoE kills the bound victim
		{kind: "set_flag", flag: "rooted"},                // bound to the victim -> SKIPPED
		{kind: "set_flag", tgt: "self", flag: "bloodied"}, // the caster -> RUNS
	})

	if got := deathGen(victim.entity); got != before+1 {
		t.Fatalf("precondition: the area op did not kill the bound victim (generation %d)", got)
	}
	if hasFlag(victim.entity, "rooted") {
		t.Fatal("a follow-up op landed on the AoE-respawned bound target (#69)")
	}
	if !hasFlag(attacker.entity, "bloodied") {
		t.Fatal("the area branch over-aborted: a self-op after an AoE kill must still run")
	}
}

// TestRunOpsAreaSkipsAVictimTheCascadeAlreadyKilled: two area ops in one list. The second must not hit
// anyone the first already killed and respawned — otherwise a `[nova, nova]` list executes a player at the
// temple. (The guard reads the dead-set on the way IN to each area victim, not just on the way out.)
func TestRunOpsAreaSkipsAVictimTheCascadeAlreadyKilled(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 10)

	c := deathGenCtx(z, attacker.entity, nil, nil)
	runOps(c, []effectOp{
		{kind: "deal_damage", area: "room", amount: 999}, // kills the victim
		{kind: "deal_damage", area: "room", amount: 999}, // must NOT hit the respawned victim
	})

	if got := deathGen(victim.entity); got != 1 {
		t.Fatalf("the victim died %d times; the second area op re-killed a player who had already respawned (#69)", got)
	}
	if got, full := resourceCurrent(victim.entity, "hp"), resourceMax(victim.entity, "hp"); got != full {
		t.Fatalf("the second area op damaged the respawned victim: hp %d, want %d", got, full)
	}
}

// TestRunOpsOnItemTargetIsNotTruncated turns the deathGen(item)==0 primitive into behavior: an item-target
// op-list (salvage, craft) must never be spuriously truncated by the guard. A non-living target's
// generation is stable, so the around-the-op comparison always reads "did not die".
func TestRunOpsOnItemTargetIsNotTruncated(t *testing.T) {
	z, attacker := deathGenZone(t)
	item := z.newEntity(ProtoRef("test:item")) // no Living
	Move(item, attacker.entity.location)

	c := deathGenCtx(z, attacker.entity, item, nil)
	runOps(c, []effectOp{
		{kind: "deal_damage", amount: 999},              // a no-op against an item, and NOT a death
		{kind: "set_flag", tgt: "self", flag: "step_2"}, // must still run
	})

	if !hasFlag(attacker.entity, "step_2") {
		t.Fatal("an item-targeting op-list was truncated by the death guard (a non-living generation must be stable)")
	}
}

// TestDeathGenerationIsInstanceOwnedNotProtoShared is the COW property. A mob's Living is aliased to its
// prototype's until first write, so an un-COW'd `deaths++` would stamp the template and every sibling —
// exactly the class of bug cow_living_test.go was written for. Killing one goblin must leave its twin, and
// the prototype, at generation 0.
//
// NOTE: this pins the die()-SIDE COW of the bump, not the runOps skip. It passes with the runOps guard
// neutralized, by design — the two are independent properties.
func TestDeathGenerationIsInstanceOwnedNotProtoShared(t *testing.T) {
	c := newProtoCache()
	protoLiving := &Living{resCur: map[string]int{"hp": 10}}
	c.define("mob:twin", []string{"twin"}, "a twin", "A twin stands here.",
		componentSet{reflect.TypeFor[*Living](): protoLiving})

	z := newDemoZone("midgaard", c)
	room := z.rooms[z.startRoom]
	doomed, spared := z.spawn("mob:twin"), z.spawn("mob:twin")
	Move(doomed, room)
	Move(spared, room)

	z.die(doomed, nil, nil)

	if got := deathGen(doomed); got != 1 {
		t.Fatalf("the dead twin's death generation is %d, want 1", got)
	}
	if got := deathGen(spared); got != 0 {
		t.Fatalf("the SPARED twin's death generation is %d, want 0 — deaths++ wrote through the proto-aliased Living (COW bug)", got)
	}
	if protoLiving.deaths != 0 {
		t.Fatalf("the PROTOTYPE's death generation is %d, want 0 — every future repop would be born pre-dead", protoLiving.deaths)
	}
}

// TestOnVitalDepletedDoesNotDieTwice is a real dupe bug the death generation exposed and fixes. An
// on_depleted hook that re-deals lethal damage re-enters onPoolDepleted recursively. The INNERMOST frame
// (the one the depth cap stops from running a hook) reaches die() and corpses the mob. As the stack
// unwound, every outer frame still saw the vital at 0 — a corpsed mob's resCur is never restored — and
// called die() AGAIN: a second corpse, a second OnKill, and a second resolveLoot. That last one is an item
// dupe. The entry-time posDead check cannot help: those frames were already inside.
//
// The measured pre-fix behavior was NINE deaths. Exactly one corpse — and, more to the point, exactly one
// guaranteed loot drop — must result.
func TestOnVitalDepletedDoesNotDieTwice(t *testing.T) {
	z, attacker := deathGenZone(t)
	room := attacker.entity.location

	// Re-register hp with an on_depleted hook that deals lethal damage right back to the dying victim.
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onDepleted: []effectOp{{kind: "deal_damage", tgt: "self", amount: 999}},
	})
	// A guaranteed drop turns the corpse-count proxy into the assertion that actually matters: a re-entrant
	// die() calls resolveLoot once per entry, and personal loot is delivered straight to the looter — so N
	// deaths mint N torches out of nothing. That is the item dupe, asserted where it bites.
	z.defs.loot.register("goblin_loot", &lootTableDef{ref: "goblin_loot", rolls: []lootRoll{
		{kind: "guaranteed", pool: []lootEntry{{item: "midgaard:obj:torch", tier: "common"}}},
	}})

	mob := aoeMob(z, room, "goblin", 10)
	mutableLiving(mob).lootTable = "goblin_loot"
	addThreat(mob, attacker.entity, 10) // loot eligibility is threat-based
	before := len(room.contents)

	c := deathGenCtx(z, attacker.entity, mob, nil)
	runOps(c, []effectOp{{kind: "deal_damage", amount: 999}})

	if got := deathGen(mob); got != 1 {
		t.Fatalf("the mob died %d times; die() must run exactly once per depletion", got)
	}
	if got := countItems(attacker.entity, "midgaard:obj:torch"); got != 1 {
		t.Fatalf("ITEM DUPE: the looter received %d torches from one kill, want exactly 1 (re-entrant die() re-ran resolveLoot)", got)
	}
	// The mob left the room (extracted into a corpse) and exactly ONE corpse replaced it, so the room's
	// occupancy is unchanged. A double die() leaves two corpses behind.
	corpses := 0
	for _, e := range room.contents {
		if e.proto == ProtoRef("corpse") {
			corpses++
		}
	}
	if corpses != 1 {
		t.Fatalf("room holds %d corpses, want exactly 1 (a re-entrant die() duplicates the corpse AND its loot)", corpses)
	}
	if len(room.contents) != before {
		t.Fatalf("room contents %d, want %d (one mob out, one corpse in)", len(room.contents), before)
	}
}

// TestDieIsIdempotentUnderOnKillReentry covers the OTHER re-entry window, the one the onPoolDepleted
// guard cannot see. die() deliberately fires OnKill and resolves loot BEFORE it latches posDead, so the
// XP/quest handler sees the kill in context. An OnKill handler that damages the victim — a cleave, an
// "execute" rider — therefore re-enters the funnel through a 0-hp entity with posDead unset and used to
// run the whole death again. die()'s own entry latch (Living.dying) is what closes it.
func TestDieIsIdempotentUnderOnKillReentry(t *testing.T) {
	z, attacker := deathGenZone(t)
	room := attacker.entity.location

	// An OnKill handler (subject = the KILLER, so it hangs off a resource the attacker has) whose op deals
	// damage to `other` — the victim, which at that instant is at 0 hp with posDead not yet latched.
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onEvent: map[eventKind][]effectOp{
			evOnKill: {{kind: "deal_damage", tgt: "other", amount: 999}},
		},
	})
	z.defs.loot.register("goblin_loot", &lootTableDef{ref: "goblin_loot", rolls: []lootRoll{
		{kind: "guaranteed", pool: []lootEntry{{item: "midgaard:obj:torch", tier: "common"}}},
	}})

	mob := aoeMob(z, room, "goblin", 10)
	mutableLiving(mob).lootTable = "goblin_loot"
	addThreat(mob, attacker.entity, 10)

	c := deathGenCtx(z, attacker.entity, mob, nil)
	runOps(c, []effectOp{{kind: "deal_damage", amount: 999}})

	if got := deathGen(mob); got != 1 {
		t.Fatalf("die() ran %d times: an OnKill handler that re-damaged the victim re-entered the death funnel", got)
	}
	if got := countItems(attacker.entity, "midgaard:obj:torch"); got != 1 {
		t.Fatalf("ITEM DUPE via OnKill re-entry: looter received %d torches from one kill, want 1", got)
	}
	corpses := 0
	for _, e := range room.contents {
		if e.proto == ProtoRef("corpse") {
			corpses++
		}
	}
	if corpses != 1 {
		t.Fatalf("room holds %d corpses, want exactly 1 (OnKill re-entry duplicated the corpse)", corpses)
	}
}

// TestDeathGenerationSurvivesRespawn is the primitive's contract, stated on its own: after die() ->
// respawnPlayer the victim reads as a standing, full-hp entity, and ONLY the generation records that they
// died. Anything that tries to detect a mid-call-stack death by reading position or hp is broken.
func TestDeathGenerationSurvivesRespawn(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 10)

	z.die(victim.entity, attacker.entity, nil)

	if position(victim.entity) == posDead {
		t.Fatal("respawnPlayer should have cleared posDead — the premise of the generation counter")
	}
	if got, full := resourceCurrent(victim.entity, "hp"), resourceMax(victim.entity, "hp"); got != full {
		t.Fatalf("respawnPlayer should have restored hp to %d, got %d", full, got)
	}
	if got := deathGen(victim.entity); got != 1 {
		t.Fatalf("death generation %d, want 1 — it is the ONLY surviving evidence of the death", got)
	}
	if victim.entity.location != z.rooms[z.startRoom] {
		t.Fatal("respawnPlayer should have moved the victim to the start room")
	}
}

// TestDeathGenOfNonLivingIsStable pins the nil/item edge runOps leans on: a non-living target (an item, a
// nil target) reads generation 0 forever, so the around-the-op comparison always reads "did not die" and
// an item-targeting op-list (salvage, craft) is never spuriously truncated.
func TestDeathGenOfNonLivingIsStable(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	if got := deathGen(nil); got != 0 {
		t.Fatalf("deathGen(nil) = %d, want 0", got)
	}
	item := z.newEntity(ProtoRef("test:item"))
	if got := deathGen(item); got != 0 {
		t.Fatalf("deathGen(item with no Living) = %d, want 0", got)
	}
}

// TestRespawnInTheStartRoomLeavesLocationUnchanged is the premise of the two guards below, and the reason
// deathGen had to be threaded into them. respawnPlayer only calls Move when the victim is NOT already in
// the start room — so a player slain in the temple respawns IN PLACE. Location is unchanged and posDead is
// cleared, which means BOTH of the old location+position death signals read "nothing happened".
func TestRespawnInTheStartRoomLeavesLocationUnchanged(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 10)
	if victim.entity.location != z.rooms[z.startRoom] {
		t.Fatal("premise: the victim must start in the start room for this test to mean anything")
	}
	before := victim.entity.location

	z.die(victim.entity, attacker.entity, nil)

	if victim.entity.location != before {
		t.Fatal("a player slain IN the start room should respawn in place (no Move)")
	}
	if position(victim.entity) == posDead {
		t.Fatal("respawnPlayer should have cleared posDead")
	}
	if got := deathGen(victim.entity); got != 1 {
		t.Fatalf("only the death generation records the death: got %d, want 1", got)
	}
}

// TestCastAbortsWhenABeforeCastReactionKillsTheCasterInPlace is the combat review's headline finding. The
// SC2 guard at ability.go compared location + posDead; a caster felled in the START ROOM by a
// BeforeCastCommit reaction respawns in place, so both read "fine" and the cast COMMITTED from beyond the
// grave — paying costs and resolving. deathGen is the only signal that sees it.
func TestCastAbortsWhenABeforeCastReactionKillsTheCasterInPlace(t *testing.T) {
	z, caster := deathGenZone(t)
	mob := aoeMob(z, caster.entity.location, "goblin", 100)
	setResourceCurrent(caster.entity, "hp", 5)

	// A BeforeCastCommit handler on the CASTER's own hp resource kills the caster outright. Subject is the
	// caster, so it hangs off a resource the caster has.
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onEvent: map[eventKind][]effectOp{
			evBeforeCastCommit: {{kind: "deal_damage", tgt: "self", amount: 999}},
		},
	})
	def := &abilityDef{
		ref: "doombolt", name: "Doombolt", invocation: "command", words: []string{"doombolt"},
		mode: tmEnemy, disposition: dispHarmful,
		ops: []effectOp{{kind: "set_flag", flag: "resolved"}},
	}

	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))

	if deathGen(caster.entity) == 0 {
		t.Fatal("precondition: the BeforeCastCommit reaction did not kill the caster")
	}
	if caster.entity.location != z.rooms[z.startRoom] {
		t.Fatal("premise: the caster must have respawned IN PLACE (start room) for this to test the blind spot")
	}
	if hasFlag(mob, "resolved") {
		t.Fatal("the cast COMMITTED after a reaction killed the caster in place — SC2 must compare deathGen, not location (#69)")
	}
}

// TestMoveAbortsWhenALeaveReactionKillsTheMoverInPlace is the same blind spot on the M1 (move) guard: an
// opportunity attack that fells the mover in the start room used to leave location unchanged and posDead
// cleared, so the move proceeded and walked the just-respawned player back out of the temple.
func TestMoveAbortsWhenALeaveReactionKillsTheMoverInPlace(t *testing.T) {
	z, mover := deathGenZone(t)
	setResourceCurrent(mover.entity, "hp", 5)
	start := z.rooms[z.startRoom]

	// OnLeaveRoom fires about each ENGAGED foe (the reactor convention), with the leaver as `other`. So the
	// opportunity attack is a handler on the REACTOR's hp that damages `other`. A one-sided fighting link
	// is exactly the case the M1 guard exists for: move() refuses while the MOVER is posFighting, so the
	// only way to provoke here is a foe pointed at an unengaged leaver.
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onEvent: map[eventKind][]effectOp{
			evOnLeaveRoom: {{kind: "deal_damage", tgt: "other", amount: 999}},
		},
	})
	foe := aoeMob(z, start, "goblin", 100)
	mutableLiving(foe).fighting = mover.entity // one-sided link: the foe is engaged, the mover is not

	_ = z.move(mover, "north") // the demo start room has a north exit

	if deathGen(mover.entity) == 0 {
		t.Fatal("precondition: the OnLeaveRoom reaction did not kill the mover")
	}
	if mover.entity.location != start {
		t.Fatal("the move PROCEEDED after a reaction killed the mover in place — M1 must compare deathGen, not location (#69)")
	}
}

// TestDoTTickLethalSkipsRemainingTickOps drives the guard through fireOnTick's runOps entry rather than a
// hand-built call: a damage-over-time tick whose first op kills must not run the tick's later ops on the
// player who just respawned at the temple.
func TestDoTTickLethalSkipsRemainingTickOps(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 3)

	z.defs.affect.register("doom", &affectDef{
		ref: "doom", name: "Doomed", maxStacks: 1, duration: 100,
		hasTick: true, tickInterval: 1,
		tickOps: []effectOp{
			{kind: "deal_damage", amount: 999},
			{kind: "set_flag", flag: "rooted"},
		},
	})
	inst := applyAffect(victim.entity, "doom", attachOpts{source: attacker.entity}, nil)
	if inst == nil {
		t.Fatal("applyAffect returned nil")
	}

	fireOnTick(victim.entity, inst, 1)

	if deathGen(victim.entity) == 0 {
		t.Fatal("precondition: the DoT tick did not kill the victim")
	}
	if hasFlag(victim.entity, "rooted") {
		t.Fatal("a later tick op landed on the respawned player (#69 must hold at the fireOnTick entry too)")
	}
}

// TestCrossRespawnGuardThroughCastAbility is the integration-tier proof: the guard holds through the REAL
// cast spine — op parsing, targeting, the step-4 harm gate, commit, and on_resolve — not just a hand-built
// effectCtx. A consenting attacker casts `[deal_damage lethal, apply_affect poison]` at a low-hp consenting
// player. The kill respawns them; the poison must not follow them to the temple.
func TestCrossRespawnGuardThroughCastAbility(t *testing.T) {
	z, attacker := deathGenZone(t)
	victim := deathGenVictim(z, attacker.entity, "Victim", 5)

	z.defs.affect.register("poison", &affectDef{ref: "poison", name: "Poisoned", maxStacks: 1, duration: 100})
	def := &abilityDef{
		ref: "doombolt", name: "Doombolt", invocation: "command", words: []string{"doombolt"},
		mode: tmEnemy, disposition: dispHarmful,
		ops: []effectOp{
			{kind: "deal_damage", amount: 999},
			{kind: "apply_affect", affect: "poison", harmful: true},
		},
	}

	z.castAbility(attacker, def, "Victim", rand.New(rand.NewSource(1)))

	if deathGen(victim.entity) == 0 {
		t.Fatal("precondition: the cast did not kill the victim (is the harm gate blocking? both consent)")
	}
	if a, ok := Get[*Affected](victim.entity); ok && len(a.list) > 0 {
		t.Fatal("a debuff followed the cast-killed player through respawn — the guard must hold via castAbility (#69)")
	}
}
