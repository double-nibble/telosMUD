package world

import (
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/luasandbox"
)

// luacaps_test.go — #368: the operator-tunable Lua caps reaching the world's own sandbox.

// TestSetLuaCapsAppliesAndClamps. The world builds its own Lua runtime rather than using luasandbox's
// Runtime, so it needs its own injection point — and a tunable that parses, validates and is then dropped is
// exactly the silent misconfiguration this issue is about.
func TestSetLuaCapsAppliesAndClamps(t *testing.T) {
	budget, deadline := luaInstrBudget, luaCallDeadline
	t.Cleanup(func() { luaInstrBudget, luaCallDeadline = budget, deadline })

	if err := SetLuaCaps(250_000, 25); err != nil {
		t.Fatalf("a valid pair was refused: %v", err)
	}
	if luaInstrBudget != 250_000 {
		t.Fatalf("luaInstrBudget = %d, want 250000", luaInstrBudget)
	}
	if luaCallDeadline != 25*time.Millisecond {
		t.Fatalf("luaCallDeadline = %v, want 25ms", luaCallDeadline)
	}

	// Zero means the compiled-in default, NEVER unlimited. A safety bound that could be switched off by
	// omitting a config field would be the wrong failure direction.
	if err := SetLuaCaps(0, 0); err != nil {
		t.Fatalf("the defaults were refused: %v", err)
	}
	if luaInstrBudget != luasandbox.InstrBudget {
		t.Fatalf("a zero budget gave %d, want the engine default %d", luaInstrBudget, luasandbox.InstrBudget)
	}
	if want := time.Duration(luasandbox.CallDeadline) * time.Millisecond; luaCallDeadline != want {
		t.Fatalf("a zero deadline gave %v, want the build default %v", luaCallDeadline, want)
	}

	// A configuration the engine cannot honor is REFUSED, and — the part worth pinning — refused without
	// changing anything. A setter that validated and then applied anyway, or applied and then errored, would
	// leave the process running caps nobody chose.
	before := luaInstrBudget
	if err := SetLuaCaps(luasandbox.MaxInstrBudget, 0); err == nil {
		t.Fatal("a budget the default deadline can never reach was accepted; the wall clock would fire first, " +
			"so the budget would bound nothing AND the breaker would stop quarantining runaway scripts")
	}
	if luaInstrBudget != before {
		t.Fatalf("a REFUSED SetLuaCaps still moved the budget to %d; a rejected configuration must change "+
			"nothing", luaInstrBudget)
	}
}

// TestLuaCapsAreActuallyArmedOnAZone is the wiring half: the configured budget must reach the VM a zone
// actually runs scripts in, not just the package var.
func TestLuaCapsAreActuallyArmedOnAZone(t *testing.T) {
	budget, deadline := luaInstrBudget, luaCallDeadline
	t.Cleanup(func() { luaInstrBudget, luaCallDeadline = budget, deadline })

	if err := SetLuaCaps(luasandbox.MinInstrBudget, 0); err != nil {
		t.Fatalf("SetLuaCaps: %v", err)
	}
	z := newDemoZone("midgaard", newProtoCache())
	defer z.lua.close()

	// A loop the default budget runs comfortably, which the configured floor must abort.
	err := z.lua.runChunk("spin", "for i = 1, 1000000 do end")
	if err == nil {
		t.Fatal("a million-iteration loop completed under a 1000-instruction budget; the configured cap never " +
			"reached the zone's VM")
	}
	if got := luasandbox.ClassifyError(err); got != luasandbox.AbortBudget {
		t.Fatalf("abort classified as %v, want AbortBudget", got)
	}
}

// TestSetLuaCapsRefusesADeadlineThatOutlivesAPulse is the host-specific bound, and the reason validation was
// moved to the injection point in the first place.
//
// luasandbox's own ceiling is a full second — it is host-agnostic and cannot know the tick. But a zone pulses
// every 250ms, so a call allowed to run a second swallows four consecutive heartbeats: combat rounds stop
// landing and affect ticks stop firing for every player in that zone while one script runs. Only the world
// can make that check.
func TestSetLuaCapsRefusesADeadlineThatOutlivesAPulse(t *testing.T) {
	budget, deadline := luaInstrBudget, luaCallDeadline
	t.Cleanup(func() { luaInstrBudget, luaCallDeadline = budget, deadline })

	ms := int(pulseInterval / time.Millisecond)
	if err := SetLuaCaps(0, ms); err == nil {
		t.Fatalf("a %dms deadline was accepted at a %v pulse; one call could then swallow a whole heartbeat",
			ms, pulseInterval)
	}
	// Comfortably under the pulse is fine — the bound must not be a blanket refusal of any raise.
	if err := SetLuaCaps(0, ms/5); err != nil {
		t.Fatalf("a deadline well under the pulse was refused: %v", err)
	}
}
