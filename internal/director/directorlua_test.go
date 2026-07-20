package director

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// directorlua_test.go — the content-defined world-director script (#47). Most tests drive the script through
// a REAL scoped bus (the same harness signals_test.go uses) so on_signal runs on the director actor goroutine
// (single-writer), exercising the full signal-up → Lua → state-write + broadcast-down path.

const testWorldScript = `
function on_signal(event, payload)
  if event == "boss_slain" then
    local boss = "unknown"
    if type(payload) == "table" and payload.boss then boss = tostring(payload.boss) end
    director.set("last_boss_slain", boss)
    director.broadcast("world_announce", { text = "The " .. boss .. " has fallen!" })
  elseif event == "echo" then
    director.set("echo_out", payload)          -- round-trip a structured payload through the bridge
  elseif event == "probe_sandbox" then
    if os == nil and io == nil and load == nil and require == nil then
      director.set("sandboxed", "yes")
    end
  elseif event == "try_reserved" then
    -- attempt to clobber the engine's scheduler state; guarded (pcall catches the raise)
    local ok = pcall(function() director.set("schedule:evil", "hijacked") end)
    director.set("reserved_blocked", not ok)
  elseif event == "boss.died" then
    director.set("saw_boss_died", "yes") -- MUST NOT run: the scheduler intercepts boss.died before Lua
  elseif event == "boom" then
    error("intentional runtime error")   -- MUST be isolated: the director loop survives + keeps draining
  elseif event == "try_broadcast_reserved" then
    local ok = pcall(function() director.broadcast("scope.state.set", { key = "x", value = 1 }) end)
    director.set("broadcast_blocked", not ok) -- the reserved-event guard must have refused it
  end
end`

// TestWorldScriptCannotBroadcastReservedEvents: director.broadcast REFUSES an engine-reserved down-event
// (scope.state.set), so a script cannot forge a state-set that bypasses the director's single-writer CAS.
func TestWorldScriptCannotBroadcastReservedEvents(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	d := New("", newMemStore(), slog.Default()).
		WithScopeBus(dirBus, "world-director-1").
		WithWorldScript(testWorldScript).
		WithTick(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "try_broadcast_reserved", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// The script's pcall over director.broadcast("scope.state.set", ...) must have FAILED (the guard raised),
	// recorded as broadcast_blocked==true — so no forged single-writer-bypassing state-set was ever emitted.
	waitFor(t, "reserved broadcast refused", func() bool {
		raw, found, _ := d.Get(ctx, "broadcast_blocked")
		return found && string(raw) == "true"
	})
}

// TestWorldScriptRuntimeErrorIsIsolated: a runtime error in on_signal is caught, the event is still drained,
// and the director keeps processing the NEXT signal — a broken script must not wedge the loop.
func TestWorldScriptRuntimeErrorIsIsolated(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	d := New("", newMemStore(), slog.Default()).
		WithScopeBus(dirBus, "world-director-1").
		WithWorldScript(testWorldScript).
		WithTick(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// A signal whose on_signal errors, immediately followed by a healthy one.
	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "boom", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "boss_slain", json.RawMessage(`{"boss":"survivor"}`)); err != nil {
		t.Fatal(err)
	}
	// The healthy signal AFTER the error still processes => the loop survived + kept draining.
	waitFor(t, "director survives a script error and drains the next signal", func() bool {
		raw, found, _ := d.Get(ctx, "last_boss_slain")
		return found && string(raw) == `"survivor"`
	})
}

// TestWorldScriptCannotWriteReservedKeys: director.set REFUSES an engine-reserved scope key (schedule:*), so
// a content script cannot corrupt the spawn scheduler's persisted state. The refusal is a catchable Lua error.
func TestWorldScriptCannotWriteReservedKeys(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	d := New("", newMemStore(), slog.Default()).
		WithScopeBus(dirBus, "world-director-1").
		WithWorldScript(testWorldScript).
		WithTick(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "try_reserved", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reserved write refused", func() bool {
		raw, found, _ := d.Get(ctx, "reserved_blocked")
		return found && string(raw) == "true"
	})
	// The reserved key must NOT have been written.
	if raw, found, _ := d.Get(ctx, "schedule:evil"); found {
		t.Fatalf("a content script wrote a reserved key: schedule:evil = %q", raw)
	}
}

// TestWorldScriptReactsToSignal: a zone signals boss_slain UP; the Lua world script records the fallen boss
// in scope state (a DERIVED write) and broadcasts a world_announce DOWN. End-to-end through the durable bus.
func TestWorldScriptReactsToSignal(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	d := New("", newMemStore(), slog.Default()).
		WithScopeBus(dirBus, "world-director-1").
		WithWorldScript(testWorldScript).
		WithTick(time.Hour)

	var mu sync.Mutex
	var announce string
	sub, err := dirBus.Subscribe(scopebus.World(), func(event string, payload json.RawMessage, _ string) {
		if event != "world_announce" {
			return
		}
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(payload, &p)
		mu.Lock()
		announce = p.Text
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "boss_slain", json.RawMessage(`{"boss":"vurgoth"}`)); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "world_announce broadcast down", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return announce == "The vurgoth has fallen!"
	})
	if raw, found, _ := d.Get(ctx, "last_boss_slain"); !found || string(raw) != `"vurgoth"` {
		t.Fatalf("last_boss_slain persisted = %q found=%v, want \"vurgoth\"", raw, found)
	}
}

// TestWorldScriptPayloadRoundTrip: a structured JSON payload survives the JSON->Lua->JSON bridge intact.
func TestWorldScriptPayloadRoundTrip(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	d := New("", newMemStore(), slog.Default()).
		WithScopeBus(dirBus, "world-director-1").
		WithWorldScript(testWorldScript).
		WithTick(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	in := `{"nested":{"n":42,"list":[1,2,3]},"name":"grove","on":true}`
	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "echo", json.RawMessage(in)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "echo_out persisted", func() bool {
		raw, found, _ := d.Get(ctx, "echo_out")
		return found && len(raw) > 0
	})
	raw, _, _ := d.Get(ctx, "echo_out")
	var got, want map[string]any
	_ = json.Unmarshal(raw, &got)
	_ = json.Unmarshal([]byte(in), &want)
	require := func(cond bool, msg string) {
		if !cond {
			t.Fatalf("%s: got %s", msg, raw)
		}
	}
	require(len(got) == 3, "top-level keys")
	require(got["name"] == "grove", "string field")
	require(got["on"] == true, "bool field")
	nested, _ := got["nested"].(map[string]any)
	require(nested != nil && nested["n"] == float64(42), "nested number")
	list, _ := nested["list"].([]any)
	require(len(list) == 3 && list[0] == float64(1), "nested array")
}

// TestWorldScriptIsSandboxed: the director VM has NO os/io/load/require — a director script cannot reach the
// host filesystem/process from inside the telos-director service.
func TestWorldScriptIsSandboxed(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	d := New("", newMemStore(), slog.Default()).
		WithScopeBus(dirBus, "world-director-1").
		WithWorldScript(testWorldScript).
		WithTick(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "probe_sandbox", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sandbox probe recorded", func() bool {
		raw, found, _ := d.Get(ctx, "sandboxed")
		return found && string(raw) == `"yes"`
	})
}

// TestWorldScriptComposesUnderSchedules: WithWorldScript is wired BEFORE WithSchedules, so the scheduler
// wrapper is OUTERMOST and DELEGATES a non-reserved event (boss_slain) to the Lua handler. This proves the
// composition order (a Lua handler never shadows the scheduler's reserved boss.died, and the scheduler never
// swallows a content event).
func TestWorldScriptComposesUnderSchedules(t *testing.T) {
	mb := commbus.NewMemBus()
	js := commbus.NewMemJetStream()
	dirBus := scopebus.New(mb).WithDurable(js, "world-director-1")
	zoneBus := scopebus.New(mb).WithDurable(js, "shard-1")

	d := New("", newMemStore(), slog.Default()).
		WithScopeBus(dirBus, "world-director-1").
		WithWorldScript(testWorldScript). // set BEFORE schedules
		WithSchedules(nil).               // scheduler wraps the Lua handler as its prev
		WithTick(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// Fire the RESERVED boss.died FIRST, then a non-reserved boss_slain. Per-source ordering (MaxAckPending=1)
	// means boss.died is fully processed before boss_slain's result appears.
	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "boss.died", json.RawMessage(`{"ref":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := zoneBus.SignalDurable(ctx, scopebus.World(), "boss_slain", json.RawMessage(`{"boss":"drake"}`)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "lua handler reached through the scheduler wrapper", func() bool {
		raw, found, _ := d.Get(ctx, "last_boss_slain")
		return found && string(raw) == `"drake"`
	})
	// boss.died is a RESERVED event the scheduler intercepts — it must NOT have reached the Lua handler.
	if _, found, _ := d.Get(ctx, "saw_boss_died"); found {
		t.Fatal("boss.died reached the Lua handler — the scheduler wrapper must intercept it (composition order broken)")
	}
}

// TestWorldScriptCompileErrorIsGraceful: a syntactically broken world script does NOT wire a handler (nor
// crash) — the director boots and drains signals with no orchestration.
func TestWorldScriptCompileErrorIsGraceful(t *testing.T) {
	d := New("", newMemStore(), slog.Default()).WithWorldScript("this is not lua ===")
	if d.handler != nil {
		t.Fatal("a compile error should leave no signal handler wired")
	}
}

// TestEmptyWorldScriptIsNoop: an empty script wires nothing.
func TestEmptyWorldScriptIsNoop(t *testing.T) {
	d := New("", newMemStore(), slog.Default()).WithWorldScript("")
	if d.handler != nil {
		t.Fatal("an empty world script should wire no handler")
	}
}

// TestNewLuaDirectorCompileError: the constructor surfaces a compile error (used by WithWorldScript).
func TestNewLuaDirectorCompileError(t *testing.T) {
	ld, err := newLuaDirector(slog.Default(), worldScriptKey, "function on_signal( -- unclosed")
	if err == nil {
		ld.close()
		t.Fatal("expected a compile error")
	}
}

// TestLuaToJSONDepthCapRejectsCycle: a self-referential Lua table is rejected by the bounded encoder rather
// than recursing unbounded (the Go-side encode is not covered by the VM instruction budget).
func TestLuaToJSONDepthCapRejectsCycle(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	t1 := L.NewTable()
	t1.RawSetString("self", t1) // cycle
	if _, err := luaToJSON(t1); err == nil {
		t.Fatal("a cyclic table must be rejected by the depth cap, not recurse unbounded")
	}
}

// TestJSONToLuaHandlesScalarsAndContainers: the decode covers each JSON kind.
func TestJSONToLuaHandlesScalarsAndContainers(t *testing.T) {
	L := lua.NewState()
	defer L.Close()
	v := jsonToLua(L, json.RawMessage(`{"s":"x","n":5,"b":true,"a":[10,20]}`))
	tbl, ok := v.(*lua.LTable)
	if !ok {
		t.Fatalf("want a table, got %T", v)
	}
	if s := tbl.RawGetString("s"); s.String() != "x" {
		t.Errorf("s = %v", s)
	}
	if n, ok := tbl.RawGetString("n").(lua.LNumber); !ok || float64(n) != 5 {
		t.Errorf("n = %v", tbl.RawGetString("n"))
	}
	if b, ok := tbl.RawGetString("b").(lua.LBool); !ok || !bool(b) {
		t.Errorf("b = %v", tbl.RawGetString("b"))
	}
	arr, ok := tbl.RawGetString("a").(*lua.LTable)
	if !ok || arr.Len() != 2 || float64(arr.RawGetInt(1).(lua.LNumber)) != 10 {
		t.Errorf("a = %v", tbl.RawGetString("a"))
	}
	// An empty/garbage payload decodes to nil, not an error.
	if got := jsonToLua(L, nil); got != lua.LNil {
		t.Errorf("nil payload = %v, want LNil", got)
	}
}
