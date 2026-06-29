package gate

import (
	"bytes"
	"io"
	"log/slog"
	"strconv"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/telnet"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestGMCPSupportedPrefix(t *testing.T) {
	g := newGMCPState()
	g.setSupports([]string{"Char", "Room.Info"})
	cases := []struct {
		pkg  string
		want bool
	}{
		{"Char", true},           // exact
		{"Char.Vitals", true},    // ancestor "Char" advertised ⇒ sub-message supported
		{"Char.Items.Inv", true}, // deeper descendant of "Char"
		{"Room.Info", true},      // exact (a specific message, not the whole package)
		{"Room", false},          // only Room.Info advertised, not the Room package
		{"Room.Players", false},  // sibling of the advertised Room.Info
		{"Comm.Channel.Text", false},
	}
	for _, c := range cases {
		if got := g.supported(c.pkg); got != c.want {
			t.Errorf("supported(%q) = %v, want %v", c.pkg, got, c.want)
		}
	}
}

func TestParseSupports(t *testing.T) {
	// Version suffixes are stripped; malformed/invalid names are skipped; bad JSON yields nothing.
	got := parseSupports([]byte(`["Char 1","Char.Vitals 1","Room 2","Bad Name With Spaces","",".x"]`))
	want := map[string]bool{"Char": true, "Char.Vitals": true, "Room": true, "Bad": true}
	if len(got) != len(want) {
		t.Fatalf("parseSupports = %v, want keys %v", got, want)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected package %q", p)
		}
	}
	if parseSupports([]byte(`not json`)) != nil {
		t.Error("malformed JSON should parse to nil")
	}
}

func TestValidGMCPPackage(t *testing.T) {
	valid := []string{"Core.Hello", "Char.Vitals", "Room", "Mud.Map", "A1.b2"}
	for _, p := range valid {
		if !validGMCPPackage(p) {
			t.Errorf("validGMCPPackage(%q) = false, want true", p)
		}
	}
	invalid := []string{
		"", ".Char", "Char.", "Char Vitals", "Char\x00", "Char\xff", "Char\x1b[31m",
		string(make([]byte, 65)), // too long
	}
	for _, p := range invalid {
		if validGMCPPackage(p) {
			t.Errorf("validGMCPPackage(%q) = true, want false (must reject for safe logging)", p)
		}
	}
}

// TestRenderFrameGMCPFilter is the integration of the support filter + the codec encoder: an enabled
// client receives only the GMCP packages it advertised, framed as IAC SB 201 … IAC SE; an unadvertised
// package is dropped before it ever reaches the wire.
func TestRenderFrameGMCPFilter(t *testing.T) {
	var out bytes.Buffer
	// A telnet.Conn whose read side delivers IAC DO 201 (the client accepting GMCP); ReadLine processes
	// it (enabling GMCP) and returns EOF. Writes go to `out`.
	tc := telnet.NewReadWriter(bytes.NewReader([]byte{255, 253, 201}), &out)
	if _, err := tc.ReadLine(); err == nil {
		t.Fatal("expected EOF after the lone IAC DO 201")
	}
	if !tc.GMCPEnabled() {
		t.Fatal("GMCP should be enabled after IAC DO 201")
	}

	g := newGMCPState()
	g.setSupports([]string{"Char"})
	c := &connState{tc: tc, gmcp: g, log: discardLog()}
	var s Server

	gmcpFrame := func(pkg, json string) *playv1.ServerFrame {
		return &playv1.ServerFrame{Payload: &playv1.ServerFrame_Gmcp{Gmcp: &playv1.GmcpOut{Pkg: pkg, Json: []byte(json)}}}
	}

	// Advertised (Char.Vitals is under the advertised "Char") → framed onto the wire.
	s.renderFrame(discardLog(), c, gmcpFrame("Char.Vitals", `{"hp":10}`))
	want := append([]byte{255, 250, 201}, []byte(`Char.Vitals {"hp":10}`)...)
	want = append(want, 255, 240)
	if !bytes.Contains(out.Bytes(), want) {
		t.Fatalf("advertised GMCP not framed; out = % x", out.Bytes())
	}

	// NOT advertised (Room) → dropped, nothing more written.
	before := out.Len()
	s.renderFrame(discardLog(), c, gmcpFrame("Room.Info", `{"num":1}`))
	if out.Len() != before {
		t.Fatalf("an unadvertised GMCP package was written: % x", out.Bytes()[before:])
	}

	// An INVALID world-supplied package name (control bytes) is dropped before WriteGMCP — the outbound
	// defense-in-depth against a future content/Lua path naming a package with CR/LF/ESC.
	before = out.Len()
	s.renderFrame(discardLog(), c, gmcpFrame("Char\r\n.Evil", `{}`))
	if out.Len() != before {
		t.Fatalf("an invalid world GMCP package was written to the wire: % x", out.Bytes()[before:])
	}
}

func TestGMCPSupportsCapped(t *testing.T) {
	g := newGMCPState()
	// Advertise far more than the cap across several Add calls; the set must not grow past the cap.
	for batch := 0; batch < 4; batch++ {
		pkgs := make([]string, 200)
		for i := range pkgs {
			pkgs[i] = "Pkg" + strconv.Itoa(batch*200+i)
		}
		g.addSupports(pkgs)
	}
	g.mu.Lock()
	n := len(g.supports)
	g.mu.Unlock()
	if n > maxSupportsEntries {
		t.Fatalf("supports set grew to %d, over the cap of %d", n, maxSupportsEntries)
	}
}

// TestGateOffersGMCPAndPlayWorks is the handshake-transparency e2e: on connect the gate offers
// IAC WILL 201, a client that accepts (IAC DO 201) + advertises supports negotiates without disturbing
// the text path — login and a normal command still round-trip.
func TestGateOffersGMCPAndPlayWorks(t *testing.T) {
	h := newHarness(t)
	const addr = "addr-a"
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	// The gate offers GMCP first thing: IAC WILL 201.
	term.expectBytes(t, []byte{255, 251, 201})
	// The client accepts and advertises a package, raw on the wire.
	if _, err := term.conn.Write([]byte{255, 253, 201}); err != nil { // IAC DO 201
		t.Fatal(err)
	}
	term.sendGMCP(t, "Core.Supports.Set", `["Char 1","Room 1"]`)

	// The text path is undisturbed: login + a command still work.
	term.login(t, "Mudleter")
	term.expect(t, "Temple Square")
	term.send(t, "look")
	term.expect(t, "Exits:")
	term.close(t)
}

// TestGMCPCharVitalsReachesClient is the world→client e2e the transport slice couldn't yet exercise:
// a GMCP client that advertised "Char" receives a Char.Vitals frame (IAC SB 201 "Char.Vitals" …),
// emitted by the world alongside the login prompt and framed onto the wire by the gate's filter.
func TestGMCPCharVitalsReachesClient(t *testing.T) {
	h := newHarness(t)
	const addr = "addr-a"
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.expectBytes(t, []byte{255, 251, 201})                        // gate offers IAC WILL 201
	if _, err := term.conn.Write([]byte{255, 253, 201}); err != nil { // client IAC DO 201
		t.Fatal(err)
	}
	term.sendGMCP(t, "Core.Supports.Set", `["Char 1"]`) // advertise the Char package

	term.login(t, "Hudder")
	// The world emits Char.Vitals on the login prompt; the gate frames it as IAC SB 201 "Char.Vitals " …
	term.expectBytes(t, []byte{255, 250, 201, 'C', 'h', 'a', 'r', '.', 'V', 'i', 't', 'a', 'l', 's', ' '})
	term.close(t)
}

// TestGMCPNotSentToUnsubscribedClient proves the default-deny filter end-to-end: a GMCP-enabled client
// that advertised NOTHING (no Core.Supports) receives no Char.Vitals frame even though the world emits
// it — and normal play still works.
func TestGMCPNotSentToUnsubscribedClient(t *testing.T) {
	h := newHarness(t)
	const addr = "addr-a"
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.expectBytes(t, []byte{255, 251, 201})
	if _, err := term.conn.Write([]byte{255, 253, 201}); err != nil { // DO 201, but advertise nothing
		t.Fatal(err)
	}
	term.login(t, "Plainish")
	term.expect(t, "Temple Square")
	term.send(t, "look")
	term.expect(t, "Exits:")
	// No package advertised ⇒ default-deny ⇒ no GMCP subnegotiation byte (0xFA after an IAC) ever sent.
	if i := indexOf(term.acc.String(), string([]byte{255, 250, 201})); i >= 0 {
		t.Fatalf("a GMCP frame reached a client that advertised no packages (at %d)", i)
	}
	term.close(t)
}
