package gate

import (
	"testing"

	"github.com/double-nibble/telosmud/internal/directory"
)

// TestShardDropWhileConnected is the headline chaos test: a player is live on a shard and
// that shard DROPS (its gRPC server stops, the Play stream dies) with no redirect. We
// inject the failure, then assert the player's DEFINED experience and the gate's reaction.
//
// CURRENT BEHAVIOR (asserted here): the gate's writer loop sees the Play stream's Recv
// return an error with NO pending redirect, and per gate.go runWriter it CLOSES the
// player's socket (b.conn.nc.Close()), which unwinds the whole connection. So the
// observable for the player is: the connection DROPS. There is no auto-reconnect and no
// in-band "the world hiccuped, hold on" message — the socket simply closes.
//
// This test first PROVES the failure reproduces (the player is live, then the shard is
// dropped) and then locks in the observable (socket closes within a deadline). It is the
// reproduction-before-assertion discipline: drop the live shard, confirm the player's
// connection tears down.
//
// CONTRACT QUESTION (for the edge-engineer / distributed-systems-architect): is
// "silently drop the socket" the intended player experience for a shard crash? The
// directory still routes this character's home zone to the (now-dead) shard, so even a
// reconnect attempt would fail until the directory's lease expires and another shard
// claims the zone. Plausible better contracts: (a) emit a "The world stumbled..."
// disconnect notice before closing so the player knows it was the server, not them; or
// (b) the gate retries the directory + re-dials a healthy shard that has rehydrated the
// player from the durable snapshot (the crash-failover path PLACEMENT.md §6 describes).
// This test documents (and pins) today's behavior; when the contract is decided, update
// the assertion to match.
func TestShardDropWhileConnected(t *testing.T) {
	const addr = "addr-a"

	h := newHarness(t)
	h.addShard("midgaard", addr, nil, nil)
	h.serveGate(directory.Static{Addr: addr})

	// The player connects and is demonstrably LIVE (a look rendered, a say echoed) — so
	// the drop below interrupts a working session, not a half-open one.
	term := h.dial(t)
	term.login(t, "Doomed")
	term.expect(t, "Temple Square")
	term.send(t, "say still here")
	term.expect(t, "You say, 'still here'")

	// INJECT THE FAILURE: the shard the player is on disappears (server stops, listener
	// closes, zone goroutine cancelled). The gate's Play stream to it now dies.
	h.dropShard(addr)

	// ASSERT THE OBSERVABLE: the player's socket closes (the reader sees EOF) within a
	// deadline. This is the current contract — a shard crash drops the connection.
	term.expectClose(t)

	// And the gate's handle goroutine returns (the connection fully tore down, not a
	// leaked half-open bridge). close() waits on the handle's done channel.
	term.close(t)
}

// TestShardDropDoesNotHangOtherPlayers proves the blast radius of one shard drop is the
// connection on it — a SECOND player on a SECOND, healthy shard keeps working after the
// first shard dies. This guards against a shared-pool or gate-wide lock turning one
// shard's death into a whole-gate stall.
func TestShardDropDoesNotHangOtherPlayers(t *testing.T) {
	const addrA, addrB = "addr-a", "addr-b"

	h := newHarness(t)
	h.addShard("midgaard", addrA, nil, nil)
	h.addShard("midgaard", addrB, nil, nil) // a second, independent single-zone shard

	// A directory that routes by character name: Alpha -> shard A, everyone else -> B.
	dir := splitDir{a: addrA, b: addrB, aChar: "Alpha"}
	h.serveGate(dir)

	// Both players live.
	alpha := h.dial(t)
	alpha.login(t, "Alpha")
	alpha.expect(t, "Temple Square")

	beta := h.dial(t)
	beta.login(t, "Beta")
	beta.expect(t, "Temple Square")

	// Drop ONLY shard A. Alpha's connection should tear down; Beta must be unaffected.
	h.dropShard(addrA)
	alpha.expectClose(t)
	alpha.close(t)

	// Beta is still live: a fresh command round-trips on the surviving shard.
	beta.send(t, "say survivor")
	beta.expect(t, "You say, 'survivor'")
	beta.close(t)
}

// splitDir routes one named character to shard a and all others to shard b, so a test can
// place two players on two different shards through the real ShardForCharacter seam.
type splitDir struct {
	a, b  string
	aChar string
}

func (d splitDir) ShardForCharacter(characterID string) (string, bool) {
	if characterID == d.aChar {
		return d.a, true
	}
	return d.b, true
}
