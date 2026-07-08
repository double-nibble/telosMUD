package integration

import (
	"bytes"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/consoleui"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// render_bidi_test.go — the #25b end-to-end guarantee across the display-templating + edge seam. A consoleui
// sheet wraps an RTL cell in a BALANCED bidi isolate (FSI…PDI) and base-pins the line with an LRM, for column
// stability on a bidi terminal. Those controls must SURVIVE the real edge write path (telnet.Conn.Write ->
// sanitizeOutput). #22 briefly stripped them (isolates were in the Trojan-Source subset); #25b narrowed the
// EDGE strip to the strong OVERRIDE block (U+202A–U+202E) only, so the engine's own grid survives while a
// smuggled OVERRIDE is still dropped. This composes the two half-guarantees — consoleui EMITS the isolates
// (internal/world TestUISheetRTLIsolated) and telnet PRESERVES them (TestWriteStripsBidiOverride) — across the
// package boundary they meet at.
func TestConsoleUISheetKeepsIsolatesThroughEdge(t *testing.T) {
	sheet := consoleui.New().
		Banner("Roster", "=").
		Row([]string{"سلام", "42"}, consoleui.Left, consoleui.Right).
		Render()
	if !strings.Contains(sheet, "\u2068") || !strings.Contains(sheet, "\u2069") {
		t.Fatalf("precondition: consoleui did not isolate the RTL cell: %q", sheet)
	}

	// Drive the rendered sheet through the REAL telnet write path.
	var out bytes.Buffer
	c := telnet.NewReadWriter(bytes.NewReader(nil), &out)
	if err := c.Write(sheet); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "\u2068") || !strings.Contains(got, "\u2069") {
		t.Errorf("edge stripped consoleui's balanced isolates — RTL columns will reorder on a bidi terminal: %q", got)
	}
	if !strings.Contains(got, "\u200e") {
		t.Errorf("edge stripped the LRM base-direction pin: %q", got)
	}
	if !strings.Contains(got, "سلام") {
		t.Errorf("RTL content lost through the edge: %q", got)
	}

	// The security guard #22 kept: a STRONG override smuggled into a cell is NOT isolate-wrapped by consoleui
	// (it holds no strong-RTL letter), so it reaches the edge raw — where sanitizeOutput must still drop it.
	evil := consoleui.New().Row([]string{"ad\u202emin", "x"}, consoleui.Left, consoleui.Left).Render()
	var out2 bytes.Buffer
	c2 := telnet.NewReadWriter(bytes.NewReader(nil), &out2)
	if err := c2.Write(evil); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(out2.String(), 0x202E) {
		t.Errorf("edge must still drop a strong RLO override even inside a sheet cell: %q", out2.String())
	}
}
