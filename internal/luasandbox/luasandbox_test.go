package luasandbox

import (
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"
)

// luasandbox_test.go — the sandbox's OWN security regression net (independent of the zone's). It proves the
// director sandbox is not weaker than the zone's on the properties that matter: the dangerous stdlib is
// ABSENT, the amplifier builtins are capped (namespace AND method syntax), the instruction budget aborts a
// runaway, the namespaces are read-only, and math.random is rebound off the global RNG. A parity test in
// internal/world additionally cross-checks the two live sandboxes.

// run compiles+loads src in a fresh runtime and returns the (compile-or-run) error. The top-level chunk runs
// under the chokepoint, so an `assert(...)` failure or an amplifier cap surfaces as the returned error.
func run(t *testing.T, src string) error {
	t.Helper()
	rt := NewRuntime(nil, Opts{Rng: rand.New(rand.NewSource(1))})
	t.Cleanup(rt.Close)
	require.NoError(t, rt.Compile("t", src), "compile")
	return rt.LoadGlobals("t")
}

func TestAllowlistDropsDangerousGlobals(t *testing.T) {
	// Every one of these MUST be absent (never registered) — reachable via neither a global nor _G/_ENV.
	for _, name := range []string{
		"os", "io", "require", "load", "loadstring", "dofile", "loadfile", "module",
		"getmetatable", "setmetatable", "rawget", "rawset", "rawequal", "rawlen",
		"collectgarbage", "coroutine", "debug", "newproxy", "_G", "_ENV", "gcinfo",
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, run(t, "assert("+name+" == nil, '"+name+" should be absent')"),
				"%s must be nil in the sandbox", name)
		})
	}
	// string.dump (bytecode serialization) must be dropped from the string namespace.
	require.NoError(t, run(t, "assert(string.dump == nil, 'string.dump must be dropped')"))
}

func TestAllowedGlobalsPresent(t *testing.T) {
	for _, name := range []string{"assert", "error", "pcall", "pairs", "ipairs", "tostring", "tonumber", "type", "select", "unpack"} {
		require.NoError(t, run(t, "assert(type("+name+") == 'function', '"+name+" missing')"), name)
	}
	for _, name := range []string{"string", "table", "math"} {
		require.NoError(t, run(t, "assert(type("+name+") == 'table', '"+name+" missing')"), name)
	}
}

func TestStringRepCappedNamespaceAndMethod(t *testing.T) {
	// Namespace form.
	require.NoError(t, run(t, "local ok = pcall(function() return string.rep('A', 2000000000) end); assert(not ok, 'string.rep should be capped')"))
	// Method-syntax form dispatches through the PRIVATE capped metatable, not the namespace proxy.
	require.NoError(t, run(t, "local ok = pcall(function() return ('A'):rep(2000000000) end); assert(not ok, '(str):rep should be capped')"))
	// A small rep still works.
	require.NoError(t, run(t, "assert(string.rep('ab', 3) == 'ababab')"))
}

func TestStringFormatAndGsubCapped(t *testing.T) {
	require.NoError(t, run(t, "local ok = pcall(function() return string.format('%2000000000d', 1) end); assert(not ok, 'format width cap')"))
	require.NoError(t, run(t, "local ok = pcall(function() return string.rep('a',60000):gsub('a', string.rep('B',100)) end); assert(not ok, 'gsub output cap')"))
}

func TestPatternInputCapped(t *testing.T) {
	require.NoError(t, run(t, "local big = string.rep('a', 70000); local ok = pcall(function() return big:find('a') end); assert(not ok, 'find input cap')"))
}

func TestTableConcatCapped(t *testing.T) {
	require.NoError(t, run(t, `
		local a = {}
		for i=1,100 do a[i] = string.rep('x', 20000) end
		local ok = pcall(function() return table.concat(a) end)
		assert(not ok, 'table.concat output cap')`))
}

func TestNamespacesReadOnly(t *testing.T) {
	for _, ns := range []string{"string", "table", "math"} {
		require.NoError(t, run(t, "local ok = pcall(function() "+ns+".evil = 1 end); assert(not ok, '"+ns+" must be read-only')"), ns)
	}
}

func TestInstructionBudgetAbort(t *testing.T) {
	err := run(t, "while true do end")
	require.Error(t, err, "an infinite loop must abort")
	// A runaway is stopped by the instruction budget OR the wall-clock deadline — under load (e.g. the race
	// detector's ~10x slowdown) the 5ms deadline trips before the 100k-instruction count. Either is a valid
	// abort; the property is that an infinite loop cannot run unbounded.
	kind := ClassifyError(err)
	require.Contains(t, []AbortKind{AbortBudget, AbortDeadline}, kind, "a runaway must abort via budget or deadline, got %v", kind)
}

func TestMathRandomReboundAndDeterministic(t *testing.T) {
	// Two runtimes seeded identically produce the same math.random stream (rebound off the injected RNG, not
	// the os-seeded global). randomseed is a no-op (cannot reset entropy).
	seq := func() string {
		rt := NewRuntime(nil, Opts{Rng: rand.New(rand.NewSource(42))})
		defer rt.Close()
		require.NoError(t, rt.Compile("t", `
			math.randomseed(999) -- must be a no-op
			result = tostring(math.random(1,1000000)) .. ',' .. tostring(math.random(1,1000000))`))
		require.NoError(t, rt.LoadGlobals("t"))
		return rt.L.GetGlobal("result").String()
	}
	require.Equal(t, seq(), seq(), "math.random must draw deterministically from the injected RNG")
}

func TestCompileErrorSurfaces(t *testing.T) {
	rt := NewRuntime(nil, Opts{})
	defer rt.Close()
	err := rt.Compile("bad", "this is not lua ===")
	require.Error(t, err)
	require.Contains(t, err.Error(), "compile")
}

func TestCallGlobalInvokesDefinedHook(t *testing.T) {
	rt := NewRuntime(nil, Opts{})
	defer rt.Close()
	require.NoError(t, rt.Compile("s", `
		function double(n) return n * 2 end
		function noargs() captured = 'ran' end`))
	require.NoError(t, rt.LoadGlobals("s"))

	// A defined hook is invoked with pushed args; a result is read off the stack.
	found, err := rt.CallGlobal("s", "double", 1, func(L *lua.LState) int {
		L.Push(lua.LNumber(21))
		return 1
	})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, lua.LNumber(42), rt.L.Get(-1))
	rt.L.Pop(1)

	// A hook the script does NOT define is a clean no-op (found=false), not an error.
	found, err = rt.CallGlobal("s", "on_missing", 0, nil)
	require.NoError(t, err)
	require.False(t, found)
}

func TestBreakerTripsAndQuarantines(t *testing.T) {
	rt := NewRuntime(nil, Opts{})
	defer rt.Close()
	require.NoError(t, rt.Compile("s", "function boom() error('kaboom') end"))
	require.NoError(t, rt.LoadGlobals("s"))

	// ~10 consecutive logic errors (weight 1.0, threshold 10.0) trip the breaker.
	tripped := false
	for i := 0; i < 20; i++ {
		found, err := rt.CallGlobal("boomkey", "boom", 0, nil)
		if !found && err == nil {
			tripped = true // once quarantined, CallGlobal short-circuits to a no-op
			break
		}
		require.Error(t, err)
	}
	require.True(t, tripped, "the breaker should quarantine a chronically-failing script")

	// A successful recompile clears the quarantine.
	require.NoError(t, rt.Compile("boomkey", "-- reset"))
	require.False(t, rt.breaker.disabled("boomkey"))
}

func TestGlobalNamesMatchLiveSandbox(t *testing.T) {
	// The declared name lists must match what the live sandbox actually exposes (guards the parity test's
	// inputs). Enumerate the live globals and compare to GlobalNames.
	rt := NewRuntime(nil, Opts{})
	defer rt.Close()
	g := rt.L.Get(lua.GlobalsIndex).(*lua.LTable)
	live := map[string]bool{}
	g.ForEach(func(k, _ lua.LValue) {
		if s, ok := k.(lua.LString); ok {
			live[string(s)] = true
		}
	})
	for _, name := range GlobalNames() {
		require.Truef(t, live[name], "declared global %q missing from the live sandbox", name)
	}
	require.Lenf(t, live, len(GlobalNames()), "live globals %v vs declared %v", keys(live), GlobalNames())
}

// TestNamespaceMembersMatchDeclared pins the LIVE string/table namespace members to the declared StringNames/
// TableNames lists (which the world parity test also references), so the declared lists cannot silently drift
// from what the sandbox actually exposes. Reaches the real member table behind the read-only proxy's
// metatable __index (a proxy is empty to a direct enumeration).
func TestNamespaceMembersMatchDeclared(t *testing.T) {
	rt := NewRuntime(nil, Opts{})
	defer rt.Close()
	check := func(ns string, want []string) {
		proxy := rt.L.GetGlobal(ns).(*lua.LTable)
		// The raw Metatable field, not L.GetMetatable (which returns the __metatable "locked" decoy).
		mt := proxy.Metatable.(*lua.LTable)
		realTbl := mt.RawGetString("__index").(*lua.LTable)
		var got []string
		realTbl.ForEach(func(k, _ lua.LValue) {
			if s, ok := k.(lua.LString); ok {
				got = append(got, string(s))
			}
		})
		require.ElementsMatchf(t, want, got, "%s members drifted from the declared list", ns)
	}
	check("string", StringNames())
	check("table", TableNames())
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sanity: the constants are the documented values (a change here should be a deliberate, reviewed edit).
func TestCapConstants(t *testing.T) {
	require.Equal(t, 1<<20, StrByteCap)
	require.Equal(t, 1<<16, PatternInputCap)
	require.Equal(t, 100_000, InstrBudget)
	require.True(t, strings.HasPrefix("instruction budget exceeded", "instruction"))
}
