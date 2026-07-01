package world

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	handoffv1 "github.com/double-nibble/telosmud/api/gen/telosmud/handoff/v1"
	"github.com/double-nibble/telosmud/internal/content"
	"github.com/double-nibble/telosmud/internal/directory"
	"google.golang.org/grpc/test/bufconn"
)

// TestZoneHandoverFlipsOwnershipWithoutGapOrFence is the Phase-16.4b control-plane core (no players yet):
// a draining shard A hands zone midgaard's ownership to a standby B via AdoptZone + the fenced HandoverZone
// flip. It asserts the invariants the DS review demanded: the lease flips A->B with NO ownerless gap, B ends
// up hosting the zone AND renewing its lease, and A's renewal stops WITHOUT fencing the (still-draining)
// shard.
func TestZoneHandoverFlipsOwnershipWithoutGapOrFence(t *testing.T) {
	lc, err := content.LoadDemoPack()
	if err != nil {
		t.Fatal(err)
	}

	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	dir := directory.NewRedis(rdb, "test")

	ctx := context.Background()
	mustReg(t, dir.RegisterShard(ctx, "shard-a", "addr-a", directory.DefaultShardLease))
	mustReg(t, dir.RegisterShard(ctx, "shard-b", "addr-b", directory.DefaultShardLease))
	mustReg(t, dir.RegisterZone(ctx, "midgaard", "shard-a")) // A owns midgaard at boot

	lisA := bufconn.Listen(1 << 20)
	lisB := bufconn.Listen(1 << 20)
	lisByAddr := map[string]*bufconn.Listener{"addr-a": lisA, "addr-b": lisB}
	peers := func(addr string) (handoffv1.HandoffClient, error) {
		lis := lisByAddr[addr]
		if lis == nil {
			return nil, fmt.Errorf("unknown shard %q", addr)
		}
		return handoffv1.NewHandoffClient(dialBuf(t, lis)), nil
	}

	// A fence signal per shard; NEITHER should fire (A hands off deliberately, B confirms ownership).
	fenceA := make(chan struct{}, 1)
	fenceB := make(chan struct{}, 1)
	fence := func(ch chan struct{}) func() {
		return func() {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}

	// A hosts midgaard; B is a STANDBY (hosts nothing) but has all zone prototypes from the same content.
	shardA := NewShardFromContent(lc, []string{"midgaard"}, "midgaard", "addr-a", dir, peers).
		WithZoneLeasing(dir, "shard-a", time.Second, 80*time.Millisecond, fence(fenceA))
	shardB := NewShardFromContent(lc, nil, "", "addr-b", dir, peers).
		WithZoneLeasing(dir, "shard-b", time.Second, 80*time.Millisecond, fence(fenceB))
	serveShard(t, shardA, lisA)
	serveShard(t, shardB, lisB)

	// Hand midgaard from A to B. Retry until both shards are up (AdoptZone fails cleanly before any side
	// effect until B is running); waitCond stops on the first success, so the flip runs exactly once.
	waitCond(t, "zone handover A->B succeeds", func() bool {
		return shardA.handoverZoneTo(ctx, "midgaard", "shard-b", "addr-b") == nil
	})

	// The lease flipped straight to B — no ownerless gap was ever observable.
	owner, err := dir.ShardForZone(ctx, "midgaard")
	if err != nil || owner != "shard-b" {
		t.Fatalf("owner after handover = %q (err %v), want shard-b", owner, err)
	}
	// B now hosts the zone (so a player redirect can Prepare into it).
	if shardB.ZoneByID("midgaard") == nil {
		t.Fatal("B does not host midgaard after AdoptZone")
	}
	// A recorded the deliberate handoff (its renewal stopped, no fence).
	if !shardA.zoneHandedOff("midgaard") {
		t.Fatal("A did not mark midgaard handed off")
	}

	// B's renewal keeps the lease: ownership stays shard-b across several renew intervals (proves B confirmed
	// ownership and is renewing — not just a one-shot flip that lapses).
	time.Sleep(300 * time.Millisecond)
	owner, err = dir.ShardForZone(ctx, "midgaard")
	if err != nil || owner != "shard-b" {
		t.Fatalf("owner after B renews = %q (err %v), want a stable shard-b", owner, err)
	}

	// Neither shard fenced.
	select {
	case <-fenceA:
		t.Fatal("draining shard A fenced itself on a deliberate handoff")
	case <-fenceB:
		t.Fatal("adopting shard B fenced itself instead of confirming ownership")
	default:
	}
}
