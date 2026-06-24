package world

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
	"github.com/double-nibble/telosmud/internal/directory"
)

// TestCrossShardHandoffInitiation proves the source side of the handoff
// (docs/PROTOCOL.md §3): a player walking through a cross-shard exit is frozen, the
// directory is updated to the destination shard with a bumped epoch, and the player
// is told to re-dial via a Redirect frame. Only shard A runs here — shard B's address
// is merely registered so A can route to it (the destination Prepare RPC is step 4).
func TestCrossShardHandoffInitiation(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	if err := dir.RegisterZone(ctx, "midgaard", "shard-a:9090"); err != nil {
		t.Fatal(err)
	}
	if err := dir.RegisterZone(ctx, "darkwood", "shard-b:9090"); err != nil {
		t.Fatal(err)
	}

	client := startShardServer(t, NewShard("midgaard", "shard-a:9090", dir))

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}

	send(t, stream, attach("Walker"))
	recvAttached(t, stream)

	// temple -> market (local), then market -> darkwood (cross-shard -> handoff).
	send(t, stream, inputSeq(1, "north"))
	send(t, stream, inputSeq(2, "north"))

	redir := recvRedirect(t, stream)
	if redir.GetTargetShardAddr() != "shard-b:9090" {
		t.Fatalf("redirect target = %q, want shard-b:9090", redir.GetTargetShardAddr())
	}
	if redir.GetHandoffToken() == "" {
		t.Fatal("redirect handoff token is empty")
	}

	// The directory now records Walker on shard B with the bumped epoch.
	place, err := dir.PlayerPlacement(ctx, "Walker")
	if err != nil {
		t.Fatalf("placement: %v", err)
	}
	if place.ShardAddr != "shard-b:9090" || place.Epoch != 2 {
		t.Fatalf("placement = %+v, want {ShardAddr:shard-b:9090 Epoch:2}", place)
	}
}

func startShardServer(t *testing.T, shard *Shard) playv1.PlayClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	shard.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	zctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go shard.Run(zctx)

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
	return playv1.NewPlayClient(cc)
}

func recvRedirect(t *testing.T, s playv1.Play_ConnectClient) *playv1.Redirect {
	t.Helper()
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv waiting for Redirect: %v", err)
		}
		if r := f.GetRedirect(); r != nil {
			return r
		}
	}
}
