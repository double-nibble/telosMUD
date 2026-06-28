package world

import (
	"context"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/content"
	lua "github.com/yuin/gopher-lua"
)

// lua_sandbox_journey_test.go — Wave-3 BLACK-BOX safety journeys for the Phase 7 Lua sandbox
// (docs/TEST-COVERAGE.md Area 3). The unit tests (luabreaker/luaentry/luastate) drive the budget,
// breaker, and marshaller directly and SINGLE-THREADED (z.pulses.tick / runChunk). These tests
// instead drive a LIVE, RUNNING zone goroutine (go z.Run) through the real command inbox and assert
// the load-bearing safety claim the sandbox exists for: a hostile/buggy content script cannot WEDGE
// or CRASH the zone — a SECOND player on the same zone keeps playing through the incident.
//
// The probe that makes "the zone survived" a hard assertion: zoneHasPlayer posts a presenceMsg and
// blocks on its reply, which is processed ON the zone goroutine. A wedged (hung) or dead (panicked-
// out-of-loop) zone never replies, so the probe's deadline trips — there is no false green.

// runningSandboxZone builds a single running zone with a custom Lua command `verb` whose body is
// `lua`, plus two logged-in players, and returns the zone + each player's out channel. The zone runs
// on its own goroutine (cancelled at cleanup), so commands flow through the REAL inbox -> dispatch ->
// dispatchSafe path exactly as the gRPC server drives them.
func runningSandboxZone(t *testing.T, verb, luaBody string) (*Zone, map[string]chan *playv1.ServerFrame) {
	t.Helper()
	z := newZone("sbx")
	room := z.newEntity("sbx:room:hall")
	Add(room, &Room{exits: map[string]ProtoRef{}})
	z.rooms["sbx:room:hall"] = room
	z.startRoom = "sbx:room:hall"
	if verb != "" {
		registerCustomCommand(z.defs, content.CommandDTO{Verb: verb, Lua: luaBody})
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go z.Run(ctx)

	outs := map[string]chan *playv1.ServerFrame{}
	for _, name := range []string{"Hammer", "Bystander"} {
		out := make(chan *playv1.ServerFrame, 64)
		s := &session{character: name, out: out, epoch: 1}
		z.newPlayerEntity(s, name)
		z.post(joinMsg{s: s})
		waitPlayer(t, z, name, true)
		outs[name] = out
	}
	return z, outs
}

// TestRunawayLuaCommandDoesNotWedgeZone is the headline runaway-script safety journey: one player
// fires a custom command whose Lua body is a tight pure-CPU infinite loop (`while true do end`). The
// instruction/deadline budget (the sole chokepoint, pcallGuarded) MUST abort it, the command fails
// closed for that player, and — the load-bearing assertion — THE ZONE KEEPS SERVING: a SECOND player
// still gets command responses and the zone goroutine answers a synchronous presence probe.
//
// CONTROLLED-BREAK NOTE: I confirmed locally that REMOVING the budget arming (so the loop is not
// aborted) makes this test HANG at the first probe after the runaway command — i.e. without the
// sandbox the zone goroutine wedges and every player on it is frozen. That hanging variant is NOT
// shipped (a hanging test is a bad test); the budget is what turns the hang into the clean abort this
// asserts. The repeated-runaway loop below ALSO trips the circuit breaker, after which the command is
// quarantined (no-op) — a second layer the assertion implicitly covers (the zone never wedges across
// many runs).
func TestRunawayLuaCommandDoesNotWedgeZone(t *testing.T) {
	z, outs := runningSandboxZone(t, "runaway", `while true do end`)

	// Hammer fires the runaway command repeatedly — each invocation must be budget-aborted, and the
	// repetition drives the breaker toward quarantine. None of these may wedge the zone.
	for i := 0; i < 15; i++ {
		z.post(inputMsg{id: "Hammer", line: "runaway"})
	}

	// THE ZONE SURVIVED: the synchronous presence probe is answered ON the zone goroutine, so a reply
	// proves the loop is alive and processing (a wedged zone hangs here until the probe's deadline).
	if !zoneHasPlayer(z, "Bystander") {
		t.Fatal("the zone goroutine did not answer a presence probe after a runaway Lua command — it WEDGED")
	}

	// And the Bystander is still LIVE: a real command round-trips and renders output. This proves the
	// runaway did not just leave the loop spinning-but-alive; the zone serves new work for other players.
	z.post(inputMsg{id: "Bystander", line: "look"})
	if !waitForOutput(t, outs["Bystander"], "", 2*time.Second) {
		t.Fatal("a second player's `look` produced no output after the runaway command — the zone is not serving")
	}
}

// TestPanicInLuaPathRecoversAndZoneServes drives a GO-LEVEL panic out of the Lua path (a builtin that
// panics in Go — the worst a buggy handle/builtin can do) and asserts the sandbox recovers it, the
// zone survives, and a second player keeps playing. This is distinct from TestZoneRecoversFromHandlerPanic
// (a panic in a CORE message handler, caught by the outer handle() recover): here the panic originates
// INSIDE a Lua invocation. The SOLE chokepoint (pcallGuarded's L.PCall) converts the Go builtin panic
// into an isolated Lua error — the action fizzles, the zone is untouched — and the gopher-lua VM itself
// pcall-wraps builtin calls, so the recovery is DEFENSE-IN-DEPTH: I tried to construct a crashing variant
// (bypass pcallGuarded's PCall AND disable both the dispatchSafe and handle() recovers) and STILL could
// not make this panic escape — a positive resilience finding, but it means the seam is robust enough that
// this test pins the OBSERVABLE (zone survives, blast radius is one command) rather than a single
// breakable line. A Lua *error* would be pcall-caught too; injecting a Go function that PANICS is the
// faithful "a builtin blew up" case a Lua error alone cannot exercise.
func TestPanicInLuaPathRecoversAndZoneServes(t *testing.T) {
	z, outs := runningSandboxZone(t, "boom", `__detonate()`)
	// Inject a Go builtin that PANICS when the Lua body calls it. SetGlobal on the zone's LState is
	// done here before any command runs; the zone goroutine is the only reader during dispatch.
	z.lua.L.SetGlobal("__detonate", z.lua.L.NewFunction(func(*lua.LState) int {
		panic("injected: a builtin blew up inside the Lua path")
	}))

	// Hammer triggers the panic-inducing command. The sandbox (pcallGuarded, backed by the
	// dispatchSafe/handle recovers) must swallow the Go panic, not let it unwind the zone goroutine.
	z.post(inputMsg{id: "Hammer", line: "boom"})

	// THE ZONE SURVIVED the Go panic: the presence probe is answered (the loop did not die).
	if !zoneHasPlayer(z, "Bystander") {
		t.Fatal("the zone goroutine died after a Go panic in the Lua path — the sandbox recover seam failed")
	}
	// The Bystander keeps playing: a fresh command renders output (blast radius was one command).
	z.post(inputMsg{id: "Bystander", line: "look"})
	if !waitForOutput(t, outs["Bystander"], "", 2*time.Second) {
		t.Fatal("a second player's `look` produced no output after a panic in the Lua path — the zone is not serving")
	}
	// And the player who TRIGGERED the panic is not stuck either: their next command works too (the
	// recover unwound just the offending command, not their session).
	z.post(inputMsg{id: "Hammer", line: "look"})
	if !waitForOutput(t, outs["Hammer"], "", 2*time.Second) {
		t.Fatal("the triggering player could not issue a command after the recovered panic — their session was lost")
	}
}

// waitForOutput drains out until a frame carrying Output (optionally containing `want`; "" matches any
// Output) arrives, or the deadline elapses. It is the running-zone analog of the e2e Expect: poll for
// the observable, never sleep-and-hope. Returns true on match.
func waitForOutput(t *testing.T, out chan *playv1.ServerFrame, want string, d time.Duration) bool {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f := <-out:
			if o := f.GetOutput(); o != nil {
				if want == "" || contains([]string{o.GetMarkup()}, want) {
					return true
				}
			}
		case <-deadline:
			return false
		}
	}
}
