//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// TestTwoPlayerFanout is the first e2e with TWO simultaneous players. Every other e2e/smoke test is a single
// session; this proves the room-presence fan-out end-to-end through the live gate — a co-located player sees
// another's `say` and movement. Both fresh characters spawn at The Temple Square (same room), so player A's
// say and departure are act(...ToRoom) broadcasts delivered to player B.
func TestTwoPlayerFanout(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	a, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s (A)", addr)
	b, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s (B)", addr)

	// Unique, capitalized names (the act render initial-caps $n; a capitalized name renders unchanged, so the
	// asserted strings are exact). <=20 runes, leading letter, no dot — the gate's login constraints.
	nameA := fmt.Sprintf("Alpha%d", time.Now().UnixNano()%1_000_000_000)
	nameB := fmt.Sprintf("Bravo%d", time.Now().UnixNano()%1_000_000_000)

	// Both log in and land in the same room (the temple). A logs in first so it is already present when B
	// arrives; both are confirmed spawned before A acts, so B is guaranteed in the room for the broadcasts.
	require.Truef(t, a.Expect("By what name", 15*time.Second), "A: no login prompt; transcript:\n%s", a.Transcript())
	a.Send(nameA)
	require.Truef(t, a.Expect("The Temple Square", 15*time.Second), "A did not spawn at the temple; transcript:\n%s", a.Transcript())
	require.Truef(t, b.Expect("By what name", 15*time.Second), "B: no login prompt; transcript:\n%s", b.Transcript())
	b.Send(nameB)
	require.Truef(t, b.Expect("The Temple Square", 15*time.Second), "B did not spawn at the temple; transcript:\n%s", b.Transcript())

	// --- A says -> B hears it (room-presence comms fan-out) ---
	from := b.Len()
	a.Send("say hello")
	require.Truef(t, b.ExpectFrom(from, nameA+" says, 'hello'", 10*time.Second),
		"B did not hear A's `say` (room fan-out regression); B transcript:\n%s", b.Transcript())

	// --- A moves north -> B (still in the temple) sees the departure line ---
	from = b.Len()
	a.Send("north")
	require.Truef(t, b.ExpectFrom(from, nameA+" leaves north.", 10*time.Second),
		"B did not see A's departure (movement fan-out regression); B transcript:\n%s", b.Transcript())

	a.Send("quit")
	b.Send("quit")
}
