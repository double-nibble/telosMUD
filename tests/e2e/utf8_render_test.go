//go:build e2e

// Package e2e — see combat_death_test.go for the tier's contract (live stack, real telnet client, skips
// cleanly when the gate is unreachable). The `e2e` build tag keeps this tier OUT of the default hermetic
// `go test ./...` surface (it belongs to `make test-e2e`, which CI's e2e job runs against `make up`).
package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/tests/helpers"
	"github.com/stretchr/testify/require"
)

// TestUTF8RTLRoundTripE2E drives the LIVE stack to prove multibyte text survives the full edge render path
// end to end (docs/REMAINING.md Track 0). A real telnet client `say`s a multibyte mix — RTL Arabic with a
// 0-width combining tanwin, CJK-wide, a decomposed base+combining grapheme, and an emoji — and asserts the
// say-echo comes back byte-intact. That round-trip exercises BOTH directions of the edge:
//
//	client bytes -> gate telnet ReadLine sanitize -> Play stream -> world CleanLine -> act("You say, '$t'")
//	-> Play stream -> gate telnet Write/sanitizeOutput (the ESC control-strip) -> socket -> client.
//
// If any stage strips, splits, or mis-decodes a UTF-8 sequence, the exact substring won't appear in the echo.
// Skips cleanly when the stack is down; CI's e2e job brings it up (make up) so this runs there.
func TestUTF8RTLRoundTripE2E(t *testing.T) {
	addr := helpers.E2EAddr(t) // SKIPs cleanly when the gate is not reachable.

	c, err := helpers.Dial(t, addr)
	require.NoErrorf(t, err, "dial gate %s", addr)

	// Log in as a fresh, unique character (the login name IS the character today; spawns at the temple).
	name := fmt.Sprintf("rtl%d", time.Now().UnixNano()%1_000_000_000)
	require.Truef(t, c.Expect("By what name", 15*time.Second),
		"gate never presented the login prompt; transcript:\n%s", c.Transcript())
	c.Send(name)
	require.Truef(t, c.Expect("The Temple Square", 15*time.Second),
		"fresh character did not spawn at The Temple Square; transcript:\n%s", c.Transcript())

	// The multibyte payload. Combining/joiner runes are built from explicit code points so the test source
	// encoding can't mask a corruption. Covers RTL + implicit-bidi (LTR embedded in RTL), CJK-wide, a
	// decomposed grapheme, and a ZWJ emoji grapheme cluster.
	rtl := "مرحبا" + string(rune(0x064B)) + " يا عالم" // Arabic + a combining tanwin
	bidi := "قال hello للعالم"                         // English embedded in Arabic (implicit bidi)
	const cjk = "世界"
	decomposed := "cafe" + string(rune(0x0301))                             // base + COMBINING ACUTE ACCENT
	family := "👨" + string(rune(0x200D)) + "👩" + string(rune(0x200D)) + "👧" // ZWJ grapheme cluster

	from := c.Len()
	c.Send("say " + rtl + " ~ " + bidi + " ~ " + cjk + " ~ " + decomposed + " ~ " + family)

	// The say-echo ("You say, '<payload>'") must contain each multibyte fragment byte-intact. Scope the
	// match to output AFTER the command so an earlier line can't produce a false positive. A contiguous
	// substring match is a sufficient signal for split/strip: a torn sequence would break the exact bytes.
	for _, want := range []struct{ label, sub string }{
		{"RTL Arabic (with combining tanwin)", rtl},
		{"implicit-bidi LTR-in-RTL", bidi},
		{"CJK-wide", cjk},
		{"decomposed base+combining", decomposed},
		{"ZWJ emoji grapheme cluster", family},
	} {
		require.Truef(t, c.ExpectFrom(from, want.sub, 10*time.Second),
			"%s did not round-trip through the edge intact — the say-echo is missing the exact bytes;\ntranscript:\n%s",
			want.label, c.Transcript())
	}
}
