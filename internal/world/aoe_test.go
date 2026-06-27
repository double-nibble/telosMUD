package world

import (
	"math/rand"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// aoe_test.go exercises Phase 6.4a: [G12] AoE / area targeting (the per-op area loop, the per-target
// harm gate, a per-target save-for-half) and [G13] room-scoped affects (attach to the room, tick over
// occupants, land on entrants, expire cleanly), plus the same-zone-containment of room_and_adjacent.
// Determinism: the d1/size-1 trick makes a band edge select an exact outcome; a seeded rng makes a
// multi-roll AoE reproducible. All effects run on the zone goroutine directly (no real timers).

// aoeZone builds a bare zone with the attributes/resources/damage type an AoE save-for-half reads. It
// returns the zone and a caster session in z.players (so resolve-by-id works). Mobs/players are added
// per-test in the caster's room.
func aoeZone(t *testing.T) (*Zone, *session) {
	t.Helper()
	z := newZone("test")
	reg := func(ref string, base float64) {
		z.defs.attr.register(ref, &attributeDef{ref: ref, base: litNode{v: base}})
	}
	reg("strength", 10)
	reg("dex_save", 0)
	reg("spell_dc", 13)
	reg("dodge", 5)
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.attr.register("max_mana", &attributeDef{ref: "max_mana", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	z.defs.res.register("mana", &resourceDef{ref: "mana", maxAttr: "max_mana"})
	z.defs.dmg.register("fire", &damageTypeDef{ref: "fire", resist: map[string]float64{"fire": 1.0}})

	caster := makeRoomPlayer(z, "Caster")
	setResourceCurrent(caster.entity, "mana", 100)
	return z, caster
}

// fireballAreaDef is the AoE save-for-half fireball used by the per-target tests: ONE area-scoped check
// op (area: room), DEX save vs spell DC, save => half (4 fire flat for determinism), fail => full (8).
// Flat amounts (no dice) keep the per-target damage exact; the SAVE outcome is forced by the d1 trick or
// by tuning dex_save/spell_dc, not by rng.
func fireballAreaDef() *abilityDef {
	return &abilityDef{
		ref: "fireball", name: "Fireball", invocation: "command", words: []string{"fireball"},
		mode: tmEnemy, disposition: dispHarmful, area: "room",
		costs: []resourceCost{{resource: "mana", amount: 30}},
		ops: []effectOp{{
			kind: "check", area: "room",
			check: &checkSpec{
				label: "Dexterity save", dice: mustDiceT("1d20"),
				bonus: attrNode{ref: "$target.dex_save"},
				vs:    checkVs{dc: attrNode{ref: "$source.spell_dc"}},
				bands: []checkBand{
					{marginMin: litNode{v: 0}, label: "save", ops: []effectOp{
						{kind: "deal_damage", dmgType: "fire", amount: 4},
					}},
					{label: "fail", ops: []effectOp{
						{kind: "deal_damage", dmgType: "fire", amount: 8},
					}},
				},
			},
		}},
	}
}

// addMob places a non-player living target with hp in the caster's room.
func aoeMob(z *Zone, room *Entity, name string, hp int) *Entity {
	e := z.newEntity(ProtoRef("test:mob"))
	e.short = name
	e.setKeywords([]string{name})
	Add(e, &Living{})
	Move(e, room)
	setResourceCurrent(e, "hp", hp)
	return e
}

// TestAoEPerTargetGate is the core [G12] security property: an AoE re-gates EACH occupant. A consenting
// foe (a mob; mob harm is always allowed) takes damage; a NON-consenting player in the same room is
// UNHARMED — per-target, not one gate for the whole blast.
func TestAoEPerTargetGate(t *testing.T) {
	z, caster := aoeZone(t)
	room := caster.entity.location

	// A mob (always harmable) and a non-consenting bystander player, both in the room.
	mob := aoeMob(z, room, "goblin", 100)
	bystander := makePlayerTargetInRoom(z, caster.entity, "Bystander")
	setResourceCurrent(bystander.entity, "hp", 100)
	// No PvP consent set on the bystander => a harmful AoE op on them must be a clean no-op.

	def := fireballAreaDef()
	// Force EVERYONE to FAIL the save (full 8) so the per-target GATE — not the save — decides who is
	// hurt: a high spell DC + zero dex_save means total(1d20+0) < 13 unless a nat-high; seed so both
	// roll low. Simpler: pin the outcome with a deterministic rng that yields low d20 faces.
	c := &effectCtx{z: z, actor: caster.entity, source: caster.entity, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(7))}
	runOps(c, def.ops)

	// The mob took damage (8 on a failed save, or 4 on a save — either way < 100); the player took NONE.
	if got := resourceCurrent(mob, "hp"); got >= 100 {
		t.Fatalf("AoE must harm the consenting mob: hp %d, want < 100", got)
	}
	if got := resourceCurrent(bystander.entity, "hp"); got != 100 {
		t.Fatalf("AoE must NOT harm a non-consenting player (per-target gate): hp %d, want 100", got)
	}

	// With PvP consent on both sides, the SAME AoE now harms the player too (the gate opens per target).
	setFlag(caster.entity, flagPvP, true)
	setFlag(bystander.entity, flagPvP, true)
	c2 := &effectCtx{z: z, actor: caster.entity, source: caster.entity, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(7))}
	runOps(c2, def.ops)
	if got := resourceCurrent(bystander.entity, "hp"); got >= 100 {
		t.Fatalf("consented AoE must harm the player: hp %d, want < 100", got)
	}
}

// TestAoESaveHalvesPerTarget is the milestone: a fireball over a room rolls a DEX save PER TARGET and the
// save HALVES the damage for that target. One mob is built to ALWAYS save (huge dex_save), one to ALWAYS
// fail (huge negative). Each takes its own band's damage — proving the save is per-target, not global.
func TestAoESaveHalvesPerTarget(t *testing.T) {
	z, caster := aoeZone(t)
	room := caster.entity.location

	saver := aoeMob(z, room, "nimble", 100)
	setAttrBase(saver, "dex_save", 100) // total = 1d20 + 100 >= 13 always => save band (4 dmg)
	failer := aoeMob(z, room, "clumsy", 100)
	setAttrBase(failer, "dex_save", -100) // total = 1d20 - 100 < 13 always => fail band (8 dmg)

	def := fireballAreaDef()
	c := &effectCtx{z: z, actor: caster.entity, source: caster.entity, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(1))}
	runOps(c, def.ops)

	if got := resourceCurrent(saver, "hp"); got != 96 {
		t.Fatalf("the saver takes HALF (4): hp %d, want 96", got)
	}
	if got := resourceCurrent(failer, "hp"); got != 92 {
		t.Fatalf("the failer takes FULL (8): hp %d, want 92", got)
	}
}

// TestAoEExcludesCaster confirms the harmful-self-exclusion: a harmful room AoE does not catch the
// caster in their own blast.
func TestAoEExcludesCaster(t *testing.T) {
	z, caster := aoeZone(t)
	room := caster.entity.location
	setResourceCurrent(caster.entity, "hp", 100)
	_ = aoeMob(z, room, "goblin", 100)

	def := fireballAreaDef()
	c := &effectCtx{z: z, actor: caster.entity, source: caster.entity, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(3))}
	runOps(c, def.ops)
	if got := resourceCurrent(caster.entity, "hp"); got != 100 {
		t.Fatalf("a harmful room AoE must exclude the caster: hp %d, want 100", got)
	}
}

// TestAoERoomAndAdjacentSameZoneContainment is the #1 distsys property: room_and_adjacent reaches the
// caster's room + same-zone adjacent rooms, but NEVER a cross-zone exit's destination. We build the
// caster's room with two exits — one to a same-zone room (a mob there IS hit) and one whose destination
// ref names ANOTHER zone (a mob placed in a room behind that ref is NOT hit, because the exit is
// excluded before any room is dereferenced).
func TestAoERoomAndAdjacentSameZoneContainment(t *testing.T) {
	z, caster := aoeZone(t)
	z.id = "midgaard"
	origin := caster.entity.location
	// Give the origin a Room with two exits: north => same-zone room, east => cross-zone ref.
	originRoom := &Room{exits: map[string]ProtoRef{
		"north": "midgaard:room:adj",
		"east":  "darkwood:room:grove", // a DIFFERENT zone — must be excluded
	}}
	Add(origin, originRoom)

	// The same-zone adjacent room, registered in z.rooms (the zone's OWN map).
	adj := z.newEntity("midgaard:room:adj")
	Add(adj, &Room{})
	z.rooms["midgaard:room:adj"] = adj
	near := aoeMob(z, adj, "near", 100)
	setAttrBase(near, "dex_save", -100) // always fails => full 8

	// A room registered under the CROSS-ZONE ref. Even though it exists in this test's map under that
	// key, the exit's dest zone ("darkwood") != z.id ("midgaard"), so areaTargets skips the exit and
	// never looks this room up — the mob inside is untouched.
	cross := z.newEntity("darkwood:room:grove")
	Add(cross, &Room{})
	z.rooms["darkwood:room:grove"] = cross
	far := aoeMob(z, cross, "far", 100)
	setAttrBase(far, "dex_save", -100)

	def := fireballAreaDef()
	// Use room_and_adjacent on both the ability and its op.
	def.area = "room_and_adjacent"
	def.ops[0].area = "room_and_adjacent"

	c := &effectCtx{z: z, actor: caster.entity, source: caster.entity, mag: 1, disp: dispHarmful,
		rng: rand.New(rand.NewSource(1))}
	runOps(c, def.ops)

	if got := resourceCurrent(near, "hp"); got != 92 {
		t.Fatalf("same-zone adjacent mob must be hit: hp %d, want 92", got)
	}
	if got := resourceCurrent(far, "hp"); got != 100 {
		t.Fatalf("cross-zone exit must be EXCLUDED (no cross-zone reach): hp %d, want 100", got)
	}
}

// TestDemoFireballIsContentAoE drives the ACTUAL demo pack: the fireball ability authored in demo.yaml
// (targeting.area: room + a per-target DEX save) is loaded from content and cast through the lifecycle,
// hitting two mobs in the room with per-target saves — proving the [G12] milestone is exercised entirely
// by CONTENT (the engine names no spell/area). One mob is built to fail (full), one to save (half).
func TestDemoFireballIsContentAoE(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	market := z.rooms["midgaard:room:market"]

	caster := &session{character: "Mage", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(caster, "Mage")
	Move(caster.entity, market)
	z.players["Mage"] = caster
	setResourceCurrent(caster.entity, "mana", 100)
	setAttrBase(caster.entity, "spell_dc", 100) // a sky-high DC so the fail band is forced for a low save

	failer := aoeMob(z, market, "kobold", 200)
	setAttrBase(failer, "dex_save", -100) // always fails => full 8d6
	saver := aoeMob(z, market, "sprite", 200)
	setAttrBase(saver, "dex_save", 200) // total still < 100 DC? no — 1d20+200 >= 100 always => save (4d6)

	def := z.abilityForVerb("fireball")
	if def == nil {
		t.Fatal("demo pack must register the fireball verb")
	}
	if def.area != "room" {
		t.Fatalf("demo fireball must be area: room, got %q", def.area)
	}
	z.castAbility(caster, def, "", rand.New(rand.NewSource(1))) // no keyword target: a room blast

	// Both mobs took fire damage; the failer (full 8d6) took strictly MORE than the saver (half 4d6).
	failDmg := 200 - resourceCurrent(failer, "hp")
	saveDmg := 200 - resourceCurrent(saver, "hp")
	if failDmg <= 0 || saveDmg <= 0 {
		t.Fatalf("both room occupants must be hit by the content AoE: fail=%d save=%d", failDmg, saveDmg)
	}
	if failDmg <= saveDmg {
		t.Fatalf("the failed-save mob must take MORE (full) than the saver (half): fail=%d save=%d", failDmg, saveDmg)
	}
	// The caster (in the room) was excluded from its own harmful blast.
	if resourceCurrent(caster.entity, "hp") < resourceMax(caster.entity, "hp") {
		t.Fatal("caster must be excluded from its own fireball")
	}
}

// TestDemoWebIsContentRoomAffect drives the demo pack's `web` ability + room-scoped affect end to end: a
// content cast roots an occupant and an entrant via the room field, proving [G13] is content (the engine
// names no affect).
func TestDemoWebIsContentRoomAffect(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	market := z.rooms["midgaard:room:market"]

	caster := &session{character: "Weaver", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(caster, "Weaver")
	Move(caster.entity, market)
	z.players["Weaver"] = caster
	setResourceCurrent(caster.entity, "mana", 100)

	occupant := aoeMob(z, market, "kobold", 100)

	def := z.abilityForVerb("web")
	if def == nil {
		t.Fatal("demo pack must register the web verb")
	}
	z.castAbility(caster, def, "", rand.New(rand.NewSource(1)))

	if !preventsTag(occupant, "move") {
		t.Fatal("demo web must root an occupant present at cast")
	}
	entrant := aoeMob(z, market, "spiderfood", 100)
	applyRoomAffectsTo(entrant)
	if !preventsTag(entrant, "move") {
		t.Fatal("demo web must root an entrant on arrival")
	}
}

// --- [G13] room-scoped affects ----------------------------------------------------------------------

// webZone builds a bare zone with the `web` room-scoped affect (prevents move, -1 dodge, tick interval
// 2, duration 6) plus a player caster. Returns the zone, the caster, and the caster's room.
func webZone(t *testing.T) (*Zone, *session, *Entity) {
	t.Helper()
	z := newZone("test")
	z.defs.attr.register("dodge", &attributeDef{ref: "dodge", base: litNode{v: 5}})
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	z.defs.affect.register("web", &affectDef{
		ref: "web", name: "Webbed", category: "snare", roomScoped: true,
		stacking: stackRefresh, dispellable: true, duration: 6,
		modifiers: []affectModifier{{attr: "dodge", add: true, value: -1}},
		prevents:  []string{"move"},
		hasTick:   true, tickInterval: 2,
	})
	caster := makeRoomPlayer(z, "Weaver")
	return z, caster, caster.entity.location
}

// TestTransferInLandsRoomAffects pins the arrival-hook PARITY (distsys 6.4a SC1/SC2): a player arriving
// via transferIn — the same arrival hook the cross-shard handoff bind now also calls — into a webbed
// room is rooted ON ARRIVAL, not only on the next room tick. Pre-fix, transferIn applied room affects
// but a parallel path (cross-shard bind) did not; this guards that the arrival hook stays wired. The web
// is mob-cast so the per-occupant landing is the realistic ungated PvE case (a mob webs the room).
func TestTransferInLandsRoomAffects(t *testing.T) {
	z, _, room := webZone(t)
	z.rooms[room.proto] = room // register so transferIn can resolve the room by ref (white-box)
	// A mob in the room casts the web (mob source => PvE, so it lands on a player without consent).
	weaver := aoeMob(z, room, "weaver", 100)
	c := &effectCtx{z: z, actor: weaver, source: weaver, mag: 1, disp: dispHarmful}
	opApplyAffect(c, &effectOp{kind: "apply_affect", affect: "web"})

	// A player arrives via transferIn into the webbed room.
	arriver := newTestPlayerEntity(z, "Arriver")
	z.transferIn(transferInMsg{s: arriver, room: room.proto})

	if !preventsTag(arriver.entity, "move") {
		t.Fatal("a player arriving via transferIn into a webbed room must be rooted on arrival (arrival-hook parity)")
	}
}

// TestWebRootsOccupantsAndEntrants is the [G13] end-to-end: a web room-affect roots the OCCUPANTS present
// when it lands AND someone who walks IN afterward; both then expire cleanly.
func TestWebRootsOccupantsAndEntrants(t *testing.T) {
	z, caster, room := webZone(t)

	// An occupant standing in the room when the web is cast.
	occupant := aoeMob(z, room, "occupant", 100)

	// Cast the web into the room (the room-scoped apply_affect path).
	c := &effectCtx{z: z, actor: caster.entity, source: caster.entity, mag: 1, disp: dispHarmful}
	opApplyAffect(c, &effectOp{kind: "apply_affect", affect: "web"})

	// The occupant present at cast is rooted (prevents the `move` tag) right away.
	if !preventsTag(occupant, "move") {
		t.Fatal("occupant present at cast must be rooted by the web")
	}
	// And the -1 dodge modifier landed (5 base -> 4).
	if got := attr(occupant, "dodge"); got != 4 {
		t.Fatalf("web -1 dodge on occupant: dodge %v, want 4", got)
	}

	// A NEW creature walks into the webbed room — it gets rooted on arrival (the entrant hook).
	entrant := z.newEntity(ProtoRef("test:mob"))
	entrant.short = "entrant"
	Add(entrant, &Living{})
	Move(entrant, room)
	applyRoomAffectsTo(entrant) // the move/arrival path calls this for a real move
	if !preventsTag(entrant, "move") {
		t.Fatal("a creature entering the webbed room must be rooted on arrival")
	}

	// Drive the room tick to expiry (duration 6). After expiry the web is gone from the room AND from
	// every occupant (the field cleared them).
	for i := 0; i < 7; i++ {
		z.pulses.tick()
	}
	if preventsTag(occupant, "move") {
		t.Fatal("after the web expires the occupant must no longer be rooted")
	}
	if preventsTag(entrant, "move") {
		t.Fatal("after the web expires the entrant must no longer be rooted")
	}
	// The room itself no longer carries the room affect.
	if a, ok := Get[*Affected](room); ok && a.hasActiveAffects() {
		t.Fatal("the web must be cleared from the room on expiry")
	}
}

// TestWebGatesEntrantPerCreature confirms the room-affect's per-creature harm funnel: a non-consenting
// PLAYER entering a (player-cast, harmful) web is NOT rooted (the gate blocks the leased CC), while a mob
// entrant IS. The room-affect landing routes the same guardHarmful every harm vector does.
func TestWebGatesEntrantPerCreature(t *testing.T) {
	z, caster, room := webZone(t)
	z.defs.attr.register("max_mana", &attributeDef{ref: "max_mana", base: litNode{v: 100}})
	z.defs.res.register("mana", &resourceDef{ref: "mana", maxAttr: "max_mana"})

	c := &effectCtx{z: z, actor: caster.entity, source: caster.entity, mag: 1, disp: dispHarmful}
	opApplyAffect(c, &effectOp{kind: "apply_affect", affect: "web"})

	// A non-consenting player entrant: the harmful web CC must be GATED (not rooted).
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	applyRoomAffectsTo(victim.entity)
	if preventsTag(victim.entity, "move") {
		t.Fatal("a non-consenting player entrant must NOT be rooted (per-creature gate)")
	}

	// A mob entrant (always harmable) IS rooted.
	mob := aoeMob(z, room, "spiderfood", 100)
	applyRoomAffectsTo(mob)
	if !preventsTag(mob, "move") {
		t.Fatal("a mob entrant must be rooted by the web")
	}
}
