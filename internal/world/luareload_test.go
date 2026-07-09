package world

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
	lua "github.com/yuin/gopher-lua"
)

// luareload_test.go — slice 7.7 hot-reload tests: the source-aware chunkFor (the MUST-FIX), the
// live mob-greeting reload (self.state survives), the old-gen mud.after drop, the broken-edit
// last-good, and the breaker reset.

// TestKeyMatchesRef pins the #57 fix: chunk-cache invalidation matches ref as a whole colon-delimited
// segment, so it never over-invalidates a longer ref that merely contains the substring.
func TestKeyMatchesRef(t *testing.T) {
	cases := []struct {
		key, ref string
		want     bool
	}{
		// interior segment (the common case: "<kind>:<ref>:<hook>")
		{"ability:orc:on_resolve", "orc", true},
		{"affect:orc:tick", "orc", true},
		{"trigger:midgaard:mob:orc:register", "midgaard:mob:orc", true}, // ref with its own colons
		{"ability:midgaard:mob:orc:on_resolve", "midgaard:mob:orc", true},
		// trailing segment ("<kind>:<ref>" with no hook)
		{"command:orc", "orc", true},
		// the #57 bug: a longer ref that merely CONTAINS the substring must NOT match
		{"ability:sorcerer:on_resolve", "orc", false},
		{"affect:orchard:tick", "orc", false},
		{"trigger:midgaard:mob:sorcerer:register", "orc", false},
		// unrelated kind/hook substrings must not match
		{"formula:force", "orc", false},
		// exact / empty guards
		{"orc", "orc", true},
		{"ability:orc:on_resolve", "", false},
	}
	for _, tc := range cases {
		if got := keyMatchesRef(tc.key, tc.ref); got != tc.want {
			t.Errorf("keyMatchesRef(%q, %q) = %v, want %v", tc.key, tc.ref, got, tc.want)
		}
	}
}

// TestReloadLuaDoesNotOverInvalidate proves the fix end-to-end at reloadLua: reloading ref "orc" drops the
// "orc" chunk but LEAVES the "sorcerer" chunk (whose key merely contains the substring "orc") — the pre-#57
// substring match would have wrongly dropped it, forcing a needless recompile.
func TestReloadLuaDoesNotOverInvalidate(t *testing.T) {
	z := newZone("rl")
	rt := z.lua

	orc := rt.chunkFor("ability:orc:on_resolve", `return 1`)
	sorc := rt.chunkFor("ability:sorcerer:on_resolve", `return 2`)
	if orc == nil || sorc == nil {
		t.Fatal("precondition: both chunks compiled")
	}
	if _, ok := rt.chunks["ability:orc:on_resolve"]; !ok {
		t.Fatal("precondition: orc chunk cached")
	}

	z.reloadLua("ability", "orc")

	if _, ok := rt.chunks["ability:orc:on_resolve"]; ok {
		t.Fatal("reloadLua should have invalidated the reloaded ref's chunk")
	}
	if _, ok := rt.chunks["ability:sorcerer:on_resolve"]; !ok {
		t.Fatal("reloadLua over-invalidated 'sorcerer' when reloading 'orc' (the #57 substring bug)")
	}
}

// --- the chunkFor source/gen MUST-FIX (the security-critical fix) --------------------------

// TestChunkForRecompilesOnSourceChange asserts a CHANGED source recompiles (the stale-cache no-op
// is gone), and — the security regression — a pvp_allowed permissive->restrictive edit now DENIES.
func TestChunkForRecompilesOnSourceChange(t *testing.T) {
	z := newZone("rl")
	rt := z.lua

	// A formula chunk: first source returns 1, edited source returns 2.
	rt.L.SetGlobal("__out", rt.L.NewFunction(func(*lua.LState) int { return 0 }))
	ch1 := rt.chunkFor("formula:test", `return 1`)
	v1, _ := rt.invokeForNumber(ch1, &luaInvocation{}, nil)
	if v1 != 1 {
		t.Fatalf("first source returned %v, want 1", v1)
	}
	// SAME key, NEW source: must recompile (not reuse the stale chunk).
	ch2 := rt.chunkFor("formula:test", `return 2`)
	if ch1 == ch2 {
		t.Fatal("chunkFor returned the SAME chunk for a CHANGED source (stale-cache no-op — the MUST-FIX bug)")
	}
	v2, _ := rt.invokeForNumber(ch2, &luaInvocation{}, nil)
	if v2 != 2 {
		t.Fatalf("edited source returned %v, want 2 (recompile took effect)", v2)
	}
	// SAME source again: reused (compile-once amortized).
	if rt.chunkFor("formula:test", `return 2`) != ch2 {
		t.Fatal("chunkFor recompiled an UNCHANGED source (should reuse)")
	}
}

// TestPvpPolicyRecompilesPermissiveToRestrictive is the SECURITY regression: editing the pvp policy
// from permissive to restrictive (same key) now DENIES — the stale-permissive-policy hazard is gone.
func TestPvpPolicyRecompilesPermissiveToRestrictive(t *testing.T) {
	z := newZone("rl")
	rt := z.lua

	permissive := rt.chunkFor("pvp_allowed", `return true`)
	if !rt.invokeForBool(permissive, &luaInvocation{}, nil) {
		t.Fatal("permissive policy should permit")
	}
	// Edit to restrictive (same key) — must recompile and now DENY.
	restrictive := rt.chunkFor("pvp_allowed", `return false`)
	if restrictive == permissive {
		t.Fatal("the pvp policy edit reused the stale permissive chunk (SECURITY HAZARD)")
	}
	if rt.invokeForBool(restrictive, &luaInvocation{}, nil) {
		t.Fatal("the restrictive policy edit did not take effect — still permitting (SECURITY REGRESSION)")
	}
}

// TestChunkForBrokenEditKeepsLastGood asserts a syntactically-broken edit keeps the LAST-GOOD chunk
// (the def keeps its old behavior, logged) — never blanks a working def.
func TestChunkForBrokenEditKeepsLastGood(t *testing.T) {
	z := newZone("rl")
	rt := z.lua
	good := rt.chunkFor("formula:t", `return 5`)
	if good == nil {
		t.Fatal("the good source did not compile")
	}
	// A broken edit: chunkFor must return the LAST-GOOD chunk, not nil.
	got := rt.chunkFor("formula:t", `return ) broken (`)
	if got != good {
		t.Fatal("a broken edit did not keep the last-good chunk")
	}
	v, _ := rt.invokeForNumber(got, &luaInvocation{}, nil)
	if v != 5 {
		t.Fatalf("the kept last-good chunk returned %v, want 5 (old behavior preserved)", v)
	}
}

// --- the live mob-greeting reload (self.state survives) ------------------------------------

// reloadScriptedZone builds a zone with a scripted mob (greeting source) and a player in a room.
func reloadScriptedZone(t *testing.T, mobLua string) (*Zone, *Entity, *Entity) {
	t.Helper()
	z := newZone("rl")
	z.protos.define("rl:mob:guard", nil, "the guard", "A guard stands here.", componentSet{
		reflect.TypeFor[*Living]():   &Living{},
		reflect.TypeFor[*Scripted](): &Scripted{source: mobLua},
	})
	room := z.newEntity("rl:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["rl:room:hall"] = room
	guard := z.spawn("rl:mob:guard")
	Move(guard, room)
	player := z.newEntity("rl:player:hero")
	Add(player, &Living{})
	player.short = "Hero"
	Move(player, room)
	return z, room, guard
}

// TestMobGreetingReloadsLiveStatePersists is the headline 7.7 test: editing a mob's Lua greeting
// reloads LIVE — the next greet uses the NEW text while self.state (who's been greeted) PERSISTS.
func TestMobGreetingReloadsLiveStatePersists(t *testing.T) {
	z, room, guard := reloadScriptedZone(t, `
		state.greets = 0
		on("greet", function(ev)
			state.greets = state.greets + 1
			state.last = "v1:"..ev.actor:name()
		end)
	`)
	player := func() *Entity {
		for _, e := range room.contents {
			if e.short == "Hero" {
				return e
			}
		}
		return nil
	}()

	// First greet under v1.
	z.fireRoomEntry(player, room)
	es := z.lua.entityScripts[guard.rid]
	if es == nil {
		t.Fatal("the guard's script did not register")
	}
	if got := es.state.RawGetString("greets"); got.String() != "1" {
		t.Fatalf("greets after v1 = %s, want 1", got.String())
	}
	if got := es.state.RawGetString("last").String(); got != "v1:Hero" {
		t.Fatalf("v1 last = %q, want v1:Hero", got)
	}

	// EDIT the mob's Lua (a new greeting text), swap the prototype, and apply the reload.
	z.protos.reload("rl:mob:guard", newPrototype("rl:mob:guard", nil, "the guard", "A guard stands here.",
		componentSet{
			reflect.TypeFor[*Living]():   &Living{},
			reflect.TypeFor[*Scripted](): &Scripted{source: `on("greet", function(ev) state.greets = state.greets + 1; state.last = "v2:"..ev.actor:name() end)`},
		}))
	z.reloadLua("mob", "rl:mob:guard")

	// Next greet uses the NEW text, and self.state (greets count) PERSISTED across the reload.
	z.fireRoomEntry(player, room)
	es2 := z.lua.entityScripts[guard.rid]
	if got := es2.state.RawGetString("greets"); got.String() != "2" {
		t.Fatalf("greets after v2 = %s, want 2 (self.state survived the reload)", got.String())
	}
	if got := es2.state.RawGetString("last").String(); got != "v2:Hero" {
		t.Fatalf("v2 last = %q, want v2:Hero (the new greeting text took effect)", got)
	}
}

// TestTopLevelStateSeedReloadGuard (#67) pins the reload semantics that the demo greeter (and the builder
// guide) depend on: a hot reload RE-RUNS the whole registration body against the PRESERVED self.state. So a
// GUARDED top-level seed (`state.x = state.x or {}`) is a no-op on reload and the data survives, while a bare
// `state.x = {}` re-executes and WIPES it. Both halves are asserted so a future engine change that alters
// re-run semantics is caught either way.
func TestTopLevelStateSeedReloadGuard(t *testing.T) {
	heroIn := func(room *Entity) *Entity {
		for _, e := range room.contents {
			if e.short == "Hero" {
				return e
			}
		}
		return nil
	}
	greetedHas := func(z *Zone, guard *Entity, name string) bool {
		es := z.lua.entityScripts[guard.rid]
		if es == nil {
			t.Fatal("script did not register")
		}
		tbl, ok := es.state.RawGetString("greeted").(*lua.LTable)
		return ok && tbl.RawGetString(name) == lua.LTrue
	}
	reloadWith := func(z *Zone, src string) {
		z.protos.reload("rl:mob:guard", newPrototype("rl:mob:guard", nil, "the guard", "A guard stands here.",
			componentSet{
				reflect.TypeFor[*Living]():   &Living{},
				reflect.TypeFor[*Scripted](): &Scripted{source: src},
			}))
		z.reloadLua("mob", "rl:mob:guard")
	}

	// GUARDED — the idiom the demo greeter uses: the seed survives a reload.
	guarded := `
		state.greeted = state.greeted or {}
		on("greet", function(ev) state.greeted[ev.actor:name()] = true end)`
	z, room, guard := reloadScriptedZone(t, guarded)
	z.fireRoomEntry(heroIn(room), room)
	if !greetedHas(z, guard, "Hero") {
		t.Fatal("guarded: Hero should be recorded after the first greet")
	}
	reloadWith(z, guarded) // edit-and-reload keeping the guarded seed
	if !greetedHas(z, guard, "Hero") {
		t.Fatal("guarded: `state.greeted = state.greeted or {}` must PRESERVE the greeted set across a reload")
	}

	// UNGUARDED — the anti-pattern #67 warns about: the bare seed re-runs and wipes the set.
	unguarded := `
		state.greeted = {}
		on("greet", function(ev) state.greeted[ev.actor:name()] = true end)`
	z2, room2, guard2 := reloadScriptedZone(t, unguarded)
	z2.fireRoomEntry(heroIn(room2), room2)
	if !greetedHas(z2, guard2, "Hero") {
		t.Fatal("unguarded: Hero should be recorded after the first greet")
	}
	reloadWith(z2, unguarded)
	if greetedHas(z2, guard2, "Hero") {
		t.Fatal("unguarded: `state.greeted = {}` re-runs on reload and WIPES the set (this is the bug the guard fixes)")
	}
}

// --- the old-gen mud.after drop (P7-D7) ----------------------------------------------------

// TestOldGenTimerDropsOnReload asserts a pending mud.after timer scheduled BEFORE a reload is
// DROPPED at fire (doesn't run old code against new state), while a durable=true finalizer COMPLETES.
func TestOldGenTimerDropsOnReload(t *testing.T) {
	z := newZone("rl")
	rt := z.lua
	var ran, ranDurable bool
	rt.L.SetGlobal("__mark", rt.L.NewFunction(func(*lua.LState) int { ran = true; return 0 }))
	rt.L.SetGlobal("__markDurable", rt.L.NewFunction(func(*lua.LState) int { ranDurable = true; return 0 }))

	// Schedule two timers at the current generation: a normal one and a durable one.
	if err := rt.runChunk("sched", `
		mud.after(2, function() __mark() end)
		mud.after(2, function() __markDurable() end, {durable=true})
	`); err != nil {
		t.Fatal(err)
	}
	// A reload bumps the chunk generation (simulating an edit between schedule and fire).
	rt.chunkGen++

	z.pulses.tick()
	z.pulses.tick() // both due now
	if ran {
		t.Fatal("an OLD-GEN mud.after callback ran after a reload (should drop — old code vs new state)")
	}
	if !ranDurable {
		t.Fatal("a durable=true mud.after finalizer did NOT complete across the reload (should run)")
	}
}

// --- the breaker reset on reload -----------------------------------------------------------

// TestBreakerResetOnReload asserts a script DISABLED by a bug is RE-ENABLED on a successful reload.
func TestBreakerResetOnHotReload(t *testing.T) {
	z := newZone("rl")
	rt := z.lua
	// Trip the breaker for an ability on_resolve via repeated errors.
	ch := rt.chunkFor("ability:bad:on_resolve", `error("boom")`)
	key := breakerKeyShared("ability:bad:on_resolve")
	for i := 0; i < 50 && !rt.breakerDisabled(key); i++ {
		_ = rt.invoke(ch, &luaInvocation{}, nil)
	}
	if !rt.breakerDisabled(key) {
		t.Fatal("the breaker did not trip")
	}
	// A reload of that ref re-enables the breaker.
	z.protos.reload("bad", nil) // no proto needed; reloadLua resets the breaker for the ref
	z.reloadLua("ability", "bad")
	if rt.breakerDisabled(key) {
		t.Fatal("the breaker was not reset on reload (a fixed script should be re-enabled)")
	}
}

// TestPerInstanceBreakerResetsOnHotReload is the 7.7 security-review follow-up: a per-INSTANCE
// trigger breaker (keyed by rid, not the shared (kind,ref) key) must also reset on a fix-reload,
// so a corrected mob script re-enables immediately instead of staying inert until the instance
// repops. Trips a live guard's per-instance breaker with a broken greet, then reloads a fixed
// source and asserts the breaker cleared.
func TestPerInstanceBreakerResetsOnHotReload(t *testing.T) {
	z, room, guard := reloadScriptedZone(t, `on("greet", function(ev) error("boom") end)`)
	var player *Entity
	for _, e := range room.contents {
		if e.short == "Hero" {
			player = e
		}
	}
	key := breakerKeyInstance(guard.rid)
	// Trip the per-instance breaker via the repeatedly-erroring greet trigger.
	for i := 0; i < 50 && !z.lua.breakerDisabled(key); i++ {
		z.fireRoomEntry(player, room)
	}
	if !z.lua.breakerDisabled(key) {
		t.Fatal("the per-instance breaker did not trip on the broken trigger")
	}
	// Fix the script and reload: the per-instance breaker must reset (re-enable the corrected trigger).
	z.protos.reload("rl:mob:guard", newPrototype("rl:mob:guard", nil, "the guard", "A guard stands here.",
		componentSet{
			reflect.TypeFor[*Living]():   &Living{},
			reflect.TypeFor[*Scripted](): &Scripted{source: `on("greet", function(ev) end)`},
		}))
	z.reloadLua("mob", "rl:mob:guard")
	if z.lua.breakerDisabled(key) {
		t.Fatal("the per-instance breaker was not reset on the fix-reload (the corrected trigger stays quarantined)")
	}
}

// --- the FULL bus path (subscriber goroutine -> notifyZones -> inbox -> reloadLua) ---------

// scriptedReloadPack is a pack with a scripted greeter mob, for the full-path reload test.
func scriptedReloadPack(lua string) content.Pack {
	return content.Pack{
		Pack: "reloadtest",
		Zones: []content.ZoneDTO{{
			Ref: "rt", Name: "Reload Test Zone", StartRoom: "rt:room:hall",
			Rooms: []content.RoomDTO{{Ref: "rt:room:hall", Name: "The Hall", Long: "A hall.", Exits: map[string]string{}}},
			Mobs: []content.ProtoDTO{{
				Ref: "rt:mob:guard", Keywords: []string{"guard"}, Short: "the guard", Long: "A guard stands here.",
				Living: &content.LivingDTO{}, Lua: lua,
			}},
		}},
	}
}

// TestHotReloadMobLuaFullPath drives the WHOLE path: a published invalidation re-reads the edited
// mob `lua`, swaps the prototype (the subscriber goroutine), and posts a reloadLuaMsg to the zone
// (the 7.7 wiring). The zone is NOT Run here (so the test can inspect single-threaded, like the
// other reload tests); we set up the scripted guard, drain the posted reloadLuaMsg via z.handle
// (exactly what Run's loop would do), and assert the live guard picks up the reloaded greeting
// while self.state survives. This proves the bus->reloader->notifyZones->inbox->reloadLua chain.
func TestHotReloadMobLuaFullPath(t *testing.T) {
	src := content.NewMemSource()
	src.SetPack(scriptedReloadPack(`state.v = "v1"; on("greet", function(ev) state.greets = (state.greets or 0) + 1 end)`))
	bus := contentbus.NewMemBus()
	defer bus.Close()

	s := newReloadShard(t, src, bus)
	z := s.Zone()

	// Set up the scripted guard + a player and greet once under v1.
	room := z.spawn("rt:room:hall")
	z.rooms["rt:room:hall"] = room
	guard := z.spawn("rt:mob:guard")
	Move(guard, room)
	player := z.newEntity("rt:player:p")
	Add(player, &Living{})
	player.short = "P"
	Move(player, room)
	z.fireRoomEntry(player, room)
	es := z.lua.entityScripts[guard.rid]
	if es == nil || es.state.RawGetString("v").String() != "v1" {
		t.Fatalf("v1 greet did not register (v=%v)", es)
	}
	if es.state.RawGetString("greets").String() != "1" {
		t.Fatalf("greets after v1 = %s, want 1", es.state.RawGetString("greets").String())
	}

	// Edit the mob's lua + publish the invalidation. The reloader swaps the proto (async, the
	// subscriber goroutine) and posts a reloadLuaMsg to the zone inbox.
	if err := src.EditMobLua("reloadtest", "rt:mob:guard", `on("greet", function(ev) state.greets = (state.greets or 0) + 10 end)`); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(context.Background(), contentbus.Invalidation{Kind: content.KindMob, Ref: "rt:mob:guard", Pack: "reloadtest"}); err != nil {
		t.Fatal(err)
	}
	// Wait for the prototype swap (the subscriber goroutine published the new source).
	waitForProto(t, s, "rt:mob:guard", func(_ *Prototype) bool {
		return z.protoScriptSource("rt:mob:guard") == `on("greet", function(ev) state.greets = (state.greets or 0) + 10 end)`
	})
	// Drain the posted reloadLuaMsg the way Run's loop would (single-threaded inspection).
	drainReloadMsg(t, z)

	// Greet again: the live guard now runs the NEW handler (+10), and self.state (greets=1) survived
	// the reload (so it is 11, not reset to 0 then +10).
	z.fireRoomEntry(player, room)
	es2 := z.lua.entityScripts[guard.rid]
	if got := es2.state.RawGetString("greets").String(); got != "11" {
		t.Fatalf("greets after the live reload = %s, want 11 (old state 1 + new handler +10 — reload took effect AND self.state survived)", got)
	}
}

// drainReloadMsg pulls one reloadLuaMsg off the zone inbox and applies it via z.handle (what Run's
// loop does), so a non-Run test can exercise the on-goroutine reload. Fails if no reload was posted.
func drainReloadMsg(t *testing.T, z *Zone) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-z.inbox:
			if _, ok := m.(reloadLuaMsg); ok {
				z.handle(m)
				return
			}
			z.handle(m) // some other message; apply and keep looking
		case <-deadline:
			t.Fatal("no reloadLuaMsg was posted to the zone (the notifyZones wiring did not fire)")
		}
	}
}
