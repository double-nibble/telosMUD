package world

import (
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// luaharm_test.go — slice 7.3c gates (docs/PHASE7-PLAN.md, P7-D3, T8): the Lua harm surface
// routes the EXISTING gate. The headline is the gate held FROM LUA: a Lua h:damage{} against a
// protected player in a safe room is a clean no-op, identical to a declarative op. Plus the
// five-invariant tests (no source/disp spoof) and the helpful/movement paths.

// harmZone builds a zone with an hp resource + a slash damage type, a room, and the runtime.
func harmZone(t *testing.T) (*Zone, *luaRuntime, *Entity) {
	t.Helper()
	// The zone id is the leading segment of the room refs ("harm:room:...") so a within-zone
	// exit (parseRef -> zone "harm") is recognized as local, not cross-zone.
	z := newZone("harm")
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	z.defs.dmg.register("slash", &damageTypeDef{ref: "slash"})
	z.defs.affect.register("weaken", &affectDef{
		ref: "weaken", name: "Weakened", stacking: stackRefresh, maxStacks: 1, duration: 20,
		modifiers: []affectModifier{{attr: "max_hp", add: true, value: -5}},
	})
	z.defs.affect.register("bless", &affectDef{
		ref: "bless", name: "Blessed", stacking: stackRefresh, maxStacks: 1, duration: 20,
		modifiers: []affectModifier{{attr: "max_hp", add: true, value: 5}},
	})
	room := z.newEntity("harm:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["harm:room:hall"] = room
	return z, z.lua, room
}

// harmPlayer makes a consenting/configurable player in room with full hp.
func harmPlayer(z *Zone, room *Entity, name string) *Entity {
	s := &session{character: name, out: make(chan *playv1.ServerFrame, 16), epoch: 1}
	z.newPlayerEntity(s, name)
	s.entity.short = name
	Move(s.entity, room)
	z.players[name] = s
	setResourceCurrent(s.entity, "hp", 100)
	return s.entity
}

// harmMob makes a non-player living entity in room with full hp.
func harmMob(z *Zone, room *Entity, name string) *Entity {
	e := z.newEntity(ProtoRef("harm:mob:" + name))
	Add(e, &Living{})
	e.short = name
	Move(e, room)
	setResourceCurrent(e, "hp", 100)
	return e
}

// --- T8 HEADLINE: the gate held from Lua --------------------------------------------------

// TestLuaDamageGateHeldInSafeRoom is the headline T8 test: a Lua h:damage{} against a PROTECTED
// PLAYER IN A SAFE ROOM is a CLEAN NO-OP — the PvP gate holds identically to a declarative op.
// The actor is a consenting player; only the safe-room flag changes the outcome.
func TestLuaDamageGateHeldInSafeRoom(t *testing.T) {
	z, rt, room := harmZone(t)
	actor := harmPlayer(z, room, "Attacker")
	victim := harmPlayer(z, room, "Victim")
	setFlag(actor, flagPvP, true)
	setFlag(victim, flagPvP, true)
	rt.L.SetGlobal("target", rt.newHandle(victim))

	// Normal room, both consent: the Lua damage LANDS (proving the path works).
	if err := rt.runChunkWithSelf("dmg", `target:damage{amount=20, type="slash"}`, actor); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(victim, "hp"); got != 80 {
		t.Fatalf("consenting victim hp = %d, want 80 (Lua harm landed in a normal room)", got)
	}

	// Flag the room safe: the SAME Lua damage must now be a clean no-op (the gate holds).
	if room.room.namedFlags == nil {
		room.room.namedFlags = map[string]bool{}
	}
	room.room.namedFlags[flagSafe] = true
	if err := rt.runChunkWithSelf("dmg", `target:damage{amount=20, type="slash"}`, actor); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(victim, "hp"); got != 80 {
		t.Fatalf("victim hp = %d after Lua damage in a SAFE room, want 80 (gate held from Lua)", got)
	}
}

// TestLuaDamageGateNonConsenting asserts a Lua h:damage{} against a NON-consenting player (no
// pvp flag) is a clean no-op even in a normal room — the default-deny gate holds from Lua.
func TestLuaDamageGateNonConsenting(t *testing.T) {
	z, rt, room := harmZone(t)
	actor := harmPlayer(z, room, "Attacker")
	victim := harmPlayer(z, room, "Victim")
	setFlag(actor, flagPvP, true) // actor consents; victim does NOT
	rt.L.SetGlobal("target", rt.newHandle(victim))

	if err := rt.runChunkWithSelf("dmg", `target:damage{amount=30, type="slash"}`, actor); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(victim, "hp"); got != 100 {
		t.Fatalf("non-consenting victim hp = %d, want 100 (Lua harm gate-blocked)", got)
	}
}

// TestLuaDamageLandsOnMob asserts the same Lua h:damage{} DOES land on a mob (the gate is
// player-vs-player only) — proving the no-op above is the gate, not a broken path.
func TestLuaDamageLandsOnMob(t *testing.T) {
	z, rt, room := harmZone(t)
	actor := harmPlayer(z, room, "Attacker")
	mob := harmMob(z, room, "goblin")
	rt.L.SetGlobal("target", rt.newHandle(mob))
	if err := rt.runChunkWithSelf("dmg", `target:damage{amount=25, type="slash"}`, actor); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(mob, "hp"); got != 75 {
		t.Fatalf("mob hp = %d, want 75 (Lua harm lands on a mob — gate is PvP-only)", got)
	}
}

// TestLuaDamagePerTargetGate asserts a Lua harm op gates PER TARGET in a multi-target context:
// a consenting foe is hit, a non-consenting player is not — driven from one script.
func TestLuaDamagePerTargetGate(t *testing.T) {
	z, rt, room := harmZone(t)
	actor := harmPlayer(z, room, "Attacker")
	consenting := harmPlayer(z, room, "Consenter")
	protected := harmPlayer(z, room, "Protected")
	setFlag(actor, flagPvP, true)
	setFlag(consenting, flagPvP, true) // protected does NOT consent
	rt.L.SetGlobal("foe", rt.newHandle(consenting))
	rt.L.SetGlobal("bystander", rt.newHandle(protected))

	if err := rt.runChunkWithSelf("dmg", `
		foe:damage{amount=20, type="slash"}
		bystander:damage{amount=20, type="slash"}
	`, actor); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(consenting, "hp"); got != 80 {
		t.Fatalf("consenting foe hp = %d, want 80 (hit)", got)
	}
	if got := resourceCurrent(protected, "hp"); got != 100 {
		t.Fatalf("protected player hp = %d, want 100 (gate held per-target)", got)
	}
}

// --- P7-D3 invariant tests: no source/disp spoof ------------------------------------------

// TestLuaApplyAffectCannotSpoofSource asserts invariant 1: a Lua h:apply_affect's source is the
// invocation actor, NOT a script-supplied `source=` — so attribution cannot be spoofed. We apply
// a harmful affect to a non-consenting player and confirm it is gate-blocked regardless of any
// `source=` the script passes.
func TestLuaApplyAffectCannotSpoofSource(t *testing.T) {
	z, rt, room := harmZone(t)
	actor := harmPlayer(z, room, "Caster")
	victim := harmPlayer(z, room, "Victim")
	setFlag(actor, flagPvP, true) // victim does NOT consent
	rt.L.SetGlobal("target", rt.newHandle(victim))
	rt.L.SetGlobal("caster", rt.newHandle(actor))

	// A harmful (stat-reducing) affect on a non-consenting player must be gate-blocked. A
	// `source=` key (even naming the victim itself, to try to make it look self-applied) is
	// ignored — the source is the invocation actor.
	if err := rt.runChunkWithSelf("aff", `target:apply_affect("weaken", {duration=10, source=target})`, actor); err != nil {
		t.Fatal(err)
	}
	if hasAffect(victim, "weaken") {
		t.Fatal("a harmful affect landed on a non-consenting player (source-spoof bypassed the gate)")
	}
}

// TestLuaDamageNoInvocationNoOps asserts a harm op with NO active invocation (a bare runChunk,
// no actor) is a clean no-op (fail-closed) — a script cannot harm without an engine-established
// actor.
func TestLuaDamageNoInvocationNoOps(t *testing.T) {
	z, rt, room := harmZone(t)
	mob := harmMob(z, room, "goblin")
	rt.L.SetGlobal("target", rt.newHandle(mob))
	// Plain runChunk: no invocation actor is set.
	if err := rt.runChunk("dmg", `target:damage{amount=50, type="slash"}`); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(mob, "hp"); got != 100 {
		t.Fatalf("mob hp = %d, want 100 (no-invocation harm must no-op)", got)
	}
}

// --- helpful paths: heal / self-buff ------------------------------------------------------

// TestLuaHealAndSelfBuff asserts the helpful paths: h:heal raises a pool, and a self-buff
// apply_affect attaches (ungated).
func TestLuaHealAndSelfBuff(t *testing.T) {
	z, rt, room := harmZone(t)
	actor := harmPlayer(z, room, "Healer")
	setResourceCurrent(actor, "hp", 40)
	rt.L.SetGlobal("me", rt.newHandle(actor))

	if err := rt.runChunkWithSelf("heal", `me:heal("hp", 25)`, actor); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(actor, "hp"); got != 65 {
		t.Fatalf("healed hp = %d, want 65", got)
	}
	// A beneficial self-buff attaches (no gate — a buff on self).
	if err := rt.runChunkWithSelf("buff", `me:apply_affect("bless", {duration=10})`, actor); err != nil {
		t.Fatal(err)
	}
	if !hasAffect(actor, "bless") {
		t.Fatal("a self-buff did not attach")
	}
}

// TestLuaModifyResourceGated asserts modify_resource on a non-consenting player is gated (any
// sign), but on self/mob it applies.
func TestLuaModifyResourceGated(t *testing.T) {
	z, rt, room := harmZone(t)
	actor := harmPlayer(z, room, "Mage")
	victim := harmPlayer(z, room, "Victim")
	setFlag(actor, flagPvP, true) // victim does NOT consent
	rt.L.SetGlobal("victim", rt.newHandle(victim))
	rt.L.SetGlobal("me", rt.newHandle(actor))

	// Cross-player resource write (negative) on a non-consenter: gated, no change.
	if err := rt.runChunkWithSelf("mod", `victim:modify_resource("hp", -30)`, actor); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(victim, "hp"); got != 100 {
		t.Fatalf("non-consenting victim hp = %d, want 100 (modify_resource gated)", got)
	}
	// Self write applies (ungated).
	setResourceCurrent(actor, "hp", 50)
	if err := rt.runChunkWithSelf("mod", `me:modify_resource("hp", 20)`, actor); err != nil {
		t.Fatal(err)
	}
	if got := resourceCurrent(actor, "hp"); got != 70 {
		t.Fatalf("self modify_resource hp = %d, want 70", got)
	}
}

// --- movement: within-zone works; cross-zone is a reserved no-op --------------------------

// TestLuaMoveWithinZone asserts h:move moves the actor through a within-zone exit.
func TestLuaMoveWithinZone(t *testing.T) {
	z, rt, room := harmZone(t)
	// A second room and an east exit from the first.
	room2 := z.newEntity("harm:room:east")
	Add(room2, &Room{exits: map[string]ProtoRef{}})
	z.rooms["harm:room:east"] = room2
	room.room.exits["east"] = "harm:room:east"

	mob := harmMob(z, room, "walker")
	rt.L.SetGlobal("me", rt.newHandle(mob))
	if err := rt.runChunkWithSelf("move", `assert(me:move("east") == true)`, mob); err != nil {
		t.Fatal(err)
	}
	if mob.location != room2 {
		t.Fatalf("h:move did not relocate within the zone (location=%v)", roomRefSafe(mob.location))
	}
}

// TestLuaMoveCrossZoneNoOps asserts h:move with an exit leading to ANOTHER zone is a clean no-op
// — no cross-zone smuggle past the single-writer boundary.
func TestLuaMoveCrossZoneNoOps(t *testing.T) {
	z, rt, room := harmZone(t)
	// An exit whose ref names a DIFFERENT zone.
	room.room.exits["north"] = "otherzone:room:cell"
	mob := harmMob(z, room, "walker")
	rt.L.SetGlobal("me", rt.newHandle(mob))
	if err := rt.runChunkWithSelf("move", `assert(me:move("north") == false)`, mob); err != nil {
		t.Fatal(err)
	}
	if mob.location != room {
		t.Fatal("h:move smuggled an entity toward another zone (must no-op)")
	}
}

// TestLuaTeleportCrossZoneNoOps asserts h:teleport to a room in ANOTHER zone is a clean no-op.
func TestLuaTeleportCrossZoneNoOps(t *testing.T) {
	z, rt, room := harmZone(t)
	zoneB := newZone("zone-b")
	bRoom := zoneB.newEntity("b:room:cell")
	Add(bRoom, &Room{exits: map[string]ProtoRef{}})
	zoneB.rooms["b:room:cell"] = bRoom

	mob := harmMob(z, room, "walker")
	rt.L.SetGlobal("me", rt.newHandle(mob))
	// A handle for zone B's room, injected into zone A's runtime.
	rt.L.SetGlobal("there", rt.newHandle(bRoom))
	if err := rt.runChunkWithSelf("tp", `assert(me:teleport(there) == false)`, mob); err != nil {
		t.Fatal(err)
	}
	if mob.location != room {
		t.Fatal("h:teleport smuggled an entity into another zone (must no-op)")
	}
	// And it certainly is not in zone B's tree.
	if zoneB.entityByRID(mob.rid) != nil {
		t.Fatal("the entity appeared in zone B's tree after a cross-zone teleport")
	}
}

// TestLuaTeleportGriefGated asserts teleporting a NON-CONSENTING player is gated (movement-grief
// vector): the actor cannot force-relocate a protected player.
func TestLuaTeleportGriefGated(t *testing.T) {
	z, rt, room := harmZone(t)
	room2 := z.newEntity("harm:room:cell")
	Add(room2, &Room{exits: map[string]ProtoRef{}})
	z.rooms["harm:room:cell"] = room2

	actor := harmPlayer(z, room, "Trickster")
	victim := harmPlayer(z, room, "Victim")
	setFlag(actor, flagPvP, true) // victim does NOT consent
	rt.L.SetGlobal("victim", rt.newHandle(victim))
	rt.L.SetGlobal("dest", rt.newHandle(room2))

	if err := rt.runChunkWithSelf("tp", `assert(victim:teleport(dest) == false)`, actor); err != nil {
		t.Fatal(err)
	}
	if victim.location != room {
		t.Fatal("a non-consenting player was force-teleported (movement-grief gate failed)")
	}
	// The actor CAN teleport itself.
	rt.L.SetGlobal("me", rt.newHandle(actor))
	if err := rt.runChunkWithSelf("tp", `assert(me:teleport(dest) == true)`, actor); err != nil {
		t.Fatal(err)
	}
	if actor.location != room2 {
		t.Fatal("the actor could not teleport itself within the zone")
	}
}

// --- P7-D3 invariant 5: depth/eventBudget threaded, never reset ---------------------------

// TestLuaHarmCtxThreadsCascadeBudget asserts invariant 5: a harm op's effectCtx inherits the
// SAME depth + eventBudget pointer from the invocation, never a fresh/zeroed one — so a Lua harm
// op fired inside an event/reaction cascade is bounded by the shared depth/width budget and
// cannot escape it. We set an invocation with a non-zero depth and a shared budget, then confirm
// harmCtx carries them through (the same pointer, not a copy).
func TestLuaHarmCtxThreadsCascadeBudget(t *testing.T) {
	z, rt, room := harmZone(t)
	actor := harmMob(z, room, "caster")
	budget := 7
	rt.inv = &luaInvocation{actor: actor, depth: 3, eventBudget: &budget}
	defer func() { rt.inv = nil }()

	c := rt.harmCtx(actor)
	if c == nil {
		t.Fatal("harmCtx returned nil with an active invocation")
	}
	if c.depth != 3 {
		t.Fatalf("harmCtx depth = %d, want 3 (threaded, not reset)", c.depth)
	}
	if c.eventBudget != &budget {
		t.Fatal("harmCtx eventBudget is not the SAME pointer as the invocation's (a reaction loop would be unbounded)")
	}
	// And the five-invariant construction: actor==source==invocation actor, disp harmful, zone rng.
	if c.actor != actor || c.source != actor {
		t.Fatal("harmCtx actor/source must be the invocation actor (invariant 1)")
	}
	if c.disp != dispHarmful {
		t.Fatal("harmCtx disp must be engine-set dispHarmful (invariant 2)")
	}
	if c.rng != rt.rng {
		t.Fatal("harmCtx rng must be the zone rng (invariant 4)")
	}
}

// roomRefSafe is a nil-safe room ref for test diagnostics.
func roomRefSafe(e *Entity) string {
	if e == nil {
		return "<nil>"
	}
	return string(e.proto)
}
