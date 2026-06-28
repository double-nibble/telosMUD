package gate

// channel_journey_test.go is the black-box (player-visible, through the gate) test for Phase-8 slice
// 8.3's DELIVERY done-when: two players on DIFFERENT shards both see a `gossip` line, rendered with the
// content channel's format. It reuses the two-shard handoff harness + a shared commbus.MemBus: each
// world shard is wired with the MemBus WORLD handle (the source — it publishes the channel line through
// the real cmdChannel publish path), and the gate is wired with the SAME MemBus's GATE handle (the sink
// — it renders). A `gossip` typed by a player on shard A fans out over the bus and reaches the socket of
// a player on shard B (and the co-located shard-A player too — exactly one delivery path, the bus).

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/commbus"
	"github.com/double-nibble/telosmud/internal/directory"
)

// TestCrossShardChannelDelivery is the Phase-8 channel-chat done-when (half the phase done-when): a
// player on shard A and a player on shard B both see a `gossip` line typed by either, rendered with the
// channel's content format. This is the cross-shard fan-out the whole comms layer exists for.
func TestCrossShardChannelDelivery(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	for _, sh := range []struct{ id, addr string }{{"shard-a", "addr-a"}, {"shard-b", "addr-b"}} {
		if err := dir.RegisterShard(ctx, sh.id, sh.addr, directory.DefaultShardLease); err != nil {
			t.Fatal(err)
		}
	}
	if err := dir.RegisterZone(ctx, "midgaard", "shard-a"); err != nil {
		t.Fatal(err)
	}
	if err := dir.RegisterZone(ctx, "darkwood", "shard-b"); err != nil {
		t.Fatal(err)
	}

	// One shared MemBus: each world shard publishes through its WORLD handle; the gate subscribes
	// through its GATE handle. This is the in-process model of one broker, two world publishers, one
	// gate sink — exactly the role split cmd/telos-world (OpenWorld) and cmd/telos-gate (OpenGate) wire.
	core := commbus.NewMemBus() // a WORLD-role MemBus; derive sibling handles over its core
	t.Cleanup(func() { _ = core.Close() })

	h := newHarness(t)
	// Shard B (darkwood) — wired with the WORLD comms handle so a gossip line typed there publishes.
	h.addShardWithComms("darkwood", "addr-b", dir, nil, core.WorldHandle())
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		if addr != "addr-b" {
			return nil, errUnknownShard(addr)
		}
		return h.dialHandoff("addr-b")
	}
	// Shard A (midgaard) — also wired with the WORLD comms handle.
	h.addShardWithComms("midgaard", "addr-a", dir, peers, core.WorldHandle())
	// The gate subscribes through the GATE handle (subscribe-only on chan/tell — the impersonation gate).
	h.serveGateWithComms(homeZoneDir{redis: dir, zone: "midgaard"}, core.GateHandle())

	// Two players: Stayer remains on shard A; Walker walks A→B so they end up on different shards.
	stayer := h.dial(t)
	stayer.login(t, "Stayer")
	stayer.expect(t, "Temple Square")

	walker := h.dial(t)
	walker.login(t, "Walker")
	walker.expect(t, "Temple Square")

	// Walk Walker A→B (temple → market → cross-shard darkwood). Now Walker lives on shard B, Stayer on A.
	walker.send(t, "north")
	walker.expect(t, "Market Square")
	walker.send(t, "north")
	walker.expect(t, "Moonlit Grove") // re-dialed B

	// Stayer (shard A) gossips. Both Stayer (co-located, via the bus) and Walker (cross-shard) must see
	// it, rendered with the content format "[Gossip] Stayer: ...".
	stayer.send(t, "gossip hello across the shards")
	stayer.expect(t, "[Gossip] Stayer: hello across the shards")
	walker.expect(t, "[Gossip] Stayer: hello across the shards")

	// Walker (shard B) gossips back. Stayer (cross-shard) sees it too — the fan-out is bidirectional.
	walker.send(t, "gossip and hello back")
	walker.expect(t, "[Gossip] Walker: and hello back")
	stayer.expect(t, "[Gossip] Walker: and hello back")

	stayer.close(t)
	walker.close(t)
}

// TestChannelLineRendersVerbatimNoTellPrefix guards that a CHANNEL line is rendered by the gate
// VERBATIM (the source world already rendered the full content line into Body) — NOT wrapped in the
// tell stand-in's "X tells you, '...'" framing. A regression that routed channel messages through the
// tell renderer would double-wrap them.
func TestChannelLineRendersVerbatimNoTellPrefix(t *testing.T) {
	const addr = "addr-a"
	core := commbus.NewMemBus()
	t.Cleanup(func() { _ = core.Close() })

	h := newHarness(t)
	h.addShardWithComms("midgaard", addr, nil, nil, core.WorldHandle())
	h.serveGateWithComms(directory.Static{Addr: addr}, core.GateHandle())

	term := h.dial(t)
	term.login(t, "Talker")
	term.expect(t, "Temple Square")

	term.send(t, "gossip verbatim please")
	term.expect(t, "[Gossip] Talker: verbatim please")
	if got := term.acc.String(); strings.Contains(got, "tells you, '[Gossip]") {
		t.Fatalf("a channel line was wrapped in the tell renderer: %q", got)
	}

	term.close(t)
}
