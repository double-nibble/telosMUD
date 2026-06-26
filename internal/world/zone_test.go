package world

import (
	"context"
	"strings"
	"testing"
	"time"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// White-box tests for the multiplayer paths the gRPC slice_test doesn't exercise:
// arrival/departure broadcasts, say-to-others, who, and movement messaging. These
// drive the zone actor directly via its inbox and read each session's out channel.

// newTestPlayerEntity builds a session + its in-world entity in zone z (the post-split
// successor to the old &player{...}), so white-box tests can join a player directly.
// The entity is created through the zone (rid/owner wired) and linked to the session via
// newPlayerEntity; epoch starts at 1 like a fresh login.
func newTestPlayerEntity(z *Zone, name string) *session {
	s := &session{character: name, out: make(chan *playv1.ServerFrame, 64), epoch: 1}
	z.newPlayerEntity(s, name)
	return s
}

// TestDispatchSafeRecoversHandlerPanic proves a panicking command handler cannot crash the
// zone goroutine — which, unrecovered, would be fatal to the whole world process and every
// player on it. A session whose entity never joined a room (location==nil) makes "look"
// null-deref; dispatchSafe must recover, keep running, and send the player an error.
func TestDispatchSafeRecoversHandlerPanic(t *testing.T) {
	z := newZone("test")
	s := newTestPlayerEntity(z, "Boom") // entity created but never joined -> location is nil

	z.dispatchSafe(s, "look") // would panic in lookRoom; must be recovered, not propagate

	select {
	case <-s.out: // received the generic-error / prompt frame: the zone survived the panic
	default:
		t.Fatal("dispatchSafe recovered the panic but produced no output to the player")
	}
}

// waitMarkup waits until an Output frame whose markup contains substr arrives,
// skipping prompt/attached frames.
func waitMarkup(t *testing.T, s *session, substr string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
				return
			}
		case <-deadline:
			t.Fatalf("player %s: timed out waiting for %q", s.character, substr)
		}
	}
}

// nextOutput returns the markup of the next Output frame.
func nextOutput(t *testing.T, s *session) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-s.out:
			if o := f.GetOutput(); o != nil {
				return o.GetMarkup()
			}
		case <-deadline:
			t.Fatalf("player %s: timed out waiting for output", s.character)
			return ""
		}
	}
}

// drain discards any currently-queued frames so a later assertion only sees new ones.
func drain(s *session) {
	for {
		select {
		case <-s.out:
		default:
			return
		}
	}
}

func TestZoneMultiplayer(t *testing.T) {
	z := NewDemoShard().Zone()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go z.Run(ctx)

	alice := newTestPlayerEntity(z, "Alice")
	bob := newTestPlayerEntity(z, "Bob")

	// Alice joins, sees the temple.
	z.post(joinMsg{s: alice})
	waitMarkup(t, alice, "The Temple Square")

	// Bob joins: Alice (already present) sees the arrival; Bob's look lists Alice.
	z.post(joinMsg{s: bob})
	waitMarkup(t, alice, "Bob arrives.")
	waitMarkup(t, bob, "Alice is here.")

	// say broadcasts to others in the room, and echoes to the speaker.
	drain(alice)
	drain(bob)
	z.post(inputMsg{id: "Alice", line: "say hi"})
	waitMarkup(t, alice, "You say, 'hi'")
	waitMarkup(t, bob, "Alice says, 'hi'")

	// who lists everyone in the zone.
	drain(bob)
	z.post(inputMsg{id: "Bob", line: "who"})
	who := nextOutput(t, bob)
	if !strings.Contains(who, "Alice") || !strings.Contains(who, "Bob") {
		t.Fatalf("who output missing a player: %q", who)
	}

	// Movement: Alice leaves north; Bob (left behind in the temple) sees it; Alice
	// arrives in the market.
	drain(alice)
	drain(bob)
	z.post(inputMsg{id: "Alice", line: "north"})
	waitMarkup(t, bob, "Alice leaves north.")
	waitMarkup(t, alice, "Market Square")

	// Unknown verb is rejected.
	drain(bob)
	z.post(inputMsg{id: "Bob", line: "frobnicate"})
	waitMarkup(t, bob, "Huh?")

	// Leaving removes the player; a fresh who from Alice no longer lists Bob.
	z.post(leaveMsg{id: "Bob"})
	drain(alice)
	z.post(inputMsg{id: "Alice", line: "who"})
	who2 := nextOutput(t, alice)
	if strings.Contains(who2, "Bob") {
		t.Fatalf("who still lists departed player: %q", who2)
	}
}
