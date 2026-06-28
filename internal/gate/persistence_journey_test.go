package gate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/world"
)

// TestReconnectLandsInSavedRoom is the persistence-milestone player journey in its
// in-process (layer 2) form: the ROADMAP Phase 4 "a character + world state survive a
// restart", reduced to the connect/reconnect-to-the-same-room smoke the role notes
// should exist EARLY.
//
// It pins the READ half of the durability ladder end to end through the gate: a player
// whose durable record places them in Market Square (a room OTHER than the start room)
// reconnects, and the gate + world rehydrate them into THAT saved room — not the Temple
// start room. We pre-seed the durable record (a character that "was last there") so the
// test exercises the load-on-login -> land-in-saved-room path deterministically, without
// depending on the asynchronous logout-flush timing (which has a known race, pinned
// separately in TestQuitFlushLostOnDetachRace below).
//
// If persistence regressed (load ignored, room reset to start), the join look would
// render "Temple Square" and this fails.
func TestReconnectLandsInSavedRoom(t *testing.T) {
	const addr = "addr-a"
	store := world.NewMemStore()
	// The durable record: this character was last in Market Square. A fresh login must
	// rehydrate them THERE. (CreateCharacter mints the row at version 0; the gate's login
	// takes the LOAD path, not create, because the row already exists.)
	if _, err := store.CreateCharacter(context.Background(), "Persephone", "midgaard", "midgaard:room:market"); err != nil {
		t.Fatalf("seed durable record: %v", err)
	}

	h := newHarness(t)
	sh := world.NewShard("midgaard", addr, nil, nil).WithPersistence(store, nil)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.login(t, "Persephone")

	// The milestone: the join look renders the SAVED room (Market Square), not the start
	// room. expect proves Market Square arrived; the guard below proves the start room's
	// text did NOT (the player was not dumped at temple).
	term.expect(t, "Market Square")
	if got := term.acc.String(); strings.Contains(got, "Temple Square") {
		t.Fatalf("reconnect rendered the start room, not the saved room:\n%s", got)
	}

	// And the returning player is LIVE in that room: a command round-trips. (This also
	// guards that the load path did not leave the session in a half-attached state.)
	//
	// NOTE: a returning persisted character whose saved appliedSeq is > 0 would be MUTE
	// for their first inputs (see TestReconnectInputSeqMuteBug) — so we seed appliedSeq=0
	// here (CreateCharacter starts at 0) precisely so this liveness check is meaningful.
	term.send(t, "say back again")
	term.expect(t, "You say, 'back again'")

	term.close(t)
}

// TestReconnectResetsInputSeqFence pins the FIX for the reconnect-mute bug this stabilization
// pass surfaced.
//
// THE BUG (now fixed): the world dedups input by a high-water mark (appliedSeq): an input whose
// seq <= appliedSeq is dropped as a replay (zone.go — the handoff/link-death exactly-once
// mechanism). loadCharacter used to RESTORE appliedSeq from the durable snapshot on a fresh login.
// But appliedSeq is SESSION-scoped: the gate mints a fresh session whose input seq restarts at 1.
// So a persisted player whose saved appliedSeq was N reconnected MUTE — the world dropped their
// first N inputs (seq 1..N) as phantom replays.
//
// THE FIX (character.go): a fresh-login / crash-rehydrate is a new session, so loadCharacter resets
// the fence to 0; the handoff/resume paths (same session) preserve it separately. This test seeds a
// durable record with appliedSeq = 5 and asserts the returning player's FIRST input is applied (it
// echoes) — i.e. the stale durable fence does not mute them.
func TestReconnectResetsInputSeqFence(t *testing.T) {
	const addr = "addr-a"
	store := world.NewMemStore()
	if _, err := store.CreateCharacter(context.Background(), "Mutebug", "midgaard", "midgaard:room:market"); err != nil {
		t.Fatalf("seed durable record: %v", err)
	}
	// Bump the saved appliedSeq above the fresh gate session's first seqs. SaveCharacter
	// CASes on version 0 (the just-created row), so this is a deterministic durable edit.
	snap, _, err := store.LoadCharacter(context.Background(), "Mutebug")
	if err != nil {
		t.Fatal(err)
	}
	snap.State.AppliedSeq = 5
	if _, ok, err := store.SaveCharacter(context.Background(), snap); err != nil || !ok {
		t.Fatalf("seed appliedSeq: ok=%v err=%v", ok, err)
	}

	h := newHarness(t)
	sh := world.NewShard("midgaard", addr, nil, nil).WithPersistence(store, nil)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.login(t, "Mutebug")
	term.expect(t, "Market Square") // rehydrated into the saved room

	// The fresh gate session's first line is seq 1. With the fence reset to 0 on login, it is
	// APPLIED (not dropped against the stale saved appliedSeq of 5): the returning player is not mute.
	term.send(t, "say echo-check")
	term.expect(t, "You say, 'echo-check'")

	term.close(t)
}

// TestQuitFlushReliableAfterMove pins the FIX for a persistence race this pass surfaced: a
// player's logout flush was INTERMITTENTLY lost, leaving the durable record at its pre-move
// location.
//
// THE RACE (now fixed): a move is not itself a flush point (only cadence/logout/drain are), so a
// player who walks then quits relies on the LOGOUT flush to record their new room. On quit the
// world sets quitting=true; the gate closes the socket; the world's stream-reader posts detachMsg.
// detach() flushes only IF it sees quitting=true — else it took the 60s link-death grace path with
// NO flush, deferring (effectively losing) the moved room. The detach vs quit-input ordering raced,
// so ~1-in-5 the moved room was not persisted promptly.
//
// THE FIX (zone.go detach): the link-death branch now enqueues a durable flush BEFORE scheduling the
// grace reap — so the player's current state is persisted immediately on ANY unexpected drop,
// regardless of whether `quitting` was observed first. This closes the race (the flush is reliable)
// and also hardens against a shard crash during the grace window losing a walked-to room.
//
// The test walks the player to a new room, quits, and asserts the durable record reliably reflects
// the moved room within a tight window (a hard assertion now — no skip).
func TestQuitFlushReliableAfterMove(t *testing.T) {
	const addr = "addr-a"
	store := world.NewMemStore()
	if _, err := store.CreateCharacter(context.Background(), "Quitter", "midgaard", "midgaard:room:temple"); err != nil {
		t.Fatalf("seed durable record: %v", err)
	}

	h := newHarness(t)
	sh := world.NewShard("midgaard", addr, nil, nil).WithPersistence(store, nil)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.login(t, "Quitter")
	term.expect(t, "Temple Square")
	term.send(t, "north")
	term.expect(t, "Market Square")
	term.send(t, "quit")
	term.expect(t, "Farewell.")
	term.close(t)

	// The flush must reliably land on the moved room (market) — promptly, well within the 60s grace.
	// Poll a tight window; the async saver writes in milliseconds on either teardown path now.
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, ok, _ := store.LoadCharacter(t.Context(), "Quitter")
		if ok && snap.RoomRef == "midgaard:room:market" {
			return // flushed the moved room, as required
		}
		if ok && snap.RoomRef != "midgaard:room:temple" && snap.RoomRef != "midgaard:room:market" {
			t.Fatalf("logout flush recorded the WRONG room: %q, want market", snap.RoomRef)
		}
		if time.Now().After(deadline) {
			snap, _, _ := store.LoadCharacter(t.Context(), "Quitter")
			t.Fatalf("logout flush did not record the moved room within deadline: durable room=%q, want market", snap.RoomRef)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
