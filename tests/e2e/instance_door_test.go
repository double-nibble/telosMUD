//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// instance_door_test.go — #425 at the e2e tier: the declared instance entrance (#435) against the real stack.
//
// # What this can and cannot assert today, stated plainly
//
// It asserts the door is REFUSED, not that it opens, and that is a property of the deployment rather than a
// limitation of the test. requestInstanceEntry requires a VERIFIED ACCOUNT — an unattributable mint is an
// uncapped mint, so it refuses without one — and the account on a session comes only from a signature-checked
// assertion. The dev-autoauth login the compose stack runs returns an empty account id, and no world service
// in deploy/docker-compose.yml sets TELOS_ACCOUNT_VERIFY_KEY, so no session on that stack has an account at
// all. Typing `enter` therefore always gets the fail-closed refusal.
//
// That is the correct behavior, and pinning it is worth real money — but it means the positive journey is NOT
// drivable here. It is covered instead at the in-process gate tier, where the harness can sign real
// assertions (internal/gate/instance_journey_test.go). WHEN the dev login learns to carry a synthetic account,
// this test should be INVERTED rather than deleted.
//
// # Why it earns its place regardless
//
// The compose stack serves content from POSTGRES, not from the embedded YAML. So this is the only test at any
// tier that proves `instance_entrances` survives the whole pipeline — YAML, seed, the rooms.body JSONB
// column, loadRooms, and into the live room map. That is precisely the field-drop class this repo has shipped
// four times: a new definition field parses from YAML, survives every in-memory test, and comes back empty
// through the store.
func TestDungeonDoorRefusesAnUnattributableSession(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	name := fmt.Sprintf("Door%d", time.Now().UnixNano()%1_000_000_000)

	c, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s", addr)
	require.Truef(t, c.Expect("By what name", 15*time.Second),
		"gate never presented the login prompt; transcript:\n%s", c.Transcript())
	c.Send(name)
	require.Truef(t, c.Expect("The Temple Square", 15*time.Second),
		"never spawned; transcript:\n%s", c.Transcript())

	// West to the guild hall, where the demo pack declares `instance_entrances: {enter: crypt}`.
	c.Send("west")
	require.Truef(t, c.Expect("The Adventurers' Guild", 15*time.Second),
		"never reached the guild hall; transcript:\n%s", c.Transcript())

	// THE FIELD-DROP ASSERTION, and the reason this test is worth running on a stack served from Postgres.
	// The door has to be VISIBLE: a player cannot type a direction they were never shown, and an entrance
	// that vanished anywhere in the YAML -> seed -> rooms.body JSONB -> loadRooms pipeline would simply be
	// absent from this line while every in-memory test in the tree stayed green.
	require.Truef(t, c.Expect("enter, north", 5*time.Second),
		"the guild hall's Exits line does not list the instance entrance, so `instance_entrances` did not "+
			"survive the store round trip into live content. (Matched against the neighbouring direction "+
			"rather than the bare word, so an `enter` appearing anywhere else in the transcript cannot "+
			"satisfy it); transcript:\n%s", c.Transcript())

	// THE CONTRAST IS THE ORACLE, and without it this test is worth nothing.
	//
	// An unwalkable direction and a real-but-refused door produce DIFFERENT messages, and the difference is
	// the whole assertion: if `instance_entrances` had been dropped anywhere in the store pipeline, `enter`
	// would fall through to the ordinary unknown-direction path and answer "You can't go that way." — which a
	// test asserting only "the door refuses" would happily accept as a pass. Sending the unwalkable direction
	// FIRST pins what that path says on THIS build, so the comparison is against an observed value rather
	// than a remembered one.
	//
	// `south` deliberately, not a nonsense word: a nonsense word is not a COMMAND at all and is answered
	// "Huh?" by the parser without ever reaching the movement path. The guild hall has no south exit, so this
	// is a real direction with nothing behind it — the exact shape a dropped entrance degrades `enter` into.
	mark := c.Len()
	c.Send("south")
	require.Truef(t, c.ExpectFrom(mark, "can't go that way", 15*time.Second),
		"a real direction with no exit did not produce the unknown-direction refusal, so this test cannot "+
			"tell that message apart from a missing door; transcript:\n%s", c.Transcript())

	mark = c.Len()
	c.Send("enter")
	require.Truef(t, c.ExpectFrom(mark, "The way does not open for you", 15*time.Second),
		"typing `enter` in the guild hall did not reach the instance entrance. Either the door is missing "+
			"from store-served content — the field-drop this test exists to catch, in which case the reply "+
			"above was the unknown-direction message — or the session now carries a verified account, in "+
			"which case this test needs INVERTING to assert the successful journey; transcript:\n%s",
		c.Transcript())

	// And specifically NOT the unknown-direction message: the door was resolved and refused on the account
	// check, rather than never having existed.
	require.Falsef(t, c.ExpectFrom(mark, "can't go that way", 2*time.Second),
		"`enter` produced the UNKNOWN-DIRECTION refusal, so the declared entrance did not survive the store "+
			"round trip into live content; transcript:\n%s", c.Transcript())

	c.Send("quit")
}
