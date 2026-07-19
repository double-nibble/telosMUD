// Package luasandbox is the shared, host-agnostic Lua sandbox core (#47). It is the SINGLE SOURCE OF TRUTH
// for the allowlist, the value caps, and the amplifier-guard mechanism that make an embedded gopher-lua VM
// safe to run UNTRUSTED-OR-BUGGY content — the same discipline the zone scripting engine (internal/world)
// holds itself to, lifted into a package the director tier (internal/director) can also consume so the two
// hosts cannot ship two divergent sandboxes.
//
// # What it provides
//
//   - New: a fresh *lua.LState with NO standard library opened (SkipOpenLibs), its globals REPLACED by the
//     curated allowlist only, the private capped-string metatable installed, and the per-call instruction
//     budget armed. A dangerous capability (load/require/os/io/debug/coroutine/get-setmetatable/raw*/
//     string.dump/collectgarbage) is ABSENT — never registered, so it cannot be reached via _G/_ENV aliasing.
//   - The amplifier caps (string.rep/format/gsub/find/match, table.concat) that stop a single-op alloc/
//     backtracking bomb the instruction count (one op) and the wall-clock deadline (no between-op check
//     inside a Go builtin) cannot catch.
//   - ReadOnly: the namespace read-only proxy used for string/table/math (and reusable by a host for its own
//     tables).
//   - The chokepoint (Runtime.Call) that arms the deadline + resets the instruction count around every VM
//     entry — the SetContext-or-no-budget invariant, without which neither budget is enforced.
//
// # What it deliberately does NOT do
//
// The zone-entangled machinery — the nested-reuse re-entrancy discipline, the harm/effect-op cascade, the
// per-entity self.state persistence, the spawn budget — stays in internal/world. This package targets a
// SINGLE-SCRIPT, NON-REENTRANT host (a director): one VM, one script, no host builtin that synchronously
// re-enters Lua. That lets Runtime.Call be a straight arm→pcall→disarm with no nesting branch.
//
// # Threat model
//
// Director scripts are TRUSTED builder content, but a runaway (an infinite loop, a giant allocation) would
// wedge the director tick — the same severity class as wedging a zone. So budget + deadline + the allowlist
// strip + the amplifier caps are load-bearing here regardless of trust: the realistic failure from a trusted
// author is a BUG, which these bound. The VM is not goroutine-safe; a host MUST touch it from one serialized
// goroutine (the director actor loop) — this package does not add its own locking.
package luasandbox

import (
	"fmt"
	"math/rand"

	lua "github.com/yuin/gopher-lua"
)

// The value caps and per-call budgets. These are the SINGLE SOURCE OF TRUTH: internal/world aliases them
// (const luaStrByteCap = luasandbox.StrByteCap, …) so the zone and director sandboxes cannot diverge, and a
// parity test cross-checks the two live sandboxes' allowlist + cap behavior.
const (
	// CallStackSize caps the Lua call-stack depth (recursion blowout). gopher-lua has no runtime
	// SetMaxStackSize; the cap is the build-time Options.CallStackSize. Deep/infinite recursion errors out
	// as a catchable Lua error, never overflowing the host goroutine stack (which would crash the process).
	CallStackSize = 200

	// RegistrySize / RegistryMaxSize bound the value stack (paired with the call stack). The registry starts
	// at RegistrySize and may grow to RegistryMaxSize; growth never shrinks back.
	RegistrySize    = 1024
	RegistryMaxSize = 1024 * 64

	// InstrBudget is the default per-call VM-instruction cap enforced by the fork's count abort. A runaway
	// loop aborts with "instruction budget exceeded" rather than stalling the host goroutine.
	InstrBudget = 100_000

	// CallDeadline (the per-call wall-clock deadline, armed via SetContext — catches a low-instruction stall
	// the instruction count cannot, e.g. a GC pause) is defined in the build-tagged deadline.go /
	// deadline_race.go: 5ms in a normal build, scaled up under `-race` (whose instrumentation makes every VM
	// op ~10x slower, so a legitimately budget-bounded template would otherwise trip the wall-clock guard in
	// CI while being sub-millisecond in production). The 100k instruction budget is the real per-call bound;
	// this deadline is the secondary stall guard, so scaling it under the race detector is safe.

	// StrByteCap bounds the OUTPUT size of the amplifier string builtins (the single-op alloc bomb:
	// string.rep("A", 2e9) allocates GB in ONE instruction). The capped wrappers reject an over-cap result
	// BEFORE allocating it. 1 MiB is generous for any legitimate script string.
	StrByteCap = 1 << 20

	// PatternInputCap bounds the INPUT length of the pattern builtins (find/match/gsub — pathological
	// backtracking). Lua patterns can backtrack super-linearly; capping input length bounds the worst case.
	PatternInputCap = 1 << 16
)

// BaseAllowlist is the exact set of base (global) functions the sandbox keeps — registered INDIVIDUALLY,
// never via OpenBase (which would bundle load/require/get-setmetatable/rawset/_G/... back in). Anything not
// listed is ABSENT. internal/world ranges over this same slice so the two hosts keep one allowlist.
var BaseAllowlist = []string{
	"assert", "error", "pcall", "xpcall", "select",
	"type", "tostring", "tonumber", "pairs", "ipairs", "unpack",
}

// stringPassthrough are the string functions with no amplification — copied verbatim from the genuine lib.
var stringPassthrough = []string{"byte", "char", "len", "lower", "upper", "sub", "reverse"}

// tablePassthrough are the table functions with no amplification (concat is capped separately).
var tablePassthrough = []string{"insert", "remove", "sort", "getn", "maxn"}

// Opts configures a sandbox build.
type Opts struct {
	// Rng backs math.random (rebound off the raw math lib so a script cannot reach the os-seeded global RNG).
	// nil => a fresh, time-independent zero-seeded RNG is created (deterministic; a host wanting reproducible
	// or per-scope streams passes its own).
	Rng *rand.Rand

	// InstrBudget overrides the per-call VM-instruction cap. 0 => the InstrBudget default.
	//
	// INJECTED, never read from config here. This package is deliberately host-agnostic — it is the single
	// source of truth for the sandbox and the parity boundary against the gopher-lua fork — so it must not
	// import a config package. The host reads the operator's setting and passes it down.
	//
	// The caller is responsible for validating the value; see ClampInstrBudget for the bounds and why they
	// exist. A zero here means "default", not "unlimited": a cap that can be turned off by omission would be
	// the wrong failure direction for the guard that stops a runaway loop from stalling the host goroutine.
	InstrBudget int

	// CallDeadlineMS overrides the per-call wall-clock deadline in milliseconds. 0 => the CallDeadline
	// default for this build (which is already scaled up under -race; see deadline_race.go).
	//
	// SECONDARY to the instruction budget, and an operator raising or lowering it should know that: the
	// budget is the real per-call bound, and this catches the stalls a count cannot — a GC pause, host load,
	// a slow builtin. Setting it too low spuriously aborts correct scripts.
	CallDeadlineMS int

	// Print is the value bound to the global `print`. nil => a no-op (a script's print is swallowed). A host
	// that wants print routed to its logger passes an LFunction built on the returned state — but since the
	// state does not exist before New, the common pattern is to override print via L.SetGlobal after New.
	Print *lua.LFunction
}

// New builds a fresh sandboxed LState: SkipOpenLibs + the recursion/value-stack caps, the globals replaced by
// the allowlist only, the private capped-string metatable installed, and the instruction budget armed. The
// host installs its own tables into the returned state's globals afterward. The state is NOT goroutine-safe;
// touch it from one serialized goroutine.
func New(opts Opts) *lua.LState {
	L := lua.NewState(lua.Options{
		CallStackSize:   CallStackSize,
		RegistrySize:    RegistrySize,
		RegistryMaxSize: RegistryMaxSize,
		SkipOpenLibs:    true,
	})
	rng := opts.Rng
	if rng == nil {
		rng = rand.New(rand.NewSource(1)) //nolint:gosec // determinism, not security
	}
	b := &builder{L: L, rng: rng}
	b.strip(opts.Print)
	L.SetInstructionBudget(ClampInstrBudget(opts.InstrBudget))
	return L
}

// Bounds on the operator-tunable Lua caps (#368).
//
// They exist because these are not free knobs. The instruction budget is the PRIMARY per-call bound — the
// thing that stops a runaway loop from stalling a zone's actor goroutine, which on this engine means
// stalling every player in that zone — so a value low enough to abort ordinary content or high enough to
// stop bounding anything are both ways of breaking the engine through configuration.
const (
	// MinInstrBudget is low enough for a trivial handler and high enough that nothing legitimate trips it.
	MinInstrBudget = 1_000
	// MaxInstrBudget bounds the stall an operator can configure. At 10M instructions a single call is already
	// ~100ms, which is many zone ticks — past here the budget has stopped being a bound in any useful sense.
	//
	// Reaching anywhere near it also requires raising the DEADLINE; see ValidateCaps, which is the check that
	// actually matters.
	MaxInstrBudget = 10_000_000

	// InstrPerMS is the engine's calibrated Lua instruction rate: how many VM instructions a call may be
	// assumed to execute per millisecond of wall clock.
	//
	// Deliberately CONSERVATIVE — measured throughput on ordinary hardware is several times this — because it
	// is used to reject unreachable budgets, and rejecting a configuration that would actually have worked is
	// a worse error than allowing a slightly generous one.
	InstrPerMS = 100_000
	// MinCallDeadlineMS: below a millisecond the secondary guard fires on scheduler noise rather than on
	// stalls, aborting correct scripts.
	MinCallDeadlineMS = 1
	// MaxCallDeadlineMS: a full second of wall clock is already far longer than any tick budget.
	MaxCallDeadlineMS = 1_000
)

// ClampInstrBudget returns the effective instruction budget for a requested value: the default for 0, and
// the requested value clamped into [MinInstrBudget, MaxInstrBudget] otherwise.
//
// Clamping rather than erroring, because this is the LAST line and it must always produce a usable bound.
// The host validates and refuses a bad value loudly at boot (config.Validate); this exists so that a value
// arriving here by some path that skipped validation still yields a sandbox that is bounded.
func ClampInstrBudget(n int) int {
	switch {
	case n == 0:
		return InstrBudget
	case n < MinInstrBudget:
		return MinInstrBudget
	case n > MaxInstrBudget:
		return MaxInstrBudget
	}
	return n
}

// ValidateCaps checks the two Lua caps against their individual bounds AND against each other (#368).
//
// # The cross-field invariant, and why it is the important half
//
// The instruction budget is the PRIMARY bound and the wall-clock deadline is the SECONDARY stall guard —
// but only while the budget is actually reachable within the deadline. Raise the budget past what the
// deadline permits and the budget stops firing entirely: every runaway now aborts on the wall clock instead.
//
// That is not merely a knob that quietly does nothing. It silently re-weights the circuit breaker. A
// tight-loop instruction abort is weighted 0.5 (pathological but deterministic — quarantine this script);
// a wall-clock abort is weighted 0.1 (probably transient host load — do not punish a script for the host
// being busy, or an attacker could trip a victim's breaker by inducing load). So an operator who raises the
// budget "to give content more room" converts every genuinely-runaway script from something the breaker
// quarantines after ~20 failures into something it needs ~100 for, and a script failing 4 times in 5 goes
// from tripping the breaker to never tripping it at all.
//
// An operator who genuinely wants a bigger budget must therefore also grant the wall clock to spend it in,
// which makes the real cost — a zone actor stalled for that long, and every player in that zone with it —
// explicit rather than hidden.
func ValidateCaps(instrBudget, callDeadlineMS int) error {
	if n := instrBudget; n != 0 && (n < MinInstrBudget || n > MaxInstrBudget) {
		return fmt.Errorf("lua instruction budget %d is outside [%d, %d]: it is the primary bound on a content "+
			"script, and a value below the floor aborts ordinary content while one above the ceiling stops "+
			"bounding the zone-actor stall it exists to prevent", n, MinInstrBudget, MaxInstrBudget)
	}
	if ms := callDeadlineMS; ms != 0 && (ms < MinCallDeadlineMS || ms > MaxCallDeadlineMS) {
		return fmt.Errorf("lua call deadline %dms is outside [%d, %d]: below the floor the secondary stall "+
			"guard fires on scheduler noise and aborts correct scripts; above the ceiling it is longer than any "+
			"tick budget and guards nothing", ms, MinCallDeadlineMS, MaxCallDeadlineMS)
	}
	budget, deadline := ClampInstrBudget(instrBudget), ClampCallDeadlineMS(callDeadlineMS)
	if reachable := deadline * InstrPerMS; budget > reachable {
		return fmt.Errorf("lua instruction budget %d cannot be reached within a %dms call deadline (at most "+
			"~%d instructions run in that time): the wall-clock guard would fire first, so the budget would "+
			"never bound anything AND every runaway script would be classified as a transient deadline abort, "+
			"which the circuit breaker weights 5x more lightly and would stop quarantining. Raise "+
			"lua_call_deadline_ms to at least %d, or lower the budget",
			budget, deadline, reachable, (budget+InstrPerMS-1)/InstrPerMS)
	}
	return nil
}

// ClampCallDeadlineMS is ClampInstrBudget for the wall-clock deadline. See it for why this clamps.
func ClampCallDeadlineMS(ms int) int {
	switch {
	case ms == 0:
		return CallDeadline
	case ms < MinCallDeadlineMS:
		return MinCallDeadlineMS
	case ms > MaxCallDeadlineMS:
		return MaxCallDeadlineMS
	}
	return ms
}

// builder holds the per-build state the strip + capped wrappers close over (mirrors internal/world's
// luaRuntime for the sandbox-construction subset).
type builder struct {
	L   *lua.LState
	rng *rand.Rand
}

// strip replaces the VM's globals with ONLY the allowlist and installs the private capped-string metatable.
// After it returns, a fresh script's environment has no path to any dropped capability.
//
// Construction order matters: harvest the real stdlib closures onto scratch globals first (so pairs/ipairs
// keep their auxiliary upvalues and the capped wrappers can delegate to the real format/find/gsub/concat),
// build the sandbox global table from the allowlist, swap it in, and override the string metatable.
func (b *builder) strip(printFn *lua.LFunction) {
	L := b.L

	// Step 1: harvest the genuine closures onto THIS state's globals (overwritten wholesale in step 3).
	lua.OpenBase(L)
	lua.OpenString(L)
	lua.OpenTable(L)
	lua.OpenMath(L)

	raw := L.Get(lua.GlobalsIndex).(*lua.LTable)
	rawString := raw.RawGetString("string").(*lua.LTable)
	rawTable := raw.RawGetString("table").(*lua.LTable)
	rawMath := raw.RawGetString("math").(*lua.LTable)

	// Step 2: the PRIVATE capped-string metatable — what method syntax ("x"):rep() dispatches through
	// (installed as builtinMts[LTString]). A script cannot reach or replace it. A separate read-only proxy
	// is the script-visible `string` namespace.
	stringMeta := b.buildCappedStringTable(rawString)
	stringMeta.RawSetString("__index", stringMeta)
	L.SetMetatable(lua.LString(""), stringMeta)

	// Step 3: build the sandbox globals from the allowlist — an unsafe capability is ABSENT, not hidden.
	env := L.NewTable()
	for _, name := range BaseAllowlist {
		if fn := raw.RawGetString(name); fn != lua.LNil {
			env.RawSetString(name, fn)
		}
	}
	if printFn != nil {
		env.RawSetString("print", printFn)
	} else {
		env.RawSetString("print", L.NewFunction(func(*lua.LState) int { return 0 }))
	}
	env.RawSetString("string", b.readOnly(b.buildCappedStringTable(rawString)))
	env.RawSetString("table", b.buildCappedTableTable(rawTable))
	env.RawSetString("math", b.buildMathTable(rawMath))

	b.setGlobals(env)

	// Arm the default per-call budget so the abort path is live; Runtime.Call re-arms per call.
	L.ResetInstructionCount()
}

// setGlobals repoints the VM's globals at env by CLEARING the existing globals table key-by-key and
// repopulating it from the allowlist — NOT swapping the pointer. The registry's _G slot holds the original
// table, so a naive pointer swap would leave the raw stdlib reachable via _G; clearing-and-repopulating is
// the pointer-stable way to guarantee nothing from the raw open survives.
func (b *builder) setGlobals(env *lua.LTable) {
	L := b.L
	g := L.Get(lua.GlobalsIndex).(*lua.LTable)
	var keys []lua.LValue
	g.ForEach(func(k, _ lua.LValue) { keys = append(keys, k) })
	for _, k := range keys {
		L.SetTable(g, k, lua.LNil)
	}
	env.ForEach(func(k, v lua.LValue) { g.RawSet(k, v) })
}

// ReadOnly wraps tbl in a new table whose __index proxies reads to tbl and whose __newindex rejects all
// writes, so a script cannot mutate a shared namespace. NOTE this guards the NAMESPACE table (a script
// reaching string.rep); it is NOT what protects method syntax — that is the private metatable installed by
// strip. Exported so a host can lock its own tables the same way.
func ReadOnly(L *lua.LState, tbl *lua.LTable) *lua.LTable {
	return (&builder{L: L}).readOnly(tbl)
}

func (b *builder) readOnly(tbl *lua.LTable) *lua.LTable {
	L := b.L
	proxy := L.NewTable()
	mt := L.NewTable()
	mt.RawSetString("__index", tbl)
	mt.RawSetString("__newindex", L.NewFunction(func(l *lua.LState) int {
		l.RaiseError("attempt to modify a read-only table")
		return 0
	}))
	mt.RawSetString("__metatable", lua.LString("locked"))
	L.SetMetatable(proxy, mt)
	return proxy
}

// GlobalNames returns the allowlisted top-level global names (base fns + print + string/table/math). Used by
// the parity test to assert the zone and director sandboxes expose the identical global surface.
func GlobalNames() []string {
	names := append([]string(nil), BaseAllowlist...)
	return append(names, "print", "string", "table", "math")
}

// StringNames / TableNames return the members the capped string/table namespaces expose, for the parity test.
func StringNames() []string {
	names := append([]string(nil), stringPassthrough...)
	return append(names, "rep", "format", "gsub", "find", "match", "gmatch")
}

// TableNames returns the members the capped table namespace exposes, for the parity test. (`unpack` is a
// Lua-5.1 GLOBAL, not a table member — the fork's table lib has no table.unpack — so it is not listed here;
// the buildCappedTableTable copy is conditional and a no-op on this fork.)
func TableNames() []string {
	names := append([]string(nil), tablePassthrough...)
	return append(names, "concat")
}
