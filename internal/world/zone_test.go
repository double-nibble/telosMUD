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
// drive the zone actor directly via its inbox and read each player's out channel.

func newTestPlayer(name string) *player {
	return &player{id: name, name: name, out: make(chan *playv1.ServerFrame, 64)}
}

// waitMarkup waits until an Output frame whose markup contains substr arrives,
// skipping prompt/attached frames.
func waitMarkup(t *testing.T, p *player, substr string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-p.out:
			if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
				return
			}
		case <-deadline:
			t.Fatalf("player %s: timed out waiting for %q", p.name, substr)
		}
	}
}

// nextOutput returns the markup of the next Output frame.
func nextOutput(t *testing.T, p *player) string {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case f := <-p.out:
			if o := f.GetOutput(); o != nil {
				return o.GetMarkup()
			}
		case <-deadline:
			t.Fatalf("player %s: timed out waiting for output", p.name)
			return ""
		}
	}
}

// drain discards any currently-queued frames so a later assertion only sees new ones.
func drain(p *player) {
	for {
		select {
		case <-p.out:
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

	alice := newTestPlayer("Alice")
	bob := newTestPlayer("Bob")

	// Alice joins, sees the temple.
	z.post(joinMsg{p: alice})
	waitMarkup(t, alice, "The Temple Square")

	// Bob joins: Alice (already present) sees the arrival; Bob's look lists Alice.
	z.post(joinMsg{p: bob})
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
