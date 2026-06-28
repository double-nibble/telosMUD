package gate

// comms_toggle_journey_test.go is the black-box (player-visible, through the gate) test for Phase-8
// slice 8.6's end-to-end receiver-side enforcement on a REAL world: the channel toggle re-subscribe
// (`channels off` stops the lines, `channels on` resumes them) and the ignore funnel (an ignored
// author's channel line is dropped at the receiver). It drives the real cmdChannels/cmdIgnore world
// commands → the world re-publishes the per-player comms-config → the gate re-subscribes / re-filters.
//
// Determinism: channel delivery to a player depends on the ASYNC login → config-publish → gate-subscribe
// round-trip (and, on a toggle/ignore, the re-publish → re-apply round-trip). A line sent before that
// round-trip completes is legitimately missed. So these tests never sleep-and-hope against the latency;
// they retry to a deterministic FLOWING / STOPPED / DROPPED state via tryExpect, with unique tokens so a
// prior line in acc never satisfies a later check.

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
)

// toggleTokSeq mints process-unique gossip tokens so a token heard in one sync phase can never satisfy a
// tryExpect/Contains in a later phase (acc is cumulative).
var toggleTokSeq atomic.Uint64

func toggleTok(prefix string) string { return fmt.Sprintf("%s%d", prefix, toggleTokSeq.Add(1)) }

// syncChannelLive blocks until `listener` is provably receiving `speaker`'s gossip — i.e. the listener's
// gate has applied its hear-set config and SUBSCRIBED to the channel. It retries a unique gossip until
// one is heard (the deterministic "channel is live" sync point).
func syncChannelLive(t *testing.T, speaker, listener *terminal) {
	t.Helper()
	for i := 0; i < 80; i++ {
		tok := toggleTok("live")
		speaker.send(t, "gossip "+tok)
		if listener.tryExpect(tok, 250*time.Millisecond) {
			return
		}
	}
	t.Fatal("channel never became live for the listener (the hear-set subscription was not established)")
}

// confirmChannelStopped blocks until `listener` provably STOPS receiving `speaker`'s gossip — i.e. the
// gate has unsubscribed after a `channels off` re-publish. Each attempt confirms the world processed the
// gossip (speaker hears their own line) and then checks it did NOT reach the listener; it retries to
// absorb the async config-republish/unsubscribe latency.
func confirmChannelStopped(t *testing.T, speaker, listener *terminal) {
	t.Helper()
	for i := 0; i < 80; i++ {
		tok := toggleTok("stop")
		speaker.send(t, "gossip "+tok)
		speaker.expect(t, tok) // the world processed it (speaker keeps gossip on, hears their own line)
		if !listener.tryExpect(tok, 250*time.Millisecond) {
			return // a world-processed gossip did NOT reach the listener: the unsubscribe has settled
		}
	}
	t.Fatal("channel lines did not stop for the listener after `channels off`")
}

// confirmAuthorDropped blocks until `dropped`'s gossip is provably filtered at `listener` while
// `sentinel`'s still flows — i.e. the ignore-config has applied at the gate. Each attempt sends a unique
// gossip from BOTH (published in order on the one channel subject); once the sentinel's line arrives
// WITHOUT the dropped author's (which was processed first and funnel-dropped), the ignore is in effect.
func confirmAuthorDropped(t *testing.T, dropped, sentinel, listener *terminal) {
	t.Helper()
	for i := 0; i < 80; i++ {
		dt := toggleTok("drop")
		st := toggleTok("pass")
		dropped.send(t, "gossip "+dt)
		sentinel.send(t, "gossip "+st)
		listener.expect(t, st) // the sentinel arrives, ordering the dropped author's delivery window
		if !strings.Contains(listener.acc.String(), dt) {
			return // the ignored author's line did NOT arrive before the sentinel: the funnel is in effect
		}
	}
	t.Fatal("the ignored author's gossip was not dropped at the receiver")
}

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

	// Baseline: wait until the Listener's gate has applied its hear-set + subscribed to gossip.
	syncChannelLive(t, speaker, listener)

	// Listener turns gossip OFF → the world re-publishes an updated hear-set → the gate unsubscribes.
	// Confirm the lines STOP (deterministically, absorbing the async unsubscribe).
	listener.send(t, "channels off gossip")
	listener.expect(t, "disable the Gossip channel")
	confirmChannelStopped(t, speaker, listener)

	// Listener turns gossip back ON → delivery resumes (the re-sync proves the re-subscribe).
	listener.send(t, "channels on gossip")
	listener.expect(t, "enable the Gossip channel")
	syncChannelLive(t, speaker, listener)

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

	// Listener ignores Troll → the world re-publishes the config with the ignore list.
	listener.send(t, "ignore Troll")
	listener.expect(t, "now ignoring Troll")

	// Ensure the Listener's gate is subscribed to gossip (hears Friend) — the async config round-trip.
	syncChannelLive(t, friend, listener)
	// Then confirm Troll's gossip is DROPPED at the Listener (the ignore-config applied) while Friend's
	// still flows — deterministically, absorbing the async re-apply.
	confirmAuthorDropped(t, troll, friend, listener)

	troll.close(t)
	friend.close(t)
	listener.close(t)
}
