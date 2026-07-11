package world

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
)

// toggledefs_test.go — content-defined player toggles (#358). Covers: the verb dispatch (report + flip),
// persistence (durable round-trip) + cross-shard handoff carry, the Lua self:toggle read (default vs
// override), the built-in-collision rejection, and the empty-pack invariant (no toggle => no verb).

// registerOverworldToggle installs the demo `overworld` toggle (default off) into a zone's registry, the
// same shape buildToggleDef produces from content.
func registerOverworldToggle(z *Zone) {
	z.defs.toggle.register("overworld", &toggleDef{
		ref: "overworld", name: "Overworld map", words: []string{"overworld"},
		defaultOn: false, desc: "Use `overworld on|off`.",
	})
}

// TestToggleVerbReportsAndFlips drives the verb through dispatch: bare `overworld` reports OFF (the
// default), `overworld on` flips + confirms and records the override, `overworld off` clears it.
func TestToggleVerbReportsAndFlips(t *testing.T) {
	z := newZone("test")
	registerOverworldToggle(z)
	s := newTestPlayerEntity(z, "Wanderer")

	z.dispatch(s, "overworld")
	if !drainContains(t, s, "OFF") {
		t.Fatal("bare `overworld` did not report the default-OFF state")
	}
	z.dispatch(s, "overworld on")
	if !drainContains(t, s, "ON") {
		t.Fatal("`overworld on` did not confirm ON")
	}
	if on, ok := commsOf(s).toggleOverride["overworld"]; !ok || !on {
		t.Fatalf("override after `overworld on` = (%v,%v), want (true,true)", on, ok)
	}
	z.dispatch(s, "overworld off")
	if on, ok := commsOf(s).toggleOverride["overworld"]; !ok || on {
		t.Fatalf("override after `overworld off` = (%v,%v), want (false,true)", on, ok)
	}
}

// TestToggleStateRoundTrip proves the toggle override rides the same StateJSON subtree as channel
// overrides: dump -> marshal -> unmarshal -> load reconstructs it (persistence across relog / rehydrate).
func TestToggleStateRoundTrip(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Saver")
	commsOf(s).toggleOverride["overworld"] = true

	dumped := dumpCommsState(s)
	if dumped == nil {
		t.Fatal("dumpCommsState returned nil for a set toggle override")
	}
	raw, err := marshalCommsState(dumped)
	if err != nil {
		t.Fatalf("marshalCommsState: %v", err)
	}
	reloaded, err := unmarshalCommsState(raw)
	if err != nil {
		t.Fatalf("unmarshalCommsState: %v", err)
	}
	s2 := newTestPlayerEntity(z, "Saver")
	loadCommsState(s2, reloaded)
	if on, ok := s2.comms.toggleOverride["overworld"]; !ok || !on {
		t.Fatalf("reloaded toggle override = (%v,%v), want (true,true)", on, ok)
	}
}

// TestToggleSurvivesHandoff proves the override rides the cross-shard handoff carry (the JSON snapshot),
// not just the durable save: dumpCommsStateJSON -> loadCommsStateJSON reconstructs it on the destination.
func TestToggleSurvivesHandoff(t *testing.T) {
	z := newZone("test")
	src := newTestPlayerEntity(z, "Walker")
	commsOf(src).toggleOverride["overworld"] = true

	raw := dumpCommsStateJSON(src)
	if raw == "" {
		t.Fatal("dumpCommsStateJSON produced no snapshot for a set toggle")
	}
	dst := newTestPlayerEntity(z, "Walker")
	loadCommsStateJSON(dst, raw)
	if dst.comms == nil {
		t.Fatal("loadCommsStateJSON installed no state on the destination")
	}
	if on, ok := dst.comms.toggleOverride["overworld"]; !ok || !on {
		t.Fatalf("handoff-carried toggle = (%v,%v), want (true,true)", on, ok)
	}
}

// TestToggleLuaReadDefaultAndOverride proves self:toggle("<ref>") reads the default_on when untouched and
// the override once set — the exact read the overworld `room` display template gates on. An unknown ref
// and a non-player subject read false / the plain default.
func TestToggleLuaReadDefaultAndOverride(t *testing.T) {
	z := newZone("test")
	// A default-ON toggle so the default path is observable as true, plus the default-OFF overworld one.
	z.defs.toggle.register("hud", &toggleDef{ref: "hud", name: "HUD", words: []string{"hud"}, defaultOn: true})
	registerOverworldToggle(z)
	s := newTestPlayerEntity(z, "Viewer")
	// The viewer must be resolvable via the zone's containment walk (resolveHandle) — place them in a room,
	// as they are in the real display-template render.
	room := z.newEntity("test:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["test:room:hall"] = room
	Move(s.entity, room)

	// Untouched: overworld reads its default (false), hud reads its default (true).
	runSelf(t, z.lua, s.entity, `
		assert(self:toggle("overworld") == false, "overworld default should be false")
		assert(self:toggle("hud") == true, "hud default should be true")
		assert(self:toggle("nonesuch") == false, "unknown toggle reads false")
	`)
	// Flip overworld on via the override; the Lua read now sees true.
	commsOf(s).toggleOverride["overworld"] = true
	runSelf(t, z.lua, s.entity, `assert(self:toggle("overworld") == true, "override should read true")`)
}

// TestToggleVerbCollisionRejected proves a toggle word that collides with a BUILT-IN verb is dropped from
// the registered def (never shadowing a core verb), while its non-colliding words survive.
func TestToggleVerbCollisionRejected(t *testing.T) {
	d := newDefRegistries()
	defineGlobals(d, &content.LoadedContent{ToggleDefs: []content.ToggleDTO{
		{Ref: "mv", Name: "Move", Words: []string{"north", "overworld"}}, // "north" is a built-in movement verb
	}})
	def := d.toggle.get("mv")
	if def == nil {
		t.Fatal("toggle def `mv` was not registered")
	}
	for _, w := range def.words {
		if w == "north" {
			t.Fatal("the built-in-colliding word `north` was NOT rejected from the toggle def")
		}
	}
	if len(def.words) != 1 || def.words[0] != "overworld" {
		t.Fatalf("kept words = %v, want [overworld]", def.words)
	}
}

// TestEmptyPackNoToggleVerbs proves the empty-boot invariant: a zone with no toggle_defs resolves no
// toggle verb (so a pack that ships none adds nothing to the command surface).
func TestEmptyPackNoToggleVerbs(t *testing.T) {
	z := newZone("test")
	if def := z.toggleForVerb("overworld"); def != nil {
		t.Fatalf("bare zone resolved a toggle verb: %v", def.ref)
	}
}
