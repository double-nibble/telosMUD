package world

import (
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// luaentry_test.go — slice 7.4 invocation-CORE tests (docs/PHASE7-PLAN.md §1.1/§4): the
// machinery every entry point uses, exercising the THREE mandatory invariants directly so they
// are proven at the foundation before any lifecycle wiring depends on them. The per-entry-point
// lifecycle tests (greeter trigger, on_resolve, custom command, formula, bus handler) land with
// each entry-point unit (the split below).

// --- invariant 3: fail-closed compile ------------------------------------------------------

// TestCompileChunkFailClosed asserts a syntactically-broken body compiles to (nil, error) — the
// caller keeps the def inert (the bare-engine invariant: no-Lua/broken-Lua content still boots).
// An empty body is (nil, nil) — the common "no Lua column" no-op.
func TestCompileChunkFailClosed(t *testing.T) {
	z := newZone("entry")
	rt := z.lua
	if ch, err := rt.compileChunk("t:broken", `this is not ) valid lua (`); err == nil || ch != nil {
		t.Fatalf("a broken body must compile to (nil, error), got (%v, %v)", ch, err)
	}
	if ch, err := rt.compileChunk("t:empty", "   "); err != nil || ch != nil {
		t.Fatalf("an empty body must be (nil, nil), got (%v, %v)", ch, err)
	}
	good, err := rt.compileChunk("t:good", `return 1`)
	if err != nil || good == nil {
		t.Fatalf("a valid body must compile, got (%v, %v)", good, err)
	}
}

// --- invariant 2: per-call fresh _ENV ------------------------------------------------------

// TestInvokeFreshEnvNoLeak asserts a global written by one invocation does NOT leak into the
// next (each call gets a fresh _ENV), and that `self` is per-call (not a stale shared global).
func TestInvokeFreshEnvNoLeak(t *testing.T) {
	z := newZone("entry")
	rt := z.lua
	room := z.newEntity("entry:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["entry:room:hall"] = room
	a := z.newEntity("entry:mob:a")
	Add(a, &Living{})
	a.short = "alpha"
	Move(a, room)
	b := z.newEntity("entry:mob:b")
	Add(b, &Living{})
	b.short = "beta"
	Move(b, room)

	// Call 1 writes a global and reads its own self.
	writer, _ := rt.compileChunk("t:writer", `leaked = "from-call-1"; assert(self:name() == "alpha")`)
	if err := rt.invoke(writer, &luaInvocation{actor: a}, map[string]lua.LValue{"self": rt.newHandle(a)}); err != nil {
		t.Fatalf("call 1 errored: %v", err)
	}
	// Call 2 must NOT see call 1's global, and sees its OWN self (beta).
	reader, _ := rt.compileChunk("t:reader", `assert(leaked == nil, "a global leaked across calls"); assert(self:name() == "beta")`)
	if err := rt.invoke(reader, &luaInvocation{actor: b}, map[string]lua.LValue{"self": rt.newHandle(b)}); err != nil {
		t.Fatalf("call 2 saw leaked state or wrong self: %v", err)
	}
	// The sandbox globals are still intact (the leak did not poison the shared env).
	check, _ := rt.compileChunk("t:check", `assert(type(string) == "table"); assert(type(mud) == "table")`)
	if err := rt.invoke(check, &luaInvocation{actor: a}, nil); err != nil {
		t.Fatalf("sandbox globals were poisoned: %v", err)
	}
}

// TestInvokeFallsThroughToGlobals asserts the fresh per-call env still resolves the sandbox
// globals (string/math/mud) and the handle methods — the __index fall-through works.
func TestInvokeFallsThroughToGlobals(t *testing.T) {
	z := newZone("entry")
	rt := z.lua
	ch, _ := rt.compileChunk("t:ft", `
		assert(("x"):rep(3) == "xxx")
		assert(math.max(1, 2) == 2)
		assert(type(mud.now) == "function")
	`)
	if err := rt.invoke(ch, &luaInvocation{actor: nil}, nil); err != nil {
		t.Fatalf("fall-through to globals failed: %v", err)
	}
}

// --- invariant 1: the firing cascade's budget is threaded ----------------------------------

// TestInvokeFromCtxThreadsBudget asserts invokeFromCtx builds the invocation from the firing
// *effectCtx — the SAME eventBudget pointer + depth — so a Lua body fired in a cascade inherits
// (not resets) the shared budget. We capture rt.inv during the call and confirm it carries the
// firing ctx's budget pointer.
func TestInvokeFromCtxThreadsBudget(t *testing.T) {
	z := newZone("entry")
	rt := z.lua
	mob := z.newEntity("entry:mob:x")
	Add(mob, &Living{})

	budget := 9
	firing := &effectCtx{z: z, actor: mob, source: mob, depth: 4, eventBudget: &budget}

	// A Go function the script calls so we can read rt.inv mid-invocation.
	var sawDepth int
	var sawBudget *int
	rt.L.SetGlobal("__capture", rt.L.NewFunction(func(*lua.LState) int {
		sawDepth = rt.inv.depth
		sawBudget = rt.inv.eventBudget
		return 0
	}))

	ch, _ := rt.compileChunk("t:cap", `__capture()`)
	if err := rt.invokeFromCtx(ch, firing, mob, nil); err != nil {
		t.Fatal(err)
	}
	if sawDepth != 4 {
		t.Fatalf("invocation depth = %d, want 4 (threaded from the firing ctx, not reset)", sawDepth)
	}
	if sawBudget != &budget {
		t.Fatal("invocation eventBudget is not the SAME pointer as the firing ctx's (a Lua handler could escape the shared budget)")
	}
}

// --- invariant 3 + SECURITY: pvp_allowed fails CLOSED --------------------------------------

// TestPvpPolicyFailsClosed asserts the Lua pvp_allowed policy invocation is FAIL-CLOSED: a
// compile-absent policy, an ERRORING policy, and a non-true return all DENY harm; only an
// explicit Lua `true` permits. This is content deciding harm-consent — a broken policy must
// never default-allow.
func TestPvpPolicyFailsClosed(t *testing.T) {
	z := newZone("entry")
	rt := z.lua
	inv := &luaInvocation{}

	// No policy (nil chunk) => deny.
	if rt.invokeForBool(nil, inv, nil) {
		t.Fatal("a missing pvp policy must DENY (fail-closed), got allow")
	}
	// An erroring policy => deny.
	boom, _ := rt.compileChunk("t:boom", `error("policy bug")`)
	if rt.invokeForBool(boom, inv, nil) {
		t.Fatal("an erroring pvp policy must DENY (fail-closed), got allow")
	}
	// A policy returning nil/false/non-bool => deny.
	for _, src := range []string{`return nil`, `return false`, `return "yes"`, `return 0`} {
		ch, _ := rt.compileChunk("t:deny", src)
		if rt.invokeForBool(ch, inv, nil) {
			t.Fatalf("policy %q must DENY (only explicit true permits), got allow", src)
		}
	}
	// Only an explicit truthy return permits.
	yes, _ := rt.compileChunk("t:yes", `return true`)
	if !rt.invokeForBool(yes, inv, nil) {
		t.Fatal("an explicit `return true` policy must PERMIT, got deny")
	}
}

// TestFormulaFailsClosed asserts a Lua formula returns (n, true) for a numeric return and
// (0, false) for a missing/erroring/non-number body — the caller falls back to the engine
// default, never a silently-corrupt stat.
func TestFormulaFailsClosed(t *testing.T) {
	z := newZone("entry")
	rt := z.lua
	inv := &luaInvocation{}

	if _, ok := rt.invokeForNumber(nil, inv, nil); ok {
		t.Fatal("a missing formula must be (0, false)")
	}
	boom, _ := rt.compileChunk("t:boom", `error("formula bug")`)
	if _, ok := rt.invokeForNumber(boom, inv, nil); ok {
		t.Fatal("an erroring formula must be (0, false)")
	}
	str, _ := rt.compileChunk("t:str", `return "not-a-number"`)
	if _, ok := rt.invokeForNumber(str, inv, nil); ok {
		t.Fatal("a non-number formula return must be (0, false)")
	}
	num, _ := rt.compileChunk("t:num", `return 17.5`)
	if v, ok := rt.invokeForNumber(num, inv, nil); !ok || v != 17.5 {
		t.Fatalf("a numeric formula must return (17.5, true), got (%v, %v)", v, ok)
	}
}

// --- invariant 3: runtime error is isolated, zone serves on --------------------------------

// TestInvokeRuntimeErrorIsolated asserts a body that errors at runtime fizzles just that call
// (the error is returned + logged, not propagated) and the runtime keeps serving the next call.
func TestInvokeRuntimeErrorIsolated(t *testing.T) {
	z := newZone("entry")
	rt := z.lua
	boom, _ := rt.compileChunk("t:boom", `error("kaboom")`)
	if err := rt.invoke(boom, &luaInvocation{}, nil); err == nil {
		t.Fatal("an erroring body should return an error")
	}
	// The runtime keeps serving.
	ok, _ := rt.compileChunk("t:ok", `assert(1 + 1 == 2)`)
	if err := rt.invoke(ok, &luaInvocation{}, nil); err != nil {
		t.Fatalf("the runtime did not serve the next call after an error: %v", err)
	}
}
