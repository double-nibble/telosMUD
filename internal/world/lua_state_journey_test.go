package world

import (
	"context"
	"testing"

	"github.com/double-nibble/telosmud/internal/content"
	lua "github.com/yuin/gopher-lua"
)

// lua_state_journey_test.go — Wave-3 end-to-end self.state PERSISTENCE journey (docs/TEST-COVERAGE.md
// Area 3, item 1). luastate_test.go pins the 7.6 marshaller by calling dumpCharacter/loadCharacter
// DIRECTLY (single-threaded). This drives the SAME self.state through the REAL durability ladder on a
// RUNNING shard: a player runs a Lua command that mutates their self.state, logs out (the async saver
// flushes the Script subtree to the in-memory store), and a fresh login REHYDRATES it — proving the
// "content authors write Lua that remembers things across logins" promise end to end, not just the
// marshal round-trip.

// TestPlayerLuaStateSurvivesLogoutReloginLadder runs a player's Lua quest counter through the full
// login -> mutate-via-Lua -> logout-flush -> relogin-rehydrate ladder on a live shard.
//
// The quest counter is written by REAL Lua: a custom `quest` command whose body calls an injected
// builtin (__quest_tick) that bumps the actor's self.state.count on the zone goroutine — the faithful
// "an ability/quest hook writes player self.state" path (custom commands bind `self` but not `state`,
// so the engine-side quest hook is the realistic writer). The counter then survives the async logout
// flush + the fresh-login reload, exactly as a quest's progress must.
func TestPlayerLuaStateSurvivesLogoutReloginLadder(t *testing.T) {
	mem := NewMemStore()
	shard := NewDemoShard().WithPersistence(mem, mem)
	z := shard.Zone()

	// BEFORE the zone goroutine starts (so the LState writes are race-free), register a custom `quest`
	// command and inject the Go builtin it calls. __quest_tick bumps self.state.count on the actor it
	// is handed and RETURNS the new count — run on the zone goroutine during dispatch, the real
	// single-writer path. The command echoes the count so the test reads it from the player's output
	// stream (no off-goroutine self.state peek needed).
	registerCustomCommand(z.defs, content.CommandDTO{
		Verb: "quest",
		Lua:  `local n = __quest_tick(self); self:send("Quest count: "..n)`,
	})
	z.lua.L.SetGlobal("__quest_tick", z.lua.L.NewFunction(func(l *lua.LState) int {
		e := resolveHandle(l, 1)
		if e == nil {
			l.Push(lua.LNumber(-1))
			return 1
		}
		st := z.lua.ensureStateTable(e)
		cur := 0
		if n, ok := st.RawGetString("count").(lua.LNumber); ok {
			cur = int(n)
		}
		cur++
		st.RawSetString("count", lua.LNumber(cur))
		l.Push(lua.LNumber(cur))
		return 1
	}))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go shard.Run(ctx)

	// --- First life: log in fresh, advance the quest TWICE via the Lua command, log out. ---
	out := login(t, shard, z, "Questor")
	waitRow(t, mem, "Questor")
	waitEntityPID(t, z, "Questor")
	drainChan(out)

	sendInput(z, "Questor", "quest")
	waitFrame(t, out, "Quest count: 1")
	sendInput(z, "Questor", "quest")
	waitFrame(t, out, "Quest count: 2")

	// Log out: the clean leave flushes durably (async). Wait until the durable row carries a non-empty
	// Script subtree (the self.state was marshalled) before the relogin reads it.
	quit(t, z, "Questor")
	snap := waitRowWhere(t, mem, "Questor", func(s CharSnapshot) bool {
		return len(s.State.Script) > 0
	})
	if len(snap.State.Script) == 0 {
		t.Fatal("logout flush did not persist the player's self.state (no Script subtree)")
	}

	// --- Second life ("restart"): a fresh login rehydrates from the store alone. The quest counter
	// must have survived at 2, so a THIRD advance reads it as 3 — proving the rehydrated self.state is
	// LIVE, not a fresh zero. If the Script subtree had been lost on the flush or ignored on the load,
	// the counter would restart and this advance would read "Quest count: 1". ---
	out2 := login(t, shard, z, "Questor")
	waitFrame(t, out2, "Temple") // rehydrated into the start room (default), live
	drainChan(out2)

	sendInput(z, "Questor", "quest")
	// The decisive assertion: the rehydrated counter continues from 2 (not restarts at 1).
	waitFrame(t, out2, "Quest count: 3")
}
