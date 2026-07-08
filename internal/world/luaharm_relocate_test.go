package world

import (
	"math/rand"
	"testing"
)

// luaharm_relocate_test.go — FOLLOW-UPS §2 combat-fidelity for the Lua relocation methods
// (h:move / h:teleport / h:recall, all backed by relocateWithinZone). It pins the per-method
// combat discipline the engine move has and the Lua relocation previously skipped:
//
//   - a relocated FIGHTER disengages — no `fighting` pointer / posFighting spans two rooms
//     (the load-bearing invariant the same-room round driver rests on);
//   - h:move (walk-like) FIRES the OnLeaveRoom checkpoint and PROVOKES opportunity attacks;
//     h:teleport / h:recall INTENTIONALLY BYPASS it (a blink grants no OA — its combat value);
//   - the POST-MOVE LIVENESS RE-CHECK aborts the arrival hooks cleanly when an OnEnter cascade
//     kills the entrant mid-arrival (no use-after-relocation).
//
// The combat content (the `reactions` OnLeaveRoom opportunity-attack resource, the combat
// attributes) is reused from reaction_test.go's reactionZone; this file adds the runtime + the
// arrival-hook (OnEnter death) content.

// relocateZone extends reactionZone (the combat + reactions content) with the per-zone Lua runtime
// the harm methods need. The reactionZone's id is "test", and its room refs are "test:room:*", so a
// within-zone destination is recognized as local (parseRef -> zone "test").
func relocateZone(t *testing.T) (*Zone, *session, *Entity) {
	t.Helper()
	z, fleer, from := reactionZone(t)
	z.combatRand = rand.New(rand.NewSource(1))
	return z, fleer, from
}

// --- (c) the fighting entity: a relocated fighter disengages -------------------------------

// TestLuaTeleportDisengagesFighter asserts a FIGHTING mover dropped by h:teleport leaves no
// fighting link spanning two rooms: the mover stops fighting (position back to standing, fighting
// nil) AND its opponent's link to it is dropped — the no-fighting-pointer-crosses-a-room invariant.
func TestLuaTeleportDisengagesFighter(t *testing.T) {
	z, fleer, from := relocateZone(t)
	dest := z.rooms["test:room:to"]
	mob := reactorMob(z, from, fleer.entity, "goblin")

	// A real two-sided fight between the fleer (the mover) and the mob, in `from`.
	fleer.entity.living.fighting = mob
	setPosition(fleer.entity, posFighting)
	mob.living.fighting = fleer.entity
	setPosition(mob, posFighting)

	rt := z.lua
	rt.L.SetGlobal("dest", rt.newHandle(dest))
	if err := rt.runChunkWithSelf("tp", `assert(self:teleport(dest) == true)`, fleer.entity); err != nil {
		t.Fatal(err)
	}

	if fleer.entity.location != dest {
		t.Fatalf("h:teleport did not relocate the fighter (location=%v)", roomRefSafe(fleer.entity.location))
	}
	// The mover disengaged.
	if fleer.entity.living.fighting != nil {
		t.Fatal("relocated fighter still holds a fighting pointer (would span two rooms)")
	}
	if position(fleer.entity) == posFighting {
		t.Fatal("relocated fighter still posFighting after relocation")
	}
	// The opponent left behind no longer points at the departed mover.
	if mob.living.fighting == fleer.entity {
		t.Fatal("opponent's fighting link still points at the relocated mover (cross-room fighting link)")
	}
	if position(mob) == posFighting {
		t.Fatal("opponent left stranded posFighting at a now-departed target")
	}
}

// --- (a) the leave checkpoint: h:move PROVOKES, h:teleport BYPASSES -------------------------

// TestLuaMoveProvokesOpportunityAttack asserts the walk-like h:move FIRES the OnLeaveRoom
// checkpoint: an engaged reactor lands its declarative opportunity attack on the mover (and the
// budget threads — a re-relocation the same round, budget spent, provokes nothing).
func TestLuaMoveProvokesOpportunityAttack(t *testing.T) {
	z, fleer, from := relocateZone(t)
	// from has a "north" exit to test:room:to (reactionZone wires it); h:move walks that exit.
	mob := reactorMob(z, from, fleer.entity, "goblin")
	// A two-sided fight so the reactor's fighting link points AT the mover (the engaged-foe gate).
	fleer.entity.living.fighting = mob
	setPosition(fleer.entity, posFighting)
	mob.living.fighting = fleer.entity
	setPosition(mob, posFighting)

	z.topUpReactions(mob)
	if got := resourceCurrent(mob, "reactions"); got != 1 {
		t.Fatalf("reactor must start with 1 reaction, got %d", got)
	}

	startHP := resourceCurrent(fleer.entity, "hp")
	rt := z.lua
	if err := rt.runChunkWithSelf("mv", `assert(self:move("north") == true)`, fleer.entity); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(fleer.entity, "hp"); got != startHP-1 {
		t.Fatalf("h:move must provoke a 1-damage opportunity attack: mover hp %d, want %d", got, startHP-1)
	}
	if got := resourceCurrent(mob, "reactions"); got != 0 {
		t.Fatalf("the opportunity attack must SPEND the reaction: reactions %d, want 0", got)
	}
	if fleer.entity.location != z.rooms["test:room:to"] {
		t.Fatalf("h:move did not relocate the mover (location=%v)", roomRefSafe(fleer.entity.location))
	}
}

// TestLuaTeleportBypassesOpportunityAttack asserts h:teleport INTENTIONALLY does NOT fire the
// OnLeaveRoom checkpoint: the same engaged reactor (with a full reaction budget) lands NO
// opportunity attack — a blink is OA-free by design, distinct from the walk-like h:move above.
func TestLuaTeleportBypassesOpportunityAttack(t *testing.T) {
	z, fleer, from := relocateZone(t)
	dest := z.rooms["test:room:to"]
	mob := reactorMob(z, from, fleer.entity, "goblin")
	fleer.entity.living.fighting = mob
	setPosition(fleer.entity, posFighting)
	mob.living.fighting = fleer.entity
	setPosition(mob, posFighting)

	z.topUpReactions(mob)
	startHP := resourceCurrent(fleer.entity, "hp")

	rt := z.lua
	rt.L.SetGlobal("dest", rt.newHandle(dest))
	if err := rt.runChunkWithSelf("tp", `assert(self:teleport(dest) == true)`, fleer.entity); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(fleer.entity, "hp"); got != startHP {
		t.Fatalf("h:teleport must NOT provoke an opportunity attack: mover hp %d, want %d (unchanged)", got, startHP)
	}
	if got := resourceCurrent(mob, "reactions"); got != 1 {
		t.Fatalf("h:teleport must not spend the reactor's budget: reactions %d, want 1 (untouched)", got)
	}
	if fleer.entity.location != dest {
		t.Fatalf("h:teleport did not relocate the mover (location=%v)", roomRefSafe(fleer.entity.location))
	}
}

// TestLuaRecallBypassesOpportunityAttack asserts h:recall, like h:teleport, BYPASSES the leave
// checkpoint (the magical yank to the recall point grants no OA).
func TestLuaRecallBypassesOpportunityAttack(t *testing.T) {
	z, fleer, from := relocateZone(t)
	// recall sends to the zone's start room — point it at the registered destination room.
	z.startRoom = "test:room:to"
	mob := reactorMob(z, from, fleer.entity, "goblin")
	fleer.entity.living.fighting = mob
	setPosition(fleer.entity, posFighting)
	mob.living.fighting = fleer.entity
	setPosition(mob, posFighting)

	z.topUpReactions(mob)
	startHP := resourceCurrent(fleer.entity, "hp")

	rt := z.lua
	if err := rt.runChunkWithSelf("rc", `assert(self:recall() == true)`, fleer.entity); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(fleer.entity, "hp"); got != startHP {
		t.Fatalf("h:recall must NOT provoke an opportunity attack: mover hp %d, want %d (unchanged)", got, startHP)
	}
	if got := resourceCurrent(mob, "reactions"); got != 1 {
		t.Fatalf("h:recall must not spend the reactor's budget: reactions %d, want 1", got)
	}
	if fleer.entity.location != z.rooms["test:room:to"] {
		t.Fatalf("h:recall did not relocate the mover (location=%v)", roomRefSafe(fleer.entity.location))
	}
}

// --- (b) the post-Move liveness re-check ----------------------------------------------------

// TestLuaRelocateLivenessRecheckOnArrivalKill is the use-after-relocation guard: an arrival hook
// that KILLS the entrant (a room DEATH-FIELD, the FIRST arrival hook) must abort the REST of the
// relocation cleanly — no LATER arrival hook runs against the dead/respawned mover. The relocation
// still reports it MOVED (it left the origin room; the death path owns its fate).
//
// The OBSERVABLE failure mode the re-check prevents: the death-field kills the entrant on arrival,
// the death path RESPAWNS the player (full hp, at the start room `from`, OFF dest). Without the
// re-check the relocation would then fire the LATER OnEnter hook ABOUT the just-respawned player —
// a use-after-relocation (acting on the mover after a hook already relocated it). A MARKER resource
// witnesses it: its OnEnter handler decrements a non-vital marker pool (so a re-kill/respawn can't
// mask it the way a vital pool would). With the re-check, OnEnter never fires post-kill, so the
// marker is untouched; without it, the marker is decremented by the stray post-relocation fire.
//
// The death-field is mob-sourced (PvE) so its lethal tick lands on the player entrant ungated.
func TestLuaRelocateLivenessRecheckOnArrivalKill(t *testing.T) {
	z, fleer, from := relocateZone(t)
	dest := z.rooms["test:room:to"]
	// The player respawns at the start room on death — point it at `from` (NOT dest) so the kill on
	// arrival demonstrably relocates the entrant OFF dest, restoring it to full hp.
	z.startRoom = from.proto

	// The DEATH-FIELD: a roomScoped affect whose tickOp deals lethal damage to each occupant on entry
	// (applyRoomAffectsTo runs tickOps on the entrant) — the FIRST arrival hook. Mob-sourced (PvE) so
	// the harm lands on a player ungated.
	z.defs.affect.register("deathfield", &affectDef{
		ref: "deathfield", name: "Death Field", roomScoped: true,
		stacking: stackRefresh, duration: 100, hasTick: true, tickInterval: 2,
		tickOps: []effectOp{{kind: "deal_damage", dmgType: "slash", amount: 9999}},
	})
	field := z.newEntity(ProtoRef("test:mob:fieldcaster"))
	Add(field, &Living{})
	Move(field, dest)
	setResourceCurrent(field, "hp", 100)
	cField := &effectCtx{z: z, actor: field, source: field, mag: 1, disp: dispHarmful}
	opApplyAffect(cField, &effectOp{kind: "apply_affect", affect: "deathfield"})
	// Pull the field caster OUT of dest so it is not itself an occupant confound.
	Move(field, from)

	// The OnEnter LATER-hook witness: a non-vital `marker` resource on the entrant whose OnEnter handler
	// decrements it. OnEnter fires LAST in the arrival sequence — so if the per-hook liveness re-check is
	// missing, it runs against the just-respawned player and the marker drops. A non-vital pool is the
	// right witness: it is NOT restored by respawn, so a stray post-relocation fire leaves a visible mark
	// (unlike a vital pool, where the re-kill's respawn would refill it and mask the bug).
	z.defs.attr.register("max_marker", &attributeDef{ref: "max_marker", base: litNode{v: 0}})
	setAttrBase(fleer.entity, "max_marker", 10)
	z.defs.res.register("marker", &resourceDef{
		ref: "marker", maxAttr: "max_marker",
		onEvent: map[eventKind][]effectOp{
			evOnEnter: {{kind: "modify_resource", resource: "marker", amount: -1}},
		},
	})
	setResourceCurrent(fleer.entity, "marker", 10)

	rt := z.lua
	rt.L.SetGlobal("dest", rt.newHandle(dest))
	// Teleport into the killing room. Must not panic / use-after-relocation; the handle returns true
	// (it DID leave origin), and the death path owns the entrant afterward.
	if err := rt.runChunkWithSelf("tp", `assert(self:teleport(dest) == true)`, fleer.entity); err != nil {
		t.Fatal(err)
	}

	// The death-field MUST have fired and killed the entrant: a player respawns to full hp at the start
	// room (`from`), so the observable signal is a CHANGED location off dest. If it is still in dest the
	// kill never happened and the assertion below proves nothing.
	if fleer.entity.location == dest && position(fleer.entity) != posDead {
		t.Fatal("precondition: the death-field did not kill the entrant — test is vacuous")
	}
	if fleer.entity.location != from {
		t.Fatalf("expected the killed entrant to respawn at the start room (%s), got %s",
			from.proto, roomRefSafe(fleer.entity.location))
	}
	// THE GUARD: the LATER OnEnter hook must NOT have fired against the respawned player. If the
	// liveness re-check had not aborted the arrival hooks after the death-field, the OnEnter handler
	// would have decremented the marker — proof it acted on the mover AFTER the field already
	// relocated it (a use-after-relocation). With the re-check, OnEnter never fires post-kill, so the
	// marker is untouched at its full value.
	if got := resourceCurrent(fleer.entity, "marker"); got != 10 {
		t.Fatalf("a later arrival hook (OnEnter) ran against the respawned entrant: marker %d, want 10 (use-after-relocation)", got)
	}
}
