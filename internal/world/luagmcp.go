package world

import (
	"encoding/json"
	"fmt"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// luagmcp.go — the `gmcp` sandbox module (#51): a content/Lua handle to emit CUSTOM GMCP frames to a
// player's rich client, e.g. a quest tracker or a boss-fight timer a Mudlet UI script consumes. The one
// call is gmcp.send(player, pkg, table).
//
// # Why this is safe to hand to untrusted content
//
// Three guards, each fail-closed:
//
//   - NAMESPACE ALLOWLIST (the load-bearing one). The package's top-level segment MUST be in
//     allowedGMCPNamespaces ("Mud", the Mudlet/IRE convention for a server's own packages). This is what
//     stops content from spoofing an ENGINE package — a gmcp.send(p, "Char.Vitals", {hp=999}) or a
//     "Core.*" frame that would feed a client false HUD/auth-shaped data. The GATE's outbound filter only
//     checks the client advertised the package (it would happily forward a content "Char.Vitals" to a
//     client that advertised Char), so the engine-vs-content boundary is enforced HERE, at the source.
//   - CHARSET + LENGTH. validCustomGMCPPackage mirrors the gate's inbound guard (alnum+'.', ≤64, no
//     edge dots) so the world fails closed rather than relying on the edge's drop-and-log.
//   - BOUNDED ENCODING. The payload table is walked by a DEPTH + NODE + BYTE bounded encoder that rejects
//     functions/userdata and can't be driven into unbounded recursion by a self-referential table.
//
// The frame then rides the normal session send, so the GATE's outbound support filter still applies: a
// client that never advertised the package stays silent. gmcp.send reports whether it reached a live
// player SESSION (a non-player / session-less handle is a clean no-op), NOT whether the client displayed
// it — that remains the client's own Core.Supports choice.

const (
	// luaGMCPMaxDepth bounds nesting in a script-built payload table. A cyclic or pathologically deep
	// table would otherwise recurse unbounded; the cap turns it into a clean Lua error.
	luaGMCPMaxDepth = 16
	// luaGMCPMaxNodes bounds the TOTAL values encoded from one gmcp.send call — the breadth guard that
	// backstops the depth cap against a wide-but-shallow alloc bomb.
	luaGMCPMaxNodes = 4096
	// luaGMCPMaxBytes caps the marshaled payload. It MUST stay well under the gate's outbound frame cap
	// (telnet.maxGMCPPayload, 1 MiB) — this world-side cap is intentionally the tighter one so a content
	// frame that passes here always fits the gate, never a silent gate-side drop. An over-cap payload is a
	// clean Lua error, not a truncated frame. Keep this invariant (luaGMCPMaxBytes < maxGMCPPayload) if
	// either bound changes.
	luaGMCPMaxBytes = 6 << 10
)

// allowedGMCPNamespaces is the ALLOWLIST of top-level GMCP namespaces content may emit under (#51),
// fail-closed: only these pass, so content can never name an engine package (Char/Core/Room/Comm) to
// spoof a client. "Mud" is the Mudlet/IRE convention for a server's custom packages. Extend deliberately.
var allowedGMCPNamespaces = map[string]bool{"Mud": true}

// installGMCPTable exposes the read-only `gmcp` global with its single `send` function. Called once at
// sandbox build (luart.go), after the allowlist env is in place. Read-only like mud/ui, so a script
// cannot replace gmcp.send with its own.
func (rt *luaRuntime) installGMCPTable() {
	L := rt.L
	tbl := L.NewTable()
	L.SetFuncs(tbl, map[string]lua.LGFunction{"send": rt.gmcpSend})
	g := L.Get(lua.GlobalsIndex).(*lua.LTable)
	g.RawSetString("gmcp", rt.readOnly(tbl))
}

// gmcpSend implements gmcp.send(player, pkg[, table]) — see the file header for the guard model. Returns
// true when the frame reached a live player session, false for a non-player / session-less / unresolved
// handle. An invalid package name, a spoofed engine namespace, a non-table payload, or an over-budget
// table each raise a clean Lua error (fail-closed) rather than emitting a partial/unsafe frame.
func (rt *luaRuntime) gmcpSend(l *lua.LState) int {
	target := resolveHandle(l, 1)
	pkg := l.CheckString(2)
	if !validCustomGMCPPackage(pkg) {
		l.RaiseError("gmcp.send: %q is not an allowed custom GMCP package (name must be alnum/'.' and under a sanctioned namespace, e.g. \"Mud.*\")", pkg)
		return 0
	}

	// The payload table (arg 3) is optional — a bare gmcp.send(p, "Mud.Ping") emits an empty object.
	payload := []byte("{}")
	if l.GetTop() >= 3 && l.Get(3) != lua.LNil {
		t, ok := l.Get(3).(*lua.LTable)
		if !ok {
			l.RaiseError("gmcp.send: payload must be a table")
			return 0
		}
		b, err := luaTableToGMCPJSON(l, t)
		if err != nil {
			l.RaiseError("gmcp.send: %v", err)
			return 0
		}
		payload = b
	}

	// Resolve AFTER validation so a bad package/payload always errors, regardless of the target.
	if target == nil {
		l.Push(lua.LFalse)
		return 1
	}
	s, ok := sessionOf(target)
	if !ok {
		l.Push(lua.LFalse) // a mob / session-less handle — clean no-op
		return 1
	}
	s.send(gmcpFrame(pkg, payload))
	l.Push(lua.LTrue)
	return 1
}

// validCustomGMCPPackage gates a content-supplied GMCP package name: the SAME strict, log-safe charset
// the gate enforces on inbound names (non-empty, ≤64 bytes, letters/digits/'.' only, no leading/trailing
// dot — mirrored here so the world fails closed at the source, not only at the edge), PLUS the #51
// namespace ALLOWLIST so content can only emit under a sanctioned prefix, never an engine package.
func validCustomGMCPPackage(pkg string) bool {
	if pkg == "" || len(pkg) > 64 {
		return false
	}
	if pkg[0] == '.' || pkg[len(pkg)-1] == '.' {
		return false
	}
	for i := 0; i < len(pkg); i++ {
		c := pkg[i]
		if c == '.' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	top := pkg
	if i := strings.IndexByte(pkg, '.'); i >= 0 {
		top = pkg[:i]
	}
	return allowedGMCPNamespaces[top]
}

// luaTableToGMCPJSON encodes a script-built payload table to bounded JSON. It walks the table with a
// shared node budget + a depth cap (gmcpValueToGo) so neither breadth nor a cycle can run away, marshals
// via encoding/json (which escapes every control byte — the payload can't inject terminal control), and
// rejects an over-cap result.
func luaTableToGMCPJSON(l *lua.LState, t *lua.LTable) ([]byte, error) {
	nodes := 0
	v, err := gmcpValueToGo(l, t, 0, &nodes)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("payload not encodable: %v", err)
	}
	if len(b) > luaGMCPMaxBytes {
		return nil, fmt.Errorf("payload too large (cap %d bytes)", luaGMCPMaxBytes)
	}
	return b, nil
}

// gmcpValueToGo converts one Lua value to a JSON-encodable Go value, charging the shared node budget and
// enforcing the depth cap. Only strings, numbers, booleans, and (nested) tables are allowed — a function,
// userdata, or thread is a clean error, so no engine handle can leak into a client payload.
func gmcpValueToGo(l *lua.LState, lv lua.LValue, depth int, nodes *int) (any, error) {
	if depth > luaGMCPMaxDepth {
		return nil, fmt.Errorf("payload nested too deep (max %d)", luaGMCPMaxDepth)
	}
	*nodes++
	if *nodes > luaGMCPMaxNodes {
		return nil, fmt.Errorf("payload has too many values (max %d)", luaGMCPMaxNodes)
	}
	switch v := lv.(type) {
	case lua.LString:
		return string(v), nil
	case lua.LNumber:
		return float64(v), nil
	case lua.LBool:
		return bool(v), nil
	case *lua.LTable:
		return gmcpTableToGo(l, v, depth, nodes)
	default:
		if lv == lua.LNil {
			return nil, nil
		}
		return nil, fmt.Errorf("payload has an unsupported %s value (only strings, numbers, booleans, tables)", lv.Type().String())
	}
}

// gmcpTableToGo encodes a Lua table as a JSON array or a JSON object, matching how GMCP payloads are used
// — clean objects or arrays, never a mix. A table with a 1..N array part (Len() > 0) becomes an ARRAY of
// exactly those N slots; any string-keyed entries alongside it are dropped (and a sparse array stops at
// the first gap). Otherwise it becomes an OBJECT of its string keys, skipping any non-string key. Both
// "degrade" a mixed/sparse table rather than erroring; content should keep a payload a clean object OR a
// dense array.
func gmcpTableToGo(l *lua.LState, t *lua.LTable, depth int, nodes *int) (any, error) {
	if n := t.Len(); n > 0 {
		arr := make([]any, 0, n)
		for i := 1; i <= n; i++ {
			ev, err := gmcpValueToGo(l, t.RawGetInt(i), depth+1, nodes)
			if err != nil {
				return nil, err
			}
			arr = append(arr, ev)
		}
		return arr, nil
	}
	obj := map[string]any{}
	var ferr error
	t.ForEach(func(k, v lua.LValue) {
		if ferr != nil {
			return
		}
		ks, ok := k.(lua.LString)
		if !ok {
			return // non-string key: skip (JSON object keys are strings)
		}
		ev, err := gmcpValueToGo(l, v, depth+1, nodes)
		if err != nil {
			ferr = err
			return
		}
		obj[string(ks)] = ev
	})
	if ferr != nil {
		return nil, ferr
	}
	return obj, nil
}
