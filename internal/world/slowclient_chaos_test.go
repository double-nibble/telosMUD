package world

import (
	"strconv"
	"strings"
	"testing"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// TestSessionSendNeverBlocksAndCountsDrops pins the Phase-16.3 backpressure contract on session.send: when
// the out buffer is full the frame is DROPPED (the zone goroutine never blocks on a slow client) and the
// per-session counters track it; a later successful enqueue resets the consecutive-drop run (the client
// caught up). The whole test would hang if send ever blocked, which is itself the assertion.
func TestSessionSendNeverBlocksAndCountsDrops(t *testing.T) {
	s := &session{character: "Wedge", out: make(chan *playv1.ServerFrame, 2)}

	// Fill the buffer to capacity — these succeed, no drops.
	s.send(textFrame("a"))
	s.send(textFrame("b"))
	if len(s.out) != 2 || s.framesDropped != 0 {
		t.Fatalf("buffer fill: len=%d dropped=%d, want 2/0", len(s.out), s.framesDropped)
	}

	// Every further send drops, immediately.
	for i := 0; i < 5; i++ {
		s.send(textFrame("drop"))
	}
	if s.framesDropped != 5 {
		t.Fatalf("framesDropped=%d, want 5", s.framesDropped)
	}
	if s.consecutiveDrops != 5 {
		t.Fatalf("consecutiveDrops=%d, want 5", s.consecutiveDrops)
	}

	// Drain one slot; the next send succeeds and resets the consecutive run (but not the lifetime total).
	<-s.out
	s.send(textFrame("ok"))
	if s.consecutiveDrops != 0 {
		t.Fatalf("consecutiveDrops=%d after a successful send, want 0", s.consecutiveDrops)
	}
	if s.framesDropped != 5 {
		t.Fatalf("framesDropped=%d, want still 5 (a success is not a drop)", s.framesDropped)
	}
}

// TestZoneServesOthersDespiteAWedgedClient is the Phase-16.3 chaos test: one co-located player whose out
// channel is NEVER drained (a wedged/dead socket) must not stall the zone or starve other players. A burst
// of room broadcasts fills the wedged player's buffer (drops pile up) while a healthy player keeps hearing
// every line and the zone keeps serving commands — the golden rule, proven under a stuck client.
func TestZoneServesOthersDespiteAWedgedClient(t *testing.T) {
	sh := NewDemoShard()
	z := sh.Zone()
	room := z.rooms[z.startRoom]

	has := func(lines []string, sub string) bool {
		for _, l := range lines {
			if strings.Contains(l, sub) {
				return true
			}
		}
		return false
	}

	// Healthy: a normal buffer we keep draining. Wedged: a depth-1 buffer we NEVER drain.
	healthy := newTestPlayerEntity(z, "Healthy")
	Move(healthy.entity, room)
	wedged := &session{character: "Wedged", out: make(chan *playv1.ServerFrame, 1), epoch: 1}
	z.newPlayerEntity(wedged, "Wedged")
	Move(wedged.entity, room)

	// Burst of broadcasts: Healthy says many lines; each fans out to the co-located Wedged via act() -> send.
	const says = 100
	for i := 0; i < says; i++ {
		drainCombat(healthy) // keep Healthy's own buffer clear so IT never drops
		z.dispatch(healthy, "say chatter "+strconv.Itoa(i))
	}

	// The zone never blocked (the test reached here) and the wedged client piled up drops on its full buffer.
	if wedged.framesDropped == 0 {
		t.Fatal("expected the wedged client to drop frames once its depth-1 buffer filled")
	}

	// The zone still serves a normal command for the healthy player — a stuck peer didn't wedge the loop.
	drainCombat(healthy)
	z.dispatch(healthy, "look")
	if got := drainCombat(healthy); !has(got, "Exits:") {
		t.Fatalf("zone stopped serving the healthy player after a wedged client; got %v", got)
	}
}
