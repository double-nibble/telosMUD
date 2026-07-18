package world

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/contentbus"
)

// instance_reload_test.go — #411's RELOAD FREEZE: a live instance is pinned to the content it was minted
// from, and the operator is told so.
//
// A running instance is a party's in-progress dungeon run. A zone-shape reconcile is a diff-and-CONVERGE
// that tears down rooms absent from the desired set, so applied mid-run a builder's edit deletes the room
// the party is standing in. The instanced semantic is "the run you started is the run you finish"; an
// instance is short-lived, so the edit lands on the next mint anyway.
//
// Both halves are asserted because they failed differently before the fix: the reconcile was skipped only by
// ACCIDENT (an instance's id is `<template>#<serial>`, so it never equals the invalidation's ref and the
// match loop happened to miss it), while the Lua fan-out was NOT skipped at all — notifyZones broadcasts by
// zone list, not by ref, so a reload really did recompile a running instance's scripts. That left the worst
// of the three possible states: new scripts over the old room graph.

// TestReloadFreezesAnInstance covers both directions of the freeze against one shard.
//
// The zones are adopted but NOT run, exactly as TestReconcileSkipsLocalZone does it: the assertion is on what
// reaches a zone's inbox, and a live actor would drain it out from under the count. Nothing here needs a
// running zone — both guards are decisions the reloader goroutine makes before it ever posts.
func TestReloadFreezesAnInstance(t *testing.T) {
	sh := newBareShard("midgaard", "", nil, nil)
	template := newZone("darkwood")
	sh.adopt("darkwood", template)
	inst := newInstanceZone("darkwood#deadbeef", "darkwood")
	sh.adopt(inst.id, inst)
	r := &reloader{shard: sh, log: slog.Default()}

	// --- the shape reconcile -------------------------------------------------------------------------------
	inv := contentbus.Invalidation{
		Kind: content.KindZone, Ref: "darkwood", Rooms: []string{"darkwood:room:grove"}, Version: 99,
	}
	r.reconcileZone(inv)
	if n := len(inst.inbox); n != 0 {
		t.Fatalf("a zone-shape reconcile was posted to a live INSTANCE (%d msg(s)): the reconcile tears down "+
			"rooms absent from the desired set, so a builder's mid-run edit deletes the room a party is "+
			"standing in", n)
	}
	// The CONTROL: the AUTHORED zone still reconciles, so the assertion above is about instancing.
	if len(template.inbox) == 0 {
		t.Fatal("the authored darkwood did not receive its reconcile either — the freeze is too broad")
	}
	// --- the Lua fan-out -----------------------------------------------------------------------------------
	// This is the half that was NOT an accident: notifyZones fans out over the whole hosted-zone list, so
	// before the fix a reload recompiled a running instance's scripts and re-registered its live handlers —
	// new scripts over the old room graph the reconcile above correctly refused to touch.
	r.notifyZones(content.KindMob, "darkwood:mob:goblin-chief")
	if n := len(inst.inbox); n != 0 {
		t.Fatalf("a Lua reload was posted to a live INSTANCE (%d msg(s)): combined with the frozen room graph "+
			"that leaves a party mid-run with new scripts over old rooms", n)
	}
	if len(template.inbox) == 0 {
		t.Fatal("the authored darkwood did not receive its Lua reload either — the freeze is too broad")
	}
	if n := len(inst.inbox); n != 0 {
		t.Fatalf("the instance's inbox holds %d msg(s) at the end; nothing from a reload may reach it", n)
	}
}

// TestReloadAdvisoryNamesPinnedInstances. The freeze above is correct and INVISIBLE: a builder edits a
// dungeon, the readout reports success, and every party currently inside keeps playing the old version with
// nothing to explain why. The advisory (#309's shape) is the only signal they get.
func TestReloadAdvisoryNamesPinnedInstances(t *testing.T) {
	sh, cancel := runningShard(t, []string{"midgaard", "darkwood"}, "midgaard")
	defer cancel()

	full := []content.Pack{{
		Pack:  "demo",
		Zones: []content.ZoneDTO{{Ref: "darkwood"}, {Ref: "midgaard"}},
	}}
	scope := newReloadScope(full, map[string]bool{"demo": true})

	// No instances yet: no advisory. Without this the assertion below could pass on a line that is always
	// emitted.
	if got := pinnedInstanceAdvisories(sh, scope); len(got) != 0 {
		t.Fatalf("advisories with no live instances: %v", got)
	}

	mustMint(t, sh, "darkwood", "acct-1")
	mustMint(t, sh, "darkwood", "acct-2")

	got := pinnedInstanceAdvisories(sh, scope)
	if len(got) != 1 {
		t.Fatalf("pinnedInstanceAdvisories = %v, want exactly one line naming darkwood", got)
	}
	if !strings.Contains(got[0], "darkwood") || !strings.Contains(got[0], "2 live instance") {
		t.Fatalf("advisory %q does not name the zone and its live-instance COUNT; the count is what lets an "+
			"operator decide whether to wait for the reaper", got[0])
	}

	// Scoped like the #309 advisories: reloading an UNRELATED pack must not narrate every dungeon on the shard.
	other := newReloadScope(full, map[string]bool{"some-other-pack": true})
	if got := pinnedInstanceAdvisories(sh, other); len(got) != 0 {
		t.Fatalf("an out-of-scope reload produced instance advisories: %v", got)
	}
	// A nil shard (a bare validate-only reloader) yields nothing rather than panicking.
	if got := pinnedInstanceAdvisories(nil, scope); got != nil {
		t.Fatalf("pinnedInstanceAdvisories(nil shard) = %v, want nil", got)
	}
}
