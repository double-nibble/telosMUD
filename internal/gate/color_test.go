package gate

import (
	"bytes"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/telnet"
)

// color_test.go — the edge-local `color` command (Track 1 slice 2): toggles the telnet conn's SGR rendering,
// is handled at the gate, and is NOT forwarded to the world.

func TestHandleColorCommand(t *testing.T) {
	var out bytes.Buffer
	tc := telnet.NewReadWriter(&bytes.Buffer{}, &out) // default color ON

	// `color off` toggles rendering off and confirms in plain text.
	if !handleColorCommand(tc, "color off") {
		t.Fatal("`color off` was not intercepted")
	}
	if tc.ColorEnabled() {
		t.Fatal("color still enabled after `color off`")
	}
	if got := out.String(); !strings.Contains(got, "OFF") || strings.ContainsRune(got, 0x1b) {
		t.Fatalf("`color off` confirmation should be plain and say OFF: %q", got)
	}

	// `color on` toggles rendering on; the confirmation itself renders in color (proves the path).
	out.Reset()
	if !handleColorCommand(tc, "COLOR On") { // case-insensitive
		t.Fatal("`color on` was not intercepted")
	}
	if !tc.ColorEnabled() {
		t.Fatal("color still disabled after `color on`")
	}
	if got := out.String(); !strings.Contains(got, "\x1b[32m") {
		t.Fatalf("`color on` confirmation should be colored green: %q", got)
	}

	// bare `color` reports status without changing it.
	out.Reset()
	if !handleColorCommand(tc, "  color  ") {
		t.Fatal("bare `color` status was not intercepted")
	}
	if !tc.ColorEnabled() {
		t.Fatal("bare `color` must not change the setting")
	}
	if got := out.String(); !strings.Contains(got, "currently on") {
		t.Fatalf("status line wrong: %q", got)
	}

	// Non-color lines are NOT intercepted (forwarded to the world).
	for _, line := range []string{"say hello", "colorful sunset", "recolor", ""} {
		if handleColorCommand(tc, line) {
			t.Fatalf("%q must not be intercepted as a color command", line)
		}
	}
}
