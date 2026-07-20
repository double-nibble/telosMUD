package world

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/luasandbox"
)

// lualog_bound_test.go — #456. Builder-authored Lua logging (print / mud.log) is LENGTH-capped,
// RATE-limited per call (over the cap the invocation aborts and feeds the breaker), and LABELLED
// source=builder_lua so ops can route it apart from engine logs.

// captureRuntimeLog swaps the runtime's logger for a Debug-level text handler over buf.
func captureRuntimeLog(rt *luaRuntime) *bytes.Buffer {
	var buf bytes.Buffer
	rt.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

// TestLuaLogLengthCapped: a builder message far over the cap is truncated in the log, not emitted
// whole. Guards the disk-fill-by-one-huge-line vector.
func TestLuaLogLengthCapped(t *testing.T) {
	z := newZone("logcap")
	rt := z.lua
	buf := captureRuntimeLog(rt)

	// A ~4KB message via mud.log; string.rep is itself capped but well above our log cap.
	ch, err := rt.compileChunk("formula:big", `mud.log("info", string.rep("A", 4000))`)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.invoke(ch, &luaInvocation{}, nil); err != nil {
		t.Fatalf("invoke: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected the over-cap message to be truncated, got:\n%s", out)
	}
	// The 'A's actually logged must be bounded near the cap, not the full 4000.
	if n := strings.Count(out, "A"); n > luasandbox.MaxLogMsgBytes+8 {
		t.Errorf("logged %d 'A's; message was not length-capped to ~%d", n, luasandbox.MaxLogMsgBytes)
	}
}

// TestLuaLogLabelled: builder logs carry source=builder_lua at every level so they are filterable.
func TestLuaLogLabelled(t *testing.T) {
	z := newZone("loglabel")
	rt := z.lua
	buf := captureRuntimeLog(rt)

	ch, err := rt.compileChunk("formula:label", `mud.log("info", "hello"); print("world")`)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.invoke(ch, &luaInvocation{}, nil); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	out := buf.String()
	if strings.Count(out, "source=builder_lua") < 2 {
		t.Errorf("both mud.log and print must be labelled source=builder_lua, got:\n%s", out)
	}
}

// TestLuaLogFloodTripsBreaker is the acceptance test: a script that logs in a tight loop past the
// per-call cap ABORTS that call (bounded output) and, repeated, TRIPS the breaker — it is
// quarantined, not left running unbounded. The zone keeps serving.
func TestLuaLogFloodTripsBreaker(t *testing.T) {
	z := newZone("logflood")
	rt := z.lua
	_ = captureRuntimeLog(rt) // silence the flood in test output

	// 250 log calls in one invocation — above the per-call cap (200) but far below any instruction
	// budget, so ONLY the log rate limit can abort this (not the instruction/deadline budgets). The
	// (cap+1)th call aborts. Origin "formula:*" => a shared breaker key, matching the breaker tests.
	src := `for i = 1, 250 do mud.log("info", "spam") end`
	ch, err := rt.compileChunk("formula:flood", src)
	if err != nil {
		t.Fatal(err)
	}
	inv := &luaInvocation{}
	key := breakerKeyShared("formula:flood")

	// First single call must ERROR (the flood abort), not run to completion.
	if err := rt.invoke(ch, inv, nil); err == nil {
		t.Fatal("a 250-line logging loop must abort at the per-call log cap, not complete")
	}

	tripped := false
	for i := 0; i < 50; i++ {
		_ = rt.invoke(ch, inv, nil)
		if rt.breakerDisabled(key) {
			tripped = true
			break
		}
	}
	if !tripped {
		t.Fatal("a script flooding the log every call must trip the breaker")
	}

	// The zone still serves a healthy script.
	good, _ := rt.compileChunk("formula:ok", `return 7`)
	if v, ok := rt.invokeForNumber(good, inv, nil); !ok || v != 7 {
		t.Fatalf("a healthy script was affected by the flooder's breaker: (%v,%v)", v, ok)
	}
}

// TestLuaLogBudgetResetsPerCall: the per-call log budget resets between calls — a script that logs
// just under the cap every call runs indefinitely without tripping (only a SUSTAINED over-cap flood
// trips). Proves the counter is per-call, not lifetime.
func TestLuaLogBudgetResetsPerCall(t *testing.T) {
	z := newZone("logreset")
	rt := z.lua
	_ = captureRuntimeLog(rt)

	// Just under the per-call cap each call.
	src := `for i = 1, 50 do mud.log("info", "ok") end`
	ch, err := rt.compileChunk("formula:under", src)
	if err != nil {
		t.Fatal(err)
	}
	inv := &luaInvocation{}
	key := breakerKeyShared("formula:under")
	for i := 0; i < 30; i++ {
		if err := rt.invoke(ch, inv, nil); err != nil {
			t.Fatalf("an under-cap logging call must not error (call %d): %v", i, err)
		}
	}
	if rt.breakerDisabled(key) {
		t.Fatal("an under-cap logger tripped the breaker; the budget is not resetting per call")
	}
}
