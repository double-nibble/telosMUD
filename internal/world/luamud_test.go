package world

import (
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	lua "github.com/yuin/gopher-lua"
)

// luamud_test.go — slice 7.3b gates (docs/PHASE7-PLAN.md, threat rows T2/T6/T9/T15) for the
// `mud` world/util table: determinism (RNG/roll), the deterministic clock, the spawn caps
// (security), the zone-wheel scheduling (T6 — no new goroutine), and the timer-handle
// __tostring (T15).

// --- T9: determinism — seeded zones produce identical RNG/roll streams --------------------

func TestMudRandomDeterministic(t *testing.T) {
	seq := func(zoneID string) []int {
		z := newZone(zoneID)
		var got []int
		z.lua.L.SetGlobal("__push", z.lua.L.NewFunction(func(l *lua.LState) int {
			got = append(got, l.CheckInt(1))
			return 0
		}))
		if err := z.lua.runChunk("rng", `for i=1,8 do __push(mud.random(1,1000)) end`); err != nil {
			t.Fatal(err)
		}
		return got
	}
	a, b, c := seq("alpha"), seq("alpha"), seq("beta")
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same-seed mud.random diverged at %d: %d vs %d", i, a[i], b[i])
		}
	}
	same := true
	for i := range a {
		if a[i] != c[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different zones produced identical mud.random streams (seed not id-dependent)")
	}
}

func TestMudRollDeterministicAndBounded(t *testing.T) {
	roll := func(zoneID string) int {
		z := newZone(zoneID)
		var got int
		z.lua.L.SetGlobal("__r", z.lua.L.NewFunction(func(l *lua.LState) int { got = l.CheckInt(1); return 0 }))
		// mud.roll takes the engine's dice notation (parseDiceSpec): NdS / pools / keep / fudge.
		// A flat +N modifier is a formula concern, not dice notation, so "3d6" not "3d6+2".
		if err := z.lua.runChunk("roll", `__r(mud.roll("3d6"))`); err != nil {
			t.Fatal(err)
		}
		return got
	}
	a, b := roll("z"), roll("z")
	if a != b {
		t.Fatalf("same-seed mud.roll diverged: %d vs %d", a, b)
	}
	if a < 3 || a > 18 { // 3d6 in [3,18]
		t.Fatalf("mud.roll('3d6') = %d, out of [3,18]", a)
	}
	// a malformed spec is a clean error, not a panic.
	z := newZone("z")
	if err := z.lua.runChunk("roll", `mud.roll("not-dice")`); err == nil {
		t.Fatal("malformed mud.roll spec should error")
	}
}

// --- T2/T9: mud.now is the deterministic pulse counter, not wall-clock --------------------

func TestMudNowPulseCounter(t *testing.T) {
	z := newZone("clock")
	var n0 int
	z.lua.L.SetGlobal("__n", z.lua.L.NewFunction(func(l *lua.LState) int { n0 = l.CheckInt(1); return 0 }))
	if err := z.lua.runChunk("now", `__n(mud.now())`); err != nil {
		t.Fatal(err)
	}
	if n0 != 0 {
		t.Fatalf("mud.now at boot = %d, want 0 (pulse counter)", n0)
	}
	// Advancing the wheel advances mud.now monotonically.
	z.pulses.tick()
	z.pulses.tick()
	z.pulses.tick()
	var n1 int
	z.lua.L.SetGlobal("__n", z.lua.L.NewFunction(func(l *lua.LState) int { n1 = l.CheckInt(1); return 0 }))
	if err := z.lua.runChunk("now", `__n(mud.now())`); err != nil {
		t.Fatal(err)
	}
	if n1 != 3 {
		t.Fatalf("mud.now after 3 ticks = %d, want 3 (monotonic pulse, not wall-clock)", n1)
	}
}

// --- T6: mud.after schedules on the zone WHEEL, on the zone goroutine, no new goroutine ----

// TestMudAfterFiresOnWheelOnZoneGoroutine asserts a mud.after callback (1) does not run before
// the wheel ticks, (2) fires when the wheel reaches its pulse, and (3) runs on the SAME
// goroutine that drives the wheel — never a new goroutine (T6). We capture the goroutine id at
// schedule time and at fire time and assert equality.
func TestMudAfterFiresOnWheelOnZoneGoroutine(t *testing.T) {
	z := newZone("wheel")
	rt := z.lua

	var fired atomic.Bool
	var fireGID atomic.Int64
	// A Go function the Lua callback calls, recording that it ran + on which goroutine.
	rt.L.SetGlobal("__mark", rt.L.NewFunction(func(*lua.LState) int {
		fired.Store(true)
		fireGID.Store(goroutineID())
		return 0
	}))

	scheduleGID := goroutineID()
	if err := rt.runChunk("sched", `mud.after(2, function() __mark() end)`); err != nil {
		t.Fatal(err)
	}
	if fired.Load() {
		t.Fatal("mud.after callback fired immediately (should wait for the wheel)")
	}
	// Tick once: not yet due (scheduled at pulse+2).
	z.pulses.tick()
	if fired.Load() {
		t.Fatal("mud.after fired after 1 tick (scheduled for 2)")
	}
	// Tick again: now due.
	z.pulses.tick()
	if !fired.Load() {
		t.Fatal("mud.after did not fire when the wheel reached its pulse")
	}
	if fireGID.Load() != scheduleGID {
		t.Fatalf("mud.after callback ran on goroutine %d, scheduled on %d — must be the same (no new goroutine, T6)",
			fireGID.Load(), scheduleGID)
	}
}

// TestMudCancelStopsCallback asserts mud.cancel prevents a scheduled callback from firing.
func TestMudCancelStopsCallback(t *testing.T) {
	z := newZone("cancel")
	rt := z.lua
	var fired atomic.Bool
	rt.L.SetGlobal("__mark", rt.L.NewFunction(func(*lua.LState) int { fired.Store(true); return 0 }))
	if err := rt.runChunk("sched", `local tm = mud.after(2, function() __mark() end); mud.cancel(tm)`); err != nil {
		t.Fatal(err)
	}
	z.pulses.tick()
	z.pulses.tick()
	z.pulses.tick()
	if fired.Load() {
		t.Fatal("a cancelled mud.after callback fired")
	}
}

// TestMudAfterCallbackIsolated asserts a callback that ERRORS fails just itself (pcall-
// isolated) — the wheel keeps ticking and a later callback still runs (T11 shape; the full
// breaker is 7.5).
func TestMudAfterCallbackIsolated(t *testing.T) {
	z := newZone("iso")
	rt := z.lua
	var good atomic.Bool
	rt.L.SetGlobal("__good", rt.L.NewFunction(func(*lua.LState) int { good.Store(true); return 0 }))
	if err := rt.runChunk("sched", `
		mud.after(1, function() error("boom") end)
		mud.after(1, function() __good() end)
	`); err != nil {
		t.Fatal(err)
	}
	z.pulses.tick() // both due; the first errors (isolated), the second runs
	if !good.Load() {
		t.Fatal("a sibling callback did not run after another errored (isolation failed)")
	}
}

// --- T15: the timer handle carries a pointer-safe __tostring -------------------------------

func TestMudTimerTostringNoPointer(t *testing.T) {
	z := newZone("ts")
	if err := z.lua.runChunk("ts", `
		local tm = mud.after(5, function() end)
		local s = tostring(tm)
		assert(s == "<timer>", "timer tostring = "..s)
		assert(s:find("0x") == nil, "timer tostring leaked a pointer: "..s)
		assert(s:find("userdata") == nil, "timer tostring leaked the userdata tag: "..s)
		mud.cancel(tm)
	`); err != nil {
		t.Fatal(err)
	}
}

// --- security: mud.spawn is bounded -------------------------------------------------------

// TestMudSpawnPerCallCap asserts a single call cannot spawn past luaSpawnPerCallCap — the
// over-cap spawn is a clean error, not a silent drop or an unbounded loop. We register a
// spawnable prototype, then loop past the cap.
func TestMudSpawnPerCallCap(t *testing.T) {
	z, rt, room := mudSpawnZone(t)
	rt.L.SetGlobal("__room", rt.newHandle(room))
	// Spawning exactly the cap succeeds; one more errors.
	err := rt.runChunk("spawn", `
		for i = 1, `+itoa(luaSpawnPerCallCap)+` do
			assert(mud.spawn("spawn:mob:goblin", __room) ~= nil, "spawn "..i.." should succeed")
		end
	`)
	if err != nil {
		t.Fatalf("spawning up to the cap should succeed: %v", err)
	}
	// A fresh call: the per-call budget reset, but the per-zone total is now at the cap's worth;
	// prove the PER-CALL cap by spawning cap+1 in ONE call on a fresh zone.
	z2, rt2, room2 := mudSpawnZone(t)
	rt2.L.SetGlobal("__room", rt2.newHandle(room2))
	err = rt2.runChunk("spawn", `
		for i = 1, `+itoa(luaSpawnPerCallCap+1)+` do mud.spawn("spawn:mob:goblin", __room) end
	`)
	if err == nil {
		t.Fatal("spawning past the per-call cap should error")
	}
	if !strings.Contains(err.Error(), "per-call spawn cap") {
		t.Fatalf("over-cap error should cite the per-call cap, got: %v", err)
	}
	_ = z
	_ = z2
}

// TestMudSpawnPerZoneCap asserts the standing per-zone spawn budget bounds spawns ACROSS calls
// (a script spawning a few per call across many calls cannot exhaust the zone). We spawn in
// repeated small calls until the per-zone cap trips.
func TestMudSpawnPerZoneCap(t *testing.T) {
	_, rt, room := mudSpawnZone(t)
	rt.L.SetGlobal("__room", rt.newHandle(room))
	tripped := false
	// Each call spawns 32; after enough calls the per-zone LIVE census (1024) trips even though no
	// single call exceeds the per-call cap (64) — the mobs never die, so the live count accumulates.
	for call := 0; call < 100; call++ {
		err := rt.runChunk("spawn", `for i=1,32 do mud.spawn("spawn:mob:goblin", __room) end`)
		if err != nil {
			if strings.Contains(err.Error(), "per-zone LIVE spawn cap") {
				tripped = true
				break
			}
			t.Fatalf("unexpected spawn error: %v", err)
		}
	}
	if !tripped {
		t.Fatal("the per-zone LIVE spawn cap never tripped across many calls")
	}
}

// TestMudSpawnUnknownProtoNil asserts spawning an unknown prototype is a clean nil (not a
// counted spawn, not an error).
func TestMudSpawnUnknownProtoNil(t *testing.T) {
	_, rt, room := mudSpawnZone(t)
	rt.L.SetGlobal("__room", rt.newHandle(room))
	if err := rt.runChunk("spawn", `assert(mud.spawn("nope:mob:ghost", __room) == nil)`); err != nil {
		t.Fatal(err)
	}
}

// TestMudSpawnDestinationMustBeRoom asserts the force-inject guard (ISSUE-A): spawning into a
// PLAYER or MOB handle (a non-room destination) is a clean error and NO item appears in their
// inventory.
func TestMudSpawnDestinationMustBeRoom(t *testing.T) {
	z, rt, room := mudSpawnZone(t)
	mob := makeLivingIn(z, room, "ogre")
	s := &session{character: "Victim", out: make(chan *playv1.ServerFrame, 8), epoch: 1}
	z.newPlayerEntity(s, "Victim")
	Move(s.entity, room)
	rt.L.SetGlobal("__mob", rt.newHandle(mob))
	rt.L.SetGlobal("__player", rt.newHandle(s.entity))

	for _, dest := range []string{"__mob", "__player"} {
		err := rt.runChunk("spawn", `mud.spawn("spawn:mob:goblin", `+dest+`)`)
		if err == nil {
			t.Fatalf("spawn into %s (non-room) should error", dest)
		}
		if !strings.Contains(err.Error(), "destination must be a room") {
			t.Fatalf("spawn into %s: error should cite the room requirement, got: %v", dest, err)
		}
	}
	if len(mob.contents) != 0 {
		t.Fatalf("an item was force-injected into the mob (%d contents)", len(mob.contents))
	}
	if len(s.entity.contents) != 0 {
		t.Fatalf("an item was force-injected into the player (%d contents)", len(s.entity.contents))
	}
}

// TestMudSpawnRejectsPlayerProto asserts a player-controlled prototype cannot be spawned
// (ISSUE-A), and the rejection does not consume the spawn budget.
func TestMudSpawnRejectsPlayerProto(t *testing.T) {
	z, rt, room := mudSpawnZone(t)
	z.protos.define("spawn:player:shell", nil, "a hollow shell", "A hollow shell stands here.",
		componentSet{reflect.TypeFor[*PlayerControlled](): &PlayerControlled{}})
	rt.L.SetGlobal("__room", rt.newHandle(room))

	err := rt.runChunk("spawn", `mud.spawn("spawn:player:shell", __room)`)
	if err == nil {
		t.Fatal("spawning a player-controlled prototype should error")
	}
	if !strings.Contains(err.Error(), "player-controlled") {
		t.Fatalf("error should cite the player-proto rejection, got: %v", err)
	}
	if rt.luaSpawnsLive != 0 {
		t.Fatalf("a rejected player-proto spawn consumed the live census (luaSpawnsLive=%d)", rt.luaSpawnsLive)
	}
}

// --- mud.pvp_allowed read-only query ------------------------------------------------------

func TestMudPvpAllowedQuery(t *testing.T) {
	_, rt, room := mudSpawnZone(t)
	// Two mobs (non-players): pvp_allowed is true (the gate only gates player-vs-player).
	a := makeLivingIn(rt.zone, room, "a")
	b := makeLivingIn(rt.zone, room, "b")
	rt.L.SetGlobal("__a", rt.newHandle(a))
	rt.L.SetGlobal("__b", rt.newHandle(b))
	if err := rt.runChunk("pvp", `assert(mud.pvp_allowed(__a, __b) == true, "mob-vs-mob allowed")`); err != nil {
		t.Fatal(err)
	}
}

// TestMudBroadcastSanitizesMarkup asserts mud.broadcast strips control/ESC from script-supplied
// markup at the world layer while preserving legitimate markup (ISSUE-B).
func TestMudBroadcastSanitizesMarkup(t *testing.T) {
	z := newZone("bcast")
	rt := z.lua
	room := z.newEntity("bcast:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["bcast:room:hall"] = room
	s := &session{character: "Reader", out: make(chan *playv1.ServerFrame, 8), epoch: 1}
	z.newPlayerEntity(s, "Reader")
	Move(s.entity, room)
	z.players["Reader"] = s
	rt.L.SetGlobal("__room", rt.newHandle(room))

	if err := rt.runChunk("bcast", `mud.broadcast(__room, "{g}ding\27[2J{x}")`); err != nil {
		t.Fatal(err)
	}
	got := drainText(t, s.out)
	if strings.ContainsRune(got, '\x1b') {
		t.Fatalf("mud.broadcast leaked an ESC rune: %q", got)
	}
	if !strings.Contains(got, "{g}") || !strings.Contains(got, "ding") {
		t.Fatalf("mud.broadcast stripped legitimate markup: %q", got)
	}
}

// --- helpers ------------------------------------------------------------------------------

// mudSpawnZone builds a zone with a registered spawnable goblin prototype and a room, and
// returns the zone, its runtime, and the room entity.
func mudSpawnZone(t *testing.T) (*Zone, *luaRuntime, *Entity) {
	t.Helper()
	z := newZone("spawn-zone")
	// A minimal goblin prototype in the zone's private proto cache.
	z.protos.define("spawn:mob:goblin", nil, "a goblin", "A goblin snarls here.", componentSet{
		reflect.TypeFor[*Living](): &Living{},
	})
	room := z.newEntity("spawn:room:cave")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["spawn:room:cave"] = room
	return z, z.lua, room
}

// makeLivingIn creates a living entity with the given short name, placed in room.
func makeLivingIn(z *Zone, room *Entity, short string) *Entity {
	e := z.newEntity(ProtoRef("test:mob:" + short))
	Add(e, &Living{})
	e.short = short
	Move(e, room)
	return e
}

// itoa is a tiny int->string for building test scripts (avoids importing strconv just here).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// goroutineID returns the current goroutine's id (parsed from the runtime stack header). Test-
// only: used to assert the wheel callback runs on the scheduling goroutine (no new goroutine).
func goroutineID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// "goroutine N [running]:" — parse N.
	s := string(buf[:n])
	s = strings.TrimPrefix(s, "goroutine ")
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return 0
	}
	var id int64
	for _, c := range s[:i] {
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + int64(c-'0')
	}
	return id
}
