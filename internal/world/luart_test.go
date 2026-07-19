package world

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
	lua "github.com/yuin/gopher-lua"
)

// luart_test.go — slice 7.1 security gates (docs/PHASE7-PLAN.md §5, threat rows
// T1/T2/T3/T4/T9/T13/T14). These assert the sandbox holds against a hostile script: the
// full DROP set is absent, the amplifier builtins are capped, the string metatable is
// unreachable (cross-script method-syntax invariance), the budgets abort, and a bare zone
// is unchanged. The security-auditor reviews these as the slice's done-when.

func newTestLua(t *testing.T) *luaRuntime {
	t.Helper()
	rt := newLuaRuntime("test-zone", slog.New(slog.NewTextHandler(testWriter{t}, nil)))
	t.Cleanup(rt.close)
	return rt
}

// testWriter funnels the runtime's slog output into the test log so a print()/mud.log line
// is visible when a test fails.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// mustRun runs src and fails the test on any error (compile or runtime).
func mustRun(t *testing.T, rt *luaRuntime, src string) {
	t.Helper()
	if err := rt.runChunk(t.Name(), src); err != nil {
		t.Fatalf("script errored: %v\nsrc: %s", err, src)
	}
}

// --- T1/T2/T14: the DROP set is absent in a fresh script _ENV -----------------------------

// TestSandboxDropSetAbsent asserts EVERY dropped global (PHASE7-PLAN.md §2.1) is nil in a
// fresh script environment. Absence (never registered), not deletion — the safe construction.
func TestSandboxDropSetAbsent(t *testing.T) {
	rt := newTestLua(t)
	dropped := []string{
		// code loading / FFI / eval (T1)
		"load", "loadstring", "dofile", "loadfile", "require", "module", "package",
		// metatable / raw / env reach (T1/T14)
		"getmetatable", "setmetatable", "rawget", "rawset", "rawequal", "rawlen",
		"next", "_G", "setfenv", "getfenv", "newproxy", "collectgarbage",
		// host reach (T2)
		"os", "io", "debug",
		// concurrency (T6)
		"coroutine", "channel",
	}
	for _, name := range dropped {
		mustRun(t, rt, "assert("+name+" == nil, '"+name+" should be nil but was not')")
	}
	// string.dump must be absent on the script-visible string namespace (T1 — bytecode dump).
	mustRun(t, rt, "assert(string.dump == nil, 'string.dump should be nil')")
}

// TestSandboxLoadErrors asserts load() is unavailable — calling it errors (it is nil, so the
// call is "attempt to call a nil value"). A script cannot eval new code (T1).
func TestSandboxLoadErrors(t *testing.T) {
	rt := newTestLua(t)
	mustRun(t, rt, `local ok = pcall(function() return load("return 1") end); assert(ok == false, "load must not be callable")`)
}

// TestSandboxKeptPresent asserts the allowlisted globals ARE present (the positive of the
// absence test — a sandbox that dropped everything would also pass the absence test).
func TestSandboxKeptPresent(t *testing.T) {
	rt := newTestLua(t)
	for _, name := range []string{
		"assert", "error", "pcall", "xpcall", "select", "type", "tostring", "tonumber",
		"pairs", "ipairs", "unpack", "print", "string", "table", "math",
	} {
		mustRun(t, rt, "assert("+name+" ~= nil, '"+name+" should be present')")
	}
	// Sanity: the kept primitives actually work.
	mustRun(t, rt, `assert(type({}) == "table"); assert(tonumber("3") == 3); assert(tostring(5) == "5")`)
	mustRun(t, rt, `local n=0; for _,v in ipairs({1,2,3}) do n=n+v end; assert(n==6)`)
	mustRun(t, rt, `assert(unpack({7,8})==7)`)
}

// --- T13: capped amplifier builtins -------------------------------------------------------

// TestCappedRep asserts string.rep("A", 2e9) is rejected at the cap (clean error, no
// multi-GB allocation), and that a within-cap rep still works.
func TestCappedRep(t *testing.T) {
	rt := newTestLua(t)
	mustRun(t, rt, `local ok = pcall(function() return string.rep("A", 2e9) end); assert(ok == false, "huge rep must be rejected")`)
	mustRun(t, rt, `assert(string.rep("ab", 3) == "ababab")`)
	// method syntax goes through the same cap.
	mustRun(t, rt, `local ok = pcall(function() return ("A"):rep(2e9) end); assert(ok == false, "huge method rep must be rejected")`)
}

// TestCappedFormat asserts string.format with an enormous explicit field width is rejected
// (one builtin call would otherwise expand a tiny format into a huge result).
func TestCappedFormat(t *testing.T) {
	rt := newTestLua(t)
	mustRun(t, rt, `local ok = pcall(function() return string.format("%2000000000d", 1) end); assert(ok == false, "huge width must be rejected")`)
	mustRun(t, rt, `assert(string.format("%d-%s", 4, "x") == "4-x")`)
}

// TestCappedConcat asserts table.concat over the byte cap is rejected, and a normal concat
// works.
func TestCappedConcat(t *testing.T) {
	rt := newTestLua(t)
	mustRun(t, rt, `assert(table.concat({"a","b","c"}, ",") == "a,b,c")`)
	mustRun(t, rt, `
		local t = {}
		local big = string.rep("A", 100000)  -- within rep cap
		for i=1,100 do t[i] = big end        -- 10MB total > 1MB cap
		local ok = pcall(function() return table.concat(t) end)
		assert(ok == false, "huge concat must be rejected")
	`)
}

// TestPatternBacktrackBound asserts a known-pathological pattern over a long subject is
// bounded: the over-cap INPUT is rejected before the matcher can backtrack super-linearly.
func TestPatternBacktrackBound(t *testing.T) {
	rt := newTestLua(t)
	mustRun(t, rt, `
		local subject = string.rep("a", 200000)  -- > pattern input cap
		local ok = pcall(function() return subject:match("(a*)(a*)(a*)b") end)
		assert(ok == false, "over-cap pattern input must be rejected")
	`)
	// a normal match still works.
	mustRun(t, rt, `assert(("hello world"):match("(%w+)") == "hello")`)
	mustRun(t, rt, `assert(("a1b2"):gsub("%d", "#") == "a#b#")`)
}

// TestGsubOutputCapStringRepl is the regression for the auditor's 370 GB DoS: gsub builds its
// result in a Go builtin with no VM re-entry, so the input cap alone leaves a
// matches×replacement alloc bomb. The OUTPUT (not input) must be bounded BEFORE the raw gsub
// runs. We assert the exact repro errors fast AND that the rejection allocates a bounded
// amount of memory (no multi-GB churn).
func TestGsubOutputCapStringRepl(t *testing.T) {
	rt := newTestLua(t)
	// The auditor's repro: 60000 matches × a 100-byte replacement = ~6 MB; with a 1 MiB
	// replacement and a 64 KiB subject the unbounded worst case is ~64 GiB. The output cap
	// must reject it up front.
	assertBoundedReject(t, func() {
		mustRunExpectErr(t, rt, `
			local subject = ("a"):rep(60000)
			local repl = string.rep("B", 100)
			return subject:gsub("a", repl)
		`)
	})
	// An even more extreme single-line bomb: a 1 MiB replacement over a near-cap subject.
	assertBoundedReject(t, func() {
		mustRunExpectErr(t, rt, `
			local subject = ("a"):rep(60000)
			local repl = string.rep("B", 1000000)  -- 1 MiB, within rep cap
			return subject:gsub("a", repl)
		`)
	})
	// A normal, within-cap gsub still works.
	mustRun(t, rt, `assert(("hello"):gsub("l", "L") == "heLLo")`)
	mustRun(t, rt, `local s,n = ("a a a"):gsub("a", "bb"); assert(s == "bb bb bb" and n == 3)`)
}

// TestGsubOutputCapFuncRepl asserts a FUNCTION replacement cannot accumulate an unbounded
// result across many matches. The guarded callback tracks cumulative returned bytes and
// errors the instant the running total exceeds the cap — so a callback that returns a large
// string per match over many matches is rejected quickly with a bounded allocation, NOT after
// churning hundreds of MB/GB.
func TestGsubOutputCapFuncRepl(t *testing.T) {
	rt := newTestLua(t)
	assertBoundedReject(t, func() {
		// 60000 matches × a 60 KB return = ~3.6 GB unbounded. The cumulative guard must reject
		// it after ~luaStrByteCap (1 MiB) of accumulated output — fast and bounded.
		mustRunExpectErr(t, rt, `
			local subject = ("a"):rep(60000)
			local chunk = string.rep("C", 60000)
			return subject:gsub("a", function() return chunk end)
		`)
	})
	// A normal function replacement still works — and the guard preserves gsub semantics:
	// captures are passed through, and a nil/false return keeps the original match.
	mustRun(t, rt, `
		local s, n = ("a1b2"):gsub("%d", function(d) return "["..d.."]" end)
		assert(s == "a[1]b[2]" and n == 2, "got "..s)
	`)
	mustRun(t, rt, `
		local s = ("key=val"):gsub("(%w+)=(%w+)", function(k, v) return v.."="..k end)
		assert(s == "val=key", "captures not passed through: got "..s)
	`)
	mustRun(t, rt, `
		local s = ("abc"):gsub("%a", function(c) if c == "b" then return "B" end end)
		assert(s == "aBc", "nil-return should keep original: got "..s)
	`)
}

// TestGsubOutputCapTableRepl asserts a TABLE replacement is bounded the same way: each lookup
// can return a big string, but the cumulative guard rejects the run before it accumulates
// unbounded output.
func TestGsubOutputCapTableRepl(t *testing.T) {
	rt := newTestLua(t)
	assertBoundedReject(t, func() {
		mustRunExpectErr(t, rt, `
			local big = string.rep("Z", 100000)
			local t = { a = big }                 -- every "a" match maps to the big value
			local subject = ("a"):rep(60000)
			return subject:gsub("(a)", t)
		`)
	})
	// A normal table replacement still works.
	mustRun(t, rt, `
		local t = { hp = "health", mp = "mana" }
		local s = ("hp/mp"):gsub("(%a+)", t)
		assert(s == "health/mana", "got "..s)
	`)
}

// TestFormatOutputCapStringArgs is the regression for the 10 MB %s bomb: maxFormatWidth scans
// only %-width/precision tokens, never the %s argument byte lengths. The wrapper must sum the
// string-argument lengths and reject an over-cap result BEFORE the uninterruptible strFormat.
func TestFormatOutputCapStringArgs(t *testing.T) {
	rt := newTestLua(t)
	assertBoundedReject(t, func() {
		mustRunExpectErr(t, rt, `
			local big = string.rep("x", 500000)          -- 500 KB, within rep cap
			local fmt = ("%s"):rep(20)                    -- twenty %s directives
			return string.format(fmt, big, big, big, big, big, big, big, big, big, big,
			                           big, big, big, big, big, big, big, big, big, big)
		`)
	})
	// A normal format with a small string arg still works.
	mustRun(t, rt, `assert(string.format("[%s]=%d", "hp", 42) == "[hp]=42")`)
}

// mustRunExpectErr runs src and FAILS the test if it does NOT error (the inverse of mustRun).
func mustRunExpectErr(t *testing.T, rt *luaRuntime, src string) {
	t.Helper()
	if err := rt.runChunk(t.Name(), src); err == nil {
		t.Fatalf("expected a cap rejection but the script succeeded\nsrc: %s", src)
	}
}

// assertBoundedReject runs fn and asserts it completes quickly with a BOUNDED heap allocation
// — proving a cap rejection does not first churn gigabytes (the auditor's 370 GB / 22 s
// signature). The threshold is generous (256 MiB) so legitimate test scaffolding allocation
// never trips it, while a multi-GB churn fails loudly.
func assertBoundedReject(t *testing.T, fn func()) {
	t.Helper()
	const allocCeiling = 256 << 20 // 256 MiB
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cap rejection did not complete within 10s (likely an unbounded builtin churn)")
	}
	runtime.ReadMemStats(&after)
	if delta := after.TotalAlloc - before.TotalAlloc; delta > allocCeiling {
		t.Fatalf("cap rejection allocated %d bytes (> %d ceiling) — output not bounded before alloc", delta, allocCeiling)
	}
}

// --- T14: cross-script method-syntax invariance (the load-bearing one) --------------------

// TestStringMetatableUnreachable is the load-bearing T14 test: a script doing `string.rep =
// evil` (and other reach attempts) CANNOT change what ("x"):rep(2) returns in a SIBLING
// script. getmetatable is nil (the getmetatable("") reach is closed).
func TestStringMetatableUnreachable(t *testing.T) {
	rt := newTestLua(t)

	// getmetatable is absent — the getmetatable("").rep = evil reach is closed.
	mustRun(t, rt, `assert(getmetatable == nil, "getmetatable must be absent")`)

	// Attempt every reach a hostile script has: overwrite the proxy field (blocked by
	// read-only), rebind the global, etc. The read-only proxy errors on write; swallow it.
	_ = rt.runChunk("attacker", `
		pcall(function() string.rep = function() return "EVIL" end end)
		pcall(function() string = { rep = function() return "EVIL" end } end)
		pcall(function() rawset(string, "rep", function() return "EVIL" end) end)
	`)

	// A SIBLING script — method syntax must be unchanged (it dispatches through the private,
	// unreachable string metatable, not the proxy global).
	mustRun(t, rt, `assert(("x"):rep(2) == "xx", "method-syntax rep poisoned: got "..tostring(("x"):rep(2)))`)
	// And the cap still holds on the method path after the attack.
	mustRun(t, rt, `local ok = pcall(function() return ("A"):rep(2e9) end); assert(ok == false)`)

	// Even nulling the `string` global entirely must NOT break method-syntax dispatch — the
	// metatable is a separate, unreachable object, not the proxy.
	_ = rt.runChunk("nuller", `pcall(function() string = nil end)`)
	mustRun(t, rt, `assert(("y"):rep(3) == "yyy", "method syntax broke after string=nil")`)

	// The raw string lib (with its bytecode-dumping `dump`) must be unreachable via BOTH the
	// proxy namespace and method syntax.
	mustRun(t, rt, `assert(("x").dump == nil, "string.dump reachable via method syntax")`)
}

// TestStringProxyReadOnly asserts the script-visible `string` namespace rejects writes.
func TestStringProxyReadOnly(t *testing.T) {
	rt := newTestLua(t)
	mustRun(t, rt, `local ok = pcall(function() string.rep = 1 end); assert(ok == false, "string namespace must be read-only")`)
	mustRun(t, rt, `local ok = pcall(function() math.pi = 0 end); assert(ok == false, "math namespace must be read-only")`)
	mustRun(t, rt, `local ok = pcall(function() table.insert = 1 end); assert(ok == false, "table namespace must be read-only")`)
}

// --- T3: instruction-count + deadline abort, zone survives --------------------------------

// TestInstructionBudgetAborts asserts a tight pure-CPU loop aborts (the vendored fork's
// instruction count), the abort is DETERMINISTIC (same outcome every run), and the runtime
// keeps serving the next chunk afterward (the zone survives).
func TestInstructionBudgetAborts(t *testing.T) {
	rt := newTestLua(t)
	err := rt.runChunk("loop", `while true do end`)
	if err == nil {
		t.Fatal("expected the infinite loop to abort")
	}
	if !strings.Contains(err.Error(), "instruction budget") && !strings.Contains(err.Error(), "context") {
		t.Fatalf("abort should cite the budget or the deadline, got: %v", err)
	}
	// The runtime keeps serving — the zone is not stalled or dead.
	mustRun(t, rt, `assert(1 + 1 == 2)`)
}

// TestInstructionBudgetDeterministic asserts the instruction-count abort is reproducible: a
// pure-CPU loop trips at the same count every run (unlike wall-clock). We arm a small budget
// and observe the count lands at the budget+1 each time.
func TestInstructionBudgetDeterministic(t *testing.T) {
	for run := 0; run < 3; run++ {
		rt := newLuaRuntime("det-zone", slog.Default())
		// A small, fixed budget makes the trip count exact and fast.
		rt.L.SetInstructionBudget(5000)
		err := rt.runChunk("loop", `local i=0; while true do i=i+1 end`)
		if err == nil || !strings.Contains(err.Error(), "instruction budget") {
			t.Fatalf("run %d: expected instruction-budget abort, got %v", run, err)
		}
		if got := rt.L.InstructionCount(); got != 5001 {
			t.Fatalf("run %d: instruction count = %d, want 5001 (deterministic trip)", run, got)
		}
		rt.close()
	}
}

// TestBudgetResetPerCall asserts the count resets between calls (a previous near-budget call
// does not starve the next): two independent short chunks both succeed.
func TestBudgetResetPerCall(t *testing.T) {
	rt := newTestLua(t)
	rt.L.SetInstructionBudget(5000)
	for i := 0; i < 5; i++ {
		mustRun(t, rt, `local s=0; for j=1,100 do s=s+j end; assert(s==5050)`)
	}
}

// --- T4: CallStackSize set, recursion errors cleanly --------------------------------------

// TestCallStackSizeSet asserts the VM was built with the recursion cap (T4) and deep
// recursion errors out as a catchable Lua error (caught by pcall) rather than overflowing the
// host goroutine stack and crashing the process.
func TestCallStackSizeSet(t *testing.T) {
	rt := newTestLua(t)
	if rt.L.Options.CallStackSize != luaCallStackSize {
		t.Fatalf("CallStackSize = %d, want %d", rt.L.Options.CallStackSize, luaCallStackSize)
	}
	// Infinite recursion must error cleanly (the process must not SIGSEGV) and the runtime
	// keeps serving afterward.
	err := rt.runChunk("rec", `local function f() return f() end; f()`)
	if err == nil {
		t.Fatal("expected deep recursion to error")
	}
	mustRun(t, rt, `assert(true)`)
}

// --- T9: determinism (rebound math.random, no-op randomseed) ------------------------------

// TestRandomseedNoOp asserts math.randomseed is a no-op: calling it does not change the RNG
// stream (a script cannot reset the per-zone entropy).
func TestRandomseedNoOp(t *testing.T) {
	rt := newTestLua(t)
	mustRun(t, rt, `math.randomseed(12345)`) // must not error
	// Two zones with the SAME id produce the SAME stream regardless of any randomseed call.
	rt2 := newLuaRuntime("test-zone", slog.Default())
	defer rt2.close()
	var a, b float64
	rt.L.SetContext(context.Background()) // not strictly needed; runChunk arms its own
	rt.L.RemoveContext()
	collect := func(r *luaRuntime, out *float64) {
		r.L.SetGlobal("__out", r.L.NewFunction(func(l *lua.LState) int { *out = float64(l.CheckNumber(1)); return 0 }))
		if err := r.runChunk("rng", `math.randomseed(99); __out(math.random())`); err != nil {
			t.Fatal(err)
		}
	}
	collect(rt, &a)
	collect(rt2, &b)
	if a != b {
		t.Fatalf("same-seeded zones diverged despite randomseed: %v vs %v", a, b)
	}
}

// TestSeededRandomDeterministic asserts two zones seeded identically (same zone id) produce
// identical math.random sequences (T9 — combat/loot/procs reproducible).
func TestSeededRandomDeterministic(t *testing.T) {
	seq := func(zoneID string) []int {
		rt := newLuaRuntime(zoneID, slog.Default())
		defer rt.close()
		var got []int
		rt.L.SetGlobal("__push", rt.L.NewFunction(func(l *lua.LState) int {
			got = append(got, l.CheckInt(1))
			return 0
		}))
		if err := rt.runChunk("rng", `for i=1,8 do __push(math.random(1,100)) end`); err != nil {
			t.Fatal(err)
		}
		return got
	}
	a := seq("alpha")
	b := seq("alpha")
	c := seq("beta")
	if len(a) != 8 {
		t.Fatalf("expected 8 draws, got %d", len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same-seed zones diverged at %d: %d vs %d", i, a[i], b[i])
		}
	}
	// Different zone ids should (overwhelmingly likely) differ somewhere — a sanity check that
	// the seed actually depends on the id.
	same := true
	for i := range a {
		if a[i] != c[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different zone ids produced identical RNG streams (seed not id-dependent)")
	}
}

// --- print -> mud.log redirect ------------------------------------------------------------

// TestPrintRedirectsToLog asserts print() reaches the structured log (no stdout, no error).
func TestPrintRedirectsToLog(t *testing.T) {
	var logged atomic.Bool
	rt := newLuaRuntime("log-zone", slog.New(captureHandler{&logged}))
	defer rt.close()
	mustRun(t, rt, `print("hello", 1, true)`)
	if !logged.Load() {
		t.Fatal("print did not reach the structured log")
	}
}

type captureHandler struct{ hit *atomic.Bool }

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "lua print") {
		h.hit.Store(true)
	}
	return nil
}
func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

// --- bare-zone invariant ------------------------------------------------------------------

// TestBareZoneHasInertLua asserts a zone built with NO scripted content still has a live VM
// that runs nothing on its own — the bare-engine invariant (Phase-6 empty-boot behavior is
// byte-identical; Lua is available but inert). The empty shard boots, logs a player in and
// out cleanly, and the VM exists but was never asked to run a script.
func TestBareZoneHasInertLua(t *testing.T) {
	empty, _ := content.Load(context.Background(), nil, nil)
	shard := NewShardFromContent(empty, []string{"void"}, "void", "", nil, nil)
	z := shard.Zone()
	if z == nil {
		t.Fatal("empty shard has no home zone")
	}
	if z.lua == nil || z.lua.L == nil {
		t.Fatal("bare zone should still have a live (but inert) Lua VM")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	out := make(chan *playv1.ServerFrame, 16)
	var cz atomic.Pointer[Zone]
	z.claimInboundArrival() // the claim the production resolver takes under s.mu; the handler releases one unconditionally (#413)
	z.post(attachMsg{character: "Inert", out: out, curZone: &cz})
	got := nextOutput(t, &session{character: "Inert", out: out})
	if !strings.Contains(got, "no rooms") {
		t.Fatalf("bare-world login = %q, want a 'no rooms' rejection (zone alive, Lua inert)", got)
	}
}
