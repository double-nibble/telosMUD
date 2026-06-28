package world

import (
	"encoding/json"
	"fmt"
	"math"

	lua "github.com/yuin/gopher-lua"
)

// luastate.go — the data-only Lua-table ↔ JSON marshaller for self.state persistence (slice 7.6,
// P7-D5, T10). This is the STATE-INJECTION trust boundary: self.state is mirrored to/from durable
// JSONB, so the marshaller must guarantee (1) only PLAIN DATA crosses (a function/closure/userdata/
// handle is REJECTED at save with a clean error naming the bad key — never a silent partial drop,
// never a persisted pointer), and (2) load reconstructs a PLAIN TABLE — it never executes code and
// never resurrects a handle (content stores h:id() and re-resolves). Caps bound the subtree so a
// runaway self.state can't balloon the snapshot or the VM.
//
// The intermediate form is plain Go (map[string]any / []any / float64 / string / bool), so the
// JSONB shape is ordinary json.Marshal output — no Lua type leaks into the persisted bytes.

const (
	// luaStateMaxBytes caps the marshalled JSON byte size of a self.state subtree (the snapshot-
	// balloon bound). 64 KiB is generous for any legitimate quest/counter state while keeping a
	// runaway state from bloating the character row / the save cadence.
	luaStateMaxBytes = 64 * 1024

	// luaStateMaxDepth caps nesting depth (a self.state table of tables of tables…). Bounds the
	// marshal recursion (stack) and a pathological deep structure. 16 is far past any sane state.
	luaStateMaxDepth = 16

	// luaStateMaxKeys caps the TOTAL number of keys/elements across the whole subtree (not per
	// table) — the structural-size bound a byte cap alone doesn't give early (many tiny keys).
	luaStateMaxKeys = 4096
)

// luaStateError is a clean, save-rejecting error from the marshaller. It NAMES the offending key
// path so an author sees exactly what to fix (a handle stored at state.greeted[3], an over-cap
// subtree). It is returned (never panicked) so a save fails loudly without crashing the zone.
type luaStateError struct{ msg string }

func (e luaStateError) Error() string { return e.msg }

// marshalLuaState converts a self.state Lua table to its JSON-able Go form and then to JSON bytes,
// enforcing the data-only allowlist + the caps (T10). It returns nil bytes for a nil/empty state
// (the no-script-state case — persisted as absent). A non-data value (function/userdata/handle) or
// an over-cap subtree is a clean luaStateError NAMING the path — the save rejects, it does NOT
// silently drop the rest. Single-writer: zone goroutine (called at dump time).
func marshalLuaState(t *lua.LTable) (json.RawMessage, error) {
	if t == nil {
		return nil, nil
	}
	keys := 0
	v, err := luaToGo(t, "state", 0, &keys)
	if err != nil {
		return nil, err
	}
	// An empty table marshals to {} or []; treat a state with no keys as absent (nil) so a
	// scriptless save carries no Script subtree (the backward-compat default).
	if keys == 0 {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, luaStateError{"self.state: not serializable: " + err.Error()}
	}
	if len(b) > luaStateMaxBytes {
		return nil, luaStateError{fmt.Sprintf("self.state too large: %d bytes (cap %d)", len(b), luaStateMaxBytes)}
	}
	return b, nil
}

// luaToGo converts one Lua value to its JSON-able Go form, recursively, enforcing the allowlist +
// depth/key caps. path is the dotted key path for error messages ("state.greeted[2]"); depth is the
// current nesting; keys is the running total key count (capped across the WHOLE subtree).
func luaToGo(v lua.LValue, path string, depth int, keys *int) (any, error) {
	switch val := v.(type) {
	case lua.LBool:
		return bool(val), nil
	case lua.LNumber:
		f := float64(val)
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, luaStateError{"self.state: non-finite number at " + path}
		}
		return f, nil
	case lua.LString:
		return string(val), nil
	case *lua.LTable:
		if depth >= luaStateMaxDepth {
			return nil, luaStateError{fmt.Sprintf("self.state too deep at %s (cap %d)", path, luaStateMaxDepth)}
		}
		return luaTableToGo(val, path, depth, keys)
	case *lua.LNilType:
		return nil, nil
	default:
		// A function, userdata (a HANDLE), thread, channel, or any non-data value: REJECT, naming
		// the path. Content must store h:id() and re-resolve — never a handle (T10).
		return nil, luaStateError{fmt.Sprintf("self.state: non-data value (%s) at %s — store a primitive (e.g. h:id()), not a handle/function", v.Type().String(), path)}
	}
}

// luaTableToGo converts a Lua table to either a []any (a 1..N integer-keyed ARRAY) or a
// map[string]any (a string-keyed MAP), recursively. A table with both array and map parts is
// treated as a map (every key stringified) so nothing is lost. Each key/element counts toward the
// global key cap.
func luaTableToGo(t *lua.LTable, path string, depth int, keys *int) (any, error) {
	// Detect a pure array (consecutive 1..N integer keys, no other keys).
	n := t.Len()
	isArray := n > 0
	if isArray {
		// Confirm there is no non-array key (ForEach over all keys; any non-1..N key => map).
		t.ForEach(func(k, _ lua.LValue) {
			if num, ok := k.(lua.LNumber); ok {
				i := float64(num)
				if i == math.Trunc(i) && i >= 1 && i <= float64(n) {
					return // a valid array index
				}
			}
			isArray = false
		})
	}

	if isArray {
		arr := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			*keys++
			if *keys > luaStateMaxKeys {
				return nil, luaStateError{fmt.Sprintf("self.state too many keys (cap %d)", luaStateMaxKeys)}
			}
			gv, err := luaToGo(t.RawGetInt(i), fmt.Sprintf("%s[%d]", path, i), depth+1, keys)
			if err != nil {
				return nil, err
			}
			arr = append(arr, gv)
		}
		return arr, nil
	}

	// Map form: stringify every key (a number key becomes its string form). A non-string/non-number
	// key (a table/bool/handle key) is rejected — keys must be plain.
	m := map[string]any{}
	var ferr error
	t.ForEach(func(k, val lua.LValue) {
		if ferr != nil {
			return
		}
		key, ok := luaKeyString(k)
		if !ok {
			ferr = luaStateError{fmt.Sprintf("self.state: non-data key (%s) in %s", k.Type().String(), path)}
			return
		}
		*keys++
		if *keys > luaStateMaxKeys {
			ferr = luaStateError{fmt.Sprintf("self.state too many keys (cap %d)", luaStateMaxKeys)}
			return
		}
		gv, err := luaToGo(val, path+"."+key, depth+1, keys)
		if err != nil {
			ferr = err
			return
		}
		m[key] = gv
	})
	if ferr != nil {
		return nil, ferr
	}
	return m, nil
}

// luaKeyString renders a table KEY as a string, accepting only plain string/number keys (a handle/
// function/table key is rejected). A number key is stringified (Lua's `5` and `"5"` collide on
// reload, but self.state is data; a content author keying by id stores a string id).
func luaKeyString(k lua.LValue) (string, bool) {
	switch key := k.(type) {
	case lua.LString:
		return string(key), true
	case lua.LNumber:
		f := float64(key)
		if f == math.Trunc(f) && !math.IsInf(f, 0) {
			return fmt.Sprintf("%d", int64(f)), true
		}
		return fmt.Sprintf("%g", f), true
	default:
		return "", false
	}
}

// unmarshalLuaState reconstructs a self.state Lua table from persisted JSON bytes, on the runtime's
// LState — a PLAIN table of numbers/strings/bools/nested tables ONLY. It NEVER executes code and
// NEVER produces a handle (the persisted form has none — the marshaller rejected them). nil/empty
// bytes yield a fresh empty table (the pre-7.6 / no-script-state default).
//
// The caps that the SAVE path enforces are MIRRORED on load (T10 symmetry — defense in depth): a
// legitimate script can never produce an over-cap state (the save cap rejects it), but a CORRUPTED
// or attacker-written DB row could, and load must not balloon the VM rehydrating it. So a MALFORMED
// or OVER-CAP blob is BOUNDED on load — it degrades to a fresh empty table (over-byte) or truncates
// (over-depth past luaStateMaxDepth) with a loud log, never a crash. Zone goroutine only.
func (rt *luaRuntime) unmarshalLuaState(b json.RawMessage) (*lua.LTable, error) {
	if rt == nil || rt.L == nil {
		return nil, fmt.Errorf("lua runtime not initialized")
	}
	if len(b) == 0 {
		return rt.L.NewTable(), nil
	}
	// BYTE cap on load (T10 symmetry): reject an over-byte blob BEFORE decoding — this bounds both
	// the total size and the key balloon up front (a corrupted/attacker row), degrading to empty.
	if len(b) > luaStateMaxBytes {
		return rt.L.NewTable(), luaStateError{fmt.Sprintf("self.state load: over-cap blob (%d bytes > %d) — degraded to empty", len(b), luaStateMaxBytes)}
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return rt.L.NewTable(), luaStateError{"self.state load: malformed JSON: " + err.Error()}
	}
	return rt.goToLuaTable(v), nil
}

// goToLua converts a decoded JSON value to its Lua form (data only: nil/bool/number/string/table).
// A JSON object -> a Lua string-keyed table; a JSON array -> a Lua 1..N table. No code, no handle.
// depth bounds nesting past luaStateMaxDepth (T10 symmetry — a deeply-nested hostile blob whose
// byte size slipped under the byte cap is TRUNCATED to nil at the cap, never a 5000-deep table).
func (rt *luaRuntime) goToLua(v any, depth int) lua.LValue {
	switch val := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(val)
	case float64:
		return lua.LNumber(val)
	case string:
		return lua.LString(val)
	case map[string]any:
		if depth >= luaStateMaxDepth {
			return lua.LNil // truncate a too-deep hostile blob (defense in depth, never a crash)
		}
		t := rt.L.NewTable()
		for k, mv := range val {
			t.RawSetString(k, rt.goToLua(mv, depth+1))
		}
		return t
	case []any:
		if depth >= luaStateMaxDepth {
			return lua.LNil
		}
		t := rt.L.NewTable()
		for i, av := range val {
			t.RawSetInt(i+1, rt.goToLua(av, depth+1))
		}
		return t
	default:
		// json.Unmarshal into `any` only ever produces the above kinds; anything else is dropped to
		// nil (defensive — never executes anything).
		return lua.LNil
	}
}

// goToLuaTable is goToLua specialized to return a *lua.LTable (the top-level self.state is always a
// table). A non-table top-level JSON value (a bare number/string — shouldn't happen for a state
// blob) yields an empty table.
func (rt *luaRuntime) goToLuaTable(v any) *lua.LTable {
	if t, ok := rt.goToLua(v, 0).(*lua.LTable); ok {
		return t
	}
	return rt.L.NewTable()
}
