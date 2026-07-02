package world

import (
	"fmt"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// luaentry.go — the entry-point INVOCATION CORE (docs/PHASE7-PLAN.md slice 7.4, §1.1, §4): the
// machinery that compiles a reserved Lua column ONCE and runs it at a lifecycle point with the
// MANDATORY invariants the 7.2 + 7.3c reviews require:
//
//  1. The FIRING cascade's budget is threaded — the luaInvocation carries the engine ctx's
//     eventBudget POINTER + depth (NOT a fresh one), so a Lua harm op / bus handler fired inside
//     an event cascade decrements the SAME shared width/depth budget and cannot re-fire to escape
//     it (invokeFromCtx below builds the invocation FROM the firing *effectCtx).
//  2. Context (self/ctx/ev) is bound in a per-call FRESH `_ENV` (not a shared global) — so a
//     reaction fired mid-handler can never observe a stale `self`. Each call gets a fresh env
//     table that falls through to the sandbox globals via __index; the binds + any globals the
//     script writes live in THAT table and are discarded when the call returns.
//  3. Fail-closed: a compile error keeps the def inert (bare-engine invariant holds — no-Lua
//     content still boots); a runtime error is pcall-isolated and fizzles just that action
//     (the circuit breaker is 7.5; here pcall + log).
//
// Single-writer: every invocation runs on the zone goroutine.

// compiledChunk is a reusable compiled Lua body plus its content origin (for logging) and the
// monotonic generation it was compiled at (the hot-reload drop seam, inert until 7.7). The
// *lua.FunctionProto is the bytecode; each invocation instantiates a fresh function over it with
// a per-call environment, so one compile serves every invocation (§1.1).
type compiledChunk struct {
	proto  *lua.FunctionProto
	origin string // "ability:fireball:on_resolve" | "trigger:guard:greet" — diagnostic
	gen    uint64
}

// compileChunk compiles src into a reusable proto, or returns an error (fail-closed: the caller
// keeps the def inert on a compile error — invariant 3). origin tags it for logs. An empty src
// yields (nil, nil) — "no Lua body", the common no-op case (a def with no reserved column).
func (rt *luaRuntime) compileChunk(origin, src string) (*compiledChunk, error) {
	if rt == nil || rt.L == nil {
		return nil, fmt.Errorf("lua runtime not initialized")
	}
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	fn, err := rt.L.Load(strings.NewReader(src), origin)
	if err != nil {
		// Fail-closed: a syntactically-broken body compiles to nothing; the def stays inert.
		rt.log.Warn("lua compile failed; def left inert", "origin", origin, "err", err.Error())
		return nil, fmt.Errorf("lua compile %s: %w", origin, err)
	}
	return &compiledChunk{proto: fn.Proto, origin: origin, gen: rt.chunkGen}, nil
}

// chunkFor returns the compiled chunk for content key `key` (e.g. "ability:fireball:on_resolve"),
// compiling src on first use and caching it. It is SOURCE-AWARE (slice 7.7 hot reload): if the
// cached entry was compiled from the SAME src it is reused (the common hot path — compile once);
// if src DIFFERS (a content edit) it RECOMPILES and bumps the chunk generation, so the new body
// actually takes effect. A recompile that FAILS keeps the LAST-GOOD chunk (a syntactically-broken
// edit keeps the old behavior, logged — like the prototype reloader keeps the last-known on a bad
// re-read). An empty src caches a nil chunk (the def is inert). Zone goroutine only.
func (rt *luaRuntime) chunkFor(key, src string) *compiledChunk {
	if rt == nil || rt.L == nil {
		return nil
	}
	if e, ok := rt.chunks[key]; ok && e.src == src {
		return e.chunk // cached for THIS source (nil if empty/failed — not retried)
	}
	// The first compiled chunk means this zone runs scripts: register the periodic memory-metric
	// pulse lazily (a scriptless zone never pays for it — the bare-engine invariant).
	rt.ensureMetricPulse()

	prev, hadPrev := rt.chunks[key]
	ch, err := rt.compileChunk(key, src)
	if err != nil && hadPrev && prev.chunk != nil {
		// A RECOMPILE on a changed source FAILED: keep the last-good chunk (don't blank a working
		// def on a broken edit). Record the new (bad) src so we don't re-attempt every call, but
		// leave prev.chunk in place so the old behavior persists until the edit is fixed.
		rt.log.Warn("lua recompile failed; keeping last-good chunk (broken edit ignored)",
			"key", key, "err", err.Error())
		prev.src = src // remember the bad source so a re-call with the same bad src doesn't recompile
		return prev.chunk
	}
	if hadPrev {
		// A SUCCESSFUL recompile of a CHANGED source is a hot reload of this def: bump the chunk
		// generation (P7-D7 — old-gen mud.after timers drop on fire, never running old code against
		// new state) and reset this script's circuit breaker (the 7.5 carry-forward — a fix reload
		// re-enables a script a bug had quarantined). breakerReset clears BOTH the shared key and the
		// per-instance keys would need a sweep, but the shared (kind,ref) key matches `key`'s origin.
		rt.chunkGen++
		rt.breakerReset(breakerKeyShared(key))
		rt.log.Debug("lua chunk hot-reloaded", "key", key, "gen", rt.chunkGen)
	}
	rt.chunks[key] = &chunkCacheEntry{chunk: ch, failed: err != nil, src: src}
	return ch
}

// invoke runs a compiled chunk under invocation inv, binding the given context variables (self/
// ctx/ev/…) in a FRESH per-call environment (invariant 2). It arms the budget/deadline + spawn-
// budget chokepoint (like runChunk), sets rt.inv for the duration (so a Lua harm op reads its
// actor/source/budget from the engine, never a Lua arg — P7-D3), and is pcall-isolated: a
// runtime error fizzles just this call and is logged, never propagated to crash the lifecycle
// (invariant 3). Returns the pcall error (nil on success) for the caller that wants the result
// (e.g. a formula reads a return value before this returns).
func (rt *luaRuntime) invoke(ch *compiledChunk, inv *luaInvocation, binds map[string]lua.LValue) error {
	if rt == nil || rt.L == nil || ch == nil || ch.proto == nil {
		return nil // no body => nothing to run (the no-Lua-content path)
	}
	key := rt.breakerKeyFor(inv, ch.origin)
	if rt.breakerDisabled(key) {
		return nil // quarantined script: a clean no-op (the zone serves on)
	}
	L := rt.L

	// A fresh per-call environment that falls through to the sandbox globals for reads
	// (string/math/mud/handle methods) but holds the per-call binds (self/ctx/ev) and absorbs
	// any global the script writes — so nothing leaks across calls (invariant 2).
	env := rt.freshCallEnv(binds)
	fn := L.NewFunctionFromProto(ch.proto)
	L.SetFEnv(fn, env)

	// Establish the invocation (who the script acts as + the threaded cascade budget) and run
	// through THE chokepoint (pcallGuarded), pcall-isolated + breaker-tracked.
	prev := rt.inv
	rt.inv = inv
	defer func() { rt.inv = prev }()

	if err := rt.runGuardedFn(key, ch.origin, fn, 0, lua.MultRet); err != nil {
		// Fail-closed isolation (invariant 3): the raw error/stack goes to OPS logs only, never to
		// a player (the caller renders a generic fizzle). The breaker accounting already happened
		// inside pcallGuarded.
		rt.log.Warn("lua entry-point error (isolated; action fizzled, zone unaffected)",
			"origin", ch.origin, "err", err.Error())
		return err
	}
	return nil
}

// invokeFromCtx is the bus/lifecycle entry: it builds the luaInvocation FROM the firing engine
// *effectCtx so the actor + the SHARED depth/eventBudget pointer are threaded (invariant 1) —
// the firing cascade's budget, never a fresh one. self binds to the script-owning entity (the
// subject of the event / the scripted entity), and the engine ctx's actor drives the harm
// surface. This is the single helper every cascade-fired Lua body goes through.
func (rt *luaRuntime) invokeFromCtx(ch *compiledChunk, c *effectCtx, self *Entity, binds map[string]lua.LValue) error {
	inv := &luaInvocation{
		actor:       c.actor,
		depth:       c.depth,       // threaded, not reset
		eventBudget: c.eventBudget, // the SAME shared pointer the cascade decrements
	}
	if binds == nil {
		binds = map[string]lua.LValue{}
	}
	if _, ok := binds["self"]; !ok {
		binds["self"] = rt.newHandle(self)
	}
	return rt.invoke(ch, inv, binds)
}

// freshCallEnv builds a per-call environment table: a fresh table whose metatable __index falls
// through to the sandbox globals (so string/math/mud/the handle methods resolve), pre-populated
// with the call's binds (self/ctx/ev). A global WRITE in the script lands in this table (the
// __newindex default for an absent key writes the table itself), so it never mutates the shared
// sandbox globals and is discarded when the call returns.
func (rt *luaRuntime) freshCallEnv(binds map[string]lua.LValue) *lua.LTable {
	L := rt.L
	env := L.NewTable()
	mt := L.NewTable()
	mt.RawSetString("__index", L.Get(lua.GlobalsIndex))
	L.SetMetatable(env, mt)
	for k, v := range binds {
		if v == nil {
			v = lua.LNil
		}
		env.RawSetString(k, v)
	}
	return env
}

// invokeForNumber runs a compiled chunk (a Lua FORMULA) and returns its first numeric return
// value. A formula reads via handles bound in `binds` and returns a number. On a compile-absent
// chunk, a runtime error, or a non-number return it returns (0, false) — the caller falls back
// to its default (fail-closed: a broken formula never silently corrupts a stat). It is
// pcall-isolated like invoke.
func (rt *luaRuntime) invokeForNumber(ch *compiledChunk, inv *luaInvocation, binds map[string]lua.LValue) (float64, bool) {
	if rt == nil || rt.L == nil || ch == nil || ch.proto == nil {
		return 0, false
	}
	key := rt.breakerKeyFor(inv, ch.origin)
	if rt.breakerDisabled(key) {
		return 0, false // quarantined: caller uses the engine default
	}
	L := rt.L
	env := rt.freshCallEnv(binds)
	fn := L.NewFunctionFromProto(ch.proto)
	L.SetFEnv(fn, env)

	prev := rt.inv
	rt.inv = inv
	defer func() { rt.inv = prev }()

	top := L.GetTop()
	if err := rt.runGuardedFn(key, ch.origin, fn, 0, 1); err != nil {
		rt.log.Warn("lua formula error (isolated; using engine default)", "origin", ch.origin, "err", err.Error())
		L.SetTop(top)
		return 0, false
	}
	ret := L.Get(-1)
	L.SetTop(top)
	if n, ok := ret.(lua.LNumber); ok {
		return float64(n), true
	}
	return 0, false
}

// invokeForString runs a compiled chunk and returns its first return value as a string. A compile-absent
// chunk, a runtime error, or a non-string return yields ("", false) — the caller falls back to its default
// (fail-closed: a broken display template never renders garbage). pcall-isolated like invoke. Used by the
// display-template render path (a body returning the assembled sheet string).
func (rt *luaRuntime) invokeForString(ch *compiledChunk, inv *luaInvocation, binds map[string]lua.LValue) (string, bool) {
	if rt == nil || rt.L == nil || ch == nil || ch.proto == nil {
		return "", false
	}
	key := rt.breakerKeyFor(inv, ch.origin)
	if rt.breakerDisabled(key) {
		return "", false // quarantined: caller uses its fallback
	}
	L := rt.L
	env := rt.freshCallEnv(binds)
	fn := L.NewFunctionFromProto(ch.proto)
	L.SetFEnv(fn, env)

	prev := rt.inv
	rt.inv = inv
	defer func() { rt.inv = prev }()

	top := L.GetTop()
	if err := rt.runGuardedFn(key, ch.origin, fn, 0, 1); err != nil {
		rt.log.Warn("lua display template error (isolated; using fallback)", "origin", ch.origin, "err", err.Error())
		L.SetTop(top)
		return "", false
	}
	ret := L.Get(-1)
	L.SetTop(top)
	if s, ok := ret.(lua.LString); ok {
		return string(s), true
	}
	return "", false
}

// invokeForBool runs a compiled chunk (the pvp_allowed POLICY) and returns its boolean result.
// SECURITY (fail-closed): a compile-absent chunk, a runtime error, or a non-boolean/false return
// all yield (false) for the gate's "is harm allowed?" question — a missing or erroring policy
// must DENY harm, never default-allow. Only an explicit Lua `true` permits. pcall-isolated.
func (rt *luaRuntime) invokeForBool(ch *compiledChunk, inv *luaInvocation, binds map[string]lua.LValue) bool {
	if rt == nil || rt.L == nil || ch == nil || ch.proto == nil {
		return false // no policy => deny (fail-closed)
	}
	key := rt.breakerKeyFor(inv, ch.origin)
	if rt.breakerDisabled(key) {
		return false // quarantined policy => deny (fail-closed)
	}
	L := rt.L
	env := rt.freshCallEnv(binds)
	fn := L.NewFunctionFromProto(ch.proto)
	L.SetFEnv(fn, env)

	prev := rt.inv
	rt.inv = inv
	defer func() { rt.inv = prev }()

	top := L.GetTop()
	if err := rt.runGuardedFn(key, ch.origin, fn, 0, 1); err != nil {
		rt.log.Warn("lua pvp_allowed policy error (isolated; FAIL-CLOSED, harm denied)",
			"origin", ch.origin, "err", err.Error())
		L.SetTop(top)
		return false // fail-closed
	}
	ret := L.Get(-1)
	L.SetTop(top)
	// STRICT: only an explicit boolean `true` permits harm. A string/number/table (even a
	// "truthy" one) or any non-true value DENIES — a policy that fails to return a clean
	// boolean is treated as broken, and a broken harm-consent policy must fail-closed. This is
	// deliberately stricter than Lua truthiness (LVAsBool) so a sloppy `return "ok"` cannot
	// silently open the gate.
	return ret == lua.LTrue
}

// invokeForStringList runs a compiled chunk and returns the array part of its first return value as a
// []string (each element coerced: a string is taken verbatim; a non-string element is skipped). A
// compile-absent chunk, a runtime error, a non-table return, or an empty return yields nil — the caller
// then adds no drops (fail-closed: a broken hatch never fabricates loot). pcall-isolated like invoke. Used
// by the loot on_roll hatch (a body returning `{ "item:ref", ... }`).
func (rt *luaRuntime) invokeForStringList(ch *compiledChunk, inv *luaInvocation, binds map[string]lua.LValue) []string {
	if rt == nil || rt.L == nil || ch == nil || ch.proto == nil {
		return nil
	}
	key := rt.breakerKeyFor(inv, ch.origin)
	if rt.breakerDisabled(key) {
		return nil // quarantined: no drops
	}
	L := rt.L
	env := rt.freshCallEnv(binds)
	fn := L.NewFunctionFromProto(ch.proto)
	L.SetFEnv(fn, env)

	prev := rt.inv
	rt.inv = inv
	defer func() { rt.inv = prev }()

	top := L.GetTop()
	if err := rt.runGuardedFn(key, ch.origin, fn, 0, 1); err != nil {
		rt.log.Warn("lua string-list hatch error (isolated; no drops added)", "origin", ch.origin, "err", err.Error())
		L.SetTop(top)
		return nil
	}
	ret := L.Get(-1)
	L.SetTop(top)
	tbl, ok := ret.(*lua.LTable)
	if !ok {
		return nil
	}
	// Iterate the ARRAY part by index (1..Len) so the drop order is deterministic (a seeded loot test can
	// pin it); a non-string element is skipped, a nil stops the array. The hash part is ignored.
	var out []string
	for i := 1; i <= tbl.Len(); i++ {
		if s, isStr := tbl.RawGetInt(i).(lua.LString); isStr {
			out = append(out, string(s))
		}
	}
	return out
}
