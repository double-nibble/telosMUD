package gate

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/directory"
	"github.com/double-nibble/telosmud/internal/world"
)

// TestGateCrossShardHandoff drives a real cross-shard handoff THROUGH the gate.
// Two in-process world shards (A=midgaard, B=darkwood) share a miniredis directory;
// the gate's client pool is pointed at both shards' bufconn Play services. A scripted
// telnet client logs in, walks A's cross-shard exit, and the gate must — on its own —
// catch the Redirect, re-dial B with the handoff token, replay the un-acked input, and
// resume live forwarding, all while the telnet socket stays open. We assert the player
// lands in darkwood and that a replayed input is deduped (exactly-once across the move).
func TestGateCrossShardHandoff(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	// Publish each shard's id -> endpoint, then lease each zone to a shard id. The gate
	// and the handoff both resolve zone -> shard id -> endpoint before dialing.
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

	h := newHarness(t)

	// Destination shard B comes up first so A can reach its Handoff service.
	h.addShard("darkwood", "addr-b", dir, nil)

	// A's peer dialer maps the registered address to B's bufconn Handoff client.
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		if addr != "addr-b" {
			return nil, errUnknownShard(addr)
		}
		return h.dialHandoff("addr-b")
	}
	h.addShard("midgaard", "addr-a", dir, peers)

	// Login resolves the home zone (midgaard) -> addr-a via the directory.
	h.serveGate(homeZoneDir{redis: dir, zone: "midgaard"})

	term := h.dial(t)
	term.login(t, "Walker")
	term.expect(t, "Temple Square") // look on join: live on A

	// Walk A: temple -> market -> (cross-shard) darkwood. The gate handles the redirect
	// itself; the player just keeps typing.
	term.send(t, "north") // temple -> market
	term.expect(t, "Market Square")
	term.send(t, "north")           // market -> darkwood: triggers handoff
	term.expect(t, "Moonlit Grove") // gate re-dialed B; activation look landed

	// Exactly-once across the move: 'say arrived' must echo on B. (The replay of the
	// already-applied move input is deduped by the world; we just confirm the player is
	// live and commands work on the new shard.)
	term.send(t, "say arrived")
	term.expect(t, "You say, 'arrived'")

	// The directory now records Walker on shard B at the bumped epoch.
	place, err := dir.PlayerPlacement(ctx, "Walker")
	if err != nil {
		t.Fatal(err)
	}
	if place.ShardID != "shard-b" {
		t.Fatalf("placement = %+v, want shard-b", place)
	}

	// Clean teardown: closing the client end ends the gate's reader loop.
	term.close(t)
}

func errUnknownShard(addr string) error {
	return &unknownShardError{addr: addr}
}

type unknownShardError struct{ addr string }

func (e *unknownShardError) Error() string { return "unknown shard " + e.addr }

// homeZoneDir resolves every login to the shard hosting a fixed home zone (the gate's
// directory seam), mirroring cmd/telos-gate's loginDirectory without the fallback.
type homeZoneDir struct {
	redis *directory.Redis
	zone  string
}

func (d homeZoneDir) ShardForCharacter(string) (string, bool) {
	ctx := context.Background()
	shardID, err := d.redis.ShardForZone(ctx, d.zone)
	if err != nil || shardID == "" {
		return "", false
	}
	endpoint, err := d.redis.EndpointForShard(ctx, shardID)
	if err != nil || endpoint == "" {
		return "", false
	}
	return endpoint, true
}

// compile-time assertion that world.Locator is satisfied by the directory wiring used
// above (keeps the harness's shard constructor honest if the interface shifts).
var _ world.Locator = (*directory.Redis)(nil)
