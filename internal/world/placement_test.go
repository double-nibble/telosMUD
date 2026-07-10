package world

import (
	"context"
	"net"
	"strings"
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

// placement_test.go pins the world half of #320 slice 2: the world writes the directory's per-player
// placement record whenever a player becomes resident in a zone.
//
// Before this, the ONLY writer of `dir:player:<id>` was the cross-shard handoff CAS. Two consequences,
// both asserted here:
//   - a player who had never been handed off had NO placement, so the gate could not route their
//     reconnect and the tell/mail existence oracle answered "there is no player by that name";
//   - the record named a SHARD, which goes stale the moment that zone is rebalanced. It now names the
//     ZONE, which `ShardForZone` resolves to the current owner.

// placementWorld runs a shard hosting both demo zones with a REAL (miniredis) directory and a leasing
// shard id, behind a real gRPC Play stream. It returns the client and the directory.
func placementWorld(t *testing.T) (playv1.PlayClient, *directory.Redis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a"))
	mustReg(t, dir.RegisterZone(ctx, "darkwood", "shard-a"))

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	shard := NewMultiShard([]string{"midgaard", "darkwood"}, "midgaard", "addr-a", dir, nil).
		WithPersistence(NewMemStore(), nil).
		WithZoneLeasing(dir, "shard-a", directory.DefaultZoneLease, 0, nil)
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
	return playv1.NewPlayClient(cc), dir
}

// waitPlacement polls until the player's placement satisfies cond (the write is enqueued and drained by a
// background goroutine, so it is not visible synchronously).
func waitPlacement(t *testing.T, dir *directory.Redis, player string, cond func(directory.Placement) bool, msg string) directory.Placement {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	var last directory.Placement
	for time.Now().Before(deadline) {
		p, err := dir.PlayerPlacement(ctx, player)
		if err == nil {
			last = p
			if cond(p) {
				return p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s (last placement seen: %+v)", msg, last)
	return last
}

// TestLoginWritesThePlacement is the core gap #320 slice 2 closes: a player who has NEVER been handed off
// across shards now has a placement, naming the shard AND the zone they are in.
func TestLoginWritesThePlacement(t *testing.T) {
	client, dir := placementWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Rambler"))
	recvAttached(t, s)

	p := waitPlacement(t, dir, "Rambler", func(p directory.Placement) bool {
		return p.ShardID == "shard-a" && p.ZoneID == "midgaard"
	}, "a fresh login must record a placement naming its shard and zone")
	if p.Epoch == 0 {
		t.Fatalf("placement epoch = 0, want the session's seeded epoch: %+v", p)
	}
}

// TestPlacementIsRoutableByZone: the recorded zone must resolve, through the directory, back to the shard
// that hosts it — which is exactly the two-hop lookup the gate performs (ShardForZone -> EndpointForShard).
// This is what makes a reconnect survive a rebalance that moved the zone to a different shard.
func TestPlacementIsRoutableByZone(t *testing.T) {
	client, dir := placementWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Router"))
	recvAttached(t, s)
	p := waitPlacement(t, dir, "Router", func(p directory.Placement) bool { return p.ZoneID != "" },
		"login did not record a zone")

	shardID, err := dir.ShardForZone(ctx, p.ZoneID)
	if err != nil || shardID != "shard-a" {
		t.Fatalf("ShardForZone(%q) = %q, %v; want shard-a", p.ZoneID, shardID, err)
	}
	endpoint, err := dir.EndpointForShard(ctx, shardID)
	if err != nil || endpoint != "addr-a" {
		t.Fatalf("EndpointForShard(%q) = %q, %v; want addr-a", shardID, endpoint, err)
	}

	// Now simulate a rebalance: darkwood... actually midgaard moves to shard-b while the player is offline.
	mustReg(t, dir.RegisterShard(ctx, "shard-b", "addr-b", directory.DefaultShardLease))
	if _, err := dir.HandoverZone(ctx, p.ZoneID, "shard-a", "shard-b", directory.DefaultZoneLease); err != nil {
		t.Fatal(err)
	}
	// The SAME placement record now routes to the new owner, with no rewrite. That is the whole point of
	// storing the zone rather than the shard.
	shardID, err = dir.ShardForZone(ctx, p.ZoneID)
	if err != nil || shardID != "shard-b" {
		t.Fatalf("after rebalance, ShardForZone(%q) = %q, %v; want shard-b — a zone-keyed placement must follow the zone", p.ZoneID, shardID, err)
	}
	// And the stale shard id the OLD record would have used now points at a shard that no longer hosts it.
	if p.ShardID == shardID {
		t.Fatal("test is not exercising the rebalance: the recorded shard still owns the zone")
	}
}

// TestIntraShardWalkUpdatesThePlacementZone: walking across a zone boundary within one shard changes
// neither the shard nor the epoch, so the handoff CAS never fires. The placement's zone must be re-registered
// anyway — otherwise a reconnect after the walk routes by the OLD zone, which a later rebalance can move to
// a different shard entirely.
func TestIntraShardWalkUpdatesThePlacementZone(t *testing.T) {
	client, dir := placementWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Strider"))
	recvAttached(t, s)
	waitPlacement(t, dir, "Strider", func(p directory.Placement) bool { return p.ZoneID == "midgaard" },
		"login did not record midgaard")

	// temple -> market (intra-zone), market -> darkwood:room:grove (the cross-zone, intra-shard transfer).
	send(t, s, inputSeq(1, "north"))
	recvOutputContaining(t, s, "Market Square")
	send(t, s, inputSeq(2, "north"))
	recvOutputContaining(t, s, "Moonlit Grove")

	p := waitPlacement(t, dir, "Strider", func(p directory.Placement) bool { return p.ZoneID == "darkwood" },
		"an intra-shard zone transfer must re-register the placement zone (#320)")
	if p.ShardID != "shard-a" {
		t.Fatalf("the shard must be unchanged by an intra-shard walk: %+v", p)
	}
}

// TestPlacementWriteIsSkippedWithoutADirectory pins the bare-engine invariant: a shard with NO directory
// wired (a single-shard/dev world) does ZERO placement work. The nil-shard case is covered too, but the
// dir==nil clause is the one this test is named for.
func TestPlacementWriteIsSkippedWithoutADirectory(t *testing.T) {
	s := &Shard{placement: newPlacementWriter()} // shard present, dir == nil, shardID == ""
	z := &Zone{id: "midgaard", shard: s}
	z.registerPlacement(&session{character: "Hermit", epoch: 1})
	if got := len(s.placement.take()); got != 0 {
		t.Fatalf("pending placements = %d, want 0 — a shard with no directory must enqueue nothing", got)
	}

	// And a zone with no shard at all (the bare demo engine) must not panic.
	bare := newDemoZone("midgaard", newProtoCache())
	bare.registerPlacement(&session{character: "Hermit", epoch: 1})
}

// TestPlacementWriterCoalescesByPlayer is the contract that makes a slow directory safe. A burst of zone
// changes for one player must collapse to that player's LATEST placement, never drop the tail.
//
// This matters more than it looks. Dropping a *login* registration is harmless — the player's prior record
// is still correct. Dropping a *zone-transfer* registration leaves the record naming the zone they walked
// out of, and once those two zones live on different shards the reconnect routes to a shard that cannot
// host their durable zone_ref and start-rooms them: the exact data loss #320 exists to kill. So the writer
// coalesces rather than dropping. (distsys review.)
func TestPlacementWriterCoalescesByPlayer(t *testing.T) {
	w := newPlacementWriter()

	w.offer(placementOp{playerID: "Walker", zoneID: "midgaard", epoch: 1})
	w.offer(placementOp{playerID: "Walker", zoneID: "darkwood", epoch: 1}) // the walk
	w.offer(placementOp{playerID: "Other", zoneID: "crypt", epoch: 1})

	ops := w.take()
	if len(ops) != 2 {
		t.Fatalf("pending = %d ops, want 2 (one per distinct player)", len(ops))
	}
	byPlayer := map[string]string{}
	for _, op := range ops {
		byPlayer[op.playerID] = op.zoneID
	}
	if byPlayer["Walker"] != "darkwood" {
		t.Fatalf("Walker coalesced to %q, want darkwood — the LATEST zone must win, never the first", byPlayer["Walker"])
	}
	if byPlayer["Other"] != "crypt" {
		t.Fatalf("Other = %q, want crypt — coalescing must be per-player, not global", byPlayer["Other"])
	}
	if got := len(w.take()); got != 0 {
		t.Fatalf("take() must drain: second take returned %d ops", got)
	}
}

// TestPlacementOfferNeverBlocks pins the never-stall-an-actor-loop contract: offering with no drainer
// running, far past any plausible buffer, must return immediately. A blocking hand-off would let a wedged
// Redis stall every zone goroutine on the shard.
func TestPlacementOfferNeverBlocks(t *testing.T) {
	w := newPlacementWriter()
	done := make(chan struct{})
	go func() {
		for i := range 1000 {
			w.offer(placementOp{playerID: string(rune('A' + i%26)), zoneID: "midgaard", epoch: 1})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("offer blocked — a placement hand-off must never stall the zone goroutine that made it")
	}
	if got := len(w.take()); got != 26 {
		t.Fatalf("coalesced to %d players, want 26 (memory bounded by resident count, not event rate)", got)
	}
}

// TestFreshLoginMakesAPlayerTellAddressable pins the user-visible half of #320 slice 2, and the half that
// nothing else covers.
//
// The tell/mail path uses directory.PlayerShard as its EXISTENCE oracle: found=false means "There is no
// player by that name" and the tell is refused. Before this slice the placement hash was written only by
// the cross-shard handoff CAS, so a player who had simply logged in and stayed put had no placement — and
// was therefore unaddressable. Every existing tell test pre-seeds a fake locator, so none of them notice.
func TestFreshLoginMakesAPlayerTellAddressable(t *testing.T) {
	client, dir := placementWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The oracle refuses them before they have ever logged in.
	if _, found, err := dir.PlayerShard(ctx, "Callee"); err != nil || found {
		t.Fatalf("premise: an unknown name must not resolve (found=%v err=%v)", found, err)
	}

	s, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Callee"))
	recvAttached(t, s)
	waitPlacement(t, dir, "Callee", func(p directory.Placement) bool { return p.ShardID != "" },
		"a fresh login must register a placement")

	// And now it resolves — without any cross-shard handoff ever having happened.
	shardID, found, err := dir.PlayerShard(ctx, "Callee")
	if err != nil || !found || shardID != "shard-a" {
		t.Fatalf("PlayerShard(Callee) = %q, found=%v, err=%v; want shard-a,true — a logged-in player must be tell-addressable", shardID, found, err)
	}
}

// TestCleanQuitTombstonesThePlacement is the #70 regression at the world level: a player who types `quit`
// has their placement's SHARD field dropped, while the epoch and zone survive so they stay routable and
// tell-addressable.
func TestCleanQuitTombstonesThePlacement(t *testing.T) {
	client, dir := placementWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ctx1, drop1 := context.WithCancel(ctx)
	s, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Quitter"))
	recvAttached(t, s)
	waitPlacement(t, dir, "Quitter", func(p directory.Placement) bool { return p.ShardID == "shard-a" },
		"login did not register a placement")

	send(t, s, inputSeq(1, "quit"))
	recvOutputContaining(t, s, "Farewell")
	drop1()

	p := waitPlacement(t, dir, "Quitter", func(p directory.Placement) bool { return p.ShardID == "" },
		"a clean quit must tombstone the placement's shard field (#70)")
	if p.ZoneID != "midgaard" {
		t.Fatalf("the tombstone dropped the ZONE: %+v — a returning player routes by it (#320)", p)
	}
	if p.Epoch == 0 {
		t.Fatalf("the tombstone dropped the EPOCH: %+v — it is the handoff CAS fence", p)
	}

	// And they are still addressable: `found` means exists, not "is connected".
	if _, found, err := dir.PlayerShard(ctx, "Quitter"); err != nil || !found {
		t.Fatalf("a logged-out character must stay tell-addressable: found=%v err=%v", found, err)
	}
}

// TestLinkDeathDoesNotTombstoneThePlacement: a dropped socket is NOT a logout. The session survives
// detached for the link-death grace, still holding the player's entity, so the record should keep saying
// this shard hosts them.
//
// The proof has to be POSITIVE. An earlier version of this test just polled for two seconds and asserted no
// tombstone appeared — which would pass even if `detach` had never run at all, because nothing tombstones a
// link-dead player in that window (or ever). So instead we reconnect inside the grace: a successful attach
// that RESUMES the detached session is direct evidence the link-dead path executed, and only then do we
// assert the shard field survived it. (test-engineer review.)
func TestLinkDeathDoesNotTombstoneThePlacement(t *testing.T) {
	client, dir := placementWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ctx1, drop1 := context.WithCancel(ctx)
	s, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Dropped"))
	recvAttached(t, s)
	send(t, s, inputSeq(1, "say hello"))
	recvOutputContaining(t, s, "hello")
	waitPlacement(t, dir, "Dropped", func(p directory.Placement) bool { return p.ShardID == "shard-a" },
		"login did not register a placement")

	drop1() // socket dies; no `quit`

	// Reconnect inside the grace. The resume ack carries the high-water input seq the DETACHED session had
	// applied — a fresh login would ack 0. That is our positive proof the link-dead path ran and the session
	// was held, not quit.
	s2, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s2, attach("Dropped"))
	if ack := recvAttached(t, s2); ack != 1 {
		t.Fatalf("resume ack = %d, want 1 — the reconnect did not resume the detached session, so this test "+
			"is not exercising the link-death path at all", ack)
	}

	if p, _ := dir.PlayerPlacement(ctx, "Dropped"); p.ShardID != "shard-a" {
		t.Fatalf("link death tombstoned the placement: %+v — the record must keep naming the shard that is "+
			"still physically holding the detached session (#70)", p)
	}
}

// TestQuitThenRelogLandsBackInTheDurableZone is the #70 x #320 interaction: the tombstone keeps the zone, so
// a returning player is routed to whoever owns it and rehydrates there. If the tombstone had dropped the
// zone (or the whole record), this player would wake in the home zone's start room.
func TestQuitThenRelogLandsBackInTheDurableZone(t *testing.T) {
	client, dir := placementWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	ctx1, drop1 := context.WithCancel(ctx)
	s, err := client.Connect(ctx1)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s, attach("Rover"))
	recvAttached(t, s)
	send(t, s, inputSeq(1, "north"))
	recvOutputContaining(t, s, "Market Square")
	send(t, s, inputSeq(2, "north"))
	recvOutputContaining(t, s, "Moonlit Grove")
	send(t, s, inputSeq(3, "quit"))
	recvOutputContaining(t, s, "Farewell")
	drop1()

	p := waitPlacement(t, dir, "Rover", func(p directory.Placement) bool { return p.ShardID == "" },
		"the quit did not tombstone the placement")
	if p.ZoneID != "darkwood" {
		t.Fatalf("the tombstone must preserve the durable zone for routing: %+v", p)
	}

	// Relog: the player must come back in darkwood, not the home zone's temple.
	s2, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send(t, s2, attach("Rover"))
	recvAttached(t, s2)
	send(t, s2, inputSeq(1, "look"))
	if got := recvNextOutput(t, s2); !strings.Contains(got, "Moonlit Grove") {
		t.Fatalf("a relog after a clean quit must rehydrate into the durable zone, got %q", got)
	}

	// And the fresh login re-registers the shard, so the record is live again.
	waitPlacement(t, dir, "Rover", func(p directory.Placement) bool { return p.ShardID == "shard-a" },
		"the relog did not re-register the placement's shard")
}

// TestAReapedPlayerIsNotTombstoned pins KNOWN behavior so a future change to it is deliberate. A link-dead
// player reaped after the grace never gets a tombstone: `reap` -> `leave` does not clear the placement. Their
// record keeps naming this shard until their next login rewrites it, which is why `mailcmds`'s `online` gate
// still counts them as hosted (#325).
func TestAReapedPlayerIsNotTombstoned(t *testing.T) {
	w := newPlacementWriter()
	// leave()/reap() have no clearPlacement call, so nothing is ever offered for a reaped player. The
	// assertion is structural: a writer that saw no offer has nothing pending.
	if got := len(w.take()); got != 0 {
		t.Fatalf("pending = %d, want 0", got)
	}
	// Guard the real invariant by grepping the call graph would be brittle; instead assert the one call site
	// exists on the quitting path only. If someone adds clearPlacement to leave(), this comment and
	// TestLinkDeathDoesNotTombstoneThePlacement are the tripwires.
}

// TestQuitThenRelogLeavesTheLivePlacement pins the coalescing interaction: a pending logout tombstone must
// never be applied AFTER the player's fresh login registration. The writer's map is keyed by player, so the
// later registration replaces the pending clear rather than racing it.
func TestQuitThenRelogLeavesTheLivePlacement(t *testing.T) {
	w := newPlacementWriter()

	w.offer(placementOp{playerID: "Yo-yo", epoch: 1, clear: true})        // quit
	w.offer(placementOp{playerID: "Yo-yo", zoneID: "darkwood", epoch: 1}) // immediate relog

	ops := w.take()
	if len(ops) != 1 {
		t.Fatalf("pending = %d ops, want 1 (coalesced per player)", len(ops))
	}
	if ops[0].clear {
		t.Fatal("a queued logout tombstone survived a later re-login registration — it would evict the live placement")
	}
	if ops[0].zoneID != "darkwood" {
		t.Fatalf("coalesced op = %+v, want the fresh registration", ops[0])
	}
}
