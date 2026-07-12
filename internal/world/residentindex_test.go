package world

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// residentindex_test.go pins #321: a reconnect routes by the LIVE in-memory residency index, not the durable
// zone_ref that lags an intra-shard walk. Without it, a link-dead resume routed by the stale durable zone
// lands in a zone that no longer holds the session, takes the fresh-login branch, and DOUBLE-OWNS the
// character — a fresh copy while the detached copy still sits in the zone they walked to.
//
// These run WITHOUT persistence, so there is NO durable zone_ref at all — the most direct form of "the
// durable record does not name the zone the session is actually in". A reconnect that ignores the in-memory
// index falls to the home zone and spawns a second copy; one that consults it re-binds the held session.

// indexWorld serves a shard hosting both demo zones (midgaard + darkwood) over a real gRPC Play stream, with
// NO persistence and NO directory. Returns the client and the shard (to inspect the residency index + pop).
func indexWorld(t *testing.T) (playv1.PlayClient, *Shard) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "addr-a", nil, nil)
	shard.Register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)

	zctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go shard.Run(zctx)

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return playv1.NewPlayClient(cc), shard
}

// TestReconnectRoutesToTheHeldZoneNotTheStaleDurableZone is the #321 core: walk midgaard->darkwood, link
// death, reconnect. The reconnect must re-attach in darkwood (where the detached session is held), NOT
// fresh-login in the home zone — and there must be exactly ONE copy of the character on the shard.
func TestReconnectRoutesToTheHeldZoneNotTheStaleDurableZone(t *testing.T) {
	client, shard := indexWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ctx1, drop1 := context.WithCancel(ctx)
	s, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Walker"))
	recvAttached(t, s)

	// Walk into darkwood (intra-shard transfer). No durable flush happens on a walk.
	send(t, s, inputSeq(1, "north"))
	recvOutputContaining(t, s, "Market Square")
	send(t, s, inputSeq(2, "north"))
	recvOutputContaining(t, s, "Moonlit Grove")

	// The residency index must now name darkwood — the whole point.
	if z := shard.zoneForResidentCharacter("Walker"); z == nil || z.id != "darkwood" {
		t.Fatalf("residency index = %v, want darkwood after the walk (#321)", zoneID(z))
	}

	drop1() // socket dies inside the grace; the detached session stays held in darkwood

	// Reconnect. With no durable record, a router that ignores the in-memory index falls to the home zone
	// (midgaard) and spawns a fresh copy. The index must route it to darkwood and re-bind.
	s2, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s2, attach("Walker"))
	recvAttached(t, s2)
	send(t, s2, inputSeq(1, "look"))
	recvOutputContaining(t, s2, "Moonlit Grove") // re-attached in darkwood, not fresh in midgaard's temple

	// And exactly ONE copy exists: darkwood holds the re-bound session, midgaard holds nothing. A double-own
	// would leave a fresh copy in midgaard AND the (detached-then-rebound) copy in darkwood.
	waitPop(t, shard.ZoneByID("midgaard"), 0, "midgaard must hold no copy of Walker — a fresh login there is the double-own (#321)")
	waitPop(t, shard.ZoneByID("darkwood"), 1, "darkwood must hold exactly one Walker (the re-bound session)")
}

// TestResidencyIndexClearsOnQuit: a clean quit removes the character from the index (delPlayer), so a later
// reconnect no longer routes by a stale residency and takes the correct fresh-login path.
func TestResidencyIndexClearsOnQuit(t *testing.T) {
	client, shard := indexWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ctx1, drop1 := context.WithCancel(ctx)
	s, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Leaver"))
	recvAttached(t, s)
	if z := shard.zoneForResidentCharacter("Leaver"); z == nil || z.id != "midgaard" {
		t.Fatalf("residency index = %v, want midgaard after login", zoneID(z))
	}

	send(t, s, inputSeq(1, "quit"))
	recvOutputContaining(t, s, "Farewell")
	drop1()

	// The quit's leave() removes the index entry.
	deadline := time.Now().Add(5 * time.Second)
	for shard.zoneForResidentCharacter("Leaver") != nil {
		if time.Now().After(deadline) {
			t.Fatal("a clean quit must clear the residency index (#321)")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestResidencyIndexFollowsAWalkOnlyToTheDestination pins the transfer-ordering guard: after a walk, the
// index names the DESTINATION zone, never left dangling on the source by a blind delete racing the
// destination's set.
func TestResidencyIndexFollowsAWalkOnlyToTheDestination(t *testing.T) {
	client, shard := indexWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Strider"))
	recvAttached(t, s)
	send(t, s, inputSeq(1, "north"))
	recvOutputContaining(t, s, "Market Square")
	send(t, s, inputSeq(2, "north"))
	recvOutputContaining(t, s, "Moonlit Grove")

	// The destination (darkwood) owns the residency; the source (midgaard) never keeps a stale entry.
	deadline := time.Now().Add(5 * time.Second)
	for {
		z := shard.zoneForResidentCharacter("Strider")
		if z != nil && z.id == "darkwood" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("residency index = %v, want darkwood (the walk destination)", zoneID(z))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func zoneID(z *Zone) string {
	if z == nil {
		return "<nil>"
	}
	return z.id
}

// waitPop polls a zone's population mirror until it equals want or the deadline elapses (the reconnect
// re-attach and any reap settle asynchronously on the zone goroutine).
func waitPop(t *testing.T, z *Zone, want int64, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := z.pop.Load()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: pop = %d, want %d", msg, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
