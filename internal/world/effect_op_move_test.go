package world

import "testing"

// effect_op_move_test.go — #516 declarative movement ops (teleport / push). The demo midgaard zone gives real
// rooms + exits: temple --north--> market, temple --west--> guildhall, market --north--> darkwood:room:grove
// (CROSS-ZONE), and guildhall --enter--> crypt (an INSTANCE ENTRANCE, in room.entrances not room.exits).

func moveTestZone(t *testing.T) (*Zone, *Entity) {
	t.Helper()
	z := newDemoZone("midgaard", newProtoCache())
	s := newTestPlayerEntity(z, "Caster")
	Move(s.entity, z.rooms["midgaard:room:temple"])
	return z, s.entity
}

// TestOpTeleportToRoom: teleport relocates the target to a literal same-zone room ref.
func TestOpTeleportToRoom(t *testing.T) {
	z, actor := moveTestZone(t)
	mob := makeMobTarget(z, actor, "goblin") // spawns in the actor's room (temple)
	c := &effectCtx{z: z, actor: actor, source: actor, target: mob}

	if err := opTeleport(c, &effectOp{moveRoom: "midgaard:room:market"}); err != nil {
		t.Fatalf("opTeleport: %v", err)
	}
	if mob.location != z.rooms["midgaard:room:market"] {
		t.Fatalf("mob did not teleport to market (in %v)", mob.location)
	}
}

// TestOpTeleportToActor: `dest: actor` pulls the target into the caster's room (the summon/pull shape).
func TestOpTeleportToActor(t *testing.T) {
	z, actor := moveTestZone(t)
	mob := makeMobTarget(z, actor, "goblin")
	Move(mob, z.rooms["midgaard:room:market"]) // put the mob elsewhere
	c := &effectCtx{z: z, actor: actor, source: actor, target: mob}

	if err := opTeleport(c, &effectOp{moveDest: "actor"}); err != nil {
		t.Fatalf("opTeleport: %v", err)
	}
	if mob.location != actor.location {
		t.Fatalf("mob was not pulled to the caster's room")
	}
}

// TestOpTeleportToStart: `dest: start` sends the target to the zone login room.
func TestOpTeleportToStart(t *testing.T) {
	z, actor := moveTestZone(t)
	mob := makeMobTarget(z, actor, "goblin")
	Move(mob, z.rooms["midgaard:room:market"])
	c := &effectCtx{z: z, actor: actor, source: actor, target: mob}

	if err := opTeleport(c, &effectOp{moveDest: "start"}); err != nil {
		t.Fatalf("opTeleport: %v", err)
	}
	if mob.location != z.rooms[z.startRoom] {
		t.Fatalf("mob did not teleport to the start room")
	}
}

// TestOpTeleportUnknownRoomNoop: an unknown / cross-zone room ref is a clean no-op.
func TestOpTeleportUnknownRoomNoop(t *testing.T) {
	z, actor := moveTestZone(t)
	mob := makeMobTarget(z, actor, "goblin")
	origin := mob.location
	c := &effectCtx{z: z, actor: actor, source: actor, target: mob}

	// A cross-zone room ref (darkwood is a different zone) is not in midgaard's z.rooms.
	if err := opTeleport(c, &effectOp{moveRoom: "darkwood:room:grove"}); err != nil {
		t.Fatalf("opTeleport: %v", err)
	}
	if mob.location != origin {
		t.Fatalf("a cross-zone teleport target should be a no-op, but the mob moved")
	}
}

// TestOpPushAlongExit: push forces the target one step along the named exit.
func TestOpPushAlongExit(t *testing.T) {
	z, actor := moveTestZone(t)
	mob := makeMobTarget(z, actor, "goblin") // in temple
	c := &effectCtx{z: z, actor: actor, source: actor, target: mob}

	if err := opPush(c, &effectOp{moveDir: "north"}); err != nil { // temple --north--> market
		t.Fatalf("opPush: %v", err)
	}
	if mob.location != z.rooms["midgaard:room:market"] {
		t.Fatalf("mob was not pushed north to market (in %v)", mob.location)
	}
}

// TestOpPushRefusesInstanceEntrance: push MUST NOT shove the target through an instance-entrance door — a
// direction in room.entrances (not room.exits) is refused by construction (the #435 mint invariant).
func TestOpPushRefusesInstanceEntrance(t *testing.T) {
	z, actor := moveTestZone(t)
	mob := makeMobTarget(z, actor, "goblin")
	Move(mob, z.rooms["midgaard:room:guildhall"]) // guildhall --enter--> crypt is an INSTANCE ENTRANCE
	origin := mob.location
	c := &effectCtx{z: z, actor: actor, source: actor, target: mob}

	if err := opPush(c, &effectOp{moveDir: "enter"}); err != nil {
		t.Fatalf("opPush: %v", err)
	}
	if mob.location != origin {
		t.Fatalf("push shoved the mob through an INSTANCE ENTRANCE — it must be refused")
	}
}

// TestOpPushCrossZoneNoop: an exit whose destination is in another zone is a clean same-zone-only no-op.
func TestOpPushCrossZoneNoop(t *testing.T) {
	z, actor := moveTestZone(t)
	mob := makeMobTarget(z, actor, "goblin")
	Move(mob, z.rooms["midgaard:room:market"]) // market --north--> darkwood:room:grove (cross-zone)
	origin := mob.location
	c := &effectCtx{z: z, actor: actor, source: actor, target: mob}

	if err := opPush(c, &effectOp{moveDir: "north"}); err != nil {
		t.Fatalf("opPush: %v", err)
	}
	if mob.location != origin {
		t.Fatalf("a cross-zone push should be a no-op, but the mob moved to %v", mob.location)
	}
}

// TestMoveOpsGateNonConsentingPlayer: forcing ANOTHER player to move is harm — gated through guardHarmful.
// A mob target is ungated (moves); a non-consenting player target is a clean no-op.
func TestMoveOpsGateNonConsentingPlayer(t *testing.T) {
	z, actor := moveTestZone(t)
	victim := newTestPlayerEntity(z, "Victim")
	Move(victim.entity, z.rooms["midgaard:room:temple"])
	origin := victim.entity.location
	c := &effectCtx{z: z, actor: actor, source: actor, target: victim.entity, disp: dispHarmful}

	// Teleporting a non-consenting player is refused (no PvP consent in the demo).
	if err := opTeleport(c, &effectOp{moveRoom: "midgaard:room:market"}); err != nil {
		t.Fatalf("opTeleport: %v", err)
	}
	if victim.entity.location != origin {
		t.Fatalf("a non-consenting player was force-teleported (grief vector)")
	}
	// Pushing a non-consenting player is likewise refused.
	if err := opPush(c, &effectOp{moveDir: "north"}); err != nil {
		t.Fatalf("opPush: %v", err)
	}
	if victim.entity.location != origin {
		t.Fatalf("a non-consenting player was force-pushed (grief vector)")
	}
}

// TestMayRelocateCtx: self and mob targets are ungated; a non-consenting player is gated.
func TestMayRelocateCtx(t *testing.T) {
	z, actor := moveTestZone(t)
	mob := makeMobTarget(z, actor, "goblin")
	victim := newTestPlayerEntity(z, "Victim")
	Move(victim.entity, actor.location)

	cSelf := &effectCtx{z: z, actor: actor, source: actor, disp: dispHarmful}
	if !mayRelocateCtx(cSelf, actor) {
		t.Fatal("moving the actor itself must be allowed")
	}
	if !mayRelocateCtx(cSelf, mob) {
		t.Fatal("moving a mob must be allowed (ungated)")
	}
	if mayRelocateCtx(cSelf, victim.entity) {
		t.Fatal("force-moving a non-consenting player must be gated (denied)")
	}
	if mayRelocateCtx(nil, actor) {
		t.Fatal("a nil ctx must fail closed")
	}
}

// TestTeleportViaAbility is the WIRING guard (#516): a real ability whose on_resolve carries a `teleport`
// op, dispatched as a command, actually relocates the caster — pinning the full ability→runOps→op path that
// the hand-built-effectCtx tests bypass.
func TestTeleportViaAbility(t *testing.T) {
	e := newCmdEnv(t) // actor spawns in the temple
	blink := &abilityDef{
		ref: "blink", name: "Blink", invocation: "command", words: []string{"blink"},
		mode: tmSelf, disposition: dispNeutral,
		ops: []effectOp{{kind: "teleport", moveRoom: "midgaard:room:market", tgt: "self"}},
	}
	e.z.defs.ability.register("blink", blink)
	e.z.defs.abilityCmds["blink"] = blink

	e.run("blink")
	if e.actor.entity.location != e.z.rooms["midgaard:room:market"] {
		t.Fatalf("the blink ability did not teleport the caster to market (in %v)", e.actor.entity.location)
	}
}

// TestOpTeleportAlreadyHereNoop: teleporting the target to the room it's already in is a clean no-op (no
// disengage / OnEnter churn).
func TestOpTeleportAlreadyHereNoop(t *testing.T) {
	z, actor := moveTestZone(t)
	mob := makeMobTarget(z, actor, "goblin") // in temple
	origin := mob.location
	c := &effectCtx{z: z, actor: actor, source: actor, target: mob}

	if err := opTeleport(c, &effectOp{moveRoom: "midgaard:room:temple"}); err != nil {
		t.Fatalf("opTeleport: %v", err)
	}
	if mob.location != origin {
		t.Fatalf("teleport to the same room should be a no-op")
	}
}

// TestRelocateEntityRefusesCrossZone (#516, security Finding 1): the funnel refuses to relocate an entity
// whose location is NOT this zone's, so the same-zone-only property is structural (not a trusted
// precondition) for every caller — including a future cross-zone c.target.
func TestRelocateEntityRefusesCrossZone(t *testing.T) {
	zA, actor := moveTestZone(t)
	zB := newDemoZone("darkwood", newProtoCache())
	// A mob living in zone B, but asked to relocate within zone A.
	foreign := makeMobTarget(zB, mustRoomOccupant(t, zB), "wisp")
	destA := zA.rooms["midgaard:room:market"]
	_ = actor
	if zA.relocateEntity(foreign, destA, false, nil) {
		t.Fatal("relocateEntity moved a foreign-zone entity into this zone (single-writer breach)")
	}
	if foreign.location != nil && foreign.location.zone != zB {
		t.Fatal("the foreign entity's location was mutated across the zone boundary")
	}
}

// mustRoomOccupant returns an entity placed in z's start room, to anchor a foreign-zone mob.
func mustRoomOccupant(t *testing.T, z *Zone) *Entity {
	t.Helper()
	s := newTestPlayerEntity(z, "Anchor")
	Move(s.entity, z.rooms[z.startRoom])
	return s.entity
}

// TestMoveOpsParse: teleport/push parse their fields from a content op map, and register as known ops.
func TestMoveOpsParse(t *testing.T) {
	tp, err := parseOp(map[string]any{"op": "teleport", "room": "z:room:x", "dest": "actor"})
	if err != nil {
		t.Fatalf("parse teleport: %v", err)
	}
	if tp.kind != "teleport" || tp.moveRoom != "z:room:x" || tp.moveDest != "actor" {
		t.Fatalf("teleport parsed wrong: %+v", tp)
	}
	pu, err := parseOp(map[string]any{"op": "push", "dir": "north"})
	if err != nil {
		t.Fatalf("parse push: %v", err)
	}
	if pu.kind != "push" || pu.moveDir != "north" {
		t.Fatalf("push parsed wrong: %+v", pu)
	}
	if _, ok := effectOpHandlers["teleport"]; !ok {
		t.Fatal("teleport not registered as a known op")
	}
	if _, ok := effectOpHandlers["push"]; !ok {
		t.Fatal("push not registered as a known op")
	}
}
