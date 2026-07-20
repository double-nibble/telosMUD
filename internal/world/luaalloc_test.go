package world

import (
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/luasandbox"
)

// luaalloc_test.go — #438: the zone sandbox's half of the per-call string-allocation budget.
//
// The zone builds its OWN VM (newLuaRuntime), so every cap has to be armed twice. TestSandboxCapConstantsShared
// keeps the two sets of CONSTANTS equal, and that is exactly the test this file exists to complement: a zone
// that defines luaStrAllocCap and never calls SetStringByteBudget passes it, because the constants still
// match. Both zone sandboxes would then abort the bomb anyway — on the wall clock — so nothing else notices.

// TestZoneArmsTheAllocBudget asks the constructed zone VM directly, which is the only observation that
// distinguishes "armed" from "the charge is a silent no-op".
func TestZoneArmsTheAllocBudget(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	defer z.lua.close()

	if !z.lua.L.ChargeStringBytes(1) {
		t.Fatal("charging a single byte failed on a fresh zone runtime")
	}
	if got := z.lua.L.StringBytesCharged(); got != 1 {
		t.Fatalf("StringBytesCharged = %d after charging 1 byte; the zone never armed the string budget, so "+
			"the cap is inert on every zone in the world while the shared constant says otherwise", got)
	}
}

// TestZoneAllocBudgetBoundsTheConcatBomb is the behavioral half, run on a real zone runtime.
//
// It asserts the CLASSIFICATION rather than merely that an error came back, because the bomb aborted before
// #438 too — on the deadline. An `err != nil` assertion here passes with the whole change reverted.
func TestZoneAllocBudgetBoundsTheConcatBomb(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	defer z.lua.close()

	err := z.lua.runChunk("bomb", `local s = string.rep("x", 1048576)
for i = 1, 40 do s = s .. s end`)
	if err == nil {
		t.Fatal("the doubling bomb completed on a zone runtime")
	}
	if !strings.Contains(err.Error(), "string allocation budget exceeded") {
		t.Fatalf("the zone stopped the bomb some other way (%v). Before #438 that was the wall clock, at "+
			"64 GB of allocation and weighted as transient host load", err)
	}
	if got := luasandbox.ClassifyError(err); got != luasandbox.AbortAlloc {
		t.Fatalf("classified %v, want AbortAlloc", got)
	}
	if got := classifyLuaError(err); got != luaAlloc {
		t.Fatalf("the ZONE's classification is %v, want luaAlloc. The zone maps luasandbox's kinds onto its "+
			"own enum, and its default arm is luaLogicErr — so an unmapped kind is silently weighted 1.0", got)
	}
}

// TestZoneAllocBudgetChargesTheZoneStringBuiltins. The zone has its own copies of the capped amplifier
// wrappers, so it needs its own charge — the parity test compares constants, not call sites.
func TestZoneAllocBudgetChargesTheZoneStringBuiltins(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	defer z.lua.close()

	// 4096 x 16 KiB = 64 MiB of individually-legal reps, each 1/64th of the per-op cap.
	err := z.lua.runChunk("reps", `local t = {}
for i = 1, 4096 do t[i] = string.rep("x", 16384) end`)
	if err == nil {
		t.Fatal("64 MiB of individually-legal string.rep calls completed on a zone runtime; the zone's " +
			"wrappers do not charge, so a loop of legal operations bypasses the per-call bound")
	}
	if !strings.Contains(err.Error(), "string allocation budget exceeded") {
		t.Fatalf("stopped for the wrong reason: %v", err)
	}
}

// TestZoneLegitimateStringBuildingIsUnaffected — the false-positive net. Quarantining correct content is
// worse than the bomb this bounds.
func TestZoneLegitimateStringBuildingIsUnaffected(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	defer z.lua.close()

	err := z.lua.runChunk("legit", `local out = {}
for i = 1, 200 do
  out[#out+1] = string.format("%s stands here, %s.", "A tall guard", "watching the gate")
end
local s = table.concat(out, "\n")
if #s < 1000 then error("fixture built nothing") end`)
	if err != nil {
		t.Fatalf("ordinary content string-building tripped the zone's allocation budget: %v", err)
	}
}
