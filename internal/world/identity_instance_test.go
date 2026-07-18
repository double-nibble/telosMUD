package world

import (
	"math/rand"
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
)

// identity_instance_test.go — the NEGATIVE POWER for #410 (slice 1 of #72, instanced zones).
//
// #410 migrated six locality decisions off raw `parseRef(...) == z.id` comparisons onto
// Zone.ownsZoneRef / Zone.localRoom. Because `template == id` for every zone that exists today, the
// refactor is provably inert — and therefore the entire existing suite would ALSO pass with all six
// sites reverted. These tests are what stops that being true: each drives one migrated site against a
// zone whose id and template DIFFER (`midgaard#1` built from `midgaard`), which is exactly the shape
// minting will produce in a later slice, and asserts the outcome that the raw comparison gets wrong.
//
// Every test here FAILS if its site is reverted to `destZone != z.id`, so the six sites are pinned
// now rather than after minting lands on top of them. The instance shape is built BY HAND (setting
// `template` directly) because the minting path does not exist yet; nothing else about a real
// instance (lease, placement, directory registration, the instance-freeze on hot reload) is
// simulated, and nothing here depends on it.

// demoInstanceZone builds a zone whose id is a synthetic instance id and whose template is the demo
// pack's `midgaard`. It mirrors newDemoZone (build.go) exactly, except that `template` is set BEFORE
// buildZone — which is itself the first assertion in this file: buildZone resolves content by
// z.template, so an instance boots its template's rooms despite no content existing under its id.
func demoInstanceZone(t *testing.T, id, template string) *Zone {
	t.Helper()
	z := newZone(id)
	z.template = template
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatalf("load embedded demo pack: %v", err)
	}
	defineContent(z.protos, lc)
	defineGlobals(z.defs, lc)
	z.buildZone(lc)
	if len(z.rooms) == 0 {
		t.Fatalf("instance zone %q built from template %q has NO rooms — buildZone must resolve content by template, not id", id, template)
	}
	for _, ref := range []ProtoRef{"midgaard:room:temple", "midgaard:room:market", "midgaard:room:guildhall"} {
		if z.rooms[ref] == nil {
			t.Fatalf("instance zone %q is missing template room %q; the demo midgaard graph is the fixture these tests read", id, ref)
		}
	}
	if z.startRoom != "midgaard:room:temple" {
		t.Fatalf("instance start room = %q, want the template's midgaard:room:temple", z.startRoom)
	}
	return z
}

// instancePlayer puts a session-backed player in room and returns it.
func instancePlayer(z *Zone, room *Entity, name string) *session {
	s := &session{character: name, out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, name)
	s.entity.short = name
	Move(s.entity, room)
	z.players[name] = s
	return s
}

// instanceMob puts a plain living mob in room and returns it.
func instanceMob(z *Zone, room *Entity, name string) *Entity {
	e := z.newEntity(ProtoRef("test:mob:" + name))
	e.short = name
	e.setKeywords([]string{name})
	Add(e, &Living{})
	Move(e, room)
	setResourceCurrent(e, "hp", 100)
	return e
}

// TestInstanceMoveStaysLocal drives commands.go's two migrated branches. Inside `midgaard#1`, the
// temple's `north` exit names `midgaard:room:market` — a TEMPLATE-prefixed ref. It must be a plain
// local move.
//
// Under the raw `destZone != z.id` this replaced, `"midgaard" != "midgaard#1"` sends the walk down
// the cross-zone path; with no shard and no handoff wired that surfaces as "The way is sealed." —
// i.e. every single exit in every instance is a wall. The negative assertion on that string is the
// point of the test.
func TestInstanceMoveStaysLocal(t *testing.T) {
	z := demoInstanceZone(t, "midgaard#1", "midgaard")
	temple, market := z.rooms["midgaard:room:temple"], z.rooms["midgaard:room:market"]
	s := instancePlayer(z, temple, "Hero")

	if released := z.move(s, "north"); released {
		t.Fatal("a move between two rooms of the SAME instance released ownership (it took the transfer/handoff path)")
	}
	if s.entity.location != market {
		t.Fatalf("player is in %v after walking north, want the instance's own market", roomRefSafe(s.entity.location))
	}
	out := drainAllText(s.out)
	if strings.Contains(out, "sealed") {
		t.Fatalf("a template-prefixed exit inside an instance was routed cross-zone (sealed boundary); output: %q", out)
	}
}

// TestInstanceMoveStillLeavesForAForeignZone is the OVER-WIDENING guard on the same two branches:
// ownsZoneRef must widen to the template and NOTHING else. The market's `north` exit names
// `darkwood:room:grove`, a genuinely foreign zone, and must still route cross-zone (sealed here,
// since a bare test zone has no handoff) rather than silently resolving as local.
func TestInstanceMoveStillLeavesForAForeignZone(t *testing.T) {
	z := demoInstanceZone(t, "midgaard#1", "midgaard")
	market := z.rooms["midgaard:room:market"]
	s := instancePlayer(z, market, "Hero")

	if released := z.move(s, "north"); released {
		t.Fatal("a sealed cross-zone move must not release ownership")
	}
	if s.entity.location != market {
		t.Fatalf("a cross-zone exit relocated the player locally to %v — ownsZoneRef widened past the template", roomRefSafe(s.entity.location))
	}
	if out := drainAllText(s.out); !strings.Contains(out, "sealed") {
		t.Fatalf("expected the sealed-boundary message for a foreign-zone exit, got %q", out)
	}
}

// TestInstanceMoveDoesNotTransferIntoItsTemplate covers commands.go's FIRST migrated branch — the
// intra-shard transfer — which the sealed-boundary test above cannot reach (that branch is inert
// without a shard). This is the worst outcome of the six: with the raw `destZone != z.id`, a walk
// inside `midgaard#1` resolves the exit's zone segment to `midgaard`, finds the shard IS hosting a
// zone by that name (the shared template zone), and hands the player over to it — so a player walking
// north inside an instance silently LEAKS OUT into the public copy of the zone, taking their session
// with them. The move must stay entirely inside the instance.
func TestInstanceMoveDoesNotTransferIntoItsTemplate(t *testing.T) {
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	template := shard.zones["midgaard"]
	inst := demoInstanceZone(t, "midgaard#1", "midgaard")
	shard.adopt("midgaard#1", inst)

	temple, market := inst.rooms["midgaard:room:temple"], inst.rooms["midgaard:room:market"]
	s := instancePlayer(inst, temple, "Hero")

	if released := inst.move(s, "north"); released {
		t.Fatal("a walk inside an instance released ownership — it was handed to another zone (the template) instead of moving locally")
	}
	if s.entity.location != market {
		t.Fatalf("player is in %v, want the INSTANCE's own market (%p)", roomRefSafe(s.entity.location), market)
	}
	if s.entity.location == template.rooms["midgaard:room:market"] {
		t.Fatal("player landed in the TEMPLATE zone's market — the instance leaked its occupant into the shared copy")
	}
	if _, gone := inst.players["Hero"]; !gone {
		t.Fatal("the instance no longer owns the player after an intra-instance walk")
	}
}

// TestInstanceFleeIsPermitted drives combat_commands.go's migrated flee gate — the highest-stakes of
// the six. move() already refuses to walk while fighting, so flee is the ONLY way out of a fight;
// under the raw comparison every exit in an instance reads as cross-zone, flee answers "You can't
// flee that way." in every room, and a party that wipes has no escape at all.
func TestInstanceFleeIsPermitted(t *testing.T) {
	z := demoInstanceZone(t, "midgaard#1", "midgaard")
	temple, market := z.rooms["midgaard:room:temple"], z.rooms["midgaard:room:market"]
	s := instancePlayer(z, temple, "Hero")
	mob := instanceMob(z, temple, "goblin")
	z.startFight(s.entity, mob)

	c := &Context{z: z, s: s, Actor: s.entity, arg: "north"}
	if err := cmdFlee(c); err != nil {
		t.Fatalf("cmdFlee: %v", err)
	}
	out := drainAllText(s.out)
	if strings.Contains(out, "can't flee that way") {
		t.Fatalf("flee through a template-prefixed exit was refused inside an instance — no escape from any fight; output: %q", out)
	}
	if s.entity.location != market {
		t.Fatalf("fleer is in %v, want the instance's own market", roomRefSafe(s.entity.location))
	}
	if s.entity.living.fighting != nil || position(s.entity) == posFighting {
		t.Fatal("flee must disengage the fleer")
	}
}

// TestInstanceFleeStillRefusesAForeignZone is the over-widening guard on flee: a cross-zone exit is
// never a valid escape (combat is same-zone; a cross-zone flee would cross the no-fighting-pointer
// boundary), and that must stay true inside an instance.
func TestInstanceFleeStillRefusesAForeignZone(t *testing.T) {
	z := demoInstanceZone(t, "midgaard#1", "midgaard")
	market := z.rooms["midgaard:room:market"]
	s := instancePlayer(z, market, "Hero")
	mob := instanceMob(z, market, "goblin")
	z.startFight(s.entity, mob)

	c := &Context{z: z, s: s, Actor: s.entity, arg: "north"} // market north -> darkwood
	if err := cmdFlee(c); err != nil {
		t.Fatalf("cmdFlee: %v", err)
	}
	if s.entity.location != market {
		t.Fatalf("flee crossed a zone boundary into %v — it must refuse a foreign-zone exit", roomRefSafe(s.entity.location))
	}
	if out := drainAllText(s.out); !strings.Contains(out, "can't flee that way") {
		t.Fatalf("expected the refusal for a cross-zone flee, got %q", out)
	}
}

// TestInstanceLuaMoveWalksScriptedMob drives luaharm.go's migrated h:move gate. Under the raw
// comparison every scripted mob in every instance — wanderers, chasers, maze content — is silently
// immobilized as a debug-level no-op, with h:move returning false for every direction.
func TestInstanceLuaMoveWalksScriptedMob(t *testing.T) {
	z := demoInstanceZone(t, "midgaard#1", "midgaard")
	temple, market := z.rooms["midgaard:room:temple"], z.rooms["midgaard:room:market"]
	rt := z.lua
	mob := instanceMob(z, temple, "walker")
	rt.L.SetGlobal("me", rt.newHandle(mob))

	if err := rt.runChunkWithSelf("move", `assert(me:move("north") == true, "h:move refused a template-prefixed exit inside an instance")`, mob); err != nil {
		t.Fatalf("h:move inside an instance: %v", err)
	}
	if mob.location != market {
		t.Fatalf("scripted mob is in %v after h:move north, want the instance's own market", roomRefSafe(mob.location))
	}
	// Over-widening guard: a genuinely foreign exit is still the reserved no-op (never a direct
	// cross-zone Move past the single-writer boundary).
	if err := rt.runChunkWithSelf("move", `assert(me:move("north") == false, "h:move crossed a real zone boundary from inside an instance")`, mob); err != nil {
		t.Fatalf("h:move cross-zone from an instance: %v", err)
	}
	if mob.location != market {
		t.Fatalf("h:move smuggled a scripted mob out of the instance to %v", roomRefSafe(mob.location))
	}
}

// TestInstanceAreaTargetsReachesAdjacentRooms drives targeting.go's migrated same-zone containment.
// Under the raw comparison every exit is excluded, so `room_and_adjacent` silently degrades to
// room-only inside every copy — the same authored content quietly behaving differently in an instance
// than in its template, with no error anywhere.
func TestInstanceAreaTargetsReachesAdjacentRooms(t *testing.T) {
	z := demoInstanceZone(t, "midgaard#1", "midgaard")
	temple := z.rooms["midgaard:room:temple"] // exits: north -> market, west -> guildhall (both local)
	market, guildhall := z.rooms["midgaard:room:market"], z.rooms["midgaard:room:guildhall"]

	s := instancePlayer(z, temple, "Mage")
	inRoom := instanceMob(z, temple, "kobold")
	northMob := instanceMob(z, market, "merchant")
	westMob := instanceMob(z, guildhall, "duelist")
	// The guildhall's `down` exit leaves for `crypt` — a mob beyond it must NOT be reached, but it is
	// in another zone entirely and so is simply unreachable here; the containment guard below asserts
	// on the market's own foreign exits instead.

	c := &effectCtx{
		z: z, actor: s.entity, source: s.entity, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(1)), //nolint:gosec // deterministic test roll
	}
	got := map[*Entity]bool{}
	for _, e := range areaTargets(c, "room_and_adjacent") {
		got[e] = true
	}
	for _, want := range []struct {
		e    *Entity
		what string
	}{
		{inRoom, "the caster's own room"},
		{northMob, "the adjacent market (north)"},
		{westMob, "the adjacent guildhall (west)"},
	} {
		if !got[want.e] {
			t.Fatalf("room_and_adjacent inside an instance missed %s — every exit read as cross-zone, degrading the area to room-only", want.what)
		}
	}
	if got[s.entity] {
		t.Fatal("a harmful area op must still exclude the caster")
	}
}

// TestInstanceAreaTargetsStillExcludesForeignZones is the over-widening guard on the containment: the
// SAME-ZONE property is a single-writer safety invariant (no cross-goroutine *Entity is ever
// reached), so widening to the template must not widen to a real neighbour zone. Casting from the
// market — whose north/exit exits name `darkwood` and `overworld` — must reach only the temple.
func TestInstanceAreaTargetsStillExcludesForeignZones(t *testing.T) {
	z := demoInstanceZone(t, "midgaard#1", "midgaard")
	market, temple := z.rooms["midgaard:room:market"], z.rooms["midgaard:room:temple"]

	// A separate zone hosting the darkwood grove the market's north exit names, with an occupant. If
	// the containment leaked, areaTargets would still not find it (it only ever reads z.rooms) — so
	// the observable is that the FOREIGN ref resolves to nothing in this zone's map, asserted via the
	// reachable set below being exactly the local one.
	other := newZone("darkwood")
	grove := other.newEntity("darkwood:room:grove")
	Add(grove, &Room{exits: map[string]ProtoRef{}})
	other.rooms["darkwood:room:grove"] = grove
	foreign := instanceMob(other, grove, "treant")

	s := instancePlayer(z, market, "Mage")
	southMob := instanceMob(z, temple, "priest")

	c := &effectCtx{
		z: z, actor: s.entity, source: s.entity, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(1)), //nolint:gosec // deterministic test roll
	}
	got := map[*Entity]bool{}
	for _, e := range areaTargets(c, "room_and_adjacent") {
		got[e] = true
	}
	if !got[southMob] {
		t.Fatal("room_and_adjacent must still reach the adjacent temple (south) from inside the instance")
	}
	if got[foreign] {
		t.Fatal("room_and_adjacent reached an entity in ANOTHER zone — the same-zone containment (a single-writer invariant) leaked")
	}
}

// TestInstanceRoomExitsYieldHandles drives luahandle.go's migrated localRoomByRef via its real
// consumer, `room:exits()`. Under the raw comparison every exit's `to` inside an instance degrades
// from a room HANDLE to a bare string, and content doing `x.to:occupants()` errors on a string —
// which is the failure a builder actually sees. The foreign exit staying a string in the same table
// is the over-widening guard.
func TestInstanceRoomExitsYieldHandles(t *testing.T) {
	z := demoInstanceZone(t, "midgaard#1", "midgaard")
	market := z.rooms["midgaard:room:market"] // south -> temple (local), north -> darkwood (foreign)
	rt := z.lua
	mob := instanceMob(z, market, "watcher")

	// The direct call first: the migrated helper resolves a template-prefixed ref to THIS instance's
	// own room entity, and a foreign ref to nothing.
	if got := localRoomByRef(z, "midgaard:room:temple"); got != z.rooms["midgaard:room:temple"] {
		t.Fatalf("localRoomByRef on a template-prefixed ref = %v, want the instance's own temple", roomRefSafe(got))
	}
	if got := localRoomByRef(z, "darkwood:room:grove"); got != nil {
		t.Fatalf("localRoomByRef handed out a room for a FOREIGN ref: %v", roomRefSafe(got))
	}

	// Then through the Lua surface content actually uses.
	src := `
local kinds = {}
for _, x in ipairs(self:room():exits()) do kinds[x.dir] = type(x.to) end
assert(kinds.south == "userdata", "a template-prefixed exit inside an instance must yield a room HANDLE, got " .. tostring(kinds.south))
assert(kinds.north == "string", "a foreign-zone exit must stay a bare ref string, got " .. tostring(kinds.north))
`
	if err := rt.runChunkWithSelf("exits", src, mob); err != nil {
		t.Fatalf("room:exits() inside an instance: %v", err)
	}
}

// TestInstanceResyncRoomAddsOwnedRoom drives world.go's migrated hot-reload ADD gate: a room ref the
// zone owns but has no live entity for is spawned. Under the raw comparison the gate reads every
// template-prefixed ref as belonging to some other zone, so an instance would silently never take a
// room ADD. (Whether a LIVE instance should accept a hot reload at all is a separate question, owned
// by the instance-freeze in the lifecycle slice; this pins the ownership test only.)
func TestInstanceResyncRoomAddsOwnedRoom(t *testing.T) {
	z := demoInstanceZone(t, "midgaard#1", "midgaard")
	const ref ProtoRef = "midgaard:room:smithy"
	if z.protos.get(ref) == nil {
		t.Fatalf("fixture: %q must be in the instance's prototype cache", ref)
	}
	delete(z.rooms, ref) // the pre-ADD state: prototype present, no live entity

	z.resyncRoom(ref)
	if z.rooms[ref] == nil {
		t.Fatalf("resyncRoom did not ADD %q — the ownership gate read a template-prefixed ref as another zone's", ref)
	}

	// Over-widening guard: a ref belonging to a genuinely different zone is still skipped, even though
	// its prototype is in the shared cache (the demo pack defines darkwood too).
	const foreign ProtoRef = "darkwood:room:grove"
	if z.protos.get(foreign) == nil {
		t.Fatalf("fixture: %q must be in the shared prototype cache for this guard to mean anything", foreign)
	}
	z.resyncRoom(foreign)
	if z.rooms[foreign] != nil {
		t.Fatalf("resyncRoom spawned %q into the instance — the ADD gate must stay scoped to rooms this zone owns", foreign)
	}
}
