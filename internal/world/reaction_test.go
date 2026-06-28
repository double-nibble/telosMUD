package world

import (
	"math/rand"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// reaction_test.go exercises Phase 6.4b: the OnLeaveRoom CHECKPOINT [G9], the declarative OPPORTUNITY
// ATTACK (an engaged foe's granted swing on a fleeing target), and the per-round REACTION BUDGET that
// bounds it (one OA per round; a second flee the same round provokes nothing). The reaction is pure
// CONTENT — a `reactions` resource carrying on_event[OnLeaveRoom] — so these tests build that content
// directly (white-box) and drive the flee path. Determinism: a d1/size-1 OA + a seeded combat rng make
// the granted damage exact; no real timers (the round driver is ticked or topUpReactions called directly).

// reactionZone builds a bare zone with the combat attributes + the `reactions` resource and its
// OnLeaveRoom opportunity-attack handler (the demo content, expressed in code). It returns the zone and a
// fleeing player in a room that has a `north` exit to a registered destination room (so a directional
// flee can relocate). A reactor mob is added per-test in the fleer's room.
func reactionZone(t *testing.T) (*Zone, *session, *Entity) {
	t.Helper()
	z := newZone("test")
	z.id = "test"
	reg := func(ref string, base float64) {
		z.defs.attr.register(ref, &attributeDef{ref: ref, base: litNode{v: base}})
	}
	reg("strength", 10)
	reg("strength_bonus", 0) // OA bonus 0 so the granted swing is exactly the weapon dice
	reg("attacks", 1)
	reg("combat_order", 0)
	reg("max_reactions", 0) // default: nobody reacts; the reactor mob overrides to 1
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	z.defs.dmg.register("slash", &damageTypeDef{ref: "slash", resist: map[string]float64{}})

	// The `reactions` resource: per-round budget + the OnLeaveRoom opportunity-attack handler. The handler
	// gates on `if reactions >= 1`, SPENDS one, then deals a granted slash (1d1=1, +str_bonus 0 = 1) at the
	// fleer (target: other) through the gated dealDamage funnel.
	z.defs.res.register("reactions", &resourceDef{
		ref: "reactions", maxAttr: "max_reactions", perRound: true,
		onEvent: map[eventKind][]effectOp{
			evOnLeaveRoom: {{
				kind: "if", ifResource: "reactions", ifResourceMin: 1,
				then: []effectOp{
					{kind: "modify_resource", resource: "reactions", amount: -1},
					{
						kind: "deal_damage", tgt: "other", dmgType: "slash", diceNum: 1, diceSize: 1,
						bonus: attrNode{ref: "$actor.strength_bonus"},
					},
				},
			}},
		},
	})

	// The fleer's room with a north exit to a registered destination.
	fleer := makeRoomPlayer(z, "Fleer")
	from := fleer.entity.location
	from.proto = "test:room:from"
	Add(from, &Room{exits: map[string]ProtoRef{"north": "test:room:to"}})
	z.rooms["test:room:from"] = from
	dest := z.newEntity("test:room:to")
	Add(dest, &Room{})
	z.rooms["test:room:to"] = dest

	setResourceCurrent(fleer.entity, "hp", 100)
	return z, fleer, from
}

// reactorMob builds a mob that opportunity-attacks: it has max_reactions=1 (so it OWNS the reactions
// resource + its OnLeaveRoom handler) and a natural weapon. It is placed in `room` already FIGHTING the
// fleer (fighting == fleer) so it is an engaged reactor.
func reactorMob(z *Zone, room, fleer *Entity, name string) *Entity {
	e := z.newEntity(ProtoRef("test:mob"))
	e.short = name
	e.setKeywords([]string{name})
	Add(e, &Living{})
	setAttrBase(e, "max_reactions", 1)
	Add(e, &Weapon{diceNum: 1, diceSize: 1, damageType: "slash"}) // natural attack, 1 dmg
	Move(e, room)
	setResourceCurrent(e, "hp", 100)
	e.living.fighting = fleer // engaged with the fleer
	return e
}

// TestFleeProvokesOpportunityAttackOnce is THE milestone (PHASE6-PLAN §4 slice 6.4): leaving a room with
// an engaged enemy PROVOKES a declarative opportunity attack that CONSUMES the reaction budget. A second
// flee in the SAME round (before the round driver tops the budget back up) gets NO opportunity attack —
// the budget is spent. All from content (the reactions resource + its OnLeaveRoom handler).
func TestFleeProvokesOpportunityAttackOnce(t *testing.T) {
	z, fleer, from := reactionZone(t)
	z.testCombatRng = rand.New(rand.NewSource(1))
	mob := reactorMob(z, from, fleer.entity, "goblin")
	// The fleer is fighting the mob (a real two-sided fight).
	fleer.entity.living.fighting = mob
	setPosition(fleer.entity, posFighting)
	setPosition(mob, posFighting)

	// Top up the reactor's per-round budget (what the round driver does at round start): reactions = 1.
	z.topUpReactions(mob)
	if got := resourceCurrent(mob, "reactions"); got != 1 {
		t.Fatalf("reactor must start the round with 1 reaction, got %d", got)
	}

	// FIRST flee north: provokes the opportunity attack (1 slash damage) and spends the reaction.
	startHP := resourceCurrent(fleer.entity, "hp")
	ctx := &Context{z: z, s: fleer, Actor: fleer.entity, arg: "north"}
	if err := cmdFlee(ctx); err != nil {
		t.Fatalf("cmdFlee north: %v", err)
	}
	if got := resourceCurrent(fleer.entity, "hp"); got != startHP-1 {
		t.Fatalf("first flee must provoke a 1-damage opportunity attack: fleer hp %d, want %d", got, startHP-1)
	}
	if got := resourceCurrent(mob, "reactions"); got != 0 {
		t.Fatalf("the opportunity attack must SPEND the reaction: reactions %d, want 0", got)
	}
	if fleer.entity.location != z.rooms["test:room:to"] {
		t.Fatalf("directional flee must relocate the fleer to the destination room")
	}

	// Re-engage in the destination (so a SECOND flee is from an engaged state) WITHOUT topping the budget
	// up — simulating a second flee in the SAME round. Move the (still-living) mob along and re-fight.
	Move(mob, fleer.entity.location)
	mob.living.fighting = fleer.entity
	fleer.entity.living.fighting = mob
	setPosition(fleer.entity, posFighting)
	setPosition(mob, posFighting)
	// Give the destination an exit so the second flee can resolve.
	Add(fleer.entity.location, &Room{exits: map[string]ProtoRef{"south": "test:room:from"}})

	hpBefore2 := resourceCurrent(fleer.entity, "hp")
	ctx2 := &Context{z: z, s: fleer, Actor: fleer.entity, arg: "south"}
	if err := cmdFlee(ctx2); err != nil {
		t.Fatalf("cmdFlee south: %v", err)
	}
	if got := resourceCurrent(fleer.entity, "hp"); got != hpBefore2 {
		t.Fatalf("a SECOND flee the same round (budget spent) must provoke NO opportunity attack: hp %d, want %d", got, hpBefore2)
	}
}

// TestReactionBudgetRefreshesNextRound proves the per-round tie: after the budget is spent, the round
// driver's start-of-round topUpReactions refills it, so the NEXT round's flee provokes again.
func TestReactionBudgetRefreshesNextRound(t *testing.T) {
	z, fleer, from := reactionZone(t)
	z.testCombatRng = rand.New(rand.NewSource(1))
	mob := reactorMob(z, from, fleer.entity, "goblin")

	z.topUpReactions(mob)
	// Spend the reaction directly (as one OA would).
	setResourceCurrent(mob, "reactions", 0)
	if got := resourceCurrent(mob, "reactions"); got != 0 {
		t.Fatalf("precondition: reactions spent, got %d", got)
	}
	// A new round tops it back up to max (1).
	z.topUpReactions(mob)
	if got := resourceCurrent(mob, "reactions"); got != 1 {
		t.Fatalf("the per-round budget must refresh to max at round start: reactions %d, want 1", got)
	}
}

// TestOpportunityAttackGatedOnNonConsentingPlayer is the SECURITY property: a mob's opportunity attack on
// a fleeing MOB lands (PvE), but a PLAYER reactor's OA on a NON-CONSENTING player fleer is GATED (the
// granted swing funnels the SAME guardHarmful every harm vector does — no PvP-gate bypass via a reaction).
func TestOpportunityAttackGatedOnNonConsentingPlayer(t *testing.T) {
	z, fleer, from := reactionZone(t)
	z.testCombatRng = rand.New(rand.NewSource(1))

	// A PLAYER reactor (not a mob) engaged with the player fleer, holding a reaction + a weapon.
	reactor := makePlayerTargetInRoom(z, fleer.entity, "Reactor")
	setAttrBase(reactor.entity, "max_reactions", 1)
	Add(reactor.entity, &Weapon{diceNum: 1, diceSize: 1, damageType: "slash"})
	setResourceCurrent(reactor.entity, "hp", 100)
	reactor.entity.living.fighting = fleer.entity
	fleer.entity.living.fighting = reactor.entity
	setPosition(fleer.entity, posFighting)
	setPosition(reactor.entity, posFighting)
	z.topUpReactions(reactor.entity)

	// No PvP consent set on the fleer => the reactor's OA must be a clean no-op (gated).
	startHP := resourceCurrent(fleer.entity, "hp")
	ctx := &Context{z: z, s: fleer, Actor: fleer.entity, arg: "north"}
	if err := cmdFlee(ctx); err != nil {
		t.Fatalf("cmdFlee north: %v", err)
	}
	if got := resourceCurrent(fleer.entity, "hp"); got != startHP {
		t.Fatalf("a non-consenting player must NOT be hit by an opportunity attack (PvP gate): hp %d, want %d", got, startHP)
	}

	// With PvP consent on both sides, the SAME OA now lands.
	_ = from
	z2, fleer2, from2 := reactionZone(t)
	z2.testCombatRng = rand.New(rand.NewSource(1))
	reactor2 := makePlayerTargetInRoom(z2, fleer2.entity, "Reactor")
	setAttrBase(reactor2.entity, "max_reactions", 1)
	Add(reactor2.entity, &Weapon{diceNum: 1, diceSize: 1, damageType: "slash"})
	setResourceCurrent(reactor2.entity, "hp", 100)
	reactor2.entity.living.fighting = fleer2.entity
	fleer2.entity.living.fighting = reactor2.entity
	setPosition(fleer2.entity, posFighting)
	setPosition(reactor2.entity, posFighting)
	z2.topUpReactions(reactor2.entity)
	setFlag(reactor2.entity, flagPvP, true)
	setFlag(fleer2.entity, flagPvP, true)
	_ = from2

	startHP2 := resourceCurrent(fleer2.entity, "hp")
	ctx2 := &Context{z: z2, s: fleer2, Actor: fleer2.entity, arg: "north"}
	if err := cmdFlee(ctx2); err != nil {
		t.Fatalf("cmdFlee north (consented): %v", err)
	}
	if got := resourceCurrent(fleer2.entity, "hp"); got != startHP2-1 {
		t.Fatalf("a consented player OA must land: hp %d, want %d", got, startHP2-1)
	}
}

// TestBareFleeStaysInPlaceNoProvoke pins the 6.3a contract: a BARE `flee` (no direction) still disengages
// IN PLACE with no room change and no opportunity attack — the directional-flee provoke is opt-in.
func TestBareFleeStaysInPlaceNoProvoke(t *testing.T) {
	z, fleer, from := reactionZone(t)
	z.testCombatRng = rand.New(rand.NewSource(1))
	mob := reactorMob(z, from, fleer.entity, "goblin")
	fleer.entity.living.fighting = mob
	setPosition(fleer.entity, posFighting)
	setPosition(mob, posFighting)
	z.topUpReactions(mob)

	startHP := resourceCurrent(fleer.entity, "hp")
	ctx := &Context{z: z, s: fleer, Actor: fleer.entity} // no arg => bare flee
	if err := cmdFlee(ctx); err != nil {
		t.Fatalf("cmdFlee: %v", err)
	}
	if fleer.entity.location != from {
		t.Fatal("a bare flee must not relocate the fleer")
	}
	if position(fleer.entity) == posFighting || fleer.entity.living.fighting != nil {
		t.Fatal("a bare flee must drop the fleer out of combat")
	}
	if got := resourceCurrent(fleer.entity, "hp"); got != startHP {
		t.Fatalf("a bare flee must NOT provoke an opportunity attack: hp %d, want %d", got, startHP)
	}
	if got := resourceCurrent(mob, "reactions"); got != 1 {
		t.Fatalf("a bare flee must not spend the reactor's reaction: reactions %d, want 1", got)
	}
}

// TestDemoGoblinChiefOpportunityAttack drives the ACTUAL demo pack: the goblin chief (max_reactions: 1 +
// the `reactions` resource's OnLeaveRoom handler, both authored in demo.yaml) lands a free swing on a
// player who `flee`s the lair while engaged — proving the [G9] opportunity-attack milestone is exercised
// entirely by CONTENT (the engine names no opportunity attack). The chief's reaction is then spent, so a
// hypothetical second flee the same round would provoke nothing.
func TestDemoGoblinChiefOpportunityAttack(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	z.testCombatRng = rand.New(rand.NewSource(7))

	lair := z.rooms["darkwood:room:lair"]
	var chief *Entity
	for _, e := range lair.contents {
		if e.proto == "darkwood:mob:goblin-chief" {
			chief = e
		}
	}
	if chief == nil {
		t.Fatal("the reset did not spawn the goblin chief")
	}
	// The chief must own the per-round reaction budget from content (max_reactions: 1).
	if got := resourceMax(chief, "reactions"); got != 1 {
		t.Fatalf("the demo chief must have max_reactions 1 (the OA budget), got %d", got)
	}

	s := &session{character: "Hero", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Hero")
	Move(s.entity, lair)
	z.players["Hero"] = s
	setResourceCurrent(s.entity, "hp", 100)

	// Engage the chief (the chief retaliates), then top up its per-round budget (a round-start would).
	ctx := &Context{z: z, s: s, Actor: s.entity, arg: "chief"}
	if err := cmdKill(ctx); err != nil {
		t.Fatalf("cmdKill chief: %v", err)
	}
	z.topUpReactions(chief)
	if chief.living.fighting != s.entity {
		t.Fatal("the chief must retaliate (be engaged with the fleer) for the OA to fire")
	}

	// Flee south (the lair -> hollow exit): the chief's content OnLeaveRoom handler lands a free swing.
	startHP := resourceCurrent(s.entity, "hp")
	fctx := &Context{z: z, s: s, Actor: s.entity, arg: "south"}
	if err := cmdFlee(fctx); err != nil {
		t.Fatalf("cmdFlee south: %v", err)
	}
	if got := resourceCurrent(s.entity, "hp"); got >= startHP {
		t.Fatalf("fleeing the engaged chief must provoke a content opportunity attack: hp %d, want < %d", got, startHP)
	}
	if got := resourceCurrent(chief, "reactions"); got != 0 {
		t.Fatalf("the chief's opportunity attack must spend its reaction: reactions %d, want 0", got)
	}
	if s.entity.location != z.rooms["darkwood:room:hollow"] {
		t.Fatal("the directional flee must relocate the hero to the hollow")
	}
}

// TestRootedCannotFlee proves a `prevents: move` affect (web/root) blocks the panic-flee (you can't run
// while held), so the OnLeaveRoom checkpoint never fires and no reaction is spent.
func TestRootedCannotFlee(t *testing.T) {
	z, fleer, from := reactionZone(t)
	z.defs.affect.register("root", &affectDef{ref: "root", name: "Rooted", prevents: []string{"move"}, duration: 12})
	mob := reactorMob(z, from, fleer.entity, "goblin")
	fleer.entity.living.fighting = mob
	setPosition(fleer.entity, posFighting)
	setPosition(mob, posFighting)
	z.topUpReactions(mob)
	applyAffect(fleer.entity, "root", attachOpts{}, nil)

	startHP := resourceCurrent(fleer.entity, "hp")
	ctx := &Context{z: z, s: fleer, Actor: fleer.entity, arg: "north"}
	if err := cmdFlee(ctx); err != nil {
		t.Fatalf("cmdFlee north: %v", err)
	}
	if fleer.entity.location != from {
		t.Fatal("a rooted fleer must not relocate")
	}
	if got := resourceCurrent(mob, "reactions"); got != 1 {
		t.Fatalf("a blocked flee must not provoke / spend a reaction: reactions %d, want 1", got)
	}
	if got := resourceCurrent(fleer.entity, "hp"); got != startHP {
		t.Fatalf("a blocked flee must not take OA damage: hp %d, want %d", got, startHP)
	}
}
