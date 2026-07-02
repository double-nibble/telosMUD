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

// TestScoreSheetE2E drives the LIVE stack to prove the display-templating feature end to end: a real telnet
// client runs `score`, and the demo pack's content-authored `score` template renders through the whole path —
// world (render the Lua template with `self` bound, built via the `ui` toolkit) → Play stream → gate edge
// (where the `{{TOKEN}}` color markup becomes SGR). It asserts the labeled rows appear (template ran, self
// bound) AND that the color tokens rendered to real SGR at the edge (the full pipeline), not literal `{{...}}`.
// Skips cleanly when the stack is down; CI's e2e job brings it up (make up).
func TestScoreSheetE2E(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	c, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s", addr)

	name := fmt.Sprintf("sc%d", time.Now().UnixNano()%1_000_000_000)
	require.Truef(t, c.Expect("By what name", 15*time.Second),
		"gate never presented the login prompt; transcript:\n%s", c.Transcript())
	c.Send(name)
	require.Truef(t, c.Expect("The Temple Square", 15*time.Second),
		"fresh character did not spawn at The Temple Square; transcript:\n%s", c.Transcript())

	from := c.Len()
	c.Send("score")

	for _, want := range []struct{ label, sub string }{
		{"character name in the banner", name},
		{"Level line", "Level"},
		{"Health line (from the template)", "Health"},
		{"Mana line (from the template)", "Mana"},
		{"Strength line (from the template)", "Strength"},
		{"cyan SGR rendered from {{FG_CYAN}} around the name", "\x1b[36m"},
		{"green SGR rendered from {{FG_GREEN}} on Health", "\x1b[32m"},
	} {
		require.Truef(t, c.ExpectFrom(from, want.sub, 10*time.Second),
			"score sheet missing %s — the display template did not render through the edge;\ntranscript:\n%s",
			want.label, c.Transcript())
	}

	// The raw color markup must NOT leak as literal text (it must render to SGR at the edge, or be absent).
	if c.ExpectFrom(from, "{{FG_CYAN}}", 1*time.Second) {
		t.Fatalf("literal color markup leaked to the client instead of rendering to SGR;\ntranscript:\n%s", c.Transcript())
	}
}
