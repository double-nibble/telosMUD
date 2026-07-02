package telnet

import (
	"bytes"
	"testing"
)

// gmcp_test.go covers the telnet GMCP codec (Phase 9.1): the WILL/DO negotiation, the inbound
// IAC SB 201 parser (incl. escaping, the over-cap drop, and not corrupting the surrounding line), and
// the outbound encoder. FuzzGMCPSubneg hammers the inbound parser with arbitrary framing.

// gmcpSink captures inbound GMCP messages for assertions.
type gmcpSink struct{ msgs []struct{ pkg, json string } }

func (s *gmcpSink) handler() func(string, []byte) {
	return func(pkg string, json []byte) {
		s.msgs = append(s.msgs, struct{ pkg, json string }{pkg, string(json)})
	}
}

func TestGMCPOfferAndNegotiation(t *testing.T) {
	var out bytes.Buffer
	// Client accepts then later rejects: IAC DO 201, "hi" LF, IAC DONT 201, "bye" LF.
	input := []byte{iac, doo, optGMCP, 'h', 'i', '\n', iac, dont, optGMCP, 'b', 'y', 'e', '\n'}
	c := NewReadWriter(bytes.NewReader(input), &out)

	if err := c.OfferGMCP(); err != nil {
		t.Fatal(err)
	}
	if got := out.Bytes(); !bytes.Equal(got, []byte{iac, will, optGMCP}) {
		t.Fatalf("offer = % x; want IAC WILL 201", got)
	}

	if l, _ := c.ReadLine(); l != "hi" {
		t.Fatalf("line 1 = %q, want hi", l)
	}
	if !c.GMCPEnabled() {
		t.Fatal("GMCP should be enabled after IAC DO 201")
	}
	if l, _ := c.ReadLine(); l != "bye" {
		t.Fatalf("line 2 = %q, want bye", l)
	}
	if c.GMCPEnabled() {
		t.Fatal("GMCP should be disabled after IAC DONT 201")
	}
}

// The inbound tests below deliberately do NOT negotiate DO 201 first: `gmcpOn` gates OUTBOUND
// emission only (WriteGMCP), while handleIAC routes an inbound SB 201 to readGMCPSubneg
// unconditionally — a client that subnegotiates without accepting the offer is parsed, not
// refused, and the gate's semantic layer decides what to do with the payload. So skipping the
// handshake here matches production exactly (ai-finding #11 asked; this is by design, not a gap).
func TestGMCPInboundParseAndLineIntact(t *testing.T) {
	sink := &gmcpSink{}
	// IAC SB 201 "Core.Hello {\"client\":\"Mudlet\"}" IAC SE, then a data-less "Core.Ping", then "go" LF.
	var input []byte
	input = append(input, iac, sb, optGMCP)
	input = append(input, []byte(`Core.Hello {"client":"Mudlet"}`)...)
	input = append(input, iac, se)
	input = append(input, iac, sb, optGMCP)
	input = append(input, []byte("Core.Ping")...) // no space → data-less
	input = append(input, iac, se)
	input = append(input, []byte("go\n")...)

	c := NewReadWriter(bytes.NewReader(input), &bytes.Buffer{})
	c.SetGMCPHandler(sink.handler())

	// The surrounding line is delivered intact, and both GMCP messages fired during the read.
	if l, _ := c.ReadLine(); l != "go" {
		t.Fatalf("line = %q, want go (GMCP must not corrupt the line)", l)
	}
	if len(sink.msgs) != 2 {
		t.Fatalf("got %d GMCP messages, want 2: %+v", len(sink.msgs), sink.msgs)
	}
	if sink.msgs[0].pkg != "Core.Hello" || sink.msgs[0].json != `{"client":"Mudlet"}` {
		t.Fatalf("msg[0] = %+v, want Core.Hello + the json", sink.msgs[0])
	}
	if sink.msgs[1].pkg != "Core.Ping" || sink.msgs[1].json != "" {
		t.Fatalf("msg[1] = %+v, want data-less Core.Ping", sink.msgs[1])
	}
}

func TestGMCPInboundEscapedIAC(t *testing.T) {
	sink := &gmcpSink{}
	// A payload byte 0xFF arrives escaped as IAC IAC inside the subneg; it must decode to one 0xFF.
	var input []byte
	input = append(input, iac, sb, optGMCP)
	input = append(input, []byte("X a")...)
	input = append(input, iac, iac) // escaped 0xFF
	input = append(input, []byte("b")...)
	input = append(input, iac, se)
	input = append(input, []byte("done\n")...)

	c := NewReadWriter(bytes.NewReader(input), &bytes.Buffer{})
	c.SetGMCPHandler(sink.handler())
	c.ReadLine()

	if len(sink.msgs) != 1 || sink.msgs[0].pkg != "X" || sink.msgs[0].json != "a\xffb" {
		t.Fatalf("escaped-IAC msg = %+v, want pkg X json a<0xFF>b", sink.msgs)
	}
}

func TestGMCPInboundOverCapDropped(t *testing.T) {
	sink := &gmcpSink{}
	// A subneg whose payload exceeds maxGMCPInBytes must be drained to IAC SE and DROPPED (no handler
	// call), and the NEXT line must still parse cleanly.
	var input []byte
	input = append(input, iac, sb, optGMCP)
	input = append(input, []byte("Big ")...)
	input = append(input, bytes.Repeat([]byte("z"), maxGMCPInBytes+100)...)
	input = append(input, iac, se)
	input = append(input, []byte("after\n")...)

	c := NewReadWriter(bytes.NewReader(input), &bytes.Buffer{})
	c.SetGMCPHandler(sink.handler())

	if l, _ := c.ReadLine(); l != "after" {
		t.Fatalf("line after an over-cap subneg = %q, want after", l)
	}
	if len(sink.msgs) != 0 {
		t.Fatalf("an over-cap GMCP subneg was delivered (%+v), want it dropped", sink.msgs)
	}
}

func TestWriteGMCPFramingAndGate(t *testing.T) {
	var out bytes.Buffer
	c := NewReadWriter(&bytes.Buffer{}, &out)

	// Disabled: WriteGMCP is a no-op.
	if err := c.WriteGMCP("Char.Vitals", []byte(`{"hp":10}`)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("WriteGMCP wrote %d bytes while disabled, want 0", out.Len())
	}

	// Enable (simulate the client's IAC DO 201) and emit.
	c.gmcpOn.Store(true)
	if err := c.WriteGMCP("Char.Vitals", []byte(`{"hp":10}`)); err != nil {
		t.Fatal(err)
	}
	want := append([]byte{iac, sb, optGMCP}, []byte(`Char.Vitals {"hp":10}`)...)
	want = append(want, iac, se)
	if got := out.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("frame = % x\nwant   % x", got, want)
	}

	// A data-less message omits the space + payload.
	out.Reset()
	c.WriteGMCP("Core.Ping", nil)
	want2 := append([]byte{iac, sb, optGMCP}, []byte("Core.Ping")...)
	want2 = append(want2, iac, se)
	if got := out.Bytes(); !bytes.Equal(got, want2) {
		t.Fatalf("data-less frame = % x\nwant         % x", got, want2)
	}

	// An over-cap frame is SHED, not an error: nil return, zero bytes written, connection still
	// usable (the error return is reserved for socket failures — an error here would make the
	// gate's runWriter close the connection over one oversize advisory frame).
	out.Reset()
	if err := c.WriteGMCP("Big.Package", make([]byte, maxGMCPPayload+1)); err != nil {
		t.Fatalf("over-cap WriteGMCP returned an error (would disconnect the player): %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("over-cap WriteGMCP wrote %d bytes, want 0 (dropped)", out.Len())
	}
	if err := c.WriteGMCP("Core.Ping", nil); err != nil || out.Len() == 0 {
		t.Fatalf("connection unusable after an over-cap drop: err=%v wrote=%d", err, out.Len())
	}
}

func TestWriteGMCPEscapesIAC(t *testing.T) {
	var out bytes.Buffer
	c := NewReadWriter(&bytes.Buffer{}, &out)
	c.gmcpOn.Store(true)
	// A 0xFF in the payload must be escaped IAC IAC so it can't be misread as a telnet command, and the
	// frame must still terminate with the real IAC SE.
	c.WriteGMCP("P", []byte{'a', iac, 'b'})
	got := out.Bytes()
	if !bytes.HasPrefix(got, []byte{iac, sb, optGMCP}) || !bytes.HasSuffix(got, []byte{iac, se}) {
		t.Fatalf("frame not IAC SB 201 … IAC SE: % x", got)
	}
	if !bytes.Contains(got, []byte{iac, iac}) {
		t.Fatalf("0xFF in payload was not escaped to IAC IAC: % x", got)
	}
}

func TestGMCPInboundIACOtherInBody(t *testing.T) {
	sink := &gmcpSink{}
	// An IAC <other> (here IAC followed by a non-SE, non-IAC byte) inside the body is malformed framing;
	// the codec skips the pair and keeps the surrounding payload intact rather than aborting.
	var input []byte
	input = append(input, iac, sb, optGMCP)
	input = append(input, []byte("P x")...)
	input = append(input, iac, 241) // IAC NOP — a stray command inside the subneg
	input = append(input, []byte("y")...)
	input = append(input, iac, se)
	input = append(input, []byte("z\n")...)

	c := NewReadWriter(bytes.NewReader(input), &bytes.Buffer{})
	c.SetGMCPHandler(sink.handler())
	if l, _ := c.ReadLine(); l != "z" {
		t.Fatalf("line = %q, want z", l)
	}
	if len(sink.msgs) != 1 || sink.msgs[0].pkg != "P" || sink.msgs[0].json != "xy" {
		t.Fatalf("IAC-other-in-body msg = %+v, want pkg P json xy (pair skipped)", sink.msgs)
	}
}

func TestGMCPInboundEOFMidSubneg(t *testing.T) {
	sink := &gmcpSink{}
	// An unterminated subneg (EOF before IAC SE) must surface a read error and deliver nothing, not hang
	// or deliver a partial message.
	input := append([]byte{iac, sb, optGMCP}, []byte("Core.Hello {}")...) // no IAC SE, then EOF
	c := NewReadWriter(bytes.NewReader(input), &bytes.Buffer{})
	c.SetGMCPHandler(sink.handler())
	if _, err := c.ReadLine(); err == nil {
		t.Fatal("ReadLine on an unterminated subneg should return a read error (EOF)")
	}
	if len(sink.msgs) != 0 {
		t.Fatalf("an unterminated subneg was delivered (%+v), want nothing", sink.msgs)
	}
}

func TestGMCPWrongDirectionRefused(t *testing.T) {
	var out bytes.Buffer
	// A client that WILLs 201 (wrong direction — 201 is server-offered) is refused with DONT, never
	// silently enabled.
	input := []byte{iac, will, optGMCP, 'x', '\n'}
	c := NewReadWriter(bytes.NewReader(input), &out)
	c.ReadLine()
	if c.GMCPEnabled() {
		t.Fatal("a client WILL 201 must not enable GMCP (only DO does)")
	}
	if got := out.Bytes(); !bytes.Equal(got, []byte{iac, dont, optGMCP}) {
		t.Fatalf("refusal = % x; want IAC DONT 201", got)
	}
}

func TestGMCPInboundPkgOverCapDropped(t *testing.T) {
	sink := &gmcpSink{}
	// The over-cap drop must fire even when the PACKAGE name (no space at all) is what exceeds the cap —
	// the guard is on the assembled buffer, before the pkg/json split.
	var input []byte
	input = append(input, iac, sb, optGMCP)
	input = append(input, bytes.Repeat([]byte("P"), maxGMCPInBytes+50)...) // no space → all pkg
	input = append(input, iac, se)
	input = append(input, []byte("ok\n")...)

	c := NewReadWriter(bytes.NewReader(input), &bytes.Buffer{})
	c.SetGMCPHandler(sink.handler())
	if l, _ := c.ReadLine(); l != "ok" {
		t.Fatalf("line = %q, want ok", l)
	}
	if len(sink.msgs) != 0 {
		t.Fatalf("an over-cap pkg-only subneg was delivered (%+v), want it dropped", sink.msgs)
	}
}

// FuzzGMCPSubneg feeds arbitrary bytes as a stream of (possibly malformed) GMCP subnegotiations and
// asserts the codec never panics and any delivered payload is bounded. A real client's framing is a
// strict subset of this — the fuzzer explores truncated/unterminated/oversized/escape-laden framing.
func FuzzGMCPSubneg(f *testing.F) {
	seeds := [][]byte{
		append(append([]byte{iac, sb, optGMCP}, []byte(`Core.Hello {}`)...), iac, se),
		{iac, sb, optGMCP, iac, se},                                  // empty payload
		{iac, sb, optGMCP, 'X', iac, iac, iac, se},                   // escaped IAC then terminator
		append([]byte{iac, sb, optGMCP}, []byte("no terminator")...), // unterminated (EOF mid-subneg)
		{iac, doo, optGMCP, 'h', 'i', '\n'},
		{iac, sb, 99, 'a', 'b', iac, se, 'x', '\n'}, // a NON-GMCP subneg (skipped) + a line
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var maxPayload int
		c := NewReadWriter(bytes.NewReader(data), &bytes.Buffer{})
		c.SetGMCPHandler(func(pkg string, json []byte) {
			if n := len(pkg) + len(json); n > maxPayload {
				maxPayload = n
			}
		})
		// Drain the stream: ReadLine must never panic on any framing, and must eventually return an
		// error (EOF) rather than looping. Bound the iterations defensively: every returned line
		// consumed at least one input byte, so len(data) iterations reach EOF; the +8 is slack for
		// the zero-consumption edges (empty input, a trailing partial frame). If that reasoning
		// were ever off, the loop just exits early — it can never hang the fuzzer (ai-finding #12).
		for i := 0; i < len(data)+8; i++ {
			if _, err := c.ReadLine(); err != nil {
				break
			}
		}
		// A delivered GMCP payload is bounded by the cap (pkg + json, the pkg coming from within the
		// capped buffer). Anything larger means the over-cap guard failed.
		if maxPayload > maxGMCPInBytes {
			t.Fatalf("delivered a GMCP payload of %d bytes, over the %d cap", maxPayload, maxGMCPInBytes)
		}
	})
}
