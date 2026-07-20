package world

import "fmt"

// luabreaker.go — the error-budget circuit breaker (slice 7.5, P7-D10, T11). A per-SCRIPT failure
// counter: each failure adds to a weighted budget; past a threshold the breaker TRIPS and DISABLES
// that script (its invocations no-op) — never the zone. It resets on a successful hot reload (7.7).
//
// Two hardening calls from the P7-D10 review:
//
//   (a) SCOPE. The breaker is keyed by a SCRIPT KEY chosen by the caller:
//         - per-INSTANCE for entity-scoped scripts (triggers / self.state-bearing) — key
//           "trigger:#<rid>" — so one buggy mob instance is quarantined, not every mob of that
//           prototype.
//         - per-(kind,ref) for genuinely SHARED defs (an ability/affect/formula/policy used by
//           many entities) — key "ability:<ref>:on_resolve" etc.
//       SHARED-DEF BLAST RADIUS (documented): a chronically-failing SHARED def trips content-wide
//       for that def — every entity using it loses it until the next reload. That is the correct
//       trade (a broken shared ability should stop firing everywhere) but it means a hostile shared
//       def is a content-wide DoS of ITSELF (not of the zone) — acceptable, and the reason Lua is
//       gated to reviewed authors (P7-D9). A per-instance breaker for a shared def would let a
//       broken ability keep erroring forever on every other caller, which is worse.
//
//   (b) WEIGHTING. A transient wall-clock DEADLINE abort is weighted FAR LIGHTER than a
//       deterministic LOGIC error or instruction-BUDGET abort. So:
//         - a GC pause / host-load spike that trips the deadline on a CORRECT script does not
//           quarantine it (it would take many deadline aborts to trip);
//         - an attacker cannot drive a victim script's breaker by inducing latency on the host
//           (each induced deadline costs them ~10x more aborts than a real bug would);
//         - a real bug (a logic error, or a tight-loop budget abort) trips quickly.
//       The budget also DECAYS toward 0 over successful calls, so sporadic failures separated by
//       healthy runs never accumulate to a trip — only a SUSTAINED failure rate does.

const (
	// breakerTripThreshold is the weighted error budget at which a script is disabled. With the
	// weights below, ~10 consecutive logic errors, or ~20 budget aborts, or ~100 deadline aborts
	// trip it — a clear "this script is broken" signal, not a hair trigger.
	breakerTripThreshold = 10.0

	// The per-kind weights (P7-D10 (b)). A logic error is the canonical "broken" signal (weight
	// 1.0). A budget abort (a tight loop) is content-pathological but deterministic — weighted a
	// bit lighter. A deadline abort is TRANSIENT (host load) — weighted ~10x lighter so latency
	// alone cannot quarantine a correct script.
	breakerWeightLogic    = 1.0
	breakerWeightBudget   = 0.5
	breakerWeightDeadline = 0.1

	// breakerDecayOnSuccess is subtracted from the budget on each SUCCESSFUL call, so isolated
	// failures separated by healthy runs never accumulate to a trip — only a sustained failure
	// rate does. Clamped at 0.
	breakerDecayOnSuccess = 1.0
)

// breakerState is one script's failure accounting.
type breakerState struct {
	budget   float64 // weighted error budget; >= breakerTripThreshold => disabled
	disabled bool    // tripped: invocations of this script no-op until a reset
	failures int     // total failures recorded (diagnostic)
}

// breakerKeyInstance is the per-INSTANCE breaker key for an entity-scoped script (a trigger / a
// self.state-bearing script). One buggy instance is quarantined, not its whole prototype.
func breakerKeyInstance(rid RuntimeID) string {
	return "instance:#" + ridStr(rid)
}

// breakerKeyShared is the per-(kind,ref) breaker key for a genuinely shared def (an ability/affect/
// formula/policy used by many entities). It is just the chunk origin, which already encodes
// kind+ref (e.g. "ability:fireball:on_resolve", "pvp_allowed", "formula:regen").
func breakerKeyShared(origin string) string { return "shared:" + origin }

// breakerKeyFor resolves the circuit-breaker key for an invocation: the invocation's explicit
// per-instance override (set by an entity-scoped trigger), else the per-(kind,ref) SHARED key
// derived from the chunk origin. This is the ONE place the per-instance-vs-shared scope decision
// is made (P7-D10 (a)).
func (rt *luaRuntime) breakerKeyFor(inv *luaInvocation, origin string) string {
	if inv != nil && inv.breakerKey != "" {
		return inv.breakerKey
	}
	return breakerKeyShared(origin)
}

// breakerDisabled reports whether the script under `key` is currently quarantined. A disabled
// script's invocations are skipped (a clean no-op / fail-closed default at the call site). nil-safe.
func (rt *luaRuntime) breakerDisabled(key string) bool {
	if rt == nil || rt.breakers == nil || key == "" {
		return false
	}
	b := rt.breakers[key]
	return b != nil && b.disabled
}

// breakerRecord folds one invocation outcome into the script's budget. A success DECAYS the
// budget; a failure ADDS its kind-weighted cost and trips the breaker (disables + alerts ops) the
// first time the budget crosses the threshold. Called by pcallGuarded with the classified kind.
func (rt *luaRuntime) breakerRecord(key, origin string, kind luaAbortKind) {
	if rt == nil || rt.breakers == nil || key == "" {
		return
	}
	b := rt.breakers[key]
	if b == nil {
		b = &breakerState{}
		rt.breakers[key] = b
	}
	if b.disabled {
		return // already quarantined; nothing more to account
	}
	switch kind {
	case luaOK:
		b.budget -= breakerDecayOnSuccess
		if b.budget < 0 {
			b.budget = 0
		}
		return
	case luaLogicErr:
		b.budget += breakerWeightLogic
	case luaBudget, luaAlloc:
		// The allocation abort shares the instruction abort's weight, and it is grouped here rather than
		// given a case of its own to make that deliberate: both are deterministic and content-pathological —
		// they reproduce every run from the script's own operands. It must NOT get the deadline's 0.1: a
		// memory bomb allocates for the whole deadline and then trips it, so before #438 the most dangerous
		// thing a script could do was weighted as "probably the host was busy".
		b.budget += breakerWeightBudget
	case luaDeadline:
		b.budget += breakerWeightDeadline
	}
	b.failures++
	if b.budget >= breakerTripThreshold {
		b.disabled = true
		// Ops ALERT (not a player-facing message): a script has been quarantined. The raw error/
		// stack was already logged by the caller at WARN; this is the trip event.
		rt.log.Error("lua circuit breaker TRIPPED — script DISABLED until reload (zone unaffected)",
			"script", key, "origin", origin, "failures", b.failures, "budget", b.budget)
		// #116: a quarantine is the highest-value debug signal — a builder testing content needs to know their
		// script is now disabled, not silently inert. Echo it to any staff watching this zone with `debug on`.
		if rt.zone != nil {
			rt.zone.echoDebug(fmt.Sprintf("lua script QUARANTINED [%s] after %d failures — disabled until reload", origin, b.failures))
		}
	}
}

// breakerReset re-enables a script's breaker (clears the budget + the disabled latch) — called on
// a SUCCESSFUL hot reload of that script (7.7). A nil/absent key is a no-op. (A whole-zone reset on
// a full content reload is a 7.7 concern; the per-key reset is the unit 7.7 needs.)
func (rt *luaRuntime) breakerReset(key string) {
	if rt == nil || rt.breakers == nil {
		return
	}
	delete(rt.breakers, key)
}

// ridStr renders a RuntimeID without importing strconv at every call site.
func ridStr(rid RuntimeID) string {
	if rid == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for rid > 0 {
		i--
		b[i] = byte('0' + rid%10)
		rid /= 10
	}
	return string(b[i:])
}
