package gate

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/world"
)

// reresolve_journey_test.go covers #324: a fresh-login Attach refused by a DRAINING shard with
// codes.Unavailable must make the gate re-resolve through the directory and re-dial the peer the zone
// leases have flipped to — on the SAME socket — instead of dropping the player with "world unreachable".
//
// The zero-drop drain story (Round 22) covered RESIDENT players; this covers ARRIVING ones.

// flipDir is a stateful directory: it answers with a sequence of addresses on successive
// ShardForCharacter calls, so a test can simulate the lease flipping from a draining shard to its peer
// between the gate's first resolve and its re-resolve. Once the sequence is exhausted it repeats the last.
type flipDir struct {
	mu    sync.Mutex
	calls int
	addrs []string
}

func (d *flipDir) ShardForCharacter(string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := d.calls
	if i >= len(d.addrs) {
		i = len(d.addrs) - 1
	}
	d.calls++
	return d.addrs[i], true
}

func (d *flipDir) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// markDraining puts a harness shard into the draining state so its Play service refuses a fresh login with
// codes.Unavailable (server.go). BeginDrain with an always-erroring chooser and no players sets draining
// synchronously, hands nothing off, and returns leaving the flag set — a shard that is draining but has no
// peer to drain onto, which is exactly "refuses fresh logins" for the purpose of this test.
func markDraining(t *testing.T, sh *world.Shard) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	noTarget := func(string, int) (string, string, error) { return "", "", fmt.Errorf("no target (test)") }
	if _, err := sh.BeginDrain(ctx, noTarget, 500*time.Millisecond); err != nil {
		t.Fatalf("BeginDrain: %v", err)
	}
}

// TestFreshLoginReResolvesPastADrainingShard is the #324 core: the directory first points the gate at a
// draining shard (which refuses the fresh login), then — on the gate's re-resolve — at a healthy peer. The
// player must land on the peer with the socket never dropped, not see "world unreachable".
//
// Both shards host the demo midgaard, so both land a login in Temple Square. Because a DRAINING shard
// refuses EVERY fresh login, a successful "Temple Square" is proof the player landed on the peer B, not A.
func TestFreshLoginReResolvesPastADrainingShard(t *testing.T) {
	h := newHarness(t)
	shardA := h.addShard("midgaard", "addr-a", nil, nil)
	h.addShard("midgaard", "addr-b", nil, nil)

	// A is draining and refuses fresh logins; the directory flips to B on the re-resolve.
	markDraining(t, shardA)
	dir := &flipDir{addrs: []string{"addr-a", "addr-b"}}
	h.serveGate(dir)

	term := h.dial(t)
	term.login(t, "Rerouted")
	// A refuses all fresh logins, so landing in Temple Square proves the gate re-resolved to B.
	term.expect(t, "Temple Square")

	if got := dir.count(); got < 2 {
		t.Fatalf("directory was resolved %d time(s); the gate must RE-RESOLVE after an Unavailable refusal (#324)", got)
	}

	// And the player is live on B: a command gets a response.
	term.send(t, "say hello")
	term.expect(t, "You say, 'hello'")
}

// TestFreshLoginGivesUpAfterMaxRetries: if every candidate keeps refusing (a fully saturated / all-draining
// fleet), the gate must not spin forever. After a bounded number of re-resolves it tells the player to
// reconnect and closes the socket — a clean refusal, never a hang.
func TestFreshLoginGivesUpAfterMaxRetries(t *testing.T) {
	h := newHarness(t)
	shardA := h.addShard("midgaard", "addr-a", nil, nil)
	markDraining(t, shardA)

	// The directory only ever knows the draining shard.
	dir := &flipDir{addrs: []string{"addr-a"}}
	h.serveGate(dir)

	term := h.dial(t)
	term.login(t, "Doomed")
	term.expect(t, "busy right now")
	term.expectClose(t)

	// It tried the initial resolve plus the bounded retries, not once and not forever.
	if got := dir.count(); got < 2 || got > maxAttachRetries+2 {
		t.Fatalf("directory resolved %d times; want a bounded re-resolve loop (2..%d)", got, maxAttachRetries+2)
	}
}
