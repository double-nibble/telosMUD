package world

import (
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// luastate_test.go — slice 7.6 tests: the data-only self.state ↔ JSON marshaller (T10, the state-
// injection trust boundary) + the PLAYER persistence round-trip.

// stateZone builds a zone + runtime and returns a fresh self.state table seeded by running a Lua
// snippet that writes into a `state` global bound to a new table.
func buildState(t *testing.T, rt *luaRuntime, src string) *lua.LTable {
	t.Helper()
	st := rt.L.NewTable()
	rt.L.SetGlobal("state", st)
	defer rt.L.SetGlobal("state", lua.LNil)
	if err := rt.runChunk("seed", src); err != nil {
		t.Fatalf("seeding state errored: %v", err)
	}
	return st
}

// --- T10: data-only allowlist (the security core) -----------------------------------------

// TestMarshalDataOnlyRoundTrip asserts a nested data table (numbers/strings/bools/nested tables/
// arrays) round-trips IDENTICALLY through marshal -> JSON -> unmarshal.
func TestMarshalDataOnlyRoundTrip(t *testing.T) {
	z := newZone("st")
	rt := z.lua
	st := buildState(t, rt, `
		state.count = 7
		state.name = "amulet"
		state.done = true
		state.greeted = { ["p1"] = true, ["p2"] = false }
		state.list = { 10, 20, 30 }
		state.nested = { a = { b = { c = "deep" } } }
	`)

	b, err := marshalLuaState(st)
	if err != nil {
		t.Fatalf("marshal errored: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("marshal produced empty bytes for a non-empty state")
	}

	// Unmarshal and deep-compare via re-marshal (a stable canonical form).
	got, err := rt.unmarshalLuaState(b)
	if err != nil {
		t.Fatalf("unmarshal errored: %v", err)
	}
	b2, err := marshalLuaState(got)
	if err != nil {
		t.Fatalf("re-marshal errored: %v", err)
	}
	if string(b) != string(b2) {
		t.Fatalf("round-trip not identical:\n  before %s\n  after  %s", b, b2)
	}
}

// TestMarshalRejectsHandle is the headline T10 test: a self.state carrying a HANDLE (userdata) is
// rejected at save with a CLEAN error NAMING the bad key — no panic, no silent drop of the rest.
func TestMarshalRejectsHandle(t *testing.T) {
	z := newZone("st")
	rt := z.lua
	mob := z.newEntity("st:mob:x")
	Add(mob, &Living{})
	room := z.newEntity("st:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["st:room:hall"] = room
	Move(mob, room)

	st := rt.L.NewTable()
	st.RawSetString("ok", lua.LNumber(1))
	st.RawSetString("bad", rt.newHandle(mob)) // a HANDLE stored in self.state — must reject

	b, err := marshalLuaState(st)
	if err == nil {
		t.Fatal("a self.state carrying a handle must be REJECTED at save")
	}
	if b != nil {
		t.Fatal("a rejected marshal must produce NO bytes (no silent partial persist)")
	}
	if !strings.Contains(err.Error(), "state.bad") {
		t.Fatalf("the rejection must NAME the bad key, got: %v", err)
	}
	if !strings.Contains(err.Error(), "handle") && !strings.Contains(err.Error(), "userdata") {
		t.Fatalf("the rejection should mention the handle/userdata, got: %v", err)
	}
}

// TestMarshalRejectsFunction asserts a function in self.state is rejected (no code persists).
func TestMarshalRejectsFunction(t *testing.T) {
	z := newZone("st")
	rt := z.lua
	st := buildState(t, rt, `state.fn = function() return 1 end`)
	if _, err := marshalLuaState(st); err == nil {
		t.Fatal("a self.state carrying a function must be rejected")
	} else if !strings.Contains(err.Error(), "state.fn") {
		t.Fatalf("the rejection must name the bad key, got: %v", err)
	}
}

// TestMarshalCapsBytes asserts an over-cap (byte) self.state is rejected.
func TestMarshalCapsBytes(t *testing.T) {
	z := newZone("st")
	rt := z.lua
	// A large string (within the per-string rep cap) exceeding the state byte cap.
	st := buildState(t, rt, `state.blob = string.rep("A", 100000)`) // > luaStateMaxBytes
	if _, err := marshalLuaState(st); err == nil {
		t.Fatal("an over-byte-cap self.state must be rejected")
	} else if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("over-cap error should cite the size, got: %v", err)
	}
}

// TestMarshalCapsDepth asserts an over-depth self.state is rejected.
func TestMarshalCapsDepth(t *testing.T) {
	z := newZone("st")
	rt := z.lua
	st := buildState(t, rt, `
		local cur = state
		for i = 1, 30 do          -- > luaStateMaxDepth
			cur.next = {}
			cur = cur.next
		end
		cur.leaf = 1
	`)
	if _, err := marshalLuaState(st); err == nil {
		t.Fatal("an over-depth-cap self.state must be rejected")
	} else if !strings.Contains(err.Error(), "too deep") {
		t.Fatalf("over-depth error should cite depth, got: %v", err)
	}
}

// TestMarshalCapsKeys asserts an over-key-count self.state is rejected. The table is built in Go
// (not via a Lua loop) so the test exercises the marshaller's key cap, not the per-call deadline
// (a 5000-iteration Lua seed loop trips the 5ms deadline under -race).
func TestMarshalCapsKeys(t *testing.T) {
	z := newZone("st")
	st := z.lua.L.NewTable()
	for i := 1; i <= 5000; i++ { // > luaStateMaxKeys
		st.RawSetInt(i, lua.LNumber(i))
	}
	if _, err := marshalLuaState(st); err == nil {
		t.Fatal("an over-key-cap self.state must be rejected")
	} else if !strings.Contains(err.Error(), "too many keys") {
		t.Fatalf("over-key error should cite keys, got: %v", err)
	}
}

// TestMarshalEmptyIsNil asserts an empty state marshals to nil (no Script subtree persisted).
func TestMarshalEmptyIsNil(t *testing.T) {
	z := newZone("st")
	b, err := marshalLuaState(z.lua.L.NewTable())
	if err != nil {
		t.Fatalf("empty state marshal errored: %v", err)
	}
	if b != nil {
		t.Fatalf("an empty state must marshal to nil (no Script), got %s", b)
	}
}

// TestUnmarshalNeverExecutesCode asserts loading a self.state blob produces only PLAIN data —
// strings stay strings, never code. A blob whose value looks like Lua code is just a string.
func TestUnmarshalNeverExecutesCode(t *testing.T) {
	z := newZone("st")
	rt := z.lua
	blob := []byte(`{"payload":"os.exit(1)","n":5}`)
	st, err := rt.unmarshalLuaState(blob)
	if err != nil {
		t.Fatalf("unmarshal errored: %v", err)
	}
	// The payload is a plain STRING, not executed.
	if v, ok := st.RawGetString("payload").(lua.LString); !ok || string(v) != "os.exit(1)" {
		t.Fatalf("payload should load as a plain string, got %v", st.RawGetString("payload"))
	}
	if v, ok := st.RawGetString("n").(lua.LNumber); !ok || float64(v) != 5 {
		t.Fatalf("n should load as 5, got %v", st.RawGetString("n"))
	}
}

// TestUnmarshalPre76Empty asserts a pre-7.6 save (no Script bytes) loads as a fresh empty table.
func TestUnmarshalPre76Empty(t *testing.T) {
	z := newZone("st")
	st, err := z.lua.unmarshalLuaState(nil)
	if err != nil {
		t.Fatalf("nil-script unmarshal errored: %v", err)
	}
	if st == nil || st.Len() != 0 {
		t.Fatal("a pre-7.6 (nil) script blob must load as a fresh empty table")
	}
}

// TestUnmarshalCapsLoadBytes asserts an OVER-BYTE persisted blob (a corrupted/attacker DB row that
// a legit save would have rejected) degrades to an empty table on load — BOUNDED, not a balloon
// (T10 symmetry: the caps hold both ways).
func TestUnmarshalCapsLoadBytes(t *testing.T) {
	z := newZone("st")
	// A valid JSON array exceeding the byte cap.
	big := []byte(`[` + strings.TrimSuffix(strings.Repeat(`"AAAAAAAAAAAAAAAAAAAA",`, 4000), ",") + `]`)
	if len(big) <= luaStateMaxBytes {
		t.Fatalf("test blob is not over the byte cap (%d <= %d)", len(big), luaStateMaxBytes)
	}
	st, err := z.lua.unmarshalLuaState(big)
	if err == nil {
		t.Fatal("an over-byte-cap blob should degrade with an error")
	}
	if !strings.Contains(err.Error(), "over-cap") {
		t.Fatalf("over-cap load error should cite the cap, got: %v", err)
	}
	if st == nil || st.Len() != 0 {
		t.Fatal("an over-cap blob must degrade to a fresh EMPTY table (bounded), not load the giant")
	}
}

// TestUnmarshalCapsLoadDepth asserts a deeply-NESTED blob (whose byte size slips under the byte
// cap) is TRUNCATED at the depth cap on load — never a 5000-deep Lua table.
func TestUnmarshalCapsLoadDepth(t *testing.T) {
	z := newZone("st")
	// A 50-deep nested object: {"n":{"n":{...}}} — small bytes, deep nesting.
	depth := 50
	blob := []byte(strings.Repeat(`{"n":`, depth) + `1` + strings.Repeat(`}`, depth))
	if len(blob) > luaStateMaxBytes {
		t.Fatal("the depth test blob should be UNDER the byte cap (depth, not size, is the vector)")
	}
	st, err := z.lua.unmarshalLuaState(blob)
	if err != nil {
		t.Fatalf("a deep-but-small blob should load (truncated), not error: %v", err)
	}
	// Walk down: the chain must TRUNCATE at luaStateMaxDepth (past that, the next link is nil).
	cur := st
	d := 0
	for cur != nil {
		next, ok := cur.RawGetString("n").(*lua.LTable)
		if !ok {
			break
		}
		cur = next
		d++
		if d > luaStateMaxDepth+2 {
			t.Fatalf("the nested chain exceeded the depth cap (%d) — not truncated on load", luaStateMaxDepth)
		}
	}
	if d > luaStateMaxDepth {
		t.Fatalf("loaded depth %d exceeds the cap %d (not truncated)", d, luaStateMaxDepth)
	}
}

// --- the PLAYER persistence round-trip -----------------------------------------------------

// TestPlayerSelfStateSurvivesLogoutLogin is the headline 7.6 test: a PLAYER's self.state quest
// counter survives logout/login — dumped to JSONB, re-hydrated into a fresh login's entity.
func TestPlayerSelfStateSurvivesLogoutLogin(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())

	// Source login: write a quest counter into the player's self.state.
	src := &session{character: "Quester"}
	se := z.newPlayerEntity(src, "Quester")
	st := z.lua.ensureStateTable(se)
	if st == nil {
		t.Fatal("could not get the player's self.state table")
	}
	st.RawSetString("amulet_step", lua.LNumber(3))
	st.RawSetString("greeted_npc", lua.LBool(true))

	// Dump (logout flush).
	snap := dumpCharacter(src)
	if len(snap.State.Script) == 0 {
		t.Fatal("the dumped snapshot has no Script subtree (self.state not persisted)")
	}

	// Fresh login: load into a NEW entity.
	dst := &session{character: "Quester"}
	z.newPlayerEntity(dst, "Quester")
	loadCharacter(z, dst, snap)

	// The re-hydrated self.state carries the quest counter.
	dst.entity.zone = z
	loaded := z.lua.ensureStateTable(dst.entity)
	if v, ok := loaded.RawGetString("amulet_step").(lua.LNumber); !ok || float64(v) != 3 {
		t.Fatalf("loaded amulet_step = %v, want 3 (self.state survived login)", loaded.RawGetString("amulet_step"))
	}
	if v, ok := loaded.RawGetString("greeted_npc").(lua.LBool); !ok || bool(v) != true {
		t.Fatalf("loaded greeted_npc = %v, want true", loaded.RawGetString("greeted_npc"))
	}
}

// TestPlayerCrashRehydrate asserts the same state survives a CRASH-rehydrate (re-load from the
// persisted JSONB bytes alone, simulating a fresh process loading the durable row).
func TestPlayerCrashRehydrate(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Survivor"}
	se := z.newPlayerEntity(src, "Survivor")
	st := z.lua.ensureStateTable(se)
	st.RawSetString("counter", lua.LNumber(42))
	snap := dumpCharacter(src)

	// Simulate a crash: a brand-new zone (fresh LState) loads the persisted snapshot.
	z2 := newDemoZone("midgaard", newProtoCache())
	dst := &session{character: "Survivor"}
	z2.newPlayerEntity(dst, "Survivor")
	loadCharacter(z2, dst, snap)
	loaded := z2.lua.ensureStateTable(dst.entity)
	if v, ok := loaded.RawGetString("counter").(lua.LNumber); !ok || float64(v) != 42 {
		t.Fatalf("crash-rehydrated counter = %v, want 42", loaded.RawGetString("counter"))
	}
}

// TestPlayerNoScriptStateRoundTrip asserts a player with NO self.state dumps no Script subtree and
// loads cleanly (the common case + backward compat).
func TestPlayerNoScriptStateRoundTrip(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Plain"}
	z.newPlayerEntity(src, "Plain")
	snap := dumpCharacter(src)
	if len(snap.State.Script) != 0 {
		t.Fatalf("a player with no self.state must dump no Script subtree, got %s", snap.State.Script)
	}
	// Load is clean.
	dst := &session{character: "Plain"}
	z.newPlayerEntity(dst, "Plain")
	loadCharacter(z, dst, snap) // must not panic / error
}

// TestPlayerBadStateRejectedAtSave asserts a player whose self.state carries a handle is REJECTED
// at dump (the Script subtree is omitted, logged loudly) — the rest of the character still dumps.
func TestPlayerBadStateRejectedAtSave(t *testing.T) {
	z := newDemoZone("midgaard", newProtoCache())
	src := &session{character: "Buggy"}
	se := z.newPlayerEntity(src, "Buggy")
	st := z.lua.ensureStateTable(se)
	st.RawSetString("ok", lua.LNumber(1))
	st.RawSetString("bad", z.lua.newHandle(se)) // a handle — rejected at dump

	snap := dumpCharacter(src) // must NOT panic
	if len(snap.State.Script) != 0 {
		t.Fatal("a self.state with a handle must NOT be persisted (Script omitted)")
	}
	// The rest of the character still dumped (a valid snapshot, not a crash).
	if snap.Name != "Buggy" {
		t.Fatal("the rest of the character did not dump after a bad self.state")
	}
}
