package world

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// setScope is a test helper: apply a world/region delta to a zone's replica directly (the same path the
// shard subscription drives, minus the bus). Runs on the test goroutine, which owns the un-Run zone.
func setScope(z *Zone, kind, key string, value any) {
	raw, _ := json.Marshal(value)
	z.applyScopeDelta(scopeDeltaMsg{kind: kind, key: key, value: raw})
}

// TestScopeLuaReads proves the Lua read surface (world.flag/world.get/region:get/region.id) reflects the
// zone's replica — the cached, synchronous, lock-free reads a zone script does.
func TestScopeLuaReads(t *testing.T) {
	z := newZone("midgaard")
	z.scopes.regionID = "heartlands"
	setScope(z, "world", "invasion_active", true)
	setScope(z, "world", "invasion_phase", 2)
	setScope(z, "region", "mood", "tense")

	results := map[string]lua.LValue{}
	z.lua.L.SetGlobal("__cap", z.lua.L.NewFunction(func(l *lua.LState) int {
		results[l.CheckString(1)] = l.CheckAny(2)
		return 0
	}))
	src := `
		__cap("flag_on",  world.flag("invasion_active"))
		__cap("flag_off", world.flag("not_set"))
		__cap("phase",    world.get("invasion_phase"))
		__cap("missing",  world.get("nope"))
		__cap("mood",     region:get("mood"))
		__cap("rmissing", region:get("nope"))
		__cap("rid",      region.id())
	`
	if err := z.lua.runChunk("scope", src); err != nil {
		t.Fatal(err)
	}

	if v := results["flag_on"]; v != lua.LTrue {
		t.Fatalf("world.flag(set) = %v, want true", v)
	}
	if v := results["flag_off"]; v != lua.LFalse {
		t.Fatalf("world.flag(unset) = %v, want false", v)
	}
	if v, ok := results["phase"].(lua.LNumber); !ok || v != 2 {
		t.Fatalf("world.get(phase) = %v, want 2", results["phase"])
	}
	if results["missing"] != lua.LNil {
		t.Fatalf("world.get(absent) = %v, want nil", results["missing"])
	}
	if v, ok := results["mood"].(lua.LString); !ok || string(v) != "tense" {
		t.Fatalf("region:get(mood) = %v, want tense", results["mood"])
	}
	if results["rmissing"] != lua.LNil {
		t.Fatalf("region:get(absent) = %v, want nil", results["rmissing"])
	}
	if v, ok := results["rid"].(lua.LString); !ok || string(v) != "heartlands" {
		t.Fatalf("region.id() = %v, want heartlands", results["rid"])
	}
}

// TestScopeRegionlessZoneReadsNil proves a zone in no region sees no region state (region:get -> nil,
// region.id() -> nil) even if a region delta were mis-delivered (applyScopeDelta ignores it).
func TestScopeRegionlessZoneReadsNil(t *testing.T) {
	z := newZone("crypt")                 // region-less (regionID stays "")
	setScope(z, "region", "mood", "grim") // ignored — no region

	var rid, mood lua.LValue
	z.lua.L.SetGlobal("__rid", z.lua.L.NewFunction(func(l *lua.LState) int { rid = l.CheckAny(1); return 0 }))
	z.lua.L.SetGlobal("__mood", z.lua.L.NewFunction(func(l *lua.LState) int { mood = l.CheckAny(1); return 0 }))
	if err := z.lua.runChunk("scope", `__rid(region.id()); __mood(region:get("mood"))`); err != nil {
		t.Fatal(err)
	}
	if rid != lua.LNil {
		t.Fatalf("region.id() on a region-less zone = %v, want nil", rid)
	}
	if mood != lua.LNil {
		t.Fatalf("region:get on a region-less zone = %v, want nil (a mis-routed region delta must be ignored)", mood)
	}
}

// TestScopeDeleteClearsFlag proves a delta with a nil/null value DELETES the key (a flag cleared by the
// director), so world.flag goes back to false.
func TestScopeDeleteClearsFlag(t *testing.T) {
	z := newZone("midgaard")
	setScope(z, "world", "invasion_active", true)
	z.applyScopeDelta(scopeDeltaMsg{kind: "world", key: "invasion_active", value: nil}) // delete

	var flag lua.LValue
	z.lua.L.SetGlobal("__f", z.lua.L.NewFunction(func(l *lua.LState) int { flag = l.CheckAny(1); return 0 }))
	if err := z.lua.runChunk("scope", `__f(world.flag("invasion_active"))`); err != nil {
		t.Fatal(err)
	}
	if flag != lua.LFalse {
		t.Fatalf("world.flag after delete = %v, want false", flag)
	}
}

// TestScopeReplicationRouting proves the shard's bus subscription routes a director broadcast to the
// right zones: a WORLD delta reaches every hosted zone; a REGION delta reaches only that region's member
// zones (heartlands = midgaard+darkwood; the crypt, region-less, gets none). End-to-end over the real
// scopebus (a MemBus transport) — the zones are NOT Run, so the test reads what landed on each inbox.
func TestScopeReplicationRouting(t *testing.T) {
	regions, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}
	s := NewMultiShard([]string{"midgaard", "darkwood", "crypt"}, "midgaard", "", nil, nil)
	bus := scopebus.New(commbus.NewMemBus())
	s.WithScopeBus(bus, regions.Regions)
	if s.scopes == nil {
		t.Fatal("WithScopeBus did not wire scope replication")
	}
	// midgaard + darkwood are stamped with the heartlands region; crypt is region-less.
	if s.zones["midgaard"].scopes.regionID != "heartlands" || s.zones["darkwood"].scopes.regionID != "heartlands" {
		t.Fatalf("member zones not stamped with region: mid=%q dark=%q",
			s.zones["midgaard"].scopes.regionID, s.zones["darkwood"].scopes.regionID)
	}
	if s.zones["crypt"].scopes.regionID != "" {
		t.Fatalf("crypt should be region-less, got %q", s.zones["crypt"].scopes.regionID)
	}

	s.scopes.start()
	defer s.scopes.stop()
	ctx := context.Background()

	mustSignal := func(scope scopebus.Scope, key string, value any) {
		raw, _ := json.Marshal(value)
		p, _ := json.Marshal(scopeStatePayload{Key: key, Value: raw})
		if err := bus.Signal(ctx, scope, scopeEventStateSet, p, "world-director"); err != nil {
			t.Fatal(err)
		}
	}
	mustSignal(scopebus.World(), "world_flag", true)
	mustSignal(scopebus.Region("heartlands"), "mood", "tense")

	// A world delta must reach ALL three zones; the heartlands region delta only midgaard + darkwood.
	wantWorld := map[string]bool{"midgaard": true, "darkwood": true, "crypt": true}
	wantRegion := map[string]bool{"midgaard": true, "darkwood": true, "crypt": false}
	for _, zid := range []string{"midgaard", "darkwood", "crypt"} {
		gotWorld, gotRegion := drainScopeDeltas(t, s.zones[zid])
		if gotWorld != wantWorld[zid] {
			t.Fatalf("%s world delta delivered=%v, want %v", zid, gotWorld, wantWorld[zid])
		}
		if gotRegion != wantRegion[zid] {
			t.Fatalf("%s region delta delivered=%v, want %v (region isolation)", zid, gotRegion, wantRegion[zid])
		}
	}
}

// drainScopeDeltas reads the scopeDeltaMsgs that landed on a (not-Run) zone's inbox within a short
// window, reporting whether a world and a region delta arrived. Non-scope messages are ignored.
func drainScopeDeltas(t *testing.T, z *Zone) (world, region bool) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case m := <-z.inbox:
			if d, ok := m.(scopeDeltaMsg); ok {
				switch d.kind {
				case "world":
					world = true
				case "region":
					region = true
				}
			}
		case <-deadline:
			return world, region
		case <-time.After(150 * time.Millisecond):
			// no more messages arriving — settle.
			return world, region
		}
	}
}
