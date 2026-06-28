package gate

import (
	"strings"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/world"
)

// reconnect_roundtrip_test.go is the BLACK-BOX (through the gate) Wave-1 regression for the
// PERSISTENCE round-trip and the single-session takeover contract.
//
// TestReconnectRoundTripPreservesMovedRoom drives the FULL player journey the Phase-4 milestone
// names — connect, CHANGE durable state IN-SESSION (walk to a new room), disconnect, reconnect,
// state intact — end to end through the gate, NOT by pre-seeding the durable row. It is the
// in-session twin of TestReconnectLandsInSavedRoom (which pre-seeds the saved room): here the
// saved room is produced by the player's own move + the logout flush, so it exercises the WRITE
// half of the durability ladder (move -> quit-flush) AND the read half (reconnect -> rehydrate)
// in one journey. A regression in either half (the flush loses the moved room, or the load ignores
// it and dumps the player at the start room) fails this.
func TestReconnectRoundTripPreservesMovedRoom(t *testing.T) {
	const addr = "addr-a"
	store := world.NewMemStore()
	// Pre-create the durable row at the START room so the first login takes the LOAD path (the row
	// exists), then the player walks AWAY from it — the reconnect must land in the WALKED-TO room,
	// proving the move + logout flush updated the durable record, not that the start room happened
	// to match.
	if _, err := store.CreateCharacter(t.Context(), "Roundtrip", "midgaard", "midgaard:room:temple"); err != nil {
		t.Fatalf("seed durable record: %v", err)
	}

	h := newHarness(t)
	sh := world.NewShard("midgaard", addr, nil, nil).WithPersistence(store, nil)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})

	// --- Session 1: log in at the temple, WALK to the market (a durable state change), quit. ---
	s1 := h.dial(t)
	s1.login(t, "Roundtrip")
	s1.expect(t, "Temple Square")
	s1.send(t, "north")
	s1.expect(t, "Market Square") // the player is now in a room OTHER than where they started.
	s1.send(t, "quit")
	s1.expect(t, "Farewell.")
	s1.close(t)

	// The logout flush must record the moved room durably before the reconnect reads it. Poll the
	// store (the async saver writes in ms on the quit path) so the reconnect below is race-free.
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, ok, _ := store.LoadCharacter(t.Context(), "Roundtrip")
		if ok && snap.RoomRef == "midgaard:room:market" {
			break
		}
		if time.Now().After(deadline) {
			snap, _, _ := store.LoadCharacter(t.Context(), "Roundtrip")
			t.Fatalf("logout flush did not record the moved room: durable room=%q, want midgaard:room:market", snap.RoomRef)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// --- Session 2: reconnect. The player must rehydrate into the MOVED room, not the start room. ---
	s2 := h.dial(t)
	s2.login(t, "Roundtrip")
	s2.expect(t, "Market Square")
	if got := s2.acc.String(); strings.Contains(got, "Temple Square") {
		t.Fatalf("reconnect dumped the player at the start room, not the saved (moved) room:\n%s", got)
	}
	// And the reconnected player is LIVE in that room: a command round-trips (CreateCharacter starts
	// appliedSeq at 0 and the move/quit did not bump the DURABLE fence past a fresh session's seqs,
	// so the first input is applied — the reconnect-mute fence reset, TestReconnectResetsInputSeqFence).
	s2.send(t, "say home again")
	s2.expect(t, "You say, 'home again'")
	s2.close(t)
}

// TestSecondLoginTakesOverSession asserts the SINGLE-SESSION CLEAN-KICK contract (FOLLOW-UPS §3): a
// second login for an ALREADY-CONNECTED character does NOT spawn a second body or get rejected — the
// NEWEST connection takes over (zone.go attach `case s != nil`, the !s.detached live-takeover branch)
// and the FIRST connection is CLEANLY KICKED, not left mute:
//
//	(1) the displaced FIRST connection receives a player-visible "logged in elsewhere" NOTICE, and
//	(2) its socket is then CLOSED (the gate renders the Disconnect frame and tears the connection down).
//	(3) the SECOND connection's FIRST command is PROCESSED (the carried-over dedup fence is reset to the
//	    fresh gate session's resume point, so seq 1 is NOT swallowed as a stale replay).
//
// This replaces the OLD wart this test used to pin (first connection left connected-yet-MUTE, second
// connection's first input droppable). The takeover ITSELF (newest wins, one body) still holds.
func TestSecondLoginTakesOverSession(t *testing.T) {
	const addr = "addr-a"
	store := world.NewMemStore()
	h := newHarness(t)
	sh := world.NewShard("midgaard", addr, nil, nil).WithPersistence(store, nil)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})

	// First connection: live in the world. Run a command so the world's dedup fence (appliedSeq)
	// advances PAST a fresh session's first seq — this is what used to swallow the takeover's first
	// input, and what the fence-reset (assertion 3) must defeat.
	first := h.dial(t)
	first.login(t, "Twin")
	first.expect(t, "Temple Square")
	first.send(t, "say one")
	first.expect(t, "You say, 'one'")
	first.send(t, "say two")
	first.expect(t, "You say, 'two'") // appliedSeq is now 2 on the world session.

	// Second connection, SAME character name, while the first is still LIVE. It takes over the session
	// (re-attach) and renders the room — the takeover.
	second := h.dial(t)
	second.login(t, "Twin")
	second.expect(t, "Temple Square")

	// (1) + (2) The displaced FIRST connection gets the clean kick: a visible notice, then its socket
	// closes. expectClose drains trailing bytes into acc so the notice still lands for inspection.
	first.expect(t, "your character logged in from another location")
	first.expectClose(t)

	// (3) The SECOND connection's FIRST command is PROCESSED — NOT dropped against the stale fence. The
	// new gate session starts at seq 1; the world re-attach reset appliedSeq (2 -> 0) to the fresh
	// resume point, so this single send round-trips immediately (no retry loop).
	second.send(t, "say takeover")
	second.expect(t, "You say, 'takeover'")

	second.close(t)
}

// TestTakeoverResetsInputFence isolates assertion (3) of the clean-kick contract: the input-seq fence
// reset on a live takeover. It drives the prior session's dedup high-water (appliedSeq) up with SEVERAL
// commands, then takes over with a fresh second login and asserts the new connection's VERY FIRST input
// (gate seq 1) is applied on the FIRST try — proving the carried-over fence was clamped to the fresh
// session's resume point, not left at the stale high-water (which would have swallowed the first N
// inputs as replays). A regression that drops the fence-reset fails here: the single first send is
// deduped and never echoes.
func TestTakeoverResetsInputFence(t *testing.T) {
	const addr = "addr-a"
	store := world.NewMemStore()
	h := newHarness(t)
	sh := world.NewShard("midgaard", addr, nil, nil).WithPersistence(store, nil)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})

	// Push the first session's appliedSeq well above 1 (four applied commands), so a fresh session's
	// seq 1..4 would ALL be deduped without the fence reset.
	first := h.dial(t)
	first.login(t, "Fencer")
	first.expect(t, "Temple Square")
	for _, w := range []string{"alpha", "bravo", "charlie", "delta"} {
		first.send(t, "say "+w)
		first.expect(t, "You say, '"+w+"'")
	}

	// Take over with a brand-new gate session (its input seq restarts at 1).
	second := h.dial(t)
	second.login(t, "Fencer")
	second.expect(t, "Temple Square")
	first.expectClose(t) // the displaced connection is kicked.

	// The fresh session's FIRST input (seq 1) must be applied immediately — one send, it echoes.
	second.send(t, "say clean")
	second.expect(t, "You say, 'clean'")

	second.close(t)
}
