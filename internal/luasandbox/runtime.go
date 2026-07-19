package luasandbox

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// runtime.go — a single-VM, single-script Runtime for a NON-REENTRANT host (a director). It owns the
// sandboxed LState (from New), a compiled-chunk cache, and the circuit breaker, and exposes the SOLE
// invocation chokepoint (Call): arm a fresh deadline → reset the instruction count → PCall → clear the
// context. That SetContext-or-no-budget sequence is load-bearing — both the instruction abort and the
// wall-clock deadline live in the fork's mainLoopWithContext, active ONLY while a context is set, so a path
// that forgets SetContext silently loses BOTH budgets. A build-failing lint forbids raw L.PCall/Call/DoString
// outside this file.
//
// NOT re-entrant: there is no nested-context-reuse branch (unlike the zone runtime). A host builtin bound
// into this VM must NOT synchronously re-enter Lua on the same state — the director's get/set/broadcast/log
// surface does not, so the simple arm→run→disarm holds. The VM is not goroutine-safe; the host drives it from
// one serialized goroutine.

// Runtime wraps a sandboxed LState with the compiled-chunk cache and the breaker.
type Runtime struct {
	L       *lua.LState
	log     *slog.Logger
	chunks  map[string]*lua.FunctionProto
	breaker *breaker
	// callDeadline is this runtime's per-call wall-clock bound, resolved at construction from Opts (#368).
	// Per-runtime rather than package-global so a host can tune it without changing it for every other
	// sandbox in the process.
	callDeadline time.Duration
}

// NewRuntime builds a Runtime over a fresh sandboxed LState. printTarget, if non-empty, routes the script's
// `print` to the logger at info under this key; otherwise print is swallowed. rng backs math.random (nil =>
// a fresh deterministic RNG).
func NewRuntime(log *slog.Logger, opts Opts) *Runtime {
	if log == nil {
		log = slog.Default()
	}
	rt := &Runtime{
		log:     log.With("subsystem", "luasandbox"),
		chunks:  map[string]*lua.FunctionProto{},
		breaker: newBreaker(),
		// Resolved once here rather than read per call: the deadline is a property of how this runtime was
		// configured, and re-deriving it on the hot path would invite a future reader to make it mutable.
		callDeadline: time.Duration(ClampCallDeadlineMS(opts.CallDeadlineMS)) * time.Millisecond,
	}
	rt.L = New(opts)
	// Route the script's `print` to the runtime logger. New installed a no-op print (the LFunction can't be
	// built before the state exists); override it now with a structured-log redirect. The globals table
	// itself is not read-only (only the string/table/math proxies are), so SetGlobal is permitted.
	rt.L.SetGlobal("print", rt.L.NewFunction(func(l *lua.LState) int {
		n := l.GetTop()
		parts := make([]string, 0, n)
		for i := 1; i <= n; i++ {
			parts = append(parts, l.ToStringMeta(l.Get(i)).String())
		}
		rt.log.Info("lua print", "msg", strings.Join(parts, " "))
		return 0
	}))
	return rt
}

// Close tears down the VM.
func (rt *Runtime) Close() {
	if rt == nil || rt.L == nil {
		return
	}
	rt.L.Close()
	rt.L = nil
}

// Compile compiles src under name into the chunk cache, replacing any prior chunk for name (source hot
// reload) and resetting that chunk's breaker. It returns a compile error (a syntax error in content). NOTE
// the compile itself runs OUTSIDE the instruction budget/deadline (the fork enforces those only during
// execution), so a host compiles ONCE at load and caches — never per event/tick — and trusts content not to
// ship a pathological parser input.
func (rt *Runtime) Compile(name, src string) error {
	fn, err := compile(name, src)
	if err != nil {
		return err
	}
	rt.chunks[name] = fn
	rt.breaker.reset(name)
	return nil
}

// compile parses src into a reusable FunctionProto without running it.
func compile(name, src string) (*lua.FunctionProto, error) {
	chunk, err := parseChunk(name, src)
	if err != nil {
		return nil, err
	}
	return chunk, nil
}

// parseChunk lexes+parses+compiles a source string to a FunctionProto (no execution). A throwaway state is
// used to Load (the proto's constants are state-independent, so it re-binds cleanly onto the sandbox state
// via NewFunctionFromProto); it is closed before returning since only the proto is kept.
func parseChunk(name, src string) (*lua.FunctionProto, error) {
	tmp := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer tmp.Close()
	fn, err := tmp.Load(strings.NewReader(src), name)
	if err != nil {
		return nil, fmt.Errorf("lua compile %s: %w", name, err)
	}
	return fn.Proto, nil
}

// Has reports whether a chunk is compiled under name.
func (rt *Runtime) Has(name string) bool { _, ok := rt.chunks[name]; return ok }

// LoadGlobals runs a compiled chunk's top level under the chokepoint so its function/global definitions
// (e.g. `function on_signal(...) ... end`) land in the sandbox globals. Call once after Compile, before
// invoking a named callback. A disabled breaker (chronically failing chunk) short-circuits to nil.
func (rt *Runtime) LoadGlobals(name string) error {
	fn, ok := rt.chunks[name]
	if !ok {
		return fmt.Errorf("luasandbox: no compiled chunk %q", name)
	}
	if rt.breaker.disabled(name) {
		return nil
	}
	lfn := rt.L.NewFunctionFromProto(fn)
	rt.L.Push(lfn)
	return rt.call(name, name+":load", 0, 0)
}

// CallGlobal invokes a global Lua function `fnName` (e.g. "on_signal") with args pushed by pushArgs, under the
// chokepoint. breakerKey attributes the call to a script for the circuit breaker. A missing function is a
// no-op (found=false, nil error): a script that defines no on_signal simply does not react. A disabled
// breaker short-circuits to a no-op. Results (if any) are left on the stack for the caller when nret>0.
func (rt *Runtime) CallGlobal(breakerKey, fnName string, nret int, pushArgs func(L *lua.LState) int) (found bool, err error) {
	if rt.breaker.disabled(breakerKey) {
		return false, nil
	}
	L := rt.L
	fn := L.GetGlobal(fnName)
	lfn, ok := fn.(*lua.LFunction)
	if !ok {
		return false, nil // no such callback — a script may define only some hooks
	}
	L.Push(lfn)
	nargs := 0
	if pushArgs != nil {
		nargs = pushArgs(L)
	}
	if err := rt.call(breakerKey, fnName, nargs, nret); err != nil {
		return true, err
	}
	return true, nil
}

// call is THE chokepoint: arm a fresh deadline + reset the instruction count, PCall (fn + nargs already
// pushed by the caller), clear the context, and feed the breaker. Non-reentrant: it always arms/disarms (a
// host builtin must not synchronously re-enter Lua here).
func (rt *Runtime) call(breakerKey, origin string, nargs, nret int) error {
	L := rt.L
	ctx, cancel := context.WithTimeout(context.Background(), rt.callDeadline)
	defer cancel()
	L.SetContext(ctx)
	L.ResetInstructionCount()
	// Deferred disarm (matches the zone chokepoint): if a Go panic escapes PCall, the context is still
	// cleared so a since-cancelled context is never left armed on the shared LState.
	defer L.RemoveContext()
	err := L.PCall(nargs, nret, nil)
	kind := ClassifyError(err)
	if breakerKey != "" {
		rt.breaker.record(rt.log, breakerKey, origin, kind)
	}
	if err != nil {
		return fmt.Errorf("lua run %s: %w", origin, err)
	}
	return nil
}
