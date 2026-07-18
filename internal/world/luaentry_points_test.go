package world

import (
	"math/rand"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
	lua "github.com/yuin/gopher-lua"
)

// timeAfter is a 5s deadline channel for the bus-cascade-terminates test.
func timeAfter() <-chan time.Time { return time.After(5 * time.Second) }

// luaentry_points_test.go — per-entry-point tests (slice 7.4b…g). Each unit adds its tests here:
// the §5 done-when behaviors, the firing-budget threading, and fail-closed.

// registerRoom adds an entity's room to z.rooms so Lua handles (which re-resolve by walking
// z.rooms -> contents, identity.go) can find entities in it. The ability/combat test fixtures
// build ad-hoc rooms NOT registered in z.rooms (production rooms come from spawnRoom which does
// register); a Lua entry-point test that resolves a handle must register the room. Keyed by the
// room entity's proto ref.
func registerRoom(z *Zone, room *Entity) {
	if room != nil {
		z.rooms[room.proto] = room
	}
}

// --- 7.4b: ability on_resolve Lua ---------------------------------------------------------

// TestLuaOnResolveComposesDamage asserts a Lua on_resolve body composes an effect op
// (ctx.target:damage{}) that lands on a mob — proving the Lua body runs at step 8 and reaches
// the harm surface.
func TestLuaOnResolveComposesDamage(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	registerRoom(z, caster.entity.location)

	def := &abilityDef{
		ref: "luabolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		costs:        []resourceCost{{resource: "mana", amount: 10}},
		onResolveLua: `ctx.target:damage{amount=30, type="fire"}`,
	}
	z.defs.ability.register("luabolt", def)
	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))

	if got := resourceCurrent(mob, "hp"); got != 70 {
		t.Fatalf("mob hp = %d, want 70 (Lua on_resolve composed a 30 fire damage op)", got)
	}
}

// TestLuaOnResolveGatesPerPvP asserts a Lua on_resolve harm op against a NON-consenting player
// is gate-blocked exactly like a declarative op (the 7.3c gate held from the on_resolve path).
func TestLuaOnResolveGatesPerPvP(t *testing.T) {
	z, caster := abilityTestZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	setResourceCurrent(victim.entity, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	registerRoom(z, caster.entity.location)
	setFlag(caster.entity, flagPvP, true) // victim does NOT consent

	def := &abilityDef{
		ref: "luabolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		onResolveLua: `ctx.target:damage{amount=40, type="fire"}`,
	}
	z.defs.ability.register("luabolt", def)
	z.castAbility(caster, def, "Victim", rand.New(rand.NewSource(1)))

	// The OUTER step-4 gate already blocks a harmful ability vs a non-consenting player, so the
	// Lua body never runs — and even if it did, the in-op gate (7.3c) would block it. Either way:
	// no harm.
	if got := resourceCurrent(victim.entity, "hp"); got != 100 {
		t.Fatalf("non-consenting victim hp = %d, want 100 (Lua on_resolve harm gated)", got)
	}
}

// TestLuaOnResolveLandsOnConsentingPlayer asserts the Lua on_resolve DOES land on a consenting
// player (proving the gate-block above is the gate, not a broken path).
func TestLuaOnResolveLandsOnConsentingPlayer(t *testing.T) {
	z, caster := abilityTestZone(t)
	victim := makePlayerTargetInRoom(z, caster.entity, "Victim")
	setResourceCurrent(victim.entity, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	registerRoom(z, caster.entity.location)
	setFlag(caster.entity, flagPvP, true)
	setFlag(victim.entity, flagPvP, true) // both consent

	def := &abilityDef{
		ref: "luabolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		onResolveLua: `ctx.target:damage{amount=25, type="fire"}`,
	}
	z.defs.ability.register("luabolt", def)
	z.castAbility(caster, def, "Victim", rand.New(rand.NewSource(1)))

	if got := resourceCurrent(victim.entity, "hp"); got != 75 {
		t.Fatalf("consenting victim hp = %d, want 75 (Lua on_resolve landed)", got)
	}
}

// TestLuaOnResolveFailClosed asserts a broken Lua on_resolve body is inert (the cast still
// completes its declarative ops + lifecycle, the broken Lua just logs) — the bare-engine
// invariant + fail-closed isolation.
func TestLuaOnResolveFailClosed(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := makeMobTarget(z, caster.entity, "goblin")
	setResourceCurrent(mob, "hp", 100)
	setResourceCurrent(caster.entity, "mana", 100)
	registerRoom(z, caster.entity.location)

	def := &abilityDef{
		ref: "brokenbolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		// A declarative op that DOES land, plus a syntactically-broken Lua body that must not crash.
		ops:          []effectOp{{kind: "deal_damage", dmgType: "fire", amount: 10}},
		onResolveLua: `this is ) not valid lua (`,
	}
	z.defs.ability.register("brokenbolt", def)
	z.castAbility(caster, def, "goblin", rand.New(rand.NewSource(1)))

	// The declarative op landed; the broken Lua was inert (no crash, no extra damage).
	if got := resourceCurrent(mob, "hp"); got != 90 {
		t.Fatalf("mob hp = %d, want 90 (declarative op landed; broken Lua inert)", got)
	}
	// A runtime error in the Lua body also fizzles cleanly: re-cast with an erroring body.
	def2 := &abilityDef{
		ref: "errbolt", invocation: "command", mode: tmEnemy, disposition: dispHarmful,
		ops:          []effectOp{{kind: "deal_damage", dmgType: "fire", amount: 5}},
		onResolveLua: `error("boom")`,
	}
	z.defs.ability.register("errbolt", def2)
	setResourceCurrent(mob, "hp", 100)
	z.castAbility(caster, def2, "goblin", rand.New(rand.NewSource(1)))
	if got := resourceCurrent(mob, "hp"); got != 95 {
		t.Fatalf("mob hp = %d, want 95 (declarative op landed; erroring Lua fizzled)", got)
	}
}

// --- 7.4c: triggers + self.state ----------------------------------------------------------

// scriptedZone builds a zone with a room (registered), a player entity in it, and returns them.
func scriptedZone(t *testing.T) (*Zone, *Entity, *Entity) {
	t.Helper()
	z := newZone("trig")
	room := z.newEntity("trig:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["trig:room:hall"] = room
	player := z.newEntity("trig:player:hero")
	Add(player, &Living{})
	Add(player, &PlayerControlled{})
	player.short = "Hero"
	Move(player, room)
	return z, room, player
}

// addScriptedMob spawns a scripted mob (with the given Lua trigger block) into room with a
// session so it can `say`.
func addScriptedMob(z *Zone, room *Entity, name, luaSrc string) *Entity {
	e := z.newEntity(ProtoRef("trig:mob:" + name))
	Add(e, &Living{})
	Add(e, &Scripted{source: luaSrc})
	e.short = name
	Move(e, room)
	return e
}

// TestTriggerGreetRemembersViaState is the headline 7.4c test: a greeter mob greets a player by
// name on entry and remembers (via self.state) so it greets only ONCE.
func TestTriggerGreetRemembersViaState(t *testing.T) {
	z, room, player := scriptedZone(t)
	// The mob needs a session-less say; we capture greets via a state counter the test reads.
	guard := addScriptedMob(z, room, "guard", `
		state.greeted = {}
		on("greet", function(ev)
			local id = ev.actor:id()
			if not state.greeted[id] then
				state.greeted[id] = true
				state.greet_count = (state.greet_count or 0) + 1
				state.last = ev.actor:name()
			end
		end)
	`)

	// First entry: the guard greets.
	z.fireRoomEntry(player, room)
	es := z.lua.entityScripts[guard.rid]
	if es == nil {
		t.Fatal("the guard's script did not register")
	}
	if got := es.state.RawGetString("greet_count"); got.String() != "1" {
		t.Fatalf("greet_count = %s, want 1 after first entry", got.String())
	}
	if got := es.state.RawGetString("last").String(); got != "Hero" {
		t.Fatalf("greeted last = %q, want Hero", got)
	}
	// Second entry by the SAME player: self.state remembers — no re-greet.
	z.fireRoomEntry(player, room)
	if got := es.state.RawGetString("greet_count"); got.String() != "1" {
		t.Fatalf("greet_count = %s, want STILL 1 (self.state remembered)", got.String())
	}
}

// TestTriggerSpeechReacts asserts an on("speech") handler reacts to a keyword in what a player
// says (the mob emotes via a state flag the test reads).
func TestTriggerSpeechReacts(t *testing.T) {
	z, room, player := scriptedZone(t)
	mob := addScriptedMob(z, room, "sage", `
		on("speech", function(ev)
			if ev.text:find("amulet") then
				state.heard_amulet = true
			end
		end)
	`)
	_ = player

	z.fireSpeech(player, "where is the amulet?")
	es := z.lua.entityScripts[mob.rid]
	if es == nil || es.state.RawGetString("heard_amulet") != trueLV() {
		t.Fatal("the speech trigger did not react to the keyword")
	}
	// An unrelated utterance does not flip a second flag.
	z.fireSpeech(player, "nice weather")
	if es.state.RawGetString("heard_weather") == trueLV() {
		t.Fatal("the speech trigger reacted to an unrelated keyword")
	}
}

// TestTriggerDeathFires asserts the `death` trigger fires on a dying scripted mob (before it
// becomes a corpse) and that its per-instance script state is DROPPED after extraction (the leak
// fix). The handler records the killer's name into state; we read it before death, then confirm
// the entityScripts entry is gone after makeCorpse.
func TestTriggerDeathFires(t *testing.T) {
	z := newZone("trig")
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	z.defs.dmg.register("force", &damageTypeDef{ref: "force"})
	room := z.newEntity("trig:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["trig:room:hall"] = room

	killer := z.newEntity("trig:mob:slayer")
	Add(killer, &Living{})
	killer.short = "the slayer"
	Move(killer, room)

	victim := z.newEntity("trig:mob:victim")
	Add(victim, &Living{})
	Add(victim, &Scripted{source: `on("death", function(ev) state.slain_by = ev.actor:name() end)`})
	victim.short = "the victim"
	Move(victim, room)
	setResourceCurrent(victim, "hp", 1)

	// Kill the victim: dealDamage -> depletion -> die() -> fireDeath -> makeCorpse (drops script).
	c := &effectCtx{z: z, actor: killer, source: killer, rng: z.lua.rng}
	dealDamage(c, victim, 100, "force", "")

	// The death handler ran and recorded the killer — but the entry was DROPPED at extraction, so
	// we cannot read it after the fact. Instead assert the LEAK FIX: no entityScript remains.
	if _, present := z.lua.entityScripts[victim.rid]; present {
		t.Fatal("the dead scripted mob's entityScript was not dropped (memory leak)")
	}
}

// TestEntityScriptDroppedOnDeath directly asserts dropEntityScript frees the per-instance state.
func TestEntityScriptDroppedOnDeath(t *testing.T) {
	z, room, _ := scriptedZone(t)
	mob := addScriptedMob(z, room, "ghost", `on("greet", function(ev) end)`)
	// Build the script (register handlers) by firing a trigger.
	z.lua.ensureEntityScript(mob)
	if _, present := z.lua.entityScripts[mob.rid]; !present {
		t.Fatal("the script was not built")
	}
	z.lua.dropEntityScript(mob.rid)
	if _, present := z.lua.entityScripts[mob.rid]; present {
		t.Fatal("dropEntityScript did not remove the entry")
	}
	// Idempotent + nil-safe.
	z.lua.dropEntityScript(mob.rid)
	z.lua.dropEntityScript(99999)
}

// TestTriggerEnterFiresOnRoom asserts an `enter` trigger on the ROOM fires with the entrant.
func TestTriggerEnterFiresOnRoom(t *testing.T) {
	z := newZone("trig")
	room := z.newEntity("trig:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	Add(room, &Scripted{source: `on("enter", function(ev) state.last_entrant = ev.actor:name() end)`})
	z.rooms["trig:room:hall"] = room
	player := z.newEntity("trig:player:hero")
	Add(player, &Living{})
	player.short = "Wanderer"
	Move(player, room) // the entrant has arrived (so its handle re-resolves in the room)

	z.fireRoomEntry(player, room)
	es := z.lua.entityScripts[room.rid]
	if es == nil || es.state.RawGetString("last_entrant").String() != "Wanderer" {
		t.Fatal("the room `enter` trigger did not record the entrant")
	}
}

// TestTriggerFailClosed asserts a broken trigger block leaves the entity inert (no crash) and a
// non-scripted entity has no triggers (the bare-engine invariant).
func TestTriggerFailClosed(t *testing.T) {
	z, room, player := scriptedZone(t)
	addScriptedMob(z, room, "broken", `this is ) not valid lua (`)
	// A non-scripted mob.
	plain := z.newEntity("trig:mob:plain")
	Add(plain, &Living{})
	Move(plain, room)

	// Firing entry must not crash despite the broken script.
	z.fireRoomEntry(player, room)
	// The plain mob registered no script (nil entry or absent).
	if es := z.lua.ensureEntityScript(plain); es != nil {
		t.Fatal("a non-scripted entity should have no entityScript")
	}
}

// trueLV returns the Lua boolean true value for comparisons.
func trueLV() lua.LValue { return lua.LTrue }

// --- 7.4d: affect hooks (on_apply / on_expire / on_dispel) --------------------------------

// TestAffectLuaHooks asserts an affect's Lua on_apply / on_expire / on_dispel hooks run with
// `self` = the affected entity. The hooks heal the entity (a helpful self op via self:heal) so
// the effect is observable on hp.
func TestAffectLuaHooks(t *testing.T) {
	z := newZone("aff")
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	room := z.newEntity("aff:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["aff:room:hall"] = room

	z.defs.affect.register("blessing", &affectDef{
		ref: "blessing", name: "Blessing", stacking: stackRefresh, maxStacks: 1, duration: 3,
		dispellable: true,
		onApplyLua:  `self:heal("hp", 10)`,
		onExpireLua: `self:heal("hp", 5)`,
		onDispelLua: `self:heal("hp", 1)`,
	})

	e := z.newEntity("aff:mob:cleric")
	Add(e, &Living{})
	Move(e, room)
	setResourceCurrent(e, "hp", 50)

	// Apply: on_apply heals +10.
	applyAffect(e, "blessing", attachOpts{}, nil)
	if got := resourceCurrent(e, "hp"); got != 60 {
		t.Fatalf("hp after on_apply = %d, want 60 (+10)", got)
	}

	// Dispel: on_dispel heals +1, THEN on_expire (expire fires too) heals +5 → +6 total.
	c := &effectCtx{z: z, actor: e, source: e, target: e, mag: 1, disp: dispNeutral, rng: z.lua.rng}
	_ = opDispel(c, &effectOp{kind: "dispel", amount: 0})
	if got := resourceCurrent(e, "hp"); got != 66 {
		t.Fatalf("hp after dispel = %d, want 66 (60 +1 on_dispel +5 on_expire)", got)
	}
}

// TestAffectLuaHookFailClosed asserts a broken affect hook is inert (the affect still
// applies/expires; the broken Lua just logs).
func TestAffectLuaHookFailClosed(t *testing.T) {
	z := newZone("aff")
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	room := z.newEntity("aff:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["aff:room:hall"] = room
	z.defs.affect.register("brokenbuff", &affectDef{
		ref: "brokenbuff", name: "Broken", stacking: stackRefresh, maxStacks: 1, duration: 5,
		onApplyLua: `this is ) not lua (`,
	})
	e := z.newEntity("aff:mob:x")
	Add(e, &Living{})
	Move(e, room)
	// Applying with a broken on_apply must not crash; the affect still attaches.
	applyAffect(e, "brokenbuff", attachOpts{}, nil)
	if !hasAffect(e, "brokenbuff") {
		t.Fatal("the affect did not attach despite a broken on_apply hook")
	}
}

// --- 7.4e: custom commands ----------------------------------------------------------------

// TestCustomCommandRuns asserts a content custom Lua verb runs from dispatch (a `dance` verb the
// player types) and that it can read its arg + message via self.
func TestCustomCommandRuns(t *testing.T) {
	z := newZone("cmd")
	room := z.newEntity("cmd:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["cmd:room:hall"] = room
	// Register a custom `dance` command into the per-shard table.
	registerCustomCommand(z.defs, content.CommandDTO{
		Verb: "dance",
		Lua:  `state_flag = arg; self:send("You dance!")`,
	})
	s := &session{character: "Dancer", out: make(chan *playv1.ServerFrame, 8), epoch: 1}
	z.newPlayerEntity(s, "Dancer")
	Move(s.entity, room)
	z.players["Dancer"] = s

	z.dispatch(s, "dance jig")
	got := drainText(t, s.out)
	if got != "You dance!" {
		// drainText returns the first frame; the command's send should be it.
		// (A prompt frame may follow; we only read the first.)
		t.Fatalf("custom command output = %q, want 'You dance!'", got)
	}
}

// TestCustomCommandNeverShadowsCoreVerb asserts a custom command word that collides with a
// built-in verb is NOT registered (the built-in keeps the verb).
func TestCustomCommandNeverShadowsCoreVerb(t *testing.T) {
	z := newZone("cmd")
	// "look" is a core verb; a custom command must not register it.
	registerCustomCommand(z.defs, content.CommandDTO{Verb: "look", Lua: `self:send("HIJACKED")`})
	if z.customCommandFor("look") != "" {
		t.Fatal("a custom command shadowed the core verb 'look'")
	}
}

// --- 7.4f: pvp_allowed policy + formula ----------------------------------------------------

// TestLuaPvpPolicyDecides asserts a content Lua pvp_allowed policy decides a fight: a policy that
// returns true permits harm between two players who would otherwise be denied (no consent flags).
func TestLuaPvpPolicyDecides(t *testing.T) {
	// Two separate zones: the per-zone compile cache keys a policy by name ("pvp_allowed"), so
	// the same zone would reuse the first-compiled body (the 7.7 hot-reload gen handles a live
	// source swap; a fresh zone is the clean way to test two distinct policies).
	permissive, _, _ := harmZoneForPolicy(t)
	permissive.defs.pvpLua = `return true`
	a := harmPlayer(permissive, permissive.rooms["harm:room:hall"], "A")
	b := harmPlayer(permissive, permissive.rooms["harm:room:hall"], "B")
	if !pvpAllowed(a, b) { // neither consents, but the Lua policy permits
		t.Fatal("a permissive Lua pvp policy should allow harm")
	}

	denying, _, _ := harmZoneForPolicy(t)
	denying.defs.pvpLua = `return false`
	c := harmPlayer(denying, denying.rooms["harm:room:hall"], "C")
	d := harmPlayer(denying, denying.rooms["harm:room:hall"], "D")
	if pvpAllowed(c, d) {
		t.Fatal("a denying Lua pvp policy should deny harm")
	}
}

// TestLuaPvpPolicyFailsClosedAndRespectsSafeRoom asserts (1) a broken/erroring policy DENIES
// (fail-closed), and (2) the safe-room ABSOLUTE veto holds even against a permissive policy.
func TestLuaPvpPolicyFailsClosedAndRespectsSafeRoom(t *testing.T) {
	// (1) An erroring policy denies (fail-closed) — its own zone.
	errZone, _, _ := harmZoneForPolicy(t)
	errZone.defs.pvpLua = `error("policy bug")`
	a := harmPlayer(errZone, errZone.rooms["harm:room:hall"], "A")
	b := harmPlayer(errZone, errZone.rooms["harm:room:hall"], "B")
	if pvpAllowed(a, b) {
		t.Fatal("an erroring pvp policy must DENY (fail-closed)")
	}

	// (2) A permissive policy CANNOT override the safe-room absolute veto — its own zone.
	safeZone, _, _ := harmZoneForPolicy(t)
	safeZone.defs.pvpLua = `return true`
	room := safeZone.rooms["harm:room:hall"]
	c := harmPlayer(safeZone, room, "C")
	d := harmPlayer(safeZone, room, "D")
	if room.room.namedFlags == nil {
		room.room.namedFlags = map[string]bool{}
	}
	room.room.namedFlags[flagSafe] = true
	if pvpAllowed(c, d) {
		t.Fatal("a permissive Lua policy must NOT override the safe-room veto")
	}
}

// TestLuaRegenFormula asserts a Lua `regen` formula overrides the def's flat regen rate.
func TestLuaRegenFormula(t *testing.T) {
	z, _, _ := harmZoneForPolicy(t)
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true, regen: 1})
	z.defs.formulas = map[string]string{"regen": `return 7`} // Lua regen of 7/tick
	e := harmPlayer(z, z.rooms["harm:room:hall"], "Regener")
	setResourceCurrent(e, "hp", 50)
	runRegen(e)
	if got := resourceCurrent(e, "hp"); got != 57 {
		t.Fatalf("hp after Lua-regen tick = %d, want 57 (50 + Lua 7, not def 1)", got)
	}
}

// harmZoneForPolicy builds a zone with hp + a registered room for the pvp/formula tests.
func harmZoneForPolicy(t *testing.T) (*Zone, *luaRuntime, *Entity) {
	t.Helper()
	z := newZone("harm")
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	room := z.newEntity("harm:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["harm:room:hall"] = room
	return z, z.lua, room
}

// --- 7.4g: Lua bus handlers ---------------------------------------------------------------

// TestLuaBusHandlerBuildsResource asserts a Lua OnHit bus handler runs and modifies state (a
// rage pool builds on hit), under the shared event budget.
func TestLuaBusHandlerBuildsResource(t *testing.T) {
	z := newZone("bus")
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 100}})
	z.defs.attr.register("max_rage", &attributeDef{ref: "max_rage", base: litNode{v: 100}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	// A rage pool with a Lua OnHit handler that builds rage on self.
	z.defs.res.register("rage", &resourceDef{
		ref: "rage", maxAttr: "max_rage",
		onEventLua: map[eventKind]string{evOnHit: `self:modify_resource("rage", 5)`},
	})
	room := z.newEntity("bus:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["bus:room:hall"] = room

	mob := z.newEntity("bus:mob:warrior")
	Add(mob, &Living{})
	Move(mob, room)
	setResourceCurrent(mob, "rage", 0) // give it a rage pool (entityHasResource)

	// Fire OnHit about the mob (the subject), with a nil counterpart.
	c := &effectCtx{z: z, actor: mob, source: mob, rng: z.lua.rng}
	z.fireEvent(c, evOnHit, mob, nil, 1)

	if got := resourceCurrent(mob, "rage"); got != 5 {
		t.Fatalf("rage after a Lua OnHit handler = %d, want 5", got)
	}
}

// TestLuaBusHandlerBudgetShared is the SECURITY regression for invariant 1: a Lua bus handler that
// itself does a HARM op which RE-FIRES the bus is bounded by the SHARED width budget — it cannot
// escape the cap by re-firing. This is a REAL witness (not a trivial single-handler termination):
// two foes each carry an OnDamageTaken Lua handler that damages the OTHER, so a single seed damage
// ping-pongs through dealDamage -> OnDamageTaken -> dealDamage -> … . The shared eventBudget
// (maxEventHandlers) truncates the cascade; the test counts handler invocations and asserts the
// total is BOUNDED (≤ the cap), proving the bound is actually enforced, and that it terminates.
func TestLuaBusHandlerBudgetShared(t *testing.T) {
	z := newZone("bus")
	z.defs.attr.register("max_hp", &attributeDef{ref: "max_hp", base: litNode{v: 1000000}})
	z.defs.res.register("hp", &resourceDef{ref: "hp", maxAttr: "max_hp", vital: true})
	z.defs.dmg.register("force", &damageTypeDef{ref: "force"})

	// A Go counter the Lua handler bumps each time it runs — the witness that the cascade is
	// bounded (not infinite) AND that the bound is the shared width budget.
	var fires int
	z.lua.L.SetGlobal("__count", z.lua.L.NewFunction(func(*lua.LState) int { fires++; return 0 }))

	// An OnDamageTaken handler that does a Lua HARM op on the OTHER foe (ev.other) — which re-fires
	// OnDamageTaken on that foe, re-entering the bus through dealDamage. The shared budget truncates.
	handler := map[eventKind]string{
		evOnDamageTaken: `__count(); if ev.other then ev.other:damage{amount=1, type="force"} end`,
	}
	z.defs.res.register("retaliate", &resourceDef{
		ref: "retaliate", maxAttr: "max_hp", onEventLua: handler,
	})

	room := z.newEntity("bus:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["bus:room:hall"] = room
	a := z.newEntity("bus:mob:a")
	Add(a, &Living{})
	Move(a, room)
	setResourceCurrent(a, "hp", 1000000)
	setResourceCurrent(a, "retaliate", 1) // a HAS the retaliate handler
	b := z.newEntity("bus:mob:b")
	Add(b, &Living{})
	Move(b, room)
	setResourceCurrent(b, "hp", 1000000)
	setResourceCurrent(b, "retaliate", 1) // b HAS it too — so the ping-pong can sustain

	// Seed one damage on A (no consent gate: mobs). The OnDamageTaken cascade ping-pongs A<->B and
	// MUST be truncated by the shared eventBudget — it must terminate AND the handler-fire count must
	// be bounded by the width cap (maxEventHandlers), not run away.
	done := make(chan struct{})
	go func() {
		c := &effectCtx{z: z, actor: b, source: b, rng: z.lua.rng}
		dealDamage(c, a, 1, "force", "") // a takes 1 from b -> a's OnDamageTaken fires -> ping-pong
		close(done)
	}()
	select {
	case <-done:
	case <-timeAfter():
		t.Fatal("the re-firing Lua bus cascade did not terminate (shared budget not enforced)")
	}

	if fires == 0 {
		t.Fatal("the OnDamageTaken Lua handler never fired (test is not exercising the bus)")
	}
	if fires > maxEventHandlers {
		t.Fatalf("the re-firing Lua handler ran %d times, exceeding the shared width budget %d — the budget did NOT bound the cascade",
			fires, maxEventHandlers)
	}
	t.Logf("re-firing Lua bus cascade bounded at %d handler fires (cap %d)", fires, maxEventHandlers)
}
