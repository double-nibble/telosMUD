package luasandbox

import (
	"strings"
	"testing"
)

// tunables_test.go — #368: the sandbox's clamp, which is the LAST line rather than the interface.
//
// config.TunablesConfig.Validate is what an operator meets: it refuses a bad value at boot and names the
// constraint. This clamp exists for any value that reaches the sandbox by a path that skipped that — a test,
// a future embedder, a caller constructing Opts directly. It must always produce a usable bound, because a
// sandbox with no instruction cap is a runaway loop away from stalling a zone's actor and everyone in it.
func TestClampsAlwaysProduceAUsableBound(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{"zero means the engine default, never unlimited", 0, InstrBudget},
		{"a negative value is raised to the floor", -5, MinInstrBudget},
		{"below the floor", MinInstrBudget - 1, MinInstrBudget},
		{"above the ceiling", MaxInstrBudget + 1, MaxInstrBudget},
		{"an in-range value passes through", 250_000, 250_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampInstrBudget(tc.in); got != tc.want {
				t.Fatalf("ClampInstrBudget(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{"zero means the build's default (already race-scaled)", 0, CallDeadline},
		{"a negative value is raised to the floor", -1, MinCallDeadlineMS},
		{"above the ceiling", MaxCallDeadlineMS + 1, MaxCallDeadlineMS},
		{"an in-range value passes through", 25, 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClampCallDeadlineMS(tc.in); got != tc.want {
				t.Fatalf("ClampCallDeadlineMS(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestOptsInstrBudgetIsActuallyArmed. The plumbing is only worth anything if the value reaches the VM: a
// tunable that parses, validates and is then dropped is the exact silent-misconfiguration this closes.
//
// Asserted behaviorally — a tiny budget must abort a loop that the default would run comfortably.
func TestOptsInstrBudgetIsActuallyArmed(t *testing.T) {
	rt := NewRuntime(nil, Opts{InstrBudget: MinInstrBudget})
	defer rt.Close()
	if err := rt.Compile("spin", "for i = 1, 1000000 do end"); err != nil {
		t.Fatalf("compile: %v", err)
	}
	err := rt.LoadGlobals("spin")
	if err == nil {
		t.Fatal("a loop of a million iterations ran to completion under a 1000-instruction budget; the Opts " +
			"budget never reached the VM")
	}
	if got := ClassifyError(err); got != AbortBudget {
		t.Fatalf("abort classified as %v, want AbortBudget — the budget is what should have stopped it", got)
	}
}

// TestValidateCapsRejectsAnUnreachableBudget is the cross-field invariant, and it is the finding this whole
// slice turned on.
//
// Measured against the real fork: with the default 5ms deadline the instruction budget stops firing somewhere
// around 850k instructions — past that, EVERY runaway aborts on the wall clock instead. So an operator who
// sets a 10M budget has not raised the primary bound, they have disabled it.
//
// And that is worse than a knob that does nothing, because of what it does to the breaker. A tight-loop
// instruction abort is weighted 0.5 (pathological, deterministic, quarantine it); a wall-clock abort is
// weighted 0.1 (probably transient host load — deliberately light, so an attacker cannot trip a victim's
// breaker by inducing load). Reclassify every runaway as the latter and a script that failed 4 times in 5
// goes from tripping the breaker to never tripping it.
func TestValidateCapsRejectsAnUnreachableBudget(t *testing.T) {
	for _, tc := range []struct {
		name            string
		budget          int
		deadlineMS      int
		wantErrFragment string
	}{
		{"the shipped defaults", 0, 0, ""},
		{"a modest raise the default deadline can still execute", 250_000, 0, ""},
		{"a big raise WITH the wall clock to spend it in", 1_000_000, 20, ""},
		{"the ceiling, with a deadline that can actually reach it", MaxInstrBudget, 100, ""},
		// Derived from the constants rather than hard-coded: the DEFAULT deadline differs by build (5ms
		// normally, 50ms under -race, since the detector makes every VM op ~10x slower). A literal here would
		// be "unreachable" in one build and comfortably reachable in the other.
		{"the largest budget the default deadline can still reach", CallDeadline * InstrPerMS, 0, ""},

		{"one instruction past what the default deadline can reach", CallDeadline*InstrPerMS + 1, 0, "cannot be reached"},
		{"the ceiling with the default deadline — the trap", MaxInstrBudget, 0, "cannot be reached"},
		{"raising the budget while LOWERING the deadline", InstrPerMS + 1, 1, "cannot be reached"},

		{"a budget below the floor", MinInstrBudget - 1, 0, "instruction budget"},
		{"a deadline above the ceiling", 0, MaxCallDeadlineMS + 1, "call deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCaps(tc.budget, tc.deadlineMS)
			if tc.wantErrFragment == "" {
				if err != nil {
					t.Fatalf("ValidateCaps(%d, %d) = %v, want nil", tc.budget, tc.deadlineMS, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateCaps(%d, %d) accepted a configuration the engine cannot honor",
					tc.budget, tc.deadlineMS)
			}
			if !strings.Contains(err.Error(), tc.wantErrFragment) {
				t.Fatalf("error %q does not mention %q — an operator has to be told WHICH constraint they hit",
					err.Error(), tc.wantErrFragment)
			}
		})
	}
}

// TestTheDeadlineOverrideIsActuallyArmed. The budget half was covered; the deadline half was not, and a
// version that parsed the deadline and then ignored it would have passed everything.
//
// Asserted by which guard fires: a generous budget with a 1ms deadline must abort on the WALL CLOCK, and the
// classification is the observable — it is also what the breaker weights on.
func TestTheDeadlineOverrideIsActuallyArmed(t *testing.T) {
	rt := NewRuntime(nil, Opts{InstrBudget: MaxInstrBudget, CallDeadlineMS: MinCallDeadlineMS})
	defer rt.Close()
	if err := rt.Compile("spin", "for i = 1, 100000000 do end"); err != nil {
		t.Fatalf("compile: %v", err)
	}
	err := rt.LoadGlobals("spin")
	if err == nil {
		t.Fatal("a 100M-iteration loop completed; neither guard fired")
	}
	if got := ClassifyError(err); got != AbortDeadline {
		t.Fatalf("abort classified as %v, want AbortDeadline — with a 10M budget and a 1ms deadline the WALL "+
			"CLOCK must be what stops it, which is only true if the configured deadline reached the runtime", got)
	}
}
