package gate

// comms_toggle_journey_test.go is the black-box (player-visible, through the gate) test for Phase-8
// slice 8.6's end-to-end receiver-side enforcement on a REAL world: the channel toggle re-subscribe
// (`channels off` stops the lines, `channels on` resumes them) and the ignore funnel (an ignored
// author's channel line is dropped at the receiver). It drives the real cmdChannels/cmdIgnore world
// commands → the world re-publishes the per-player comms-config → the gate re-subscribes / re-filters.

import (
	"strings"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
)

// TestChannelToggleStopsAndResumesLines is the toggle-unsubscribe done-when on a real world: a player
// gossips and a second player hears it; the second player `channels off gossip` and the next gossip does
// NOT reach them; `channels on gossip` resumes delivery. The world re-publishes the hear-set on each
// toggle and the gate (un)subscribes the concrete channel subject.
func TestChannelToggleStopsAndResumesLines(t *testing.T) {
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
	listener.login(t, "Listener")
	listener.expect(t, "Temple Square")

	// Baseline: Listener (gossip default_on) hears Speaker's gossip.
	speaker.send(t, "gossip one")
	listener.expect(t, "[Gossip] Speaker: one")

	// Listener turns gossip OFF. The world re-publishes an updated hear-set; the gate unsubscribes gossip.
	listener.send(t, "channels off gossip")
	listener.expect(t, "disable the Gossip channel")
	// Give the config re-publish + the gate unsubscribe a beat to take effect before the next line.
	time.Sleep(100 * time.Millisecond)

	speaker.send(t, "gossip two")
	// Speaker still hears their own line (they kept gossip on) — a positive checkpoint to order against.
	speaker.expect(t, "[Gossip] Speaker: two")
	// Listener must NOT have received "two". Drive a tell to Listener as a sentinel that DOES arrive, then
	// assert "two" never showed up before it.
	time.Sleep(150 * time.Millisecond)
	if got := listener.acc.String(); strings.Contains(got, "Speaker: two") {
		t.Fatalf("a gossip line arrived after `channels off gossip`: %q", got)
	}

	// Listener turns gossip back ON: delivery resumes.
	listener.send(t, "channels on gossip")
	listener.expect(t, "enable the Gossip channel")
	time.Sleep(100 * time.Millisecond)
	speaker.send(t, "gossip three")
	listener.expect(t, "[Gossip] Speaker: three")

	speaker.close(t)
	listener.close(t)
}

// TestIgnoreFunnelEndToEnd is the receiver-side ignore done-when on a real world: a player ignores
// another, and the ignored author's gossip line is dropped at the receiver's gate (the funnel), while a
// non-ignored author's line passes. Drives the real `ignore` command → the world re-publishes the config
// with the ignore list → the gate's funnel drops by author id.
func TestIgnoreFunnelEndToEnd(t *testing.T) {
	const addr = "addr-a"
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })

	h := newHarness(t)
	h.addShardWithComms("midgaard", addr, nil, nil, core.WorldHandle())
	h.serveGateWithComms(directory.Static{Addr: addr}, core.GateHandle())

	troll := h.dial(t)
	troll.login(t, "Troll")
	troll.expect(t, "Temple Square")

	friend := h.dial(t)
	friend.login(t, "Friend")
	friend.expect(t, "Temple Square")

	listener := h.dial(t)
	listener.login(t, "Listener")
	listener.expect(t, "Temple Square")

	// Listener ignores Troll.
	listener.send(t, "ignore Troll")
	listener.expect(t, "now ignoring Troll")
	time.Sleep(100 * time.Millisecond)

	// Troll gossips: dropped at Listener's gate.
	troll.send(t, "gossip from the troll")
	troll.expect(t, "[Gossip] Troll: from the troll") // Troll hears their own line
	// Friend gossips: a non-ignored author — Listener DOES hear this. Use it as the ordering sentinel.
	friend.send(t, "gossip from a friend")
	listener.expect(t, "[Gossip] Friend: from a friend")

	if got := listener.acc.String(); strings.Contains(got, "Troll: from the troll") {
		t.Fatalf("an ignored author's gossip reached the receiver: %q", got)
	}

	troll.close(t)
	friend.close(t)
	listener.close(t)
}
