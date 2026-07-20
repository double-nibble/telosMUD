package world

import (
	"log/slog"
	"math/rand"
	"testing"

	lua "github.com/yuin/gopher-lua"

	"github.com/double-nibble/telosmud/internal/luasandbox"
	"github.com/stretchr/testify/require"
)

// lua_parity_test.go — the cross-sandbox parity net (#47). The director tier runs Lua through the shared
// internal/luasandbox core, while the zone runs its own (proven) sandbox mechanism here in internal/world.
// The two MUST NOT diverge on the security properties that matter, or a director script could reach a
// capability a zone script cannot (or an amplifier bomb one sandbox caps and the other doesn't). This test
// runs an identical battery of probe snippets in BOTH live sandboxes and asserts identical pass/fail — so a
// future edit that weakens one but not the other fails the build. It complements each sandbox's own suite
// (world's luart_test/fuzz + luasandbox's own tests): those prove each is correct; this proves they AGREE.

// runInLuasandbox compiles+loads src in a fresh luasandbox runtime and returns whether it errored.
func runInLuasandbox(t *testing.T, src string) bool {
	t.Helper()
	rt := luasandbox.NewRuntime(slog.Default(), luasandbox.Opts{Rng: rand.New(rand.NewSource(1))})
	t.Cleanup(rt.Close)
	if err := rt.Compile("t", src); err != nil {
		return true // a compile error counts as "errored" (both sandboxes compile identically)
	}
	return rt.LoadGlobals("t") != nil
}

// runInZone runs src in a fresh zone sandbox and returns whether it errored.
func runInZone(t *testing.T, src string) bool {
	t.Helper()
	rt := newLuaRuntime("parity-zone", slog.Default())
	t.Cleanup(rt.close)
	return rt.runChunk("t", src) != nil
}

func TestSandboxParityWithLuasandbox(t *testing.T) {
	// Each probe asserts a security property; wantErr is the expected outcome, which MUST be identical in
	// both sandboxes. A `false` (no error) probe proves an ALLOWED capability works in both; a `true` probe
	// proves a DROPPED capability / capped amplifier is blocked in both.
	probes := []struct {
		name    string
		src     string
		wantErr bool
	}{
		// Dropped dangerous globals — absent in both.
		{"os-absent", "assert(os == nil)", false},
		{"io-absent", "assert(io == nil)", false},
		{"load-absent", "assert(load == nil)", false},
		{"loadstring-absent", "assert(loadstring == nil)", false},
		{"require-absent", "assert(require == nil)", false},
		{"dofile-absent", "assert(dofile == nil)", false},
		{"getmetatable-absent", "assert(getmetatable == nil)", false},
		{"setmetatable-absent", "assert(setmetatable == nil)", false},
		{"rawset-absent", "assert(rawset == nil)", false},
		{"collectgarbage-absent", "assert(collectgarbage == nil)", false},
		{"coroutine-absent", "assert(coroutine == nil)", false},
		{"debug-absent", "assert(debug == nil)", false},
		{"stringdump-absent", "assert(string.dump == nil)", false},
		// The clear-and-repopulate setGlobals targets _G reachability specifically — probe it (and the raw*
		// escape hatches + next) in both sandboxes.
		{"_G-absent", "assert(_G == nil)", false},
		{"_ENV-absent", "assert(_ENV == nil)", false},
		{"rawget-absent", "assert(rawget == nil)", false},
		{"rawequal-absent", "assert(rawequal == nil)", false},
		{"rawlen-absent", "assert(rawlen == nil)", false},
		{"next-absent", "assert(next == nil)", false},
		// Allowed base capabilities — present in both.
		{"pcall-present", "assert(type(pcall) == 'function')", false},
		{"pairs-present", "assert(type(pairs) == 'function')", false},
		{"select-present", "assert(type(select) == 'function')", false},
		{"unpack-present", "assert(type(unpack) == 'function')", false},
		{"tostring-present", "assert(type(tostring) == 'function')", false},
		// Capped amplifiers — blocked in both (namespace + method syntax).
		{"rep-ns-cap", "string.rep('A', 2000000000)", true},
		{"rep-method-cap", "('A'):rep(2000000000)", true},
		{"format-width-cap", "string.format('%2000000000d', 1)", true},
		{"gsub-output-cap", "string.rep('a',60000):gsub('a', string.rep('B',100))", true},
		{"find-input-cap", "string.rep('a',70000):find('a')", true},
		{"concat-output-cap", "local a={}; for i=1,100 do a[i]=string.rep('x',20000) end; return table.concat(a)", true},
		// Read-only namespaces — writes blocked in both.
		{"string-readonly", "string.evil = 1", true},
		{"math-readonly", "math.evil = 1", true},
		{"table-readonly", "table.evil = 1", true},
		// Instruction budget — a runaway aborts in both.
		{"budget-abort", "while true do end", true},
		// Small legitimate uses — succeed in both.
		{"rep-small-ok", "assert(string.rep('ab',3) == 'ababab')", false},
		{"concat-small-ok", "assert(table.concat({'a','b','c'}, ',') == 'a,b,c')", false},
		{"math-floor-ok", "assert(math.floor(3.7) == 3)", false},
	}

	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			zoneErr := runInZone(t, p.src)
			sandboxErr := runInLuasandbox(t, p.src)
			if zoneErr != sandboxErr {
				t.Fatalf("SANDBOX DIVERGENCE on %q: zone errored=%v, luasandbox errored=%v — the two sandboxes disagree on this security property",
					p.name, zoneErr, sandboxErr)
			}
			if zoneErr != p.wantErr {
				t.Fatalf("probe %q: both sandboxes errored=%v, want %v (the probe's own expectation drifted)",
					p.name, zoneErr, p.wantErr)
			}
		})
	}
}

// namespaceMembers enumerates the member names of a read-only namespace proxy (string/table/math) by reaching
// the REAL table behind the proxy's metatable __index (a read-only proxy is empty to `pairs`; its members live
// on the __index target). It reads the proxy's raw Metatable field directly — L.GetMetatable would return the
// __metatable "locked" decoy the proxy installs, not the metatable — then follows __index to the member set.
func namespaceMembers(t *testing.T, L *lua.LState, ns string) []string {
	t.Helper()
	proxy, ok := L.GetGlobal(ns).(*lua.LTable)
	require.Truef(t, ok, "%s is not a table", ns)
	// Read the raw Metatable field directly — L.GetMetatable would return the __metatable "locked" decoy the
	// read-only proxy installs, not the real metatable.
	mt, ok := proxy.Metatable.(*lua.LTable)
	require.Truef(t, ok, "%s has no metatable (not a read-only proxy?)", ns)
	realTbl, ok := mt.RawGetString("__index").(*lua.LTable)
	require.Truef(t, ok, "%s metatable __index is not a table", ns)
	var names []string
	realTbl.ForEach(func(k, _ lua.LValue) {
		if s, ok := k.(lua.LString); ok {
			names = append(names, string(s))
		}
	})
	return names
}

// TestSandboxMemberKeysetsMatch closes the one seam the aliased constants + the fixed probe battery do NOT
// cover (scripting review): the string/table/math MEMBER lists are built independently in each sandbox, so a
// member added to only one (e.g. re-adding the dropped string.dump to one side) would pass every probe. This
// enumerates the LIVE members of each namespace in BOTH sandboxes and asserts they are identical — a one-sided
// member add fails the build.
func TestSandboxMemberKeysetsMatch(t *testing.T) {
	zoneRT := newLuaRuntime("parity-zone", slog.Default())
	defer zoneRT.close()
	sbRT := luasandbox.NewRuntime(slog.Default(), luasandbox.Opts{})
	defer sbRT.Close()

	for _, ns := range []string{"string", "table", "math"} {
		zoneMembers := namespaceMembers(t, zoneRT.L, ns)
		sbMembers := namespaceMembers(t, sbRT.L, ns)
		require.ElementsMatchf(t, zoneMembers, sbMembers,
			"%s namespace members DIVERGE between the zone and director sandboxes — a builtin was added/removed on only one side",
			ns)
	}
}

// TestSandboxCapConstantsShared asserts the zone's cap constants are literally the shared luasandbox values
// (they are aliases), documenting the single-source-of-truth and failing loudly if an alias is ever unwired.
func TestSandboxCapConstantsShared(t *testing.T) {
	cases := []struct {
		name       string
		zone, want int
	}{
		{"StrByteCap", luaStrByteCap, luasandbox.StrByteCap},
		{"PatternInputCap", luaPatternInputCap, luasandbox.PatternInputCap},
		{"StrAllocCap", luaStrAllocCap, luasandbox.StrAllocCap},
		{"InstrBudget", luaInstrBudget, luasandbox.InstrBudget},
		{"CallStackSize", luaCallStackSize, luasandbox.CallStackSize},
		{"RegistrySize", luaRegistrySize, luasandbox.RegistrySize},
		{"RegistryMaxSize", luaRegistryMaxSize, luasandbox.RegistryMaxSize},
	}
	for _, c := range cases {
		if c.zone != c.want {
			t.Errorf("%s: zone=%d != luasandbox=%d (the single-source alias is unwired)", c.name, c.zone, c.want)
		}
	}
}
