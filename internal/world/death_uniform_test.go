package world

import (
	"math/rand"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// death_uniform_test.go exercises Phase 6.5: UNIFORM, CANCELLABLE DEATH. Before 6.5 death was reached
// from EXACTLY ONE place — a weapon swing's applySwingDamage. A pure-caster's spell/AoE/DoT could empty
// a victim's vital pool but NEVER kill. 6.5 moves the depletion->death seam INTO the shared dealDamage
// funnel (effect_op.go), so EVERY content damage source kills through one path, and adds a CANCELLABLE
// checkpoint: an on_depleted hook that revives the victim aborts the death (a death-ward). These tests
// drive the funnel directly (white-box) — no swing — to prove non-swing damage now kills.

// roomCorpse returns the first container (a corpse) in room, or nil. Death drops a corpse for a mob.
func roomCorpse(room *Entity) *Entity {
	for _, e := range room.contents {
		if _, ok := Get[*Container](e); ok {
			return e
		}
	}
	return nil
}

// hasAffectRef reports whether e currently carries an active affect with the given ref.
func hasAffectRef(e *Entity, ref string) bool {
	a := affectedComponent(e)
	if a == nil {
		return false
	}
	for _, inst := range a.list {
		if inst != nil && inst.def != nil && inst.def.ref == ref {
			return true
		}
	}
	return false
}

// dealRaw runs a raw deal_damage through the SHARED funnel with `attacker` as the source (the killer
// attribution) and `target` as the victim — exactly the path a spell's deal_damage op takes, with NO
// swing pipeline involved. Returns the applied amount.
func dealRaw(z *Zone, attacker, target *Entity, raw float64, dmgType string) int {
	c := &effectCtx{
		z: z, actor: attacker, source: attacker, target: target,
		mag: 1, disp: dispHarmful, rng: rand.New(rand.NewSource(1)),
	}
	return dealDamage(c, target, raw, dmgType, "")
}

// --- Spell / AoE kill (the core 6.5 gap) -------------------------------------------------------

// TestSpellKillDropsCorpse proves a NON-SWING deal_damage (a spell/AoE) that empties a mob's vital pool
// triggers death -> a corpse, exactly as a swing would. This is the pure-caster killing blow that 6.4a's
// fireball could NOT land before 6.5.
func TestSpellKillDropsCorpse(t *testing.T) {
	z, s := combatZone(t)
	mob := combatMob(z, s.entity, "goblin", "", 8)
	room := mob.location

	dealt := dealRaw(z, s.entity, mob, 50, "slash") // 50 >> 8 hp: a one-shot spell
	if dealt <= 0 {
		t.Fatalf("spell dealt %d damage, want > 0", dealt)
	}
	if position(mob) != posDead {
		t.Fatalf("mob position = %v after a lethal spell, want posDead", position(mob))
	}
	if roomCorpse(room) == nil {
		t.Fatalf("a lethal SPELL (non-swing) did not drop a corpse — death is still swing-only")
	}
	for _, e := range room.contents {
		if e == mob {
			t.Fatalf("dead mob still in the room after a spell kill")
		}
	}
}

// TestSpellKillRespawnsPlayer proves a non-swing deal_damage that empties a PLAYER's vital pool respawns
// them at the start room (the player twin of the corpse path), through the shared funnel.
func TestSpellKillRespawnsPlayer(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	z.combatRand = rand.New(rand.NewSource(1))

	market := z.rooms["midgaard:room:market"]
	s := &session{character: "Victim", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Victim")
	Move(s.entity, market)
	z.players["Victim"] = s
	setResourceCurrent(s.entity, "hp", 5)

	// A caster mob in the room is the source of the lethal spell (so OnKill has a subject + PvE is allowed).
	caster := z.newEntity(ProtoRef("test:caster"))
	caster.short = "a dark mage"
	Add(caster, &Living{})
	Move(caster, market)

	dealt := dealRaw(z, caster, s.entity, 100, "fire") // overkill spell, no weapon involved
	if dealt <= 0 {
		t.Fatalf("spell on player dealt %d, want > 0", dealt)
	}
	if s.entity.location != z.rooms[z.startRoom] {
		t.Fatalf("player killed by a SPELL not respawned to the start room (at %v)", targetShort(s.entity.location))
	}
	if position(s.entity) == posDead {
		t.Fatalf("player killed by a spell left wedged posDead (no respawn)")
	}
	if got := resourceCurrent(s.entity, "hp"); got != resourceMax(s.entity, "hp") {
		t.Fatalf("respawned player hp = %d, want full %d", got, resourceMax(s.entity, "hp"))
	}
}

// --- DoT kill ----------------------------------------------------------------------------------

// TestDoTTickKills proves an AFFECT TICK whose deal_damage empties a victim's vital pool kills through
// the shared funnel — a poison that ticks the target to 0 drops a corpse, with no swing anywhere.
func TestDoTTickKills(t *testing.T) {
	z, s := combatZone(t)
	// A poison DoT: each tick deals 5 slash. The applier (source) is the hero, so the kill is attributed
	// and OnKill has a subject.
	z.defs.affect.register("poison", &affectDef{
		ref: "poison", name: "Poison", stacking: stackRefresh, maxStacks: 1, duration: 100,
		tickInterval: 1, hasTick: true,
		tickOps: []effectOp{{kind: "deal_damage", dmgType: "slash", amount: 5}},
	})

	mob := combatMob(z, s.entity, "goblin", "", 4) // 4 hp: one 5-damage tick empties it
	room := mob.location
	applyAffect(mob, "poison", attachOpts{source: s.entity}, nil)

	// Fire the tick directly (the runtime path fireOnTick uses for a DoT). It routes deal_damage through
	// the shared funnel, which now owns the death seam.
	inst := affectedComponent(mob).list[0]
	fireOnTick(mob, inst, 1)

	if position(mob) != posDead {
		t.Fatalf("mob position = %v after a lethal DoT tick, want posDead", position(mob))
	}
	if roomCorpse(room) == nil {
		t.Fatalf("a lethal DoT TICK did not drop a corpse — DoTs still cannot kill")
	}
}

// --- OA-lethal-flee (the SC3 regression) -------------------------------------------------------

// TestFleeIntoLethalOpportunityAttackRespawnsNotTeleports is the SC3 distsys regression: a player at 1
// hp flees a room with an engaged reactor mob; the opportunity attack (a gated deal_damage, 1 damage)
// empties the fleer's vital pool. With uniform death the OA now KILLS — the fleer must RESPAWN at the
// start room, NOT be teleported to the flee destination (the move is aborted by the liveness guard once
// the lethal OA respawned the fleer). Models reaction_test.go's reactionZone/reactorMob.
func TestFleeIntoLethalOpportunityAttackRespawnsNotTeleports(t *testing.T) {
	z, fleer, from := reactionZone(t)
	z.combatRand = rand.New(rand.NewSource(1))

	// A start room the dead fleer respawns into (NOT the flee destination). reactionZone left z.startRoom
	// unset; register a temple as the start and a separate flee destination ("north" -> test:room:to).
	temple := z.newEntity("test:room:temple")
	Add(temple, &Room{})
	z.rooms["test:room:temple"] = temple
	z.startRoom = "test:room:temple"

	// The reactor mob is engaged with the fleer and will OA on the leave. Its 1d1 natural weapon deals 1.
	// Both sides must be in a real two-sided fight for the flee command + the OnLeaveRoom checkpoint.
	guard := reactorMob(z, from, fleer.entity, "guard")
	fleer.entity.living.fighting = guard
	setPosition(fleer.entity, posFighting)
	setPosition(guard, posFighting)
	z.topUpReactions(guard) // ensure the reactor has its per-round reaction budget

	// The fleer is at 1 hp: the 1-damage opportunity attack is lethal.
	setResourceCurrent(fleer.entity, "hp", 1)

	ctx := &Context{z: z, s: fleer, Actor: fleer.entity, arg: "north"}
	_ = cmdFlee(ctx)

	// The lethal OA killed the fleer DURING the leave; the flee must NOT have completed to the destination.
	if fleer.entity.location == z.rooms["test:room:to"] {
		t.Fatalf("fleer was teleported to the flee destination despite dying to the opportunity attack")
	}
	// Death respawned the fleer at the START room (temple), alive and full hp — never the flee dest.
	if fleer.entity.location != temple {
		t.Fatalf("dead fleer not at the start room; at %v", targetShort(fleer.entity.location))
	}
	if position(fleer.entity) == posDead {
		t.Fatalf("fleer left wedged posDead after the lethal OA")
	}
	if got := resourceCurrent(fleer.entity, "hp"); got != resourceMax(fleer.entity, "hp") {
		t.Fatalf("respawned fleer hp = %d, want full %d", got, resourceMax(fleer.entity, "hp"))
	}
}

// --- Death-ward CANCEL (the cancellable checkpoint) --------------------------------------------

// TestDeathWardCancelsDeath proves the user-mandated cancellable death checkpoint: a victim whose
// on_depleted hook sets hp to 1 and applies a "rooted" (can't-move) affect does NOT die — no corpse, no
// respawn, position stays living, and the victim ends at hp=1, rooted. The on_depleted re-check IS the
// declarative cancel: the hook revived the vital above 0, so onVitalDepleted aborts before die().
func TestDeathWardCancelsDeath(t *testing.T) {
	z, s := combatZone(t)
	// The "rooted" CC affect the death-ward applies — a can't-move affect via a prevents tag, no stat mod.
	z.defs.affect.register("rooted", &affectDef{
		ref: "rooted", name: "Rooted", stacking: stackRefresh, maxStacks: 1, duration: 20,
		prevents: []string{"move"},
	})
	// hp's on_depleted is the death-ward: set hp to 1 (modify_resource +1 from the clamped-0 floor) and
	// apply rooted. The re-check sees hp > 0 and CANCELS the death — pure content, no engine "cancel".
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onDepleted: []effectOp{
			{kind: "modify_resource", resource: "hp", amount: 1, tgt: "self"},
			{kind: "apply_affect", affect: "rooted", tgt: "self"},
		},
	})

	mob := combatMob(z, s.entity, "wardling", "", 3)
	room := mob.location
	mob.living.fighting = s.entity
	setPosition(mob, posFighting)

	// A lethal spell empties the pool; the on_depleted death-ward revives it to 1 and roots it.
	dealRaw(z, s.entity, mob, 50, "slash")

	if position(mob) == posDead {
		t.Fatalf("death-ward victim is posDead — the on_depleted cancel did not abort the death")
	}
	if roomCorpse(room) != nil {
		t.Fatalf("a corpse dropped despite the death-ward cancelling the death")
	}
	for _, e := range room.contents {
		if e == mob {
			goto stillPresent
		}
	}
	t.Fatalf("warded mob removed from the room despite surviving (death not cancelled)")
stillPresent:
	if got := resourceCurrent(mob, "hp"); got != 1 {
		t.Fatalf("warded mob hp = %d, want 1 (the death-ward floor)", got)
	}
	if !hasAffectRef(mob, "rooted") {
		t.Fatalf("warded mob is not rooted — the death-ward's can't-move affect did not apply")
	}
	if p := position(mob); p != posStanding && p != posFighting {
		t.Fatalf("warded mob position = %v, want a living position (standing/fighting)", p)
	}
}

// --- Recursion bound (security M1: the stack-overflow guard) -----------------------------------

// TestRecursiveOnDepletedTerminates is the M1 regression: an on_depleted that RE-DEALS lethal damage to
// the dying entity (a buggy/malicious death hook) must TERMINATE — bounded at maxEventDepth by the death
// seam (death.go runDeathHook) — not recurse forever into a process-fatal stack overflow. The hook can't
// save the victim (it only does more damage), so the entity ends DEAD. Reaching the assertions at all is
// the proof: an unbounded cycle would have crashed the whole process before any assert ran.
func TestRecursiveOnDepletedTerminates(t *testing.T) {
	z, s := combatZone(t)
	// hp's on_depleted re-deals 50 lethal damage to SELF — the runaway the auditor reproduced. Each level
	// empties the (already-0, re-revived-to-0?) pool and would call back into onVitalDepleted forever
	// without the depth cap. The hook never raises the vital, so the victim stays depleted -> dies.
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onDepleted: []effectOp{{kind: "deal_damage", dmgType: "slash", amount: 50, tgt: "self"}},
	})
	mob := combatMob(z, s.entity, "doomling", "", 10)
	room := mob.location

	// Kill it through the shared funnel. If the seam were unbounded this line would stack-overflow the
	// process; with the depth cap it returns and the mob is dead.
	dealRaw(z, s.entity, mob, 50, "slash")

	if position(mob) != posDead {
		t.Fatalf("recursive on_depleted victim is not posDead (the hook only re-damaged; it must die)")
	}
	if roomCorpse(room) == nil {
		t.Fatalf("recursive on_depleted did not resolve to a death/corpse (cascade should truncate, then die)")
	}
	// CARDINALITY, not just existence (#69). This test registered the exact fixture that produced a
	// re-entrant die(), and passed anyway for a long time: roomCorpse returns the FIRST container it finds,
	// and `!= nil` is satisfied identically by one corpse or by nine. Termination was proven; exactly-once
	// was not — and the missing half WAS an item dupe (N deaths => N resolveLoot calls). A recursion-bound
	// test must assert idempotency on the same fixture, or a double-fire hides behind "it terminated and a
	// corpse exists".
	corpses := 0
	for _, e := range room.contents {
		if _, ok := Get[*Container](e); ok {
			corpses++
		}
	}
	if corpses != 1 {
		t.Fatalf("re-entrant on_depleted produced %d corpses, want exactly 1 (double die() = double loot = item dupe)", corpses)
	}
	if got := deathGen(mob); got != 1 {
		t.Fatalf("the mob died %d times via the recursive on_depleted hook, want exactly 1", got)
	}
}

// TestPingPongOnDepletedTerminates is the two-entity M1 variant: A's on_depleted deals lethal damage to
// `other` (its killer), whose own on_depleted deals lethal damage back — a cross-entity ping-pong that,
// unbounded, recurses forever. The shared eventBudget + depth cap at the death seam must terminate it.
// Reaching the post-call assertions proves the bound holds (no stack overflow).
func TestPingPongOnDepletedTerminates(t *testing.T) {
	z, s := combatZone(t)
	// Both entities share the one hp def whose on_depleted deals lethal damage to `other` (the killer).
	// A dying -> hits its killer B -> B dies -> hits its killer A -> ... bounded at maxEventDepth.
	z.defs.res.register("hp", &resourceDef{
		ref: "hp", maxAttr: "max_hp", vital: true,
		onDepleted: []effectOp{{kind: "deal_damage", dmgType: "slash", amount: 50, tgt: "other"}},
	})
	a := combatMob(z, s.entity, "alpha", "", 10)
	b := combatMob(z, s.entity, "beta", "", 10)

	// B kills A; A's on_depleted retaliates at `other` (= B), kicking off the ping-pong. The call must
	// return (terminate) rather than overflow the stack.
	c := &effectCtx{
		z: z, actor: b, source: b, target: a, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(1)),
	}
	dealDamage(c, a, 50, "slash", "")

	// Both ended dead (the hooks only damage; neither revives). The point of the test is that we GET HERE.
	if position(a) != posDead {
		t.Fatalf("ping-pong: alpha not posDead")
	}
	// beta may or may not be dead depending on where the bounded cascade truncated, but the process must
	// have survived — reaching this assertion is the real proof. Accept either terminal state for beta.
	_ = b
}
