package luasandbox

import (
	"context"
	"strings"
)

// AbortKind classifies how a Lua invocation ended, for the circuit breaker's weighting: a deterministic LOGIC
// error (a bug — always reproduces) is weighted heavily; a budget ABORT (a tight loop / instruction cap) is
// deterministic too but content-pathological; a transient DEADLINE abort (wall-clock — a GC pause, host load)
// is weighted LIGHTLY so latency does not quarantine a correct script.
type AbortKind int

// The abort kinds, ordered by the breaker weight they carry (OK < deadline-transient < budget < logic).
const (
	AbortOK       AbortKind = iota // no error
	AbortLogic                     // a deterministic Lua error (a content bug)
	AbortBudget                    // the instruction-count abort (deterministic, content-pathological)
	AbortAlloc                     // the string-allocation abort (deterministic, content-pathological)
	AbortDeadline                  // the wall-clock deadline (transient — weighted lightly)
)

// ClassifyError maps a pcall error to an abort kind. The fork raises "instruction budget exceeded" for the
// count abort and the context error (DeadlineExceeded) for the wall-clock abort; everything else is a logic
// error. This is the single source of truth for the classification shared by the zone and director sandboxes.
func ClassifyError(err error) AbortKind {
	if err == nil {
		return AbortOK
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "instruction budget exceeded"):
		return AbortBudget
	// Checked BEFORE the deadline case, and that ordering is the whole point of having a separate kind
	// (#438). A memory bomb allocates for the entire deadline and then trips it, so before this existed the
	// single most dangerous thing a script could do was classified AbortDeadline and weighted 0.1 — the
	// "probably transient host load, do not punish the script" bucket. It is neither transient nor the
	// host's fault: it reproduces exactly, every run, from the script's own operands.
	case strings.Contains(msg, "string allocation budget exceeded"):
		return AbortAlloc
	case strings.Contains(msg, context.DeadlineExceeded.Error()):
		return AbortDeadline
	default:
		return AbortLogic
	}
}
