package world

import (
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
)

// TestCrossShardHandoff drives a full world-to-world handoff across two in-process
// shards (A=midgaard, B=darkwood) sharing a directory. A player walks A's cross-shard
// exit; A.Prepare rehydrates them pending on B; A claims the directory and redirects;
// the simulated gate re-dials B with the token; B activates the player into
// darkwood:grove; and a replayed input is deduped while a new one applies — exactly-
// once across the move.
func TestCrossShardHandoff(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	// Publish each shard's id -> endpoint, then lease each zone to a shard id. Routing
	// resolves zone -> shard id -> endpoint, so the peer dialer is still keyed by address.
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterShard(ctx, "shard-b", "addr-b", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a"))
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-b"))

	// Destination shard B runs first so A can reach its Handoff service.
	lisB := bufconn.Listen(1 << 20)
	bPlay := serveShard(t, NewShard("darkwood", "addr-b", dir, nil), lisB)

	// A's peer dialer maps the registered address to B's bufconn Handoff client.
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		if addr != "addr-b" {
			return nil, fmt.Errorf("unknown shard %q", addr)
		}
		return handoffv1.NewHandoffClient(dialBuf(t, lisB)), nil
	}
	lisA := bufconn.Listen(1 << 20)
	aPlay := serveShard(t, NewShard("midgaard", "addr-a", dir, peers), lisA)

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Player connects to A and walks temple -> market -> (cross-shard) darkwood.
	sA, err := aPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sA, attach("Walker"))
	recvAttached(t, sA)
	send(t, sA, inputSeq(1, "north")) // temple -> market
	send(t, sA, inputSeq(2, "north")) // market -> darkwood: triggers the handoff

	redir := recvRedirect(t, sA)
	if redir.GetTargetShardAddr() != "addr-b" {
		t.Fatalf("redirect target = %q, want addr-b", redir.GetTargetShardAddr())
	}

	// The simulated gate re-dials B with the handoff token: B activates the pending
	// player into darkwood's grove.
	sB, err := bPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sB, attachWithToken("Walker", redir.GetHandoffToken()))
	recvAttached(t, sB)
	recvUntilOutput(t, sB, "Moonlit Grove") // the activation look: the player is live on B

	// Exactly-once across the move: replay seq 2 (<= the resumed high-water) is dropped;
	// the new seq 3 applies.
	send(t, sB, inputSeq(2, "say DUP"))
	send(t, sB, inputSeq(3, "say arrived"))
	recvSay(t, sB, "arrived")

	// The directory records Walker on shard B at the bumped epoch.
	place, err := dir.PlayerPlacement(ctx, "Walker")
	if err != nil {
		t.Fatal(err)
	}
	if place.ShardID != "shard-b" || place.Epoch != 2 {
		t.Fatalf("placement = %+v, want {ShardID:shard-b Epoch:2}", place)
	}
}

// TestCrossShardHandoffRoundTrip walks a player A->B->A. The return leg exercises the
// fix for the architect's flagged bug: when B hands back, A still holds the player's
// stale FROZEN copy from the outbound hop, so A.Prepare must discard it (lower epoch)
// and rehydrate rather than reject with "already present".
func TestCrossShardHandoffRoundTrip(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	// Publish each shard's id -> endpoint, then lease each zone to a shard id. Routing
	// resolves zone -> shard id -> endpoint, so the peer dialer is still keyed by address.
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterShard(ctx, "shard-b", "addr-b", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a"))
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-b"))

	lisA := bufconn.Listen(1 << 20)
	lisB := bufconn.Listen(1 << 20)
	lisByAddr := map[string]*bufconn.Listener{"addr-a": lisA, "addr-b": lisB}
	// One dialer both shards use to reach each other's Handoff service.
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		lis := lisByAddr[addr]
		if lis == nil {
			return nil, fmt.Errorf("unknown shard %q", addr)
		}
		return handoffv1.NewHandoffClient(dialBuf(t, lis)), nil
	}
	aPlay := serveShard(t, NewShard("midgaard", "addr-a", dir, peers), lisA)
	bPlay := serveShard(t, NewShard("darkwood", "addr-b", dir, peers), lisB)

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- outbound: A -> B ---
	sA, err := aPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sA, attach("Strider"))
	recvAttached(t, sA)
	send(t, sA, inputSeq(1, "north")) // temple -> market
	send(t, sA, inputSeq(2, "north")) // market -> darkwood
	redirB := recvRedirect(t, sA)
	if redirB.GetTargetShardAddr() != "addr-b" {
		t.Fatalf("outbound redirect = %q, want addr-b", redirB.GetTargetShardAddr())
	}
	sB, err := bPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sB, attachWithToken("Strider", redirB.GetHandoffToken()))
	recvAttached(t, sB)
	recvUntilOutput(t, sB, "Moonlit Grove")

	// --- return: B -> A (A still holds the frozen copy) ---
	send(t, sB, inputSeq(3, "south")) // grove -> midgaard:market
	redirA := recvRedirect(t, sB)
	if redirA.GetTargetShardAddr() != "addr-a" {
		t.Fatalf("return redirect = %q, want addr-a", redirA.GetTargetShardAddr())
	}
	sA2, err := aPlay.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, sA2, attachWithToken("Strider", redirA.GetHandoffToken()))
	recvAttached(t, sA2)
	recvUntilOutput(t, sA2, "Market Square") // back home in midgaard

	if place, _ := dir.PlayerPlacement(ctx, "Strider"); place.ShardID != "shard-a" || place.Epoch != 3 {
		t.Fatalf("placement = %+v, want {ShardID:shard-a Epoch:3}", place)
	}
}

// TestHandoffBindRejectsUnknownToken guards the fix for a state-loss path: an Attach
// carrying a handoff token but with no matching pending player must be rejected, not
// silently spawn a fresh character.
func TestHandoffBindRejectsUnknownToken(t *testing.T) {
	client := startWorld(t) // NewDemoShard: a single zone with no pending players
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, stream, attachWithToken("Ghost", "bogus-token"))
	if d := recvDisconnect(t, stream); d.GetReason() == "" {
		t.Fatal("expected a non-empty disconnect reason for an unknown handoff token")
	}
}

func recvDisconnect(t *testing.T, s playv1.Play_ConnectClient) *playv1.Disconnect {
	t.Helper()
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv waiting for Disconnect: %v", err)
		}
		if d := f.GetDisconnect(); d != nil {
			return d
		}
	}
}

func mustReg(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func serveShard(t *testing.T, shard *Shard, lis *bufconn.Listener) playv1.PlayClient {
	t.Helper()
	gs := grpc.NewServer()
	shard.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	zctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go shard.Run(zctx)

	return playv1.NewPlayClient(dialBuf(t, lis))
}

func dialBuf(t *testing.T, lis *bufconn.Listener) *grpc.ClientConn {
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

func attachWithToken(name, token string) *playv1.ClientFrame {
	return &playv1.ClientFrame{Payload: &playv1.ClientFrame_Attach{Attach: &playv1.Attach{
		CharacterId:  name,
		HandoffToken: token,
	}}}
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

func recvUntilOutput(t *testing.T, s playv1.Play_ConnectClient, substr string) {
	t.Helper()
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv waiting for output %q: %v", substr, err)
		}
		if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), substr) {
			return
		}
	}
}

// TestCrossShardHandoffFailureRestoresPlayer covers the bug where a cross-zone move into a
// zone NO shard hosts left the player's entity detached from its room (location==nil), so the
// next room action null-dereferenced and crashed the zone. The handoff must fail gracefully
// ("The way is barred"), thaw the player, and RESTORE them to the room they tried to leave —
// after which look and a normal move must work.
func TestCrossShardHandoffFailureRestoresPlayer(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	// Only midgaard is hosted; darkwood is registered NOWHERE, so the market->grove
	// cross-shard exit resolves to no shard and the handoff cannot be initiated.
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a"))

	lis := bufconn.Listen(1 << 20)
	// The peer dialer must never be reached — resolution fails before any dial.
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		return nil, fmt.Errorf("unexpected dial to %q", addr)
	}
	play := serveShard(t, NewShard("midgaard", "addr-a", dir, peers), lis)

	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := play.Connect(sctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Lost"))
	recvAttached(t, s)
	send(t, s, inputSeq(1, "north")) // temple -> market
	expectMarkup(t, s, "Market Square")
	send(t, s, inputSeq(2, "north")) // market -> darkwood:grove, but darkwood is unhosted
	recvUntilOutput(t, s, "The way is barred")

	// Before the fix the entity's location was nil here; look would null-deref and crash the
	// zone. After the fix the player is back in the market and these work normally.
	send(t, s, inputSeq(3, "look"))
	recvUntilOutput(t, s, "Market Square")
	send(t, s, inputSeq(4, "south")) // market -> temple, a normal move still works
	recvUntilOutput(t, s, "The Temple Square")
}
