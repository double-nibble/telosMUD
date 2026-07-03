//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// TestCoreLoopReconnect is the ROADMAP ACCEPTANCE journey at the e2e tier: connect -> look -> move -> say ->
// quit -> RECONNECT the same character -> land back in the SAME room. The reconnect-to-same-room persistence
// path had NO Go e2e coverage — only smoke.sh's assert_cross_shard_reconnect. This is the INTRA-shard variant
// (temple -> market, no cross-shard handoff) so it is fast and isolates the persistence seam from the handoff
// machinery the combat test already exercises.
func TestCoreLoopReconnect(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	name := fmt.Sprintf("Loop%d", time.Now().UnixNano()%1_000_000_000)

	// --- session 1: connect -> look -> move -> say -> quit ---
	c, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s", addr)
	require.Truef(t, c.Expect("By what name", 15*time.Second),
		"gate never presented the login prompt; transcript:\n%s", c.Transcript())
	c.Send(name)
	require.Truef(t, c.Expect("The Temple Square", 15*time.Second),
		"fresh character did not spawn at The Temple Square; transcript:\n%s", c.Transcript())

	from := c.Len()
	c.Send("look")
	require.Truef(t, c.ExpectFrom(from, "The Temple Square", 10*time.Second),
		"`look` did not render the spawn room; transcript:\n%s", c.Transcript())

	c.Send("north")
	require.Truef(t, c.Expect("Market Square", 15*time.Second),
		"north from the temple did not reach Market Square; transcript:\n%s", c.Transcript())

	from = c.Len()
	c.Send("say hello")
	require.Truef(t, c.ExpectFrom(from, "You say, 'hello'", 10*time.Second),
		"`say` did not echo to the speaker; transcript:\n%s", c.Transcript())

	c.Send("quit")

	// --- session 2: reconnect the SAME character -> must land back in Market Square, NOT the temple ---
	// The quit flush is ASYNC (quit -> detach -> leave -> saveFinal is a chain of inbox posts + an
	// off-goroutine saver write, with no client-observable "flush done" signal — the socket just closes). A
	// reconnect can arrive before the flush lands; when it does it RE-LOADS the durable row (never a fresh
	// Temple spawn — the row exists from first login, and create-before-CAS ordering structurally precludes
	// Temple ever overwriting Market), so the worst case is landing at the pre-flush room, never data loss.
	// Give the flush a head start, then RE-DIAL up to a bounded budget until the reconnect lands in the quit
	// room — mirroring smoke.sh's assert_cross_shard_reconnect, so the test stays robust on a loaded CI box
	// where a single attempt could beat a slow flush. Re-dialing on a miss is safe (idempotent; no clobber).
	time.Sleep(3 * time.Second)

	landed := false
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); {
		rc, err := helpers.Dial(t, addr)
		require.NoErrorf(t, err, "reconnect dial")
		require.Truef(t, rc.Expect("By what name", 15*time.Second),
			"reconnect: gate never presented the login prompt; transcript:\n%s", rc.Transcript())
		rc.Send(name)
		// Market Square = the quit room (success). "mid-transfer" is inert on this INTRA-shard journey (no
		// cross-shard boundary is crossed, so no frozen session for this character ever exists) — it is
		// asserted only as harmless insurance; the cross-shard reconnect regression is smoke.sh's concern.
		got := rc.ExpectAny([]string{"Market Square", "mid-transfer"}, 10*time.Second)
		require.NotEqualf(t, "mid-transfer", got,
			"reconnect was rejected 'mid-transfer' (unexpected on an intra-shard reconnect); transcript:\n%s", rc.Transcript())
		if got == "Market Square" {
			rc.Send("quit")
			rc.Close()
			landed = true
			break
		}
		// Landed neither in the quit room nor mid-transfer — the flush likely isn't visible yet. Close and
		// retry after a short beat (the durable room only moves forward; re-loading is safe).
		rc.Close()
		time.Sleep(2 * time.Second)
	}
	require.Truef(t, landed,
		"reconnect never landed back in the quit room (Market Square) within the budget — persistence regression")
}
