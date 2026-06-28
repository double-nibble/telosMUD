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

// TestSecondLoginTakesOverSession pins the SINGLE-SESSION contract observed live: a second login for
// an ALREADY-CONNECTED character does NOT spawn a second body or get rejected — it re-binds (the
// link-dead resume path, zone.go attach `case s != nil`) so the NEWEST connection becomes the live
// one and the FIRST connection is displaced (left mute: its s.out was swapped to the new socket).
//
// REPRODUCE-THEN-ASSERT (the chaos discipline): we first establish BOTH connections are for the same
// character, then assert the observable contract — (1) the second connection is LIVE (a command
// echoes), and (2) the first connection is MUTE (its command does NOT echo, because the world session
// now writes to the second socket). This proves there is effectively ONE live session per character
// and the newest wins.
//
// CONTRACT NOTE (for the edge-engineer / persistence-engineer): today the displaced FIRST connection
// is left silently mute rather than getting a clean "logged in elsewhere" disconnect + socket close.
// A friendlier contract would send the old connection a notice and close its socket. This test pins
// TODAY's behavior (newest-wins, old-goes-mute); update the first-connection assertion when the
// contract is hardened. The takeover ITSELF (newest connection is live, no duplicate body) is the
// invariant that must hold regardless.
func TestSecondLoginTakesOverSession(t *testing.T) {
	const addr = "addr-a"
	store := world.NewMemStore()
	h := newHarness(t)
	sh := world.NewShard("midgaard", addr, nil, nil).WithPersistence(store, nil)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})

	// First connection: live in the world.
	first := h.dial(t)
	first.login(t, "Twin")
	first.expect(t, "Temple Square")

	// Second connection, SAME character name, while the first is still connected. It re-binds the
	// existing session (re-attach) and renders the room — the takeover.
	second := h.dial(t)
	second.login(t, "Twin")
	second.expect(t, "Temple Square")

	// (1) The SECOND connection is LIVE: a command round-trips. The world's input-seq fence may drop
	// the very first input as a stale replay (the re-attach preserves the prior appliedSeq while the
	// fresh gate session restarts at seq 1), so we send a few times and assert the echo lands — the
	// takeover is "the newest connection controls the body", not "the first input is never deduped".
	live := false
	for i := 0; i < 5 && !live; i++ {
		second.send(t, "say takeover")
		live = !echoAbsent(second, "You say, 'takeover'", 500*time.Millisecond)
	}
	if !live {
		t.Fatalf("second login never became live (the takeover did not bind the new connection); output:\n%s", second.acc.String())
	}

	// (2) The FIRST connection is now MUTE: its command does NOT echo back, because the world session
	// writes to the second socket now (single live session; the first was displaced). A regression that
	// left the first connection ALSO live (two bodies / two live sockets for one character) fails here.
	first.send(t, "say still mine")
	if !echoAbsent(first, "You say, 'still mine'", 1*time.Second) {
		t.Fatalf("the displaced FIRST connection is still live — two live sessions for one character (single-session contract broken); output:\n%s", first.acc.String())
	}

	second.close(t)
	first.close(t)
}
