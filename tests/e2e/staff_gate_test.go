//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// TestMortalCannotReachStaffVerbs is the security-relevant NEGATIVE guarantee for the Round 8/9 trust ladder:
// a plain rank-0 player can neither SEE nor RUN the staff-only verbs (stat/wizinvis/holylight/rolls). Each is
// registered MinRank=rankStaff (=1) + CmdHidden, so for a mortal (rank 0) it falls through to the parser's
// generic "Huh?" — the command's existence never leaks (the classic wiz-command posture) and it certainly
// never executes. This is drivable with the default dev-autoauth login (every demo player is rank 0), so it
// needs NO staff-rank fixture; the positive/elevated side is an integration concern (#133).
//
// Scope note: the "Huh?" arm assumes the loaded pack defines no ability/custom-Lua/channel verb literally
// named stat/wizinvis/holylight/rolls (the demo pack does not). If one ever did, a mortal would land on that
// content handler instead of "Huh?" and this test would FAIL LOUDLY on the first assertion — a fail-loud
// coupling to the pack, not a silent gap.
func TestMortalCannotReachStaffVerbs(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	c, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s", addr)

	name := fmt.Sprintf("Mortal%d", time.Now().UnixNano()%1_000_000_000)
	require.Truef(t, c.Expect("By what name", 15*time.Second),
		"gate never presented the login prompt; transcript:\n%s", c.Transcript())
	c.Send(name)
	require.Truef(t, c.Expect("The Temple Square", 15*time.Second),
		"fresh character did not spawn at The Temple Square; transcript:\n%s", c.Transcript())

	// Each staff verb a mortal tries, paired with the success string that must NEVER appear (proving the
	// action did not execute, not merely that the verb was hidden).
	cases := []struct {
		cmd     string
		success string
	}{
		{"stat", "[rid #"},                // the stat inspection sheet header (stat.go:95)
		{"wizinvis on", "Wizinvis ON"},    // toggles.go:48
		{"holylight on", "Holylight ON."}, // toggles.go:77
		{"rolls on", "Roll math ON."},     // toggles.go:101
	}
	for _, tc := range cases {
		from := c.Len()
		c.Send(tc.cmd)
		// A mortal gets the generic unknown-command reply — the verb's existence never leaks.
		require.Truef(t, c.ExpectFrom(from, "Huh?", 10*time.Second),
			"mortal %q should get the generic 'Huh?' (a hidden staff verb must not leak); transcript:\n%s", tc.cmd, c.Transcript())
		// ...and the staff action NEVER executed: its success string must be absent from the response window.
		// "Huh?" already arrived, so the command was fully processed — the success line would be present too
		// if it were going to appear.
		window := c.Transcript()[from:]
		require.NotContainsf(t, window, tc.success,
			"mortal %q leaked/executed the staff action (%q appeared in the response)", tc.cmd, tc.success)
	}

	c.Send("quit")
}
