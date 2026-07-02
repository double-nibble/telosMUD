package world

import (
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// luagmcp_test.go — #51 gates for the `gmcp` sandbox module: a custom Mud.* frame reaches the target's
// session, the namespace allowlist blocks engine-package spoofing, invalid names / unsupported values /
// unbounded tables fail closed, and a non-player target is a clean no-op.

// runGMCP binds the player handle as `p`, captures the boolean return via `__ok`, and runs one gmcp.send
// chunk. Returns the captured return value and the (possibly nil) script error.
func runGMCP(t *testing.T, z *Zone, target *Entity, code string) (bool, error) {
	t.Helper()
	rt := z.lua
	// resolveHandle re-resolves via a z.rooms containment walk (T7); makeRoomPlayer doesn't register its
	// room, so register the target's room here — otherwise the handle resolves to nil and every send no-ops.
	if r := target.location; r != nil && Has[*Room](r) {
		z.rooms[r.proto] = r
	}
	rt.L.SetGlobal("p", rt.newHandle(target))
	var ret bool
	rt.L.SetGlobal("__ok", rt.L.NewFunction(func(l *lua.LState) int {
		ret = l.ToBool(1)
		return 0
	}))
	return ret, rt.runChunk("gmcp", code)
}

func TestGMCPSendDeliversMudFrame(t *testing.T) {
	z, caster := abilityTestZone(t)
	ok, err := runGMCP(t, z, caster.entity, `__ok(gmcp.send(p, "Mud.Quest", {name="Slay the dragon", step=2, done=false}))`)
	if err != nil {
		t.Fatalf("gmcp.send errored: %v", err)
	}
	if !ok {
		t.Fatal("gmcp.send to a live player session returned false")
	}
	frame, sent := drainGMCP(caster)["Mud.Quest"]
	if !sent {
		t.Fatal("no Mud.Quest GMCP frame reached the session")
	}
	for _, want := range []string{`"name":"Slay the dragon"`, `"step":2`, `"done":false`} {
		if !strings.Contains(frame, want) {
			t.Errorf("Mud.Quest payload missing %s: %s", want, frame)
		}
	}
}

func TestGMCPSendBarePackageEmitsEmptyObject(t *testing.T) {
	z, caster := abilityTestZone(t)
	if _, err := runGMCP(t, z, caster.entity, `gmcp.send(p, "Mud.Ping")`); err != nil {
		t.Fatalf("bare gmcp.send errored: %v", err)
	}
	if frame := drainGMCP(caster)["Mud.Ping"]; frame != "{}" {
		t.Fatalf("bare gmcp.send payload = %q, want {}", frame)
	}
}

// TestGMCPSendRejectsEngineNamespace is the headline security gate: content CANNOT spoof an engine
// package (Char/Core/Room/Comm) — the namespace allowlist rejects it BEFORE any frame is built.
func TestGMCPSendRejectsEngineNamespace(t *testing.T) {
	z, caster := abilityTestZone(t)
	for _, pkg := range []string{"Char.Vitals", "Core.Goodbye", "Room.Info", "Comm.Channel.Text"} {
		_, err := runGMCP(t, z, caster.entity, `gmcp.send(p, "`+pkg+`", {spoof=true})`)
		if err == nil {
			t.Fatalf("gmcp.send(%q) was allowed — content spoofed an engine package", pkg)
		}
		if _, sent := drainGMCP(caster)[pkg]; sent {
			t.Fatalf("a spoofed %q frame was emitted despite the allowlist", pkg)
		}
	}
}

func TestGMCPSendRejectsBadNamesAndValues(t *testing.T) {
	z, caster := abilityTestZone(t)
	cases := map[string]string{
		"space in name":     `gmcp.send(p, "Mud Quest", {})`,
		"control byte":      "gmcp.send(p, \"Mud.\\009x\", {})",
		"leading dot":       `gmcp.send(p, ".Mud", {})`,
		"non-table payload": `gmcp.send(p, "Mud.X", "not a table")`,
		"function value":    `gmcp.send(p, "Mud.X", {fn=function() end})`,
	}
	for name, code := range cases {
		if _, err := runGMCP(t, z, caster.entity, code); err == nil {
			t.Errorf("%s: gmcp.send should have failed closed", name)
		}
	}
}

// TestGMCPSendRejectsUnboundedTable proves the DoS guard: a self-referential (cyclic) table can't drive
// unbounded recursion — the depth cap turns it into a clean error, no frame emitted.
func TestGMCPSendRejectsUnboundedTable(t *testing.T) {
	z, caster := abilityTestZone(t)
	_, err := runGMCP(t, z, caster.entity, `local t = {}; t.self = t; gmcp.send(p, "Mud.Cycle", t)`)
	if err == nil {
		t.Fatal("a cyclic payload table was accepted (unbounded-recursion guard failed)")
	}
	if _, sent := drainGMCP(caster)["Mud.Cycle"]; sent {
		t.Fatal("a frame was emitted from a rejected cyclic table")
	}
}

// TestGMCPSendNonPlayerNoop: sending to a mob (session-less) handle is a clean no-op returning false —
// never an error, since content routinely targets whatever handle it holds.
func TestGMCPSendNonPlayerNoop(t *testing.T) {
	z, caster := abilityTestZone(t)
	mob := z.newEntity("test:mob")
	Add(mob, &Living{})
	Move(mob, caster.entity.location)
	ok, err := runGMCP(t, z, mob, `__ok(gmcp.send(p, "Mud.Quest", {x=1}))`)
	if err != nil {
		t.Fatalf("gmcp.send to a mob errored (should be a clean no-op): %v", err)
	}
	if ok {
		t.Fatal("gmcp.send to a session-less handle returned true")
	}
}

func TestValidCustomGMCPPackage(t *testing.T) {
	valid := []string{"Mud", "Mud.Quest", "Mud.Combat.Round", "Mud.A1.b2"}
	for _, p := range valid {
		if !validCustomGMCPPackage(p) {
			t.Errorf("validCustomGMCPPackage(%q) = false, want true", p)
		}
	}
	invalid := []string{
		"", "Char", "Char.Vitals", "Core", "Room.Info", "Comm.Channel.Text", // reserved / not allowlisted
		"mud", "MUD", // case-sensitive: only "Mud" is allowlisted
		".Mud", "Mud.", "Mud Quest", "Mud\x1b[m", "Mud\x00", strings.Repeat("Mud.", 20), // charset/length
	}
	for _, p := range invalid {
		if validCustomGMCPPackage(p) {
			t.Errorf("validCustomGMCPPackage(%q) = true, want false", p)
		}
	}
}
