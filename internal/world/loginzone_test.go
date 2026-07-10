package world

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// loginzone_test.go pins #320: a rehydrating login attaches to the zone named by the character's DURABLE
// zone_ref, not blindly to the shard's home zone.
//
// The bug: `zone := s.shard.zoneByID(s.shard.home)` ran for every non-handoff login, and loginRoom's
// resolveRoom silently falls back to that zone's start room when the saved room_ref names a room some
// OTHER zone hosts. A shard hosts many zones — the demo pack ships midgaard, darkwood and crypt, and
// midgaard's north exit crosses into darkwood — so an ordinary intra-shard walk followed by a logout lost
// the player's durable location. No rebalance and no cross-shard hop required.

// startMultiZoneWorld runs a shard hosting BOTH demo zones (home = midgaard) behind a real gRPC Play
// stream, backed by an in-memory character store so a logout actually persists a zone_ref to rehydrate
// from. It returns the client and the store, so a test can inspect what was saved.
func startMultiZoneWorld(t *testing.T) (playv1.PlayClient, *MemStore) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	store := NewMemStore()
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil).
		WithPersistence(store, nil)
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
	return playv1.NewPlayClient(cc), store
}

// recvOutputContaining reads frames until one whose markup contains want, or fails on timeout.
func recvOutputContaining(t *testing.T, s playv1.Play_ConnectClient, want string) {
	t.Helper()
	for {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv waiting for output containing %q: %v", want, err)
		}
		if o := f.GetOutput(); o != nil && strings.Contains(o.GetMarkup(), want) {
			return
		}
	}
}

// TestRelogRehydratesIntoTheDurableZone is the #320 regression, driven through the real Play stream: walk
// midgaard -> darkwood (an intra-shard transfer), quit, then log back in. The player must come back in
// DARKWOOD. Pre-fix they were attached to the home zone (midgaard) and resolveRoom quietly dropped them in
// midgaard's temple, discarding the saved location.
func TestRelogRehydratesIntoTheDurableZone(t *testing.T) {
	client, store := startMultiZoneWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// --- session 1: walk north into darkwood, then quit cleanly ---
	ctx1, drop1 := context.WithCancel(ctx)
	s1, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s1, attach("Wanderer"))
	recvAttached(t, s1)

	// temple -> market (intra-zone), then market -> darkwood:room:grove (the cross-zone exit, an
	// intra-shard transfer because this shard co-hosts both zones).
	send(t, s1, inputSeq(1, "north"))
	recvOutputContaining(t, s1, "Market Square")
	send(t, s1, inputSeq(2, "north"))
	recvOutputContaining(t, s1, "Moonlit Grove")

	send(t, s1, inputSeq(3, "quit"))
	recvOutputContaining(t, s1, "Farewell")
	drop1()

	// The durable record must name darkwood — this is the premise; if the save is wrong, the login fix
	// cannot help and the test would be asserting the wrong thing.
	waitFor(t, func() bool {
		snap, ok, _ := store.LoadCharacter(ctx, "Wanderer")
		return ok && snap.ZoneRef == "darkwood"
	}, "durable zone_ref never became darkwood")

	snap, _, _ := store.LoadCharacter(ctx, "Wanderer")
	if snap.RoomRef != "darkwood:room:grove" {
		t.Fatalf("durable room_ref = %q, want darkwood:room:grove", snap.RoomRef)
	}

	// --- session 2: log back in. The login must attach to DARKWOOD, not the home zone. ---
	s2, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s2, attach("Wanderer"))
	recvAttached(t, s2)

	// `look` renders the room the player actually landed in. Pre-fix this said "The Temple of Midgaard".
	send(t, s2, inputSeq(1, "look"))
	f := recvNextOutput(t, s2)
	if strings.Contains(f, "Temple Square") {
		t.Fatalf("relog dumped the player in the HOME zone's start room, losing their durable location (#320): %q", f)
	}
	if !strings.Contains(f, "Moonlit Grove") {
		t.Fatalf("relog should have rehydrated into darkwood:room:grove, got %q", f)
	}
}

// TestLoginFallsBackToHomeForABrandNewCharacter pins the other half: a character with no durable record
// (loadedOK == false) still starts in the shard's home zone. The zone_ref path must not break first login.
func TestLoginFallsBackToHomeForABrandNewCharacter(t *testing.T) {
	client, _ := startMultiZoneWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Newbie"))
	recvAttached(t, s)

	send(t, s, inputSeq(1, "look"))
	if got := recvNextOutput(t, s); !strings.Contains(got, "Temple Square") {
		t.Fatalf("a brand-new character must spawn in the home zone's start room, got %q", got)
	}
}

// TestLinkDeadReconnectResumesInTheDurableZone: the same routing must hold for a reconnect inside the
// link-death grace. detach() flushes the player's state before the grace, so zone_ref already names
// darkwood; the reconnect must land there and re-bind the detached session rather than being dropped into
// midgaard as a fresh arrival.
func TestLinkDeadReconnectResumesInTheDurableZone(t *testing.T) {
	client, store := startMultiZoneWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ctx1, drop1 := context.WithCancel(ctx)
	s1, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s1, attach("Ghost"))
	recvAttached(t, s1)
	send(t, s1, inputSeq(1, "north"))
	recvOutputContaining(t, s1, "Market Square")
	send(t, s1, inputSeq(2, "north"))
	recvOutputContaining(t, s1, "Moonlit Grove")
	drop1() // link death, no quit

	// SYNCHRONIZE, do not race (distsys review). An intra-shard transferIn does NOT flush, so the durable
	// zone_ref still says midgaard until detach's enqueueSave(saveFlush) is DRAINED by the saver's
	// background goroutine. Reconnecting before that lands reads a stale zone_ref, routes to midgaard, and
	// spawns a SECOND live copy while the detached one waits in darkwood for the reap. That window is a
	// real (pre-existing, narrower-than-before) production race — see #321 — and it must not be what this
	// test is measuring.
	waitFor(t, func() bool {
		snap, ok, _ := store.LoadCharacter(ctx, "Ghost")
		return ok && snap.ZoneRef == "darkwood"
	}, "the link-death flush never landed a darkwood zone_ref")

	s2, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s2, attach("Ghost"))
	recvAttached(t, s2)
	send(t, s2, inputSeq(3, "look"))
	if got := recvNextOutput(t, s2); !strings.Contains(got, "Moonlit Grove") {
		t.Fatalf("a link-dead reconnect must resume in the durable zone (darkwood), got %q", got)
	}
}

// recvNextOutput reads until the next non-empty Output frame and returns its markup.
func recvNextOutput(t *testing.T, s playv1.Play_ConnectClient) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, err := s.Recv()
		if err != nil {
			t.Fatalf("recv waiting for output: %v", err)
		}
		if o := f.GetOutput(); o != nil && strings.TrimSpace(o.GetMarkup()) != "" {
			return o.GetMarkup()
		}
	}
	t.Fatal("timed out waiting for an output frame")
	return ""
}

// waitFor polls cond until it holds or the deadline lapses (the save is asynchronous — the saver writes
// off the zone goroutine).
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
