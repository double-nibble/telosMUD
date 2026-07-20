package luasandbox

import (
	"context"
	"math/rand"
	"runtime"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// stralloc_test.go — #438: the per-call string-allocation budget, engine side.
//
// The fork owns the concat opcode's charge and tests it there. These tests cover what the ENGINE adds: that
// the budget is actually ARMED on a constructed sandbox (a cap nobody arms is the classic inert bound), that
// the string BUILTINS charge against it, and that the abort is CLASSIFIED so the breaker weights it as the
// deterministic, content-pathological thing it is rather than as transient host load.

// runPatient executes src through the REAL per-call chokepoint (NewRuntime, so the deadline is armed and the
// instruction count reset exactly as in production) but with a deliberately generous deadline and instruction
// budget, so that whatever stops these programs is the allocation budget and not the wall clock.
//
// That distinction is the whole point: the wall clock is what stopped every one of these programs BEFORE
// #438, so a test run at the default deadline passes identically with this change reverted.
func runPatient(t *testing.T, src string) error {
	t.Helper()
	rt := NewRuntime(nil, Opts{
		Rng:            rand.New(rand.NewSource(1)),
		CallDeadlineMS: 1000,
		InstrBudget:    MaxInstrBudget,
	})
	t.Cleanup(rt.Close)
	if err := rt.Compile("t", src); err != nil {
		t.Fatalf("compile: %v", err)
	}
	return rt.LoadGlobals("t")
}

// TestSandboxArmsTheAllocBudget is the WIRING test, and it is the one that matters most.
//
// TestSandboxCapConstantsShared-style assertions compare CONSTANTS: they pass whether or not anything calls
// SetStringByteBudget. And every probe below would also pass unarmed, because these programs eventually hit
// the wall-clock deadline anyway. So this asks the constructed VM directly.
func TestSandboxArmsTheAllocBudget(t *testing.T) {
	L := New(Opts{})
	defer L.Close()

	// Charge one byte and read the tally back: the only way to observe the armed budget from outside, and it
	// distinguishes "armed" from "the charge is a no-op because the budget is 0".
	if !L.ChargeStringBytes(1) {
		t.Fatal("charging a single byte failed on a fresh sandbox")
	}
	if got := L.StringBytesCharged(); got != 1 {
		t.Fatalf("StringBytesCharged = %d after charging 1 byte; the sandbox never armed the budget, so every "+
			"charge is a no-op and the cap is inert", got)
	}
}

// TestAllocBudgetIsReachableAtTheDefaultDeadline. A cap above what the shipping deadline can allocate is
// INERT: it never fires in production and every test still passes, because the deadline does the work. That
// is exactly how #368's instruction budget went dead above ~850k, and it is the most likely way for this
// constant to become decoration.
//
// So: run the bomb under the REAL default deadline and require the allocation abort to be what stops it.
func TestAllocBudgetIsReachableAtTheDefaultDeadline(t *testing.T) {
	L := New(Opts{})
	defer L.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(CallDeadline)*time.Millisecond)
	defer cancel()
	L.SetContext(ctx)
	L.ResetInstructionCount()

	err := L.DoString(`local s = string.rep("x", 1048576)
for i = 1, 40 do s = s .. s end`)
	if err == nil {
		t.Fatal("the doubling bomb completed")
	}
	if got := ClassifyError(err); got != AbortAlloc {
		t.Fatalf("at the default %dms deadline the bomb was stopped by %v, not AbortAlloc (%v). A cap the "+
			"deadline always beats is inert: it never fires in production, and every other test in this file "+
			"still passes because the deadline does the work", CallDeadline, got, err)
	}
}

// TestAllocBudgetBoundsActualAllocation carries its own ORACLE. It runs the identical bomb twice — once on
// the real sandbox, once on a bare LState with the budget explicitly disarmed — and requires an order of
// magnitude between them.
//
// Asserting only "it errors" would be vacuous: the disarmed run errors too, on the deadline. The gap is the
// thing the change cannot fake, and it is also what catches a charge levied AFTER the allocation.
func TestAllocBudgetBoundsActualAllocation(t *testing.T) {
	const bomb = `local s = string.rep("x", 1048576)
for i = 1, 40 do s = s .. s end`

	measure := func(arm bool) float64 {
		L := New(Opts{})
		defer L.Close()
		if !arm {
			L.SetStringByteBudget(0)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		L.SetContext(ctx)
		L.ResetInstructionCount()

		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		_ = L.DoString(bomb)
		runtime.ReadMemStats(&m1)
		return float64(m1.TotalAlloc-m0.TotalAlloc) / (1 << 20)
	}

	armed, disarmed := measure(true), measure(false)
	t.Logf("armed=%.1f MB disarmed=%.1f MB", armed, disarmed)
	if disarmed < 10*armed {
		t.Fatalf("the budget did not meaningfully bound allocation: armed %.1f MB vs disarmed %.1f MB. A "+
			"charge that is a no-op — or one levied after the allocation — produces exactly this", armed, disarmed)
	}
}

// TestAllocBudgetChargesTheStringBuiltins. The fork covers the `..` opcode; the engine's own capped wrappers
// have to charge too, or a loop of individually-legal string.rep calls bypasses the budget entirely and the
// per-call bound is only a bound on one operator.
//
// Each case is built from operations that are each WELL inside StrByteCap: what trips is the running total.
func TestAllocBudgetChargesTheStringBuiltins(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		// 16 KiB per rep, 4096 times = 64 MiB. Each single call is 1/64th of StrByteCap.
		{"string.rep", `local t = {}
for i = 1, 4096 do t[i] = string.rep("x", 16384) end`},
		// Same shape through format's %s path.
		{"string.format", `local chunk = string.rep("y", 16384)
local t = {}
for i = 1, 4096 do t[i] = string.format("%s", chunk) end`},
		// And through table.concat.
		{"table.concat", `local parts = {}
for i = 1, 16 do parts[i] = string.rep("z", 1024) end
local t = {}
for i = 1, 4096 do t[i] = table.concat(parts) end`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runPatient(t, tc.src)
			if err == nil {
				t.Fatalf("64 MiB of individually-legal %s calls completed under an %d-byte per-call budget; "+
					"this builtin does not charge, so a loop of legal operations bypasses the bound",
					tc.name, StrAllocCap)
			}
			if got := ClassifyError(err); got != AbortAlloc {
				t.Fatalf("stopped by %v, not the allocation budget: %v", got, err)
			}
		})
	}
}

// TestAllocChargeIsLeviedBeforeTheAllocation. The single load-bearing decision in the whole change, and it
// was UNTESTED until review moved the charge below strings.Join and every test still passed.
//
// The doubling bomb cannot detect the ordering: charging after only permits one extra allocation of roughly
// the size already reached, so 7 MB becomes 7 MB and no ratio gate trips. What detects it is a single CONCAT
// OPCODE with many operands — Lua compiles `a .. a .. a .. ...` into ONE instruction, so one uninterruptible
// Join builds the whole result. Charge first and it never runs; charge after and the full result is built and
// then complained about. Measured: 1.3 MB versus 129 MB for 128 operands.
func TestAllocChargeIsLeviedBeforeTheAllocation(t *testing.T) {
	// 128 operands of 1 MiB in ONE expression: 128 MiB from a single opcode, against an 8 MiB budget.
	var b strings.Builder
	b.WriteString("local c = string.rep(\"x\", 1048576)\nlocal s = c")
	for i := 0; i < 127; i++ {
		b.WriteString(" .. c")
	}

	L := New(Opts{})
	defer L.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	L.SetContext(ctx)
	L.ResetInstructionCount()

	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	err := L.DoString(b.String())
	runtime.ReadMemStats(&m1)
	allocMB := float64(m1.TotalAlloc-m0.TotalAlloc) / (1 << 20)

	if err == nil {
		t.Fatal("a 128 MiB single-opcode concat completed under an 8 MiB budget")
	}
	if got := ClassifyError(err); got != AbortAlloc {
		t.Fatalf("stopped by %v, not the allocation budget: %v", got, err)
	}
	t.Logf("single-opcode concat of 128 x 1 MiB: allocated %.1f MB", allocMB)
	// The 1 MiB seed itself is charged and allocated, so allow generous headroom above it — but nothing like
	// the 128 MiB the result would take.
	if allocMB > 32 {
		t.Fatalf("the refused concat still allocated %.1f MB. The charge is being levied AFTER strings.Join, "+
			"so the full result is built and only then complained about — which is the entire failure this "+
			"budget exists to prevent, since one Join is uninterruptible", allocMB)
	}
}

// TestAllocBudgetChargesTheOneToOneTransforms. string.lower/upper/reverse/char amplify nothing but each
// allocates a whole new string of script-controlled size, and they sat in the "no amplification" passthrough
// list uncharged until review measured 2 GB in a single call through string.reverse.
//
// "Does not multiply its input" and "does not allocate" are different questions; only the second bounds
// memory. The one that got away also aborted on the DEADLINE, i.e. in the 0.1-weight bucket this change
// exists to move memory bombs out of.
func TestAllocBudgetChargesTheOneToOneTransforms(t *testing.T) {
	for _, name := range stringTransforms {
		if name == "char" {
			continue // charges per ARGUMENT; reaching 8 MiB that way is 8M VM ops. See the test below.
		}
		t.Run(name, func(t *testing.T) {
			// 64 iterations over a 1 MiB string = 64 MiB of 1:1 transforms, each single call entirely legal.
			src := `local s = string.rep("x", 1048576)
local t = {}
for i = 1, 64 do t[i] = string.` + name + `(s) end`
			err := runPatient(t, src)
			if err == nil {
				t.Fatalf("64 MiB of string.%s calls completed under an %d-byte per-call budget: this builtin "+
					"allocates proportional to script-controlled input and charges nothing, so a loop through "+
					"it is an uncharged memory bomb", name, StrAllocCap)
			}
			if got := ClassifyError(err); got != AbortAlloc {
				t.Fatalf("stopped by %v, not the allocation budget: %v", got, err)
			}
		})
	}
}

// TestAllocBudgetChargesFormatFieldWidth. `string.format("%1000000d", 1)` allocates a megabyte from an
// eleven-byte format string and NO string arguments, so a charge summing only len(format) and the %s
// arguments undercharged it ~100,000x — measured 1.7 GB allocated against 15 KB charged.
func TestAllocBudgetChargesFormatFieldWidth(t *testing.T) {
	err := runPatient(t, `local t = {}
for i = 1, 64 do t[i] = string.format("%1000000d", i) end`)
	if err == nil {
		t.Fatalf("64 MiB of field-width padding completed under an %d-byte budget; the width is validated "+
			"against the per-op cap but never charged, so it is free", StrAllocCap)
	}
	if got := ClassifyError(err); got != AbortAlloc {
		t.Fatalf("stopped by %v, not the allocation budget: %v", got, err)
	}
}

// TestTableConcatIsChargedExactlyOnce. table.concat delegates to gopher-lua's tableConcat, which is built on
// the concat opcode's helper — so the FORK already charges it, and the engine wrapper charging it too billed
// exactly 2x (measured), halving the effective budget for the idiom the docs recommend INSTEAD of the
// quadratic accumulator.
//
// The engine wrapper's charge was therefore removed, which leaves an invisible coupling to the delegate's
// internals. This test is what makes it visible: if gopher-lua ever stops routing table.concat through the
// opcode, the charge silently disappears and this fails rather than the bound quietly going away.
func TestTableConcatIsChargedExactlyOnce(t *testing.T) {
	L := New(Opts{})
	defer L.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	L.SetContext(ctx)
	L.ResetInstructionCount()

	// Build the parts with string.rep (charged), note the tally, then concat and look at the delta only.
	if err := L.DoString(`parts = {}
for i = 1, 64 do parts[i] = string.rep("x", 1024) end`); err != nil {
		t.Fatal(err)
	}
	before := L.StringBytesCharged()
	if err := L.DoString(`joined = table.concat(parts)`); err != nil {
		t.Fatal(err)
	}
	const result = 64 * 1024
	charged := L.StringBytesCharged() - before

	if charged == 0 {
		t.Fatalf("table.concat charged NOTHING for a %d-byte result. The engine wrapper does not charge (by "+
			"design — the fork's concat opcode does), so if the delegate stopped routing through that opcode "+
			"the bound has silently disappeared", result)
	}
	if charged > 1.5*result {
		t.Fatalf("table.concat charged %d bytes for a %d-byte result (%.1fx). It is being charged twice — "+
			"once by the fork's opcode and once by the engine wrapper — which halves the effective budget for "+
			"the idiom recommended over the quadratic accumulator", charged, result, float64(charged)/result)
	}
}

// TestAllocAbortMessageExplainsTheQuadratic. The budget's most surprising property is that `s = s .. chunk`
// charges O(n^2), so an author refused after building a 34 KB string sees a cap quoted in megabytes and has
// no way to connect the two. The remedy has to be in the message: they will not read this file.
func TestAllocAbortMessageExplainsTheQuadratic(t *testing.T) {
	err := runPatient(t, `local chunk = string.rep("x", 4096)
local s = ""
for i = 1, 4096 do s = s .. chunk end`)
	if err == nil {
		t.Fatal("the accumulator completed")
	}
	if !strings.Contains(err.Error(), "table.concat") {
		t.Fatalf("the refusal does not name the remedy, so an author who built a 34 KB string and was told "+
			"about an 8 MB cap has nothing to act on: %v", err)
	}
}

// TestAllocBudgetChargesStringChar covers the fourth 1:1 transform, whose charge is its ARGUMENT COUNT rather
// than an input length — reaching the cap through it would take ~8M VM operations, so this asserts the charge
// DIRECTLY instead. That is the sharper assertion anyway: it pins the amount billed, not merely that
// something eventually refused.
func TestAllocBudgetChargesStringChar(t *testing.T) {
	rt := NewRuntime(nil, Opts{Rng: rand.New(rand.NewSource(1)), CallDeadlineMS: 1000})
	t.Cleanup(rt.Close)
	if err := rt.Compile("t", `local a = {}
for i = 1, 512 do a[i] = 65 end
local s = string.char(unpack(a))
if #s ~= 512 then error("wrong length: " .. #s) end`); err != nil {
		t.Fatal(err)
	}
	if err := rt.LoadGlobals("t"); err != nil {
		t.Fatal(err)
	}
	// The 512-argument call must have billed ~512 bytes; nothing else in the chunk charges.
	if got := rt.L.StringBytesCharged(); got < 512 {
		t.Fatalf("string.char(512 args) charged %d bytes total; it allocates one byte per argument and must "+
			"charge for them, or a loop through it is an uncharged allocator", got)
	}
}

// TestAllocAbortClassifiedAsItsOwnKind. Before #438 the memory bomb allocated for the whole deadline and then
// tripped it, so it was classified AbortDeadline — weight 0.1, the "probably transient host load, do not
// punish the script" bucket. It is neither transient nor the host's fault: it reproduces exactly, every run,
// from the script's own operands.
func TestAllocAbortClassifiedAsItsOwnKind(t *testing.T) {
	err := runPatient(t, `local s = string.rep("x", 1048576)
for i = 1, 40 do s = s .. s end`)
	if err == nil {
		t.Fatal("the bomb completed")
	}
	if !strings.Contains(err.Error(), "string allocation budget exceeded") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := ClassifyError(err); got != AbortAlloc {
		t.Fatalf("ClassifyError = %v, want AbortAlloc. Falling through to AbortDeadline weights the single "+
			"most dangerous thing a script can do at 0.1, and to AbortLogic weights it at 1.0 — neither is the "+
			"deliberate choice", got)
	}
}

// TestAllocAbortCostsTheBreakerLikeABudgetAbort. The classification only matters because of what the breaker
// does with it, and BOTH breaker switches have no default case — so a kind nobody wired costs ZERO and a
// script bombing memory on every call would never be quarantined.
func TestAllocAbortCostsTheBreakerLikeABudgetAbort(t *testing.T) {
	alloc, budget := newBreaker(), newBreaker()
	for i := 0; i < 4; i++ {
		alloc.record(nil, "s", "t", AbortAlloc)
		budget.record(nil, "s", "t", AbortBudget)
	}
	got, want := alloc.states["s"].budget, budget.states["s"].budget
	if got == 0 {
		t.Fatal("four allocation aborts cost the breaker NOTHING. Both breaker switches lack a default case, " +
			"so an unwired abort kind is free and a script that bombs memory every call is never quarantined")
	}
	if got != want {
		t.Fatalf("an allocation abort costs %v where an instruction abort costs %v; they are both "+
			"deterministic and content-pathological and should weigh the same", got, want)
	}
}

// TestLegitimateStringBuildingIsUnaffected is the false-positive net, and the reason the cap is 8 MiB rather
// than something tighter. A guard that quarantines correct content is worse than the problem it solves.
func TestLegitimateStringBuildingIsUnaffected(t *testing.T) {
	// A generous room description assembled the way content actually does it, 200 times over in one call.
	src := `local out = {}
for i = 1, 200 do
  local line = string.format("%s stands here, %s.", "A tall guard", "watching the gate")
  out[#out+1] = line .. " " .. string.rep("-", 60)
end
local s = table.concat(out, "\n")
if #s < 1000 then error("fixture built nothing: " .. #s) end`
	if err := runPatient(t, src); err != nil {
		t.Fatalf("ordinary content string-building tripped the allocation budget, which would quarantine "+
			"correct content: %v", err)
	}
}

// TestAllocBudgetResetsPerCall. If the tally is not zeroed between entry-point calls the budget becomes a
// lifetime quota, and a script charging a modest amount each call dies on its Nth invocation for no reason
// its author can see. The fork resets it inside ResetInstructionCount; this pins that the ENGINE's per-call
// chokepoint actually calls that.
func TestAllocBudgetResetsPerCall(t *testing.T) {
	L := New(Opts{})
	defer L.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	L.SetContext(ctx)

	// 1 MiB per call against an 8 MiB budget: fine forever if the tally resets, dead by call 9 if not.
	const perCall = `local s = string.rep("q", 1048576)
if #s ~= 1048576 then error("short") end`
	for i := 0; i < 50; i++ {
		L.ResetInstructionCount()
		if err := L.DoString(perCall); err != nil {
			t.Fatalf("legitimate content aborted on call %d of 50: %v. The tally is carrying across calls, so "+
				"the per-call budget behaves as a lifetime quota", i+1, err)
		}
	}
}

// TestAllocBudgetDoesNotAffectAnUnarmedState. The fork's contract, relied on by every bare LState in the
// tree: a VM that never opts in behaves exactly like stock gopher-lua.
func TestAllocBudgetDoesNotAffectAnUnarmedState(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	if err := L.DoString(`local s = string.rep("x", 1024)
for i = 1, 10 do s = s .. s end
if #s ~= 1048576 then error("wrong length") end`); err != nil {
		t.Fatalf("a bare LState that never armed the budget was constrained by it: %v", err)
	}
}
