//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// TestInventoryWearKeepCoalesce is the e2e for the inventory verbs whose LOGIC is unit-covered but whose
// player-visible RENDER (the act-template item-name substitution + the identical-item coalescing) was
// untested end-to-end: #36's drop-refuses-worn and the keep/unkeep no-drop flag, plus the "(N)" coalescing.
// It uses the market's FLOOR items (an iron helmet + wooden torches, reachable via one `north`, no combat).
//
// CI is deterministic (fresh stack = a full floor of 1 helmet + 5 torches; e2e tests run sequentially and no
// other test depletes the market floor). Like combat_death_test's goblin, a fast LOCAL re-run can race a
// not-yet-repopped floor (a crashed prior run carried the helmet off; each run spends 2 of 5 torches) —
// space local reruns by the ~90s reset_secs stride or restart world-midgaard to force a repop.
func TestInventoryWearKeepCoalesce(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	c, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s", addr)

	name := fmt.Sprintf("Inv%d", time.Now().UnixNano()%1_000_000_000)
	require.Truef(t, c.Expect("By what name", 15*time.Second),
		"gate never presented the login prompt; transcript:\n%s", c.Transcript())
	c.Send(name)
	require.Truef(t, c.Expect("The Temple Square", 15*time.Second),
		"fresh character did not spawn at The Temple Square; transcript:\n%s", c.Transcript())
	c.Send("north")
	require.Truef(t, c.Expect("Market Square", 15*time.Second),
		"north from the temple did not reach Market Square; transcript:\n%s", c.Transcript())

	// --- wear -> drop-REFUSES-worn -> remove (#36 part 2) ---
	from := c.Len()
	c.Send("get helmet")
	require.Truef(t, c.ExpectFrom(from, "You get an iron helmet.", 10*time.Second),
		"could not get the helmet from the market floor; transcript:\n%s", c.Transcript())
	from = c.Len()
	c.Send("wear helmet")
	require.Truef(t, c.ExpectFrom(from, "You wear an iron helmet on your head.", 10*time.Second),
		"`wear helmet` did not confirm; transcript:\n%s", c.Transcript())
	from = c.Len()
	c.Send("drop helmet")
	require.Truef(t, c.ExpectFrom(from, "You must remove an iron helmet before you can part with it.", 10*time.Second),
		"dropping a WORN item must be refused, not silently un-equipped (#36); transcript:\n%s", c.Transcript())
	from = c.Len()
	c.Send("remove helmet")
	require.Truef(t, c.ExpectFrom(from, "You stop using an iron helmet.", 10*time.Second),
		"`remove helmet` did not confirm; transcript:\n%s", c.Transcript())

	// --- keep -> kept-BLOCKS-drop -> unkeep -> drop succeeds (#36 keep no-drop) ---
	from = c.Len()
	c.Send("keep helmet")
	require.Truef(t, c.ExpectFrom(from, "You mark an iron helmet keep", 10*time.Second),
		"`keep helmet` did not confirm; transcript:\n%s", c.Transcript())
	from = c.Len()
	c.Send("drop helmet")
	require.Truef(t, c.ExpectFrom(from, "is marked keep; `unkeep` it first", 10*time.Second),
		"dropping a KEPT item must be refused; transcript:\n%s", c.Transcript())
	from = c.Len()
	c.Send("unkeep helmet")
	require.Truef(t, c.ExpectFrom(from, "You no longer keep an iron helmet.", 10*time.Second),
		"`unkeep helmet` did not confirm the flag was cleared; transcript:\n%s", c.Transcript())
	from = c.Len()
	c.Send("drop helmet")
	require.Truef(t, c.ExpectFrom(from, "You drop an iron helmet.", 10*time.Second),
		"drop after unkeep should succeed; transcript:\n%s", c.Transcript())

	// --- coalescing: two identical torches render as one line with a "(2)" count ---
	c.Send("get torch")
	c.Send("get torch")
	from = c.Len()
	c.Send("inventory")
	require.Truef(t, c.ExpectFrom(from, "A wooden torch (2)", 10*time.Second),
		"two identical torches should coalesce to 'A wooden torch (2)' in inventory; transcript:\n%s", c.Transcript())

	c.Send("quit")
}
