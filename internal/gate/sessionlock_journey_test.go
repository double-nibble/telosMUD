package gate

import (
	"context"
	"testing"
	"time"

	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/sessionlock"
	"github.com/double-nibble/telosmud/internal/world"
)

// sessionlock_journey_test.go — Phase 14.4: the single-session lock end to end. A live player whose lock is
// taken over (a login ELSEWHERE in the fleet) is kicked by its own lock-renewer — the cross-shard takeover
// that the within-shard displacedKick (zone.go) can't see. We simulate the "elsewhere" login by acquiring
// the SHARED lock directly, which is exactly what another shard's login does to the same key.

func TestSessionLockTakeoverKicksDisplacedConnection(t *testing.T) {
	lock := sessionlock.NewMem()
	const addr = "addr-a"
	h := newHarness(t)
	// Small TTL + a fast renew so the takeover is noticed quickly in the test.
	sh := world.NewShard("midgaard", addr, nil, nil).
		WithSessionLock(lock, 500*time.Millisecond, 20*time.Millisecond)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.login(t, "Solo") // legacy name login (no account service wired in this harness)
	term.expect(t, "The Temple Square")

	// A login ELSEWHERE (another shard) takes over the lock for the same character. Poll-acquire until the
	// login connection's OWN lock is actually present first: "The Temple Square" is emitted as the player
	// enters the world, which can slightly precede the renewer's initial Acquire. If our takeover landed in
	// that window the login's Acquire would clobber it and never self-kick (the flake). Acquire returns the
	// PREVIOUS holder, so we know we've truly taken over from the login once prev is its (non-empty,
	// non-ours) token — a synchronized takeover instead of a hopeful one.
	deadline := time.Now().Add(3 * time.Second)
	for {
		prev, err := lock.Acquire(context.Background(), sessionlock.Key("Solo"), "elsewhere-token", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if prev != "" && prev != "elsewhere-token" {
			break // the login connection held the lock; we have now displaced it
		}
		if time.Now().After(deadline) {
			t.Fatal("login connection never acquired its session lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// This connection's renewer notices the lock was lost and kicks it with the takeover notice, then the
	// gate closes the socket.
	term.expect(t, "logged in from another location")
	select {
	case <-term.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("expected the displaced connection to close; got %q", term.acc.String())
	}
}

// TestSessionLockNoSpuriousKick: a live player whose lock is NOT contended keeps playing across several renew
// cycles (the heartbeat renews its own lock, never self-kicking).
func TestSessionLockNoSpuriousKick(t *testing.T) {
	lock := sessionlock.NewMem()
	const addr = "addr-a"
	h := newHarness(t)
	sh := world.NewShard("midgaard", addr, nil, nil).
		WithSessionLock(lock, 500*time.Millisecond, 20*time.Millisecond)
	h.serveShard(addr, sh)
	h.serveGate(directory.Static{Addr: addr})

	term := h.dial(t)
	term.login(t, "Steady")
	term.expect(t, "The Temple Square")

	// Several renew cycles pass; the player is still live and responsive (no spurious kick).
	time.Sleep(120 * time.Millisecond)
	term.send(t, "look")
	term.expect(t, "The Temple Square")
	term.close(t)
}
