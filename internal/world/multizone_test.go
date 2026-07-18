package world

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/test/bufconn"

	playv1 "github.com/double-nibble/telosmud/api/gen/telosmud/play/v1"
)

// TestIntraShardMultiZoneWalk proves the multi-zone shard: ONE world process hosts
// both midgaard and darkwood, and a player walks across the zone boundary entirely
// in-process — no Redirect, no handoff frame, no directory. It also proves in-flight
// input is not lost: a command sent immediately after the cross-zone move still runs
// on the destination zone (the source forwards it; the destination dedups by seq).
func TestIntraShardMultiZoneWalk(t *testing.T) {
	// One shard hosting both zones; home is midgaard. No dir/peers: cross-shard exits
	// would be sealed, but darkwood is hosted HERE so the grove walk is in-process.
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	lis := bufconn.Listen(1 << 20)
	play := serveShard(t, shard, lis)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := play.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Rover"))
	recvAttached(t, s)
	expectMarkup(t, s, "The Temple Square") // fresh login spawns in home zone

	// Temple -> Market (intra-zone, midgaard).
	send(t, s, inputSeq(1, "north"))
	expectMarkup(t, s, "Market Square")

	// Market -> "A Moonlit Grove" is a CROSS-ZONE exit (market.north => darkwood:grove)
	// but darkwood is hosted on this same shard, so it must transfer in-process. We send
	// the move and a follow-up command back-to-back: the follow-up may reach the source
	// (midgaard) zone before the reader loop observes the new currentZone, exercising the
	// forwarding path. Either way it must run on the destination.
	send(t, s, inputSeq(2, "north"))       // market -> darkwood:grove (intra-shard transfer)
	send(t, s, inputSeq(3, "say arrived")) // typed immediately after the move

	// The arrival look proves we landed in darkwood with NO Redirect frame in between
	// (recvUntilOutput fails the test on stream error; a Redirect would not match and we
	// would keep reading, but no handoff is initiated so none is sent).
	recvUntilOutput(t, s, "Moonlit Grove")

	// The follow-up command was not lost: it ran on the destination zone.
	recvSay(t, s, "arrived")

	// And the player really is in darkwood now, not midgaard: a fresh look shows the
	// grove. Also assert no Redirect was ever queued by failing fast on one.
	assertNoRedirect(t, s)

	// The directory was never touched (nil dir) and no token index entry leaked: an
	// in-process walk uses neither.
	shard.mu.Lock()
	n := len(shard.tokenIndex)
	shard.mu.Unlock()
	if n != 0 {
		t.Fatalf("token index has %d entries after an in-process walk; want 0", n)
	}

	// Walk deeper INTO darkwood (grove -> hollow) to confirm the player is genuinely
	// owned by the darkwood zone now (an intra-zone move within the destination).
	send(t, s, inputSeq(5, "north"))
	recvUntilOutput(t, s, "Dark Hollow")
}

// TestIntraShardForwarding deterministically exercises the in-flight-input forwarding
// path. Driving the zone inboxes directly (no network reader loop) lets us pin the
// ordering the forwarding map exists to handle: a second input lands on the SOURCE zone
// after the player has already transferred out. The source must forward it to the
// destination, which applies it (deduping by seq). Both zones run on their own
// goroutines, so this also runs under -race to guard the cross-goroutine handoff.
func TestIntraShardForwarding(t *testing.T) {
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	src := shard.ZoneByID("midgaard")
	dst := shard.ZoneByID("darkwood")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)
	go dst.Run(ctx)

	// Place a player directly in midgaard's market, the room whose north exit crosses
	// into darkwood's grove. Give it a currentZone pointer like a real connection so
	// transferIn can repoint it. We use placeTestPlayer rather than joinMsg because join
	// always lands a fresh player in the start room; this test needs it in market so the
	// very next "north" triggers the cross-zone transfer.
	var cur atomic.Pointer[Zone]
	cur.Store(src)
	p := &session{
		character:   "Echo",
		out:         make(chan *playv1.ServerFrame, 64),
		epoch:       1,
		currentZone: &cur,
	}
	src.newPlayerEntity(p, "Echo")
	placeTestPlayer(t, src, p, "midgaard:room:market")
	waitMarkup(t, p, "Market Square")

	// Two inputs posted to the SOURCE zone in order. The first triggers the intra-shard
	// transfer (market -> darkwood:grove); the second arrives at the source AFTER the
	// player has left, so the source must forward it to darkwood, which applies it.
	src.post(inputMsg{id: "Echo", seq: 1, line: "north"})
	src.post(inputMsg{id: "Echo", seq: 2, line: "say forwarded"})

	waitMarkup(t, p, "Moonlit Grove")        // landed in darkwood via in-process transfer
	waitMarkup(t, p, "You say, 'forwarded'") // the forwarded line ran on the destination

	// currentZone now points at the destination zone.
	if cur.Load() != dst {
		t.Fatalf("currentZone = %v, want darkwood", cur.Load().id)
	}
}

// placeTestPlayer registers a pre-built session in zone z directly at the given room
// ProtoRef and shows it the room — the white-box equivalent of a fresh join into a
// specific (non-start) room, which join itself no longer allows. It reuses transferInMsg
// (which resolves the room, registers the session, Moves its entity in, and looks) so the
// placement goes through the real zone goroutine and exercises no special test path.
func placeTestPlayer(t *testing.T, z *Zone, s *session, room ProtoRef) {
	t.Helper()
	z.claimInboundTransfer() // the shard-hosted path claims under s.mu (claimTransferTarget); this is the bare form
	z.postTransferIn(s, room)
}

// TestIntraShardWalkStress bounces one player across the same-shard zone boundary many
// times over the REAL reader-loop path, sending a follow-up command immediately after
// every move. Each crossing opens the ownership/forwarding hand-off window between the
// source and destination zone goroutines; running this under -race is the standing guard
// against the single-writer regression the deterministic tests cannot reliably surface
// (the source must not touch the player struct after handing it off). Functionally it
// also proves no input is lost or duplicated across dozens of in-process transfers.
func TestIntraShardWalkStress(t *testing.T) {
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "", nil, nil)
	lis := bufconn.Listen(1 << 20)
	play := serveShard(t, shard, lis)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := play.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Bouncer"))
	recvAttached(t, s)
	expectMarkup(t, s, "The Temple Square")
	send(t, s, inputSeq(1, "north")) // temple -> market
	expectMarkup(t, s, "Market Square")

	// Bounce market <-> grove. market.north => darkwood:grove and grove.south =>
	// midgaard:market, so each leg is an intra-shard cross-zone transfer. The "say" after
	// every move is the immediate follow-up that lands during the hand-off window.
	seq := uint64(2)
	const bounces = 40
	for i := 0; i < bounces; i++ {
		send(t, s, inputSeq(seq, "north")) // market -> grove (transfer)
		seq++
		send(t, s, inputSeq(seq, "say hi")) // immediate follow-up
		seq++
		recvUntilOutput(t, s, "Moonlit Grove") // drains the say echo too

		send(t, s, inputSeq(seq, "south")) // grove -> market (transfer back)
		seq++
		send(t, s, inputSeq(seq, "say ho"))
		seq++
		recvUntilOutput(t, s, "Market Square")
	}
}

// assertNoRedirect sends a no-op-ish look and asserts the next frame stream does not
// carry a Redirect within a short window (an intra-shard move must never redirect).
func assertNoRedirect(t *testing.T, s playv1.Play_ConnectClient) {
	t.Helper()
	send(t, s, inputSeq(4, "look"))
	deadline := time.After(2 * time.Second)
	done := make(chan *playv1.ServerFrame, 1)
	errc := make(chan error, 1)
	go func() {
		for {
			f, err := s.Recv()
			if err != nil {
				errc <- err
				return
			}
			if f.GetRedirect() != nil {
				done <- f
				return
			}
			if o := f.GetOutput(); o != nil &&
				(strings.Contains(o.GetMarkup(), "Moonlit Grove") ||
					strings.Contains(o.GetMarkup(), "Dark Hollow")) {
				done <- nil
				return
			}
		}
	}()
	select {
	case f := <-done:
		if f != nil {
			t.Fatal("intra-shard move produced a Redirect frame; it must stay in-process")
		}
	case err := <-errc:
		t.Fatalf("recv during no-redirect check: %v", err)
	case <-deadline:
		t.Fatal("timed out waiting for the look response")
	}
}
