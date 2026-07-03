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
	// The quit flush + placement write are ASYNC (the same boundary smoke.sh's assert_cross_shard_reconnect
	// waits out): a reconnect that RACES the flush could spawn fresh at the temple and clobber the persisted
	// market location. Give the flush a brief head start, then POLL for the landing room.
	time.Sleep(3 * time.Second)

	rc, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "reconnect dial")
	require.Truef(t, rc.Expect("By what name", 15*time.Second),
		"reconnect: gate never presented the login prompt; transcript:\n%s", rc.Transcript())
	rc.Send(name)
	// A reconnect must land in the QUIT room (Market Square), never the temple, and never be rejected
	// "mid-transfer" (the aa64b06 regression class).
	got := rc.ExpectAny([]string{"Market Square", "mid-transfer"}, 20*time.Second)
	require.NotEqualf(t, "mid-transfer", got,
		"reconnect was rejected 'mid-transfer' (reconnect regression); transcript:\n%s", rc.Transcript())
	require.Equalf(t, "Market Square", got,
		"reconnect did not land back in the quit room (Market Square) — persistence regression; transcript:\n%s", rc.Transcript())
	rc.Send("quit")
}
