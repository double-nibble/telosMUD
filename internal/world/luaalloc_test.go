package world

import (
	"strings"
	"testing"
	"time"

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

// TestZoneAllocBudgetChargesTheZoneStringBuiltins. The zone has its own copies of every capped wrapper, so it
// needs its own charge at every one — the parity test compares CONSTANTS, not call sites, so a zone wrapper
// that forgets to charge is invisible to it.
//
// Every case here was a surviving mutation: review deleted each charge in turn and the world suite stayed
// green, because only string.rep was covered.
func TestZoneAllocBudgetChargesTheZoneStringBuiltins(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		// 4096 x 16 KiB = 64 MiB of individually-legal calls, each 1/64th of the per-op cap.
		{"string.rep", `local t = {}
for i = 1, 4096 do t[i] = string.rep("x", 16384) end`},
		{"string.format", `local chunk = string.rep("y", 16384)
local t = {}
for i = 1, 4096 do t[i] = string.format("%s", chunk) end`},
		// The field WIDTH, which was validated against the per-op cap but never charged: a megabyte of
		// padding from an eleven-byte format string and no string arguments.
		{"string.format field width", `local t = {}
for i = 1, 64 do t[i] = string.format("%1000000d", i) end`},
		// The 1:1 transforms, which sat in the "no amplification" passthrough list and allocated 2 GB in one
		// call while charging nothing.
		{"string.reverse", `local s = string.rep("x", 1048576)
local t = {}
for i = 1, 64 do t[i] = string.reverse(s) end`},
		{"string.upper", `local s = string.rep("x", 1048576)
local t = {}
for i = 1, 64 do t[i] = string.upper(s) end`},
		// gsub's FUNCTION-replacement path, whose per-match output guard is a separate call site from the
		// others. Sized for the FEWEST possible Lua re-entries (16 matches x 32 KiB = 512 KiB per gsub, half
		// the per-op cap; 20 calls carry 10 MiB of charge in 320 callbacks). The obvious shape — many small
		// matches — charges the same and costs an order of magnitude more VM work, which under `-race` on CI
		// hardware runs the wall clock out before the budget and fails for the wrong reason.
		{"string.gsub", `local subject = string.rep("a", 16)
local chunk = string.rep("x", 32768)
local t = {}
for i = 1, 20 do t[i] = string.gsub(subject, "a", function(m) return chunk end) end`},
	}
	// A PATIENT deadline for the duration, so that what stops these programs is the allocation budget and not
	// the wall clock. The wall clock is what stopped every one of them BEFORE #438 — at gigabytes of
	// allocation, and classified as transient host load — so at the default 5ms several of these cases pass
	// with the whole change reverted.
	saved := luaCallDeadline
	luaCallDeadline = 5 * time.Second
	t.Cleanup(func() { luaCallDeadline = saved })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z := newDemoZone("midgaard", newProtoCache())
			defer z.lua.close()

			err := z.lua.runChunk("bomb", tc.src)
			if err == nil {
				t.Fatalf("64 MiB of individually-legal %s calls completed on a zone runtime; this wrapper "+
					"does not charge, so a loop of legal operations bypasses the per-call bound", tc.name)
			}
			if !strings.Contains(err.Error(), "string allocation budget exceeded") {
				t.Fatalf("stopped for the wrong reason: %v", err)
			}
		})
	}
}

// TestZoneAllocAbortCostsTheZoneBreaker is the world's twin of the luasandbox breaker test, and it closes a
// mutation that survived: deleting luaAlloc from luabreaker's weight switch left the ENTIRE world suite green.
//
// The switch has no default arm, so an unwired kind adds ZERO to the budget while still incrementing the
// failure count — and the trip test reads the budget. A script that bombs memory on every single call would
// therefore never be quarantined. This is the production path; internal/luasandbox's breaker is the
// director's.
func TestZoneAllocAbortCostsTheZoneBreaker(t *testing.T) {
	// A bare runtime is enough: breakerRecord only touches rt.breakers.
	alloc := &luaRuntime{breakers: map[string]*breakerState{}}
	budget := &luaRuntime{breakers: map[string]*breakerState{}}
	for i := 0; i < 4; i++ {
		alloc.breakerRecord("s", "origin", luaAlloc)
		budget.breakerRecord("s", "origin", luaBudget)
	}
	got, want := alloc.breakers["s"].budget, budget.breakers["s"].budget
	if got == 0 {
		t.Fatal("four allocation aborts cost the ZONE breaker nothing. luabreaker's switch has no default " +
			"arm, so an unwired kind is free: a script bombing memory on every call is never quarantined")
	}
	if got != want {
		t.Fatalf("an allocation abort costs %v where an instruction abort costs %v; both are deterministic "+
			"and content-pathological and must weigh the same", got, want)
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
