package gate

import (
	"bytes"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// TestDeliverChannelEmitsGMCP pins the Comm.Channel.Text mirror (Phase 9.5 + #49): a channel line
// delivered to a client that advertised "Comm" is ALSO emitted as structured Comm.Channel.Text
// {channel, talker, text, msg} — text is the rendered line, msg the raw body; a client that did not
// advertise Comm gets only the text line, no GMCP.
func TestDeliverChannelEmitsGMCP(t *testing.T) {
	msg := commbus.Message{Subject: commbus.ChanSubject("gossip"), AuthorName: "Alice", Body: "[Gossip] Alice: hi", Text: "hi"}

	// Advertised "Comm" → the GMCP frame is emitted with the channel/talker + the RAW message body.
	var out bytes.Buffer
	tc := telnet.NewReadWriter(bytes.NewReader([]byte{255, 253, 201}), &out) // IAC DO 201 → GMCP enabled
	tc.ReadLine()
	g := newGMCPState()
	g.setSupports([]string{"Comm"})
	cc := &commsClient{log: discardLog(), tc: tc, gmcp: g, ignore: map[string]struct{}{}}
	cc.deliverChannel(msg)
	got := out.String()
	if !strings.Contains(got, string([]byte{255, 250, 201})+"Comm.Channel.Text ") {
		t.Fatalf("no Comm.Channel.Text GMCP frame; out = %q", got)
	}
	if !strings.Contains(got, `"channel":"gossip"`) || !strings.Contains(got, `"talker":"Alice"`) {
		t.Fatalf("Comm.Channel.Text payload missing channel/talker; out = %q", got)
	}
	// #49: the RAW player message rides alongside the rendered `text` so a client can tab per channel.
	if !strings.Contains(got, `"msg":"hi"`) {
		t.Fatalf("Comm.Channel.Text payload missing the raw msg body; out = %q", got)
	}

	// NOT advertised → only the text line, no GMCP subnegotiation.
	var out2 bytes.Buffer
	tc2 := telnet.NewReadWriter(bytes.NewReader([]byte{255, 253, 201}), &out2)
	tc2.ReadLine()
	cc2 := &commsClient{log: discardLog(), tc: tc2, gmcp: newGMCPState(), ignore: map[string]struct{}{}}
	cc2.deliverChannel(msg)
	if strings.Contains(out2.String(), string([]byte{255, 250, 201})) {
		t.Fatalf("a Comm GMCP frame reached a client that didn't advertise Comm; out = %q", out2.String())
	}
}

// TestDeliverChannelGMCPStripsColorTokens pins the Track-1 guard rail on the Comm mirror: a
// channel_def format may carry {{TOKEN}} color markup, which the telnet Write path renders — the
// structured Comm.Channel.Text payload must carry the STRIPPED text, never the literal tokens.
func TestDeliverChannelGMCPStripsColorTokens(t *testing.T) {
	msg := commbus.Message{
		Subject:    commbus.ChanSubject("gossip"),
		AuthorName: "{{BOLD}}Alice{{RESET}}", // talker is stripped too (engine-set today, defense in depth)
		Body:       "[{{FG_MAGENTA}}Gossip{{RESET}}] Alice: hi",
		Text:       "{{BOLD}}hi{{RESET}}", // the raw body is stripped on the same path (#49)
	}
	var out bytes.Buffer
	tc := telnet.NewReadWriter(bytes.NewReader([]byte{255, 253, 201}), &out) // IAC DO 201 → GMCP enabled
	tc.ReadLine()
	g := newGMCPState()
	g.setSupports([]string{"Comm"})
	cc := &commsClient{log: discardLog(), tc: tc, gmcp: g, ignore: map[string]struct{}{}}
	cc.deliverChannel(msg)
	got := out.String()
	if !strings.Contains(got, `"text":"[Gossip] Alice: hi"`) {
		t.Fatalf("Comm.Channel.Text did not carry the stripped body; out = %q", got)
	}
	if !strings.Contains(got, `"msg":"hi"`) {
		t.Fatalf("Comm.Channel.Text raw msg body not stripped; out = %q", got)
	}
	if strings.Contains(got, "{{") {
		t.Fatalf("literal {{tokens}} leaked to the client; out = %q", got)
	}
}

// TestDeliverChannelGMCPNeutralizesBidi is the #22 regression on the GMCP path: the Comm.Channel.Text
// mirror BYPASSES telnet.Write's sanitizeOutput, so a Trojan-Source bidi-override control (RLO U+202E and
// the isolates) in a player's channel message would reach a rich client's display verbatim and spoof it.
// The gate must neutralize the override subset on every player-text field (talker/text/msg) while leaving
// legitimate RTL text intact — JSON-escaping alone keeps the bytes wire-safe but not display-safe.
func TestDeliverChannelGMCPNeutralizesBidi(t *testing.T) {
	rlo := string(rune(0x202E))
	pdi := string(rune(0x2069))
	msg := commbus.Message{
		Subject:    commbus.ChanSubject("gossip"),
		AuthorName: "Ev" + rlo + "il",                         // spoofed talker
		Body:       "[Gossip] " + "Ev" + rlo + "il: مرحبا",    // rendered line, with legit Arabic that must survive
		Text:       "mo" + rlo + "re" + pdi + " loot for you", // raw msg
	}
	var out bytes.Buffer
	tc := telnet.NewReadWriter(bytes.NewReader([]byte{255, 253, 201}), &out) // IAC DO 201 → GMCP enabled
	tc.ReadLine()
	g := newGMCPState()
	g.setSupports([]string{"Comm"})
	cc := &commsClient{log: discardLog(), tc: tc, gmcp: g, ignore: map[string]struct{}{}}
	cc.deliverChannel(msg)
	got := out.String()
	if strings.ContainsRune(got, 0x202E) || strings.ContainsRune(got, 0x2069) {
		t.Fatalf("bidi-override control leaked into the Comm.Channel.Text GMCP payload; out = %q", got)
	}
	if !strings.Contains(got, `"talker":"Evil"`) {
		t.Fatalf("talker not neutralized to plain text; out = %q", got)
	}
	if !strings.Contains(got, `"msg":"more loot for you"`) {
		t.Fatalf("raw msg not neutralized; out = %q", got)
	}
	if !strings.Contains(got, "مرحبا") { // legitimate Arabic in the rendered text survives
		t.Fatalf("legitimate Arabic stripped from the GMCP text field; out = %q", got)
	}
}

// TestApplyConfigEmitsChannelList pins Comm.Channel.List (#49): when the world publishes a hear-set, a
// client that advertised Comm.Channel.List gets a sorted array of its usable channel refs (one tab each);
// a client that did not advertise it gets none.
func TestApplyConfigEmitsChannelList(t *testing.T) {
	body, err := commbus.MarshalConfig(commbus.ConfigPayload{HearChannels: []string{"gossip", "auction", "chat"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := commbus.Message{Body: body}

	// Advertised → the sorted list frame is emitted.
	var out bytes.Buffer
	tc := telnet.NewReadWriter(bytes.NewReader([]byte{255, 253, 201}), &out) // IAC DO 201 → GMCP enabled
	tc.ReadLine()
	g := newGMCPState()
	g.setSupports([]string{"Comm"})
	cc := &commsClient{
		log: discardLog(), tc: tc, gmcp: g, bus: commbus.OpenGate("", nil),
		chanSubs: map[string]commbus.Subscription{}, rosterSubs: map[string]commbus.Subscription{}, ignore: map[string]struct{}{},
	}
	cc.applyConfig(cfg)
	got := out.String()
	if !strings.Contains(got, string([]byte{255, 250, 201})+"Comm.Channel.List ") {
		t.Fatalf("no Comm.Channel.List GMCP frame; out = %q", got)
	}
	if !strings.Contains(got, `["auction","chat","gossip"]`) {
		t.Fatalf("Comm.Channel.List not the sorted ref array; out = %q", got)
	}

	// NOT advertised → no list frame.
	var out2 bytes.Buffer
	tc2 := telnet.NewReadWriter(bytes.NewReader([]byte{255, 253, 201}), &out2)
	tc2.ReadLine()
	cc2 := &commsClient{
		log: discardLog(), tc: tc2, gmcp: newGMCPState(), bus: commbus.OpenGate("", nil),
		chanSubs: map[string]commbus.Subscription{}, rosterSubs: map[string]commbus.Subscription{}, ignore: map[string]struct{}{},
	}
	cc2.applyConfig(cfg)
	if strings.Contains(out2.String(), "Comm.Channel.List") {
		t.Fatalf("Comm.Channel.List reached a client that didn't advertise it; out = %q", out2.String())
	}
}

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

// TestGMCPCommChannelReachesClient is the channel-routing e2e: a listener advertising "Comm" receives
// a gossip line BOTH as text AND as a structured Comm.Channel.Text GMCP frame.
func TestGMCPCommChannelReachesClient(t *testing.T) {
	const addr = "addr-a"
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })

	h := newHarness(t)
	h.addShardWithComms("midgaard", addr, nil, nil, core.WorldHandle())
	h.serveGateWithComms(directory.Static{Addr: addr}, core.GateHandle())

	speaker := h.dial(t)
	speaker.login(t, "Speaker")
	speaker.expect(t, "Temple Square")

	listener := h.dial(t)
	listener.expectBytes(t, []byte{255, 251, 201}) // the GMCP offer
	if _, err := listener.conn.Write([]byte{255, 253, 201}); err != nil {
		t.Fatal(err)
	}
	listener.sendGMCP(t, "Core.Supports.Set", `["Comm 1"]`)
	listener.login(t, "Listener")
	listener.expect(t, "Temple Square")

	// Wait until the listener's gate has subscribed to gossip (the async hear-set), then a fresh gossip
	// arrives as a Comm.Channel.Text GMCP frame.
	syncChannelLive(t, speaker, listener)
	speaker.send(t, "gossip hello gmcp world")
	listener.expectBytes(t, append([]byte{255, 250, 201}, []byte("Comm.Channel.Text ")...))
	listener.close(t)
	speaker.close(t)
}

// TestGMCPCharItemsReachesClient is the inventory-panel e2e: a client advertising "Char" receives a
// Char.Items.List frame on login (Char.Items.List is under the advertised Char ancestor).
func TestGMCPCharItemsReachesClient(t *testing.T) {
	h := newHarness(t)
	const addr = "addr-a"
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.expectBytes(t, []byte{255, 251, 201})
	if _, err := term.conn.Write([]byte{255, 253, 201}); err != nil {
		t.Fatal(err)
	}
	term.sendGMCP(t, "Core.Supports.Set", `["Char 1"]`)
	term.login(t, "Bagman")
	term.expectBytes(t, []byte{255, 250, 201, 'C', 'h', 'a', 'r', '.', 'I', 't', 'e', 'm', 's', '.', 'L', 'i', 's', 't', ' '})
	term.close(t)
}

// TestGMCPRoomInfoReachesClient is the minimap e2e: a client advertising "Room" receives Room.Info
// (IAC SB 201 "Room.Info" …) on login, emitted by the world's look path and framed by the gate.
func TestGMCPRoomInfoReachesClient(t *testing.T) {
	h := newHarness(t)
	const addr = "addr-a"
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.expectBytes(t, []byte{255, 251, 201})
	if _, err := term.conn.Write([]byte{255, 253, 201}); err != nil { // IAC DO 201
		t.Fatal(err)
	}
	term.sendGMCP(t, "Core.Supports.Set", `["Room 1"]`)

	term.login(t, "Walker")
	term.expectBytes(t, []byte{255, 250, 201, 'R', 'o', 'o', 'm', '.', 'I', 'n', 'f', 'o', ' '})
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

// TestGMCPCharItemsUpdateReachesClient closes the one wire-level gap issue #142 flagged that the
// world-unit and generic-framing tests don't compose end-to-end: an INCREMENTAL Char.Items delta —
// specifically the count-bump Char.Items.Update — framed onto a real GMCP client's wire, not just
// the login Char.Items.List snapshot (TestGMCPCharItemsReachesClient). The market floor resets five
// identical torches (demo.yaml); picking up a third coalesces the carried group to count 3, which the
// world emits as a same-id Char.Items.Update (world/gmcp.go — payload proven exact, incl. the stable
// g<hash> id and count:3, by world.TestCharItemsCoalescesCount) and the gate frames as
// IAC SB 201 "Char.Items.Update" …. Asserting both the package header AND the count:3 payload on the
// wire proves the incremental delta (not a full List re-send or a Remove+Add churn) reaches the client.
//
// The remaining #142 sub-cases: the gauge-leak filter is pinned ON THE WIRE by
// TestGMCPCharVitalsGaugeFilterOnWire (below); the Comm.Channel.Text rendered/raw split and its
// {{token}} stripping are gate-level covered by TestDeliverChannelEmitsGMCP +
// TestDeliverChannelGMCPStripsColorTokens. The Room.Info NAME {{token}} strip is covered at the
// world-unit tier (world.TestGMCPPayloadsStripColorTokens) rather than end-to-end: the demo ships no
// room whose display name carries a {{token}}, so a gate-wire version would need a synthetic content
// fixture — disproportionate given the gate's GMCP framing is package-agnostic (proven by the
// ...ReachesClient tests + TestRenderFrameGMCPFilter), so what it frames is exactly the world's
// already-stripped payload.
func TestGMCPCharItemsUpdateReachesClient(t *testing.T) {
	h := newHarness(t)
	const addr = "addr-a"
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.expectBytes(t, []byte{255, 251, 201})                        // gate offers IAC WILL GMCP
	if _, err := term.conn.Write([]byte{255, 253, 201}); err != nil { // client IAC DO GMCP
		t.Fatal(err)
	}
	term.sendGMCP(t, "Core.Supports.Set", `["Char 1"]`) // advertise the Char package (Char.Items.* is under it)

	term.login(t, "Stacker")
	term.expect(t, "Temple Square")
	term.send(t, "north")
	term.expect(t, "Market Square")

	// Pick up three of the five identical market torches. Each get flushes an inventory delta on the
	// prompt; the third bumps the coalesced group to count 3. Sends are paced by the synchronous
	// net.Pipe (each blocks until the gate reads it), so they reach the world in order.
	for i := 0; i < 3; i++ {
		term.send(t, "get torch")
	}

	// The incremental delta framed onto the wire: a Char.Items.Update carrying the group at count 3.
	term.expectBytes(t, []byte("Char.Items.Update "))
	term.expectBytes(t, []byte(`"count":3`))
	term.close(t)
}

// TestGMCPCharVitalsGaugeFilterOnWire pins the #50 gauge filter AT THE GATE-WIRE tier issue #142
// asks for (the world-unit filter is proven by world.TestHUDResourceRefsGaugeFilter; this proves the
// already-filtered payload survives the gate's GMCP framing onto a real client). The demo flags hp+mana
// gauge:true and leaves the internal per-round `reactions` pool unflagged, so the Char.Vitals the world
// emits on the login prompt must show hp but never leak the reaction budget to a rich client's gauges.
//
// The absence assertion is airtight because expectGMCPPayload reads the COMPLETE Char.Vitals frame
// (through its closing IAC SE) before we inspect it, and it scopes to that frame's payload — so a later
// unrelated byte can neither satisfy nor mask the "no reactions" check.
func TestGMCPCharVitalsGaugeFilterOnWire(t *testing.T) {
	h := newHarness(t)
	const addr = "addr-a"
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.expectBytes(t, []byte{255, 251, 201})                        // gate offers IAC WILL GMCP
	if _, err := term.conn.Write([]byte{255, 253, 201}); err != nil { // client IAC DO GMCP
		t.Fatal(err)
	}
	term.sendGMCP(t, "Core.Supports.Set", `["Char 1"]`) // advertise Char (Char.Vitals is under it)
	term.login(t, "Gauger")

	payload := term.expectGMCPPayload(t, "Char.Vitals")
	if !strings.Contains(payload, `"hp"`) {
		t.Fatalf("Char.Vitals wire payload missing the gauged hp pool: %q", payload)
	}
	if strings.Contains(payload, "reactions") {
		t.Fatalf("Char.Vitals leaked the un-gauged internal reactions pool onto the wire: %q", payload)
	}
	term.close(t)
}
