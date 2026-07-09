package gate

import (
	"bytes"
	"strings"
	"testing"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/telnet"
)

// TestDeliverRosterEmitsGMCP pins the #90 gate emit: a per-channel roster (director-authored listener set)
// delivered to a client that advertised "Comm.Channel.Players" is re-emitted as structured
// Comm.Channel.Players {channel, players}; a client that did not advertise it gets nothing.
func TestDeliverRosterEmitsGMCP(t *testing.T) {
	msg := commbus.Message{
		Subject: commbus.RosterSubject("gossip"),
		Body:    `{"channel":"gossip","players":["Ana","Bo"]}`,
	}

	// Advertised -> the Comm.Channel.Players frame is emitted with the channel + listener names.
	var out bytes.Buffer
	tc := telnet.NewReadWriter(bytes.NewReader([]byte{255, 253, 201}), &out) // IAC DO 201 -> GMCP enabled
	tc.ReadLine()
	g := newGMCPState()
	g.setSupports([]string{"Comm.Channel.Players"})
	cc := &commsClient{log: discardLog(), tc: tc, gmcp: g, ignore: map[string]struct{}{}}
	cc.deliverRoster(msg)
	got := out.String()
	if !strings.Contains(got, string([]byte{255, 250, 201})+"Comm.Channel.Players ") {
		t.Fatalf("no Comm.Channel.Players GMCP frame; out = %q", got)
	}
	if !strings.Contains(got, `"channel":"gossip"`) || !strings.Contains(got, `"players":["Ana","Bo"]`) {
		t.Fatalf("Comm.Channel.Players payload missing channel/players; out = %q", got)
	}

	// NOT advertised -> no subnegotiation at all.
	var out2 bytes.Buffer
	tc2 := telnet.NewReadWriter(bytes.NewReader([]byte{255, 253, 201}), &out2)
	tc2.ReadLine()
	cc2 := &commsClient{log: discardLog(), tc: tc2, gmcp: newGMCPState(), ignore: map[string]struct{}{}}
	cc2.deliverRoster(msg)
	if strings.Contains(out2.String(), string([]byte{255, 250, 201})) {
		t.Fatalf("a roster GMCP frame reached a client that didn't advertise Comm.Channel.Players; out = %q", out2.String())
	}
}

// TestDeliverRosterNeutralizesNames is the #22/#98 regression on the roster path: this GMCP emit bypasses
// telnet.Write's sanitizeOutput, so a crafted display name (color markup + a Trojan-Source bidi override)
// must be neutralized before it reaches a rich client's channel panel.
func TestDeliverRosterNeutralizesNames(t *testing.T) {
	rlo := string(rune(0x202E))
	msg := commbus.Message{
		Subject: commbus.RosterSubject("gossip"),
		Body:    `{"channel":"gossip","players":["{{BOLD}}Ana{{RESET}}","Ev` + rlo + `il"]}`,
	}
	var out bytes.Buffer
	tc := telnet.NewReadWriter(bytes.NewReader([]byte{255, 253, 201}), &out)
	tc.ReadLine()
	g := newGMCPState()
	g.setSupports([]string{"Comm.Channel.Players"})
	cc := &commsClient{log: discardLog(), tc: tc, gmcp: g, ignore: map[string]struct{}{}}
	cc.deliverRoster(msg)
	got := out.String()
	if strings.ContainsRune(got, 0x202E) {
		t.Fatalf("bidi override leaked into the roster payload; out = %q", got)
	}
	if strings.Contains(got, "{{") {
		t.Fatalf("literal {{tokens}} leaked into the roster payload; out = %q", got)
	}
	if !strings.Contains(got, `"Ana"`) || !strings.Contains(got, `"Evil"`) {
		t.Fatalf("names not neutralized to plain text; out = %q", got)
	}
}
