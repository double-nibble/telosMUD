package world

import (
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// traverse_test.go — #370: named exits (a content exit keyword typed as a movement verb) and the
// cancellable `traverse` hook (a pre-commit gate that can block/message/redirect a move).

// traverseZone builds a two-room zone A --north--> B (and A --gate--> B via a NAMED exit `gate`), with an
// optional `traverse`/other Lua block on room A. Returns the zone, both rooms, and a joined player in A.
func traverseZone(t *testing.T, roomALua string) (*Zone, *Entity, *Entity, *session) {
	t.Helper()
	z := newZone("tz")
	roomA := z.newEntity("tz:room:a")
	// `north` is a compass exit; `gate` is a NAMED exit (no registered verb) — both lead to B.
	Add(roomA, &Room{exits: map[string]ProtoRef{"north": "tz:room:b", "gate": "tz:room:b"}})
	if roomALua != "" {
		Add(roomA, &Scripted{source: roomALua})
	}
	z.rooms["tz:room:a"] = roomA
	roomB := z.newEntity("tz:room:b")
	Add(roomB, &Room{exits: map[string]ProtoRef{"south": "tz:room:a"}})
	z.rooms["tz:room:b"] = roomB
	z.startRoom = "tz:room:a"

	s := &session{character: "Walker", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Walker")
	Move(s.entity, roomA)
	z.players["Walker"] = s
	return z, roomA, roomB, s
}

// sessionSaw reports whether any queued output frame contains substr (draining the channel).
func sessionSaw(s *session, substr string) bool {
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
				return true
			}
		default:
			return false
		}
	}
}

// --- named-exit dispatch (slice 1) ----------------------------------------------------------

// TestNamedExitTraversal proves a content exit keyword is typeable as a movement verb: typing `gate`
// (which has no registered verb) traverses the exit, exactly like a compass direction.
func TestNamedExitTraversal(t *testing.T) {
	z, _, roomB, s := traverseZone(t, "")

	z.dispatch(s, "gate") // a NAMED exit — not a registered verb, not a compass dir

	if s.entity.location != roomB {
		t.Fatal("typing the named exit `gate` did not traverse it to room B")
	}
}

// TestNamedExitUnknownStillHuh proves the fall-through is EXACT-match on a real exit only: a word that is
// neither a verb nor an exit key still yields "Huh?" (no accidental traversal).
func TestNamedExitUnknownStillHuh(t *testing.T) {
	z, roomA, _, s := traverseZone(t, "")

	z.dispatch(s, "grove") // not a verb, not an exit of room A

	if s.entity.location != roomA {
		t.Fatal("an unknown word moved the player")
	}
	if !sessionSaw(s, "Huh?") {
		t.Fatal("an unknown non-exit word did not produce Huh?")
	}
}

// TestNamedExitDoesNotShadowCompass proves a compass abbreviation still wins: `n` resolves to the north
// verb (registered), never treated as a named-exit lookup.
func TestNamedExitDoesNotShadowCompass(t *testing.T) {
	z, _, roomB, s := traverseZone(t, "")
	z.dispatch(s, "n") // the north abbreviation
	if s.entity.location != roomB {
		t.Fatal("the `n` compass abbreviation did not move the player north")
	}
}

// TestDemoWardedSanctumGuard drives the ACTUAL shipped demo content (darkwood sanctum) so a runtime error
// in its `traverse` Lua — or a regression in named-exit dispatch or the hook — is caught here. An unproven
// player typing the named exit `warded` is blocked by the spectral warden and stays put; a `proven` player
// passes into the inner sanctum. The ungated `west` exit is unaffected either way.
func TestDemoWardedSanctumGuard(t *testing.T) {
	z := newDemoZone("darkwood", newProtoCache())
	sanctum := z.rooms["darkwood:room:sanctum"]
	inner := z.rooms["darkwood:room:innersanctum"]
	if sanctum == nil || inner == nil {
		t.Fatal("demo darkwood is missing the sanctum / inner sanctum rooms")
	}

	s := &session{character: "Seeker", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Seeker")
	Move(s.entity, sanctum)
	z.players["Seeker"] = s

	// Unproven: the named exit `warded` is blocked by the warden; the player stays in the sanctum.
	z.dispatch(s, "warded")
	if s.entity.location != sanctum {
		t.Fatal("an unproven player passed the warded gate (the traverse hook did not block, or errored)")
	}
	if !sessionSaw(s, "spectral warden") {
		t.Fatal("the warden block message was not shown")
	}

	// Prove them, then `warded` lets them through to the inner sanctum.
	setFlag(s.entity, "proven", true)
	z.dispatch(s, "warded")
	if s.entity.location != inner {
		t.Fatal("a proven player was still blocked from the inner sanctum")
	}
}

// --- the cancellable traverse hook (slice 2) ------------------------------------------------

// TestTraverseHookBlocksWithMessage is the canonical guard example: a `traverse` hook that calls
// block("msg") on a specific exit cancels the move and shows the message; the player stays put.
func TestTraverseHookBlocksWithMessage(t *testing.T) {
	lua := `on("traverse", function(ev)
		if ev.exit == "north" then block("A grizzled guard steps in front of you.") end
	end)`
	z, roomA, _, s := traverseZone(t, lua)

	z.dispatch(s, "north")

	if s.entity.location != roomA {
		t.Fatal("the traverse hook did not block the move — the player left the room")
	}
	if !sessionSaw(s, "A grizzled guard steps in front of you.") {
		t.Fatal("the block message was not shown to the mover")
	}
}

// TestTraverseHookBlocksNamedExit proves the hook composes with named-exit dispatch: blocking the `gate`
// keyword stops a named-exit traversal too.
func TestTraverseHookBlocksNamedExit(t *testing.T) {
	lua := `on("traverse", function(ev)
		if ev.exit == "gate" then block("The gate is barred.") end
	end)`
	z, roomA, _, s := traverseZone(t, lua)

	z.dispatch(s, "gate")

	if s.entity.location != roomA {
		t.Fatal("the traverse hook did not block the named-exit traversal")
	}
	if !sessionSaw(s, "The gate is barred.") {
		t.Fatal("the block message was not shown")
	}
}

// TestTraverseHookReturnFalseBlocks proves a bare `return false` blocks with the engine default message.
func TestTraverseHookReturnFalseBlocks(t *testing.T) {
	z, roomA, _, s := traverseZone(t, `on("traverse", function(ev) return false end)`)

	z.dispatch(s, "north")

	if s.entity.location != roomA {
		t.Fatal("returning false from the traverse hook did not block the move")
	}
	if !sessionSaw(s, "You can't go that way.") {
		t.Fatal("the default block message was not shown")
	}
}

// TestTraverseHookAllowsUngatedExit proves the hook only affects what it blocks: a hook gating `north`
// lets a DIFFERENT exit (`gate`) through untouched.
func TestTraverseHookAllowsUngatedExit(t *testing.T) {
	lua := `on("traverse", function(ev)
		if ev.exit == "north" then block("no") end
	end)`
	z, _, roomB, s := traverseZone(t, lua)

	z.dispatch(s, "gate") // not the gated exit

	if s.entity.location != roomB {
		t.Fatal("the traverse hook blocked an exit it should not have")
	}
}

// TestTraverseHookNoHandlerAllows proves a scripted room WITHOUT a traverse handler (only other triggers)
// allows movement — the hook is opt-in and fails open.
func TestTraverseHookNoHandlerAllows(t *testing.T) {
	z, _, roomB, s := traverseZone(t, `on("enter", function(ev) state.n = 1 end)`)
	z.dispatch(s, "north")
	if s.entity.location != roomB {
		t.Fatal("a room with no traverse handler blocked a move")
	}
}

// TestTraverseHookErrorFailsOpen proves an ERRORING traverse handler ALLOWS the move (fail-open): a buggy
// gate must never imprison a player.
func TestTraverseHookErrorFailsOpen(t *testing.T) {
	z, _, roomB, s := traverseZone(t, `on("traverse", function(ev) error("boom") end)`)
	z.dispatch(s, "north")
	if s.entity.location != roomB {
		t.Fatal("an erroring traverse handler blocked the move (should fail open)")
	}
}

// TestTraverseHookGatesCrossZoneExit proves the hook fires BEFORE the transfer branch, so it can gate a
// CROSS-ZONE exit too (the destination names a zone this shard does not host). Blocking it keeps the player
// in place with no transfer attempted.
func TestTraverseHookGatesCrossZoneExit(t *testing.T) {
	z := newZone("tz")
	roomA := z.newEntity("tz:room:a")
	Add(roomA, &Room{exits: map[string]ProtoRef{"out": "faraway:room:x"}}) // a cross-zone exit
	Add(roomA, &Scripted{source: `on("traverse", function(ev) block("The way is sealed by a ward.") end)`})
	z.rooms["tz:room:a"] = roomA
	z.startRoom = "tz:room:a"

	s := &session{character: "Walker", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Walker")
	Move(s.entity, roomA)
	z.players["Walker"] = s

	z.dispatch(s, "out")

	if s.entity.location != roomA {
		t.Fatal("the traverse hook did not gate the cross-zone exit (fired after the transfer branch?)")
	}
	if !sessionSaw(s, "sealed by a ward") {
		t.Fatal("the cross-zone block message was not shown")
	}
}

// TestTraverseHookRedirect proves the redirect(dir) primitive: a hook that calls redirect("warp") on the
// `north` attempt sends the mover through the `warp` exit instead — so they land at C, NOT the original
// destination B. The engine performs the redirected move (no harm-gated teleport needed).
func TestTraverseHookRedirect(t *testing.T) {
	z := newZone("tz")
	roomA := z.newEntity("tz:room:a")
	// `north` is the exit the player attempts; `warp` is a second exit to C the hook redirects them through.
	Add(roomA, &Room{exits: map[string]ProtoRef{"north": "tz:room:b", "warp": "tz:room:c"}})
	z.rooms["tz:room:a"] = roomA
	roomB := z.newEntity("tz:room:b")
	Add(roomB, &Room{exits: map[string]ProtoRef{"south": "tz:room:a"}})
	z.rooms["tz:room:b"] = roomB
	roomC := z.newEntity("tz:room:c")
	Add(roomC, &Room{exits: map[string]ProtoRef{}})
	z.rooms["tz:room:c"] = roomC
	// On attempting `north`, the room redirects the mover through `warp` (to C) instead.
	Add(roomA, &Scripted{source: `on("traverse", function(ev)
		if ev.exit == "north" then redirect("warp") end
	end)`})
	z.startRoom = "tz:room:a"

	s := &session{character: "Walker", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Walker")
	Move(s.entity, roomA)
	z.players["Walker"] = s

	z.dispatch(s, "north")

	if s.entity.location == roomB {
		t.Fatal("the redirect did not take effect — the player reached the ORIGINAL destination B")
	}
	if s.entity.location != roomC {
		t.Fatalf("the player was not redirected to C (at %v)", s.entity.location)
	}
}

// TestTraverseHookRedirectCannotReachInstanceEntrance is the #435-invariant regression guard for the
// redirect primitive (security review F1): a `traverse` hook that redirect()s toward an instance ENTRANCE
// key must NOT reach requestInstanceEntry — entrances are reachable ONLY by the player's own typed direction
// (the depth-0 move), never a content-initiated redirect recursion. The redirect misses `exits`, the entrance
// branch is skipped (not the player-typed move), and the player is refused with the ordinary message.
func TestTraverseHookRedirectCannotReachInstanceEntrance(t *testing.T) {
	z := newZone("tz")
	roomA := z.newEntity("tz:room:a")
	// `north` is a real exit; `crypt` is an instance ENTRANCE (a dungeon door), not an ordinary exit.
	r := &Room{
		exits:     map[string]ProtoRef{"north": "tz:room:b"},
		entrances: map[string]string{"crypt": "somedungeon"},
	}
	Add(roomA, r)
	// The hook tries to shove the mover through the dungeon door via redirect.
	Add(roomA, &Scripted{source: `on("traverse", function(ev)
		if ev.exit == "north" then redirect("crypt") end
	end)`})
	z.rooms["tz:room:a"] = roomA
	roomB := z.newEntity("tz:room:b")
	Add(roomB, &Room{exits: map[string]ProtoRef{"south": "tz:room:a"}})
	z.rooms["tz:room:b"] = roomB
	z.startRoom = "tz:room:a"

	s := &session{character: "Walker", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Walker")
	Move(s.entity, roomA)
	z.players["Walker"] = s

	z.dispatch(s, "north")

	if s.instanceMintPending {
		t.Fatal("a redirect toward an instance entrance reached requestInstanceEntry (#435 invariant violated)")
	}
	if s.entity.location != roomA {
		t.Fatalf("the mover should have been refused (stayed in A), but is at %v", s.entity.location)
	}
	if !sessionSaw(s, "can't go that way") {
		t.Fatal("the refused redirect-to-entrance did not yield the ordinary 'can't go that way' message")
	}
}

// TestTraverseHookRedirectLoopBounded proves the redirect budget: a hook that redirects UNCONDITIONALLY (so
// every re-attempt redirects again) is refused after maxTraverseRedirects hops rather than recursing forever,
// and the mover stays put.
func TestTraverseHookRedirectLoopBounded(t *testing.T) {
	z := newZone("tz")
	roomA := z.newEntity("tz:room:a")
	Add(roomA, &Room{exits: map[string]ProtoRef{"north": "tz:room:b", "gate": "tz:room:b"}})
	z.rooms["tz:room:a"] = roomA
	roomB := z.newEntity("tz:room:b")
	Add(roomB, &Room{exits: map[string]ProtoRef{"south": "tz:room:a"}})
	z.rooms["tz:room:b"] = roomB
	// A pathological hook: it always redirects to the OTHER exit, so north->gate->north->... never settles.
	Add(roomA, &Scripted{source: `on("traverse", function(ev)
		if ev.exit == "north" then redirect("gate") else redirect("north") end
	end)`})
	z.startRoom = "tz:room:a"

	s := &session{character: "Walker", out: make(chan *playv1.ServerFrame, 256), epoch: 1}
	z.newPlayerEntity(s, "Walker")
	Move(s.entity, roomA)
	z.players["Walker"] = s

	z.dispatch(s, "north")

	if s.entity.location != roomA {
		t.Fatal("a redirect loop escaped the budget and moved the player")
	}
	if !sessionSaw(s, "turned around") {
		t.Fatal("the redirect-budget-exhausted message was not shown")
	}
}
