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

// TestWhoSheetE2E drives the LIVE stack to prove the COLLECTION-bound display surface (#24 part a) end to end:
// a real telnet client runs `who`, and the demo pack's content-authored `who` template renders through the whole
// path — world (blocking presence-roster read OFF the zone goroutine, result posted BACK to the zone inbox, Lua
// template rendered ON the zone goroutine with `self` + `list` bound, built via the `ui` toolkit) → Play stream
// → gate edge (where the `{{TOKEN}}` color markup becomes SGR).
//
// It is the sibling of TestScoreSheetE2E, and the only e2e that exercises the async-fetch → zone-goroutine-render
// bounce: if that render ever moves back into the fetch goroutine, the unit -race gate
// (TestWhoTemplateConcurrentRendersAreRaceFree) fires and this test still proves the player-visible output.
// Skips cleanly when the stack is down; CI's e2e job brings it up (make up).
func TestWhoSheetE2E(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	c, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s", addr)

	name := fmt.Sprintf("wh%d", time.Now().UnixNano()%1_000_000_000)
	require.Truef(t, c.Expect("By what name", 15*time.Second),
		"gate never presented the login prompt; transcript:\n%s", c.Transcript())
	c.Send(name)
	require.Truef(t, c.Expect("The Temple Square", 15*time.Second),
		"fresh character did not spawn at The Temple Square; transcript:\n%s", c.Transcript())

	from := c.Len()
	c.Send("who")

	for _, want := range []struct{ label, sub string }{
		{"the template's banner header", "Players online:"},
		{"this player's row (bound from the roster `list`)", name},
		{"cyan SGR rendered from {{FG_CYAN}} on the banner", "\x1b[36m"},
	} {
		require.Truef(t, c.ExpectFrom(from, want.sub, 15*time.Second),
			"who sheet missing %s — the collection display template did not render through the edge;\ntranscript:\n%s",
			want.label, c.Transcript())
	}

	// The raw color markup must NOT leak as literal text (it must render to SGR at the edge, or be absent).
	if c.ExpectFrom(from, "{{FG_CYAN}}", 1*time.Second) {
		t.Fatalf("literal color markup leaked to the client instead of rendering to SGR;\ntranscript:\n%s", c.Transcript())
	}
}
