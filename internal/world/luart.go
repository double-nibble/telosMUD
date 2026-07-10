package world

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/double-nibble/telosmud/internal/luasandbox"
)

// luart.go — the per-zone Lua runtime: VM lifecycle + the restricted-globals sandbox
// (docs/PHASE7-PLAN.md slice 7.1, docs/LUA.md). This slice builds ONLY the VM and the
// sandbox skeleton — no entity handles, no effect ops, no entry points (those are 7.2+).
//
// The single most security-sensitive code in the engine: builders run arbitrary Lua
// IN-PROCESS, ON the zone goroutine. The sandbox is defense-in-depth that must hold even
// against a hostile author (P7-D9). Every constant and construction step below carries a
// threat-model row from PHASE7-PLAN.md §2; the security-auditor reviews this file.
//
// Concurrency: the *lua.LState is constructed at zone build and called ONLY from
// Zone.Run's goroutine (the single-writer invariant). gopher-lua is not goroutine-safe
// and we never need it to be — no goroutine touches it, no lock guards it.

// The zone sandbox's caps + budgets are ALIASES of the shared luasandbox constants (#47), so the zone and
// the director sandboxes cannot diverge: luasandbox is the single source of truth, and a parity test
// (TestSandboxParityWithLuasandbox) cross-checks the two live sandboxes' allowlist + amplifier-cap behavior.
// The threat-model rationale for each (T3/T4/T13) lives in docs/PHASE7-PLAN.md §2 and in luasandbox's doc.
const (
	luaCallStackSize   = luasandbox.CallStackSize   // T4 — recursion blowout (build-time Options.CallStackSize)
	luaRegistrySize    = luasandbox.RegistrySize    // T4 — value-stack floor
	luaRegistryMaxSize = luasandbox.RegistryMaxSize // T4 — value-stack ceiling
	luaInstrBudget     = luasandbox.InstrBudget     // T3 — per-call instruction cap (fork count abort)
	luaStrByteCap      = luasandbox.StrByteCap      // T13 — amplifier OUTPUT byte cap
	luaPatternInputCap = luasandbox.PatternInputCap // T13 — pattern INPUT byte cap

	// luaCallDeadline is the per-call wall-clock deadline (T3), armed via SetContext — catches a
	// low-instruction stall the count cannot (a GC pause). Derived from the shared millisecond constant.
	luaCallDeadline = luasandbox.CallDeadline * time.Millisecond
)

// luaRuntime is a zone's Lua VM plus the engine-owned state the sandbox depends on. It is
// zone-owned: created at zone build, torn down on zone stop, touched ONLY by the zone
// goroutine. nil on a zone that was built without a runtime (none today — every zone gets
// one — but the field is nil-checked so the bare-engine path is unaffected if a future
// build path skips it).
type luaRuntime struct {
	L *lua.LState

	// rng is the per-zone seeded RNG that math.random / mud.random draw from (T9 / P7-D4).
	// Seeded deterministically from the zone id at build, so two runs of the same scripted
	// content produce identical rolls (reproducible in tests and replays). NOT crypto; this
	// is gameplay determinism. Only the zone goroutine touches it.
	rng *rand.Rand

	// log is the scoped logger print()/mud.log route to (structured). Tagged with the zone.
	log *slog.Logger

	// stringProxy is the read-only `string.`-namespace table scripts see as the global
	// `string`. It is a SEPARATE table from the private string metatable (T14): poisoning it
	// (which the read-only guard blocks anyway) cannot reach method-syntax dispatch.
	stringProxy *lua.LTable

	// stringMeta is the PRIVATE, engine-owned string metatable holding the capped wrappers.
	// It is installed as builtinMts[LTString] (via SetMetatable) so method syntax
	// ("x"):rep() dispatches through the capped wrappers, and is NEVER exposed as a
	// script-reachable global (T14 — the load-bearing invariant). No Lua value references a
	// mutable copy of it.
	stringMeta *lua.LTable

	// zone is the runtime's owning Zone — the world the mud.* table reads/messages/spawns into
	// and whose pulse wheel mud.after schedules on (luamud.go). Set by newZone right after the
	// runtime is built (the runtime is created from the zone id; the back-pointer is wired once
	// the Zone exists). nil only on a runtime built standalone in a test before a zone wires it;
	// the mud.* functions nil-check it. Only the zone goroutine touches it.
	zone *Zone

	// spawnThisCall is the per-CALL mud.spawn count (T-abuse/DoS, luamud.go): the entities spawned
	// in the CURRENT entry-point call, reset at the chokepoint (per call). The per-ZONE standing
	// bound is the LIVE census luaSpawnsLive (below), not a monotonic lifetime count. Zone
	// goroutine only.
	spawnThisCall int

	// chunkGen is the runtime's current chunk generation — the hot-reload drop seam a mud.after
	// timer captures at scheduling time so a callback bound to a swapped chunk is dropped on
	// fire (mirrors the zone-gen handle guard). Wired but NEVER bumped until slice 7.7. Zone
	// goroutine only.
	chunkGen uint64

	// inv is the CURRENT invocation context (luahandle.go, slice 7.3c) — the ENGINE-owned
	// answer to "on whose behalf, and inside what cascade, is this script running?" It is the
	// sole source of the actor/source/disp/rng/depth/eventBudget a Lua harm op's effectCtx is
	// built from (P7-D3's five invariants): a harm method NEVER takes actor/source/disp from a
	// Lua argument. It is set when an entry point invokes a script (runChunkWithSelf binds the
	// scripted entity as the invocation actor) and cleared after, so a harm op outside any
	// invocation (a bare runChunk) has no actor and every harm method cleanly no-ops. The full
	// cascade-threading (depth/eventBudget from a fired event) lands with the entry points/
	// reactions (7.4/7.9); here the fields exist and are threaded so the invariant is structural
	// from the start. Zone goroutine only.
	inv *luaInvocation

	// chunks is the per-zone compiled-chunk cache (luaentry.go, slice 7.4): a content Lua body
	// (an ability on_resolve, an affect hook, a trigger, a formula, the pvp policy) is compiled
	// ONCE per zone on first use and cached here keyed by a stable content key. The cache is
	// per-RUNTIME (per-zone) because a *lua.FunctionProto is bound to THIS zone's LState — the
	// shared per-shard defs cannot carry a proto. A compile FAILURE caches a sentinel (failed)
	// entry so a broken def is inert and not recompiled every invocation. This is also the seam
	// slice 7.7 invalidates on a hot reload. Zone goroutine only.
	chunks map[string]*chunkCacheEntry

	// entityScripts holds the per-INSTANCE trigger state (luaentry_triggers.go, slice 7.4c): for
	// each scripted entity (one carrying a *Scripted component), its registered on(event,fn)
	// handler table + its self.state table, both Lua values bound to THIS zone's LState. Built
	// lazily on first fire by running the entity's Scripted.source. Keyed by RuntimeID (per-zone
	// unique). In-memory only at 7.4c — the self.state ↔ JSONB persistence is slice 7.6 (the seam
	// is noted there). Zone goroutine only.
	entityScripts map[RuntimeID]*entityScript

	// breakers is the per-script error-budget circuit breaker (luabreaker.go, slice 7.5, P7-D10):
	// keyed by a SCRIPT KEY (per-instance for entity-scoped scripts: "trigger:#<rid>"; per-(kind,
	// ref) for shared defs: "ability:<ref>:on_resolve"). A chronically-failing script trips its
	// breaker and is DISABLED (its invocations no-op) — never the zone. Reset on a successful hot
	// reload (7.7). Zone goroutine only.
	breakers map[string]*breakerState

	// luaTimersLive counts the live Lua-scheduled mud.after wheel entries (luamud.go, slice 7.5):
	// a callback that schedules ≥1 timer each fire would otherwise grow the wheel unboundedly
	// across ticks (bounded by neither the per-call budget nor the spawn cap). Incremented on a
	// mud.after schedule, decremented when the timer fires or is cancelled; over the cap, a
	// mud.after is a clean error. Zone goroutine only.
	luaTimersLive int

	// luaSpawnsLive is the LIVE census of Lua-spawned entities (luamud.go, slice 7.5): incremented
	// on a mud.spawn, DECREMENTED when such an entity dies/despawns, so the per-zone spawn cap
	// bounds the LIVE population (not the lifetime count) — a long-lived zone keeps spawning as its
	// Lua-spawned mobs die. Zone goroutine only.
	luaSpawnsLive int

	// luaSpawnedRIDs is the set of rids mud.spawn created, so death/despawn can decrement the live
	// census (dropLuaSpawn). Lazily allocated on the first spawn. Zone goroutine only.
	luaSpawnedRIDs map[RuntimeID]bool

	// metricPulse is the cancel handle for the periodic per-zone Lua memory-metric pulse (T5,
	// luaentry_chokepoint.go), registered LAZILY on the first compiled chunk so a scriptless zone
	// never pays for it. Zone goroutine only.
	metricPulse *pulseHandle

	// fireData carries the firer's plain-data table for the custom event CURRENTLY being dispatched
	// (mud.fire's 3rd arg, event.go fireCustomEvent). It is set immediately before mud.fire enters
	// z.fireEvent and cleared immediately after — so the engine's shared fireEvent signature never
	// has to carry a Lua value, and a nested custom fire (a handler that fires another) save/restores
	// it stack-discipline so each handler reads ITS firer's data. Zone goroutine only (single-writer).
	fireData *lua.LTable
}

// chunkCacheEntry is one cached compile result: the chunk (nil if the source was empty or the
// compile failed) plus a `failed` discriminator so a broken def (compiled nil) is distinguished
// from a never-compiled key, plus the SOURCE STRING it was compiled from (the hot-reload key —
// slice 7.7). chunkFor recompiles when the passed src differs from `src` here, so a content edit
// actually takes effect (the 7.4/7.5 review MUST-FIX: caching by key alone made hot-reload a
// silent no-op — and, for pvp_allowed, kept a stale permissive policy). On a recompile that FAILS,
// the LAST-GOOD chunk is kept (a broken edit keeps the old behavior, like the prototype reloader).
// Zone-goroutine-owned.
type chunkCacheEntry struct {
	chunk  *compiledChunk
	failed bool
	src    string // the source this entry was compiled from (the recompile-on-change key)
}

// luaInvocation is the engine-owned context of the script call currently on the stack — who it
// acts as and what cascade budget it inherits. It is the ONLY place a Lua harm op's
// actor/source/depth/eventBudget come from (never a Lua argument — P7-D3 invariants 1 & 5). A
// nil runtime.inv means "no entry-point invocation is active": a harm op then has no actor and
// no-ops (fail-closed).
type luaInvocation struct {
	// actor is the entity on whose behalf the script runs (the scripted mob for a trigger, the
	// caster for an ability on_resolve). It is BOTH the effectCtx actor and source — a script
	// cannot name a different source to spoof attribution past the gate (invariant 1).
	actor *Entity
	// depth / eventBudget are threaded from the invoking cascade (invariant 5) so a Lua harm op
	// fired inside an event/reaction cascade inherits the SAME shared depth/width budget and
	// cannot escape it. nil eventBudget outside a cascade (a top-level trigger), exactly like a
	// command-issued cast (effect_op.go). Entry points (7.4) populate these from the fire that
	// invoked the script; 7.3c threads them so the binding is correct from the first harm op.
	depth       int
	eventBudget *int

	// breakerKey, when set, OVERRIDES the chokepoint's default (shared-by-origin) circuit-breaker
	// key (luabreaker.go, slice 7.5). An ENTITY-SCOPED invocation (a trigger / a self.state-bearing
	// script) sets it to a per-INSTANCE key (breakerKeyInstance(rid)) so one buggy instance is
	// quarantined, not its whole prototype. Empty => the chokepoint derives the per-(kind,ref)
	// shared key from the chunk origin (the right scope for a shared ability/affect/formula/policy).
	breakerKey string

	// display marks a DISPLAY-surface render (renderDisplaySheet/renderDisplayList): a content template
	// assembling a sheet FOR a viewer, not a mechanics cascade. It flips the PERCEPTION-vs-MECHANICS behavior
	// of a handle traversal that legitimately differs between the two — today only hContents: raw for mechanics
	// (an AoE / room-scoped affect MUST reach a hidden entity), but canSee-filtered when a display reads the
	// contents of a container that is NOT the viewer (a room, a chest), so a sheet can't reveal what the viewer
	// can't perceive (#250 — the "reach for contents() instead of occupants()" leak trap). `actor` is the viewer.
	display bool
	// displayRoom is the viewer's room CAPTURED at the start of a display render — the STABLE anchor
	// displayReachesForeignRoom (#253) uses to decide "the viewer's own room" vs "a neighbor". It must NOT be
	// read live from actor.location: a display template can call self:teleport()/move()/recall() mid-render, so
	// a live anchor would let it relocate the viewer into a neighbor, enumerate it, and relocate back (a TOCTOU
	// scry the security review PoC'd). Frozen here, the anchor can't be shifted by anything the render does.
	// Set only on the display path; nil elsewhere (a direct-invocation test falls back to the live location).
	displayRoom *Entity
}

// newLuaRuntime builds the per-zone VM and installs the restricted-globals sandbox. The
// returned runtime's LState has ONLY the allowlisted globals (PHASE7-PLAN.md §2.1); the
// full DROP set is absent (never registered, not deleted — T1/T2/T14). seed makes the
// per-zone RNG deterministic.
func newLuaRuntime(zoneID string, log *slog.Logger) *luaRuntime {
	// SkipOpenLibs: the VM starts with NO standard library opened — the safe base. We then
	// register ONLY the allowlist by hand (NOT lua.OpenBase, which bundles load/require/
	// get/setmetatable/rawset/_G/... back in — T1/T14). The Options caps are the build-time
	// recursion/value-stack bounds (T4).
	L := lua.NewState(lua.Options{
		CallStackSize:   luaCallStackSize,
		RegistrySize:    luaRegistrySize,
		RegistryMaxSize: luaRegistryMaxSize,
		SkipOpenLibs:    true,
	})

	rt := &luaRuntime{
		L:             L,
		rng:           rand.New(rand.NewSource(seedFromZoneID(zoneID))), //nolint:gosec // gameplay determinism, not security
		log:           log.With("subsystem", "lua"),
		chunks:        map[string]*chunkCacheEntry{},
		entityScripts: map[RuntimeID]*entityScript{},
		breakers:      map[string]*breakerState{},
	}

	rt.installSandbox()
	return rt
}

// close tears down the VM. Called on zone stop, on the zone goroutine.
func (rt *luaRuntime) close() {
	if rt == nil || rt.L == nil {
		return
	}
	rt.L.Close()
	rt.L = nil
}

// seedFromZoneID derives a stable 64-bit seed from the zone id so a zone's RNG is
// reproducible across runs (T9). A simple FNV-1a-style hash — determinism, not security.
func seedFromZoneID(zoneID string) int64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(zoneID); i++ {
		h ^= uint64(zoneID[i])
		h *= 1099511628211
	}
	return int64(h)
}

// installSandbox replaces the VM's globals with ONLY the allowlist and installs the private
// capped string metatable. After this returns, a fresh script's _ENV (the global table) has
// no path to any dropped capability (T1/T2/T13/T14).
//
// Construction order matters: we harvest the real stdlib closures into a scratch global
// table first (so pairs/ipairs keep their auxiliary upvalues, format/find keep their real
// implementations to wrap), then build the sandbox global table from the allowlist, then
// swap it in and override the string metatable.
func (rt *luaRuntime) installSandbox() {
	L := rt.L

	// Step 1: harvest. Open base/string/table/math onto a scratch state's globals so we get
	// the genuine closures (pairs/ipairs carry upvalues we can't reconstruct by hand, and
	// the capped wrappers delegate to the real format/find/match/gsub/concat). We open onto
	// THIS state's globals, then overwrite the globals wholesale in step 3 — nothing from the
	// raw open survives into the script-visible environment except the closures we copy.
	lua.OpenBase(L)
	lua.OpenString(L)
	lua.OpenTable(L)
	lua.OpenMath(L)

	raw := L.Get(lua.GlobalsIndex).(*lua.LTable)
	rawString := raw.RawGetString("string").(*lua.LTable)
	rawTable := raw.RawGetString("table").(*lua.LTable)
	rawMath := raw.RawGetString("math").(*lua.LTable)

	// Step 2: build the private capped string metatable (T14). This table — NOT the
	// script-visible `string` proxy — is what method syntax ("x"):rep() dispatches through,
	// because we install it as builtinMts[LTString]. It holds the capped amplifier wrappers
	// plus the safe passthroughs, and points __index at itself (the string-lib convention).
	rt.stringMeta = rt.buildCappedStringTable(rawString)
	rt.stringMeta.RawSetString("__index", rt.stringMeta)
	// SetMetatable on a string value writes builtinMts[LTString] — the exported seam. After
	// this, ("x"):rep(n) resolves rep through stringMeta's capped wrapper, and a sibling
	// script setting `string.rep = evil` (on the read-only proxy, which itself blocks it)
	// cannot change it. No Lua value references stringMeta (it is never set as a global).
	L.SetMetatable(lua.LString(""), rt.stringMeta)

	// The script-visible `string` namespace is a SEPARATE read-only proxy table holding the
	// SAME capped wrappers, so `string.rep("A", n)` is also capped and the table is a
	// different object from the metatable (T14 separation).
	rt.stringProxy = rt.buildCappedStringTable(rawString)
	rt.stringProxy = rt.readOnly(rt.stringProxy)

	// Step 3: build the sandbox global table from the allowlist (T1/T2). We start a FRESH
	// table and copy in ONLY kept entries — an unsafe capability is ABSENT, not hidden
	// (deletion would be defeatable by _G/_ENV aliasing).
	env := L.NewTable()

	// Kept base functions (registered individually — never OpenBase, PHASE7-PLAN.md §2.1). The name set is
	// the SHARED allowlist (#47, luasandbox.BaseAllowlist) so the zone and director sandboxes cannot diverge.
	for _, name := range luasandbox.BaseAllowlist {
		if fn := raw.RawGetString(name); fn != lua.LNil {
			env.RawSetString(name, fn)
		}
	}
	// print -> mud.log stub (structured). The full mud table lands in 7.3b; here print is the
	// only redirect needed and proves the log path.
	env.RawSetString("print", L.NewFunction(rt.luaPrint))

	// Kept tables.
	env.RawSetString("string", rt.stringProxy)
	env.RawSetString("table", rt.buildCappedTableTable(rawTable))
	env.RawSetString("math", rt.buildMathTable(rawMath))

	// Swap the sandbox env in as the VM globals. Everything OpenBase/OpenString/... put on the
	// old globals (load, require, get/setmetatable, rawset, _G, os, io, coroutine, channel,
	// string.dump, ...) is now unreachable — the script sees only `env`. We do NOT register
	// `_ENV`/`_G`: gopher-lua is Lua 5.1 (no _ENV upvalue semantics), and exposing the globals
	// table by name would only re-add a self-reference a script could enumerate; the allowlist
	// is reached directly as globals.
	rt.setGlobals(env)

	// Register the entity-handle userdata type (luahandle.go, slice 7.2): its method table +
	// the pointer-safe __tostring (T15). The metatable lives in the registry, not the globals,
	// so it survives the setGlobals swap and is never script-reachable as a global. Read-only
	// handles — no effect ops / harm surface this slice.
	rt.installHandleType()

	// Register the reaction-context userdata type (luareact.go, slice 7.9): the typed `rx`
	// handed to a result-altering reaction hook at the Phase-6 checkpoints. Like the handle it
	// carries a pointer-safe __tostring (T15) and an engine-owned metatable never exposed as a
	// script global; a script only ever receives an `rx` the engine binds at a checkpoint.
	rt.installReactType()

	// Register the `mud` world/util table (luamud.go, slice 7.3b): RNG/clock/log/scan/
	// broadcast/spawn/after/pvp_allowed — the NON-HARM world API + the timer-handle userdata
	// type (with its pointer-safe __tostring, T15). No harm surface this slice.
	rt.installMudTable()

	// Register the `world` + `region` scope-read tables (luascope.go, Phase 10.3b): the cached,
	// synchronous reads of the director-owned region/world state replica. world.flag/world.get and
	// region:get — reads are local; the write surface (signal_region/signal_world) is 10.3c.
	rt.installScopeTables()

	// Register the `ui` sheet toolkit (luaui.go): the pure-formatting layout engine (dividers,
	// banners, rows, spans) content display templates render sheets with. No world state / no harm.
	rt.installUITable()

	// Register the `screen` builder (luascreen.go, #31): the trusted full-screen/ANSI capability. Curated
	// SAFE-BY-CONSTRUCTION primitives (clear/at/color/write) render to a bounded ANSI subset and flush to a
	// player as one Screen frame — no raw-bytes surface, so no report-query/OSC/exfil sequence is reachable.
	rt.installScreenTable()

	// Register the `gmcp` custom-frame handle (luagmcp.go, #51): gmcp.send(player, pkg, table) emits a
	// content-namespaced (Mud.*) GMCP frame to a rich client, guarded by a fail-closed namespace allowlist
	// (content can't spoof Char.*/Core.*) + a bounded payload encoder. The gate's outbound support filter
	// still applies, so a client that didn't advertise the package stays silent.
	rt.installGMCPTable()

	// Arm the default per-call budgets (T3/T4). The per-call re-arm chokepoint is slice 7.5;
	// arming once here makes the abort path live and testable now.
	L.SetInstructionBudget(luaInstrBudget)
	L.ResetInstructionCount()
}

// setGlobals points the VM's globals table at env. gopher-lua keeps the globals at
// GlobalsIndex in the registry; replacing it makes every subsequent global read/write hit
// the sandbox table. We copy env's entries onto the existing globals table rather than
// swapping the pointer, because internal references (the registry's _G slot) hold the
// original; clearing the original and repopulating it from the allowlist is the robust,
// pointer-stable way to guarantee NOTHING from the raw open survives.
func (rt *luaRuntime) setGlobals(env *lua.LTable) {
	L := rt.L
	g := L.Get(lua.GlobalsIndex).(*lua.LTable)
	// Clear every existing key (the raw stdlib globals) by setting it nil.
	var keys []lua.LValue
	g.ForEach(func(k, _ lua.LValue) { keys = append(keys, k) })
	for _, k := range keys {
		L.SetTable(g, k, lua.LNil)
	}
	// Repopulate from the allowlist only.
	env.ForEach(func(k, v lua.LValue) { g.RawSet(k, v) })
}

// readOnly wraps tbl in a new table whose __index proxies reads to tbl and whose __newindex
// rejects all writes — so script code cannot mutate the `string`/`table`/`math` namespaces.
// NOTE this guard is sufficient for the NAMESPACE tables (a script reaching `string.rep`),
// but is NOT what protects method syntax — that is the unreachable private metatable (T14).
func (rt *luaRuntime) readOnly(tbl *lua.LTable) *lua.LTable {
	L := rt.L
	proxy := L.NewTable()
	mt := L.NewTable()
	mt.RawSetString("__index", tbl)
	mt.RawSetString("__newindex", L.NewFunction(func(l *lua.LState) int {
		l.RaiseError("attempt to modify a read-only table")
		return 0
	}))
	// __metatable is a decoy string so getmetatable (absent anyway) couldn't reach the real
	// mt; harmless belt-and-suspenders.
	mt.RawSetString("__metatable", lua.LString("locked"))
	L.SetMetatable(proxy, mt)
	return proxy
}

// buildCappedStringTable returns a fresh table holding the capped amplifier wrappers (T13)
// plus the safe passthrough string functions copied from raw. The DROP entries (notably
// `dump` — string.dump serializes bytecode) are simply never copied.
func (rt *luaRuntime) buildCappedStringTable(raw *lua.LTable) *lua.LTable {
	L := rt.L
	t := L.NewTable()

	// Safe passthroughs (no amplification): copy the genuine closures.
	for _, name := range []string{"byte", "char", "len", "lower", "upper", "sub", "reverse"} {
		if fn := raw.RawGetString(name); fn != lua.LNil {
			t.RawSetString(name, fn)
		}
	}

	// Capped amplifiers (T13). rep is implemented directly (guard before any allocation);
	// the others delegate to the real implementation AFTER a guard.
	rawFormat := raw.RawGetString("format")
	rawGsub := raw.RawGetString("gsub")
	rawFind := raw.RawGetString("find")
	rawMatch := raw.RawGetString("match")
	rawGmatch := raw.RawGetString("gmatch")

	t.RawSetString("rep", L.NewFunction(rt.cappedRep))
	t.RawSetString("format", rt.wrapFormat(rawFormat))
	// gsub gets a dedicated OUTPUT cap (matches × replacement length) — its raw builtin
	// builds the whole result in Go with no VM re-entry, so the input cap alone leaves a
	// matches×repl alloc bomb (T13).
	t.RawSetString("gsub", rt.wrapGsub(rawGsub))
	// find/match/gmatch return slices of the (already input-capped) subject, never an
	// amplified result, so the input cap is the correct and sufficient bound for them.
	t.RawSetString("find", rt.wrapPattern(rawFind, "find"))
	t.RawSetString("match", rt.wrapPattern(rawMatch, "match"))
	if rawGmatch != lua.LNil {
		t.RawSetString("gmatch", rt.wrapPattern(rawGmatch, "gmatch"))
	}

	return t
}

// cappedRep is string.rep with the output-size guard (T13). It rejects an n*#s result over
// luaStrByteCap BEFORE the underlying allocation — string.rep("A", 2e9) is a clean error,
// not a multi-GB allocation. Implemented directly (not delegating) so the guard runs before
// any allocation.
func (rt *luaRuntime) cappedRep(L *lua.LState) int {
	s := L.CheckString(1)
	n := L.CheckInt(2)
	if n <= 0 || len(s) == 0 {
		L.Push(lua.LString(""))
		return 1
	}
	// len(s)*n can overflow int; check via division to avoid the multiply.
	if n > luaStrByteCap/len(s) {
		L.RaiseError("string.rep result too large (cap %d bytes)", luaStrByteCap)
		return 0
	}
	L.Push(lua.LString(strings.Repeat(s, n)))
	return 1
}

// wrapFormat caps string.format's OUTPUT before delegating (T13). strFormat builds the whole
// result in a Go builtin with no VM re-entry, so the per-instruction budget/deadline cannot
// interrupt it — the guard must be a complete pre-check. Three amplification vectors are
// bounded: (1) the format string itself (one literal can be huge), (2) explicit field
// width/precision tokens (`%2000000000d` expands a tiny format into a huge result), and —
// the one the input/width checks miss — (3) the SUM of the `%s`/`%q` STRING ARGUMENT byte
// lengths (`("%s"):rep(20)` over twenty 500 KiB args is 10 MB of output the width scan never
// sees). We sum the string args the same way wrapConcat sums element bytes.
func (rt *luaRuntime) wrapFormat(raw lua.LValue) *lua.LFunction {
	L := rt.L
	return L.NewFunction(func(l *lua.LState) int {
		format := l.CheckString(1)
		if len(format) > luaStrByteCap {
			l.RaiseError("string.format format too large (cap %d bytes)", luaStrByteCap)
			return 0
		}
		if w := maxFormatWidth(format); w > luaStrByteCap {
			l.RaiseError("string.format field width too large (cap %d bytes)", luaStrByteCap)
			return 0
		}
		// Sum the byte lengths of every string-valued argument (the %s/%q expansion vector).
		// Start from the format length so a format that is itself near-cap plus a big arg is
		// still caught. Numbers contribute a bounded amount and are ignored here (a number can
		// expand at most via a width token, already checked).
		total := len(format)
		for i := 2; i <= l.GetTop(); i++ {
			if s, ok := l.Get(i).(lua.LString); ok {
				total += len(string(s))
				if total > luaStrByteCap {
					l.RaiseError("string.format result too large (cap %d bytes)", luaStrByteCap)
					return 0
				}
			}
		}
		return rt.callDelegate(l, raw)
	})
}

// wrapGsub caps string.gsub's OUTPUT before delegating (T13). The raw strGsub builds the
// whole result in Go with NO early exit — it accumulates every replacement into one buffer
// and then assembles — so the input-length cap alone leaves a matches×replacement alloc bomb:
// the auditor's `("a"):rep(60000):gsub("a", string.rep("B",100))` churned 370 GB. Input-capped
// at 64 KiB subject × a 1 MiB replacement is up to 64 GiB of output in one uninterruptible
// builtin. The fix bounds the OUTPUT for every replacement kind:
//
//   - STRING replacement: the replacement length is known up front, so the exact worst-case
//     output — len(subject) + (len(subject)+1)*replLen (at most len(subject)+1 matches) — is
//     rejected before the raw gsub runs.
//   - FUNCTION/TABLE replacement: the per-match value is not knowable up front and the raw
//     builtin offers no per-match hook, so we substitute a GUARDED replacement function
//     (outputGuardedFunc) that runs the real lookup/callback, tracks the CUMULATIVE returned
//     bytes, and raises a clean error the instant the running total would exceed
//     luaStrByteCap. This bounds total output regardless of match count or per-call work — a
//     match-count cap alone is insufficient (4096 matches × a 1 MiB value is still 4 GiB).
func (rt *luaRuntime) wrapGsub(raw lua.LValue) *lua.LFunction {
	L := rt.L
	return L.NewFunction(func(l *lua.LState) int {
		subject := l.CheckString(1)
		if len(subject) > luaPatternInputCap {
			l.RaiseError("string.gsub input too large (cap %d bytes)", luaPatternInputCap)
			return 0
		}
		repl := l.CheckAny(3)
		switch r := repl.(type) {
		case lua.LString:
			// Known replacement length: bound the exact worst-case output up front.
			replLen := len(string(r))
			maxMatches := len(subject) + 1
			// out = len(subject) + maxMatches*replLen; check the product without overflow.
			if replLen > 0 && maxMatches > (luaStrByteCap-len(subject))/replLen {
				l.RaiseError("string.gsub result too large (cap %d bytes)", luaStrByteCap)
				return 0
			}
		case *lua.LFunction:
			// Wrap the callback so cumulative output is bounded (see outputGuardedFunc). Replace
			// arg 3 with the guard, preserving any arg-4 limit the script passed.
			rt.replaceGsubRepl(l, rt.outputGuardedFunc(rt.callLuaFuncRepl(r)))
		case *lua.LTable:
			// Wrap the table lookup the same way — a guarded function that reads the script's
			// table for each match and tracks cumulative output.
			rt.replaceGsubRepl(l, rt.outputGuardedFunc(rt.tableLookupRepl(r)))
		}
		return rt.callDelegate(l, raw)
	})
}

// replaceGsubRepl rewrites the gsub call on the stack so argument 3 (the replacement) becomes
// guarded, preserving arguments 1, 2 and any 4 (the limit). The stack is [subj, pat, repl,
// limit?]; we rebuild it with guarded in slot 3.
func (rt *luaRuntime) replaceGsubRepl(l *lua.LState, guarded *lua.LFunction) {
	subj := l.Get(1)
	pat := l.Get(2)
	var limit lua.LValue
	if l.GetTop() >= 4 {
		limit = l.Get(4)
	}
	l.SetTop(0)
	l.Push(subj)
	l.Push(pat)
	l.Push(guarded)
	if limit != nil {
		l.Push(limit)
	}
}

// outputGuardedFunc wraps a per-match replacement producer in a Lua function that tracks the
// CUMULATIVE bytes returned across all matches and raises a clean error the moment the total
// would exceed luaStrByteCap (T13). This is what bounds a FUNCTION/TABLE gsub's total output —
// the only universal bound, since per-match values are script-controlled and the raw builtin
// has no early-exit. The closure's counter lives for the duration of the one gsub call.
func (rt *luaRuntime) outputGuardedFunc(produce func(l *lua.LState) lua.LValue) *lua.LFunction {
	var total int
	return rt.L.NewFunction(func(l *lua.LState) int {
		v := produce(l)
		if s, ok := v.(lua.LString); ok {
			total += len(string(s))
		} else if n, ok := v.(lua.LNumber); ok {
			total += len(lua.LNumber(n).String())
		}
		if total > luaStrByteCap {
			l.RaiseError("string.gsub result too large (cap %d bytes)", luaStrByteCap)
			return 0
		}
		l.Push(v)
		return 1
	})
}

// callLuaFuncRepl returns a producer that invokes the script's replacement function with the
// current match arguments (already on the guard's stack) and returns its first result.
func (rt *luaRuntime) callLuaFuncRepl(fn *lua.LFunction) func(l *lua.LState) lua.LValue {
	return func(l *lua.LState) lua.LValue {
		nargs := l.GetTop()
		l.Push(fn)
		for i := 1; i <= nargs; i++ {
			l.Push(l.Get(i))
		}
		l.Call(nargs, 1)
		ret := l.Get(-1)
		l.Pop(1)
		return ret
	}
}

// tableLookupRepl returns a producer that looks up the script's replacement table by the
// current match (the first capture / whole match), mirroring strGsubTable semantics.
func (rt *luaRuntime) tableLookupRepl(tbl *lua.LTable) func(l *lua.LState) lua.LValue {
	return func(l *lua.LState) lua.LValue {
		key := l.Get(1) // the whole match or first capture, supplied by gsub
		return l.GetTable(tbl, key)
	}
}

// wrapPattern caps the INPUT subject length of find/match/gmatch (T13 — backtracking). These
// return slices of the (input-capped) subject, never an amplified result, so the input cap is
// the correct bound. gsub is NOT routed here — it amplifies (matches × replacement) and gets
// the dedicated output cap in wrapGsub. The subject is argument 1. Over-cap input is a clean
// error.
func (rt *luaRuntime) wrapPattern(raw lua.LValue, name string) *lua.LFunction {
	L := rt.L
	return L.NewFunction(func(l *lua.LState) int {
		subject := l.CheckString(1)
		if len(subject) > luaPatternInputCap {
			l.RaiseError("string.%s input too large (cap %d bytes)", name, luaPatternInputCap)
			return 0
		}
		return rt.callDelegate(l, raw)
	})
}

// callDelegate forwards the current call's arguments to the wrapped raw function and returns
// its results. It is the shared tail of the capped wrappers: the guard already ran, now run
// the genuine implementation with the same arguments. It snapshots the arguments (the
// wrapper's stack [1..top]), pushes the raw function then the args, runs with MultRet, and
// returns the number of results — which now occupy the stack region the args did.
func (rt *luaRuntime) callDelegate(l *lua.LState, raw lua.LValue) int {
	nargs := l.GetTop()
	args := make([]lua.LValue, nargs)
	for i := 1; i <= nargs; i++ {
		args[i-1] = l.Get(i)
	}
	base := l.GetTop() // == nargs; results land at base..top after Call consumes fn+args
	l.Push(raw)
	for _, a := range args {
		l.Push(a)
	}
	l.Call(nargs, lua.MultRet)
	return l.GetTop() - base
}

// buildCappedTableTable returns a fresh `table` namespace with concat capped (T13) and the
// rest of the safe table functions passed through. Returned read-only.
func (rt *luaRuntime) buildCappedTableTable(raw *lua.LTable) *lua.LTable {
	L := rt.L
	t := L.NewTable()
	for _, name := range []string{"insert", "remove", "sort", "getn", "maxn"} {
		if fn := raw.RawGetString(name); fn != lua.LNil {
			t.RawSetString(name, fn)
		}
	}
	// unpack lives on table in 5.2+ (we also keep the global `unpack`).
	if fn := raw.RawGetString("unpack"); fn != lua.LNil {
		t.RawSetString("unpack", fn)
	}
	rawConcat := raw.RawGetString("concat")
	t.RawSetString("concat", rt.wrapConcat(rawConcat))
	return rt.readOnly(t)
}

// wrapConcat caps table.concat's output size (T13). It sums the byte lengths of the array
// part (plus separators) and rejects an over-cap total BEFORE delegating to the real concat.
func (rt *luaRuntime) wrapConcat(raw lua.LValue) *lua.LFunction {
	L := rt.L
	return L.NewFunction(func(l *lua.LState) int {
		tbl := l.CheckTable(1)
		sep := l.OptString(2, "")
		i := l.OptInt(3, 1)
		j := l.OptInt(4, tbl.Len())
		var total int
		for k := i; k <= j; k++ {
			v := tbl.RawGetInt(k)
			switch vv := v.(type) {
			case lua.LString:
				total += len(string(vv))
			case lua.LNumber:
				total += 24 // generous upper bound on a formatted number
			default:
				// non-string/number => the real concat errors; let it, after the cap check.
			}
			total += len(sep)
			if total > luaStrByteCap {
				l.RaiseError("table.concat result too large (cap %d bytes)", luaStrByteCap)
				return 0
			}
		}
		return rt.callDelegate(l, raw)
	})
}

// buildMathTable returns a fresh `math` namespace: every safe math function passed through,
// `random` REBOUND to the per-zone RNG (T9/P7-D4), `randomseed` a NO-OP (no entropy reset).
// Returned read-only.
func (rt *luaRuntime) buildMathTable(raw *lua.LTable) *lua.LTable {
	L := rt.L
	t := L.NewTable()
	raw.ForEach(func(k, v lua.LValue) {
		name, ok := k.(lua.LString)
		if !ok {
			return
		}
		switch string(name) {
		case "random", "randomseed":
			// handled below
		default:
			t.RawSet(k, v)
		}
	})
	t.RawSetString("random", L.NewFunction(rt.luaMathRandom))
	t.RawSetString("randomseed", L.NewFunction(func(*lua.LState) int { return 0 })) // no-op (T9)
	return rt.readOnly(t)
}

// luaMathRandom mirrors Lua 5.1 math.random semantics but draws from the per-zone seeded RNG
// (T9): no args -> [0,1); one arg m -> [1,m]; two args m,n -> [m,n].
func (rt *luaRuntime) luaMathRandom(L *lua.LState) int {
	switch L.GetTop() {
	case 0:
		L.Push(lua.LNumber(rt.rng.Float64()))
	case 1:
		m := L.CheckInt(1)
		if m < 1 {
			L.RaiseError("bad argument #1 to 'random' (interval is empty)")
			return 0
		}
		L.Push(lua.LNumber(rt.rng.Intn(m) + 1))
	default:
		m := L.CheckInt(1)
		n := L.CheckInt(2)
		if m > n {
			L.RaiseError("bad argument #2 to 'random' (interval is empty)")
			return 0
		}
		L.Push(lua.LNumber(rt.rng.Intn(n-m+1) + m))
	}
	return 1
}

// luaPrint is the print -> mud.log redirect (structured). Concatenates its arguments with a
// space (Lua print semantics) and logs at info. The full mud.log with levels lands in 7.3b;
// here it proves the redirect and the bare-engine log path.
func (rt *luaRuntime) luaPrint(L *lua.LState) int {
	n := L.GetTop()
	parts := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		parts = append(parts, L.ToStringMeta(L.Get(i)).String())
	}
	rt.log.Info("lua print", "msg", strings.Join(parts, " "))
	return 0
}

// maxFormatWidth scans a printf-style format string for the largest explicit numeric field
// width (e.g. the 2000000000 in "%2000000000d"). Used to reject a width that would expand a
// tiny format into a huge result in one builtin call (T13). Returns 0 if none.
func maxFormatWidth(format string) int {
	largest := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		// skip flags
		for i < len(format) && strings.IndexByte("-+ #0", format[i]) >= 0 {
			i++
		}
		// read width digits
		start := i
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			i++
		}
		if i > start {
			w := 0
			for _, c := range format[start:i] {
				w = w*10 + int(c-'0')
				if w > luaStrByteCap { // saturate; we only compare against the cap
					break
				}
			}
			if w > largest {
				largest = w
			}
		}
		// a precision (.N) can also expand %s/%f; treat it the same as a width.
		if i < len(format) && format[i] == '.' {
			i++
			pstart := i
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				i++
			}
			if i > pstart {
				p := 0
				for _, c := range format[pstart:i] {
					p = p*10 + int(c-'0')
					if p > luaStrByteCap {
						break
					}
				}
				if p > largest {
					largest = p
				}
			}
		}
	}
	return largest
}

// runChunk compiles and runs a Lua source string in the sandbox, returning any error. It is
// a STANDALONE runner for slice 7.1 — proving the VM boots and runs sandboxed code. The
// full entry-point invocation chokepoint (fresh per-call context + budget re-arm + pcall +
// the circuit breaker) is slice 7.4/7.5; this runner arms a single fresh deadline + resets
// the instruction count so the abort paths are exercised now.
func (rt *luaRuntime) runChunk(name, src string) error {
	if rt == nil || rt.L == nil {
		return fmt.Errorf("lua runtime not initialized")
	}
	L := rt.L
	fn, err := L.Load(strings.NewReader(src), name)
	if err != nil {
		return fmt.Errorf("lua compile %s: %w", name, err)
	}
	// Route THE chokepoint (pcallGuarded). runChunk is the standalone runner (slice 7.1 / tests);
	// it is NOT breaker-tracked (empty scriptKey) — the breaker tracks the entry-point invoke*
	// paths, not the bare runner. The deadline + instruction count + spawn budget are armed inside
	// pcallGuarded, the SOLE place SetContext+PCall live.
	return rt.runGuardedFn("", name, fn, 0, lua.MultRet)
}

// contextWithLuaDeadline returns a fresh per-call wall-clock deadline context (T3 layer 2),
// the one place the deadline duration is set so every Lua-entry chokepoint (runChunk, callFn)
// arms an identical fresh deadline. The caller MUST defer the returned cancel.
func contextWithLuaDeadline() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), luaCallDeadline)
}
