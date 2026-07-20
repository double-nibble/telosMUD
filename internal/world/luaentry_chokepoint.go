package world

import (
	"fmt"

	lua "github.com/yuin/gopher-lua"

	"github.com/double-nibble/telosmud/internal/luasandbox"
)

// luaentry_chokepoint.go — THE SOLE Lua-invocation chokepoint (slice 7.5, §4 invariant) + the
// error-budget circuit breaker (P7-D10, T11) + the per-zone memory metric (T5).
//
// SECURITY INVARIANT (the §4 sole-chokepoint): both the instruction-count abort AND the wall-clock
// deadline live in the fork's mainLoopWithContext, which is only active while a context is set. A
// Lua-invoking path that forgets SetContext silently loses BOTH budgets (the 7.1 review finding).
// So EVERY path that runs Lua MUST go through pcallGuarded — the ONE function that does
// SetContext(fresh) → ResetInstructionCount → resetSpawnBudget → run → RemoveContext. No engine
// code calls a raw L.PCall/L.Call/L.DoString outside this function; a build-failing lint
// (luaharm_lint_test.go TestNoRawLuaCallsOutsideChokepoint) enforces it.

// luaAbortKind classifies how a Lua invocation ended, for the circuit breaker's weighting: a
// deterministic LOGIC error (a bug — always reproduces) is weighted heavily; a budget ABORT (a
// tight loop / instruction cap) is deterministic too but content-pathological; a transient
// DEADLINE abort (wall-clock — a GC pause, host load) is weighted LIGHTLY so latency does not
// quarantine a correct script and an attacker cannot drive a victim's breaker by inducing load.
type luaAbortKind int

const (
	luaOK       luaAbortKind = iota // no error
	luaLogicErr                     // a deterministic Lua error (a content bug)
	luaBudget                       // the instruction-count abort (deterministic, content-pathological)
	luaAlloc                        // the string-allocation abort (deterministic, content-pathological)
	luaDeadline                     // the wall-clock deadline (transient — weighted lightly)
)

// classifyLuaError maps a pcall error to an abort kind for the breaker's weighting. The fork-error-string
// matching is the SHARED luasandbox.ClassifyError (#47) so the zone and director agree on the classification;
// this adapts its result onto the zone's luaAbortKind enum.
func classifyLuaError(err error) luaAbortKind {
	switch luasandbox.ClassifyError(err) {
	case luasandbox.AbortOK:
		return luaOK
	case luasandbox.AbortBudget:
		return luaBudget
	case luasandbox.AbortAlloc:
		return luaAlloc
	case luasandbox.AbortDeadline:
		return luaDeadline
	default:
		return luaLogicErr
	}
}

// pcallGuarded is THE chokepoint. It arms a fresh per-call deadline + resets the instruction count
// + the spawn budget, runs fn (already pushed-with-args by the caller — the caller pushes fn then
// its nargs values, exactly like a raw L.PCall), and clears the context after. It returns the
// pcall error (nil on success). It also feeds the circuit breaker under `scriptKey` (empty ==
// not breaker-tracked, e.g. the standalone runChunk in tests): the breaker classifies the abort
// and may DISABLE a chronically-failing script. The raw error/stack is the caller's to log to ops
// (never to a player); pcallGuarded only records the breaker accounting.
//
// CALL CONTRACT: the caller must push fn and nargs arguments BEFORE calling, exactly as for a raw
// L.PCall(nargs, nret, nil). pcallGuarded does the PCall and the context lifecycle; the caller
// reads results off the stack after (for nret>0).
func (rt *luaRuntime) pcallGuarded(scriptKey, origin string, nargs, nret int) error {
	L := rt.L

	// NESTING (the single-writer re-entrant case): a Lua entry point can run a Go builtin (a harm op,
	// mud.fire) that fires an event whose handler is ITSELF Lua — a NESTED pcallGuarded on the same
	// LState, while the OUTER call is still mid-flight on the Go stack. The outer call is executing
	// mainLoopWithContext, which reads L.ctx every op (vm.go); if the nested call RemoveContext'd on
	// return, the outer loop would deref a nil ctx (a crash) the moment it ran its next bytecode op.
	// So a nested call must NOT tear down the parent's context — it REUSES it: the nested Lua runs
	// under the parent's wall-clock deadline and the parent's running instruction count (STRICTER,
	// never weaker — nested work counts against the same budget, so a script cannot re-nest to reset
	// its instruction tally and run unbounded). Only the TOP-LEVEL entry (no context set) arms a
	// fresh deadline + zeroes the count, and only it tears the context down after. The spawn budget
	// is likewise per-cascade: only the top-level entry resets it (a nested spawn counts against the
	// outer call's per-call cap, the same stricter discipline).
	nested := L.Context() != nil
	if !nested {
		ctx, cancel := contextWithLuaDeadline()
		defer cancel()
		L.SetContext(ctx)
		L.ResetInstructionCount()
		rt.resetSpawnBudget()
		defer L.RemoveContext()
	}
	// The per-FRAME builder-log line cap resets EVERY frame (nested included), UNLIKE the instruction/
	// spawn budgets which are per-cascade. A flood abort must be charged to the script that actually
	// flooded — a per-cascade shared counter would let an earlier sibling log up to the cap, then the
	// (cap+1)th call in a co-firing VICTIM's frame would abort and be recorded against the victim's
	// breaker key. Resetting per frame charges each script for its own logging. The cross-call/nested
	// VOLUME bound is logLimiter (wall-clock), which nesting cannot reset — so per-frame reset here does
	// not reopen the disk-fill vector (#456, review finding).
	prevLogs := rt.logsThisCall
	rt.logsThisCall = 0
	defer func() { rt.logsThisCall = prevLogs }()

	err := L.PCall(nargs, nret, nil)
	kind := classifyLuaError(err)
	if scriptKey != "" {
		rt.breakerRecord(scriptKey, origin, kind)
	}
	if err != nil {
		// Cap the rendered error at the source: a builder controls the Lua error message via
		// error(msg), and every isolated-callback path logs this err.Error() to the ops log. Without
		// this a script could error(string.rep(..., 8MB)) and stream an uncapped line into the log
		// store on every call — the same disk-fill/poisoning vector #456 bounds for the log sinks, via
		// the error channel. %w is dropped deliberately (nothing unwraps these; callers only log or
		// nil-check), so the capped string is what any downstream err.Error() renders (#456).
		return fmt.Errorf("lua run %s: %s", origin, luasandbox.CapLogMsg(err.Error()))
	}
	return nil
}

// runGuardedFn is the convenience wrapper the invoke* helpers use: push fn, run pcallGuarded,
// return the error. The caller has already SetFEnv'd fn and set rt.inv. nret results (if any)
// are left on the stack for the caller to read.
func (rt *luaRuntime) runGuardedFn(scriptKey, origin string, fn *lua.LFunction, nargs, nret int) error {
	rt.L.Push(fn)
	return rt.pcallGuarded(scriptKey, origin, nargs, nret)
}

// metricPulseStride is how often (in pulses) the per-zone Lua memory metric is emitted. The pulse
// is registered LAZILY on the first compiled chunk (a zone with no scripted content never pays for
// it — the bare-engine invariant), and emits a detection-only snapshot each stride. ~quarter-second
// pulses × 240 = ~once a minute, cheap.
const metricPulseStride = 240

// ensureMetricPulse registers the periodic memory-metric pulse on the zone's wheel the first time
// the zone compiles a Lua chunk (i.e. it actually runs scripts). Idempotent. A scriptless zone
// never registers it. Zone goroutine only.
func (rt *luaRuntime) ensureMetricPulse() {
	if rt == nil || rt.zone == nil || rt.zone.pulses == nil || rt.metricPulse != nil {
		return
	}
	rt.metricPulse = rt.zone.pulses.every(metricPulseStride, func(uint64) bool {
		rt.reportMemoryMetric()
		return true // keep ticking while the zone runs
	})
}

// reportMemoryMetric emits the DETECTION-ONLY per-zone Lua memory metric (T5): the VM's value-
// stack registry capacity (the primary growable allocation — fork RegistryCap) plus the counts of
// the engine-held tables that grow with content (compiled chunks, live entity scripts, tripped
// breakers, live timers, live Lua-spawns). It is DETECTION, not prevention — it fires AFTER an
// allocation; the single-op bombs are already PREVENTED by the 7.1 capped builtins. Emitted as a
// structured log (the established metrics seam until OTel lands — internal/obs). Returns the
// reported registry slot count so a caller/test can assert it. Zone goroutine only.
func (rt *luaRuntime) reportMemoryMetric() int {
	if rt == nil || rt.L == nil {
		return 0
	}
	regCap := rt.L.RegistryCap()
	tripped := 0
	for _, b := range rt.breakers {
		if b != nil && b.disabled {
			tripped++
		}
	}
	rt.log.Info("lua.vm.memory", // metric (detection-only, T5)
		"registry_slots", regCap, // the VM value-stack capacity (growable footprint proxy)
		"chunks", len(rt.chunks), // compiled-chunk cache size
		"entity_scripts", len(rt.entityScripts), // live per-instance trigger states
		"breakers_tripped", tripped, // quarantined scripts
		"timers_live", rt.luaTimersLive, // live mud.after wheel entries
		"spawns_live", rt.luaSpawnsLive, // live Lua-spawned population
		"builder_logs_dropped", rt.logLimiter.Dropped(), // #456: builder log lines dropped by the sustained-rate limit
	)
	return regCap
}
