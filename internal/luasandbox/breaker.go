package luasandbox

import "log/slog"

// breaker.go — the error-budget circuit breaker, the host-agnostic port of the zone breaker (internal/world/
// luabreaker.go). A per-SCRIPT weighted failure counter: each failure adds its kind-weighted cost; past a
// threshold the breaker TRIPS and DISABLES that script (its invocations no-op) — never the host loop. It
// resets on a successful (re)compile. The weighting makes a transient wall-clock DEADLINE abort cost ~10x
// less than a deterministic LOGIC error or instruction-BUDGET abort, so host latency alone cannot quarantine
// a correct script, and the budget DECAYS on success so only a SUSTAINED failure rate trips it.
const (
	breakerTripThreshold  = 10.0 // weighted budget at which a script is disabled
	breakerWeightLogic    = 1.0  // a deterministic content bug — the canonical "broken" signal
	breakerWeightBudget   = 0.5  // a tight-loop instruction abort — pathological but deterministic
	breakerWeightDeadline = 0.1  // a transient wall-clock abort (host load) — weighted lightly
	breakerDecayOnSuccess = 1.0  // subtracted on each successful call; clamped at 0
)

// breakerState is one script's failure accounting.
type breakerState struct {
	budget   float64
	disabled bool
	failures int
}

// breaker holds the per-key accounting for one runtime's scripts.
type breaker struct {
	states map[string]*breakerState
}

func newBreaker() *breaker { return &breaker{states: map[string]*breakerState{}} }

// disabled reports whether the script under key is currently quarantined (its invocations skipped).
func (br *breaker) disabled(key string) bool {
	if br == nil || key == "" {
		return false
	}
	s := br.states[key]
	return s != nil && s.disabled
}

// record folds one invocation outcome into the script's budget: a success DECAYS it; a failure ADDS its
// kind-weighted cost and trips the breaker (disables + alerts ops) the first time the budget crosses the
// threshold.
func (br *breaker) record(log *slog.Logger, key, origin string, kind AbortKind) {
	if br == nil || key == "" {
		return
	}
	s := br.states[key]
	if s == nil {
		s = &breakerState{}
		br.states[key] = s
	}
	if s.disabled {
		return
	}
	switch kind {
	case AbortOK:
		s.budget -= breakerDecayOnSuccess
		if s.budget < 0 {
			s.budget = 0
		}
		return
	case AbortLogic:
		s.budget += breakerWeightLogic
	case AbortBudget:
		s.budget += breakerWeightBudget
	case AbortDeadline:
		s.budget += breakerWeightDeadline
	}
	s.failures++
	if s.budget >= breakerTripThreshold {
		s.disabled = true
		if log != nil {
			log.Error("lua circuit breaker TRIPPED — script DISABLED until reload (host unaffected)",
				"script", key, "origin", origin, "failures", s.failures, "budget", s.budget)
		}
	}
}

// reset re-enables a script's breaker (clears the budget + the disabled latch) — called on a successful
// (re)compile of that script.
func (br *breaker) reset(key string) {
	if br == nil {
		return
	}
	delete(br.states, key)
}
