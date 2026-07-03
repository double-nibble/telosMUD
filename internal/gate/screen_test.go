package gate

import (
	"bytes"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// screen_test.go — #31: the gate's client-capability gate on the trusted Screen frame. A color-enabled
// (ANSI-capable) client gets the raw bytes verbatim; a `color off` client has the frame dropped so raw
// escapes never garble a plain terminal.
func TestRenderFrameScreenClientCapability(t *testing.T) {
	var out bytes.Buffer
	tc := telnet.NewReadWriter(&bytes.Buffer{}, &out)
	c := &connState{tc: tc, log: discardLog()}
	var s Server

	frame := &playv1.ServerFrame{Payload: &playv1.ServerFrame_Screen{Screen: &playv1.Screen{Data: []byte("\x1b[2J\x1b[H")}}}

	// color on (default): the raw ANSI is written verbatim (bypassing the sanitizer).
	if err := s.renderFrame(discardLog(), c, frame); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("\x1b[2J")) {
		t.Fatalf("a color-enabled client should receive the raw screen bytes; out = % x", out.Bytes())
	}

	// color off: the Screen frame is dropped (no raw escapes to a plain client).
	out.Reset()
	tc.SetColor(false)
	if err := s.renderFrame(discardLog(), c, frame); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("a color-off client must not receive raw screen bytes; got % x", out.Bytes())
	}
}
