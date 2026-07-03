//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// TestRestAndVitals is the end-to-end acceptance test for the REST mechanic (#39) and the live-vitals HUD
// (#40): a real telnet client toggles rest/stand and vitals and asserts the player-visible responses plus the
// vitals prompt prefix. These verbs are player-visible and were uncovered end-to-end — the deeper
// regen-multiplier / OnRest-once mechanics stay at the unit tier. It runs entirely at the temple: no combat,
// no cross-shard handoff, so it is fast and repop-independent.
func TestRestAndVitals(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	c, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s", addr)

	// A fresh, unique character (spawns at The Temple Square with full pools).
	name := fmt.Sprintf("rv%d", time.Now().UnixNano()%1_000_000_000)
	require.Truef(t, c.Expect("By what name", 15*time.Second),
		"gate never presented the login prompt; transcript:\n%s", c.Transcript())
	c.Send(name)
	require.Truef(t, c.Expect("The Temple Square", 15*time.Second),
		"fresh character did not spawn at The Temple Square; transcript:\n%s", c.Transcript())

	// --- rest -> already-resting -> stand (the bodily-state verbs) ---
	from := c.Len()
	c.Send("rest")
	require.Truef(t, c.ExpectFrom(from, "You sit down and rest.", 10*time.Second),
		"`rest` did not confirm entering the resting state; transcript:\n%s", c.Transcript())

	from = c.Len()
	c.Send("rest")
	require.Truef(t, c.ExpectFrom(from, "You are already resting.", 10*time.Second),
		"a second `rest` should report already-resting (no-op notice); transcript:\n%s", c.Transcript())

	from = c.Len()
	c.Send("stand")
	require.Truef(t, c.ExpectFrom(from, "You stand up.", 10*time.Second),
		"`stand` did not confirm leaving the resting state; transcript:\n%s", c.Transcript())

	// --- vitals on -> confirmation AND the plain-text prompt gains the "[hp: cur/max ...]" prefix ---
	// The confirmation and the next prompt both land in the window after `vitals on`, so a single scoped
	// window asserts both. (Cross-checked live: the prompt shows "[hp: 105/105 mana: 83/83]" immediately.)
	from = c.Len()
	c.Send("vitals on")
	require.Truef(t, c.ExpectFrom(from, "Live vitals ON.", 10*time.Second),
		"`vitals on` did not confirm; transcript:\n%s", c.Transcript())
	require.Truef(t, c.ExpectFrom(from, "[hp: ", 10*time.Second),
		"the live-vitals prompt prefix did not appear after `vitals on` (#40 vitals prompt); transcript:\n%s", c.Transcript())

	// --- vitals off -> confirmation ---
	from = c.Len()
	c.Send("vitals off")
	require.Truef(t, c.ExpectFrom(from, "Live vitals OFF.", 10*time.Second),
		"`vitals off` did not confirm; transcript:\n%s", c.Transcript())

	c.Send("quit")
}
