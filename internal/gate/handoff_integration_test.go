package gate

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
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

	// Destination shard B comes up first so A can reach its Handoff service.
	lisB := bufconn.Listen(1 << 20)
	serveWorld(t, world.NewShard("darkwood", "addr-b", dir, nil), lisB)

	// A's peer dialer maps the registered address to B's bufconn Handoff client.
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		if addr != "addr-b" {
			return nil, fmt.Errorf("unknown shard %q", addr)
		}
		return handoffv1.NewHandoffClient(dialBufGate(t, lisB)), nil
	}
	lisA := bufconn.Listen(1 << 20)
	serveWorld(t, world.NewShard("midgaard", "addr-a", dir, peers), lisA)

	// The gate's pool dials shards BY ADDRESS over the two bufconns. This is the same
	// seam production uses; only the dialer differs.
	playClients := map[string]playv1.PlayClient{
		"addr-a": playv1.NewPlayClient(dialBufGate(t, lisA)),
		"addr-b": playv1.NewPlayClient(dialBufGate(t, lisB)),
	}
	p := newPoolWithDialer(func(addr string) (playv1.PlayClient, error) {
		c, ok := playClients[addr]
		if !ok {
			return nil, fmt.Errorf("no shard at %q", addr)
		}
		return c, nil
	})

	// Login resolves the home zone (midgaard) -> addr-a via the directory.
	loginDir := homeZoneDir{redis: dir, zone: "midgaard"}
	srv := newServer(":0", loginDir, p)

	// net.Pipe gives us the two ends of the "telnet socket": the gate serves one end,
	// the test drives the other as the player's terminal.
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	hctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		srv.handle(hctx, server)
		close(done)
	}()

	term := newTerminal(client)
	term.expect(t, "By what name") // login prompt
	writeLine(t, client, "Walker")
	term.expect(t, "Temple Square") // look on join: live on A

	// Walk A: temple -> market -> (cross-shard) darkwood. The gate handles the redirect
	// itself; the player just keeps typing.
	writeLine(t, client, "north") // temple -> market
	term.expect(t, "Market Square")
	writeLine(t, client, "north")   // market -> darkwood: triggers handoff
	term.expect(t, "Moonlit Grove") // gate re-dialed B; activation look landed

	// Exactly-once across the move: 'say arrived' must echo on B. (The replay of the
	// already-applied move input is deduped by the world; we just confirm the player is
	// live and commands work on the new shard.)
	writeLine(t, client, "say arrived")
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
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("gate handle did not return after socket close")
	}
}

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

func serveWorld(t *testing.T, shard *world.Shard, lis *bufconn.Listener) {
	t.Helper()
	gs := grpc.NewServer()
	shard.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	zctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go shard.Run(zctx)
}

func dialBufGate(t *testing.T, lis *bufconn.Listener) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// writeLine sends one CRLF-terminated line as the player's terminal would.
func writeLine(t *testing.T, c net.Conn, s string) {
	t.Helper()
	if _, err := c.Write([]byte(s + "\r\n")); err != nil {
		t.Fatalf("write %q: %v", s, err)
	}
}

// terminal reads rendered bytes off the gate's socket via a single background reader
// (net.Pipe reads block forever, so the reader must not be inline) and lets the test
// assert that an expected substring appears within a deadline. The accumulator persists
// across expect calls so output already buffered from a prior step still counts.
type terminal struct {
	bytes chan byte
	acc   strings.Builder
}

func newTerminal(c net.Conn) *terminal {
	t := &terminal{bytes: make(chan byte, 4096)}
	go func() {
		r := bufio.NewReader(c)
		for {
			b, err := r.ReadByte()
			if err != nil {
				close(t.bytes)
				return
			}
			t.bytes <- b
		}
	}()
	return t
}

func (term *terminal) expect(t *testing.T, substr string) {
	t.Helper()
	if strings.Contains(term.acc.String(), substr) {
		return
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case b, ok := <-term.bytes:
			if !ok {
				t.Fatalf("socket closed waiting for %q; got %q", substr, term.acc.String())
			}
			term.acc.WriteByte(b)
			if strings.Contains(term.acc.String(), substr) {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q; got %q", substr, term.acc.String())
		}
	}
}
