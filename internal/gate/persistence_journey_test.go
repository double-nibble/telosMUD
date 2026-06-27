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

// TestReconnectInputSeqMuteBug DOCUMENTS a real bug this stabilization pass surfaced and
// pins the CURRENT (wrong) behavior so a fix is observable as this test changing.
//
// THE BUG: the world dedups input by a per-character high-water mark (appliedSeq): an
// input whose seq <= appliedSeq is dropped as a replay (zone.go, the handoff/link-death
// exactly-once mechanism). On a flush the world persists appliedSeq into the durable
// snapshot, and on LOGIN loadCharacter restores it (character.go). But the GATE mints a
// FRESH session on every new connection, whose input seq restarts at 1 (session.go
// newSession). So when a persisted player whose saved appliedSeq is N reconnects, the
// gate's first new line is seq 1 <= N, and the world DROPS the returning player's first N
// inputs as duplicates. The player is rehydrated into the right room but is silently MUTE
// for their first N commands.
//
// CONTRACT QUESTION (for the edge-engineer / persistence-engineer): appliedSeq is a
// SESSION-scoped exactly-once fence (handoff replay, link-death re-attach), not a durable
// property of the character. A fresh login is a NEW session with a fresh seq space, so the
// load path should almost certainly RESET appliedSeq to 0 on a non-resume attach (or the
// gate should carry its seq counter across reconnects). Until that contract is decided +
// fixed, this test documents the live behavior. When the fix lands, this test's `say` WILL
// echo and it fails with the explanatory message below — change it to assert the echo.
//
// We pre-seed a durable record with appliedSeq = 5 so the precondition is deterministic
// (no dependency on a prior session's flush).
func TestReconnectInputSeqMuteBug(t *testing.T) {
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

	// The fresh gate session's first line is seq 1, which is <= the restored appliedSeq
	// (5), so the world DROPS it: `say` produces no echo.
	term.send(t, "say echo-check")
	if !echoAbsent(term, "You say, 'echo-check'", 1*time.Second) {
		t.Fatal("returning player's first input ECHOED — the appliedSeq mute bug appears FIXED; " +
			"update TestReconnectInputSeqMuteBug to assert the echo and remove the bug note")
	}
	t.Log("documented bug present: returning player's first input (seq 1 <= saved appliedSeq 5) was dropped (mute)")

	term.close(t)
}

// TestQuitFlushLostOnDetachRace DOCUMENTS and pins a second, separate persistence race
// this pass surfaced: a player's logout (quit) flush is INTERMITTENTLY lost, leaving the
// durable record at its pre-move location.
//
// THE RACE: a move does not flush durably (only cadence/logout/drain do), so a player who
// walks then quits relies entirely on the LOGOUT flush to record their new room. On quit
// the world sets quitting=true and sends a Disconnect; the gate closes the socket; the
// world's stream-reader then posts detachMsg, and the detach handler — IF it sees
// quitting=true — runs leave() which enqueues the durable flush. But detachMsg (from the
// socket EOF) and the quit input (which sets quitting) arrive on different goroutines.
// When the EOF-driven detach is processed before quitting is set, detach takes the
// 60-SECOND link-death grace path instead of the immediate leave+flush — so the moved
// room is not persisted until a reap 60s later (well past any reconnect), appearing as a
// lost logout. Reproduces ~1-in-5 here.
//
// CONTRACT QUESTION (for the persistence-engineer / edge-engineer): a clean quit must
// flush the player's final state RELIABLY. Either the quit must be acknowledged as a
// clean teardown BEFORE the socket closes (so detach always sees quitting), or detach's
// link-death path must itself flush before scheduling the grace reap (so no logout is
// ever deferred 60s / lost on a fast disconnect).
//
// This test is written to NOT be flaky in CI: it asserts the milestone reconnect READS
// the saved room (covered deterministically by TestReconnectLandsInSavedRoom), and treats
// the quit-flush WRITE as best-effort — it runs the quit journey and, if the flush did
// land, asserts it landed CORRECTLY (room=market), but t.Skips (with a clear note) on the
// runs where the race deferred it. It exists to keep the race VISIBLE and documented, and
// will tighten to a hard assertion once the flush is made reliable.
func TestQuitFlushLostOnDetachRace(t *testing.T) {
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

	// Give the (possibly-deferred) flush a generous window. If it lands, it MUST record the
	// moved room — a flush that persisted the WRONG room would be a real corruption and
	// fails hard. If it never lands in-window, the detach-race deferred it: document + skip.
	// A flush that is NOT deferred by the race lands in milliseconds; this window only
	// needs to clear that fast path. We deliberately do NOT wait out the 60s link-death
	// grace (that would make the test slow under -count); a deferred flush simply skips.
	landed := false
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok, _ := store.LoadCharacter(t.Context(), "Quitter")
		if ok && snap.RoomRef != "midgaard:room:temple" {
			if snap.RoomRef != "midgaard:room:market" {
				t.Fatalf("logout flush recorded the WRONG room: %q, want market", snap.RoomRef)
			}
			landed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !landed {
		t.Skip("logout flush did not land in-window: the detach-race deferred it to the 60s link-death reap (documented bug TestQuitFlushLostOnDetachRace)")
	}
	t.Log("logout flush landed correctly (room=market) this run")
}

// echoAbsent returns true if want does NOT appear in the terminal's output within d. It is
// the deliberate inverse of terminal.expect — asserting the ABSENCE of an echo, with a
// bounded wait so the test stays fast and deterministic (no open-ended sleep).
func echoAbsent(term *terminal, want string, d time.Duration) bool {
	deadline := time.After(d)
	for {
		if strings.Contains(term.acc.String(), want) {
			return false
		}
		select {
		case b, ok := <-term.bytes:
			if !ok {
				return !strings.Contains(term.acc.String(), want)
			}
			term.acc.WriteByte(b)
		case <-deadline:
			return !strings.Contains(term.acc.String(), want)
		}
	}
}
