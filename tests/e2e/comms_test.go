//go:build e2e

// Package e2e — see combat_death_test.go for the tier's contract (live stack, real telnet client, skips
// cleanly when the gate is unreachable). The `e2e` build tag keeps this OUT of the default `go test ./...`
// surface (it belongs to `make test-e2e`).
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// TestCommsGossipRoundTripE2E is the regression guard for #78: the demo GATE must be wired to NATS
// (TELOS_NATS_URL) so it SUBSCRIBES to the player's channels — without it OpenGate degrades to a Disabled
// bus and comms are silently dead in the stack. A fresh player is subscribed to the open, default-on
// `gossip` channel; the world is the SOURCE (it publishes the line to the bus), the gate is the SINK (it
// renders what it receives — no direct socket echo). So a speaker seeing their OWN gossip line proves the
// full world→NATS→gate comms round-trip is live. Skips cleanly when the stack is down.
func TestCommsGossipRoundTripE2E(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	c, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s", addr)

	name := fmt.Sprintf("gos%d", time.Now().UnixNano()%1_000_000_000)
	require.Truef(t, c.Expect("By what name", 15*time.Second),
		"gate never presented the login prompt; transcript:\n%s", c.Transcript())
	c.Send(name)
	require.Truef(t, c.Expect("The Temple Square", 15*time.Second),
		"fresh character did not spawn at The Temple Square; transcript:\n%s", c.Transcript())

	// Gossip a unique line; it must come BACK to the speaker via the bus round-trip (world publishes,
	// the gate's NATS subscription delivers it, the gate renders "[Gossip] <name>: <msg>"). This only
	// works when the gate is wired to NATS (#78) — a Disabled gate bus never subscribes, so nothing returns.
	from := c.Len()
	msg := fmt.Sprintf("roundtrip-%d", time.Now().UnixNano()%1_000_000)
	c.Send("gossip " + msg)

	want := fmt.Sprintf("[Gossip] %s: %s", name, msg)
	require.Truef(t, c.ExpectFrom(from, want, 10*time.Second),
		"the speaker never received their own gossip line %q — the gate's comms subscription is dead (is TELOS_NATS_URL set on the gate? #78);\ntranscript:\n%s",
		want, c.Transcript())
}
