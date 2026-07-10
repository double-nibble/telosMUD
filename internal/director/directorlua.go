package director

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	lua "github.com/yuin/gopher-lua"

	"github.com/double-nibble/telosmud/internal/luasandbox"
	"github.com/double-nibble/telosmud/internal/scopebus"
)

// maxScopeValueBytes caps the JSON size a single director.set may persist to scope state — defense-in-depth
// on what a content script can write to Postgres (the value is otherwise bounded only indirectly by the VM
// instruction budget). 64 KiB is far past any legitimate world-state value.
const maxScopeValueBytes = 64 * 1024

// reservedDownEvent reports whether an event name is ENGINE-RESERVED on the scope bus, so director.broadcast
// cannot spoof it. This is the fail-closed guard closing the trust-boundary widening (#47 security F3): the
// director signal handler is now CONTENT, so a script must not be able to forge a reserved down-event —
// especially scope.state.set (which a zone read-replica applies as a state delta, BYPASSING the director's
// single-writer CAS) or a content.* pull/reload status line. Reserved:
//   - scopebus.EventStateSet — the authoritative-state-set channel (single-writer bypass);
//   - the content.* namespace — contentbus pull/reload result + audit events;
//   - the director's own boss schedule signals.
func reservedDownEvent(event string) bool {
	switch event {
	case scopebus.EventStateSet, SpawnBossEvent, BossDiedEvent:
		return true
	}
	return strings.HasPrefix(event, "content.")
}

// reservedScopeKeyPrefixes are the world-scope-state key prefixes the ENGINE owns (the Go director's own
// persisted state — e.g. the spawn scheduler's per-boss next-spawn record, schedule_run.go scheduleKey).
// A content world_script may use any OTHER key freely, but director.set REFUSES a reserved-prefixed key so a
// script cannot corrupt engine state (integrity, defense-in-depth even for trusted content). Reads are
// allowed (harmless). Extend this list when a new engine feature persists director scope state under a prefix.
var reservedScopeKeyPrefixes = []string{"schedule:"}

// reservedScopeKey reports whether key is owned by the engine (director.set must refuse it).
func reservedScopeKey(key string) bool {
	for _, p := range reservedScopeKeyPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// directorlua.go — the content-defined WORLD-DIRECTOR script (#47, Phase 10.4). A sandboxed Lua VM (the
// shared internal/luasandbox core) runs the pack's world_script, which defines `on_signal(event, payload)`.
// The director invokes it once per signal-up event ON the director actor goroutine (single-writer), with the
// event payload decoded to a Lua value and a `director` host table (get/set/broadcast/log) bound to the
// director's API for the duration of the call.
//
// GOLDEN RULE (docs/WORLD-EVENTS.md): the script never reaches into a zone. director.set writes the
// director's authoritative scope state (a CAS) and broadcasts the change DOWN; director.broadcast emits a
// transient remote effect DOWN — each member zone applies the consequence locally on its own goroutine.
//
// IDEMPOTENCY: signal-up is durable/at-least-once and a crash between handler-run and ack replays an event
// once, so a script's on_signal must be idempotent — write DERIVED values (director.set("last_boss", p.boss)),
// never a blind read-modify-write. This is a content contract (see the WorldScript DTO doc); the runtime does
// not add a dedup layer in v1.

const worldScriptKey = "world_script"

// luaJSONMaxDepth bounds the recursion of the JSON<->Lua bridge. The instruction budget does NOT cover the
// Go-side encode/decode, so a deeply-nested or CYCLIC Lua table (director.set) is bounded HERE — past the cap
// the encode errors cleanly rather than recursing unbounded.
const luaJSONMaxDepth = 16

// luaDirector is the sandboxed world-director signal handler. Single-goroutine: the director actor loop is
// the only caller, so the VM and the api handle need no lock.
type luaDirector struct {
	rt  *luasandbox.Runtime
	log *slog.Logger
	// api is the director-API handle valid ONLY during an OnSignal call — the `director` table's functions
	// read it. nil outside a call (the host functions guard against a stray call).
	api *API
}

// newLuaDirector compiles the world script, installs the `director` host table, and runs the script's top
// level so its `on_signal` definition lands in the sandbox globals. A compile/load error is returned so the
// caller can decide (the director logs it and runs without orchestration rather than crashing the tier).
func newLuaDirector(log *slog.Logger, script string) (*luaDirector, error) {
	if log == nil {
		log = slog.Default()
	}
	ld := &luaDirector{log: log.With("subsystem", "director-lua")}
	ld.rt = luasandbox.NewRuntime(ld.log, luasandbox.Opts{})
	ld.installDirectorTable()
	if err := ld.rt.Compile(worldScriptKey, script); err != nil {
		ld.rt.Close()
		return nil, err
	}
	if err := ld.rt.LoadGlobals(worldScriptKey); err != nil {
		ld.rt.Close()
		return nil, fmt.Errorf("world_script load: %w", err)
	}
	return ld, nil
}

// close tears down the VM.
func (ld *luaDirector) close() {
	if ld != nil && ld.rt != nil {
		ld.rt.Close()
	}
}

// WithWorldScript wires the content-defined world-director script (#47) onto the director: it compiles the
// pack's world_script into a sandboxed Lua VM and installs its on_signal as the director's signal handler.
// Call BEFORE WithSchedules so the scheduler's reserved-event (boss.died) handling composes OUTERMOST — the
// Lua handler becomes its `prev` and only sees non-reserved events. An empty script is a no-op; a COMPILE
// ERROR is logged and the director runs WITHOUT orchestration (a broken script must never prevent the tier
// from booting). Not safe to call after Run.
func (d *Director) WithWorldScript(script string) *Director {
	if script == "" {
		return d
	}
	ld, err := newLuaDirector(d.log, script)
	if err != nil {
		d.log.Error("world_script compile failed; director runs without orchestration (#47)", "err", err)
		return d
	}
	d.log.Info("world_script loaded; director orchestration active (#47)")
	return d.WithSignalHandler(ld.OnSignal)
}

// OnSignal is the director.SignalHandler: it invokes the script's on_signal(event, payload) with the payload
// decoded to a Lua value. Runs on the director goroutine; the api handle is valid only for this call. A
// script error (or a tripped breaker) is logged, never propagated — a broken world script must not wedge the
// director loop or block signal acks.
func (ld *luaDirector) OnSignal(api *API, event string, payload json.RawMessage) {
	ld.api = api
	defer func() { ld.api = nil }()
	_, err := ld.rt.CallGlobal(worldScriptKey, "on_signal", 0, func(L *lua.LState) int {
		L.Push(lua.LString(event))
		L.Push(jsonToLua(L, payload))
		return 2
	})
	if err != nil {
		ld.log.Warn("world_script on_signal failed", "event", event, "err", err)
	}
}

// installDirectorTable binds the read-only `director` host table (get/set/broadcast/log) into the sandbox.
func (ld *luaDirector) installDirectorTable() {
	L := ld.rt.L
	t := L.NewTable()
	L.SetFuncs(t, map[string]lua.LGFunction{
		"get":       ld.luaGet,
		"set":       ld.luaSet,
		"broadcast": ld.luaBroadcast,
		"log":       ld.luaLog,
	})
	// Read-only so a script cannot clobber director.set with its own function.
	L.SetGlobal("director", luasandbox.ReadOnly(L, t))
}

// luaGet: director.get(key) -> the scope value decoded to a Lua value, or nil when unset.
func (ld *luaDirector) luaGet(L *lua.LState) int {
	if ld.api == nil {
		L.Push(lua.LNil)
		return 1
	}
	key := L.CheckString(1)
	raw, ok := ld.api.Get(key)
	if !ok {
		L.Push(lua.LNil)
		return 1
	}
	L.Push(jsonToLua(L, raw))
	return 1
}

// luaSet: director.set(key, value) — write key=value to the director's scope (CAS + broadcast down). A CAS
// loss (a failover race) raises a Lua error the script's pcall may catch; unhandled, it aborts on_signal and
// the outcome is logged (the director reconciles its state on reload).
func (ld *luaDirector) luaSet(L *lua.LState) int {
	if ld.api == nil {
		L.RaiseError("director.set called outside a signal handler")
		return 0
	}
	key := L.CheckString(1)
	if reservedScopeKey(key) {
		L.RaiseError("director.set: key %q is reserved by the engine and cannot be written from content", key)
		return 0
	}
	raw, err := luaToJSON(L.CheckAny(2))
	if err != nil {
		L.RaiseError("director.set(%q): %v", key, err)
		return 0
	}
	if len(raw) > maxScopeValueBytes {
		L.RaiseError("director.set(%q): value too large (%d bytes, cap %d)", key, len(raw), maxScopeValueBytes)
		return 0
	}
	if err := ld.api.Set(key, raw); err != nil {
		L.RaiseError("director.set(%q): %v", key, err)
		return 0
	}
	return 0
}

// luaBroadcast: director.broadcast(event[, payload]) — emit a transient remote effect DOWN to member zones.
func (ld *luaDirector) luaBroadcast(L *lua.LState) int {
	if ld.api == nil {
		L.RaiseError("director.broadcast called outside a signal handler")
		return 0
	}
	event := L.CheckString(1)
	if reservedDownEvent(event) {
		L.RaiseError("director.broadcast: event %q is engine-reserved and cannot be emitted from content", event)
		return 0
	}
	raw := json.RawMessage("null")
	if L.GetTop() >= 2 && L.Get(2) != lua.LNil {
		r, err := luaToJSON(L.Get(2))
		if err != nil {
			L.RaiseError("director.broadcast(%q): %v", event, err)
			return 0
		}
		raw = r
	}
	ld.api.Broadcast(event, raw)
	return 0
}

// luaLog: director.log(msg) — a structured info log (the director's print-with-context).
func (ld *luaDirector) luaLog(L *lua.LState) int {
	ld.log.Info("world_script", "msg", L.CheckString(1))
	return 0
}

// --- JSON <-> Lua bridge (bounded) ----------------------------------------------------------------

// jsonToLua decodes a JSON payload into a Lua value (nil/bool/number/string/table). A malformed or empty
// payload decodes to nil (a script sees a nil payload, not an error).
func jsonToLua(L *lua.LState, raw json.RawMessage) lua.LValue {
	if len(raw) == 0 {
		return lua.LNil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return lua.LNil
	}
	return goToLua(L, v, 0)
}

func goToLua(L *lua.LState, v any, depth int) lua.LValue {
	if depth > luaJSONMaxDepth {
		return lua.LNil
	}
	switch vv := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(vv)
	case float64:
		return lua.LNumber(vv)
	case string:
		return lua.LString(vv)
	case []any:
		t := L.NewTable()
		for i, e := range vv {
			t.RawSetInt(i+1, goToLua(L, e, depth+1))
		}
		return t
	case map[string]any:
		t := L.NewTable()
		for k, e := range vv {
			t.RawSetString(k, goToLua(L, e, depth+1))
		}
		return t
	default:
		return lua.LNil
	}
}

// luaToJSON encodes a Lua value to a JSON payload. Depth-bounded so a cyclic or pathologically-nested table
// errors cleanly (the Go-side recursion is not covered by the VM instruction budget).
func luaToJSON(v lua.LValue) (json.RawMessage, error) {
	g, err := luaToGo(v, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(g)
}

func luaToGo(v lua.LValue, depth int) (any, error) {
	if depth > luaJSONMaxDepth {
		return nil, fmt.Errorf("value too deeply nested (max depth %d) — a cyclic table?", luaJSONMaxDepth)
	}
	switch vv := v.(type) {
	case *lua.LNilType:
		return nil, nil
	case lua.LBool:
		return bool(vv), nil
	case lua.LNumber:
		return float64(vv), nil
	case lua.LString:
		return string(vv), nil
	case *lua.LTable:
		return luaTableToGo(vv, depth)
	default:
		return nil, fmt.Errorf("cannot encode Lua %s to JSON", v.Type().String())
	}
}

// luaTableToGo encodes a Lua table as a JSON array when it is a pure 1..n sequence, else as a JSON object
// (string-ifying numeric keys). Bounded by depth via luaToGo.
func luaTableToGo(t *lua.LTable, depth int) (any, error) {
	n := t.Len()
	count := 0
	pureArray := true
	t.ForEach(func(k, _ lua.LValue) {
		count++
		if ik, ok := k.(lua.LNumber); ok {
			f := float64(ik)
			if f == float64(int(f)) && int(f) >= 1 && int(f) <= n {
				return
			}
		}
		pureArray = false
	})
	if pureArray && count == n && n > 0 {
		arr := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			e, err := luaToGo(t.RawGetInt(i), depth+1)
			if err != nil {
				return nil, err
			}
			arr = append(arr, e)
		}
		return arr, nil
	}
	obj := map[string]any{}
	var iterErr error
	t.ForEach(func(k, val lua.LValue) {
		if iterErr != nil {
			return
		}
		var key string
		switch kk := k.(type) {
		case lua.LString:
			key = string(kk)
		case lua.LNumber:
			key = strconv.FormatFloat(float64(kk), 'f', -1, 64)
		default:
			iterErr = fmt.Errorf("unsupported table key type %s", k.Type().String())
			return
		}
		g, err := luaToGo(val, depth+1)
		if err != nil {
			iterErr = err
			return
		}
		obj[key] = g
	})
	if iterErr != nil {
		return nil, iterErr
	}
	return obj, nil
}
