package director

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/luasandbox"
)

// directorlua_caps_test.go — #368: the director tier's half of the Lua tunables.
//
// The director runs the world-director script (#47) in its own luasandbox Runtime, so the operator's caps
// have to reach it too — and without this the two Opts fields could be deleted and nothing would fail.

// TestDirectorLuaCapsReachTheRuntime pins the injection behaviorally: a configured floor budget must abort a
// script the default budget runs comfortably.
func TestDirectorLuaCapsReachTheRuntime(t *testing.T) {
	restoreLuaCaps(t)

	if err := SetLuaCaps(luasandbox.MinInstrBudget, 0); err != nil {
		t.Fatalf("SetLuaCaps: %v", err)
	}
	_, err := newLuaDirector(nil, worldScriptKey, "for i = 1, 1000000 do end\nfunction on_signal() end")
	if err == nil {
		t.Fatal("a million-iteration top level ran under a 1000-instruction budget; the configured cap never " +
			"reached the director's runtime")
	}
	if got := luasandbox.ClassifyError(err); got != luasandbox.AbortBudget {
		t.Fatalf("abort classified as %v, want AbortBudget", got)
	}
}

// TestDirectorSetLuaCapsRefusesAnUnhonorablePair. Validation lives at the injection point precisely so a host
// cannot apply caps without validating them — so the director's setter must refuse the same pairs the world's
// does, and refuse without changing anything.
func TestDirectorSetLuaCapsRefusesAnUnhonorablePair(t *testing.T) {
	restoreLuaCaps(t)

	directorLuaInstrBudget = 0
	if err := SetLuaCaps(luasandbox.MaxInstrBudget, 0); err == nil {
		t.Fatal("the director accepted a budget its deadline can never reach; the two tiers must not be able " +
			"to drift into different postures")
	}
	if directorLuaInstrBudget != 0 {
		t.Fatalf("a refused SetLuaCaps still applied (%d)", directorLuaInstrBudget)
	}
}

// restoreLuaCaps saves and restores the package-level Lua caps AND the capsFrozen latch (#356) around a
// test. The latch is what makes SetLuaCaps refuse to run after a VM exists, so any earlier test in the
// package that compiled a script would otherwise make these tests fail purely on ordering. Unlatching here
// is a test-only affordance; production has exactly one boot-time caller.
func restoreLuaCaps(t *testing.T) {
	t.Helper()
	budget, deadline, frozen := directorLuaInstrBudget, directorLuaCallDeadlineMS, capsFrozen.Load()
	capsFrozen.Store(false)
	t.Cleanup(func() {
		directorLuaInstrBudget, directorLuaCallDeadlineMS = budget, deadline
		capsFrozen.Store(frozen)
	})
}
